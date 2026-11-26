package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testToken = "test-token-0000000000000000"

// fakeEngine stands in for api.indexnow.org: it records the URL list it was
// given and answers with whatever status the test asks for.
type fakeEngine struct {
	server   *httptest.Server
	status   int
	retry    string
	gotURLs  []string
	gotHost  string
	gotCalls int
}

func newFakeEngine(t *testing.T, status int) *fakeEngine {
	t.Helper()

	engine := &fakeEngine{status: status}
	engine.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		engine.gotCalls++

		if engine.retry != "" {
			w.Header().Set("Retry-After", engine.retry)
		}

		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Host    string   `json:"host"`
			URLList []string `json:"urlList"`
		}
		_ = json.Unmarshal(body, &payload)
		engine.gotHost = payload.Host
		engine.gotURLs = payload.URLList

		w.WriteHeader(engine.status)
	}))
	t.Cleanup(engine.server.Close)

	return engine
}

func newTestServer(t *testing.T, engine *fakeEngine) *server {
	t.Helper()

	cfg := &Config{
		Listen: "127.0.0.1:0",
		Sites: []Site{{
			Host:        "example.com",
			IndexNowKey: "abc123def456",
			KeyLocation: "https://example.com/abc123def456.txt",
			Token:       testToken,
		}},
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return newServer(cfg, newIndexNowClient(engine.server.URL), log)
}

func post(t *testing.T, srv *server, body string, authorization string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/submit", strings.NewReader(body))
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}

	recorder := httptest.NewRecorder()
	srv.routes().ServeHTTP(recorder, req)

	var decoded map[string]any
	_ = json.Unmarshal(recorder.Body.Bytes(), &decoded)

	return recorder, decoded
}

func TestSubmitForwardsURLs(t *testing.T) {
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	recorder, body := post(t, srv, `{"urls":["https://example.com/a","https://example.com/b","https://example.com/a"]}`, "Bearer "+testToken)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
	if body["ok"] != true || body["submitted"] != float64(2) {
		t.Fatalf("unexpected body: %v", body)
	}
	// Duplicates are collapsed before the engine ever sees them.
	if len(engine.gotURLs) != 2 {
		t.Fatalf("engine received %d urls, want 2: %v", len(engine.gotURLs), engine.gotURLs)
	}
	if engine.gotHost != "example.com" {
		t.Fatalf("engine host = %q", engine.gotHost)
	}
}

func TestSubmitRejectsUnknownToken(t *testing.T) {
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	for _, authorization := range []string{"", "Bearer wrong", "Basic " + testToken} {
		recorder, body := post(t, srv, `{"urls":["https://example.com/a"]}`, authorization)

		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%q: status = %d, want 401", authorization, recorder.Code)
		}
		if body["reason"] != "unknown_token" {
			t.Fatalf("%q: reason = %v", authorization, body["reason"])
		}
	}

	if engine.gotCalls != 0 {
		t.Fatalf("engine was called %d times for rejected requests", engine.gotCalls)
	}
}

func TestSubmitMapsEngineStatus(t *testing.T) {
	cases := []struct {
		engineStatus int
		wantHTTP     int
		wantReason   string
	}{
		{http.StatusAccepted, http.StatusOK, ""},
		{http.StatusBadRequest, http.StatusBadRequest, reasonBadRequest},
		{http.StatusForbidden, http.StatusForbidden, reasonKeyRejected},
		{http.StatusUnprocessableEntity, http.StatusUnprocessableEntity, reasonHostMismatch},
		{http.StatusTooManyRequests, http.StatusTooManyRequests, reasonRateLimited},
		{http.StatusInternalServerError, http.StatusBadGateway, reasonEngineError},
	}

	for _, testCase := range cases {
		engine := newFakeEngine(t, testCase.engineStatus)
		srv := newTestServer(t, engine)

		recorder, body := post(t, srv, `{"urls":["https://example.com/a"]}`, "Bearer "+testToken)

		if recorder.Code != testCase.wantHTTP {
			t.Fatalf("engine %d: status = %d, want %d", testCase.engineStatus, recorder.Code, testCase.wantHTTP)
		}
		if reason, _ := body["reason"].(string); reason != testCase.wantReason {
			t.Fatalf("engine %d: reason = %q, want %q", testCase.engineStatus, reason, testCase.wantReason)
		}
	}
}

func TestSubmitSurfacesRetryAfter(t *testing.T) {
	engine := newFakeEngine(t, http.StatusTooManyRequests)
	engine.retry = "120"
	srv := newTestServer(t, engine)

	recorder, body := post(t, srv, `{"urls":["https://example.com/a"]}`, "Bearer "+testToken)

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", recorder.Code)
	}
	if got := recorder.Header().Get("Retry-After"); got != "120" {
		t.Fatalf("Retry-After = %q, want 120", got)
	}
	if body["retry_after_seconds"] != float64(120) {
		t.Fatalf("retry_after_seconds = %v", body["retry_after_seconds"])
	}
}

func TestSubmitRejectsForeignHostWithoutCallingEngine(t *testing.T) {
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	recorder, body := post(t, srv, `{"urls":["https://example.com/a","https://attacker.test/b"]}`, "Bearer "+testToken)

	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", recorder.Code)
	}
	if body["reason"] != reasonHostMismatch {
		t.Fatalf("reason = %v", body["reason"])
	}
	if engine.gotCalls != 0 {
		t.Fatalf("engine was called %d times for a rejected batch", engine.gotCalls)
	}
}

func TestSubmitRejectsMalformedBody(t *testing.T) {
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	recorder, body := post(t, srv, `{"urls":"not-a-list"}`, "Bearer "+testToken)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", recorder.Code)
	}
	if body["reason"] != reasonBadRequest {
		t.Fatalf("reason = %v", body["reason"])
	}
}

func TestHealthReportsSites(t *testing.T) {
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	recorder := httptest.NewRecorder()
	srv.routes().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/health", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}

	var body healthResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.OK || body.Sites != 1 {
		t.Fatalf("unexpected health: %+v", body)
	}
}

func TestVerifyAllMarksBrokenKeyFile(t *testing.T) {
	// The key file endpoint answers with the wrong content, which is the case
	// that used to fail silently as a 403 on every submission.
	keyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("a-different-key"))
	}))
	defer keyServer.Close()

	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)
	srv.cfg.Sites[0].KeyLocation = keyServer.URL + "/key.txt"

	srv.verifyAll(context.Background())

	srv.mu.RLock()
	defer srv.mu.RUnlock()
	if srv.verified["example.com"] != false {
		t.Fatal("expected the host to be marked unverified")
	}
}

func TestVerifyAllMarksGoodKeyFile(t *testing.T) {
	keyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("abc123def456\n"))
	}))
	defer keyServer.Close()

	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)
	srv.cfg.Sites[0].KeyLocation = keyServer.URL + "/key.txt"

	srv.verifyAll(context.Background())

	srv.mu.RLock()
	defer srv.mu.RUnlock()
	if srv.verified["example.com"] != true {
		t.Fatal("expected the host to be verified")
	}
}
