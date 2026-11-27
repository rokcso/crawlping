package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixtures are built here rather than written as literals so that every
// assertion can be derived from the same value the command was given. A test
// that restates a secret's last four characters is a test that fails when the
// fixture changes, for no reason worth reading.

func testFixtureKey() string { return "k" + strings.Repeat("7", 15) }

func testConfigYAML(key, token, comment string) string {
	return fmt.Sprintf(`%slisten: "127.0.0.1:0"

sites:
%s  - host: example.com
    indexnow_key: %s
    token: %s
`, comment, comment, key, token)
}

// writeTestConfig writes a loadable config into a temp dir and returns its path.
func writeTestConfig(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), initConfigName)
	if err := os.WriteFile(path, []byte(body), initConfigMode); err != nil {
		t.Fatalf("write config: %v", err)
	}

	return path
}

func oneSiteConfigPath(t *testing.T) string {
	t.Helper()

	return writeTestConfig(t, testConfigYAML(testFixtureKey(), testToken, ""))
}

func TestSiteAddAppendsASiteAndKeepsComments(t *testing.T) {
	// The comments are the point of this test. A command that rewrites a config
	// file has to leave the notes in it alone, because those notes are read by
	// whoever edits the file next.
	const notes = "# the production site\n"

	path := writeTestConfig(t, `# The listener address. Changing this to 0.0.0.0 publishes it.
listen: "127.0.0.1:0"

sites:
  # the production site
  - host: example.com
    indexnow_key: `+testFixtureKey()+`
    token: `+testToken+`
`)

	var out bytes.Buffer
	if err := runSiteAdd(&out, siteAddOptions{configPath: path, host: "shop.example.org"}); err != nil {
		t.Fatalf("site add: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}

	for _, keep := range []string{
		"# The listener address. Changing this to 0.0.0.0 publishes it.",
		strings.TrimSpace(notes),
		"shop.example.org",
	} {
		if !strings.Contains(string(raw), keep) {
			t.Fatalf("the edited config lost %q:\n%s", keep, raw)
		}
	}

	// The written file has to load: that is the whole contract of the command.
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("the edited config does not load: %v\n%s", err, raw)
	}
	if len(cfg.Sites) != 2 {
		t.Fatalf("sites = %d, want 2", len(cfg.Sites))
	}
	if cfg.Sites[1].Host != "shop.example.org" {
		t.Fatalf("second host = %q, want shop.example.org", cfg.Sites[1].Host)
	}
	if cfg.Sites[1].Token == cfg.Sites[0].Token {
		t.Fatal("the new site was given the existing site's token")
	}
	if len(cfg.Sites[1].IndexNowKey) < minKeyLength {
		t.Fatalf("the generated key %q is shorter than the spec allows", cfg.Sites[1].IndexNowKey)
	}
}

func TestSiteAddWritesTheKeyFile(t *testing.T) {
	path := oneSiteConfigPath(t)
	key := "k" + strings.Repeat("3", 15)

	var out bytes.Buffer
	if err := runSiteAdd(&out, siteAddOptions{configPath: path, host: "shop.example.org", key: key}); err != nil {
		t.Fatalf("site add: %v", err)
	}

	keyPath := filepath.Join(filepath.Dir(path), key+".txt")
	body, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read key file: %v", err)
	}
	if strings.TrimSpace(string(body)) != key {
		t.Fatalf("key file = %q, want the key itself", body)
	}

	// The key is served at the site root, so this file is public: 0644 is the
	// mode the deployment needs and the one init already uses.
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat key file: %v", err)
	}
	if mode := info.Mode().Perm(); mode != initKeyMode {
		t.Fatalf("key file mode = %04o, want %04o", mode, initKeyMode)
	}
}

func TestSiteAddKeepsTheConfigMode(t *testing.T) {
	// The token is in this file. A command that replaces it must not hand out
	// whatever the umask happens to be.
	path := oneSiteConfigPath(t)
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	var out bytes.Buffer
	if err := runSiteAdd(&out, siteAddOptions{configPath: path, host: "shop.example.org"}); err != nil {
		t.Fatalf("site add: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o640 {
		t.Fatalf("config mode = %04o, want 0640", mode)
	}
}

func TestSiteAddRefusesADuplicateHost(t *testing.T) {
	path := oneSiteConfigPath(t)

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var out bytes.Buffer
	err = runSiteAdd(&out, siteAddOptions{configPath: path, host: "example.com"})
	if err == nil {
		t.Fatal("site add accepted a host that is already configured")
	}
	if !strings.Contains(err.Error(), "already in") {
		t.Fatalf("error = %v, want it to name the duplicate", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(before) != string(after) {
		t.Fatal("the config was modified by a refused add")
	}
}

func TestSiteAddRefusesABadHost(t *testing.T) {
	path := oneSiteConfigPath(t)

	for _, host := range []string{"https://example.com", "example.com:8080", "example.com/x", "", "localhost"} {
		var out bytes.Buffer
		if err := runSiteAdd(&out, siteAddOptions{configPath: path, host: host}); err == nil {
			t.Fatalf("site add accepted -host %q", host)
		}
	}
}

func TestSiteAddRefusesAConfigThatDoesNotLoad(t *testing.T) {
	// Editing a broken config would hide the original problem behind the new
	// one, so the command refuses instead of layering.
	path := writeTestConfig(t, "listen: nope\nsites: []\n")

	var out bytes.Buffer
	err := runSiteAdd(&out, siteAddOptions{configPath: path, host: "shop.example.org"})
	if err == nil {
		t.Fatal("site add edited a config that does not load")
	}
	if !strings.Contains(err.Error(), "nothing was written") {
		t.Fatalf("error = %v, want it to say nothing happened", err)
	}
}

func TestSiteAddPointsAtInitWhenThereIsNoConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), initConfigName)

	var out bytes.Buffer
	err := runSiteAdd(&out, siteAddOptions{configPath: path, host: "shop.example.org"})
	if err == nil {
		t.Fatal("site add succeeded with no config file")
	}
	if !strings.Contains(err.Error(), "init") {
		t.Fatalf("error = %v, want it to point at init", err)
	}
}

func TestSiteRemoveCutsOneSiteAndLeavesTheRest(t *testing.T) {
	path := oneSiteConfigPath(t)

	var out bytes.Buffer
	if err := runSiteAdd(&out, siteAddOptions{configPath: path, host: "shop.example.org"}); err != nil {
		t.Fatalf("site add: %v", err)
	}

	out.Reset()
	if err := runSiteRemove(&out, path, "shop.example.org"); err != nil {
		t.Fatalf("site rm: %v", err)
	}

	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("the edited config does not load: %v", err)
	}
	if len(cfg.Sites) != 1 || cfg.Sites[0].Host != "example.com" {
		t.Fatalf("sites = %+v, want only example.com", cfg.Sites)
	}
}

func TestSiteRemoveRefusesTheLastSite(t *testing.T) {
	// A config with no sites does not load, so removing the last one would write
	// a file the service refuses to start on.
	path := oneSiteConfigPath(t)

	var out bytes.Buffer
	err := runSiteRemove(&out, path, "example.com")
	if err == nil {
		t.Fatal("site rm removed the only site")
	}
	if !strings.Contains(err.Error(), "only site") {
		t.Fatalf("error = %v, want it to name the reason", err)
	}
}

func TestSiteRemoveRefusesAnUnknownHost(t *testing.T) {
	path := oneSiteConfigPath(t)

	var out bytes.Buffer
	if err := runSiteAdd(&out, siteAddOptions{configPath: path, host: "shop.example.org"}); err != nil {
		t.Fatalf("site add: %v", err)
	}

	err := runSiteRemove(&out, path, "nope.example.com")
	if err == nil {
		t.Fatal("site rm removed a host that is not configured")
	}
	if !strings.Contains(err.Error(), "is not in") {
		t.Fatalf("error = %v, want it to say the host is absent", err)
	}
}

func TestSiteListMasksTheToken(t *testing.T) {
	path := oneSiteConfigPath(t)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	token := cfg.Sites[0].Token
	key := cfg.Sites[0].IndexNowKey

	var out bytes.Buffer
	if err := runSiteList(&out, path); err != nil {
		t.Fatalf("site list: %v", err)
	}

	printed := out.String()
	if !strings.Contains(printed, "example.com") {
		t.Fatalf("listing never names the host:\n%s", printed)
	}
	// The token is the only secret in the table, and its whole value must not
	// appear. The key is public - it is served at the host root - so it does.
	if strings.Contains(printed, token) {
		t.Fatalf("listing printed the whole token:\n%s", printed)
	}
	if !strings.Contains(printed, maskedToken(token)) {
		t.Fatalf("listing did not print the masked token %q:\n%s", maskedToken(token), printed)
	}
	if !strings.Contains(printed, key) {
		t.Fatalf("listing hid the public key:\n%s", printed)
	}
}

func TestSiteAddSurvivesAConfigWithNoTrailingNewline(t *testing.T) {
	path := writeTestConfig(t, strings.TrimSuffix(testConfigYAML(testFixtureKey(), testToken, ""), "\n"))

	var out bytes.Buffer
	if err := runSiteAdd(&out, siteAddOptions{configPath: path, host: "shop.example.org"}); err != nil {
		t.Fatalf("site add: %v", err)
	}
	if _, err := loadConfig(path); err != nil {
		t.Fatalf("the edited config does not load: %v", err)
	}
}

func TestSiteAddRefusesAMissingDirectory(t *testing.T) {
	// A rename either happens or it does not; what must never happen is a
	// truncated config where a working one was.
	path := oneSiteConfigPath(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	missing := filepath.Join(filepath.Dir(path), "gone", initConfigName)
	var out bytes.Buffer
	if err := runSiteAdd(&out, siteAddOptions{configPath: missing, host: "shop.example.org"}); err == nil {
		t.Fatal("site add wrote into a directory that does not exist")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("the untouched config changed:\n%s", after)
	}
}

func TestAppendSiteRefusesAFileThatIsNotAMapping(t *testing.T) {
	if _, err := appendSite([]byte("- a\n- b\n"), Site{Host: "shop.example.org"}); err == nil {
		t.Fatal("appendSite accepted a document that is not a mapping")
	}
}

func TestMaskedToken(t *testing.T) {
	for _, tc := range []struct{ token, want string }{
		// Derived, not restated: the mask keeps the tail of whatever token it is
		// given, so the expectation has to come from the same value.
		{testToken, "•••" + testToken[len(testToken)-4:]},
		{"abcd", "••••"},
		{"", ""},
	} {
		if got := maskedToken(tc.token); got != tc.want {
			t.Fatalf("maskedToken(%q) = %q, want %q", tc.token, got, tc.want)
		}
	}
}

func TestSiteAddPrintsTheNextSteps(t *testing.T) {
	path := oneSiteConfigPath(t)

	var out bytes.Buffer
	if err := runSiteAdd(&out, siteAddOptions{configPath: path, host: "shop.example.org"}); err != nil {
		t.Fatalf("site add: %v", err)
	}

	printed := out.String()
	// The reload command is the step that makes the site live, and it is the one
	// a person forgets. The key location is the other: the file has to be
	// deployed before the first submission.
	for _, want := range []string{"HUP", "shop.example.org", "check", "Bearer"} {
		if !strings.Contains(printed, want) {
			t.Fatalf("the output never mentions %q:\n%s", want, printed)
		}
	}
}

func TestSiteAddNamesTheKeyLocation(t *testing.T) {
	path := oneSiteConfigPath(t)
	key := "k" + strings.Repeat("5", 15)

	var out bytes.Buffer
	if err := runSiteAdd(&out, siteAddOptions{configPath: path, host: "shop.example.org", key: key}); err != nil {
		t.Fatalf("site add: %v", err)
	}

	if want := fmt.Sprintf("https://shop.example.org/%s.txt", key); !strings.Contains(out.String(), want) {
		t.Fatalf("output does not name %s:\n%s", want, out.String())
	}
}

func TestSiteAddExplainsTheShareWhenItAddsTheSecondSite(t *testing.T) {
	// Adding the second site is the only moment the per-site engine share would
	// change, and the share is decided when the service starts: the reload this
	// command prints applies the site list, not the share. The operator running
	// this is the one who will not read the running process's warning, so the
	// caveat belongs here too.
	path := oneSiteConfigPath(t)

	var out bytes.Buffer
	if err := runSiteAdd(&out, siteAddOptions{configPath: path, host: "shop.example.org"}); err != nil {
		t.Fatalf("site add: %v", err)
	}

	printed := out.String()
	for _, want := range []string{
		"share",
		fmt.Sprintf("keeps all %d engine calls", maxInFlightEngineCalls),
		fmt.Sprintf("documented as %d", perHostInFlightLimit(2)),
		"restart is what applies the share",
	} {
		if !strings.Contains(printed, want) {
			t.Fatalf("the output never says %q:\n%s", want, printed)
		}
	}
}

func TestSiteAddRepeatsTheShareOnlyForTheSecondSite(t *testing.T) {
	// One site holds the whole instance cap and any number above one holds half
	// of it, so a third site changes nothing. Saying it anyway is the kind of
	// noise that trains a reader to skip the block that follows.
	for _, total := range []int{1, 3} {
		var out bytes.Buffer
		printSiteAdded(&out, "config.yaml", "/tmp/"+testFixtureKey()+".txt", Site{
			Host:        "shop.example.org",
			IndexNowKey: testFixtureKey(),
			KeyLocation: "https://shop.example.org/" + testFixtureKey() + ".txt",
		}, total, true)

		if strings.Contains(out.String(), "share") {
			t.Fatalf("%d sites repeated the share caveat:\n%s", total, out.String())
		}
	}
}

func TestSiteSubcommandNames(t *testing.T) {
	if err := cmdSite(nil); err == nil {
		t.Fatal("site with no subcommand succeeded")
	}
	if err := cmdSite([]string{"bogus"}); err == nil {
		t.Fatal("site with an unknown subcommand succeeded")
	}
}
