package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
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

type server struct {
	cfg    *Config
	client *indexNowClient
	log    *slog.Logger

	mu sync.RWMutex
	// verified remembers the last key-file check per host, so /health can say
	// whether submissions are currently expected to work.
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
	Reason    string `json:"reason,omitempty"`
	Detail    string `json:"detail,omitempty"`
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
		writeJSON(w, http.StatusBadRequest, submitResponse{
			Host:   site.Host,
			Status: http.StatusBadRequest,
			Reason: reasonBadRequest,
			Detail: "body must be {\"urls\": [\"https://...\"]}",
		})
		return
	}

	result := s.client.submit(r.Context(), *site, request.URLs)

	if result.Reason == reasonKeyRejected {
		s.setVerified(site.Host, false)
	}

	status := httpStatusFor(result)
	if result.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(result.RetryAfter.Seconds())))
	}

	s.log.Info("submit",
		"host", site.Host,
		"engine_status", result.EngineStatus,
		"http_status", status,
		"submitted", result.Submitted,
		"reason", result.Reason,
	)

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
	OK    bool            `json:"ok"`
	Sites int             `json:"sites"`
	Keys  map[string]bool `json:"keys"`
}

// handleHealth answers whether the process is up. A stale key file is reported
// in `keys` but does not fail the health check: restarting the container would
// not fix a key file on someone else's origin.
func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	keys := make(map[string]bool, len(s.verified))
	for host, ok := range s.verified {
		keys[host] = ok
	}
	s.mu.RUnlock()

	writeJSON(w, http.StatusOK, healthResponse{
		OK:    true,
		Sites: len(s.cfg.Sites),
		Keys:  keys,
	})
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
// runs at startup so a broken key file is visible immediately instead of
// showing up as a 403 on the first real submission.
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
