package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// maxBodyBytes bounds a submission body. 100 URLs fit far below this; anything
// larger is a mistake or an attempt to make the process allocate.
const maxBodyBytes = 64 << 10

// reasonPayloadTooLarge is the only reason the handler produces on its own. It
// lives here rather than with the engine reasons because an oversized body is
// refused before a submission is built: no engine ever saw the payload, so
// calling it a malformed one would send the caller looking at their syntax.
const reasonPayloadTooLarge = "payload_too_large"

type server struct {
	cfg    *Config
	client *indexNowClient
	log    *slog.Logger

	mu sync.RWMutex
	// verified remembers the last key-file check per host, so /health can say
	// whether submissions are currently expected to work. A host missing from
	// the map has not been checked yet, which is not the same as checked and
	// failed; setVerified is the only writer, and a successful submission sets
	// it back to true so a fixed key file recovers without a restart.
	verified map[string]bool
}

func newServer(cfg *Config, client *indexNowClient, log *slog.Logger) *server {
	return &server{
		cfg:      cfg,
		client:   client,
		log:      log,
		verified: make(map[string]bool, len(cfg.Sites)),
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
			Reason: "unknown_token",
		})
		return
	}

	site := s.siteForToken(token)
	if site == nil {
		s.log.Warn("submit rejected", "reason", "unknown_token", "remote", r.RemoteAddr)
		writeJSON(w, http.StatusUnauthorized, submitResponse{
			Status: http.StatusUnauthorized,
			Reason: "unknown_token",
		})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	var request submitRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
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

		writeJSON(w, http.StatusBadRequest, submitResponse{
			Host:   site.Host,
			Status: http.StatusBadRequest,
			Reason: reasonBadRequest,
			Detail: "body must be {\"urls\": [\"https://...\"]}",
		})
		return
	}

	result := s.client.submit(r.Context(), *site, request.URLs)

	switch {
	case result.accepted():
		// The engine took a batch for this host, so an earlier key_rejected is
		// no longer the current state: a key file fixed at the origin recovers
		// on the next submission instead of waiting for a restart.
		s.setVerified(site.Host, true)
	case result.Reason == reasonKeyRejected:
		s.setVerified(site.Host, false)
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

type healthResponse struct {
	OK    bool `json:"ok"`
	Sites int  `json:"sites"`
	// Keys holds only the hosts whose key file has answered at least once.
	// true and false are both results; a host that is absent has no result yet.
	Keys map[string]bool `json:"keys"`
	// Pending names the hosts still being checked. It is how "not checked yet"
	// is told apart from "checked and failed" while the startup check runs,
	// which is now that the listener comes up first.
	Pending []string `json:"pending,omitempty"`
}

// handleHealth answers whether the process is up. A stale key file is reported
// in `keys` but does not fail the health check: restarting the container would
// not fix a key file on someone else's origin.
func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	keys, pending := s.keyStatus()

	writeJSON(w, http.StatusOK, healthResponse{
		OK:      true,
		Sites:   len(s.cfg.Sites),
		Keys:    keys,
		Pending: pending,
	})
}

// keyStatus splits the configured hosts into the ones a key check has answered
// for and the ones it has not reached yet.
func (s *server) keyStatus() (map[string]bool, []string) {
	s.mu.RLock()
	keys := make(map[string]bool, len(s.verified))
	for host, ok := range s.verified {
		keys[host] = ok
	}
	s.mu.RUnlock()

	var pending []string
	for _, site := range s.cfg.Sites {
		if _, known := keys[site.Host]; !known {
			pending = append(pending, site.Host)
		}
	}

	sort.Strings(pending)

	return keys, pending
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

func (s *server) setVerified(host string, ok bool) {
	s.mu.Lock()
	s.verified[host] = ok
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
// origins cannot delay the first byte: each check has its own 10s timeout and
// they would otherwise add up in front of a 5s container start period. The
// result is advisory - it only fills in the `keys` field of /health, and a
// failed check never stops the process.
func (s *server) verifyAll(ctx context.Context) {
	for _, site := range s.cfg.Sites {
		checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := s.client.verifyKey(checkCtx, site)
		cancel()

		if err != nil {
			s.setVerified(site.Host, false)
			s.log.Error("key file check failed",
				"host", site.Host,
				"key_location", site.KeyLocation,
				"error", err,
			)
			continue
		}

		s.setVerified(site.Host, true)
		s.log.Info("key file ok", "host", site.Host)
	}
}
