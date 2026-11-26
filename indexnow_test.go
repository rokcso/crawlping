package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubEngine stands in for api.indexnow.org so the tests can pin what the
// client sends and what it does with the answer.
type stubEngine struct {
	server  *httptest.Server
	status  int
	body    string
	calls   int
	gotURLs []string
}

func newStubEngine(t *testing.T, status int, body string) *stubEngine {
	t.Helper()

	engine := &stubEngine{status: status, body: body}
	engine.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		engine.calls++

		var payload struct {
			URLList []string `json:"urlList"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		engine.gotURLs = payload.URLList

		w.WriteHeader(engine.status)
		if engine.body != "" {
			_, _ = io.WriteString(w, engine.body)
		}
	}))
	t.Cleanup(engine.server.Close)

	return engine
}

func (e *stubEngine) client() *indexNowClient {
	return newIndexNowClient(e.server.URL)
}

func stubSite(host string) Site {
	return Site{
		Host:        host,
		IndexNowKey: "test-key",
		KeyLocation: "https://" + host + "/test-key.txt",
	}
}

func buildURLs(host string, count int) []string {
	urls := make([]string, 0, count)
	for i := 0; i < count; i++ {
		urls = append(urls, fmt.Sprintf("https://%s/page-%d", host, i))
	}
	return urls
}

func TestFilterURLsRefusesWithAnAccurateError(t *testing.T) {
	cases := []struct {
		name     string
		host     string
		urls     []string
		want     error
		mentions string
	}{
		{
			name:     "foreign host",
			host:     "example.com",
			urls:     []string{"https://example.com/a", "https://evil.test/b"},
			want:     errForeignHost,
			mentions: "https://evil.test/b",
		},
		{
			name:     "lookalike subdomain",
			host:     "example.com",
			urls:     []string{"https://example.com.evil.test/a"},
			want:     errForeignHost,
			mentions: "https://example.com.evil.test/a",
		},
		{
			name:     "non-http scheme",
			host:     "example.com",
			urls:     []string{"javascript:alert(1)"},
			want:     errInvalidURL,
			mentions: "javascript:alert(1)",
		},
		{
			name:     "garbage",
			host:     "example.com",
			urls:     []string{"not a url"},
			want:     errInvalidURL,
			mentions: "not a url",
		},
		{
			name:     "relative url",
			host:     "example.com",
			urls:     []string{"/a"},
			want:     errInvalidURL,
			mentions: "/a",
		},
		{
			name:     "non-default port",
			host:     "example.com",
			urls:     []string{"https://example.com:8443/a"},
			want:     errNonDefaultPort,
			mentions: "https://example.com:8443/a",
		},
		{
			name: "empty batch",
			host: "example.com",
			urls: nil,
			want: errNoURLs,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			clean, err := filterURLs(testCase.host, testCase.urls)

			if err == nil {
				t.Fatalf("expected a refusal, got %v", clean)
			}
			if !errors.Is(err, testCase.want) {
				t.Fatalf("err = %v, want %v", err, testCase.want)
			}
			if testCase.mentions != "" && !strings.Contains(err.Error(), testCase.mentions) {
				t.Fatalf("error does not name the offending entry: %v", err)
			}
			if len(clean) != 0 {
				t.Fatalf("clean = %v, want no urls", clean)
			}

			reason := reasonForURLError(err)
			if testCase.want == errForeignHost {
				if reason != reasonHostMismatch {
					t.Fatalf("reason = %q, want %q", reason, reasonHostMismatch)
				}
				return
			}
			// A request problem must never be reported as a host problem: that
			// sends the caller looking at their domain instead of their batch.
			if reason == reasonHostMismatch {
				t.Fatalf("reason = %q for %v, want a request reason", reason, err)
			}
		})
	}
}

func TestFilterURLsSentinelRegression(t *testing.T) {
	// An empty entry used to be indistinguishable from "no error", so a good
	// URL next to it was dropped and the caller was told "no usable urls".
	clean, err := filterURLs("example.com", []string{"https://example.com/ok", ""})

	if err == nil {
		t.Fatalf("expected a refusal, got %v", clean)
	}
	if errors.Is(err, errNoURLs) {
		t.Fatalf("empty entry reported as %q: %v", errNoURLs, err)
	}
	if !errors.Is(err, errInvalidURL) {
		t.Fatalf("err = %v, want %v", err, errInvalidURL)
	}
	if !strings.Contains(err.Error(), "entry 2") || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("error does not name the offending entry: %v", err)
	}
	if len(clean) != 0 {
		t.Fatalf("clean = %v, want no urls", clean)
	}
}

func TestFilterURLsRejectsOversizedBatchAsTooMany(t *testing.T) {
	urls := buildURLs("example.com", maxURLsPerRequest+1)

	clean, err := filterURLs("example.com", urls)

	if err == nil {
		t.Fatalf("expected a refusal, got %d urls", len(clean))
	}
	if !errors.Is(err, errTooManyURLs) {
		t.Fatalf("err = %v, want %v", err, errTooManyURLs)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("more than %d urls", maxURLsPerRequest)) {
		t.Fatalf("error does not explain the limit: %v", err)
	}
	if reason := reasonForURLError(err); reason != reasonTooMany {
		t.Fatalf("reason = %q, want %q", reason, reasonTooMany)
	}
	if got := httpStatusFor(submissionResult{Reason: reasonTooMany}); got != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", got, http.StatusBadRequest)
	}
}

func TestFilterURLsStripsFragments(t *testing.T) {
	clean, err := filterURLs("example.com", []string{
		"https://example.com/a#s1",
		"https://example.com/a#s2",
	})

	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if len(clean) != 1 {
		t.Fatalf("clean = %v, want one url", clean)
	}
	if clean[0] != "https://example.com/a" {
		t.Fatalf("clean[0] = %q, want the fragment dropped", clean[0])
	}
}

func TestFilterURLsNormalizesHost(t *testing.T) {
	cases := []struct {
		name string
		host string
		urls []string
	}{
		{
			name: "trailing dot in the configured host",
			host: "example.com.",
			urls: []string{"https://example.com/a"},
		},
		{
			name: "trailing dot and case in the configured host",
			host: "Example.COM.",
			urls: []string{"https://example.com/a"},
		},
		{
			name: "trailing dot and case in the url",
			host: "example.com",
			urls: []string{"https://Example.COM./a"},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			clean, err := filterURLs(testCase.host, testCase.urls)
			if err != nil {
				t.Fatalf("filter: %v", err)
			}
			if len(clean) != 1 {
				t.Fatalf("clean = %v, want one url", clean)
			}
		})
	}
}

func TestFilterURLsAcceptsDefaultPort(t *testing.T) {
	clean, err := filterURLs("example.com", []string{
		"https://example.com:443/a",
		"http://example.com:80/b",
	})

	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if len(clean) != 2 {
		t.Fatalf("clean = %v, want two urls", clean)
	}
}

func TestFilterURLsCollapsesFragmentOnlyDuplicatesForEngine(t *testing.T) {
	engine := newStubEngine(t, http.StatusOK, "")
	result := engine.client().submit(stubSite("example.com"), []string{
		"https://example.com/a#s1",
		"https://example.com/a#s2",
		"https://example.com/b",
	})

	if !result.accepted() {
		t.Fatalf("reason = %q (%s)", result.Reason, result.Detail)
	}
	if result.Submitted != 2 {
		t.Fatalf("submitted = %d, want 2", result.Submitted)
	}
	if len(engine.gotURLs) != 2 {
		t.Fatalf("engine received %d urls, want 2: %v", len(engine.gotURLs), engine.gotURLs)
	}
	for _, sent := range engine.gotURLs {
		if strings.Contains(sent, "#") {
			t.Fatalf("engine received a fragment: %q", sent)
		}
	}
}

func TestFilterURLsRootKeyScopeAllowsEveryPath(t *testing.T) {
	// A key file at the host root answers for the whole host, which is both the
	// default configuration and every deployment that predates path scoping, so
	// the root scope must not restrict anything.
	urls := []string{
		"https://example.com/anything",
		"https://example.com/",
		"https://example.com/a/b",
		"https://example.com/help/deep/page",
	}

	for _, scope := range []string{"/", ""} {
		clean, err := filterURLsInScope("example.com", scope, urls)
		if err != nil {
			t.Fatalf("scope %q: %v", scope, err)
		}
		if len(clean) != len(urls) {
			t.Fatalf("scope %q: clean = %v, want every url kept", scope, clean)
		}
	}

	// The unscoped call is the same call, so an existing caller keeps working.
	clean, err := filterURLs("example.com", urls)
	if err != nil {
		t.Fatalf("filterURLs: %v", err)
	}
	if len(clean) != len(urls) {
		t.Fatalf("filterURLs: clean = %v, want every url kept", clean)
	}
}

func TestFilterURLsInScopeKeepsOnlyURLsUnderTheKeyDirectory(t *testing.T) {
	// The spec: a key file at /catalog/<key>.txt may submit URLs starting with
	// /catalog/ and nothing else. /catalogue/x is the case a plain string
	// prefix check gets wrong, and /catalog is the directory itself, which is
	// inside it.
	cases := []struct {
		name string
		url  string
		want bool
	}{
		{name: "the directory itself", url: "https://example.com/catalog", want: true},
		{name: "the directory with a trailing slash", url: "https://example.com/catalog/", want: true},
		{name: "a page under it", url: "https://example.com/catalog/x", want: true},
		{name: "a page under it with a query", url: "https://example.com/catalog/show?item=23", want: true},
		{name: "the host root", url: "https://example.com/", want: false},
		{name: "another directory", url: "https://example.com/help/page", want: false},
		{name: "a sibling sharing the name prefix", url: "https://example.com/catalogue/x", want: false},
		{name: "a dot segment out of the directory", url: "https://example.com/catalog/../help", want: false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			clean, err := filterURLsInScope("example.com", "/catalog/", []string{testCase.url})

			if testCase.want {
				if err != nil {
					t.Fatalf("unexpected refusal: %v", err)
				}
				if len(clean) != 1 {
					t.Fatalf("clean = %v, want the url kept", clean)
				}
				return
			}

			if err == nil {
				t.Fatalf("expected a refusal, got %v", clean)
			}
			if !errors.Is(err, errOutOfKeyScope) {
				t.Fatalf("err = %v, want %v", err, errOutOfKeyScope)
			}
			if !strings.Contains(err.Error(), testCase.url) {
				t.Fatalf("error does not name the offending url: %v", err)
			}
			if !strings.Contains(err.Error(), "/catalog/") {
				t.Fatalf("error does not name the allowed prefix: %v", err)
			}
			if reason := reasonForURLError(err); reason != reasonOutOfKeyScope {
				t.Fatalf("reason = %q, want %q", reason, reasonOutOfKeyScope)
			}
			if got := httpStatusFor(submissionResult{Reason: reasonOutOfKeyScope}); got != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", got, http.StatusBadRequest)
			}
		})
	}
}

func TestFilterURLsInScopeComparesSegmentsAtAnyDepth(t *testing.T) {
	// A nested key file scopes to the nested directory only.
	clean, err := filterURLsInScope("example.com", "/a/b/", []string{
		"https://example.com/a/b",
		"https://example.com/a/b/c/d",
	})
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if len(clean) != 2 {
		t.Fatalf("clean = %v, want two urls", clean)
	}

	for _, outside := range []string{"https://example.com/a", "https://example.com/a/bc/d", "https://example.com/b/c"} {
		if _, err := filterURLsInScope("example.com", "/a/b/", []string{outside}); !errors.Is(err, errOutOfKeyScope) {
			t.Fatalf("%q: err = %v, want %v", outside, err, errOutOfKeyScope)
		}
	}
}

func TestFilterURLsInScopeLeavesForeignHostAsHostMismatch(t *testing.T) {
	// A foreign host stays host_mismatch even when it is also outside the scope:
	// the host is the more basic problem and the fix is different.
	_, err := filterURLsInScope("example.com", "/catalog/", []string{"https://evil.test/help/page"})

	if !errors.Is(err, errForeignHost) {
		t.Fatalf("err = %v, want %v", err, errForeignHost)
	}
	if reason := reasonForURLError(err); reason != reasonHostMismatch {
		t.Fatalf("reason = %q, want %q", reason, reasonHostMismatch)
	}
}

func TestSubmitRefusesOutOfScopeURLWithoutCallingEngine(t *testing.T) {
	engine := newStubEngine(t, http.StatusOK, "")
	site := stubSite("example.com")
	site.PathScope = "/catalog/"

	result := engine.client().submit(site, []string{
		"https://example.com/catalog/ok",
		"https://example.com/help/page",
	})

	if engine.calls != 0 {
		t.Fatalf("engine was called %d times for an out-of-scope batch", engine.calls)
	}
	if result.Reason != reasonOutOfKeyScope {
		t.Fatalf("reason = %q, want %q", result.Reason, reasonOutOfKeyScope)
	}
	if !strings.Contains(result.Detail, "https://example.com/help/page") {
		t.Fatalf("detail does not name the offending url: %q", result.Detail)
	}
	if !strings.Contains(result.Detail, "/catalog/") {
		t.Fatalf("detail does not name the allowed prefix: %q", result.Detail)
	}
	if result.EngineStatus != 0 {
		t.Fatalf("engine status = %d, want 0", result.EngineStatus)
	}
	if got := httpStatusFor(result); got != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", got, http.StatusBadRequest)
	}
}

func TestSubmitForwardsURLsInsideKeyScope(t *testing.T) {
	engine := newStubEngine(t, http.StatusOK, "")
	site := stubSite("example.com")
	site.PathScope = "/catalog/"

	result := engine.client().submit(site, []string{
		"https://example.com/catalog",
		"https://example.com/catalog/show?item=23",
	})

	if !result.accepted() {
		t.Fatalf("reason = %q (%s)", result.Reason, result.Detail)
	}
	if result.Submitted != 2 {
		t.Fatalf("submitted = %d, want 2", result.Submitted)
	}
	if engine.calls != 1 {
		t.Fatalf("engine was called %d times, want 1", engine.calls)
	}
}

func TestSubmitLocalRefusalLeavesEngineStatusZero(t *testing.T) {
	engine := newStubEngine(t, http.StatusOK, "")
	result := engine.client().submit(stubSite("example.com"), []string{"https://example.com/ok", ""})

	if engine.calls != 0 {
		t.Fatalf("engine was called %d times for a refused batch", engine.calls)
	}
	if result.EngineStatus != 0 {
		t.Fatalf("engine status = %d, want 0 (the engine was never called)", result.EngineStatus)
	}
	if result.Reason != reasonInvalidURL {
		t.Fatalf("reason = %q, want %q", result.Reason, reasonInvalidURL)
	}
	if !strings.Contains(result.Detail, "entry 2") {
		t.Fatalf("detail does not name the offending entry: %q", result.Detail)
	}
	if got := httpStatusFor(result); got != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", got, http.StatusBadRequest)
	}
}

func TestSubmitRefusesOversizedBatchWithoutCallingEngine(t *testing.T) {
	engine := newStubEngine(t, http.StatusOK, "")
	result := engine.client().submit(stubSite("example.com"), buildURLs("example.com", maxURLsPerRequest+1))

	if engine.calls != 0 {
		t.Fatalf("engine was called %d times for an oversized batch", engine.calls)
	}
	if result.EngineStatus != 0 {
		t.Fatalf("engine status = %d, want 0", result.EngineStatus)
	}
	if result.Reason != reasonTooMany {
		t.Fatalf("reason = %q, want %q", result.Reason, reasonTooMany)
	}
	if got := httpStatusFor(result); got != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", got, http.StatusBadRequest)
	}
}

func TestSubmitKeepsOneLineOfEngineDetail(t *testing.T) {
	engine := newStubEngine(t, http.StatusServiceUnavailable, "upstream\n  exploded\t badly\r\n\x00\x01")
	result := engine.client().submit(stubSite("example.com"), []string{"https://example.com/a"})

	if result.Reason != reasonEngineError {
		t.Fatalf("reason = %q, want %q", result.Reason, reasonEngineError)
	}
	if result.EngineStatus != http.StatusServiceUnavailable {
		t.Fatalf("engine status = %d, want %d", result.EngineStatus, http.StatusServiceUnavailable)
	}
	if result.Detail != "upstream exploded badly" {
		t.Fatalf("detail = %q", result.Detail)
	}
	if strings.ContainsAny(result.Detail, "\n\r\t") {
		t.Fatalf("detail is not a single line: %q", result.Detail)
	}
}

func TestSubmitBoundsEngineDetail(t *testing.T) {
	engine := newStubEngine(t, http.StatusInternalServerError, strings.Repeat("long-body ", 2000))
	result := engine.client().submit(stubSite("example.com"), []string{"https://example.com/a"})

	if result.Detail == "" {
		t.Fatal("expected the engine body to reach Detail")
	}
	if len(result.Detail) > maxEngineDetailBytes {
		t.Fatalf("detail is %d bytes, want at most %d", len(result.Detail), maxEngineDetailBytes)
	}
}

func TestSubmitMapsEngineStatusToReason(t *testing.T) {
	cases := []struct {
		name         string
		engineStatus int
		wantReason   string
		wantHTTP     int
	}{
		{"accepted", http.StatusOK, reasonAccepted, http.StatusOK},
		{"accepted asynchronously", http.StatusAccepted, reasonAccepted, http.StatusOK},
		{"bad request", http.StatusBadRequest, reasonBadRequest, http.StatusBadRequest},
		{"unauthorized", http.StatusUnauthorized, reasonEngineRejected, http.StatusBadRequest},
		{"forbidden", http.StatusForbidden, reasonKeyRejected, http.StatusForbidden},
		{"payload too large", http.StatusRequestEntityTooLarge, reasonEngineRejected, http.StatusBadRequest},
		{"unprocessable", http.StatusUnprocessableEntity, reasonHostMismatch, http.StatusUnprocessableEntity},
		{"rate limited", http.StatusTooManyRequests, reasonRateLimited, http.StatusTooManyRequests},
		{"engine error", http.StatusInternalServerError, reasonEngineError, http.StatusBadGateway},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			engine := newStubEngine(t, testCase.engineStatus, "")
			result := engine.client().submit(stubSite("example.com"), []string{"https://example.com/a"})

			if engine.calls != 1 {
				t.Fatalf("engine was called %d times, want 1", engine.calls)
			}
			if result.EngineStatus != testCase.engineStatus {
				t.Fatalf("engine status = %d, want %d", result.EngineStatus, testCase.engineStatus)
			}
			if result.Reason != testCase.wantReason {
				t.Fatalf("reason = %q, want %q", result.Reason, testCase.wantReason)
			}
			if got := httpStatusFor(result); got != testCase.wantHTTP {
				t.Fatalf("status = %d, want %d", got, testCase.wantHTTP)
			}
		})
	}
}

func TestHTTPStatusForMapsEveryReason(t *testing.T) {
	cases := map[string]int{
		reasonAccepted:         http.StatusOK,
		reasonBadRequest:       http.StatusBadRequest,
		reasonInvalidURL:       http.StatusBadRequest,
		reasonTooMany:          http.StatusBadRequest,
		reasonNonDefaultPort:   http.StatusBadRequest,
		reasonOutOfKeyScope:    http.StatusBadRequest,
		reasonEngineRejected:   http.StatusBadRequest,
		reasonKeyRejected:      http.StatusForbidden,
		reasonHostMismatch:     http.StatusUnprocessableEntity,
		reasonRateLimited:      http.StatusTooManyRequests,
		reasonThrottled:        http.StatusTooManyRequests,
		reasonOverloaded:       http.StatusServiceUnavailable,
		reasonEngineError:      http.StatusBadGateway,
		"a_reason_added_later": http.StatusBadGateway,
	}

	for reason, want := range cases {
		if got := httpStatusFor(submissionResult{Reason: reason}); got != want {
			t.Fatalf("%q: status = %d, want %d", reason, got, want)
		}
	}
}

func TestKeyStateAfterSubmission(t *testing.T) {
	// Only the engine's own verdicts say something about the key. A rate limit
	// or an engine error must not be read as a broken key file.
	cases := []struct {
		name         string
		engineStatus int
		want         keyState
	}{
		{name: "key validated", engineStatus: http.StatusOK, want: keyStateVerified},
		{name: "validation still pending", engineStatus: http.StatusAccepted, want: keyStatePending},
		{name: "key refused", engineStatus: http.StatusForbidden, want: keyStateFailed},
		{name: "rate limited proves nothing", engineStatus: http.StatusTooManyRequests, want: ""},
		{name: "engine error proves nothing", engineStatus: http.StatusInternalServerError, want: ""},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			engine := newStubEngine(t, testCase.engineStatus, "")
			result := engine.client().submit(stubSite("example.com"), []string{"https://example.com/a"})

			if got := result.keyStateAfter(); got != testCase.want {
				t.Fatalf("engine %d: key state = %q, want %q", testCase.engineStatus, got, testCase.want)
			}
		})
	}
}

func TestKeyStateAfterCheck(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want keyState
	}{
		{name: "file read and matched", err: nil, want: keyStateVerified},
		{
			name: "shutdown cancelled the check",
			err:  fmt.Errorf("%w: %w", errKeyFileUnreachable, context.Canceled),
			want: "",
		},
		{
			name: "check ran out of time",
			err:  fmt.Errorf("%w: %w", errKeyFileUnreachable, context.DeadlineExceeded),
			want: keyStatePending,
		},
		{
			name: "origin did not answer",
			err:  fmt.Errorf("%w: %w", errKeyFileUnreachable, errors.New("connection refused")),
			want: keyStatePending,
		},
		{
			name: "file read and refused",
			err:  fmt.Errorf("%w: key file returned 404", errKeyFileRejected),
			want: keyStateFailed,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := keyStateAfterCheck(testCase.err); got != testCase.want {
				t.Fatalf("err %v: state = %q, want %q", testCase.err, got, testCase.want)
			}
		})
	}
}

func TestVerifyKeySeparatesRefusedFromUnreachable(t *testing.T) {
	// The distinction is what keeps a wrong key file apart from an origin that
	// is simply down, so it is pinned on the errors themselves.
	refused := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer refused.Close()

	site := stubSite("example.com")
	site.KeyLocation = refused.URL + "/test-key.txt"

	err := newIndexNowClient("").verifyKey(context.Background(), site)

	if !errors.Is(err, errKeyFileRejected) {
		t.Fatalf("err = %v, want it to wrap %v", err, errKeyFileRejected)
	}
	if errors.Is(err, errKeyFileUnreachable) {
		t.Fatalf("a 404 was reported as unreachable: %v", err)
	}

	unreachable := stubSite("example.com")
	unreachable.KeyLocation = "http://127.0.0.1:1/test-key.txt"

	err = newIndexNowClient("").verifyKey(context.Background(), unreachable)

	if !errors.Is(err, errKeyFileUnreachable) {
		t.Fatalf("err = %v, want it to wrap %v", err, errKeyFileUnreachable)
	}
	if errors.Is(err, errKeyFileRejected) {
		t.Fatalf("an unreachable origin was reported as a refused key file: %v", err)
	}
}

func TestThrottledIsNotTheEnginesRateLimit(t *testing.T) {
	// Both are 429 and both mean "back off", but they have different owners:
	// throttled is this instance protecting its own egress IP, rate_limited is
	// the engine already penalising it. Sharing one reason would hide which
	// limit the caller hit.
	if reasonThrottled == reasonRateLimited {
		t.Fatal("a local refusal reuses the engine's rate_limited reason")
	}
	if reasonOverloaded == reasonRateLimited || reasonOverloaded == reasonThrottled {
		t.Fatal("overloaded shares a reason with another refusal")
	}

	if got := httpStatusFor(submissionResult{Reason: reasonThrottled}); got != http.StatusTooManyRequests {
		t.Fatalf("throttled maps to %d, want 429", got)
	}
	if got := httpStatusFor(submissionResult{Reason: reasonOverloaded}); got != http.StatusServiceUnavailable {
		t.Fatalf("overloaded maps to %d, want 503", got)
	}
}
