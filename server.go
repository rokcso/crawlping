package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxBodyBytes bounds a submission body. 100 URLs fit far below this; anything
// larger is a mistake or an attempt to make the process allocate.
const maxBodyBytes = 64 << 10

// verifyTimeout bounds one key-file check, so a slow origin cannot hold the
// background check open indefinitely.
const verifyTimeout = 10 * time.Second

// reasonPayloadTooLarge is the only reason the handler produces on its own. It
// lives here rather than with the engine reasons because an oversized body is
// refused before a submission is built: no engine ever saw the payload, so
// calling it a malformed one would send the caller looking at their syntax.
const reasonPayloadTooLarge = "payload_too_large"

// reasonUnknownToken is the single refusal reason for a missing or unrecognised
// bearer token, on every endpoint that takes one.
const reasonUnknownToken = "unknown_token"

// reasonMethodNotAllowed means the endpoint does not answer this method. It is a
// reason rather than a bare status because a caller branches on reason, and it
// pairs with the Allow header that names the methods that do work.
const reasonMethodNotAllowed = "method_not_allowed"

// There are two different 429s on this endpoint, and the difference is the
// point of the guard below. reasonThrottled is this instance refusing to spend
// any more of its own egress IP on one site right now: the engine never saw the
// request. reasonRateLimited is the engine's own answer, saying it is throttling
// this IP already. A caller has to be able to tell whose limit it hit, because
// only the second one means the operator's IP is being penalised.
const (
	// reasonThrottled is a local refusal: the site is out of tokens.
	reasonThrottled = "throttled"
	// reasonOverloaded is a local refusal: every engine slot is in use.
	reasonOverloaded = "overloaded"
)

// Guard defaults. IndexNow does not publish its limits, so these are
// deliberately conservative: the job is to keep one site's retry loop from
// burning the operator's egress IP, not to squeeze the last request out of the
// engine. Raise them only with a reason and never past what the engine
// tolerates from one IP.
const (
	// rateBurstTokens is how many submissions one host may send back to back
	// before it has to wait for a token.
	rateBurstTokens = 10
	// rateRefillInterval is how long one host waits for its next token: one
	// submission per second, sustained, per host.
	rateRefillInterval = time.Second
	// maxInFlightEngineCalls is how many engine requests may be open at once,
	// across every site, so a burst on one host cannot exhaust sockets either.
	maxInFlightEngineCalls = 8
	// inFlightWait is how long a submission waits for a free slot before it is
	// told the instance is busy.
	inFlightWait = 2 * time.Second
	// busyRetryAfter is the retry hint an overloaded answer carries.
	busyRetryAfter = time.Second
	// recheckTimeout bounds one on-demand recheck of the key files.
	recheckTimeout = 5 * time.Second
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

// keyState is everything this service can honestly say about a host's key file.
// There are three states rather than two because a check that reached no answer
// is not a failed check: an origin that is down, a check cut short by shutdown,
// and a key file that is definitely wrong are different facts, and only the last
// one is anybody's fault to fix.
type keyState string

const (
	// keyStateVerified means the key is known to work: the key file was fetched
	// and matched, or the engine accepted a batch with 200, which is the status
	// IndexNow returns once it has validated the key.
	keyStateVerified keyState = "verified"
	// keyStatePending means nothing is known: the host has not been checked
	// yet, a check is still running, the check was cancelled or timed out, or
	// the engine answered 202, which only says the batch was received and key
	// validation is still pending. It is never reported as verified or failed.
	keyStatePending keyState = "pending"
	// keyStateFailed means the key is known not to work: the key file was
	// fetched and refused (non-2xx status, unreadable body, or a body that is
	// not the configured key), or the engine answered 403.
	keyStateFailed keyState = "failed"
)

// keyRecord is one host's slot in /health: the state, when it was last decided,
// and the reason it was decided that way. A state on its own goes stale
// silently - "verified" would keep describing a key file that broke hours ago -
// and for a tool whose selling point is failure visibility, "failed" on its own
// says too little about what broke.
type keyRecord struct {
	State keyState `json:"state"`
	// CheckedAt is when this record was last written, in UTC to the second.
	// It is omitted while the host has never been checked, because there is no
	// honest time to report yet.
	CheckedAt time.Time `json:"checked_at,omitzero"`
	// Detail is the last failure or inconclusive reason, empty once verified.
	Detail string `json:"detail"`
}

// newKeyRecord stamps a state with the time it was decided.
func newKeyRecord(state keyState, detail string) keyRecord {
	return keyRecord{
		State:     state,
		CheckedAt: time.Now().UTC().Truncate(time.Second),
		Detail:    detail,
	}
}

type server struct {
	cfg    *Config
	client *indexNowClient
	log    *slog.Logger

	mu sync.RWMutex
	// keyStates holds the last recorded state per host. A host that is absent
	// is pending, which is the honest default and the reason the map only ever
	// grows: setKeyState is the only writer.
	keyStates map[string]keyRecord

	// rate and inflight bound what this instance spends its own egress IP on.
	// Without them a leaked token or a retry loop in one caller can push the
	// operator's IP into the engine's throttle, which is the exact harm this
	// project exists to avoid: cross-domain abuse is already impossible (a token
	// is bound to one host), frequency was not bounded at all.
	rate     *rateBucket
	inflight *inflightGuard
	// window is the per-URL half of the quota guard: a URL it remembers is
	// dropped from the batch before the request is built, because every
	// submitted URL spends crawl quota whether or not the engine already had it.
	window *submitWindow
	// recheckTimeout bounds one ?recheck=1, and is a field so a test can spend
	// a few milliseconds instead of five seconds.
	recheckTimeout time.Duration
}

func newServer(cfg *Config, client *indexNowClient, log *slog.Logger) *server {
	return &server{
		cfg:            cfg,
		client:         client,
		log:            log,
		keyStates:      make(map[string]keyRecord, len(cfg.Sites)),
		rate:           newRateBucket(rateBurstTokens, rateRefillInterval),
		inflight:       newInflightGuard(maxInFlightEngineCalls),
		window:         newSubmitWindow(resubmitWindow, maxTrackedURLs),
		recheckTimeout: recheckTimeout,
	}
}

// rateBucket is a per-host token bucket. It is the frequency half of the guard:
// a token is bound to one host, so nobody can submit for a domain they do not
// own, but a leaked token or a caller looping on a retry can hammer the engine
// from the operator's own IP until it throttles that IP - which breaks the
// legitimate submissions too, and is the harm this tool was built to avoid.
//
// The bucket is keyed on site.Host, so one noisy site cannot starve another.
// The mutex covers the arithmetic on the tokens and nothing else: it is never
// held across an engine call, and refill is computed from elapsed time rather
// than from a ticker, so there is no sleeping while holding a lock either.
//
// A refusal here is reasonThrottled and never reasonRateLimited: the first is
// this instance protecting its own egress IP and nothing was sent, the second is
// the engine already penalising that IP. A caller has to be able to tell which
// limit it hit, so the two reasons stay separate even though both answer 429.
type rateBucket struct {
	capacity float64
	// refill is tokens per second.
	refill float64
	// now is the clock. It is a field so a test can advance time instead of
	// sleeping; production always uses time.Now, whose readings stay monotonic
	// when subtracted, so a wall-clock jump cannot mint tokens.
	now func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucketTokens
}

// bucketTokens is one host's balance. last is the time the balance was last
// brought up to date.
type bucketTokens struct {
	tokens float64
	last   time.Time
}

func newRateBucket(capacity int, refillInterval time.Duration) *rateBucket {
	return &rateBucket{
		capacity: float64(capacity),
		refill:   1 / refillInterval.Seconds(),
		now:      time.Now,
		buckets:  make(map[string]*bucketTokens, 4),
	}
}

// take spends one token for host. It returns zero when a token was available,
// and otherwise how long the caller has to wait for the next one: a full burst
// is allowed, then one submission per refill interval.
func (b *rateBucket) take(host string) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()

	bucket, known := b.buckets[host]
	if !known {
		// A host starts full, so a site that has been quiet all day is not
		// slower on its first submission than on its tenth.
		bucket = &bucketTokens{tokens: b.capacity, last: now}
		b.buckets[host] = bucket
	}

	// Elapsed time, not a ticker: a bucket that was last touched five minutes
	// ago refills to capacity at the next look, with no goroutine behind it.
	if elapsed := now.Sub(bucket.last); elapsed > 0 {
		bucket.tokens = math.Min(b.capacity, bucket.tokens+elapsed.Seconds()*b.refill)
		bucket.last = now
	}

	if bucket.tokens >= 1 {
		bucket.tokens--
		return 0
	}

	// Round up, so the reported wait is never shorter than the real one and a
	// caller that waits exactly this long is not refused again on arrival.
	missing := 1 - bucket.tokens
	return time.Duration(math.Ceil(missing / b.refill * float64(time.Second)))
}

// inflightGuard caps how many engine requests this instance has open at once. A
// buffered channel is the semaphore: sending takes a slot, receiving frees one,
// and no dependency is needed for that.
type inflightGuard struct {
	slots chan struct{}
	// wait is how long acquire blocks before it gives up.
	wait time.Duration
}

func newInflightGuard(limit int) *inflightGuard {
	return &inflightGuard{slots: make(chan struct{}, limit), wait: inFlightWait}
}

// acquire takes a slot, waiting at most wait for one to free up. A caller that
// gives up is told the instance is busy instead of queueing behind the engine
// for as long as the engine takes, and it never reaches the engine.
func (g *inflightGuard) acquire(ctx context.Context) bool {
	select {
	case g.slots <- struct{}{}:
		return true
	default:
	}

	timer := time.NewTimer(g.wait)
	defer timer.Stop()

	select {
	case g.slots <- struct{}{}:
		return true
	case <-timer.C:
		return false
	case <-ctx.Done():
		// The caller is gone, so there is nobody to answer and no reason to
		// hold a slot. Reported as no slot, which is what it is.
		return false
	}
}

// release frees the slot acquire took. Every successful acquire has exactly one
// release, deferred by the handler.
func (g *inflightGuard) release() {
	<-g.slots
}

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
type submitWindow struct {
	window time.Duration
	limit  int
	// now is the clock. Production uses time.Now, whose readings carry a
	// monotonic component, so a wall-clock step cannot stretch or fake the age of
	// an entry.
	now func() time.Time

	mu   sync.Mutex
	sent map[windowKey]time.Time
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
		window: window,
		limit:  limit,
		now:    time.Now,
		sent:   make(map[windowKey]time.Time, 4),
	}
}

// keep drops the URLs sent less than the window ago and reports how many it
// dropped. The kept slice is what the request is built from, so the count and
// what reaches the engine cannot disagree.
func (w *submitWindow) keep(host string, urls []string) (kept []string, skipped int) {
	host = canonicalHost(host)
	now := w.now()

	w.mu.Lock()
	defer w.mu.Unlock()

	kept = make([]string, 0, len(urls))
	for _, url := range urls {
		if w.insideWindow(windowKey{host: host, url: url}, now) {
			skipped++
			continue
		}
		kept = append(kept, url)
	}

	return kept, skipped
}

// insideWindow reports whether key was sent less than the window ago, and
// forgets it when it was not: an entry the window has released can answer
// nothing, so it must not keep occupying a bounded map.
func (w *submitWindow) insideWindow(key windowKey, now time.Time) bool {
	sentAt, known := w.sent[key]
	if !known {
		return false
	}

	age := now.Sub(sentAt)
	if age >= 0 && age < w.window {
		return true
	}

	// Either the window has passed, or the clock went backwards. A backwards
	// step is not evidence of a recent send, and honouring it would suppress the
	// URL until the clock caught up, so the entry is forgotten and the URL is
	// sent again.
	delete(w.sent, key)

	return false
}

// record remembers that these URLs were sent just now. Only a request that was
// actually built and accepted is recorded: a URL that was suppressed must never
// extend its own window, and a submission that failed leaves the caller a retry
// to make, which suppressing would silently eat.
func (w *submitWindow) record(host string, urls []string) {
	host = canonicalHost(host)
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

	w.mu.Lock()
	defer w.mu.Unlock()

	// Make room for the whole batch once rather than once per URL, so one batch
	// pays for one sweep.
	w.makeRoom(now, len(keys))

	for _, key := range keys {
		w.sent[key] = now
	}

	// A single batch larger than the whole map cannot be remembered in full, and
	// here the cap wins: it is what keeps arbitrary input from growing this
	// structure. maxURLsPerRequest is far below maxTrackedURLs, so this only runs
	// for a deliberately tiny window.
	for len(w.sent) > w.limit {
		w.evictOldest()
	}
}

// makeRoom forgets entries until the map can hold need more.
func (w *submitWindow) makeRoom(now time.Time, need int) {
	if len(w.sent)+need <= w.limit {
		return
	}

	// Entries the window has already released answer nothing, so they go first:
	// in a normal deployment they are the only ones that ever need to.
	for key, sentAt := range w.sent {
		if age := now.Sub(sentAt); age < 0 || age >= w.window {
			delete(w.sent, key)
		}
	}

	for len(w.sent)+need > w.limit && len(w.sent) > 0 {
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

	for key, sentAt := range w.sent {
		if !found || sentAt.Before(oldestAt) {
			oldest, oldestAt, found = key, sentAt, true
		}
	}

	if found {
		delete(w.sent, oldest)
	}
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	// Registered on the path and not on a method, which is deliberate. A mux
	// "GET /submit" pattern also routes HEAD, and the Allow header it builds from
	// its patterns then advertises HEAD for an endpoint where a HEAD would have to
	// submit something. The handler answers 405 itself instead, so Allow says
	// exactly what the endpoint does: GET for IndexNow's own single-URL form, which
	// makes citing one page a single curl, and POST for a batch.
	mux.HandleFunc("/submit", s.handleSubmit)
	mux.HandleFunc("GET /health", s.handleHealth)
	return mux
}

type submitRequest struct {
	URLs []string `json:"urls"`
}

type submitResponse struct {
	OK bool `json:"ok"`
	// Status is the HTTP status of this response. A locally refused batch was
	// never sent to the engine, so putting an engine status here would be a
	// status no engine ever returned.
	Status    int    `json:"status"`
	Host      string `json:"host,omitempty"`
	Submitted int    `json:"submitted"`
	// Skipped counts the URLs the resubmission window dropped before the request
	// was built. They are not in Submitted, and they are not a reason: the caller
	// asked for something that is already true. Omitted when nothing was dropped.
	Skipped int `json:"skipped,omitempty"`
	// EngineStatus is the status the engine answered with, and is absent
	// whenever the engine was never called. Together with Reason it separates
	// "the engine refused this with 422 host_mismatch" from "this instance
	// refused with 422 host_mismatch": same reason, different owner, different
	// fix, and only this field tells them apart.
	EngineStatus int `json:"engine_status,omitempty"`
	// Reason names why the batch was not accepted. payload_too_large,
	// throttled, overloaded and method_not_allowed are the values the handler
	// adds itself: no submission was ever sent, so no engine produced them.
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
	// RetryAfterSeconds is set for rate_limited, throttled and overloaded, so a
	// caller can schedule the retry without guessing.
	RetryAfterSeconds int `json:"retry_after_seconds,omitempty"`
}

func (s *server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		// The endpoint owns its 405 because the router's would advertise HEAD,
		// which cannot submit anything (see routes). Nothing is read, so an
		// unsupported method costs no token check and no guard token either.
		w.Header().Set("Allow", "GET, POST")
		writeJSON(w, http.StatusMethodNotAllowed, submitResponse{
			Status: http.StatusMethodNotAllowed,
			Reason: reasonMethodNotAllowed,
			Detail: "submit accepts GET and POST only",
		})
		return
	}

	token := bearerToken(r.Header.Get("Authorization"))
	if token == "" {
		writeJSON(w, http.StatusUnauthorized, submitResponse{
			Status: http.StatusUnauthorized,
			Reason: reasonUnknownToken,
		})
		return
	}

	site := s.siteForToken(token)
	if site == nil {
		s.log.Warn("submit rejected", "reason", reasonUnknownToken, "remote", r.RemoteAddr)
		writeJSON(w, http.StatusUnauthorized, submitResponse{
			Status: http.StatusUnauthorized,
			Reason: reasonUnknownToken,
		})
		return
	}

	// The two guards come before the body is read, in the order a caller can
	// act on them: a request this instance will not send is refused without
	// spending a byte on it, and it never reaches the engine. That is the whole
	// point - the engine must not see a request the operator's IP should not
	// have spent.
	if wait := s.rate.take(site.Host); wait > 0 {
		s.refuse(w, r, site.Host, submissionResult{
			Reason:     reasonThrottled,
			RetryAfter: wait,
			Detail:     fmt.Sprintf("%s has spent its %d request burst and has to wait for a token", site.Host, rateBurstTokens),
		})
		return
	}

	if !s.inflight.acquire(r.Context()) {
		s.refuse(w, r, site.Host, submissionResult{
			Reason:     reasonOverloaded,
			RetryAfter: busyRetryAfter,
			Detail:     fmt.Sprintf("no engine slot free within %s", s.inflight.wait),
		})
		return
	}
	defer s.inflight.release()

	urls, ok := s.requestURLs(w, r, site)
	if !ok {
		return
	}

	// duration_ms is the engine call and nothing else, so latency rising before
	// a throttle shows up in the numbers instead of only in the 429s. It is the
	// only early warning this tool can give: the engine's limit is not
	// published, so the trend is the signal.
	start := time.Now()
	result := s.client.submitWithWindow(*site, urls, s.window)
	duration := time.Since(start)

	// Record a key verdict only when the submission produced one. A 200 proves
	// the key works, 202 says validation is still pending, and 403 says the key
	// was refused; an engine error or a rate limit says nothing about the key,
	// so the recorded state is left alone.
	if state := result.keyStateAfter(); state != "" {
		s.recordSubmissionKeyState(site.Host, state, result)
	}

	status := httpStatusFor(result)
	if result.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(result.RetryAfter)))
	}

	// engine_status is logged only when an engine actually answered. A locally
	// refused batch leaves it at 0, and logging that would read as "the engine
	// answered zero".
	attrs := make([]slog.Attr, 0, 8)
	attrs = append(attrs,
		slog.String("host", site.Host),
		slog.String("remote", r.RemoteAddr),
	)
	if result.EngineStatus != 0 {
		attrs = append(attrs, slog.Int("engine_status", result.EngineStatus))
	}
	attrs = append(attrs,
		slog.Int("http_status", status),
		slog.Int("submitted", result.Submitted),
		// Logged unconditionally, including a zero: a line that sometimes has the
		// field and sometimes does not is harder to group on than one that always
		// says how much the window dropped.
		slog.Int("skipped", result.Skipped),
		slog.String("reason", result.Reason),
		slog.Int64("duration_ms", duration.Milliseconds()),
	)
	s.log.LogAttrs(r.Context(), slog.LevelInfo, "submit", attrs...)

	writeJSON(w, status, submitResponse{
		OK:                result.accepted(),
		Status:            status,
		Host:              site.Host,
		Submitted:         result.Submitted,
		Skipped:           result.Skipped,
		EngineStatus:      result.EngineStatus,
		Reason:            result.Reason,
		Detail:            result.Detail,
		RetryAfterSeconds: retryAfterSeconds(result.RetryAfter),
	})
}

// requestURLs reads the URL list a /submit request carries. A GET carries it in
// repeated url query parameters, which is IndexNow's own single-URL form and
// what makes citing one page a single curl; a POST carries it in a
// {"urls": [...]} object. The method decides nothing after this point, so both
// go through the same validation, the same reasons and statuses, the same log
// line and the same key-state and window update: one path, one answer shape,
// whichever method was used.
//
// It writes the refusal itself and reports false when the list cannot be read:
// an oversized or unparseable body has its own status and its own detail, so
// there is nothing left for the caller to answer.
func (s *server) requestURLs(w http.ResponseWriter, r *http.Request, site *Site) ([]string, bool) {
	if r.Method == http.MethodGet {
		// Query()["url"] returns every repeated value, so ?url=a&url=b carries
		// two. A query with no url at all leaves the list empty, which the
		// submission path already refuses as bad_request.
		return r.URL.Query()["url"], true
	}

	// POST, the only other method the handler answers, and the only one that
	// carries a body: the size cap and the JSON decoder below apply to it alone.

	// The cap is enforced by reading the body, not by the decoder. A decoder
	// stops at the end of the first JSON value, so a small value followed by
	// an enormous amount of trailing bytes never reaches the limit, and
	// MaxBytesReader has nothing to refuse. Reading all of it applies the cap
	// to the bytes that were actually sent, and json.Unmarshal then rejects
	// anything left over after the value.
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	raw, err := io.ReadAll(r.Body)
	if err != nil {
		// An oversized body is not a malformed one: it can be perfectly valid
		// JSON that simply does not fit, so it gets its own status and reason
		// instead of the syntax message below.
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			s.log.Warn("submit rejected",
				"host", site.Host,
				"reason", reasonPayloadTooLarge,
				"remote", r.RemoteAddr,
			)
			writeJSON(w, http.StatusRequestEntityTooLarge, submitResponse{
				Host:   site.Host,
				Status: http.StatusRequestEntityTooLarge,
				Reason: reasonPayloadTooLarge,
				Detail: fmt.Sprintf("body exceeded the %d byte limit", maxBodyBytes),
			})
			return nil, false
		}

		// The body could not be read at all, so there is nothing to parse and
		// nothing that was submitted.
		writeJSON(w, http.StatusBadRequest, submitResponse{
			Host:   site.Host,
			Status: http.StatusBadRequest,
			Reason: reasonBadRequest,
			Detail: "body could not be read",
		})
		return nil, false
	}

	var request submitRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		writeJSON(w, http.StatusBadRequest, submitResponse{
			Host:   site.Host,
			Status: http.StatusBadRequest,
			Reason: reasonBadRequest,
			Detail: `body must be a single {"urls": ["https://..."]} object with no trailing data`,
		})
		return nil, false
	}

	return request.URLs, true
}

// refuse answers a submission this instance declined to send. The engine was
// never called, so the response carries no engine_status and the reason is one
// of this handler's own: a caller can tell a local refusal from an engine
// verdict, which is the only way it can know whose limit it hit.
func (s *server) refuse(w http.ResponseWriter, r *http.Request, host string, result submissionResult) {
	status := httpStatusFor(result)
	seconds := retryAfterSeconds(result.RetryAfter)

	if result.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(seconds))
	}

	s.log.Warn("submit rejected",
		"host", host,
		"reason", result.Reason,
		"remote", r.RemoteAddr,
		"retry_after_seconds", seconds,
	)

	writeJSON(w, status, submitResponse{
		Status:            status,
		Host:              host,
		Reason:            result.Reason,
		Detail:            result.Detail,
		RetryAfterSeconds: seconds,
	})
}

// retryAfterSeconds turns a wait into whole seconds for Retry-After and
// retry_after_seconds. Zero means "no retry hint"; anything else rounds up so a
// caller that waits exactly as long as it was told is not refused again.
func retryAfterSeconds(wait time.Duration) int {
	if wait <= 0 {
		return 0
	}

	return int(math.Ceil(wait.Seconds()))
}

// healthResponse is the unauthenticated shape: counts only, never a host name.
// /health is reachable by anything that can reach the port, and a Cloudflare
// Tunnel makes that the whole internet, so the operator's domain list is not
// something an anonymous GET may have.
type healthResponse struct {
	OK       bool `json:"ok"`
	Sites    int  `json:"sites"`
	Verified int  `json:"verified"`
	Failed   int  `json:"failed"`
	Pending  int  `json:"pending"`
}

// healthDetailResponse is the per-host view, served only to a caller that
// proved it holds a site token.
type healthDetailResponse struct {
	OK    bool                 `json:"ok"`
	Sites int                  `json:"sites"`
	Keys  map[string]keyRecord `json:"keys"`
}

// healthErrorResponse is the refusal shape, kept separate so the success shapes
// stay fixed.
type healthErrorResponse struct {
	OK     bool   `json:"ok"`
	Status int    `json:"status"`
	Reason string `json:"reason"`
}

// handleHealth answers whether the process is up. A stale key file is counted
// as failed but never changes the status code: the process is up, and
// restarting the container would not fix a key file on someone else's origin.
func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if !detailRequested(r) {
		verified, failed, pending := countKeyStates(s.keyStatesSnapshot())

		writeJSON(w, http.StatusOK, healthResponse{
			OK:       true,
			Sites:    len(s.cfg.Sites),
			Verified: verified,
			Failed:   failed,
			Pending:  pending,
		})
		return
	}

	// Per-host detail names the operator's domains, so it needs a token. An
	// absent header cannot pass by accident: every configured token is at least
	// minTokenLength characters, and an empty string never matches one.
	if s.siteForToken(bearerToken(r.Header.Get("Authorization"))) == nil {
		s.log.Warn("health detail rejected", "reason", reasonUnknownToken, "remote", r.RemoteAddr)
		writeJSON(w, http.StatusUnauthorized, healthErrorResponse{
			OK:     false,
			Status: http.StatusUnauthorized,
			Reason: reasonUnknownToken,
		})
		return
	}

	// A recheck runs after the token check, so an unauthenticated caller can
	// never make this instance fetch anything.
	if recheckRequested(r) {
		s.recheckKeys(r.Context())
	}

	writeJSON(w, http.StatusOK, healthDetailResponse{
		OK:    true,
		Sites: len(s.cfg.Sites),
		Keys:  s.keyStatesSnapshot(),
	})
}

// recheckRequested reports whether the caller asked for a fresh key-file check.
// It follows the same rule as detailRequested: the parameter counts as soon as
// it is present, and only an explicit false switches it off.
func recheckRequested(r *http.Request) bool {
	query := r.URL.Query()
	if !query.Has("recheck") {
		return false
	}

	value := strings.TrimSpace(query.Get("recheck"))

	return value != "0" && !strings.EqualFold(value, "false")
}

// detailRequested reports whether the caller asked for the per-host view. The
// parameter counts as soon as it is present, so ?detail, ?detail=1 and
// ?detail=yes all work; only an explicit false switches it off, so a typo never
// silently downgrades the response the caller asked for.
func detailRequested(r *http.Request) bool {
	query := r.URL.Query()
	if !query.Has("detail") {
		return false
	}

	value := strings.TrimSpace(query.Get("detail"))

	return value != "0" && !strings.EqualFold(value, "false")
}

// keyStatesSnapshot returns a record for every configured host, defaulting to
// pending, so the counts always add up to the number of sites.
func (s *server) keyStatesSnapshot() map[string]keyRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()

	states := make(map[string]keyRecord, len(s.cfg.Sites))
	for _, site := range s.cfg.Sites {
		record, known := s.keyStates[site.Host]
		if !known {
			// Never checked: pending, with no time and no detail, because there
			// is no honest answer to either yet.
			record = keyRecord{State: keyStatePending}
		}
		states[site.Host] = record
	}

	return states
}

// countKeyStates turns a snapshot into the three counters /health reports. It
// reads the state only: the timestamps and details stay behind the token.
func countKeyStates(states map[string]keyRecord) (verified, failed, pending int) {
	for _, record := range states {
		switch record.State {
		case keyStateVerified:
			verified++
		case keyStateFailed:
			failed++
		default:
			pending++
		}
	}

	return verified, failed, pending
}

// siteForToken compares against every configured token in constant time and
// keeps the last match, so timing does not reveal which site a token belongs
// to.
func (s *server) siteForToken(token string) *Site {
	given := []byte(token)

	var found *Site
	for i := range s.cfg.Sites {
		site := &s.cfg.Sites[i]
		if subtle.ConstantTimeCompare(given, []byte(site.Token)) == 1 {
			found = site
		}
	}

	return found
}

// setKeyState records what is known about a host's key, stamped with the time
// and the reason. It is the only writer of keyStates.
func (s *server) setKeyState(host string, state keyState, detail string) {
	s.mu.Lock()
	s.keyStates[host] = newKeyRecord(state, detail)
	s.mu.Unlock()
}

// recordSubmissionKeyState records the key verdict a submission produced.
// detail is the engine's own explanation for a refusal, or a note that the
// engine has not finished validating yet, so /health explains "failed" instead
// of only saying it.
func (s *server) recordSubmissionKeyState(host string, state keyState, result submissionResult) {
	detail := ""
	if state != keyStateVerified {
		detail = result.Detail
		if detail == "" {
			detail = fmt.Sprintf("engine answered %d", result.EngineStatus)
		}
	}

	s.setKeyState(host, state, detail)
}

func httpStatusFor(result submissionResult) int {
	switch result.Reason {
	case reasonAccepted:
		return http.StatusOK
	case reasonBadRequest, reasonInvalidURL, reasonTooMany, reasonNonDefaultPort, reasonOutOfKeyScope, reasonEngineRejected:
		// The request will be refused identically if it is sent again, so it is
		// a client error, not a host problem and not a retryable one. An
		// out-of-scope URL belongs here and not with host_mismatch: the host was
		// right, so the caller has to move the key file or stop submitting URLs
		// outside its directory.
		return http.StatusBadRequest
	case reasonPayloadTooLarge:
		return http.StatusRequestEntityTooLarge
	case reasonKeyRejected:
		return http.StatusForbidden
	case reasonHostMismatch:
		return http.StatusUnprocessableEntity
	case reasonRateLimited, reasonThrottled:
		// Two different 429s share a status because the caller's action is the
		// same - back off - but the reasons stay apart so it knows whether it
		// annoyed this instance or the engine.
		return http.StatusTooManyRequests
	case reasonOverloaded:
		// Not the caller's fault and not permanent: this instance had no free
		// engine slot in time, so the request is worth retrying shortly.
		return http.StatusServiceUnavailable
	case reasonMethodNotAllowed:
		// The method is wrong, so the identical request will be refused
		// identically: a client error, not a retry.
		return http.StatusMethodNotAllowed
	default:
		// engine_error and anything unrecognised: the engine failed or the
		// request never landed, so the caller should try again later.
		return http.StatusBadGateway
	}
}

func bearerToken(header string) string {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// verifyAll checks every configured key file once and records the outcome. It
// runs alongside the listener rather than before it, so three unreachable
// origins cannot delay the first byte: each check has its own timeout and they
// would otherwise add up in front of a 5s container start period. The result is
// advisory - it only fills in /health, and no outcome here stops the process.
func (s *server) verifyAll(ctx context.Context) {
	for _, site := range s.cfg.Sites {
		checkCtx, cancel := context.WithTimeout(ctx, verifyTimeout)
		err := s.client.verifyKey(checkCtx, site)
		cancel()

		s.recordCheck(site, err)
	}
}

// recheckKeys runs the key-file check on demand, for every host this instance
// serves, and returns once the budget is spent. There is no timer and no
// periodic loop on purpose: a key file changes about never, so a background
// check would pay a permanent cost - a fetch per host, forever - for a rare
// event. An operator who has just fixed a key file can ask for a fresh answer
// instead, which is the only moment the answer matters.
func (s *server) recheckKeys(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, s.recheckTimeout)
	defer cancel()

	for _, site := range s.cfg.Sites {
		err := s.client.verifyKey(ctx, site)

		if ctx.Err() != nil {
			// The recheck ran out of budget, not the origin being wrong. A
			// check that was cut short learned nothing, and downgrading a good
			// state because this instance ran out of patience would be a lie
			// about someone else's key file.
			s.log.Debug("key file recheck cut short", "host", site.Host)
			continue
		}

		s.recordCheck(site, err)
	}
}

// recordCheck stores what a finished key check proved and logs it at the level
// the outcome deserves. A cancelled check writes nothing: shutdown is not a
// fault, and neither is a request that went away.
func (s *server) recordCheck(site Site, err error) {
	state := keyStateAfterCheck(err)
	if state == "" {
		// Cancelled: the process is shutting down, which is nobody's key file
		// being wrong. The recorded state stays as it is, and this is not
		// logged at ERROR - shutdown is not a fault.
		s.log.Debug("key file check cancelled", "host", site.Host)
		return
	}

	detail := ""
	if err != nil {
		detail = err.Error()
	}

	s.setKeyState(site.Host, state, detail)

	switch state {
	case keyStateVerified:
		s.log.Info("key file ok", "host", site.Host)
	case keyStateFailed:
		s.log.Error("key file check failed",
			"host", site.Host,
			"key_location", site.KeyLocation,
			"error", err,
		)
	default:
		// Nothing is known: the origin did not answer, answered too slowly,
		// or the check never ran to completion. Below ERROR because no key
		// has been shown to be broken.
		s.log.Warn("key file check inconclusive",
			"host", site.Host,
			"key_location", site.KeyLocation,
			"error", err,
		)
	}
}
