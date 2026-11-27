package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// logBuffer is a buffer a server can log to while a test reads it.
//
// These tests start a server and then read what it wrote, and a reload that adds
// a host starts a background key-file check, which logs from its own goroutine.
// A strings.Builder shared that way is a data race the test would only lose
// sometimes - and CI now runs this package under -race, so "sometimes" is a
// flaky build rather than a rare surprise. The mutex is the whole reason this
// type exists.
type logBuffer struct {
	mu sync.Mutex
	sb strings.Builder
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.sb.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.sb.String()
}

func (b *logBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.sb.Reset()
}

// The reload tests are about the promise SIGHUP makes: the site list changes,
// and nothing else does. A reload is not a restart, so everything a restart
// would reset - the counters, the throttle, the in-flight window - has to still
// be there afterwards.

func reloadTestConfig(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), initConfigName)
	if err := os.WriteFile(path, []byte(body), initConfigMode); err != nil {
		t.Fatalf("write config: %v", err)
	}

	return path
}

// reloadTwoSiteConfig is a config with two sites, written for a reload. endpoint
// is the indexnow_endpoint the reloaded runtime will submit to, and it is a
// parameter rather than left out because a reload builds a fresh client from the
// config it reads: a fixture without one gets the default endpoint and every
// submission after the reload goes to the public engine instead of this test's
// own httptest server, which is both a network call and an assertion that
// depends on api.indexnow.org.
func reloadTwoSiteConfig(endpoint, secondHost, secondToken string) string {
	return fmt.Sprintf(`listen: "127.0.0.1:0"

indexnow_endpoint: %q

sites:
  - host: example.com
    indexnow_key: %s
    token: %s
  - host: %s
    indexnow_key: %s
    token: %s
`, endpoint, testFixtureKey(), testToken, secondHost, "k"+strings.Repeat("8", 15), secondToken)
}

// reloadOneSiteConfig is the one-site config a reload test removes a host from.
// It carries its own indexnow_endpoint for the same reason reloadTwoSiteConfig
// takes one: a reloaded runtime that is pointed at nothing in particular reaches
// the public engine.
func reloadOneSiteConfig(endpoint string) string {
	return fmt.Sprintf(`listen: "127.0.0.1:0"

indexnow_endpoint: %q

sites:
  - host: example.com
    indexnow_key: %s
    token: %s
`, endpoint, testFixtureKey(), testToken)
}

// reloadLimitsConfig is a one-site config with an explicit limits block, so a
// reload can change the submission size and nothing else.
func reloadLimitsConfig(endpoint string, maxURLs, maxBodyBytes int) string {
	return fmt.Sprintf(`listen: "127.0.0.1:0"

indexnow_endpoint: %q

limits:
  max_urls_per_request: %d
  max_body_bytes: %d

sites:
  - host: example.com
    indexnow_key: %s
    token: %s
`, endpoint, maxURLs, maxBodyBytes, testFixtureKey(), testToken)
}

func TestReloadAddsASiteWithoutRestarting(t *testing.T) {
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)
	path := reloadTestConfig(t, reloadTwoSiteConfig(engine.server.URL, "shop.example.org", "second-token-0123456789"))

	// Before the reload the new host is unknown, which is the state a caller
	// sees until the signal arrives.
	if _, body := post(t, srv, `{"urls":["https://shop.example.org/a"]}`, "Bearer second-token-0123456789"); body["status"] != float64(http.StatusUnauthorized) {
		t.Fatalf("before the reload: status = %v, want 401", body["status"])
	}

	if err := srv.reload(context.Background(), path); err != nil {
		t.Fatalf("reload: %v", err)
	}

	if got := getHealth(t, srv); got.Sites != 2 {
		t.Fatalf("sites after reload = %d, want 2", got.Sites)
	}

	recorder, _ := post(t, srv, `{"urls":["https://shop.example.org/a"]}`, "Bearer second-token-0123456789")
	if recorder.Code != http.StatusOK {
		t.Fatalf("after the reload: status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
}

func TestReloadKeepsTheMetricsItAlreadyCounted(t *testing.T) {
	// A reload must not look like a fresh process to /metrics: the counters are
	// the only record of what this instance did, and a restart is exactly what
	// SIGHUP exists to avoid.
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	if recorder, _ := post(t, srv, `{"urls":["https://example.com/a"]}`, "Bearer "+testToken); recorder.Code != http.StatusOK {
		t.Fatalf("seed submission: status = %d, want 200", recorder.Code)
	}
	before := metricValue(t, metricsText(t, srv, "Bearer "+testToken), `crawlping_submissions_total{host="example.com"}`)
	if before != 1 {
		t.Fatalf("accepted counter before reload = %v, want 1", before)
	}

	path := reloadTestConfig(t, reloadTwoSiteConfig(engine.server.URL, "shop.example.org", "second-token-0123456789"))
	if err := srv.reload(context.Background(), path); err != nil {
		t.Fatalf("reload: %v", err)
	}

	after := metricValue(t, metricsText(t, srv, "Bearer "+testToken), `crawlping_submissions_total{host="example.com"}`)
	if after != before {
		t.Fatalf("accepted counter after reload = %v, want it to stay %v", after, before)
	}
}

func TestReloadKeepsTheThrottleItAlreadySpent(t *testing.T) {
	// The burst is per host and it is spent, not granted. A reload that rebuilt
	// the bucket would hand a looping caller a second burst for free - the exact
	// spend SIGHUP must not authorise.
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	for i := 0; i < rateBurstTokens; i++ {
		recorder, body := post(t, srv, `{"urls":["https://example.com/a"]}`, "Bearer "+testToken)
		if recorder.Code != http.StatusOK {
			t.Fatalf("submission %d: status = %d (%v)", i, recorder.Code, body)
		}
	}

	path := reloadTestConfig(t, reloadOneSiteConfig(engine.server.URL))
	if err := srv.reload(context.Background(), path); err != nil {
		t.Fatalf("reload: %v", err)
	}

	recorder, body := post(t, srv, `{"urls":["https://example.com/b"]}`, "Bearer "+testToken)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("after a reload that spent nothing, status = %d, want 429 (%v)", recorder.Code, body)
	}
}

func TestReloadRejectsAConfigThatDoesNotLoadAndKeepsServing(t *testing.T) {
	// The promise that makes SIGHUP safe to attach to a typo: a file that does
	// not load changes nothing, and the running site list keeps answering.
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)
	path := reloadTestConfig(t, "listen: [this is not a mapping\nsites: []\n")

	err := srv.reload(context.Background(), path)
	if err == nil {
		t.Fatal("reload accepted a config that does not load")
	}

	if got := getHealth(t, srv); got.Sites != 1 {
		t.Fatalf("sites after a failed reload = %d, want the running 1", got.Sites)
	}
	if recorder, _ := post(t, srv, `{"urls":["https://example.com/a"]}`, "Bearer "+testToken); recorder.Code != http.StatusOK {
		t.Fatalf("the running config stopped answering: status = %d", recorder.Code)
	}
}

func TestReloadDropsAHostThatWasRemoved(t *testing.T) {
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	added := reloadTestConfig(t, reloadTwoSiteConfig(engine.server.URL, "shop.example.org", "second-token-0123456789"))
	if err := srv.reload(context.Background(), added); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if recorder, _ := post(t, srv, `{"urls":["https://shop.example.org/a"]}`, "Bearer second-token-0123456789"); recorder.Code != http.StatusOK {
		t.Fatalf("the added host does not answer: status = %d", recorder.Code)
	}

	removed := filepath.Join(t.TempDir(), initConfigName)
	if err := os.WriteFile(removed, []byte(reloadOneSiteConfig(engine.server.URL)), initConfigMode); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := srv.reload(context.Background(), removed); err != nil {
		t.Fatalf("reload: %v", err)
	}

	// A token that is no longer configured must stop working, or removing a site
	// would only hide it.
	if _, body := post(t, srv, `{"urls":["https://shop.example.org/a"]}`, "Bearer second-token-0123456789"); body["status"] != float64(http.StatusUnauthorized) {
		t.Fatalf("a removed host's token still works: %v", body)
	}
	if got := getHealth(t, srv); got.Sites != 1 {
		t.Fatalf("sites after removal = %d, want 1", got.Sites)
	}
}

func TestReloadReusesOneClientForTheWholeRuntime(t *testing.T) {
	// The site list and the client have to come from the same runtime, or a
	// request in flight during a reload could submit through a client that
	// belongs to a different config.
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)
	path := reloadTestConfig(t, reloadTwoSiteConfig(engine.server.URL, "shop.example.org", "second-token-0123456789"))

	if err := srv.reload(context.Background(), path); err != nil {
		t.Fatalf("reload: %v", err)
	}

	rt := testRuntime(srv)
	if rt.client == nil {
		t.Fatal("the reloaded runtime has no client")
	}
	if len(rt.sites) != len(rt.cfg.Sites) {
		t.Fatalf("runtime sites = %d, config sites = %d", len(rt.sites), len(rt.cfg.Sites))
	}
}

func TestReloadLogsWhatChanged(t *testing.T) {
	engine := newFakeEngine(t, http.StatusOK)
	var logs logBuffer
	srv := newServer(&Config{
		Listen: "127.0.0.1:0",
		Sites: []Site{{
			Host:        "example.com",
			IndexNowKey: testFixtureKey(),
			KeyLocation: "https://example.com/" + testFixtureKey() + ".txt",
			Token:       testToken,
		}},
	}, newIndexNowClient(engine.server.URL), slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))

	path := reloadTestConfig(t, reloadTwoSiteConfig(engine.server.URL, "shop.example.org", "second-token-0123456789"))
	if err := srv.reload(context.Background(), path); err != nil {
		t.Fatalf("reload: %v", err)
	}

	// The line has to say how many sites are now served, so an operator reading
	// container logs can tell a reload apart from a restart.
	if !strings.Contains(logs.String(), "config reloaded") {
		t.Fatalf("no reload line in the log:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), `"sites":2`) {
		t.Fatalf("the reload line does not carry the new site count:\n%s", logs.String())
	}
}

func TestReloadChecksOnlyTheNewHostsKeyFile(t *testing.T) {
	// A reload must not overwrite a verdict with a weaker one, and it must not
	// re-fetch every file on every signal either. Only the added host is checked.
	key := testFixtureKey()
	origin := doctorKeyServer(t, key, func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	})
	defer origin.Close()

	engine := newFakeEngine(t, http.StatusOK)
	cfg := &Config{
		Listen: "127.0.0.1:0",
		Sites: []Site{{
			Host:        "example.com",
			IndexNowKey: key,
			KeyLocation: origin.URL + "/" + key + ".txt",
			Token:       testToken,
		}},
	}
	srv := newServer(cfg, newIndexNowClient(engine.server.URL), slog.New(slog.NewTextHandler(io.Discard, nil)))

	// A submission proves the key for example.com, which is the strongest
	// evidence this instance can hold: the engine's own verdict.
	if recorder, _ := post(t, srv, `{"urls":["https://example.com/a"]}`, "Bearer "+testToken); recorder.Code != http.StatusOK {
		t.Fatalf("seed submission: status = %d", recorder.Code)
	}
	if got := getHealthDetail(t, srv).Keys["example.com"].State; got != keyStateVerified {
		t.Fatalf("seed state = %q, want %q", got, keyStateVerified)
	}

	// The added host's key file is not served, so its check ends in "failed".
	// If the reload also re-checked example.com, whose file is not served
	// either, that engine verdict would be replaced by a weaker one.
	secondKey := "k" + strings.Repeat("8", 15)
	next := &Config{
		Listen:           "127.0.0.1:0",
		IndexNowEndpoint: engine.server.URL,
		Sites: []Site{
			{
				Host:        "example.com",
				IndexNowKey: key,
				KeyLocation: origin.URL + "/" + key + ".txt",
				Token:       testToken,
			},
			{
				Host:        "shop.example.org",
				IndexNowKey: secondKey,
				KeyLocation: origin.URL + "/nonexistent/" + secondKey + ".txt",
				Token:       "second-token-0123456789",
			},
		},
	}

	if err := srv.applyConfig(context.Background(), next); err != nil {
		t.Fatalf("applyConfig: %v", err)
	}

	// Wait for the background check of the added host to land, then confirm the
	// existing verdict survived it.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if state := getHealthDetail(t, srv).Keys["shop.example.org"].State; state != keyStatePending {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got := getHealthDetail(t, srv).Keys["example.com"].State; got != keyStateVerified {
		t.Fatalf("the reload downgraded example.com from %q to %q", keyStateVerified, got)
	}
	if got := getHealthDetail(t, srv).Keys["shop.example.org"].State; got == keyStatePending {
		t.Fatalf("the added host was never checked; state = %q", got)
	}
}

func TestReloadWarnsWhenTheFileDidNotChange(t *testing.T) {
	// The failure this catches is silent otherwise. A container whose config is
	// a single-file bind mount keeps the old inode after the host replaces the
	// file, so `site add` + SIGHUP reloads the same bytes and adds nothing. The
	// signal was delivered, the config loaded, the log says "reloaded" - and the
	// new site is not there. Nothing can be fixed from inside that inode, so the
	// line has to say what to change.
	engine := newFakeEngine(t, http.StatusOK)
	var logs logBuffer
	srv := newServer(&Config{
		Listen: "127.0.0.1:0",
		Sites: []Site{{
			Host:        "example.com",
			IndexNowKey: testFixtureKey(),
			KeyLocation: "https://example.com/" + testFixtureKey() + ".txt",
			Token:       testToken,
		}},
	}, newIndexNowClient(engine.server.URL), slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))

	path := reloadTestConfig(t, reloadOneSiteConfig(engine.server.URL))
	if err := srv.reload(context.Background(), path); err != nil {
		t.Fatalf("first reload: %v", err)
	}

	logs.Reset()
	// The same file, unchanged: what a pinned inode looks like from here.
	if err := srv.reload(context.Background(), path); err != nil {
		t.Fatalf("second reload: %v", err)
	}

	if !strings.Contains(logs.String(), "pinned inode") {
		t.Fatalf("a reload of unchanged bytes said nothing about the mount:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "WARN") {
		t.Fatalf("the pinned-inode line is not a warning:\n%s", logs.String())
	}
}

func TestReloadDoesNotWarnWhenTheFileReallyChanged(t *testing.T) {
	engine := newFakeEngine(t, http.StatusOK)
	var logs logBuffer
	srv := newServer(&Config{
		Listen: "127.0.0.1:0",
		Sites: []Site{{
			Host:        "example.com",
			IndexNowKey: testFixtureKey(),
			KeyLocation: "https://example.com/" + testFixtureKey() + ".txt",
			Token:       testToken,
		}},
	}, newIndexNowClient(engine.server.URL), slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))

	path := reloadTestConfig(t, reloadOneSiteConfig(engine.server.URL))
	if err := srv.reload(context.Background(), path); err != nil {
		t.Fatalf("first reload: %v", err)
	}

	logs.Reset()
	if err := os.WriteFile(path, []byte(reloadTwoSiteConfig(engine.server.URL, "shop.example.org", "second-token-0123456789")), initConfigMode); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := srv.reload(context.Background(), path); err != nil {
		t.Fatalf("second reload: %v", err)
	}

	if strings.Contains(logs.String(), "pinned inode") {
		t.Fatalf("a reload that changed the config still blamed the mount:\n%s", logs.String())
	}
	if got := getHealth(t, srv); got.Sites != 2 {
		t.Fatalf("sites after the reload = %d, want 2", got.Sites)
	}
}

func TestServerRemembersTheBytesItStartedFrom(t *testing.T) {
	// The pinned-inode warning depends on this: a server that forgets the bytes
	// it was started with can only notice a stale file after one successful
	// reload, which is exactly one reload too late.
	engine := newFakeEngine(t, http.StatusOK)
	raw := []byte(reloadOneSiteConfig(engine.server.URL))

	cfg, err := parseConfig(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	srv := newServerWithSource(cfg, newIndexNowClient(engine.server.URL),
		slog.New(slog.NewTextHandler(io.Discard, nil)), raw)

	if got := testRuntime(srv).source; string(got) != string(raw) {
		t.Fatalf("the runtime forgot the bytes it was built from")
	}

	// A server built without a file has nothing to compare, so it must not claim
	// a reload found stale bytes.
	plain := newServer(cfg, newIndexNowClient(engine.server.URL), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if got := testRuntime(plain).source; got != nil {
		t.Fatalf("a hand-built server claims a source of %q", got)
	}
}

func TestReloadGrowsTheResubmissionWindowToAFullBatch(t *testing.T) {
	// The window has to remember one full batch, or a raised
	// max_urls_per_request sends URLs it then forgets and every one of them is
	// resubmitted immediately. That has to hold for the config a reload installs,
	// not only for the one the process started on.
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	if got := windowCap(srv.window); got != maxTrackedURLs {
		t.Fatalf("window cap at startup = %d, want %d", got, maxTrackedURLs)
	}

	// 2000 fits under the default cap, so the window already remembers a full
	// batch of this size and the number does not have to move for the property to
	// hold. What matters here is that the reload does not leave a window smaller
	// than the batch it now accepts.
	if err := srv.reload(context.Background(), reloadTestConfig(t, reloadLimitsConfig(engine.server.URL, 2000, 512000))); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := windowCap(srv.window); got < 2000 {
		t.Fatalf("window cap after a 2000-url limit = %d, which cannot remember one full batch", got)
	}

	// Past the default cap, growth is what makes the difference: 5000 URLs do not
	// fit in 4096 entries.
	if err := srv.reload(context.Background(), reloadTestConfig(t, reloadLimitsConfig(engine.server.URL, 5000, 128000))); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := windowCap(srv.window); got != 5000 {
		t.Fatalf("window cap after a 5000-url limit = %d, want 5000", got)
	}

	// And a reload that lowers the count does not shrink it again: the cap is a
	// memory bound, and evicting entries a caller was already promised would
	// resubmit URLs nobody changed.
	if err := srv.reload(context.Background(), reloadTestConfig(t, reloadLimitsConfig(engine.server.URL, 2000, 512000))); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := windowCap(srv.window); got != 5000 {
		t.Fatalf("window cap after the count went back down = %d, want it kept at 5000", got)
	}
}

func TestReloadKeepsThePerSiteShareItStartedWith(t *testing.T) {
	// The per-site share is the width of a semaphore channel that may already have
	// slots held, so a reload cannot resize it. A single-site deployment that adds
	// a second site therefore keeps all 8 slots per host, where a restart would
	// give 4 - and it keeps serving, which is what SIGHUP is for.
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)

	if got := srv.inflight.perHost; got != maxInFlightEngineCalls {
		t.Fatalf("per-host cap before the reload = %d, want %d", got, maxInFlightEngineCalls)
	}

	path := reloadTestConfig(t, reloadTwoSiteConfig(engine.server.URL, otherHost, otherToken))
	if err := srv.reload(context.Background(), path); err != nil {
		t.Fatalf("reload: %v", err)
	}

	if got := getHealth(t, srv).Sites; got != 2 {
		t.Fatalf("sites after the reload = %d, want 2", got)
	}
	if got := srv.inflight.perHost; got != maxInFlightEngineCalls {
		t.Fatalf("per-host cap after the reload = %d, want the startup value %d kept", got, maxInFlightEngineCalls)
	}
	if got := cap(srv.inflight.hostSlots(otherHost)); got != maxInFlightEngineCalls {
		t.Fatalf("the added host's semaphore is %d wide, want %d", got, maxInFlightEngineCalls)
	}

	// And the added host is served, through this test's own engine: the fixture
	// carries the endpoint a reloaded runtime builds its client from.
	recorder, body := postURL(t, srv, "Bearer "+otherToken, "https://"+otherHost+"/a")
	if recorder.Code != http.StatusOK {
		t.Fatalf("the added host: status = %d, want 200 (%s)", recorder.Code, recorder.Body.String())
	}
	if body["host"] != otherHost {
		t.Fatalf("the added host: host = %v, want %q", body["host"], otherHost)
	}
	if got := engine.calls(); got != 1 {
		t.Fatalf("engine calls = %d, want 1", got)
	}
}

func TestReloadWarnsWhenThePerSiteShareWouldChange(t *testing.T) {
	// The other direction, and the one a log line can be checked on without a
	// background key check racing the assertion: a two-site deployment that drops
	// to one site keeps 4 slots per host while a restart would give 8. The running
	// number is kept, and the line has to say both numbers, because otherwise an
	// operator reads the documented split and believes it is in force.
	engine := newFakeEngine(t, http.StatusOK)
	var logs logBuffer
	srv := newServer(&Config{
		Listen:           "127.0.0.1:0",
		IndexNowEndpoint: engine.server.URL,
		Sites:            reloadTwoSites(),
	}, newIndexNowClient(engine.server.URL), slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))

	path := reloadTestConfig(t, reloadOneSiteConfig(engine.server.URL))
	if err := srv.reload(context.Background(), path); err != nil {
		t.Fatalf("reload: %v", err)
	}

	if got := srv.inflight.perHost; got != maxInFlightEngineCalls/2 {
		t.Fatalf("per-host cap after dropping a site = %d, want the startup %d kept", got, maxInFlightEngineCalls/2)
	}

	line := logs.String()
	if !strings.Contains(line, "per-site engine share") {
		t.Fatalf("the reload said nothing about the share it kept:\n%s", line)
	}
	if !strings.Contains(line, "WARN") {
		t.Fatalf("the kept share is not a warning:\n%s", line)
	}
	for _, want := range []string{`"running":4`, `"after_restart":8`} {
		if !strings.Contains(line, want) {
			t.Fatalf("the line does not carry %s:\n%s", want, line)
		}
	}
}

// reloadTwoSites is the site list reloadTwoSiteConfig writes, as values, for a
// test that starts a server from it rather than reloading into it.
func reloadTwoSites() []Site {
	return []Site{
		{
			Host:        "example.com",
			IndexNowKey: testFixtureKey(),
			KeyLocation: "https://example.com/" + testFixtureKey() + ".txt",
			Token:       testToken,
		},
		{
			Host:        otherHost,
			IndexNowKey: "k" + strings.Repeat("8", 15),
			KeyLocation: "https://" + otherHost + "/k" + strings.Repeat("8", 15) + ".txt",
			Token:       otherToken,
		},
	}
}

func TestReloadForgetsARemovedHostsKeyState(t *testing.T) {
	// A key verdict is a fact about a host this instance serves. Keeping it after
	// the host is removed is what let a re-added host inherit an old "verified"
	// with an old checked_at: only hosts the previous config did not carry are
	// checked, so a returning host has no evidence of its own and would fall back
	// on last time's.
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTwoSiteServer(t, engine)

	srv.setKeyState(otherHost, keyStateVerified, "")
	if got := keyStateFor(srv, otherHost); got != keyStateVerified {
		t.Fatalf("seed state = %q, want %q", got, keyStateVerified)
	}

	path := reloadTestConfig(t, reloadOneSiteConfig(engine.server.URL))
	if err := srv.reload(context.Background(), path); err != nil {
		t.Fatalf("reload: %v", err)
	}

	srv.mu.RLock()
	_, known := srv.keyStates[otherHost]
	srv.mu.RUnlock()

	if known {
		t.Fatalf("the removed host kept its key state")
	}

	// Re-adding the host must not bring the verdict back. The site's key file is
	// served by a local origin that answers 404, so the check that a returning
	// host now gets can only end in pending or failed, never in a stale verified.
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer origin.Close()

	const secondKey = "k8888888888888888"

	next := &Config{
		Listen:           "127.0.0.1:0",
		IndexNowEndpoint: engine.server.URL,
		Sites: []Site{
			{
				Host:        "example.com",
				IndexNowKey: testFixtureKey(),
				KeyLocation: "https://example.com/" + testFixtureKey() + ".txt",
				Token:       testToken,
			},
			{
				Host:        otherHost,
				IndexNowKey: secondKey,
				KeyLocation: origin.URL + "/" + secondKey + ".txt",
				Token:       otherToken,
			},
		},
	}

	if err := srv.applyConfig(context.Background(), next); err != nil {
		t.Fatalf("applyConfig: %v", err)
	}

	// No wait and no sleep: the assertion holds before the background check lands
	// (the record is absent, which is pending) and after it (the record is
	// failed), so waiting would only make the test slower.
	if got := keyStateFor(srv, otherHost); got == keyStateVerified {
		t.Fatalf("a re-added host inherited the old verdict: state = %q", got)
	}
}
