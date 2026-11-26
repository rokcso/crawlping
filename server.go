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

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /submit", s.handleSubmit)
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
	// EngineStatus is the status the engine answered with, and is absent
	// whenever the engine was never called. Together with Reason it separates
	// "the engine refused this with 422 host_mismatch" from "this instance
	// refused with 422 host_mismatch": same reason, different owner, different
	// fix, and only this field tells them apart.
	EngineStatus int `json:"engine_status,omitempty"`
	// Reason names why the batch was not accepted. payload_too_large,
	// throttled and overloaded are the values the handler adds itself: no
	// submission was ever sent, so no engine produced them.
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
	// RetryAfterSeconds is set for rate_limited, throttled and overloaded, so a
	// caller can schedule the retry without guessing.
	RetryAfterSeconds int `json:"retry_after_seconds,omitempty"`
}

func (s *server) handleSubmit(w http.ResponseWriter, r *http.Request) {
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
			return
		}

		// The body could not be read at all, so there is nothing to parse and
		// nothing that was submitted.
		writeJSON(w, http.StatusBadRequest, submitResponse{
			Host:   site.Host,
			Status: http.StatusBadRequest,
			Reason: reasonBadRequest,
			Detail: "body could not be read",
		})
		return
	}

	var request submitRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		writeJSON(w, http.StatusBadRequest, submitResponse{
			Host:   site.Host,
			Status: http.StatusBadRequest,
			Reason: reasonBadRequest,
			Detail: `body must be a single {"urls": ["https://..."]} object with no trailing data`,
		})
		return
	}

	// duration_ms is the engine call and nothing else, so latency rising before
	// a throttle shows up in the numbers instead of only in the 429s. It is the
	// only early warning this tool can give: the engine's limit is not
	// published, so the trend is the signal.
	start := time.Now()
	result := s.client.submit(*site, request.URLs)
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
	attrs := make([]slog.Attr, 0, 7)
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
		slog.String("reason", result.Reason),
		slog.Int64("duration_ms", duration.Milliseconds()),
	)
	s.log.LogAttrs(r.Context(), slog.LevelInfo, "submit", attrs...)

	writeJSON(w, status, submitResponse{
		OK:                result.accepted(),
		Status:            status,
		Host:              site.Host,
		Submitted:         result.Submitted,
		EngineStatus:      result.EngineStatus,
		Reason:            result.Reason,
		Detail:            result.Detail,
		RetryAfterSeconds: retryAfterSeconds(result.RetryAfter),
	})
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
	case reasonBadRequest, reasonInvalidURL, reasonTooMany, reasonNonDefaultPort, reasonEngineRejected:
		// The request will be refused identically if it is sent again, so it is
		// a client error, not a host problem and not a retryable one.
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
