package main

import (
	"sync"
	"time"
)

// The resubmission window. IndexNow says a URL should not be resubmitted unless
// it changed, and that at least 5 minutes should pass before the same URL is
// sent again - 10 minutes elsewhere in the same documentation. Five is the
// shorter of the two and the one chosen here: the window exists to keep a
// caller's retry loop from spending crawl quota on a URL it just sent, not to
// police how often a page may change, and that decision stays with the caller.
// Erring short means an operator who genuinely changed a page is delayed less.
const (
	// resubmitWindow is how long a URL is remembered after it was sent, per
	// site host.
	resubmitWindow = 5 * time.Minute
	// maxTrackedURLs caps how many URLs are remembered at once. The list is
	// arbitrary caller input and the token behind it can leak - the same reason
	// the rate bucket exists - so a map that grows with every URL ever submitted
	// is a leak a caller controls. 4096 is far more than a realistic site
	// resubmits inside five minutes and stays a few megabytes at worst.
	maxTrackedURLs = 4096
	// maxTrackedURLLength is the longest URL worth remembering. A count-only cap
	// leaves one entry's size unbounded, and a single URL may be tens of
	// kilobytes long, so the bound is on bytes too: the worst case is
	// maxTrackedURLs * maxTrackedURLLength. Past 2048 bytes a URL is longer than
	// anything a crawler treats as a page, so it is simply not remembered, which
	// costs at most one resubmission.
	maxTrackedURLLength = 2048
)

// submitWindow remembers which URLs were sent, per site host, so a caller
// retrying on a timer does not spend crawl quota on a URL that was announced
// minutes ago. Every submitted URL spends that quota whether or not the engine
// already had it.
//
// The map is bounded twice - maxTrackedURLs entries of at most
// maxTrackedURLLength bytes - because the input is arbitrary. When both are
// reached the oldest entry goes: every entry has the same lifetime, so the
// oldest is also the one the window would release first, and forgetting it costs
// at most one early resubmission. An unbounded map would be a leak a caller
// controls, which is the same harm the rate bucket exists to prevent.
//
// The mutex covers the map arithmetic only, never an engine call, and the clock
// is a field so a test can advance time instead of sleeping - the same shape as
// rateBucket, for the same reasons.
//
// A URL enters the map as a claim before the batch that carries it is sent, and
// only a batch the engine accepted promotes that claim to a recorded send. The
// claim is what closes the check-then-act race the two calls used to leave open:
// two concurrent requests for the same URL both read "not in the window" before
// either had written anything, and both spent quota on it. A claim is bounded in
// time like everything else here, so one that is never released or promoted ages
// out with the window and cannot suppress a URL forever.
type submitWindow struct {
	window time.Duration
	limit  int
	// now is the clock. Production uses time.Now, whose readings carry a
	// monotonic component, so a wall-clock step cannot stretch or fake the age of
	// an entry.
	now func() time.Time

	mu      sync.Mutex
	entries map[windowKey]windowEntry
}

// windowEntry is one remembered URL. claimed means a batch carrying it is in
// flight right now: the URL is inside the window, so a concurrent request skips
// it, but nothing has been sent yet - so a batch that fails can drop the claim
// and leave the caller's retry unsuppressed.
type windowEntry struct {
	at      time.Time
	claimed bool
}

// windowKey is one URL on one site. The host is part of the key rather than
// implied by the URL: a URL is absolute, but isolation between sites must not
// depend on that, and one site may never suppress another's URL.
type windowKey struct {
	host string
	url  string
}

func newSubmitWindow(window time.Duration, limit int) *submitWindow {
	return &submitWindow{
		window:  window,
		limit:   limit,
		now:     time.Now,
		entries: make(map[windowKey]windowEntry, 4),
	}
}

// growLimit raises the map's cap to n, and never lowers it. Growing is safe
// because only the cap changes: no entry is evicted to make room, so every URL
// this window already remembered keeps its place. Lowering it is refused on
// purpose - the limit is a memory bound, not a property of the current config,
// and a smaller cap would evict URLs a caller was promised were remembered, at
// the price of resubmitting pages nobody changed.
func (w *submitWindow) growLimit(n int) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if n > w.limit {
		w.limit = n
	}
}

// keep drops the URLs the window already carries and claims the rest, so a
// concurrent request carrying the same URL sees it inside the window and skips
// it. The claim is made under the same lock as the lookup, which is what makes
// exactly one of two racing requests the one that reaches the engine.
//
// The kept slice is what the request is built from, so the count and what
// reaches the engine cannot disagree. Every kept URL is also claimed unless it is
// past maxTrackedURLLength, which is not worth remembering - such a URL is sent
// and simply not remembered, which costs at most one resubmission.
func (w *submitWindow) keep(host string, urls []string) (kept []string, skipped int) {
	host = canonicalHost(host)

	w.mu.Lock()
	defer w.mu.Unlock()

	// The clock is read under the lock, and that is not incidental. Reading it
	// before the lock made concurrent callers stamp the same URL out of order:
	// the goroutine that sampled later could still take the lock first, so the
	// one holding the earlier reading computed a negative age, took it for a
	// backwards clock, forgot the entry, and let both send. Locking first makes
	// the stamps follow lock order, and a monotonic reading cannot go backwards
	// between two of them, so the age a reader computes is never negative.
	now := w.now()

	kept = make([]string, 0, len(urls))
	claims := make([]windowKey, 0, len(urls))

	for _, url := range urls {
		if w.insideWindow(windowKey{host: host, url: url}, now) {
			skipped++
			continue
		}

		kept = append(kept, url)

		if len(url) <= maxTrackedURLLength {
			claims = append(claims, windowKey{host: host, url: url})
		}
	}

	if len(claims) > 0 {
		w.makeRoom(now, len(claims))

		for _, key := range claims {
			w.entries[key] = windowEntry{at: now, claimed: true}
		}

		// The same cap record honours: a batch larger than the whole map cannot
		// be remembered in full, and here the cap wins.
		for len(w.entries) > w.limit {
			w.evictOldest()
		}
	}

	return kept, skipped
}

// insideWindow reports whether key is inside the window - sent less than the
// window ago, or claimed by a batch that is in flight right now - and forgets it
// when it was not: an entry the window has released can answer nothing, so it
// must not keep occupying a bounded map.
//
// A claim is treated exactly like a recorded send while its batch is out, which
// is the whole point of making it; and because it is stamped when the batch is
// built, a claim that is never released or promoted is forgotten after one
// window like any other entry. A leaked claim is therefore self-healing and
// cannot suppress a URL for longer than a send would have.
func (w *submitWindow) insideWindow(key windowKey, now time.Time) bool {
	entry, known := w.entries[key]
	if !known {
		return false
	}

	age := now.Sub(entry.at)
	if age >= 0 && age < w.window {
		return true
	}

	// Either the window has passed, or the clock went backwards. A backwards
	// step is not evidence of a recent send, and honouring it would suppress the
	// URL until the clock caught up, so the entry is forgotten and the URL is
	// sent again.
	//
	// A real clock cannot do that here: keep and record read the clock under the
	// same mutex that stamps the entry, and time.Now carries a monotonic reading,
	// so a reader's now is never earlier than what it just read. This branch is
	// for a clock that is handed in by a test, and it stays because a window that
	// trusted a backwards stamp would suppress a URL until the clock caught up.
	delete(w.entries, key)

	return false
}

// record remembers that these URLs were sent just now, promoting the claim keep
// made for each of them. Only a request that was actually built and accepted is
// recorded: a URL that was suppressed must never extend its own window, and a
// submission that failed leaves the caller a retry to make, which suppressing
// would silently eat.
func (w *submitWindow) record(host string, urls []string) {
	host = canonicalHost(host)

	w.mu.Lock()
	defer w.mu.Unlock()

	// Under the lock for the reason keep gives: a stamp taken outside it can be
	// earlier than one an earlier lock holder wrote.
	now := w.now()

	keys := make([]windowKey, 0, len(urls))
	for _, url := range urls {
		if len(url) > maxTrackedURLLength {
			continue
		}
		keys = append(keys, windowKey{host: host, url: url})
	}
	if len(keys) == 0 {
		return
	}

	// Make room for the whole batch once rather than once per URL, so one batch
	// pays for one sweep.
	w.makeRoom(now, len(keys))

	for _, key := range keys {
		w.entries[key] = windowEntry{at: now}
	}

	// A single batch larger than the whole map cannot be remembered in full, and
	// here the cap wins: it is what keeps arbitrary input from growing this
	// structure. maxURLsPerRequest is far below maxTrackedURLs, so this only runs
	// for a deliberately tiny window.
	for len(w.entries) > w.limit {
		w.evictOldest()
	}
}

// release drops the claims keep made for a batch that was never accepted, so a
// failure leaves the caller's retry unsuppressed. Only a claim is dropped: a
// recorded entry is evidence the engine took those URLs, and a release must
// never be able to un-send one.
func (w *submitWindow) release(host string, urls []string) {
	host = canonicalHost(host)

	w.mu.Lock()
	defer w.mu.Unlock()

	for _, url := range urls {
		key := windowKey{host: host, url: url}
		if entry, known := w.entries[key]; known && entry.claimed {
			delete(w.entries, key)
		}
	}
}

// makeRoom forgets entries until the map can hold need more.
func (w *submitWindow) makeRoom(now time.Time, need int) {
	if len(w.entries)+need <= w.limit {
		return
	}

	// Entries the window has already released answer nothing, so they go first:
	// in a normal deployment they are the only ones that ever need to.
	for key, entry := range w.entries {
		if age := now.Sub(entry.at); age < 0 || age >= w.window {
			delete(w.entries, key)
		}
	}

	for len(w.entries)+need > w.limit && len(w.entries) > 0 {
		w.evictOldest()
	}
}

// evictOldest drops the entry with the earliest timestamp, which is the one the
// window would have released first anyway.
func (w *submitWindow) evictOldest() {
	var (
		oldest   windowKey
		oldestAt time.Time
		found    bool
	)

	for key, entry := range w.entries {
		if !found || entry.at.Before(oldestAt) {
			oldest, oldestAt, found = key, entry.at, true
		}
	}

	if found {
		delete(w.entries, oldest)
	}
}
