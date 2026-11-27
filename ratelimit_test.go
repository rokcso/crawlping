package main

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestRateBucketRefusesWhenTheBurstIsSpent(t *testing.T) {
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	// The clock stands still: this test measures the burst, not how fast the
	// machine can send eleven requests.
	clock := newFrozenClock()
	srv.rate.now = clock.Now

	for i := 0; i < rateBurstTokens; i++ {
		// A distinct URL per request: the resubmission window is not what this
		// test is about, and a repeated URL would be dropped before the engine
		// was ever called.
		recorder, decoded := postURL(t, srv, "Bearer "+testToken, "https://example.com/page-"+strconv.Itoa(i))

		if recorder.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200 (%s)", i+1, recorder.Code, recorder.Body.String())
		}
		if decoded["reason"] != nil {
			t.Fatalf("request %d: reason = %v, want none", i+1, decoded["reason"])
		}
	}

	if got := engine.calls(); got != rateBurstTokens {
		t.Fatalf("engine calls = %d, want %d", got, rateBurstTokens)
	}

	// The (capacity+1)th request in the same instant is refused locally.
	recorder, decoded := postURL(t, srv, "Bearer "+testToken, "https://example.com/last")

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (%s)", recorder.Code, recorder.Body.String())
	}
	if decoded["reason"] != reasonThrottled {
		t.Fatalf("reason = %v, want %q", decoded["reason"], reasonThrottled)
	}
	if got := recorder.Header().Get("Retry-After"); got == "" {
		t.Fatal("a throttled response carries no Retry-After header")
	}
	if decoded["retry_after_seconds"] != float64(1) {
		t.Fatalf("retry_after_seconds = %v, want 1", decoded["retry_after_seconds"])
	}
	if _, present := decoded["engine_status"]; present {
		t.Fatalf("a locally throttled batch reports an engine status: %v", decoded)
	}
	// Refused by the guard, so the engine never sees it: spending the egress IP
	// on this request is exactly what the guard exists to prevent.
	if got := engine.calls(); got != rateBurstTokens {
		t.Fatalf("engine calls after a local refusal = %d, want %d", got, rateBurstTokens)
	}
}

func TestRateLimitIsPerSite(t *testing.T) {
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTwoSiteServer(t, engine)

	clock := newFrozenClock()
	srv.rate.now = clock.Now

	for i := 0; i < rateBurstTokens; i++ {
		if recorder, _ := postURL(t, srv, "Bearer "+testToken, "https://example.com/page-"+strconv.Itoa(i)); recorder.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i+1, recorder.Code)
		}
	}

	if recorder, _ := postURL(t, srv, "Bearer "+testToken, "https://example.com/last"); recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("exhausted host answered %d, want 429", recorder.Code)
	}

	// The other site has spent nothing, so it must not inherit its neighbour's
	// limit: one noisy site is not allowed to silence the rest.
	recorder, decoded := postURL(t, srv, "Bearer "+otherToken, "https://"+otherHost+"/a")

	if recorder.Code != http.StatusOK {
		t.Fatalf("other site: status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
	if decoded["host"] != otherHost {
		t.Fatalf("other site: host = %v, want %q", decoded["host"], otherHost)
	}
	if got := engine.calls(); got != rateBurstTokens+1 {
		t.Fatalf("engine calls = %d, want %d", got, rateBurstTokens+1)
	}
}

func TestRefillAllowsTheNextRequest(t *testing.T) {
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	// A bucket of one token that refills every 100ms: the shortest interval a
	// test can observe without sleeping.
	srv.rate = newRateBucket(1, 100*time.Millisecond)
	clock := newFrozenClock()
	srv.rate.now = clock.Now

	if recorder, _ := postURL(t, srv, "Bearer "+testToken, "https://example.com/a"); recorder.Code != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200", recorder.Code)
	}

	recorder, decoded := postURL(t, srv, "Bearer "+testToken, "https://example.com/b")
	if recorder.Code != http.StatusTooManyRequests || decoded["reason"] != reasonThrottled {
		t.Fatalf("second request: status = %d reason = %v, want 429 %q", recorder.Code, decoded["reason"], reasonThrottled)
	}

	clock.advance(100 * time.Millisecond)

	if recorder, _ := postURL(t, srv, "Bearer "+testToken, "https://example.com/c"); recorder.Code != http.StatusOK {
		t.Fatalf("after a refill: status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}

	if got := engine.calls(); got != 2 {
		t.Fatalf("engine calls = %d, want 2", got)
	}
}

func TestRateBucketDoesNotRefillFromABackwardsClock(t *testing.T) {
	// Elapsed time is the only thing that mints tokens, and a clock that jumps
	// backwards must not mint any: a wall-clock step is not work done.
	bucket := newRateBucket(1, time.Second)
	clock := newFrozenClock()
	bucket.now = clock.Now

	if wait := bucket.take("example.com"); wait != 0 {
		t.Fatalf("first take waited %s, want 0", wait)
	}

	clock.advance(-time.Hour)

	if wait := bucket.take("example.com"); wait == 0 {
		t.Fatal("a backwards clock refilled the bucket")
	}
}
