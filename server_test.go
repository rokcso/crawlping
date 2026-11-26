package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const testToken = "test-token-0000000000000000"

// fakeEngine stands in for api.indexnow.org: it records the URL list it was
// given and answers with whatever status the test asks for. The status is
// mutable through setStatus because a test has to be able to change the
// engine's mind between two requests.
type fakeEngine struct {
	server *httptest.Server

	mu       sync.Mutex
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
		engine.mu.Lock()
		engine.gotCalls++
		status := engine.status
		retry := engine.retry
		engine.mu.Unlock()

		if retry != "" {
			w.Header().Set("Retry-After", retry)
		}

		body, _ := io.ReadAll(r.Body)
		var payload struct {
			Host    string   `json:"host"`
			URLList []string `json:"urlList"`
		}
		_ = json.Unmarshal(body, &payload)

		engine.mu.Lock()
		engine.gotHost = payload.Host
		engine.gotURLs = payload.URLList
		engine.mu.Unlock()

		w.WriteHeader(status)
	}))
	t.Cleanup(engine.server.Close)

	return engine
}

func (e *fakeEngine) setStatus(status int) {
	e.mu.Lock()
	e.status = status
	e.mu.Unlock()
}

func (e *fakeEngine) setRetryAfter(retry string) {
	e.mu.Lock()
	e.retry = retry
	e.mu.Unlock()
}

func (e *fakeEngine) calls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.gotCalls
}

func (e *fakeEngine) urls() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.gotURLs...)
}

func (e *fakeEngine) host() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.gotHost
}

func newTestServer(t *testing.T, engine *fakeEngine) *server {
	t.Helper()

	return newTestServerWithLog(t, engine, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func newTestServerWithLog(t *testing.T, engine *fakeEngine, log *slog.Logger) *server {
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
	if got := engine.urls(); len(got) != 2 {
		t.Fatalf("engine received %d urls, want 2: %v", len(got), got)
	}
	if engine.host() != "example.com" {
		t.Fatalf("engine host = %q", engine.host())
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

	if engine.calls() != 0 {
		t.Fatalf("engine was called %d times for rejected requests", engine.calls())
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
	engine.setRetryAfter("120")
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
	if engine.calls() != 0 {
		t.Fatalf("engine was called %d times for a rejected batch", engine.calls())
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

func getHealth(t *testing.T, srv *server) healthResponse {
	t.Helper()

	recorder := httptest.NewRecorder()
	srv.routes().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/health", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}

	var body healthResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	return body
}

func TestHealthReportsSites(t *testing.T) {
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	body := getHealth(t, srv)

	if !body.OK || body.Sites != 1 {
		t.Fatalf("unexpected health: %+v", body)
	}
}

func TestHealthOmitsKeysUntilChecked(t *testing.T) {
	// The listener now answers before the key checks run, so /health has to say
	// "not known yet" rather than reporting every host as broken: a false that
	// appears only because the check has not finished is a lie.
	keyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("abc123def456\n"))
	}))
	defer keyServer.Close()

	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)
	srv.cfg.Sites[0].KeyLocation = keyServer.URL + "/key.txt"

	pending := getHealth(t, srv)
	if _, known := pending.Keys["example.com"]; known {
		t.Fatalf("keys reports example.com before any check: %v", pending.Keys)
	}
	if len(pending.Pending) != 1 || pending.Pending[0] != "example.com" {
		t.Fatalf("pending = %v, want [example.com]", pending.Pending)
	}

	srv.verifyAll(context.Background())

	checked := getHealth(t, srv)
	if verified, known := checked.Keys["example.com"]; !known || !verified {
		t.Fatalf("keys = %v, want example.com:true", checked.Keys)
	}
	if len(checked.Pending) != 0 {
		t.Fatalf("pending = %v, want empty after the check finished", checked.Pending)
	}
}

func TestHealthReportsFailedKeyCheck(t *testing.T) {
	// A completed check that failed is a result, not a pending state: the host
	// moves from `pending` into `keys` with false.
	keyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer keyServer.Close()

	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)
	srv.cfg.Sites[0].KeyLocation = keyServer.URL + "/key.txt"

	srv.verifyAll(context.Background())

	body := getHealth(t, srv)
	if verified, known := body.Keys["example.com"]; !known || verified {
		t.Fatalf("keys = %v, want example.com:false", body.Keys)
	}
	if len(body.Pending) != 0 {
		t.Fatalf("pending = %v, want empty", body.Pending)
	}
	if !body.OK {
		t.Fatal("a stale key file must not fail the health check")
	}
}

func verifiedFor(srv *server, host string) bool {
	srv.mu.RLock()
	defer srv.mu.RUnlock()
	return srv.verified[host]
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

	if verifiedFor(srv, "example.com") {
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

	if !verifiedFor(srv, "example.com") {
		t.Fatal("expected the host to be verified")
	}
}

func TestSubmitRejectsOversizedBody(t *testing.T) {
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	// Valid JSON that simply does not fit. The caller must be told the body is
	// too large, not that its syntax is wrong.
	body := `{"urls":["https://example.com/a"],"padding":"` + strings.Repeat("x", maxBodyBytes+1) + `"}`

	recorder, decoded := post(t, srv, body, "Bearer "+testToken)

	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (%s)", recorder.Code, recorder.Body.String())
	}
	if decoded["reason"] != reasonPayloadTooLarge {
		t.Fatalf("reason = %v, want %q", decoded["reason"], reasonPayloadTooLarge)
	}

	detail, _ := decoded["detail"].(string)
	if !strings.Contains(detail, strconv.Itoa(maxBodyBytes)) {
		t.Fatalf("detail %q does not name the %d byte limit", detail, maxBodyBytes)
	}
	if engine.calls() != 0 {
		t.Fatalf("engine was called %d times for an oversized body", engine.calls())
	}
}

func TestSubmitRecoversKeyVerificationAfterKeyRejected(t *testing.T) {
	engine := newFakeEngine(t, http.StatusForbidden)
	srv := newTestServer(t, engine)

	recorder, decoded := post(t, srv, `{"urls":["https://example.com/a"]}`, "Bearer "+testToken)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (%s)", recorder.Code, recorder.Body.String())
	}
	if decoded["reason"] != reasonKeyRejected {
		t.Fatalf("reason = %v, want %q", decoded["reason"], reasonKeyRejected)
	}
	if verifiedFor(srv, "example.com") {
		t.Fatal("after key_rejected the host must be marked unverified")
	}

	// The key file was fixed at the origin. Nothing restarts here, so the host
	// has to become verified again on its own or /health lies forever.
	engine.setStatus(http.StatusOK)

	recorder, _ = post(t, srv, `{"urls":["https://example.com/b"]}`, "Bearer "+testToken)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
	if !verifiedFor(srv, "example.com") {
		t.Fatal("an accepted batch must mark the host verified again")
	}
}

func TestSubmitLogsEngineStatusOnlyWhenTheEngineAnswered(t *testing.T) {
	var lines bytes.Buffer

	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServerWithLog(t, engine, slog.New(slog.NewTextHandler(&lines, nil)))

	// Refused locally: the engine was never called, so there is no engine
	// status to log.
	post(t, srv, `{"urls":["https://example.com/a","https://attacker.test/b"]}`, "Bearer "+testToken)

	if strings.Contains(lines.String(), "engine_status") {
		t.Fatalf("log claims an engine status for a batch the engine never saw: %s", lines.String())
	}

	lines.Reset()

	post(t, srv, `{"urls":["https://example.com/a"]}`, "Bearer "+testToken)

	if !strings.Contains(lines.String(), "engine_status=200") {
		t.Fatalf("log is missing the engine status the engine did produce: %s", lines.String())
	}
}
