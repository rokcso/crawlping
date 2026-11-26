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
	"time"
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

// callHealth performs one /health request, so a test controls the query and the
// credentials the way a real caller would.
func callHealth(t *testing.T, srv *server, query string, authorization string) *httptest.ResponseRecorder {
	t.Helper()

	target := "/health"
	if query != "" {
		target += "?" + query
	}

	req := httptest.NewRequest(http.MethodGet, target, nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}

	recorder := httptest.NewRecorder()
	srv.routes().ServeHTTP(recorder, req)

	return recorder
}

func getHealth(t *testing.T, srv *server) healthResponse {
	t.Helper()

	recorder := callHealth(t, srv, "", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}

	var body healthResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	return body
}

func getHealthDetail(t *testing.T, srv *server) healthDetailResponse {
	t.Helper()

	recorder := callHealth(t, srv, "detail=1", "Bearer "+testToken)
	if recorder.Code != http.StatusOK {
		t.Fatalf("detail status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}

	var body healthDetailResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	return body
}

// keyStateFor reads the recorded state, defaulting to pending the same way the
// server does, so a test never has to know whether an absent entry and an
// explicit pending are stored differently.
func keyStateFor(srv *server, host string) keyState {
	srv.mu.RLock()
	defer srv.mu.RUnlock()

	if state, known := srv.keyStates[host]; known {
		return state
	}

	return keyStatePending
}

func TestHealthReportsSites(t *testing.T) {
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	body := getHealth(t, srv)

	if !body.OK || body.Sites != 1 {
		t.Fatalf("unexpected health: %+v", body)
	}
}

func TestHealthCountsPendingUntilChecked(t *testing.T) {
	// The listener answers before the key checks run, so /health has to say
	// "not known yet" rather than counting every host as broken: a failure that
	// appears only because the check has not finished is a lie.
	keyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("abc123def456\n"))
	}))
	defer keyServer.Close()

	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)
	srv.cfg.Sites[0].KeyLocation = keyServer.URL + "/key.txt"

	pending := getHealth(t, srv)
	if !pending.OK || pending.Sites != 1 || pending.Pending != 1 || pending.Verified != 0 || pending.Failed != 0 {
		t.Fatalf("health before the check = %+v, want one pending site", pending)
	}
	if state := getHealthDetail(t, srv).Keys["example.com"]; state != keyStatePending {
		t.Fatalf("detail state = %q, want %q", state, keyStatePending)
	}

	srv.verifyAll(context.Background())

	checked := getHealth(t, srv)
	if checked.Verified != 1 || checked.Pending != 0 || checked.Failed != 0 {
		t.Fatalf("health after the check = %+v, want one verified site", checked)
	}
}

func TestHealthCountsFailedKeyCheck(t *testing.T) {
	// A 404 is a result: the key file was fetched and refused, so it is failed,
	// not pending.
	keyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer keyServer.Close()

	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)
	srv.cfg.Sites[0].KeyLocation = keyServer.URL + "/key.txt"

	srv.verifyAll(context.Background())

	body := getHealth(t, srv)
	if body.Failed != 1 || body.Verified != 0 || body.Pending != 0 {
		t.Fatalf("health = %+v, want one failed site", body)
	}
	if !body.OK {
		t.Fatal("a stale key file must not fail the health check")
	}
	if state := getHealthDetail(t, srv).Keys["example.com"]; state != keyStateFailed {
		t.Fatalf("detail state = %q, want %q", state, keyStateFailed)
	}
}

func TestHealthWithoutTokenOmitsHostNames(t *testing.T) {
	// /health needs no token, and a Cloudflare Tunnel makes it publicly
	// reachable, so the operator's domain list must not be in it.
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)
	srv.setKeyState("example.com", keyStateVerified)

	recorder := callHealth(t, srv, "", "")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}

	raw := recorder.Body.String()
	if strings.Contains(raw, "example.com") {
		t.Fatalf("unauthenticated /health names a host: %s", raw)
	}

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, field := range []string{"ok", "sites", "verified", "failed", "pending"} {
		if _, present := decoded[field]; !present {
			t.Fatalf("response is missing %q: %s", field, raw)
		}
	}
	if _, present := decoded["keys"]; present {
		t.Fatalf("unauthenticated /health carries per-host keys: %s", raw)
	}
}

func TestHealthDetailRequiresToken(t *testing.T) {
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	cases := []struct {
		name          string
		query         string
		authorization string
	}{
		{name: "no token", query: "detail=1"},
		{name: "wrong token", query: "detail=1", authorization: "Bearer wrong"},
		{name: "wrong scheme", query: "detail=1", authorization: "Basic " + testToken},
		{name: "bare parameter", query: "detail"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := callHealth(t, srv, testCase.query, testCase.authorization)

			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (%s)", recorder.Code, recorder.Body.String())
			}

			var body healthErrorResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body.OK || body.Status != http.StatusUnauthorized || body.Reason != reasonUnknownToken {
				t.Fatalf("unexpected refusal: %+v", body)
			}
			if strings.Contains(recorder.Body.String(), "example.com") {
				t.Fatalf("a refusal names a host: %s", recorder.Body.String())
			}
		})
	}
}

func TestHealthDetailListsPerHostStates(t *testing.T) {
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	if state := getHealthDetail(t, srv).Keys["example.com"]; state != keyStatePending {
		t.Fatalf("state before any check = %q, want %q", state, keyStatePending)
	}

	srv.setKeyState("example.com", keyStateFailed)

	detail := getHealthDetail(t, srv)
	if detail.Sites != 1 || detail.Keys["example.com"] != keyStateFailed {
		t.Fatalf("detail = %+v, want example.com:failed", detail)
	}

	// The unauthenticated view stays host-free and only counts.
	if counts := getHealth(t, srv); counts.Failed != 1 || counts.Pending != 0 {
		t.Fatalf("counts = %+v, want one failed site", counts)
	}
}

func TestSubmit202LeavesKeyPending(t *testing.T) {
	// 202 means the batch was received and key validation is still pending. It
	// is not proof the key works, so it must not be recorded as verified.
	engine := newFakeEngine(t, http.StatusAccepted)
	srv := newTestServer(t, engine)

	recorder, _ := post(t, srv, `{"urls":["https://example.com/a"]}`, "Bearer "+testToken)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
	if state := keyStateFor(srv, "example.com"); state != keyStatePending {
		t.Fatalf("state after 202 = %q, want %q", state, keyStatePending)
	}
	if counts := getHealth(t, srv); counts.Verified != 0 || counts.Pending != 1 || counts.Failed != 0 {
		t.Fatalf("health = %+v, want one pending site and nothing verified", counts)
	}
}

func TestSubmit200MarksKeyVerified(t *testing.T) {
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	post(t, srv, `{"urls":["https://example.com/a"]}`, "Bearer "+testToken)

	if state := keyStateFor(srv, "example.com"); state != keyStateVerified {
		t.Fatalf("state after 200 = %q, want %q", state, keyStateVerified)
	}
}

func TestSubmitEngineErrorLeavesKeyStateAlone(t *testing.T) {
	// A 502 says the engine broke, not that the key file is wrong.
	engine := newFakeEngine(t, http.StatusInternalServerError)
	srv := newTestServer(t, engine)
	srv.setKeyState("example.com", keyStateVerified)

	post(t, srv, `{"urls":["https://example.com/a"]}`, "Bearer "+testToken)

	if state := keyStateFor(srv, "example.com"); state != keyStateVerified {
		t.Fatalf("state after an engine error = %q, want %q left alone", state, keyStateVerified)
	}
}

func TestVerifyAllCanceledLeavesStateUntouched(t *testing.T) {
	// SIGTERM cancels the check context. A check that never ran is not a broken
	// key file, and shutdown is not a fault, so the state stays and nothing is
	// logged at ERROR.
	var lines bytes.Buffer

	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServerWithLog(t, engine, slog.New(slog.NewTextHandler(&lines, &slog.HandlerOptions{Level: slog.LevelDebug})))
	srv.cfg.Sites[0].KeyLocation = "http://127.0.0.1:1/key.txt"
	srv.setKeyState("example.com", keyStateVerified)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	srv.verifyAll(ctx)

	if state := keyStateFor(srv, "example.com"); state != keyStateVerified {
		t.Fatalf("state after a cancelled check = %q, want %q", state, keyStateVerified)
	}
	if strings.Contains(lines.String(), "level=ERROR") {
		t.Fatalf("a cancelled check logged at ERROR: %s", lines.String())
	}
}

func TestVerifyAllDeadlineLeavesPending(t *testing.T) {
	// A check that ran out of time learned nothing: it is pending, and it is
	// logged at WARN because no key has been shown to be broken.
	var lines bytes.Buffer

	keyServer := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer keyServer.Close()

	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServerWithLog(t, engine, slog.New(slog.NewTextHandler(&lines, nil)))
	srv.cfg.Sites[0].KeyLocation = keyServer.URL + "/key.txt"
	srv.setKeyState("example.com", keyStateVerified)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	srv.verifyAll(ctx)

	if state := keyStateFor(srv, "example.com"); state != keyStatePending {
		t.Fatalf("state after a timed-out check = %q, want %q", state, keyStatePending)
	}
	if strings.Contains(lines.String(), "level=ERROR") {
		t.Fatalf("an inconclusive check logged at ERROR: %s", lines.String())
	}
	if !strings.Contains(lines.String(), "level=WARN") {
		t.Fatalf("an inconclusive check was not logged at WARN: %s", lines.String())
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

	if state := keyStateFor(srv, "example.com"); state != keyStateFailed {
		t.Fatalf("state = %q, want %q", state, keyStateFailed)
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

	if state := keyStateFor(srv, "example.com"); state != keyStateVerified {
		t.Fatalf("state = %q, want %q", state, keyStateVerified)
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
	if state := keyStateFor(srv, "example.com"); state != keyStateFailed {
		t.Fatalf("after key_rejected the state = %q, want %q", state, keyStateFailed)
	}

	// The key file was fixed at the origin. Nothing restarts here, so the host
	// has to become verified again on its own or /health lies forever.
	engine.setStatus(http.StatusOK)

	recorder, _ = post(t, srv, `{"urls":["https://example.com/b"]}`, "Bearer "+testToken)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
	if state := keyStateFor(srv, "example.com"); state != keyStateVerified {
		t.Fatalf("an accepted batch must mark the host verified again, got %q", state)
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

func TestSubmitRejectsTrailingData(t *testing.T) {
	// The decoder stopped at the end of the first JSON value, so a second value
	// after it used to be accepted and the trailing bytes silently ignored. The
	// size cap cannot catch this: the body is small.
	cases := map[string]string{
		"second json value": `{"urls":["https://example.com/a"]}{"urls":["https://example.com/b"]}`,
		"trailing garbage":  `{"urls":["https://example.com/a"]} not json`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			engine := newFakeEngine(t, http.StatusOK)
			srv := newTestServer(t, engine)

			recorder, decoded := post(t, srv, body, "Bearer "+testToken)

			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", recorder.Code, recorder.Body.String())
			}
			if decoded["reason"] != reasonBadRequest {
				t.Fatalf("reason = %v, want %q", decoded["reason"], reasonBadRequest)
			}
			if engine.calls() != 0 {
				t.Fatalf("engine was called %d times for a body with trailing data", engine.calls())
			}
		})
	}
}

func TestSubmitRejectsTrailingFloodOverTheCap(t *testing.T) {
	// The original hole: a body that fits the cap followed by far more bytes
	// than the cap was read as "the first JSON value" and answered 200. The cap
	// is applied while the bytes are read now, so the flood is refused.
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	body := `{"urls":["https://example.com/a"]}` + strings.Repeat("x", maxBodyBytes)

	recorder, decoded := post(t, srv, body, "Bearer "+testToken)

	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (%s)", recorder.Code, recorder.Body.String())
	}
	if decoded["reason"] != reasonPayloadTooLarge {
		t.Fatalf("reason = %v, want %q", decoded["reason"], reasonPayloadTooLarge)
	}
	if engine.calls() != 0 {
		t.Fatalf("engine was called %d times for an oversized body", engine.calls())
	}
}
