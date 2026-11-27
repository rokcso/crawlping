package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// maxBodyBytes is the default submission body bound, used when the config does
// not set limits.max_body_bytes. The effective bound is server.maxBodyBytes,
// which comes from the config: the two limits move together, and loadConfig
// refuses a pair where the body could not carry the URL count.
const maxBodyBytes = defaultMaxBodyBytes

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

type server struct {
	// rt is everything the config file decides. It is a pointer that is swapped
	// whole on SIGHUP rather than fields edited in place, so a request either
	// sees the old config or the new one and never a half-applied mixture, and
	// readers need no lock to get a consistent view.
	//
	// The stateful guards below are deliberately not in it: a reload must not
	// forget a throttle that is still running (that would hand out the burst
	// again) and must not reset the counters /metrics exists to report. A reload
	// changes who may submit and where submissions go; it does not rewrite what
	// already happened.
	rt  atomic.Pointer[runtime]
	log *slog.Logger

	mu sync.RWMutex
	// keyStates holds the last recorded state per host. A host that is absent
	// is pending, which is the honest default. setKeyState is the only writer of
	// a verdict; applyConfigBytes is the only remover, and it drops exactly the
	// hosts the config no longer carries, because a fact about a host this
	// instance stopped serving is not worth keeping and would be inherited by
	// that host if it were added back.
	keyStates map[string]keyRecord

	// rate and inflight bound what this instance spends its own egress IP on.
	// Without them a leaked token or a retry loop in one caller can push the
	// operator's IP into the engine's throttle, which is the exact harm this
	// project exists to avoid: cross-domain abuse is already impossible (a token
	// is bound to one host), frequency was not bounded at all.
	rate     *rateBucket
	inflight *inflightGuard
	// metrics is what this instance has done, counted. It lives here rather than
	// on the client because the decisions are made here: a batch refused locally
	// is a decision too, and the client never sees it.
	metrics *submissionMetrics
	// window is the per-URL half of the quota guard: a URL it remembers is
	// dropped from the batch before the request is built, because every
	// submitted URL spends crawl quota whether or not the engine already had it.
	window *submitWindow
	// recheckTimeout bounds one ?recheck=1, and is a field so a test can spend
	// a few milliseconds instead of five seconds.
	recheckTimeout time.Duration
}

// runtime is one loaded config and the client that submits for it. Everything
// here is derived from the config file, and the Config it points at is never
// mutated after construction, which is what makes handing the same slices to
// concurrent requests safe.
type runtime struct {
	cfg    *Config
	sites  []Site
	client *indexNowClient
	// maxURLs and maxBodyBytes are the effective submission limits. They are
	// fields rather than reads of cfg.Limits because a Config built without
	// loadConfig has zero values, and those have to mean the default here too -
	// otherwise a zero body cap would answer 413 to every request.
	maxURLs      int
	maxBodyBytes int
	// source is the config bytes this runtime was built from, when it came from
	// a file. It is how a reload tells "the operator changed nothing" apart from
	// "this process is reading an inode the host has already replaced".
	source []byte
}

func newRuntime(cfg *Config, client *indexNowClient) *runtime {
	// Zero means "not set", the same rule the file follows, so a Config built by
	// hand (a test, or any caller that did not come through loadConfig) takes the
	// defaults instead of answering 413 to everything.
	maxURLs, maxBodyBytes := cfg.Limits.MaxURLs, cfg.Limits.MaxBodyBytes
	if maxURLs == 0 {
		maxURLs = defaultMaxURLs
	}
	if maxBodyBytes == 0 {
		maxBodyBytes = defaultMaxBodyBytes
	}

	return &runtime{
		cfg:          cfg,
		sites:        cfg.Sites,
		client:       client,
		maxURLs:      maxURLs,
		maxBodyBytes: maxBodyBytes,
	}
}

func newServer(cfg *Config, client *indexNowClient, log *slog.Logger) *server {
	return newServerWithSource(cfg, client, log, nil)
}

// newServerWithSource is newServer for a config that came from a file: source is
// the bytes it was built from, and it is what lets a reload tell "nothing
// changed" apart from "this process is reading an inode the host has already
// replaced". Only cmdServe passes it; tests and hand-built configs pass nil.
func newServerWithSource(cfg *Config, client *indexNowClient, log *slog.Logger, source []byte) *server {
	// The window has to be able to remember one full batch, or a raised
	// max_urls_per_request would send URLs it then silently forgets, and every
	// one of them would be resubmitted immediately. Its own cap still applies on
	// top, as the bound on arbitrary input. This is the size the process starts
	// with; applyConfigBytes grows it to match a reload that raised the count, so
	// the property holds for every config this instance ever serves, not only
	// the one it booted on.
	tracked := maxTrackedURLs
	if maxURLs := newRuntime(cfg, client).maxURLs; maxURLs > tracked {
		tracked = maxURLs
	}

	s := &server{
		log:            log,
		keyStates:      make(map[string]keyRecord, len(cfg.Sites)),
		rate:           newRateBucket(rateBurstTokens, rateRefillInterval),
		inflight:       newInflightGuard(perHostInFlightLimit(len(cfg.Sites)), maxInFlightEngineCalls),
		metrics:        newSubmissionMetrics(),
		window:         newSubmitWindow(resubmitWindow, tracked),
		recheckTimeout: recheckTimeout,
	}
	rt := newRuntime(cfg, client)
	rt.source = source
	s.rt.Store(rt)

	return s
}

// reload swaps in the config that is on disk now, and is what SIGHUP runs.
//
// It is the reason adding a site does not need a restart: the running process
// keeps its listener, its in-flight submissions and its counters, and the next
// request sees the new site list. A config that does not load changes nothing -
// the running one keeps serving - because a typo in a file must not be able to
// take down an API that a Worker is calling.
func (s *server) reload(ctx context.Context, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}

	cfg, err := parseConfig(raw)
	if err != nil {
		return err
	}

	// The bytes are compared, not the file. A container whose config is mounted
	// as a single file keeps the inode it was started with, so an atomic replace
	// on the host - which is what `site add` does, on purpose - is invisible
	// here: the signal arrives, the config loads, nothing is added. That failure
	// is silent, which makes it worse than an error, so it gets a line of its
	// own. Nothing else can be done from inside the wrong inode.
	if prev := s.rt.Load(); prev != nil && prev.source != nil && bytes.Equal(prev.source, raw) {
		s.log.Warn("config reload read the same bytes it already had: if the file was just edited, this container is reading a pinned inode - mount its directory instead of the file",
			"path", path, "sites", len(cfg.Sites))
	}

	return s.applyConfigBytes(ctx, cfg, raw)
}

// applyConfig installs a config that has already loaded. reload is the only
// production caller; it is separate so a test can install a Config directly,
// which is the only way to point a runtime at a local key server without
// weakening the host rule loadConfig enforces.
func (s *server) applyConfig(ctx context.Context, cfg *Config) error {
	return s.applyConfigBytes(ctx, cfg, nil)
}

func (s *server) applyConfigBytes(ctx context.Context, cfg *Config, raw []byte) error {
	next := newRuntime(cfg, newIndexNowClient(cfg.IndexNowEndpoint))
	next.source = raw
	prev := s.rt.Swap(next)

	// The window's cap has to keep up with max_urls_per_request, or a reload that
	// raised the count would leave a table too small to remember one full batch:
	// the batch goes out, the table cannot hold it, and every URL in it is
	// forgotten and sent again on the next attempt. Growing is the only direction
	// needed - the cap is a memory bound, and a shrunk one would evict entries a
	// caller was already promised - which is why there is no setLimit beside it.
	want := maxTrackedURLs
	if next.maxURLs > want {
		want = next.maxURLs
	}
	s.window.growLimit(want)

	// The per-site share is the width of a semaphore channel that may already have
	// slots held, and a channel cannot be resized without losing them, so this is
	// decided once when the process starts and a reload deliberately keeps it.
	// That matters when the site count crosses the one-site threshold, because the
	// two answers differ: a deployment that started with one site keeps all 8 slots
	// per host after a second site is added, while the documented split for two
	// sites is 4. Keeping it silently would leave an operator believing the split
	// is in force, so the reload says which number runs and which one a restart
	// would apply.
	if prev != nil {
		if running, afterRestart := perHostInFlightLimit(len(prev.sites)), perHostInFlightLimit(len(next.sites)); running != afterRestart {
			s.log.Warn("per-site engine share is decided at startup; this reload kept the running one",
				"running", s.inflight.perHost,
				"after_restart", afterRestart,
				"sites", len(next.sites),
			)
		}
	}

	// A key verdict is a fact about a host this instance serves. Once a host leaves
	// the config, the record says nothing about anything this instance can submit
	// for, and keeping it is what made a re-added host inherit an old "verified"
	// with an old checked_at without being re-checked: only hosts that are new
	// relative to the previous config are checked, so a returning host would have
	// no evidence of its own and would fall back on last time's. Dropping the
	// record is what makes it come back pending and get its key file checked.
	s.mu.Lock()
	served := make(map[string]bool, len(next.sites))
	for _, site := range next.sites {
		served[site.Host] = true
	}
	for host := range s.keyStates {
		if !served[host] {
			delete(s.keyStates, host)
		}
	}
	s.mu.Unlock()

	// Only hosts the previous config did not already carry are checked. The
	// other states are evidence about files that have not changed, and a reload
	// must not replace a verdict the engine itself gave with the weaker answer of
	// a key-file fetch that may simply be timing out.
	var added []Site
	if prev != nil {
		known := make(map[string]bool, len(prev.sites))
		for _, site := range prev.sites {
			known[site.Host] = true
		}
		for _, site := range next.sites {
			if !known[site.Host] {
				added = append(added, site)
			}
		}
	}

	s.log.Info("config reloaded", "sites", len(next.sites), "added", len(added))

	if len(added) > 0 {
		// In the background, like the startup check: a slow origin must not hold
		// the signal handler, and /health fills in as each answer arrives.
		go s.verifySites(ctx, next.client, added)
	}

	return nil
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
	// GET only, like /health: reading the counters has no side effects, and the
	// mux answers any other method with 405 and an Allow of its own.
	mux.HandleFunc("GET /metrics", s.handleMetrics)
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
	rt := s.rt.Load()

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

	site := rt.siteForToken(token)
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
	//
	// The cost of that order is that these guards cannot see the URL list, so a
	// batch the resubmission window then empties has still spent a token while
	// sending nothing. A caller retrying one unchanged URL can therefore use up
	// its burst and be answered throttled for requests that would have sent
	// nothing at all. Moving the window ahead of the guards would fix that and
	// break the rule above: a request that reads and parses a body before
	// deciding not to send it was not refused cheaply. The trade is stated here
	// rather than reversed, because frequency is the cheaper thing to police and
	// the caller can see both counts in the response.
	if wait := s.rate.take(site.Host); wait > 0 {
		s.metrics.recordGuardRefusal(site.Host, reasonThrottled)
		s.refuse(w, r, site.Host, submissionResult{
			Reason:     reasonThrottled,
			RetryAfter: wait,
			Detail:     fmt.Sprintf("%s has spent its %d request burst and has to wait for a token", site.Host, rateBurstTokens),
		})
		return
	}

	if decision := s.inflight.acquire(r.Context(), site.Host); decision != guardAcquired {
		// Counted under the reason the caller sees, so this family and the
		// refusal it describes always agree. A caller that hung up while queued
		// is counted too: the guard refused it, and nobody can read the answer
		// either way.
		s.metrics.recordGuardRefusal(site.Host, reasonOverloaded)
		s.refuse(w, r, site.Host, submissionResult{
			Reason:     reasonOverloaded,
			RetryAfter: busyRetryAfter,
			Detail:     s.overloadedDetail(site.Host, decision),
		})
		return
	}
	defer s.inflight.release(site.Host)

	urls, ok := s.requestURLs(w, r, rt, site)
	if !ok {
		return
	}

	// duration_ms is the engine call and nothing else, so latency rising before
	// a throttle shows up in the numbers instead of only in the 429s. It is the
	// only early warning this tool can give: the engine's limit is not
	// published, so the trend is the signal.
	start := time.Now()
	result := rt.client.submitWithWindow(*site, urls, s.window, rt.maxURLs)
	duration := time.Since(start)

	// Counted whatever the outcome: this request was handled, and the reason
	// label (or accepted) is what happened to it. The engine counters and the
	// latency histogram stay absent unless the engine was actually asked, so a
	// local refusal cannot look like an engine answer.
	s.metrics.record(site.Host, result, duration, engineInvoked(result))

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
func (s *server) requestURLs(w http.ResponseWriter, r *http.Request, rt *runtime, site *Site) ([]string, bool) {
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
	r.Body = http.MaxBytesReader(w, r.Body, int64(rt.maxBodyBytes))

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
			s.refuseBody(w, site, http.StatusRequestEntityTooLarge, reasonPayloadTooLarge,
				fmt.Sprintf("body exceeded the %d byte limit", rt.maxBodyBytes))
			return nil, false
		}

		// The body could not be read at all, so there is nothing to parse and
		// nothing that was submitted.
		s.refuseBody(w, site, http.StatusBadRequest, reasonBadRequest, "body could not be read")
		return nil, false
	}

	var request submitRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		s.refuseBody(w, site, http.StatusBadRequest, reasonBadRequest,
			`body must be a single {"urls": ["https://..."]} object with no trailing data`)
		return nil, false
	}

	return request.URLs, true
}

// refuseBody answers a submission whose body could not be read or parsed, and
// counts it: it is a submission this instance handled, with a reason that says
// what was wrong and no engine call behind it.
func (s *server) refuseBody(w http.ResponseWriter, site *Site, status int, reason, detail string) {
	s.metrics.recordRefusal(site.Host, reason)

	writeJSON(w, status, submitResponse{
		Host:   site.Host,
		Status: status,
		Reason: reason,
		Detail: detail,
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

// overloadedDetail says which cap refused the request, because the caller's fix
// differs: a site over its own cap has to slow that site down, while a request
// refused by the instance-wide cap is competing with every other site and can
// only back off and retry. The numbers are read from the guard so the answer
// cannot drift from the cap that actually ran.
func (s *server) overloadedDetail(host string, decision guardDecision) string {
	switch decision {
	case guardRefusedSite:
		return fmt.Sprintf("this site already has %d engine calls in flight and none freed up within %s", s.inflight.perHost, s.inflight.wait)
	case guardRefusedCancelled:
		// Nobody is left to read this answer, but it is written honestly anyway:
		// a client that hung up is not an overloaded instance, and a response
		// logged by a test or a proxy must not say it was.
		return "the client went away while waiting for an engine slot"
	default:
		return fmt.Sprintf("the instance already has %d engine calls in flight and none freed up within %s", cap(s.inflight.global), s.inflight.wait)
	}
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

// handleMetrics answers the counters in the Prometheus text exposition format.
//
// The token decides one thing and nothing else: whether the host label is
// present. Without one every family is aggregated into a single series with no
// host label at all - omitted rather than blanked - because /metrics is usually
// scraped unauthenticated and a Cloudflare Tunnel makes it publicly reachable,
// so an anonymous GET must not be able to enumerate the operator's domains. That
// is the same rule /health follows, and it reuses the same token lookup.
func (s *server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	rt := s.rt.Load()
	perHost := rt.siteForToken(bearerToken(r.Header.Get("Authorization"))) != nil

	w.Header().Set("Content-Type", metricsContentType)
	w.WriteHeader(http.StatusOK)

	// The two snapshots are taken separately and never nested, so a scrape
	// cannot hold the key-state lock while it waits for the counter lock.
	writeMetrics(w, s.metrics.snapshot(), s.keyStatesSnapshot(rt), perHost)
}

// handleHealth answers whether the process is up. A stale key file is counted
// as failed but never changes the status code: the process is up, and
// restarting the container would not fix a key file on someone else's origin.
func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	rt := s.rt.Load()

	if !detailRequested(r) {
		verified, failed, pending := countKeyStates(s.keyStatesSnapshot(rt))

		writeJSON(w, http.StatusOK, healthResponse{
			OK:       true,
			Sites:    len(rt.sites),
			Verified: verified,
			Failed:   failed,
			Pending:  pending,
		})
		return
	}

	// Per-host detail names the operator's domains, so it needs a token. An
	// absent header cannot pass by accident: every configured token is at least
	// minTokenLength characters, and an empty string never matches one.
	if rt.siteForToken(bearerToken(r.Header.Get("Authorization"))) == nil {
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
		s.recheckKeys(r.Context(), rt)
	}

	writeJSON(w, http.StatusOK, healthDetailResponse{
		OK:    true,
		Sites: len(rt.sites),
		Keys:  s.keyStatesSnapshot(rt),
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

// siteForToken compares against every configured token in constant time and
// keeps the last match, so timing does not reveal which site a token belongs
// to.
func (rt *runtime) siteForToken(token string) *Site {
	given := []byte(token)

	var found *Site
	for i := range rt.sites {
		site := &rt.sites[i]
		if subtle.ConstantTimeCompare(given, []byte(site.Token)) == 1 {
			found = site
		}
	}

	return found
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
