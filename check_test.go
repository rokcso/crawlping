package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// checkConfig is one site whose key file is served by keyLocation. The host has
// to be 127.0.0.1 rather than example.com: loadConfig refuses a key_location on
// another host, and a test cannot serve example.com.
func checkConfig(keyLocation, key string) string {
	return fmt.Sprintf(`
sites:
  - host: "127.0.0.1"
    indexnow_key: %q
    key_location: %q
    token: %q
`, key, keyLocation, configTestToken)
}

// runCheck drives cmdCheck and returns what it printed.
func runCheck(t *testing.T, configPath string) (string, error) {
	t.Helper()

	var out bytes.Buffer
	err := cmdCheck([]string{"-config", configPath}, &out)

	return out.String(), err
}

func TestCheckReportsOkAndSucceeds(t *testing.T) {
	keyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, configTestKey)
	}))
	defer keyServer.Close()

	out, err := runCheck(t, writeConfig(t, checkConfig(keyServer.URL+"/key.txt", configTestKey)))

	if err != nil {
		t.Fatalf("check failed on a good key file: %v", err)
	}
	if !strings.HasPrefix(out, "ok") {
		t.Fatalf("output does not start with ok: %q", out)
	}
	// The address is what the operator needs to paste into a browser.
	if !strings.Contains(out, keyServer.URL+"/key.txt") {
		t.Fatalf("output does not name the key location: %q", out)
	}
}

func TestCheckReportsFailedAndFailsTheCommand(t *testing.T) {
	// A 404 is an answer: the file was fetched and refused. It has to FAIL the
	// command, because the exit status is what gates a deploy.
	keyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer keyServer.Close()

	out, err := runCheck(t, writeConfig(t, checkConfig(keyServer.URL+"/key.txt", configTestKey)))

	if err == nil {
		t.Fatal("a refused key file did not fail the command")
	}
	if !strings.Contains(out, "FAIL") {
		t.Fatalf("output does not say FAIL: %q", out)
	}
	if !strings.Contains(err.Error(), "1 of 1 key files failed") {
		t.Fatalf("error = %q, want it to count the failure", err)
	}
}

func TestCheckReportsMismatchedBodyAsFailed(t *testing.T) {
	// The case that used to pass locally and be refused by the engine on every
	// submission: the file is there, it just is not this key.
	keyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "a-different-key")
	}))
	defer keyServer.Close()

	out, err := runCheck(t, writeConfig(t, checkConfig(keyServer.URL+"/key.txt", configTestKey)))

	if err == nil {
		t.Fatal("a mismatched key file did not fail the command")
	}
	if !strings.Contains(out, "FAIL") {
		t.Fatalf("output does not say FAIL: %q", out)
	}
	if !strings.Contains(out, "does not match") {
		t.Fatalf("output does not explain the mismatch: %q", out)
	}
}

func TestCheckReportsUnreachableAsUnknownNotFailed(t *testing.T) {
	// An origin that never answers is not a broken key file, and the two have to
	// read differently: one is the operator's problem, the other is somebody
	// else's outage. The budget is spent on a real hang rather than a refused
	// connection, which is the case that actually reaches this branch in the
	// field, so it is shortened instead of waited out.
	defer func(d time.Duration) { checkSiteTimeout = d }(checkSiteTimeout)
	checkSiteTimeout = 50 * time.Millisecond

	hang := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer hang.Close()

	out, err := runCheck(t, writeConfig(t, checkConfig(hang.URL+"/key.txt", configTestKey)))

	if err == nil {
		t.Fatal("an unreachable origin did not fail the command")
	}
	if !strings.Contains(out, "????") {
		t.Fatalf("output does not use the unknown marker: %q", out)
	}
	if strings.Contains(out, "FAIL") {
		t.Fatalf("an unreachable origin was reported as a failure: %q", out)
	}
	if !strings.Contains(err.Error(), "could not be checked") {
		t.Fatalf("error = %q, want it to say nothing is known", err)
	}
	if strings.Contains(err.Error(), "failed") {
		t.Fatalf("error = %q, blames a key file for a network problem", err)
	}
}

func TestCheckCountsFailuresAndUnknownsTogether(t *testing.T) {
	// Two sites need two hosts, and each key_location has to be on its own host.
	// The second one hangs, so the budget is shortened rather than waited out.
	defer func(d time.Duration) { checkSiteTimeout = d }(checkSiteTimeout)
	checkSiteTimeout = 50 * time.Millisecond

	keyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer keyServer.Close()

	hang := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer hang.Close()

	body := fmt.Sprintf(`
sites:
  - host: "127.0.0.1"
    indexnow_key: %q
    key_location: %q
    token: %q
  - host: "127.0.0.2"
    indexnow_key: %q
    key_location: %q
    token: %q
`, configTestKey, keyServer.URL+"/key.txt", configTestToken,
		configTestKey, "http://127.0.0.2"+strings.TrimPrefix(hang.URL, "http://127.0.0.1")+"/key.txt",
		"{{SECRET_2fxaotjm}}")

	out, err := runCheck(t, writeConfig(t, body))

	if err == nil {
		t.Fatal("check with a failure and an unknown did not fail the command")
	}
	if !strings.Contains(out, "FAIL") || !strings.Contains(out, "????") {
		t.Fatalf("output does not report both states: %q", out)
	}
	for _, want := range []string{"1 of 2 key files failed", "1 could not be checked"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to mention %q", err, want)
		}
	}
}

func TestCheckRefusesAConfigTheLoaderRefuses(t *testing.T) {
	// check is the first command a new operator runs, so its config errors have
	// to be the loader's, not a panic and not a silent empty run.
	out, err := runCheck(t, writeConfig(t, "sites: []\n"))

	if err == nil {
		t.Fatal("an empty config did not fail the command")
	}
	if out != "" {
		t.Fatalf("a config error printed a per-site table: %q", out)
	}
}
