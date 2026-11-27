package main

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// callMetrics performs one GET /metrics, so a test controls the credentials the
// way a real scraper would.
func callMetrics(t *testing.T, srv *server, authorization string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}

	recorder := httptest.NewRecorder()
	srv.routes().ServeHTTP(recorder, req)

	return recorder
}

// metricsText reads the exposition, failing if the endpoint refused.
func metricsText(t *testing.T, srv *server, authorization string) string {
	t.Helper()

	recorder := callMetrics(t, srv, authorization)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}

	return recorder.Body.String()
}

// findMetric returns the value of the one series whose name and labels match
// exactly, so an assertion names the series it means instead of a substring a
// longer series also contains.
func findMetric(body, series string) (float64, bool) {
	for _, line := range strings.Split(body, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		name, raw, found := strings.Cut(line, " ")
		if !found || name != series {
			continue
		}

		value, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return 0, false
		}

		return value, true
	}

	return 0, false
}

func metricValue(t *testing.T, body, series string) float64 {
	t.Helper()

	value, found := findMetric(body, series)
	if !found {
		t.Fatalf("no series %q in:\n%s", series, body)
	}

	return value
}

func TestMetricsContentTypeAndMethod(t *testing.T) {
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	recorder := callMetrics(t, srv, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want the text exposition format", got)
	}

	// Reading the counters is a GET: the mux advertises that, so a POST is
	// answered by the mux rather than by a handler that has to remember.
	req := httptest.NewRequest(http.MethodPost, "/metrics", nil)
	post := httptest.NewRecorder()
	srv.routes().ServeHTTP(post, req)

	if post.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /metrics = %d, want 405", post.Code)
	}
}

func TestMetricsCarryHelpAndTypeForEveryFamily(t *testing.T) {
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	body := metricsText(t, srv, "")

	families := map[string]string{
		"crawlping_submissions_total":        "counter",
		"crawlping_submission_reasons_total": "counter",
		"crawlping_engine_requests_total":    "counter",
		"crawlping_engine_duration_seconds":  "histogram",
		"crawlping_urls_submitted_total":     "counter",
		"crawlping_urls_skipped_total":       "counter",
		"crawlping_guard_refusals_total":     "counter",
		"crawlping_key_state":                "gauge",
	}

	for family, kind := range families {
		if !strings.Contains(body, "# HELP "+family+" ") {
			t.Fatalf("%s has no HELP line:\n%s", family, body)
		}
		if !strings.Contains(body, "# TYPE "+family+" "+kind+"\n") {
			t.Fatalf("%s is not declared as a %s:\n%s", family, kind, body)
		}
	}
}

func TestMetricsCountASuccessfulSubmission(t *testing.T) {
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	if recorder, _ := postURL(t, srv, "Bearer "+testToken, "https://example.com/a", "https://example.com/b"); recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}

	body := metricsText(t, srv, "Bearer "+testToken)

	if got := metricValue(t, body, `crawlping_submissions_total{host="example.com"}`); got != 1 {
		t.Fatalf("submissions = %v, want 1", got)
	}
	if got := metricValue(t, body, `crawlping_submission_reasons_total{host="example.com",reason="accepted"}`); got != 1 {
		t.Fatalf("accepted reasons = %v, want 1", got)
	}
	if got := metricValue(t, body, `crawlping_engine_requests_total{host="example.com",status="200"}`); got != 1 {
		t.Fatalf("engine status 200 = %v, want 1", got)
	}
	if got := metricValue(t, body, `crawlping_urls_submitted_total{host="example.com"}`); got != 2 {
		t.Fatalf("urls submitted = %v, want 2", got)
	}
	if got := metricValue(t, body, `crawlping_urls_skipped_total{host="example.com"}`); got != 0 {
		t.Fatalf("urls skipped = %v, want 0", got)
	}
	if got := metricValue(t, body, `crawlping_engine_duration_seconds_count{host="example.com"}`); got != 1 {
		t.Fatalf("duration samples = %v, want 1", got)
	}
}

func TestMetricsCountALocalRefusalWithoutAnEngineStatus(t *testing.T) {
	// A batch refused here never reaches the engine, so it has a reason and no
	// engine series at all: reporting a status the engine never sent would be
	// the metrics version of the wrong engine_status in the response.
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	if recorder, _ := postURL(t, srv, "Bearer "+testToken, "https://attacker.test/a"); recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (%s)", recorder.Code, recorder.Body.String())
	}

	body := metricsText(t, srv, "Bearer "+testToken)

	if got := metricValue(t, body, `crawlping_submissions_total{host="example.com"}`); got != 1 {
		t.Fatalf("submissions = %v, want 1", got)
	}
	if got := metricValue(t, body, `crawlping_submission_reasons_total{host="example.com",reason="host_mismatch"}`); got != 1 {
		t.Fatalf("host_mismatch reasons = %v, want 1", got)
	}
	if _, found := findMetric(body, `crawlping_engine_requests_total{host="example.com",status="422"}`); found {
		t.Fatalf("a locally refused batch reports an engine status:\n%s", body)
	}
	if strings.Contains(body, `crawlping_engine_requests_total{`) {
		t.Fatalf("a locally refused batch produced an engine series:\n%s", body)
	}
	if got := metricValue(t, body, `crawlping_engine_duration_seconds_count{host="example.com"}`); got != 0 {
		t.Fatalf("duration samples = %v, want 0 (no engine call)", got)
	}
	if got := metricValue(t, body, `crawlping_urls_submitted_total{host="example.com"}`); got != 0 {
		t.Fatalf("urls submitted = %v, want 0", got)
	}
	if got := engine.calls(); got != 0 {
		t.Fatalf("engine calls = %d, want 0", got)
	}
}

func TestMetricsCountAnOversizedBody(t *testing.T) {
	// A body refused before it could be parsed is still a submission this
	// instance handled, and its reason is one of the closed set.
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	body := `{"urls":["https://example.com/a"],"padding":"` + strings.Repeat("x", maxBodyBytes+1) + `"}`
	if recorder, _ := post(t, srv, body, "Bearer "+testToken); recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413 (%s)", recorder.Code, recorder.Body.String())
	}

	text := metricsText(t, srv, "Bearer "+testToken)

	if got := metricValue(t, text, `crawlping_submission_reasons_total{host="example.com",reason="payload_too_large"}`); got != 1 {
		t.Fatalf("payload_too_large reasons = %v, want 1", got)
	}
	if strings.Contains(text, `crawlping_engine_requests_total{`) {
		t.Fatalf("an oversized body produced an engine series:\n%s", text)
	}
}

func TestMetricsCountGuardRefusalsByKind(t *testing.T) {
	t.Run("throttled", func(t *testing.T) {
		engine := newFakeEngine(t, http.StatusOK)
		srv := newTestServer(t, engine)

		clock := newFrozenClock()
		srv.rate.now = clock.Now

		for i := 0; i < rateBurstTokens; i++ {
			if recorder, _ := postURL(t, srv, "Bearer "+testToken, "https://example.com/page-"+strconv.Itoa(i)); recorder.Code != http.StatusOK {
				t.Fatalf("request %d: status = %d, want 200", i+1, recorder.Code)
			}
		}

		if recorder, _ := postURL(t, srv, "Bearer "+testToken, "https://example.com/last"); recorder.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429", recorder.Code)
		}

		body := metricsText(t, srv, "Bearer "+testToken)

		if got := metricValue(t, body, `crawlping_guard_refusals_total{host="example.com",kind="throttled"}`); got != 1 {
			t.Fatalf("throttled guard refusals = %v, want 1", got)
		}
		// The guard answered the request, so it is one of the requests this
		// handler handled and the reason family says which kind of answer it
		// got. A refusal that showed up only under guard_refusals_total would
		// leave submissions and reasons as two numbers that do not add up.
		if got := metricValue(t, body, `crawlping_submissions_total{host="example.com"}`); got != rateBurstTokens+1 {
			t.Fatalf("submissions = %v, want the %d that were sent plus the one throttled", got, rateBurstTokens)
		}
		if got := metricValue(t, body, `crawlping_submission_reasons_total{host="example.com",reason="throttled"}`); got != 1 {
			t.Fatalf("throttled reasons = %v, want 1", got)
		}
	})

	t.Run("overloaded", func(t *testing.T) {
		engine := newFakeEngine(t, http.StatusOK)
		srv := newTestServer(t, engine)

		srv.inflight = newInflightGuard(1, 1)
		srv.inflight.wait = 20 * time.Millisecond

		// Hold the only instance slot, as an engine call still in flight would.
		srv.inflight.global <- struct{}{}
		defer func() { <-srv.inflight.global }()

		if recorder, _ := postURL(t, srv, "Bearer "+testToken, "https://example.com/a"); recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", recorder.Code)
		}

		body := metricsText(t, srv, "Bearer "+testToken)

		if got := metricValue(t, body, `crawlping_guard_refusals_total{host="example.com",kind="overloaded"}`); got != 1 {
			t.Fatalf("overloaded guard refusals = %v, want 1", got)
		}
		// The one request this host made never reached the engine, so it is one
		// handled submission and one reason, and the histogram still has no
		// sample: the guard's answer is not an engine latency.
		if got := metricValue(t, body, `crawlping_submissions_total{host="example.com"}`); got != 1 {
			t.Fatalf("submissions = %v, want 1 (the request the guard answered)", got)
		}
		if got := metricValue(t, body, `crawlping_submission_reasons_total{host="example.com",reason="overloaded"}`); got != 1 {
			t.Fatalf("overloaded reasons = %v, want 1", got)
		}
		if got := metricValue(t, body, `crawlping_engine_duration_seconds_count{host="example.com"}`); got != 0 {
			t.Fatalf("duration samples = %v, want 0 (no engine call)", got)
		}
	})
}

func TestMetricsCountAGuardRefusalAsASubmission(t *testing.T) {
	// One refusal, counted directly: the guard's two kinds are reasons a request
	// got, so the request is one submission, one reason and one kind. It is not an
	// engine request and not a latency sample, because nothing was sent.
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	srv.metrics.recordGuardRefusal("example.com", "throttled")

	body := metricsText(t, srv, "Bearer "+testToken)

	if got := metricValue(t, body, `crawlping_submissions_total{host="example.com"}`); got != 1 {
		t.Fatalf("submissions = %v, want 1", got)
	}
	if got := metricValue(t, body, `crawlping_submission_reasons_total{host="example.com",reason="throttled"}`); got != 1 {
		t.Fatalf("throttled reasons = %v, want 1", got)
	}
	if got := metricValue(t, body, `crawlping_guard_refusals_total{host="example.com",kind="throttled"}`); got != 1 {
		t.Fatalf("throttled guard refusals = %v, want 1", got)
	}
	if strings.Contains(body, `crawlping_engine_requests_total{`) {
		t.Fatalf("a guard refusal produced an engine series:\n%s", body)
	}
	if got := metricValue(t, body, `crawlping_engine_duration_seconds_count{host="example.com"}`); got != 0 {
		t.Fatalf("duration samples = %v, want 0 (no engine call)", got)
	}
	if got := engine.calls(); got != 0 {
		t.Fatalf("engine calls = %d, want 0", got)
	}
}

func TestMetricsDoNotDoubleCountAGuardRefusal(t *testing.T) {
	// One accepted submission and one guard refusal for the same host are two
	// requests this handler handled, so submissions is their sum and reasons is
	// the same two under two labels. A guard refusal added to a separate total
	// would make one of these requests invisible to the submissions family.
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	if recorder, _ := postURL(t, srv, "Bearer "+testToken, "https://example.com/a"); recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}

	srv.metrics.recordGuardRefusal("example.com", "throttled")

	body := metricsText(t, srv, "Bearer "+testToken)

	if got := metricValue(t, body, `crawlping_submissions_total{host="example.com"}`); got != 2 {
		t.Fatalf("submissions = %v, want 2 (one accepted, one throttled)", got)
	}
	if got := metricValue(t, body, `crawlping_submission_reasons_total{host="example.com",reason="accepted"}`); got != 1 {
		t.Fatalf("accepted reasons = %v, want 1", got)
	}
	if got := metricValue(t, body, `crawlping_submission_reasons_total{host="example.com",reason="throttled"}`); got != 1 {
		t.Fatalf("throttled reasons = %v, want 1", got)
	}
	if got := metricValue(t, body, `crawlping_guard_refusals_total{host="example.com",kind="throttled"}`); got != 1 {
		t.Fatalf("throttled guard refusals = %v, want 1", got)
	}
	// The refusal was the only request that never reached the engine.
	if got := metricValue(t, body, `crawlping_engine_duration_seconds_count{host="example.com"}`); got != 1 {
		t.Fatalf("duration samples = %v, want 1", got)
	}
}

func TestMetricsObserveOneDurationSamplePerEngineCall(t *testing.T) {
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	// One engine call: one sample.
	if recorder, _ := postURL(t, srv, "Bearer "+testToken, "https://example.com/a"); recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}

	body := metricsText(t, srv, "Bearer "+testToken)
	if got := metricValue(t, body, `crawlping_engine_duration_seconds_count{host="example.com"}`); got != 1 {
		t.Fatalf("duration samples = %v, want 1", got)
	}

	// A local refusal is not an engine call, so it adds no sample.
	if recorder, _ := postURL(t, srv, "Bearer "+testToken, "https://attacker.test/a"); recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("local refusal: status = %d, want 422", recorder.Code)
	}

	body = metricsText(t, srv, "Bearer "+testToken)
	if got := metricValue(t, body, `crawlping_engine_duration_seconds_count{host="example.com"}`); got != 1 {
		t.Fatalf("duration samples after a local refusal = %v, want 1", got)
	}

	// An engine that answers with an error is still an engine call.
	engine.setStatus(http.StatusInternalServerError)

	if recorder, _ := postURL(t, srv, "Bearer "+testToken, "https://example.com/b"); recorder.Code != http.StatusBadGateway {
		t.Fatalf("engine error: status = %d, want 502", recorder.Code)
	}

	body = metricsText(t, srv, "Bearer "+testToken)
	if got := metricValue(t, body, `crawlping_engine_duration_seconds_count{host="example.com"}`); got != 2 {
		t.Fatalf("duration samples after an engine error = %v, want 2", got)
	}

	// And so is a call that never landed: that is the latency worth seeing most
	// of all, so it must not be the one case the histogram skips.
	setTestClient(srv, newIndexNowClient("http://127.0.0.1:1/indexnow"))

	if recorder, _ := postURL(t, srv, "Bearer "+testToken, "https://example.com/c"); recorder.Code != http.StatusBadGateway {
		t.Fatalf("unreachable engine: status = %d, want 502 (%s)", recorder.Code, recorder.Body.String())
	}

	body = metricsText(t, srv, "Bearer "+testToken)

	if got := metricValue(t, body, `crawlping_engine_duration_seconds_count{host="example.com"}`); got != 3 {
		t.Fatalf("duration samples after a call that never landed = %v, want 3", got)
	}
	// The +Inf bucket is the count, which is the histogram's own consistency
	// check: every sample landed in exactly one bucket.
	if got := metricValue(t, body, `crawlping_engine_duration_seconds_bucket{host="example.com",le="+Inf"}`); got != 3 {
		t.Fatalf("+Inf bucket = %v, want 3", got)
	}
	// A call that never landed leaves no engine status, which is the same rule
	// the response follows.
	if _, found := findMetric(body, `crawlping_engine_requests_total{host="example.com",status="502"}`); found {
		t.Fatalf("a call that never landed reports an engine status:\n%s", body)
	}
	if got := metricValue(t, body, `crawlping_engine_requests_total{host="example.com",status="500"}`); got != 1 {
		t.Fatalf("engine status 500 = %v, want 1", got)
	}
}

func TestMetricsCountURLsTheWindowSkipped(t *testing.T) {
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	clock := newFrozenClock()
	srv.window.now = clock.Now

	postURL(t, srv, "Bearer "+testToken, "https://example.com/a")
	clock.advance(time.Minute)

	recorder, body := postURL(t, srv, "Bearer "+testToken, "https://example.com/a")
	if recorder.Code != http.StatusOK || body["skipped"] != float64(1) {
		t.Fatalf("repeat: status = %d, skipped = %v, want a suppressed 200", recorder.Code, body["skipped"])
	}

	text := metricsText(t, srv, "Bearer "+testToken)

	if got := metricValue(t, text, `crawlping_urls_skipped_total{host="example.com"}`); got != 1 {
		t.Fatalf("urls skipped = %v, want 1", got)
	}
	if got := metricValue(t, text, `crawlping_urls_submitted_total{host="example.com"}`); got != 1 {
		t.Fatalf("urls submitted = %v, want 1", got)
	}
	// A batch the window emptied is not a refusal: it is an accepted submission
	// with nothing to send, and the engine is not called again.
	if got := metricValue(t, text, `crawlping_submissions_total{host="example.com"}`); got != 2 {
		t.Fatalf("submissions = %v, want 2", got)
	}
	if got := metricValue(t, text, `crawlping_submission_reasons_total{host="example.com",reason="accepted"}`); got != 2 {
		t.Fatalf("accepted reasons = %v, want 2", got)
	}
	if got := metricValue(t, text, `crawlping_engine_requests_total{host="example.com",status="200"}`); got != 1 {
		t.Fatalf("engine status 200 = %v, want 1 (the second batch had nothing to send)", got)
	}
}

func TestMetricsWithoutTokenOmitHostNames(t *testing.T) {
	// /metrics has to answer unauthenticated - that is what a scrape usually is -
	// and a Cloudflare Tunnel makes it publicly reachable, so it must not be a
	// way to enumerate the operator's domains. The counts survive; the host
	// label does not.
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	if recorder, _ := postURL(t, srv, "Bearer "+testToken, "https://example.com/a"); recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}

	for _, authorization := range []string{"", "Bearer wrong", "Basic " + testToken} {
		body := metricsText(t, srv, authorization)

		if strings.Contains(body, `host="`) {
			t.Fatalf("%q: the exposition carries a host label:\n%s", authorization, body)
		}
		if strings.Contains(body, "example.com") {
			t.Fatalf("%q: the exposition names a host:\n%s", authorization, body)
		}

		// Aggregated, not dropped: one series per remaining label.
		if got := metricValue(t, body, "crawlping_submissions_total"); got != 1 {
			t.Fatalf("%q: submissions = %v, want 1", authorization, got)
		}
		if got := metricValue(t, body, `crawlping_submission_reasons_total{reason="accepted"}`); got != 1 {
			t.Fatalf("%q: accepted reasons = %v, want 1", authorization, got)
		}
		if got := metricValue(t, body, `crawlping_engine_requests_total{status="200"}`); got != 1 {
			t.Fatalf("%q: engine status 200 = %v, want 1", authorization, got)
		}
		if got := metricValue(t, body, `crawlping_urls_submitted_total`); got != 1 {
			t.Fatalf("%q: urls submitted = %v, want 1", authorization, got)
		}
		if got := metricValue(t, body, `crawlping_engine_duration_seconds_count`); got != 1 {
			t.Fatalf("%q: duration samples = %v, want 1", authorization, got)
		}
		// The gauge is the count per state here, the same shape /health reports
		// unauthenticated: the accepted submission verified this host's key.
		if got := metricValue(t, body, `crawlping_key_state{state="verified"}`); got != 1 {
			t.Fatalf("%q: verified key state = %v, want 1", authorization, got)
		}
		if got := metricValue(t, body, `crawlping_key_state{state="pending"}`); got != 0 {
			t.Fatalf("%q: pending key state = %v, want 0", authorization, got)
		}
	}
}

func TestMetricsWithTokenIncludeHostLabels(t *testing.T) {
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTwoSiteServer(t, engine)

	if recorder, _ := postURL(t, srv, "Bearer "+testToken, "https://example.com/a"); recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}

	body := metricsText(t, srv, "Bearer "+testToken)

	if got := metricValue(t, body, `crawlping_submissions_total{host="example.com"}`); got != 1 {
		t.Fatalf("submissions for example.com = %v, want 1", got)
	}
	if got := metricValue(t, body, `crawlping_submission_reasons_total{host="example.com",reason="accepted"}`); got != 1 {
		t.Fatalf("accepted reasons for example.com = %v, want 1", got)
	}
	if got := metricValue(t, body, `crawlping_engine_requests_total{host="example.com",status="200"}`); got != 1 {
		t.Fatalf("engine status 200 for example.com = %v, want 1", got)
	}
	if got := metricValue(t, body, `crawlping_engine_duration_seconds_bucket{host="example.com",le="+Inf"}`); got != 1 {
		t.Fatalf("+Inf bucket for example.com = %v, want 1", got)
	}

	// The gauge covers every configured site, including one that has not been
	// decided yet: pending is 0, verified 1, failed 2.
	if got := metricValue(t, body, `crawlping_key_state{host="example.com"}`); got != 1 {
		t.Fatalf("example.com key state = %v, want 1 (verified by the 200)", got)
	}
	if got := metricValue(t, body, `crawlping_key_state{host="`+otherHost+`"}`); got != 0 {
		t.Fatalf("%s key state = %v, want 0 (pending)", otherHost, got)
	}

	// The per-host view is complete: a host that has not submitted yet still has
	// no counters, and that is the only difference from the authenticated view
	// of a busy one.
	if strings.Contains(body, `crawlping_submissions_total{host="`+otherHost+`"}`) {
		t.Fatalf("a host that submitted nothing has a submissions series:\n%s", body)
	}
}

func TestMetricsAreSafeUnderConcurrentSubmits(t *testing.T) {
	// The counters are shared by every request, so the race detector is half of
	// this test; the other half is that nothing is lost or double counted while
	// a scrape reads the maps.
	const perSite = 5

	engine := newFakeEngine(t, http.StatusOK)
	srv := newTwoSiteServer(t, engine)

	sites := []struct{ token, host string }{
		{testToken, "example.com"},
		{otherToken, otherHost},
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []int
	)

	for _, site := range sites {
		for i := 0; i < perSite; i++ {
			wg.Add(1)

			go func(site struct{ token, host string }, i int) {
				defer wg.Done()

				// A distinct URL per request: the resubmission window is not what
				// this test measures, and a repeated URL would never reach the
				// engine.
				target := "https://" + site.host + "/page-" + strconv.Itoa(i)

				var recorder *httptest.ResponseRecorder
				if i%2 == 0 {
					recorder, _ = postURL(t, srv, "Bearer "+site.token, target)
				} else {
					recorder, _ = getSubmit(t, srv, submitQuery(target), "Bearer "+site.token)
				}

				mu.Lock()
				results = append(results, recorder.Code)
				mu.Unlock()
			}(site, i)
		}
	}

	// Scrape while the submits are in flight, so the snapshot is taken against
	// maps that are being written.
	for scrape := 0; scrape < 4; scrape++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			code := callMetrics(t, srv, "Bearer "+testToken).Code

			mu.Lock()
			results = append(results, code)
			mu.Unlock()
		}()
	}

	wg.Wait()

	for _, code := range results {
		if code != http.StatusOK {
			t.Fatalf("a concurrent request answered %d, want 200", code)
		}
	}

	body := metricsText(t, srv, "Bearer "+testToken)

	for _, site := range sites {
		if got := metricValue(t, body, `crawlping_submissions_total{host="`+site.host+`"}`); got != perSite {
			t.Fatalf("%s: submissions = %v, want %d", site.host, got, perSite)
		}
		if got := metricValue(t, body, `crawlping_submission_reasons_total{host="`+site.host+`",reason="accepted"}`); got != perSite {
			t.Fatalf("%s: accepted reasons = %v, want %d", site.host, got, perSite)
		}
		if got := metricValue(t, body, `crawlping_urls_submitted_total{host="`+site.host+`"}`); got != perSite {
			t.Fatalf("%s: urls submitted = %v, want %d", site.host, got, perSite)
		}
		if got := metricValue(t, body, `crawlping_engine_duration_seconds_count{host="`+site.host+`"}`); got != perSite {
			t.Fatalf("%s: duration samples = %v, want %d", site.host, got, perSite)
		}
	}
}

func TestMetricsKeysAreBoundedByTheConfig(t *testing.T) {
	// The label values are the config and this package's closed vocabulary, never
	// a value out of a request: a URL must not become a series. This sends URLs
	// that would be distinctive if any of them leaked into a label.
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	postURL(t, srv, "Bearer "+testToken,
		"https://example.com/a-path-that-must-not-be-a-label?token=secret#fragment",
		"https://example.com/another-one?session=abc123",
	)

	body := metricsText(t, srv, "Bearer "+testToken)

	for _, leaked := range []string{"a-path-that-must-not-be-a-label", "secret", "session", "fragment", "another-one"} {
		if strings.Contains(body, leaked) {
			t.Fatalf("%q leaked into the exposition:\n%s", leaked, body)
		}
	}

	// The only label names are the bounded ones.
	for _, line := range strings.Split(body, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		if _, rest, found := strings.Cut(line, "{"); found {
			labels, _, _ := strings.Cut(rest, "}")

			for _, pair := range strings.Split(labels, ",") {
				name, _, _ := strings.Cut(pair, "=")
				switch name {
				case "host", "reason", "status", "kind", "le", "state":
				default:
					t.Fatalf("series %q carries an unexpected label %q", line, name)
				}
			}
		}
	}
}
