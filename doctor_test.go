package main

import (
	"bytes"

	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The doctor tests build a *Config and call doctorConfig directly rather than
// writing a file and loading it. loadConfig refuses a key_location on a host
// other than the site's own, which is the right rule and the reason 127.0.0.1
// cannot be a fixture; handing the checks a Config keeps them testable without
// weakening that rule. The rest of the suite reaches a local origin the same way.

// doctorKeyServer serves the key at /<key>.txt and answers everything else with
// whatever the test asks for. A 404 is what a static host does; a 200 with HTML
// is what a site with a fallback route does, and that is the case worth testing.
func doctorKeyServer(t *testing.T, key string, missing http.HandlerFunc) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/"+key+".txt" {
			_, _ = w.Write([]byte(key + "\n"))
			return
		}
		missing(w, r)
	}))
}

func doctorTestConfig(keyLocation, listen, endpoint string) *Config {
	return &Config{
		Listen:           listen,
		IndexNowEndpoint: endpoint,
		Sites: []Site{{
			Host:        "example.com",
			IndexNowKey: testFixtureKey(),
			KeyLocation: keyLocation,
			Token:       testToken,
		}},
	}
}

func freePort(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	address := listener.Addr().String()
	listener.Close()

	return address
}

// configFileAtMode leaves a file at an exact mode, which os.WriteFile alone
// cannot promise: the umask decides what the 0600 in the call becomes, so the
// chmod afterwards is what the check under test actually reads.
func configFileAtMode(t *testing.T, mode os.FileMode) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), initConfigName)
	if err := os.WriteFile(path, []byte("listen: 8080\nsites: []\n"), initConfigMode); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod %04o: %v", mode, err)
	}

	return path
}

func TestDoctorPassesOnAWorkingDeployment(t *testing.T) {
	key := testFixtureKey()
	origin := doctorKeyServer(t, key, func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	})
	defer origin.Close()

	cfg := doctorTestConfig(origin.URL+"/"+key+".txt", freePort(t), origin.URL)

	var out bytes.Buffer
	if err := doctorConfig(&out, cfg, "config.yaml"); err != nil {
		t.Fatalf("doctor: %v\n%s", err, out.String())
	}

	printed := out.String()
	for _, want := range []string{"ok", "config", "listen", "engine", "key"} {
		if !strings.Contains(printed, want) {
			t.Fatalf("doctor output never mentions %q:\n%s", want, printed)
		}
	}
	if strings.Contains(printed, "FAIL") || strings.Contains(printed, "????") {
		t.Fatalf("doctor did not pass a working deployment:\n%s", printed)
	}
}

func TestDoctorCatchesAFallbackPage(t *testing.T) {
	// The failure that looks like success: a static site with a catch-all route
	// answers 200 for every path, so "curl it and see 200" is not evidence. The
	// engine reads that page and refuses the key, and doctor has to say why in
	// the words that make the cause obvious.
	key := testFixtureKey()
	origin := doctorKeyServer(t, key, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<!DOCTYPE html><html><body>not found</body></html>"))
	})
	defer origin.Close()

	cfg := doctorTestConfig(origin.URL+"/nonexistent.txt", freePort(t), origin.URL)

	var out bytes.Buffer
	if err := doctorConfig(&out, cfg, "config.yaml"); err == nil {
		t.Fatalf("doctor passed a key file served by a fallback page:\n%s", out.String())
	}

	printed := out.String()
	if !strings.Contains(printed, "FAIL") {
		t.Fatalf("doctor did not fail the key check:\n%s", printed)
	}
	if !strings.Contains(printed, "a page, not a key file") {
		t.Fatalf("doctor did not explain the 200 that is HTML:\n%s", printed)
	}
}

func TestDoctorReportsAWrongKeyFileAsFailed(t *testing.T) {
	// The file is served and holds something else: a definite failure, not an
	// unknown. Serving a different key's file is the same situation.
	other := "k" + strings.Repeat("4", 15)
	origin := doctorKeyServer(t, other, func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	})
	defer origin.Close()

	cfg := doctorTestConfig(origin.URL+"/"+other+".txt", freePort(t), origin.URL)

	var out bytes.Buffer
	if err := doctorConfig(&out, cfg, "config.yaml"); err == nil {
		t.Fatalf("doctor passed a mismatched key file:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "holds something else") {
		t.Fatalf("doctor did not say the content differs:\n%s", out.String())
	}
}

func TestDoctorSeparatesUnreachableFromWrong(t *testing.T) {
	// Nothing is listening, so the file was never read. That is not a verdict on
	// the key, and the report has to keep the two apart the way check does.
	cfg := doctorTestConfig("http://"+freePort(t)+"/"+testFixtureKey()+".txt", freePort(t), "https://api.indexnow.org/indexnow")

	var out bytes.Buffer
	if err := doctorConfig(&out, cfg, "config.yaml"); err == nil {
		t.Fatalf("doctor passed an unreachable key file:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "could not determine") {
		t.Fatalf("doctor did not separate unknown from failed:\n%s", out.String())
	}
}

func TestDoctorFailsAConfigThatDoesNotLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, initConfigName)
	if err := os.WriteFile(path, []byte("listen: 1\nsites: []\n"), initConfigMode); err != nil {
		t.Fatalf("write: %v", err)
	}

	var out bytes.Buffer
	if err := runDoctor(&out, path); err == nil {
		t.Fatal("doctor passed a config that does not load")
	}
	if !strings.Contains(out.String(), "does not load") {
		t.Fatalf("doctor did not name the config as the failure:\n%s", out.String())
	}
}

func TestDoctorPassesAPrivateConfigMode(t *testing.T) {
	// 0600 is what init and site add write, and the reason they bother: the file
	// holds every site's token. A check that is noisy here is a check an operator
	// learns to skip.
	path := configFileAtMode(t, initConfigMode)

	report := &doctorReport{}
	var out bytes.Buffer
	checkConfigMode(report, &out, path)

	printed := out.String()
	if report.failed != 0 {
		t.Fatalf("checkConfigMode counted %d failures for a 0600 config:\n%s", report.failed, printed)
	}
	if !strings.HasPrefix(printed, "ok") || !strings.Contains(printed, "0600") {
		t.Fatalf("checkConfigMode did not report 0600 as private:\n%s", printed)
	}
	if lines := strings.Count(strings.TrimSuffix(printed, "\n"), "\n") + 1; lines != 1 {
		t.Fatalf("checkConfigMode printed %d lines for a private mode, want the one ok line:\n%s", lines, printed)
	}
}

func TestDoctorFailsAWorldReadableConfig(t *testing.T) {
	// The defect the check exists for: the same file after an editor, a cp from a
	// backup or a chmod -R. Every token in it is now readable by any account on
	// the machine, and nothing else in the deployment has changed.
	path := configFileAtMode(t, 0o644)

	report := &doctorReport{}
	var out bytes.Buffer
	checkConfigMode(report, &out, path)

	printed := out.String()
	if report.failed != 1 {
		t.Fatalf("checkConfigMode counted %d failures for a 0644 config, want 1:\n%s", report.failed, printed)
	}
	for _, want := range []string{"FAIL", "0644", "chmod 600 " + path} {
		if !strings.Contains(printed, want) {
			t.Fatalf("checkConfigMode output never mentions %q:\n%s", want, printed)
		}
	}
}

func TestDoctorAcceptsAReadOnlyConfigMode(t *testing.T) {
	// 0400 leaks nothing, and requiring exactly 0600 would fail it. A check that
	// refuses a correct file is worse than one that says nothing: the operator
	// fixes the file, doctor keeps failing, and the next real failure is ignored.
	path := configFileAtMode(t, 0o400)

	report := &doctorReport{}
	var out bytes.Buffer
	checkConfigMode(report, &out, path)

	printed := out.String()
	if report.failed != 0 {
		t.Fatalf("checkConfigMode failed a 0400 config:\n%s", printed)
	}
	if !strings.HasPrefix(printed, "ok") || !strings.Contains(printed, "0400") {
		t.Fatalf("checkConfigMode did not accept 0400 as private:\n%s", printed)
	}
}

func TestDoctorReturnsAnErrorForALooseConfigMode(t *testing.T) {
	// The mode is a check like the others, so it has to reach the exit status:
	// a deployment whose config anyone can read is not healthy even when the
	// key files, the port and the engine are all fine.
	key := testFixtureKey()
	origin := doctorKeyServer(t, key, func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	})
	defer origin.Close()

	path := configFileAtMode(t, 0o644)
	cfg := doctorTestConfig(origin.URL+"/"+key+".txt", freePort(t), origin.URL)

	var out bytes.Buffer
	err := doctorConfig(&out, cfg, path)
	if err == nil {
		t.Fatalf("doctor passed a config file anyone can read:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "FAIL  mode") {
		t.Fatalf("doctor failed for a reason other than the mode: %v\n%s", err, out.String())
	}
}

func TestDoctorReportsABusyPortThatIsNotCrawlping(t *testing.T) {
	// A port held by something else is a deployment that cannot start, and the
	// answer must not read as healthy: doctor asks the process on the address
	// instead of concluding from the bind failure alone.
	key := testFixtureKey()
	origin := doctorKeyServer(t, key, func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	})
	defer origin.Close()

	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not crawlping", http.StatusTeapot)
	}))
	defer busy.Close()

	cfg := doctorTestConfig(origin.URL+"/"+key+".txt", strings.TrimPrefix(busy.URL, "http://"), origin.URL)

	var out bytes.Buffer
	_ = doctorConfig(&out, cfg, "config.yaml")

	if !strings.Contains(out.String(), "not crawlping") {
		t.Fatalf("doctor did not report the address as someone else's:\n%s", out.String())
	}
}

func TestDoctorReportsAPortItsOwnInstanceHolds(t *testing.T) {
	// The normal state of a deployed instance: the bind fails because this
	// process is already there. That is not a fault, and reporting it as one
	// would teach an operator to ignore doctor.
	key := testFixtureKey()
	origin := doctorKeyServer(t, key, func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	})
	defer origin.Close()

	running := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer running.Close()

	cfg := doctorTestConfig(origin.URL+"/"+key+".txt", strings.TrimPrefix(running.URL, "http://"), origin.URL)

	var out bytes.Buffer
	if err := doctorConfig(&out, cfg, "config.yaml"); err != nil {
		t.Fatalf("doctor failed an instance that is simply running: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "held by this instance") {
		t.Fatalf("doctor did not recognise its own instance:\n%s", out.String())
	}
}

func TestDoctorFailsAnUnreachableEndpoint(t *testing.T) {
	key := testFixtureKey()
	origin := doctorKeyServer(t, key, func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	})
	defer origin.Close()

	cfg := doctorTestConfig(origin.URL+"/"+key+".txt", freePort(t), "http://"+freePort(t))

	var out bytes.Buffer
	if err := doctorConfig(&out, cfg, "config.yaml"); err == nil {
		t.Fatalf("doctor passed an unreachable engine:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "cannot reach") {
		t.Fatalf("doctor did not report the engine as unreachable:\n%s", out.String())
	}
}

func TestLooksLikeHTML(t *testing.T) {
	for _, tc := range []struct {
		name  string
		probe keyFileProbe
		want  bool
	}{
		{"content type says so", keyFileProbe{ContentType: "text/html; charset=utf-8", Body: []byte("x")}, true},
		{"doctype", keyFileProbe{Body: []byte("  <!DOCTYPE html>\n<html>")}, true},
		{"html tag", keyFileProbe{Body: []byte("<html>")}, true},
		{"a key", keyFileProbe{ContentType: "text/plain", Body: []byte(testFixtureKey() + "\n")}, false},
		{"empty", keyFileProbe{}, false},
	} {
		if got := looksLikeHTML(tc.probe); got != tc.want {
			t.Fatalf("%s: looksLikeHTML = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestDescribeBody(t *testing.T) {
	probe := keyFileProbe{ContentType: "text/html", Body: []byte("12345")}
	if got, want := describeBody(probe), "text/html, 5 bytes"; got != want {
		t.Fatalf("describeBody = %q, want %q", got, want)
	}
	if got, want := describeBody(keyFileProbe{Body: []byte("x")}), "no content-type, 1 bytes"; got != want {
		t.Fatalf("describeBody without a type = %q, want %q", got, want)
	}
}

func TestFreePortIsActuallyFree(t *testing.T) {
	// The helper the rest of these tests depend on: an address nothing holds, so
	// a bind in doctor has a predictable answer.
	address := freePort(t)
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatalf("the port doctorTestConfig was told is free is not: %v", err)
	}
	listener.Close()

}
