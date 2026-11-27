package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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

func reloadTwoSiteConfig(secondHost, secondToken string) string {
	return fmt.Sprintf(`listen: "127.0.0.1:0"

sites:
  - host: example.com
    indexnow_key: %s
    token: %s
  - host: %s
    indexnow_key: %s
    token: %s
`, testFixtureKey(), testToken, secondHost, "k"+strings.Repeat("8", 15), secondToken)
}

func TestReloadAddsASiteWithoutRestarting(t *testing.T) {
	engine := newFakeEngine(t, http.StatusOK)
	srv := newTestServer(t, engine)
	path := reloadTestConfig(t, reloadTwoSiteConfig("shop.example.org", "second-token-0123456789"))

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

	path := reloadTestConfig(t, reloadTwoSiteConfig("shop.example.org", "second-token-0123456789"))
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

	path := reloadTestConfig(t, oneSiteConfigBody())
	if err := srv.reload(context.Background(), path); err != nil {
		t.Fatalf("reload: %v", err)
	}

	recorder, body := post(t, srv, `{"urls":["https://example.com/b"]}`, "Bearer "+testToken)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("after a reload that spent nothing, status = %d, want 429 (%v)", recorder.Code, body)
	}
}

// oneSiteConfigBody is the config the reload tests start from, written through
// the same helper the site tests use so the two cannot drift.
func oneSiteConfigBody() string {
	return testConfigYAML(testFixtureKey(), testToken, "")
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

	added := reloadTestConfig(t, reloadTwoSiteConfig("shop.example.org", "second-token-0123456789"))
	if err := srv.reload(context.Background(), added); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if recorder, _ := post(t, srv, `{"urls":["https://shop.example.org/a"]}`, "Bearer second-token-0123456789"); recorder.Code != http.StatusOK {
		t.Fatalf("the added host does not answer: status = %d", recorder.Code)
	}

	removed := filepath.Join(t.TempDir(), initConfigName)
	if err := os.WriteFile(removed, []byte(oneSiteConfigBody()), initConfigMode); err != nil {
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
	path := reloadTestConfig(t, reloadTwoSiteConfig("shop.example.org", "second-token-0123456789"))

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
	var logs strings.Builder
	srv := newServer(&Config{
		Listen: "127.0.0.1:0",
		Sites: []Site{{
			Host:        "example.com",
			IndexNowKey: testFixtureKey(),
			KeyLocation: "https://example.com/" + testFixtureKey() + ".txt",
			Token:       testToken,
		}},
	}, newIndexNowClient(engine.server.URL), slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))

	path := reloadTestConfig(t, reloadTwoSiteConfig("shop.example.org", "second-token-0123456789"))
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
		Listen: "127.0.0.1:0",
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
	var logs strings.Builder
	srv := newServer(&Config{
		Listen: "127.0.0.1:0",
		Sites: []Site{{
			Host:        "example.com",
			IndexNowKey: testFixtureKey(),
			KeyLocation: "https://example.com/" + testFixtureKey() + ".txt",
			Token:       testToken,
		}},
	}, newIndexNowClient(engine.server.URL), slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))

	path := reloadTestConfig(t, oneSiteConfigBody())
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
	var logs strings.Builder
	srv := newServer(&Config{
		Listen: "127.0.0.1:0",
		Sites: []Site{{
			Host:        "example.com",
			IndexNowKey: testFixtureKey(),
			KeyLocation: "https://example.com/" + testFixtureKey() + ".txt",
			Token:       testToken,
		}},
	}, newIndexNowClient(engine.server.URL), slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))

	path := reloadTestConfig(t, oneSiteConfigBody())
	if err := srv.reload(context.Background(), path); err != nil {
		t.Fatalf("first reload: %v", err)
	}

	logs.Reset()
	if err := os.WriteFile(path, []byte(reloadTwoSiteConfig("shop.example.org", "second-token-0123456789")), initConfigMode); err != nil {
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
	raw := []byte(oneSiteConfigBody())

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
