package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
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

type server struct {
	cfg    *Config
	client *indexNowClient
	log    *slog.Logger

	mu sync.RWMutex
	// keyStates holds the last recorded state per host. A host that is absent
	// is pending, which is the honest default and the reason the map only ever
	// grows: setKeyState is the only writer.
	keyStates map[string]keyState
}

func newServer(cfg *Config, client *indexNowClient, log *slog.Logger) *server {
	return &server{
		cfg:       cfg,
		client:    client,
		log:       log,
		keyStates: make(map[string]keyState, len(cfg.Sites)),
	}
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
	// Reason names why the batch was not accepted. payload_too_large is the
	// only value the handler adds itself: the body was larger than maxBodyBytes
	// and no submission was ever built.
	Reason string `json:"reason,omitempty"`
	Detail string `json:"detail,omitempty"`
	// RetryAfterSeconds is set only for rate_limited, so a caller can schedule
	// the retry without guessing.
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

	result := s.client.submit(r.Context(), *site, request.URLs)

	// Record a key verdict only when the submission produced one. A 200 proves
	// the key works, 202 says validation is still pending, and 403 says the key
	// was refused; an engine error or a rate limit says nothing about the key,
	// so the recorded state is left alone.
	if state := result.keyStateAfter(); state != "" {
		s.setKeyState(site.Host, state)
	}

	status := httpStatusFor(result)
	if result.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(result.RetryAfter.Seconds())))
	}

	// engine_status is logged only when an engine actually answered. A locally
	// refused batch leaves it at 0, and logging that would read as "the engine
	// answered zero".
	attrs := make([]slog.Attr, 0, 5)
	attrs = append(attrs, slog.String("host", site.Host))
	if result.EngineStatus != 0 {
		attrs = append(attrs, slog.Int("engine_status", result.EngineStatus))
	}
	attrs = append(attrs,
		slog.Int("http_status", status),
		slog.Int("submitted", result.Submitted),
		slog.String("reason", result.Reason),
	)
	s.log.LogAttrs(r.Context(), slog.LevelInfo, "submit", attrs...)

	writeJSON(w, status, submitResponse{
		OK:                result.accepted(),
		Status:            status,
		Host:              site.Host,
		Submitted:         result.Submitted,
		Reason:            result.Reason,
		Detail:            result.Detail,
		RetryAfterSeconds: int(result.RetryAfter.Seconds()),
	})
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
	OK    bool                `json:"ok"`
	Sites int                 `json:"sites"`
	Keys  map[string]keyState `json:"keys"`
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
	states := s.keyStatesSnapshot()

	if !detailRequested(r) {
		verified, failed, pending := countKeyStates(states)

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

	writeJSON(w, http.StatusOK, healthDetailResponse{
		OK:    true,
		Sites: len(s.cfg.Sites),
		Keys:  states,
	})
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

// keyStatesSnapshot returns a state for every configured host, defaulting to
// pending, so the counts always add up to the number of sites.
func (s *server) keyStatesSnapshot() map[string]keyState {
	s.mu.RLock()
	defer s.mu.RUnlock()

	states := make(map[string]keyState, len(s.cfg.Sites))
	for _, site := range s.cfg.Sites {
		state, known := s.keyStates[site.Host]
		if !known {
			state = keyStatePending
		}
		states[site.Host] = state
	}

	return states
}

// countKeyStates turns a snapshot into the three counters /health reports.
func countKeyStates(states map[string]keyState) (verified, failed, pending int) {
	for _, state := range states {
		switch state {
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

func (s *server) setKeyState(host string, state keyState) {
	s.mu.Lock()
	s.keyStates[host] = state
	s.mu.Unlock()
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
	case reasonRateLimited:
		return http.StatusTooManyRequests
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

		state := keyStateAfterCheck(err)
		if state == "" {
			// Cancelled: the process is shutting down, which is nobody's key
			// file being wrong. The recorded state stays as it is, and this is
			// not logged at ERROR - shutdown is not a fault.
			s.log.Debug("key file check cancelled", "host", site.Host)
			continue
		}

		s.setKeyState(site.Host, state)

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
}
