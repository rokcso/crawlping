package main

import (
	"context"
	"fmt"
	"time"
)

// verifyTimeout bounds one key-file check, so a slow origin cannot hold the
// background check open indefinitely.
const verifyTimeout = 10 * time.Second

// recheckTimeout bounds one on-demand recheck of the key files.
const recheckTimeout = 5 * time.Second

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

// keyStatesSnapshot returns a record for every configured host, defaulting to
// pending, so the counts always add up to the number of sites.
func (s *server) keyStatesSnapshot(rt *runtime) map[string]keyRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()

	states := make(map[string]keyRecord, len(rt.sites))
	for _, site := range rt.sites {
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

// verifyAll checks every configured key file once and records the outcome. It
// runs alongside the listener rather than before it, so three unreachable
// origins cannot delay the first byte: each check has its own timeout and they
// would otherwise add up in front of a 5s container start period. The result is
// advisory - it only fills in /health, and no outcome here stops the process.
func (s *server) verifyAll(ctx context.Context) {
	rt := s.rt.Load()
	s.verifySites(ctx, rt.client, rt.sites)
}

// verifySites checks every key file in the given list. The client and the site
// list are passed together rather than read from the server, so a reload during
// a run cannot mix the old client with the new sites.
func (s *server) verifySites(ctx context.Context, client *indexNowClient, sites []Site) {
	for _, site := range sites {
		checkCtx, cancel := context.WithTimeout(ctx, verifyTimeout)
		err := client.verifyKey(checkCtx, site)
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
func (s *server) recheckKeys(ctx context.Context, rt *runtime) {
	ctx, cancel := context.WithTimeout(ctx, s.recheckTimeout)
	defer cancel()

	for _, site := range rt.sites {
		err := rt.client.verifyKey(ctx, site)

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
