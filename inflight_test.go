package main

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The two-level guard. The per-host cap is the fairness half of the concurrency
// limit: the instance-wide cap keeps the engine's side of the connection finite,
// and the per-host cap stops one site from holding all of it.

func TestPerHostInFlightLimit(t *testing.T) {
	// One site gets the whole instance cap. Two or more sites split it, so one
	// noisy site can hold at most half and the rest keep making progress.
	cases := map[int]int{
		0:  maxInFlightEngineCalls,
		1:  maxInFlightEngineCalls,
		2:  maxInFlightEngineCalls / 2,
		3:  maxInFlightEngineCalls / 2,
		50: maxInFlightEngineCalls / 2,
	}

	for sites, want := range cases {
		if got := perHostInFlightLimit(sites); got != want {
			t.Fatalf("%d sites: per-host limit = %d, want %d", sites, got, want)
		}
	}

	for sites := 0; sites <= 100; sites++ {
		got := perHostInFlightLimit(sites)
		if got < 1 {
			t.Fatalf("%d sites: per-host limit = %d, which would refuse every request", sites, got)
		}
		if got > maxInFlightEngineCalls {
			t.Fatalf("%d sites: per-host limit = %d, wider than the instance cap", sites, got)
		}
	}
}

func TestSingleSiteServerKeepsEverySlot(t *testing.T) {
	// The no-regression rule: a deployment with one site has every engine slot
	// it had before the per-host cap existed.
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	if got := cap(srv.inflight.global); got != maxInFlightEngineCalls {
		t.Fatalf("instance cap = %d, want %d", got, maxInFlightEngineCalls)
	}
	if got := srv.inflight.perHost; got != maxInFlightEngineCalls {
		t.Fatalf("per-host cap = %d, want the instance cap %d", got, maxInFlightEngineCalls)
	}
}

func TestMultiSiteServerHalvesThePerHostCap(t *testing.T) {
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTwoSiteServer(t, engine)

	if got := cap(srv.inflight.global); got != maxInFlightEngineCalls {
		t.Fatalf("instance cap = %d, want %d", got, maxInFlightEngineCalls)
	}
	if got := srv.inflight.perHost; got != maxInFlightEngineCalls/2 {
		t.Fatalf("per-host cap = %d, want half the instance cap (%d)", got, maxInFlightEngineCalls/2)
	}
}

func TestGuardTakesTheSiteSlotFirst(t *testing.T) {
	// Both caps are full, and the answer says the site, not the instance: the
	// site slot is tried first, so a request that cannot even take its own
	// host's slot is refused without touching - let alone holding - an
	// instance-wide one.
	guard := newInflightGuard(1, 1)
	guard.wait = 10 * time.Millisecond

	guard.hostSlots("example.com") <- struct{}{}
	guard.global <- struct{}{}

	if decision := guard.acquire(context.Background(), "example.com"); decision != guardRefusedSite {
		t.Fatalf("decision = %v, want guardRefusedSite", decision)
	}

	// Nothing was acquired, so nothing may be left held.
	if held := len(guard.global); held != 1 {
		t.Fatalf("global slots in use = %d, want the one the test took", held)
	}
}

func TestGuardRefusesTheInstanceAndFreesTheSiteSlot(t *testing.T) {
	// The site has room, the instance does not. The refusal names the instance,
	// and the site slot the request did take is given back: otherwise a busy
	// moment instance-wide would leave that site short of its own capacity long
	// after the instance recovered.
	guard := newInflightGuard(2, 1)
	guard.wait = 10 * time.Millisecond

	guard.global <- struct{}{}

	if decision := guard.acquire(context.Background(), "example.com"); decision != guardRefusedInstance {
		t.Fatalf("decision = %v, want guardRefusedInstance", decision)
	}

	if held := len(guard.hostSlots("example.com")); held != 0 {
		t.Fatalf("a refused request kept %d of the site's slots", held)
	}
}

func TestGuardServesAnotherSiteWhileOneIsAtItsCap(t *testing.T) {
	// The point of the change, driven through the handler: one site holding its
	// whole share does not starve its neighbour, because the neighbour's slots
	// and most of the instance's are still free.
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTwoSiteServer(t, engine)
	srv.inflight.wait = 20 * time.Millisecond

	held := srv.inflight.hostSlots("example.com")
	for i := 0; i < srv.inflight.perHost; i++ {
		held <- struct{}{}
	}
	t.Cleanup(func() {
		for i := 0; i < srv.inflight.perHost; i++ {
			<-held
		}
	})

	recorder, decoded := postURL(t, srv, "Bearer "+testToken, "https://example.com/a")

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("the capped site: status = %d, want 503 (%s)", recorder.Code, recorder.Body.String())
	}
	if decoded["reason"] != reasonOverloaded {
		t.Fatalf("the capped site: reason = %v, want %q", decoded["reason"], reasonOverloaded)
	}
	detail, _ := decoded["detail"].(string)
	if !strings.Contains(detail, "this site") {
		t.Fatalf("detail %q does not name this site's own cap", detail)
	}
	if !strings.Contains(detail, strconv.Itoa(srv.inflight.perHost)) {
		t.Fatalf("detail %q does not carry the per-host cap %d", detail, srv.inflight.perHost)
	}

	recorder, decoded = postURL(t, srv, "Bearer "+otherToken, "https://"+otherHost+"/a")

	if recorder.Code != http.StatusOK {
		t.Fatalf("the neighbouring site: status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
	if decoded["host"] != otherHost {
		t.Fatalf("the neighbouring site: host = %v, want %q", decoded["host"], otherHost)
	}
	if got := engine.calls(); got != 1 {
		t.Fatalf("engine calls = %d, want 1 (only the site that had a slot)", got)
	}
}

func TestGuardIsSafeAndLeavesNothingHeldUnderContention(t *testing.T) {
	// Not a timing test: the guard's waits are bounded, so the properties that
	// can still break are a slot that is taken twice or never given back. Every
	// goroutine ends up with no slots held, whatever mix of refusals and
	// acquisitions it saw - and the race detector is the other half of this
	// test.
	const (
		hosts   = 3
		workers = 12
		rounds  = 25
	)

	guard := newInflightGuard(1, 2)
	guard.wait = 2 * time.Millisecond

	names := make([]string, 0, hosts)
	for i := 0; i < hosts; i++ {
		names = append(names, "site-"+strconv.Itoa(i)+".example")
	}

	var wg sync.WaitGroup

	for worker := 0; worker < workers; worker++ {
		wg.Add(1)

		go func(worker int) {
			defer wg.Done()

			host := names[worker%hosts]

			for round := 0; round < rounds; round++ {
				if guard.acquire(context.Background(), host) == guardAcquired {
					guard.release(host)
				}
			}
		}(worker)
	}

	wg.Wait()

	if held := len(guard.global); held != 0 {
		t.Fatalf("%d instance slots are still held", held)
	}

	for _, host := range names {
		if held := len(guard.hostSlots(host)); held != 0 {
			t.Fatalf("%s still holds %d of its slots", host, held)
		}
	}

	// And the guard still works afterwards: a leaked slot would show up here as
	// a refusal.
	if decision := guard.acquire(context.Background(), names[0]); decision != guardAcquired {
		t.Fatalf("after the contention: decision = %v, want guardAcquired", decision)
	}
	guard.release(names[0])
}
