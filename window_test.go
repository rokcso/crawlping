package main

import (
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"
)

// --- the resubmission window ------------------------------------------------

// windowLen reads how many URLs the window remembers, under its lock, the way
// the server does.
func windowLen(w *submitWindow) int {
	w.mu.Lock()
	defer w.mu.Unlock()

	return len(w.entries)
}

// windowCap reads the window's entry cap, under its lock: it is not a constant,
// because a reload that raises the batch size grows it.
func windowCap(w *submitWindow) int {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.limit
}

func TestResubmitWindowDropsARepeatWithinTheWindow(t *testing.T) {
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	clock := newFrozenClock()
	srv.window.now = clock.Now

	if recorder, _ := postURL(t, srv, "Bearer "+testToken, "https://example.com/a"); recorder.Code != http.StatusOK {
		t.Fatalf("first: status = %d, want 200", recorder.Code)
	}

	clock.advance(time.Minute)

	// The same URL over the other method: there is one window and one submission
	// path, so which method a caller used cannot change what was already sent.
	recorder, body := getSubmit(t, srv, submitQuery("https://example.com/a"), "Bearer "+testToken)

	if recorder.Code != http.StatusOK {
		t.Fatalf("repeat: status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
	// Nothing to send is not a failure: the caller asked for something that is
	// already true, so the answer is a plain ok with nothing sent.
	if body["ok"] != true {
		t.Fatalf("a suppressed batch is not ok: %v", body)
	}
	if body["submitted"] != float64(0) || body["skipped"] != float64(1) {
		t.Fatalf("repeat: submitted = %v, skipped = %v, want 0 and 1", body["submitted"], body["skipped"])
	}
	if _, present := body["reason"]; present {
		t.Fatalf("a suppressed batch carries a reason: %v", body)
	}
	if _, present := body["engine_status"]; present {
		t.Fatalf("a suppressed batch claims an engine status: %v", body)
	}
	if got := engine.calls(); got != 1 {
		t.Fatalf("engine calls = %d, want 1", got)
	}
}

func TestResubmitWindowReleasesAfterTheWindow(t *testing.T) {
	// A suppressed URL must not extend its own window: the timestamp is written
	// when a URL is actually sent, so a URL retried every minute is sent again a
	// window after the send, not a window after the last attempt.
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	clock := newFrozenClock()
	srv.window.now = clock.Now

	postURL(t, srv, "Bearer "+testToken, "https://example.com/a")

	clock.advance(resubmitWindow - time.Second)

	if _, body := postURL(t, srv, "Bearer "+testToken, "https://example.com/a"); body["skipped"] != float64(1) {
		t.Fatalf("inside the window: skipped = %v, want 1", body["skipped"])
	}

	// A window after the send, with the suppressed attempt in between.
	clock.advance(time.Second)

	recorder, body := postURL(t, srv, "Bearer "+testToken, "https://example.com/a")

	if recorder.Code != http.StatusOK {
		t.Fatalf("after the window: status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
	if body["submitted"] != float64(1) {
		t.Fatalf("after the window: submitted = %v, want 1", body["submitted"])
	}
	if got := engine.calls(); got != 2 {
		t.Fatalf("engine calls = %d, want 2", got)
	}
}

func TestResubmitWindowCountsSuppressedURLsBesideSentOnes(t *testing.T) {
	// A partly suppressed batch still sends the rest, and the dropped URL is
	// counted rather than vanishing from a response that says submitted: 1.
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	clock := newFrozenClock()
	srv.window.now = clock.Now

	postURL(t, srv, "Bearer "+testToken, "https://example.com/a")
	clock.advance(time.Minute)

	recorder, body := postURL(t, srv, "Bearer "+testToken, "https://example.com/a", "https://example.com/b")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
	if body["submitted"] != float64(1) || body["skipped"] != float64(1) {
		t.Fatalf("submitted = %v, skipped = %v, want 1 and 1", body["submitted"], body["skipped"])
	}
	if got := engine.urls(); len(got) != 1 || got[0] != "https://example.com/b" {
		t.Fatalf("engine received %v, want only the url that was not suppressed", got)
	}
}

func TestResubmitWindowIsPerHost(t *testing.T) {
	// Two sites serve the same path. The window is keyed on the site's host, so
	// one site's submission cannot silence another's URL.
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTwoSiteServer(t, engine)

	clock := newFrozenClock()
	srv.window.now = clock.Now

	if recorder, _ := postURL(t, srv, "Bearer "+testToken, "https://example.com/a"); recorder.Code != http.StatusOK {
		t.Fatalf("first site: status = %d, want 200", recorder.Code)
	}

	recorder, body := postURL(t, srv, "Bearer "+otherToken, "https://"+otherHost+"/a")

	if recorder.Code != http.StatusOK {
		t.Fatalf("second site: status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
	if body["submitted"] != float64(1) {
		t.Fatalf("second site: submitted = %v, want 1", body["submitted"])
	}
	if _, present := body["skipped"]; present {
		t.Fatalf("second site was suppressed by the first: %v", body)
	}
	if got := engine.calls(); got != 2 {
		t.Fatalf("engine calls = %d, want 2", got)
	}
}

func TestResubmitWindowForgetsAFailedSubmission(t *testing.T) {
	// Only an accepted batch proves the URLs were taken. A failure leaves the
	// caller a retry to make, and suppressing that retry would answer 200
	// "submitted: 0" to a caller whose first attempt never landed.
	engine := newFakeEngine(t, http.StatusInternalServerError)
	srv := newTestServer(t, engine)

	clock := newFrozenClock()
	srv.window.now = clock.Now

	if recorder, _ := postURL(t, srv, "Bearer "+testToken, "https://example.com/a"); recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", recorder.Code)
	}

	engine.setStatus(http.StatusOK)

	recorder, body := postURL(t, srv, "Bearer "+testToken, "https://example.com/a")

	if recorder.Code != http.StatusOK {
		t.Fatalf("retry: status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
	if body["submitted"] != float64(1) {
		t.Fatalf("retry: submitted = %v, want the url sent again", body["submitted"])
	}
	if got := engine.calls(); got != 2 {
		t.Fatalf("engine calls = %d, want 2", got)
	}
}

func TestResubmitWindowStaysBounded(t *testing.T) {
	// A caller can send arbitrary URLs, so the map has a cap: an unbounded one is
	// a leak that caller controls. Past the cap the oldest entry goes, because
	// every entry expires anyway and the oldest is the next to go.
	const limit = 4

	window := newSubmitWindow(resubmitWindow, limit)
	clock := newFrozenClock()
	window.now = clock.Now

	for i := 0; i < limit; i++ {
		window.record("example.com", []string{"https://example.com/page-" + strconv.Itoa(i)})
		clock.advance(time.Second)
	}

	if got := windowLen(window); got != limit {
		t.Fatalf("window holds %d urls, want %d", got, limit)
	}

	window.record("example.com", []string{"https://example.com/one-more"})

	if got := windowLen(window); got != limit {
		t.Fatalf("window holds %d urls after one insert past the cap, want %d", got, limit)
	}

	// page-0 was the oldest, so it is the entry that went. The neighbour is asked
	// about first because a URL that is not remembered is claimed as it is looked
	// up, and a claim is a write: it needs room like any other entry, so it may
	// evict the oldest one. Asking about the suppressed neighbour first keeps both
	// assertions about what the cap actually kept, in the order they hold.
	if _, skipped := window.keep("example.com", []string{"https://example.com/page-1"}); skipped != 1 {
		t.Fatalf("a recent entry was evicted: skipped %d, want 1", skipped)
	}
	if kept, skipped := window.keep("example.com", []string{"https://example.com/page-0"}); skipped != 0 || len(kept) != 1 {
		t.Fatalf("the oldest entry survived the cap: skipped %d, kept %v", skipped, kept)
	}

	// A batch larger than the whole window cannot be remembered in full. The cap
	// still wins: it is what keeps arbitrary input from growing this structure.
	window.record("example.com", []string{
		"https://example.com/a", "https://example.com/b", "https://example.com/c",
		"https://example.com/d", "https://example.com/e", "https://example.com/f",
	})

	if got := windowLen(window); got > limit {
		t.Fatalf("window holds %d urls after an oversized batch, want at most %d", got, limit)
	}
}

func TestResubmitWindowIsSafeUnderConcurrency(t *testing.T) {
	window := newSubmitWindow(resubmitWindow, 8)

	var wg sync.WaitGroup

	for worker := 0; worker < 8; worker++ {
		wg.Add(1)

		go func(worker int) {
			defer wg.Done()

			urls := []string{
				"https://example.com/page-" + strconv.Itoa(worker),
				"https://example.com/shared",
			}

			for i := 0; i < 50; i++ {
				kept, skipped := window.keep("example.com", urls)
				if skipped > len(urls) {
					t.Errorf("skipped %d of %d urls", skipped, len(urls))
				}
				window.record("example.com", kept)
			}
		}(worker)
	}

	wg.Wait()

	if got := windowLen(window); got > 8 {
		t.Fatalf("window holds %d urls, want at most 8", got)
	}
}

func TestResubmitWindowClaimsAURLUntilItIsReleasedOrRecorded(t *testing.T) {
	// The window is the check, and the engine call is the act. Claiming the URL
	// under the window's own lock is what stops two concurrent requests from both
	// passing the check and both spending quota on one URL.
	window := newSubmitWindow(resubmitWindow, 8)
	clock := newFrozenClock()
	window.now = clock.Now

	urls := []string{"https://example.com/a"}

	if kept, skipped := window.keep("example.com", urls); skipped != 0 || len(kept) != 1 {
		t.Fatalf("first keep: kept %v, skipped %d, want the url kept", kept, skipped)
	}

	// A second look while the first batch is in flight sees the claim, and skips
	// the URL: this is the half that used to be missing.
	if kept, skipped := window.keep("example.com", urls); skipped != 1 || len(kept) != 0 {
		t.Fatalf("second keep: kept %v, skipped %d, want the claim to suppress it", kept, skipped)
	}

	// A batch that did not land gives its claim back, so the caller's retry is
	// sent instead of being suppressed for a whole window.
	window.release("example.com", urls)
	if kept, skipped := window.keep("example.com", urls); skipped != 0 || len(kept) != 1 {
		t.Fatalf("after release: kept %v, skipped %d, want the url kept again", kept, skipped)
	}

	// A recorded send is evidence that the engine took the URL, so a release can
	// never un-send it.
	window.record("example.com", urls)
	window.release("example.com", urls)
	if kept, skipped := window.keep("example.com", urls); skipped != 1 || len(kept) != 0 {
		t.Fatalf("after record: kept %v, skipped %d, want the send remembered", kept, skipped)
	}

	// And a claim nobody released or promoted is self-healing: it ages out with
	// the window like a send would, so a leak cannot suppress a URL forever.
	leaked := []string{"https://example.com/leaked"}
	if _, skipped := window.keep("example.com", leaked); skipped != 0 {
		t.Fatalf("claiming a new url was skipped: %d", skipped)
	}
	clock.advance(resubmitWindow)
	if kept, skipped := window.keep("example.com", leaked); skipped != 0 || len(kept) != 1 {
		t.Fatalf("a claim outlived the window: kept %v, skipped %d", kept, skipped)
	}
}
