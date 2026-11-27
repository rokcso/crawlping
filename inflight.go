package main

import (
	"context"
	"sync"
	"time"
)

// Guard defaults. IndexNow does not publish its limits, so these are
// deliberately conservative: the job is to keep one site's retry loop from
// burning the operator's egress IP, not to squeeze the last request out of the
// engine. Raise them only with a reason and never past what the engine
// tolerates from one IP.
const (
	// maxInFlightEngineCalls is how many engine requests may be open at once,
	// across every site, so a burst on one host cannot exhaust sockets either.
	// One host may hold at most perHostInFlightLimit of them.
	maxInFlightEngineCalls = 8
	// inFlightWait is how long a submission waits for its slots - one for the
	// site, then one for the instance - before it is told which cap it hit.
	inFlightWait = 2 * time.Second
	// busyRetryAfter is the retry hint an overloaded answer carries.
	busyRetryAfter = time.Second
)

// inflightGuard caps how many engine requests this instance has open at once,
// at two levels: one across the whole instance, one per host.
//
// A single global cap is enough while there is one site. With several it is
// unfair: eight slots shared by every host means one site with a retry loop can
// hold all eight, and every other site is answered 503 without ever having had a
// slot - the same starvation the per-host rate bucket exists to remove, one
// layer down. The per-host cap bounds what one site can hold so the other sites
// keep making progress.
//
// A buffered channel is each semaphore: sending takes a slot, receiving frees
// one, and no dependency is needed for that.
type inflightGuard struct {
	// global is the instance-wide semaphore, maxInFlightEngineCalls slots wide.
	global chan struct{}
	// perHost is how many slots one host may hold at once. It comes from
	// perHostInFlightLimit and is never wider than the instance-wide cap.
	perHost int
	// wait is the whole budget one request may spend taking both slots. It is
	// one budget rather than one per cap, so a submission's worst case stays
	// inFlightWait instead of twice that.
	wait time.Duration

	mu sync.Mutex
	// hosts holds each host's own semaphore, created on first use. The key is
	// always a host from the config and never a value out of a request, so the
	// map is bounded by the config - the same rule the metric labels follow.
	hosts map[string]chan struct{}
}

// guardDecision is what acquire decided. The refusal values are separate because
// the caller has to be able to say which cap refused it: slowing one site down
// and slowing the whole instance down are different instructions.
type guardDecision int

const (
	// guardAcquired means both slots are held and have to be released.
	guardAcquired guardDecision = iota
	// guardRefusedSite means this site already held its own cap of slots.
	guardRefusedSite
	// guardRefusedInstance means the site had room but the instance did not.
	guardRefusedInstance
	// guardRefusedCancelled means the caller went away while waiting. Nothing
	// was acquired, so there is nothing to release.
	guardRefusedCancelled
)

// perHostInFlightLimit is how many engine calls one host may have open at once,
// derived from how many sites this instance serves.
//
// One site means the per-host cap could only ever duplicate the global one, so
// it is the global cap itself: a single-site deployment is exactly one host, and
// capping it below the instance's own ceiling would throw away half the engine
// concurrency the deployment is allowed, for a starvation problem that cannot
// exist with one site. That is the no-regression rule: a deployment that worked
// at eight slots keeps working at eight.
//
// More than one site halves it, so at least two hosts can be at their cap at the
// same time and one noisy host can hold at most half the instance's slots while
// the other half keeps serving everyone else. Half is the smallest split that
// removes the starvation; a smaller share would refuse each site sooner than the
// instance needs to.
//
// This is answered once, when the process starts. The share is the width of a
// semaphore channel, and a channel that already has slots held cannot be resized
// without losing them, so a reload keeps the share it booted with and only warns
// when the site count has crossed the threshold (see applyConfigBytes). A restart
// is what applies the share the current site count implies.
func perHostInFlightLimit(sites int) int {
	if sites <= 1 {
		return maxInFlightEngineCalls
	}

	limit := maxInFlightEngineCalls / 2
	if limit < 1 {
		// A cap of zero would refuse every request, so one slot is the floor
		// even if the global cap were ever lowered to one.
		limit = 1
	}

	return limit
}

// newInflightGuard builds the two-level guard. A per-host cap wider than the
// instance-wide one is clamped: it could never be reached, and keeping the
// fields consistent makes the refusal detail honest.
func newInflightGuard(perHost, global int) *inflightGuard {
	if global < 1 {
		global = 1
	}
	if perHost < 1 {
		perHost = 1
	}
	if perHost > global {
		perHost = global
	}

	return &inflightGuard{
		global:  make(chan struct{}, global),
		perHost: perHost,
		wait:    inFlightWait,
		hosts:   make(map[string]chan struct{}, 4),
	}
}

// hostSlots returns one host's semaphore, creating it on first use. The key is
// always a configured host, so this map cannot grow past the config.
func (g *inflightGuard) hostSlots(host string) chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()

	slots, known := g.hosts[host]
	if !known {
		slots = make(chan struct{}, g.perHost)
		g.hosts[host] = slots
	}

	return slots
}

// acquire takes one of the host's slots and then one of the instance's, waiting
// at most g.wait in total, and reports which cap refused when it could not take
// both. A caller that is refused holds nothing and has nothing to release.
//
// The order is host first, then instance, and it is what keeps this
// deadlock-free: a goroutine that holds a global slot never waits for a host
// slot (the submission path only ever releases), so there is no cycle to close.
// Doing it the other way round - global, then host - would hold an instance slot
// hostage behind whatever a single site's traffic is doing.
//
// It is starvation-free for the same reason plus the per-host cap: at most
// perHost requests of one host are ever queued on the global semaphore, that
// queue is served in arrival order, and no host can add more than its cap, so no
// host can own the global queue. A request that cannot take both slots inside
// the budget is refused rather than waiting forever, so nothing waits
// indefinitely for either cap.
func (g *inflightGuard) acquire(ctx context.Context, host string) guardDecision {
	hostSlots := g.hostSlots(host)

	deadline := time.Now().Add(g.wait)

	if !takeSlot(ctx, hostSlots, deadline) {
		if ctx.Err() != nil {
			return guardRefusedCancelled
		}

		return guardRefusedSite
	}

	if !takeSlot(ctx, g.global, deadline) {
		// Give the site's slot back before answering: a request that never
		// reached the engine must not keep this host's slot, or one busy moment
		// instance-wide would leave the site paying for it afterwards.
		<-hostSlots

		if ctx.Err() != nil {
			return guardRefusedCancelled
		}

		return guardRefusedInstance
	}

	return guardAcquired
}

// release frees both slots acquire took, the instance's first: a sibling request
// of the same host then finds both free in the order it needs them, instead of
// taking the host slot and bouncing off a still-full global semaphore.
func (g *inflightGuard) release(host string) {
	<-g.global
	<-g.hostSlots(host)
}

// takeSlot waits until deadline for a slot on slots.
//
// A slot that is free right now is taken whatever the caller's context says: the
// submission path deliberately does not cancel a request to the engine because
// the caller hung up (see submit), so a cancelled caller that finds a free slot
// is still served. Only a caller that has to wait is dropped when it goes away.
func takeSlot(ctx context.Context, slots chan struct{}, deadline time.Time) bool {
	select {
	case slots <- struct{}{}:
		return true
	default:
	}

	remaining := time.Until(deadline)
	if remaining <= 0 {
		// Out of budget: one last look, no waiting. A slot that is free right
		// now still counts, and waiting past the deadline is exactly what the
		// budget forbids.
		select {
		case slots <- struct{}{}:
			return true
		default:
			return false
		}
	}

	timer := time.NewTimer(remaining)
	defer timer.Stop()

	select {
	case slots <- struct{}{}:
		return true
	case <-timer.C:
		return false
	case <-ctx.Done():
		// The caller is gone, so there is nobody to answer and no reason to
		// hold a slot. Reported as no slot, which is what it is.
		return false
	}
}
