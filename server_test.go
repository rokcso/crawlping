package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func newTestServerForSite(t *testing.T, engine *fakeEngine, site Site) *server {
	t.Helper()

	cfg := &Config{
		Listen: "127.0.0.1:0",
		Sites:  []Site{site},
	}

	return newServer(cfg, newIndexNowClient(engine.server.URL), slog.New(slog.NewTextHandler(io.Discard, nil)))
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

// postURL posts one batch carrying exactly these URLs, so a test that cares
// about which URLs it sent does not hand-write JSON.
func postURL(t *testing.T, srv *server, authorization string, urls ...string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()

	body, err := json.Marshal(submitRequest{URLs: urls})
	if err != nil {
		t.Fatalf("marshal batch: %v", err)
	}

	return post(t, srv, string(body), authorization)
}

// submitQuery builds the GET form of a submission: one url parameter per URL,
// escaped the way a shell would have to escape it.
func submitQuery(urls ...string) string {
	values := make(url.Values, 1)
	for _, u := range urls {
		values.Add("url", u)
	}

	return values.Encode()
}

// getSubmit performs one GET /submit, which carries the URL list in the query
// string instead of a body.
func getSubmit(t *testing.T, srv *server, query string, authorization string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/submit?"+query, nil)
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

func TestSubmitRejectsURLOutsideKeyScopeWithoutCallingEngine(t *testing.T) {
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServerForSite(t, engine, Site{
		Host:        "example.com",
		IndexNowKey: "abc123def456",
		KeyLocation: "https://example.com/catalog/abc123def456.txt",
		PathScope:   "/catalog/",
		Token:       testToken,
	})

	recorder, body := post(t, srv, `{"urls":["https://example.com/help/page"]}`, "Bearer "+testToken)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", recorder.Code, recorder.Body.String())
	}
	// Pinned as a string because it is what a caller branches on.
	if body["reason"] != "out_of_key_scope" {
		t.Fatalf("reason = %v, want out_of_key_scope", body["reason"])
	}
	detail, _ := body["detail"].(string)
	if !strings.Contains(detail, "https://example.com/help/page") {
		t.Fatalf("detail does not name the offending url: %q", detail)
	}
	if !strings.Contains(detail, "/catalog/") {
		t.Fatalf("detail does not name the allowed prefix: %q", detail)
	}
	if engine.calls() != 0 {
		t.Fatalf("engine was called %d times for an out-of-scope batch", engine.calls())
	}
}

func TestSubmitAcceptsURLsInsideKeyScope(t *testing.T) {
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServerForSite(t, engine, Site{
		Host:        "example.com",
		IndexNowKey: "abc123def456",
		KeyLocation: "https://example.com/catalog/abc123def456.txt",
		PathScope:   "/catalog/",
		Token:       testToken,
	})

	recorder, body := post(t,
		srv,
		`{"urls":["https://example.com/catalog","https://example.com/catalog/show?item=23"]}`,
		"Bearer "+testToken,
	)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
	if body["submitted"] != float64(2) {
		t.Fatalf("submitted = %v, want 2", body["submitted"])
	}
	if engine.calls() != 1 {
		t.Fatalf("engine was called %d times, want 1", engine.calls())
	}
}

func TestSubmitWithRootKeyFileAcceptsEveryPath(t *testing.T) {
	// The derived scope of the default key_location, https://<host>/<key>.txt,
	// is the host root. Every existing deployment runs that configuration, so a
	// root scope must not restrict anything.
	cfg, err := loadConfig(writeConfig(t, singleSiteConfig("example.com", configTestKey)))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	engine := newFakeEngine(t, http.StatusOK)
	srv := newServer(cfg, newIndexNowClient(engine.server.URL), slog.New(slog.NewTextHandler(io.Discard, nil)))

	recorder, body := post(t,
		srv,
		`{"urls":["https://example.com/","https://example.com/anything","https://example.com/a/b"]}`,
		"Bearer "+configTestToken,
	)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
	if body["submitted"] != float64(3) {
		t.Fatalf("submitted = %v, want 3", body["submitted"])
	}
	if engine.calls() != 1 {
		t.Fatalf("engine was called %d times, want 1", engine.calls())
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

// --- GET, IndexNow's own single-URL form ------------------------------------

func TestSubmitGETForwardsTheURL(t *testing.T) {
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	recorder, body := getSubmit(t, srv, submitQuery("https://example.com/blog/new-post"), "Bearer "+testToken)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
	if body["ok"] != true || body["submitted"] != float64(1) {
		t.Fatalf("unexpected body: %v", body)
	}
	if got := engine.urls(); len(got) != 1 || got[0] != "https://example.com/blog/new-post" {
		t.Fatalf("engine received %v, want the one url", got)
	}
	if engine.host() != "example.com" {
		t.Fatalf("engine host = %q", engine.host())
	}
}

func TestSubmitGETRejectsUnknownToken(t *testing.T) {
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	for _, authorization := range []string{"", "Bearer wrong", "Basic " + testToken} {
		recorder, body := getSubmit(t, srv, submitQuery("https://example.com/a"), authorization)

		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%q: status = %d, want 401", authorization, recorder.Code)
		}
		if body["reason"] != reasonUnknownToken {
			t.Fatalf("%q: reason = %v", authorization, body["reason"])
		}
	}

	if got := engine.calls(); got != 0 {
		t.Fatalf("engine was called %d times for rejected requests", got)
	}
}

func TestSubmitGETRejectsForeignHostWithoutCallingEngine(t *testing.T) {
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	recorder, body := getSubmit(t, srv, submitQuery("https://attacker.test/b"), "Bearer "+testToken)

	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (%s)", recorder.Code, recorder.Body.String())
	}
	if body["reason"] != reasonHostMismatch {
		t.Fatalf("reason = %v, want %q", body["reason"], reasonHostMismatch)
	}
	if got := engine.calls(); got != 0 {
		t.Fatalf("engine was called %d times for a rejected url", got)
	}
}

func TestSubmitGETWithoutAnURLIsBadRequest(t *testing.T) {
	// Nothing to submit is the same answer on both methods: the empty list is
	// refused by the submission path, not by the method that carried it.
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	recorder, body := getSubmit(t, srv, "", "Bearer "+testToken)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", recorder.Code, recorder.Body.String())
	}
	if body["reason"] != reasonBadRequest {
		t.Fatalf("reason = %v, want %q", body["reason"], reasonBadRequest)
	}
	if got := engine.calls(); got != 0 {
		t.Fatalf("engine was called %d times for an empty batch", got)
	}
}

func TestSubmitGETSubmitsEveryRepeatedURL(t *testing.T) {
	// One url parameter is the common case, but the repeated form is what makes
	// a small batch a single call, so it has to carry all of them - and go
	// through the same duplicate collapsing as the JSON form.
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	recorder, body := getSubmit(t, srv,
		submitQuery("https://example.com/a", "https://example.com/b", "https://example.com/a"),
		"Bearer "+testToken,
	)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
	if body["submitted"] != float64(2) {
		t.Fatalf("submitted = %v, want 2", body["submitted"])
	}
	if got := engine.urls(); len(got) != 2 {
		t.Fatalf("engine received %d urls, want 2: %v", len(got), got)
	}
}

func TestSubmitUnsupportedMethodAnswers405(t *testing.T) {
	// /submit is registered on the path rather than per method, so a HEAD would
	// reach the handler and the Allow header is the handler's own: it must name
	// both methods that work and nothing else.
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	for _, method := range []string{http.MethodPut, http.MethodDelete, http.MethodHead} {
		req := httptest.NewRequest(method, "/submit", strings.NewReader(`{"urls":["https://example.com/a"]}`))
		recorder := httptest.NewRecorder()
		srv.routes().ServeHTTP(recorder, req)

		if recorder.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: status = %d, want 405 (%s)", method, recorder.Code, recorder.Body.String())
		}
		if got := recorder.Header().Get("Allow"); got != "GET, POST" {
			t.Fatalf("%s: Allow = %q, want %q", method, got, "GET, POST")
		}

		var body submitResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: decode: %v", method, err)
		}
		if body.OK || body.Status != http.StatusMethodNotAllowed || body.Reason != reasonMethodNotAllowed {
			t.Fatalf("%s: unexpected refusal: %+v", method, body)
		}
	}

	if got := engine.calls(); got != 0 {
		t.Fatalf("engine was called %d times for an unsupported method", got)
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

	return getHealthDetailWith(t, srv, "detail=1")
}

func getHealthDetailWith(t *testing.T, srv *server, query string) healthDetailResponse {
	t.Helper()

	recorder := callHealth(t, srv, query, "Bearer "+testToken)
	if recorder.Code != http.StatusOK {
		t.Fatalf("%s: status = %d, want 200 (%s)", query, recorder.Code, recorder.Body.String())
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

	if record, known := srv.keyStates[host]; known {
		return record.State
	}

	return keyStatePending
}

// keyRecordFor reads the whole recorded entry, so a test can pin the timestamp
// and the detail as well as the state.
func keyRecordFor(srv *server, host string) keyRecord {
	srv.mu.RLock()
	defer srv.mu.RUnlock()

	return srv.keyStates[host]
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
	if state := getHealthDetail(t, srv).Keys["example.com"].State; state != keyStatePending {
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
	if state := getHealthDetail(t, srv).Keys["example.com"].State; state != keyStateFailed {
		t.Fatalf("detail state = %q, want %q", state, keyStateFailed)
	}
}

func TestHealthWithoutTokenOmitsHostNames(t *testing.T) {
	// /health needs no token, and a Cloudflare Tunnel makes it publicly
	// reachable, so the operator's domain list must not be in it.
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)
	srv.setKeyState("example.com", keyStateVerified, "")

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

	if state := getHealthDetail(t, srv).Keys["example.com"].State; state != keyStatePending {
		t.Fatalf("state before any check = %q, want %q", state, keyStatePending)
	}

	srv.setKeyState("example.com", keyStateFailed, "")

	detail := getHealthDetail(t, srv)
	if detail.Sites != 1 || detail.Keys["example.com"].State != keyStateFailed {
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
	srv.setKeyState("example.com", keyStateVerified, "")

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
	srv.setKeyState("example.com", keyStateVerified, "")

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
	srv.setKeyState("example.com", keyStateVerified, "")

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

// --- the egress guard -------------------------------------------------------

// otherHost and otherToken are the second site a per-site test needs: the point
// of keying the bucket on the host is that one site's exhaustion cannot touch
// another.
const (
	otherHost  = "shop.example.org"
	otherToken = "second-site-token"
)

// newTwoSiteServer builds a server with two configured hosts, each with its own
// token, so a test can exhaust one and check the other.
func newTwoSiteServer(t *testing.T, engine *fakeEngine) *server {
	t.Helper()

	cfg := &Config{
		Listen: "127.0.0.1:0",
		Sites: []Site{
			{
				Host:        "example.com",
				IndexNowKey: "abc123def456",
				KeyLocation: "https://example.com/abc123def456.txt",
				Token:       testToken,
			},
			{
				Host:        otherHost,
				IndexNowKey: "abc123def456",
				KeyLocation: "https://shop.example.org/abc123def456.txt",
				Token:       otherToken,
			},
		},
	}

	return newServer(cfg, newIndexNowClient(engine.server.URL), slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// frozenClock is a clock a test moves by hand. Refill is computed from elapsed
// time, so a test that waited in real time would be slow and still racy;
// stepping the clock is exact and costs nothing.
type frozenClock struct {
	now time.Time
}

func newFrozenClock() *frozenClock {
	return &frozenClock{now: time.Now()}
}

func (c *frozenClock) Now() time.Time {
	return c.now
}

func (c *frozenClock) advance(d time.Duration) {
	c.now = c.now.Add(d)
}

func TestRateBucketRefusesWhenTheBurstIsSpent(t *testing.T) {
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	// The clock stands still: this test measures the burst, not how fast the
	// machine can send eleven requests.
	clock := newFrozenClock()
	srv.rate.now = clock.Now

	for i := 0; i < rateBurstTokens; i++ {
		// A distinct URL per request: the resubmission window is not what this
		// test is about, and a repeated URL would be dropped before the engine
		// was ever called.
		recorder, decoded := postURL(t, srv, "Bearer "+testToken, "https://example.com/page-"+strconv.Itoa(i))

		if recorder.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200 (%s)", i+1, recorder.Code, recorder.Body.String())
		}
		if decoded["reason"] != nil {
			t.Fatalf("request %d: reason = %v, want none", i+1, decoded["reason"])
		}
	}

	if got := engine.calls(); got != rateBurstTokens {
		t.Fatalf("engine calls = %d, want %d", got, rateBurstTokens)
	}

	// The (capacity+1)th request in the same instant is refused locally.
	recorder, decoded := postURL(t, srv, "Bearer "+testToken, "https://example.com/last")

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (%s)", recorder.Code, recorder.Body.String())
	}
	if decoded["reason"] != reasonThrottled {
		t.Fatalf("reason = %v, want %q", decoded["reason"], reasonThrottled)
	}
	if got := recorder.Header().Get("Retry-After"); got == "" {
		t.Fatal("a throttled response carries no Retry-After header")
	}
	if decoded["retry_after_seconds"] != float64(1) {
		t.Fatalf("retry_after_seconds = %v, want 1", decoded["retry_after_seconds"])
	}
	if _, present := decoded["engine_status"]; present {
		t.Fatalf("a locally throttled batch reports an engine status: %v", decoded)
	}
	// Refused by the guard, so the engine never sees it: spending the egress IP
	// on this request is exactly what the guard exists to prevent.
	if got := engine.calls(); got != rateBurstTokens {
		t.Fatalf("engine calls after a local refusal = %d, want %d", got, rateBurstTokens)
	}
}

func TestRateLimitIsPerSite(t *testing.T) {
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTwoSiteServer(t, engine)

	clock := newFrozenClock()
	srv.rate.now = clock.Now

	for i := 0; i < rateBurstTokens; i++ {
		if recorder, _ := postURL(t, srv, "Bearer "+testToken, "https://example.com/page-"+strconv.Itoa(i)); recorder.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i+1, recorder.Code)
		}
	}

	if recorder, _ := postURL(t, srv, "Bearer "+testToken, "https://example.com/last"); recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("exhausted host answered %d, want 429", recorder.Code)
	}

	// The other site has spent nothing, so it must not inherit its neighbour's
	// limit: one noisy site is not allowed to silence the rest.
	recorder, decoded := postURL(t, srv, "Bearer "+otherToken, "https://"+otherHost+"/a")

	if recorder.Code != http.StatusOK {
		t.Fatalf("other site: status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
	if decoded["host"] != otherHost {
		t.Fatalf("other site: host = %v, want %q", decoded["host"], otherHost)
	}
	if got := engine.calls(); got != rateBurstTokens+1 {
		t.Fatalf("engine calls = %d, want %d", got, rateBurstTokens+1)
	}
}

func TestRefillAllowsTheNextRequest(t *testing.T) {
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	// A bucket of one token that refills every 100ms: the shortest interval a
	// test can observe without sleeping.
	srv.rate = newRateBucket(1, 100*time.Millisecond)
	clock := newFrozenClock()
	srv.rate.now = clock.Now

	if recorder, _ := postURL(t, srv, "Bearer "+testToken, "https://example.com/a"); recorder.Code != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200", recorder.Code)
	}

	recorder, decoded := postURL(t, srv, "Bearer "+testToken, "https://example.com/b")
	if recorder.Code != http.StatusTooManyRequests || decoded["reason"] != reasonThrottled {
		t.Fatalf("second request: status = %d reason = %v, want 429 %q", recorder.Code, decoded["reason"], reasonThrottled)
	}

	clock.advance(100 * time.Millisecond)

	if recorder, _ := postURL(t, srv, "Bearer "+testToken, "https://example.com/c"); recorder.Code != http.StatusOK {
		t.Fatalf("after a refill: status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}

	if got := engine.calls(); got != 2 {
		t.Fatalf("engine calls = %d, want 2", got)
	}
}

func TestRateBucketDoesNotRefillFromABackwardsClock(t *testing.T) {
	// Elapsed time is the only thing that mints tokens, and a clock that jumps
	// backwards must not mint any: a wall-clock step is not work done.
	bucket := newRateBucket(1, time.Second)
	clock := newFrozenClock()
	bucket.now = clock.Now

	if wait := bucket.take("example.com"); wait != 0 {
		t.Fatalf("first take waited %s, want 0", wait)
	}

	clock.advance(-time.Hour)

	if wait := bucket.take("example.com"); wait == 0 {
		t.Fatal("a backwards clock refilled the bucket")
	}
}

func TestOverloadedWhenNoEngineSlotIsFree(t *testing.T) {
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	srv.inflight = newInflightGuard(1, 1)
	// The wait is 2s in production; a test that spent 2s waiting would be slow,
	// and the behaviour under test is what happens when the wait runs out.
	srv.inflight.wait = 20 * time.Millisecond

	// Hold the instance's only slot, as an engine request still in flight would.
	// The host's slot is free, so this exercises the instance-wide cap.
	srv.inflight.global <- struct{}{}

	recorder, decoded := postURL(t, srv, "Bearer "+testToken, "https://example.com/a")

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (%s)", recorder.Code, recorder.Body.String())
	}
	if decoded["reason"] != reasonOverloaded {
		t.Fatalf("reason = %v, want %q", decoded["reason"], reasonOverloaded)
	}
	if got := recorder.Header().Get("Retry-After"); got == "" {
		t.Fatal("an overloaded response carries no Retry-After header")
	}
	if decoded["retry_after_seconds"] == nil {
		t.Fatalf("an overloaded response carries no retry_after_seconds: %v", decoded)
	}
	// The detail has to name the cap that refused, or the caller cannot tell
	// whether to slow one site down or the whole instance.
	detail, _ := decoded["detail"].(string)
	if !strings.Contains(detail, "the instance") {
		t.Fatalf("detail %q does not name the instance-wide cap", detail)
	}
	// No slot, so no engine call: the cap is there to keep the engine's side of
	// the connection finite, and a request that got no slot never becomes one.
	if got := engine.calls(); got != 0 {
		t.Fatalf("engine was called %d times without a free slot", got)
	}
	// The site's own slot was taken and given back, so the refusal does not leak
	// a host slot for as long as this instance stays busy.
	if held := len(srv.inflight.hostSlots("example.com")); held != 0 {
		t.Fatalf("a refused request kept %d of this site's slots", held)
	}

	// Once the slot is free the same request goes through, so a full instance
	// is busy rather than broken.
	<-srv.inflight.global

	if recorder, _ := postURL(t, srv, "Bearer "+testToken, "https://example.com/a"); recorder.Code != http.StatusOK {
		t.Fatalf("after the slot was freed: status = %d, want 200", recorder.Code)
	}
	if got := engine.calls(); got != 1 {
		t.Fatalf("engine calls = %d, want 1", got)
	}
}

func TestSubmitSeparatesEngineRefusalFromLocalRefusal(t *testing.T) {
	// Both answers are 422 host_mismatch, and they need different fixes from
	// different people: the engine refused what the caller sent, while this
	// instance refused to send it at all. engine_status is the only field that
	// tells them apart, so it must be present on the first and absent on the
	// second.
	engine := newFakeEngine(t, http.StatusUnprocessableEntity)
	srv := newTestServer(t, engine)

	recorder, decoded := post(t, srv, `{"urls":["https://example.com/a"]}`, "Bearer "+testToken)

	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("engine refusal: status = %d, want 422", recorder.Code)
	}
	if decoded["reason"] != reasonHostMismatch {
		t.Fatalf("engine refusal: reason = %v, want %q", decoded["reason"], reasonHostMismatch)
	}
	if decoded["engine_status"] != float64(http.StatusUnprocessableEntity) {
		t.Fatalf("engine refusal: engine_status = %v, want 422", decoded["engine_status"])
	}

	recorder, decoded = post(t, srv, `{"urls":["https://attacker.test/a"]}`, "Bearer "+testToken)

	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("local refusal: status = %d, want 422", recorder.Code)
	}
	if decoded["reason"] != reasonHostMismatch {
		t.Fatalf("local refusal: reason = %v, want %q", decoded["reason"], reasonHostMismatch)
	}
	if _, present := decoded["engine_status"]; present {
		t.Fatalf("local refusal claims an engine status: %v", decoded)
	}
	if got := engine.calls(); got != 1 {
		t.Fatalf("engine calls = %d, want 1 (the locally refused batch never reached it)", got)
	}
}

func TestSubmitLogsDurationAndRemote(t *testing.T) {
	var lines bytes.Buffer

	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServerWithLog(t, engine, slog.New(slog.NewTextHandler(&lines, nil)))

	post(t, srv, `{"urls":["https://example.com/a"]}`, "Bearer "+testToken)

	logged := lines.String()
	if !strings.Contains(logged, "duration_ms=") {
		t.Fatalf("submit log is missing duration_ms: %s", logged)
	}
	if !strings.Contains(logged, "remote=") {
		t.Fatalf("submit log is missing remote: %s", logged)
	}
}

func TestSubmitFinishesAfterTheCallerGivesUp(t *testing.T) {
	// A caller that times out must not cancel the POST to the engine: the URL
	// set is already decided, and the engine may already have received the
	// notice, so the caller's patience cannot change the outcome.
	// The engine takes 50ms to answer, and the caller is already gone by then.
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-time.After(50 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(engine.Close)

	cfg := &Config{
		Listen: "127.0.0.1:0",
		Sites: []Site{{
			Host:        "example.com",
			IndexNowKey: "abc123def456",
			KeyLocation: "https://example.com/abc123def456.txt",
			Token:       testToken,
		}},
	}
	srv := newServer(cfg, newIndexNowClient(engine.URL), slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	req := httptest.NewRequest(http.MethodPost, "/submit", strings.NewReader(`{"urls":["https://example.com/a"]}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req = req.WithContext(ctx)

	recorder := httptest.NewRecorder()
	srv.routes().ServeHTTP(recorder, req)

	// The engine answered after the caller had already gone, and the answer is
	// the engine's, not a cancelled request.
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
	if state := keyStateFor(srv, "example.com"); state != keyStateVerified {
		t.Fatalf("state after an accepted batch = %q, want %q", state, keyStateVerified)
	}
}

// --- key records ------------------------------------------------------------

func TestHealthDetailReportsCheckedAtAndDetail(t *testing.T) {
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	srv.setKeyState("example.com", keyStateFailed, "key file returned 404")

	recorder := callHealth(t, srv, "detail=1", "Bearer "+testToken)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}

	// The per-host value is an object, not a bare state word, so a caller can
	// see how old the answer is and what went wrong.
	var body struct {
		Keys map[string]map[string]json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	record, present := body.Keys["example.com"]
	if !present {
		t.Fatalf("keys has no entry for example.com: %s", recorder.Body.String())
	}
	for _, field := range []string{"state", "checked_at", "detail"} {
		if _, present := record[field]; !present {
			t.Fatalf("key record is missing %q: %s", field, recorder.Body.String())
		}
	}

	var state keyState
	if err := json.Unmarshal(record["state"], &state); err != nil || state != keyStateFailed {
		t.Fatalf("state = %s (%v), want %q", record["state"], err, keyStateFailed)
	}

	var detail string
	if err := json.Unmarshal(record["detail"], &detail); err != nil || detail != "key file returned 404" {
		t.Fatalf("detail = %s (%v), want the failure reason", record["detail"], err)
	}

	var checkedAt time.Time
	if err := json.Unmarshal(record["checked_at"], &checkedAt); err != nil {
		t.Fatalf("checked_at is not an RFC 3339 time: %s (%v)", record["checked_at"], err)
	}
	if time.Since(checkedAt) > time.Minute {
		t.Fatalf("checked_at = %s, want the time the state was recorded", checkedAt)
	}

	// The unauthenticated view keeps its counts-only shape.
	if strings.Contains(callHealth(t, srv, "", "").Body.String(), "example.com") {
		t.Fatal("the aggregate view names a host")
	}
}

func TestRecheckRefreshesCheckedAt(t *testing.T) {
	var mu sync.Mutex
	hits := 0

	keyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()

		_, _ = w.Write([]byte("abc123def456\n"))
	}))
	defer keyServer.Close()

	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)
	srv.cfg.Sites[0].KeyLocation = keyServer.URL + "/key.txt"

	stale := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	srv.mu.Lock()
	srv.keyStates["example.com"] = keyRecord{State: keyStateFailed, CheckedAt: stale, Detail: "stale answer"}
	srv.mu.Unlock()

	keyFetches := func() int {
		mu.Lock()
		defer mu.Unlock()
		return hits
	}

	// Reading the detail view answers from what is recorded: it does not fetch
	// anything, which is the whole reason ?recheck=1 exists.
	before := getHealthDetail(t, srv).Keys["example.com"]
	if before.State != keyStateFailed || !before.CheckedAt.Equal(stale) || before.Detail != "stale answer" {
		t.Fatalf("detail before a recheck = %+v, want the recorded answer", before)
	}
	if got := keyFetches(); got != 0 {
		t.Fatalf("detail=1 fetched the key file %d times", got)
	}

	after := getHealthDetailWith(t, srv, "detail=1&recheck=1").Keys["example.com"]

	if after.State != keyStateVerified {
		t.Fatalf("state after a recheck = %q, want %q", after.State, keyStateVerified)
	}
	if after.Detail != "" {
		t.Fatalf("detail after a verified recheck = %q, want empty", after.Detail)
	}
	if !after.CheckedAt.After(stale) {
		t.Fatalf("checked_at after a recheck = %s, want later than %s", after.CheckedAt, stale)
	}
	if got := keyFetches(); got != 1 {
		t.Fatalf("recheck fetched the key file %d times, want 1", got)
	}
}

func TestRecheckCutShortDoesNotDowngrade(t *testing.T) {
	// A recheck that runs out of budget learned nothing about the key file. It
	// must not turn a good state into a bad one just because this instance ran
	// out of patience.
	keyServer := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer keyServer.Close()

	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)
	srv.cfg.Sites[0].KeyLocation = keyServer.URL + "/key.txt"
	srv.recheckTimeout = 20 * time.Millisecond
	srv.setKeyState("example.com", keyStateVerified, "")

	after := getHealthDetailWith(t, srv, "detail=1&recheck=1").Keys["example.com"]

	if after.State != keyStateVerified {
		t.Fatalf("state after a cut-short recheck = %q, want %q left alone", after.State, keyStateVerified)
	}
}

func TestHealthDetailRecheckRequiresToken(t *testing.T) {
	// A recheck must never be reachable unauthenticated: it would let anything
	// that can reach the port make this instance fetch from the operator's
	// origins.
	var mu sync.Mutex
	hits := 0

	keyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()

		_, _ = w.Write([]byte("abc123def456\n"))
	}))
	defer keyServer.Close()

	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)
	srv.cfg.Sites[0].KeyLocation = keyServer.URL + "/key.txt"

	for _, authorization := range []string{"", "Bearer wrong"} {
		recorder := callHealth(t, srv, "detail=1&recheck=1", authorization)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%q: status = %d, want 401", authorization, recorder.Code)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if hits != 0 {
		t.Fatalf("an unauthenticated recheck fetched the key file %d times", hits)
	}
}

// --- the resubmission window ------------------------------------------------

// windowLen reads how many URLs the window remembers, under its lock, the way
// the server does.
func windowLen(w *submitWindow) int {
	w.mu.Lock()
	defer w.mu.Unlock()

	return len(w.sent)
}

func TestResubmitWindowDropsARepeatWithinTheWindow(t *testing.T) {
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	clock := newFrozenClock()
	srv.window.now = clock.Now

	if recorder, _ := postURL(t, srv, "Bearer "+testToken, "https://example.com/a"); recorder.Code != http.StatusOK {
		t.Fatalf("first: status = %d, want 200", recorder.Code)
	}

	clock.advance(time.Minute)

	// The same URL over the other method: there is one window and one submission
	// path, so which method a caller used cannot change what was already sent.
	recorder, body := getSubmit(t, srv, submitQuery("https://example.com/a"), "Bearer "+testToken)

	if recorder.Code != http.StatusOK {
		t.Fatalf("repeat: status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
	// Nothing to send is not a failure: the caller asked for something that is
	// already true, so the answer is a plain ok with nothing sent.
	if body["ok"] != true {
		t.Fatalf("a suppressed batch is not ok: %v", body)
	}
	if body["submitted"] != float64(0) || body["skipped"] != float64(1) {
		t.Fatalf("repeat: submitted = %v, skipped = %v, want 0 and 1", body["submitted"], body["skipped"])
	}
	if _, present := body["reason"]; present {
		t.Fatalf("a suppressed batch carries a reason: %v", body)
	}
	if _, present := body["engine_status"]; present {
		t.Fatalf("a suppressed batch claims an engine status: %v", body)
	}
	if got := engine.calls(); got != 1 {
		t.Fatalf("engine calls = %d, want 1", got)
	}
}

func TestResubmitWindowReleasesAfterTheWindow(t *testing.T) {
	// A suppressed URL must not extend its own window: the timestamp is written
	// when a URL is actually sent, so a URL retried every minute is sent again a
	// window after the send, not a window after the last attempt.
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	clock := newFrozenClock()
	srv.window.now = clock.Now

	postURL(t, srv, "Bearer "+testToken, "https://example.com/a")

	clock.advance(resubmitWindow - time.Second)

	if _, body := postURL(t, srv, "Bearer "+testToken, "https://example.com/a"); body["skipped"] != float64(1) {
		t.Fatalf("inside the window: skipped = %v, want 1", body["skipped"])
	}

	// A window after the send, with the suppressed attempt in between.
	clock.advance(time.Second)

	recorder, body := postURL(t, srv, "Bearer "+testToken, "https://example.com/a")

	if recorder.Code != http.StatusOK {
		t.Fatalf("after the window: status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
	if body["submitted"] != float64(1) {
		t.Fatalf("after the window: submitted = %v, want 1", body["submitted"])
	}
	if got := engine.calls(); got != 2 {
		t.Fatalf("engine calls = %d, want 2", got)
	}
}

func TestResubmitWindowCountsSuppressedURLsBesideSentOnes(t *testing.T) {
	// A partly suppressed batch still sends the rest, and the dropped URL is
	// counted rather than vanishing from a response that says submitted: 1.
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	clock := newFrozenClock()
	srv.window.now = clock.Now

	postURL(t, srv, "Bearer "+testToken, "https://example.com/a")
	clock.advance(time.Minute)

	recorder, body := postURL(t, srv, "Bearer "+testToken, "https://example.com/a", "https://example.com/b")

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
	if body["submitted"] != float64(1) || body["skipped"] != float64(1) {
		t.Fatalf("submitted = %v, skipped = %v, want 1 and 1", body["submitted"], body["skipped"])
	}
	if got := engine.urls(); len(got) != 1 || got[0] != "https://example.com/b" {
		t.Fatalf("engine received %v, want only the url that was not suppressed", got)
	}
}

func TestResubmitWindowIsPerHost(t *testing.T) {
	// Two sites serve the same path. The window is keyed on the site's host, so
	// one site's submission cannot silence another's URL.
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTwoSiteServer(t, engine)

	clock := newFrozenClock()
	srv.window.now = clock.Now

	if recorder, _ := postURL(t, srv, "Bearer "+testToken, "https://example.com/a"); recorder.Code != http.StatusOK {
		t.Fatalf("first site: status = %d, want 200", recorder.Code)
	}

	recorder, body := postURL(t, srv, "Bearer "+otherToken, "https://"+otherHost+"/a")

	if recorder.Code != http.StatusOK {
		t.Fatalf("second site: status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
	if body["submitted"] != float64(1) {
		t.Fatalf("second site: submitted = %v, want 1", body["submitted"])
	}
	if _, present := body["skipped"]; present {
		t.Fatalf("second site was suppressed by the first: %v", body)
	}
	if got := engine.calls(); got != 2 {
		t.Fatalf("engine calls = %d, want 2", got)
	}
}

func TestResubmitWindowForgetsAFailedSubmission(t *testing.T) {
	// Only an accepted batch proves the URLs were taken. A failure leaves the
	// caller a retry to make, and suppressing that retry would answer 200
	// "submitted: 0" to a caller whose first attempt never landed.
	engine := newFakeEngine(t, http.StatusInternalServerError)
	srv := newTestServer(t, engine)

	clock := newFrozenClock()
	srv.window.now = clock.Now

	if recorder, _ := postURL(t, srv, "Bearer "+testToken, "https://example.com/a"); recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", recorder.Code)
	}

	engine.setStatus(http.StatusOK)

	recorder, body := postURL(t, srv, "Bearer "+testToken, "https://example.com/a")

	if recorder.Code != http.StatusOK {
		t.Fatalf("retry: status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
	if body["submitted"] != float64(1) {
		t.Fatalf("retry: submitted = %v, want the url sent again", body["submitted"])
	}
	if got := engine.calls(); got != 2 {
		t.Fatalf("engine calls = %d, want 2", got)
	}
}

func TestResubmitWindowStaysBounded(t *testing.T) {
	// A caller can send arbitrary URLs, so the map has a cap: an unbounded one is
	// a leak that caller controls. Past the cap the oldest entry goes, because
	// every entry expires anyway and the oldest is the next to go.
	const limit = 4

	window := newSubmitWindow(resubmitWindow, limit)
	clock := newFrozenClock()
	window.now = clock.Now

	for i := 0; i < limit; i++ {
		window.record("example.com", []string{"https://example.com/page-" + strconv.Itoa(i)})
		clock.advance(time.Second)
	}

	if got := windowLen(window); got != limit {
		t.Fatalf("window holds %d urls, want %d", got, limit)
	}

	window.record("example.com", []string{"https://example.com/one-more"})

	if got := windowLen(window); got != limit {
		t.Fatalf("window holds %d urls after one insert past the cap, want %d", got, limit)
	}

	// page-0 was the oldest, so it is the entry that went: asking about it keeps
	// it, while its neighbour is still inside the window.
	if kept, skipped := window.keep("example.com", []string{"https://example.com/page-0"}); skipped != 0 || len(kept) != 1 {
		t.Fatalf("the oldest entry survived the cap: skipped %d, kept %v", skipped, kept)
	}
	if _, skipped := window.keep("example.com", []string{"https://example.com/page-1"}); skipped != 1 {
		t.Fatalf("a recent entry was evicted: skipped %d, want 1", skipped)
	}

	// A batch larger than the whole window cannot be remembered in full. The cap
	// still wins: it is what keeps arbitrary input from growing this structure.
	window.record("example.com", []string{
		"https://example.com/a", "https://example.com/b", "https://example.com/c",
		"https://example.com/d", "https://example.com/e", "https://example.com/f",
	})

	if got := windowLen(window); got > limit {
		t.Fatalf("window holds %d urls after an oversized batch, want at most %d", got, limit)
	}
}

func TestResubmitWindowIsSafeUnderConcurrency(t *testing.T) {
	// The window is shared by every request, so the race detector is the point of
	// this test: many goroutines keep and record at once, on a window small enough
	// that all of them also evict.
	window := newSubmitWindow(resubmitWindow, 8)

	var wg sync.WaitGroup

	for worker := 0; worker < 8; worker++ {
		wg.Add(1)

		go func(worker int) {
			defer wg.Done()

			urls := []string{
				"https://example.com/page-" + strconv.Itoa(worker),
				"https://example.com/shared",
			}

			for i := 0; i < 50; i++ {
				kept, skipped := window.keep("example.com", urls)
				if skipped > len(urls) {
					t.Errorf("skipped %d of %d urls", skipped, len(urls))
				}
				window.record("example.com", kept)
			}
		}(worker)
	}

	wg.Wait()

	if got := windowLen(window); got > 8 {
		t.Fatalf("window holds %d urls, want at most 8", got)
	}
}

func TestSubmitLogsSkipped(t *testing.T) {
	var lines bytes.Buffer

	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServerWithLog(t, engine, slog.New(slog.NewTextHandler(&lines, nil)))

	clock := newFrozenClock()
	srv.window.now = clock.Now

	postURL(t, srv, "Bearer "+testToken, "https://example.com/a")
	postURL(t, srv, "Bearer "+testToken, "https://example.com/a")

	if !strings.Contains(lines.String(), "skipped=1") {
		t.Fatalf("submit log does not say what the window dropped: %s", lines.String())
	}
}

func TestSubmitCarriesARaisedCeilingEndToEnd(t *testing.T) {
	// The documented reason to submit a lot at once is a site migration, and the
	// point of making the limits configurable is that such a batch gets through
	// instead of being refused at 100. This drives the real handler, so it also
	// pins that the body cap moved with the count: 400 realistic URLs is well
	// past 64 KiB, so a body cap left at the old default would answer 413 here.
	const count = 400
	urls := make([]string, 0, count)
	for i := 0; i < count; i++ {
		urls = append(urls, fmt.Sprintf("https://example.com/2026/11/a-rather-long-article-slug-number-%03d?utm_source=%s", i, strings.Repeat("x", 160)))
	}

	body, err := json.Marshal(map[string]any{"urls": urls})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(body) <= maxBodyBytes {
		t.Fatalf("the fixture is %d bytes, which does not exceed the default cap; it proves nothing", len(body))
	}

	engine := newFakeEngine(t, http.StatusOK)
	cfg := &Config{
		Listen: "127.0.0.1:0",
		Limits: Limits{MaxURLs: count, MaxBodyBytes: 1 << 20},
		Sites: []Site{{
			Host:        "example.com",
			IndexNowKey: "abc123def456",
			KeyLocation: "https://example.com/abc123def456.txt",
			Token:       testToken,
		}},
	}
	srv := newServer(cfg, newIndexNowClient(engine.server.URL), slog.New(slog.NewTextHandler(io.Discard, nil)))

	recorder, decoded := post(t, srv, string(body), "Bearer "+testToken)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
	if decoded["submitted"] != float64(count) {
		t.Fatalf("submitted = %v, want %d", decoded["submitted"], count)
	}
	if len(engine.gotURLs) != count {
		t.Fatalf("engine received %d urls, want %d", len(engine.gotURLs), count)
	}

	// And the same body is still refused when the ceiling is left at the default,
	// which is what makes the two settings one decision.
	engine = newFakeEngine(t, http.StatusOK)
	srv = newServer(&Config{
		Listen: "127.0.0.1:0",
		Sites:  cfg.Sites,
	}, newIndexNowClient(engine.server.URL), slog.New(slog.NewTextHandler(io.Discard, nil)))

	recorder, decoded = post(t, srv, string(body), "Bearer "+testToken)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d at the default limits, want 413", recorder.Code)
	}
	if decoded["reason"] != reasonPayloadTooLarge {
		t.Fatalf("reason = %v, want %q", decoded["reason"], reasonPayloadTooLarge)
	}
	if engine.gotCalls != 0 {
		t.Fatalf("engine was called %d times for an oversized body", engine.gotCalls)
	}
}
