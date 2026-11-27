package main

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runInitInto drives the subcommand the way main does, but with a throwaway
// directory and the output captured, so a case can assert on both halves of
// what it produced: the files and the checklist.
func runInitInto(t *testing.T, opts initOptions) string {
	t.Helper()

	var out bytes.Buffer
	if err := runInit(&out, opts); err != nil {
		t.Fatalf("init: %v", err)
	}

	return out.String()
}

// initDir returns a directory and the single key file in it after a successful
// run, so a case does not have to guess the generated key.
func initDir(t *testing.T, opts initOptions) (string, string) {
	t.Helper()

	dir := t.TempDir()
	opts.dir = dir
	runInitInto(t, opts)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("wrote %d files, want exactly 2: %v", len(entries), entryNames(entries))
	}

	return dir, onlyKeyFile(t, dir)
}

func entryNames(entries []os.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func onlyKeyFile(t *testing.T, dir string) string {
	t.Helper()

	matches, err := filepath.Glob(filepath.Join(dir, "*.txt"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("found %d key files, want 1", len(matches))
	}

	return matches[0]
}

func TestInitWritesAConfigTheLoaderAccepts(t *testing.T) {
	// This is the contract between the generator and the loader: whatever the
	// template becomes, the real loader has to accept it, and the fields have
	// to be the ones the shape demands. Everything else here is a detail.
	shapes := []struct {
		shape  string
		listen string
		must   []string
		mustNo []string
	}{
		{
			shape:  shapeDocker,
			listen: "0.0.0.0:8080",
			// The comment is half the value: it is the only thing in the file
			// that says why the wildcard is required.
			must: []string{"eth0", "-p 127.0.0.1:8080:8080"},
		},
		{
			shape:  shapeBinary,
			listen: "127.0.0.1:8080",
			must:   []string{"loopback", "reverse proxy"},
			mustNo: []string{"eth0"},
		},
		{
			shape:  shapeTunnel,
			listen: "127.0.0.1:8080",
			must:   []string{"cloudflared", "network namespace"},
			mustNo: []string{"eth0"},
		},
	}

	for _, testCase := range shapes {
		t.Run(testCase.shape, func(t *testing.T) {
			dir, keyPath := initDir(t, initOptions{host: "example.com", shape: testCase.shape})
			configPath := filepath.Join(dir, "config.yaml")

			cfg, err := loadConfig(configPath)
			if err != nil {
				t.Fatalf("the generated config does not load: %v", err)
			}

			if cfg.Listen != testCase.listen {
				t.Fatalf("listen = %q, want %q for the %s shape", cfg.Listen, testCase.listen, testCase.shape)
			}

			// The endpoint is left out on purpose, so this is the loader's
			// default rather than a value the generator invented.
			if cfg.IndexNowEndpoint != defaultEndpoint {
				t.Fatalf("endpoint = %q, want the default %q", cfg.IndexNowEndpoint, defaultEndpoint)
			}

			if len(cfg.Sites) != 1 {
				t.Fatalf("sites = %d, want 1", len(cfg.Sites))
			}

			site := cfg.Sites[0]
			if site.Host != "example.com" {
				t.Fatalf("host = %q, want example.com", site.Host)
			}

			// The key file is at the host root, which is what makes the key
			// cover every URL on the host instead of one directory.
			wantLocation := "https://example.com/" + filepath.Base(keyPath)
			if site.KeyLocation != wantLocation {
				t.Fatalf("key_location = %q, want %q", site.KeyLocation, wantLocation)
			}
			if site.PathScope != "/" {
				t.Fatalf("path scope = %q, want /", site.PathScope)
			}

			body := readFile(t, configPath)
			for _, want := range testCase.must {
				if !strings.Contains(body, want) {
					t.Fatalf("config comment does not mention %q:\n%s", want, body)
				}
			}
			for _, unwanted := range testCase.mustNo {
				if strings.Contains(body, unwanted) {
					t.Fatalf("config mentions %q, which belongs to another shape:\n%s", unwanted, body)
				}
			}

			// The default endpoint is right for almost everyone, so the file
			// does not set it at all - not even to the value it would get.
			if strings.Contains(body, "indexnow_endpoint") {
				t.Fatalf("config sets indexnow_endpoint:\n%s", body)
			}
		})
	}
}

func TestInitWritesNothingButTheTwoFiles(t *testing.T) {
	dir, keyPath := initDir(t, initOptions{host: "example.com", shape: shapeDocker})
	configPath := filepath.Join(dir, "config.yaml")

	if got := filepath.Base(keyPath); !strings.HasSuffix(got, ".txt") {
		t.Fatalf("key file %q is not <key>.txt", got)
	}

	// config.yaml holds the token, so 0600 is part of what it writes; the key
	// file is public by design and the engine has to be able to fetch it.
	if mode := fileMode(t, configPath); mode != initConfigMode {
		t.Fatalf("config.yaml mode = %04o, want %04o", mode, initConfigMode)
	}
	if mode := fileMode(t, keyPath); mode != initKeyMode {
		t.Fatalf("key file mode = %04o, want %04o", mode, initKeyMode)
	}
}

func TestInitKeyAndTokenSatisfyTheValidators(t *testing.T) {
	dir, keyPath := initDir(t, initOptions{host: "example.com", shape: shapeDocker})

	key := strings.TrimSuffix(filepath.Base(keyPath), ".txt")
	if got, want := len(key), 32; got != want {
		t.Fatalf("key length = %d, want %d hex characters", got, want)
	}
	if key != strings.ToLower(key) {
		t.Fatalf("key %q is not lowercase", key)
	}
	if !indexNowKeyPattern.MatchString(key) {
		t.Fatalf("key %q does not match the IndexNow key pattern", key)
	}

	// The file body is the key, and a trailing newline is all the generator may
	// add: the verifier trims whitespace, so anything else would be a mismatch.
	if body := readFile(t, keyPath); strings.TrimSpace(body) != key {
		t.Fatalf("key file body = %q, want %q", body, key)
	}

	cfg, err := loadConfig(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Sites[0].IndexNowKey != key {
		t.Fatalf("config key %q does not match the key file %q", cfg.Sites[0].IndexNowKey, key)
	}

	token := cfg.Sites[0].Token
	if len(token) != 2*initTokenBytes {
		t.Fatalf("token length = %d, want %d", len(token), 2*initTokenBytes)
	}
	if len(token) < minTokenLength {
		t.Fatalf("token %q is shorter than the loader's minimum", token)
	}
	if _, err := hex.DecodeString(token); err != nil {
		t.Fatalf("token %q is not hex: %v", token, err)
	}
	if token != strings.ToLower(token) {
		t.Fatalf("token %q is not lowercase", token)
	}
}

func TestRenderConfigQuotesValuesThatLookNumeric(t *testing.T) {
	// A 32-character hex key can be all digits, and YAML reads an unquoted
	// 32-digit number as an int - the loader would then reject the file this
	// generator just wrote. Both secrets are quoted for exactly that reason.
	shape, err := initShapeFor(shapeDocker, "/tmp/config.yaml")
	if err != nil {
		t.Fatalf("shape: %v", err)
	}

	key := strings.Repeat("9", 32)
	token := strings.Repeat("1", 64)

	cfg, err := loadConfig(writeConfig(t, renderConfig("example.com", key, token, shape)))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Sites[0].IndexNowKey != key {
		t.Fatalf("key = %q, want %q", cfg.Sites[0].IndexNowKey, key)
	}
	if cfg.Sites[0].Token != token {
		t.Fatalf("token = %q, want %q", cfg.Sites[0].Token, token)
	}
}

func TestInitGeneratesDifferentSecretsEachRun(t *testing.T) {
	// A key that repeats is a key two sites could share, and a token that
	// repeats is a token one site could use against another.
	firstDir := t.TempDir()
	secondDir := t.TempDir()

	runInitInto(t, initOptions{dir: firstDir, host: "example.com", shape: shapeDocker})
	runInitInto(t, initOptions{dir: secondDir, host: "example.com", shape: shapeDocker})

	first := readConfigForTest(t, firstDir)
	second := readConfigForTest(t, secondDir)

	if first.Sites[0].IndexNowKey == second.Sites[0].IndexNowKey {
		t.Fatal("two runs generated the same key")
	}
	if first.Sites[0].Token == second.Sites[0].Token {
		t.Fatal("two runs generated the same token")
	}
}

func TestInitRefusesAHostTheLoaderWouldRefuse(t *testing.T) {
	// The explanations are the loader's own, because init calls normalizeHost
	// rather than reimplementing it: a generator with a second opinion would
	// write files its own service refuses to start with.
	cases := []struct {
		name string
		host string
		want string
	}{
		{"scheme", "https://example.com", "it starts with a scheme"},
		{"port", "example.com:8443", "it includes a port"},
		{"trailing dot", "example.com.", "it ends with a dot"},
		{"single label", "localhost", "it needs at least two labels"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			// A directory that does not exist yet, so "wrote nothing" also
			// covers "did not get as far as creating -dir".
			dir := filepath.Join(t.TempDir(), "nested")

			err := runInit(io.Discard, initOptions{dir: dir, host: testCase.host, shape: shapeDocker})
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error %q does not mention %q", err, testCase.want)
			}

			if _, statErr := os.Stat(dir); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("%s exists after a refused host (err = %v)", dir, statErr)
			}
		})
	}
}

func TestInitWithoutHostNamesTheFlag(t *testing.T) {
	dir := t.TempDir()

	// Called through the flag set rather than runInit, so this pins the wiring
	// main uses: a missing required flag is a message, not a panic and not a
	// silently written config.
	err := cmdInit([]string{"-dir", dir})
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "-host") {
		t.Fatalf("error %q does not name -host", err)
	}

	if entries, readErr := os.ReadDir(dir); readErr != nil || len(entries) != 0 {
		t.Fatalf("wrote %d files without a host (err = %v)", len(entries), readErr)
	}
}

func TestInitRefusesAnUnknownShape(t *testing.T) {
	err := cmdInit([]string{"-host", "example.com", "-shape", "k8s", "-dir", t.TempDir()})
	if err == nil {
		t.Fatal("expected an error, got nil")
	}

	for _, want := range []string{"-shape", shapeDocker, shapeBinary, shapeTunnel} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err, want)
		}
	}
}

func TestInitRefusesToOverwriteWithoutForce(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")

	const mine = "# a config I wrote by hand\nsites: []\n"
	if err := os.WriteFile(configPath, []byte(mine), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	err := runInit(io.Discard, initOptions{dir: dir, host: "example.com", shape: shapeDocker})
	if err == nil {
		t.Fatal("expected a refusal, got nil")
	}

	// The message has to name the file: "file exists" alone leaves the operator
	// guessing which of the two it would have replaced.
	if !strings.Contains(err.Error(), configPath) {
		t.Fatalf("error %q does not name %q", err, configPath)
	}
	if !strings.Contains(err.Error(), "-force") {
		t.Fatalf("error %q does not name -force", err)
	}

	if body := readFile(t, configPath); body != mine {
		t.Fatalf("config.yaml was modified without -force:\n%s", body)
	}

	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatalf("read dir: %v", readErr)
	}
	if len(entries) != 1 {
		t.Fatalf("wrote extra files beside the refused config: %v", entryNames(entries))
	}
}

func TestInitForceReplacesTheConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")

	// 0644 rather than 0600 on purpose: an overwrite keeps the old permissions
	// unless the generator sets the mode itself, and a world-readable config
	// holding a token is a downgrade.
	if err := os.WriteFile(configPath, []byte("sites: []\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	runInitInto(t, initOptions{dir: dir, host: "example.com", shape: shapeDocker, force: true})

	cfg, err := loadConfig(configPath)
	if err != nil {
		t.Fatalf("the replaced config does not load: %v", err)
	}
	if cfg.Sites[0].Host != "example.com" {
		t.Fatalf("host = %q, want example.com", cfg.Sites[0].Host)
	}
	if mode := fileMode(t, configPath); mode != initConfigMode {
		t.Fatalf("config.yaml mode = %04o after -force, want %04o", mode, initConfigMode)
	}
}

// swapRand points the key and token generator at a reader the test controls and
// puts the real one back when the test ends.
func swapRand(t *testing.T, reader io.Reader) {
	t.Helper()

	previous := randReader
	randReader = reader
	t.Cleanup(func() { randReader = previous })
}

// fixedRand replaces crypto/rand with a fixed stream, so a test can know a key
// before it is generated.
func fixedRand(t *testing.T, fill byte) {
	t.Helper()

	swapRand(t, bytes.NewReader(bytes.Repeat([]byte{fill}, 4*(initKeyBytes+initTokenBytes))))
}

// initTestRandFails is a reader that cannot deliver a secret. The generator has
// to report that rather than write a config with a predictable key in it.
type initTestRandFails struct{}

func (initTestRandFails) Read([]byte) (int, error) {
	return 0, errors.New("no entropy")
}

func TestInitRefusesToOverwriteAnExistingKeyFile(t *testing.T) {
	dir := t.TempDir()
	fixedRand(t, 0xab)

	// The key the fixed stream produces, so the file can exist before the run.
	key := strings.Repeat("ab", initKeyBytes)
	keyPath := filepath.Join(dir, key+".txt")
	if err := os.WriteFile(keyPath, []byte("someone else's key\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	err := runInit(io.Discard, initOptions{dir: dir, host: "example.com", shape: shapeDocker})
	if err == nil {
		t.Fatal("expected a refusal, got nil")
	}
	if !strings.Contains(err.Error(), keyPath) {
		t.Fatalf("error %q does not name %q", err, keyPath)
	}
	if body := readFile(t, keyPath); body != "someone else's key\n" {
		t.Fatalf("the key file was replaced without -force: %q", body)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "config.yaml")); statErr == nil {
		t.Fatal("config.yaml was written despite the refused key file")
	}
}

func TestInitReportsAFailingRandomSource(t *testing.T) {
	dir := t.TempDir()
	swapRand(t, initTestRandFails{})

	err := runInit(io.Discard, initOptions{dir: dir, host: "example.com", shape: shapeDocker})
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "random") {
		t.Fatalf("error %q does not say what failed", err)
	}

	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatalf("read dir: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("wrote %v after failing to read a secret", entryNames(entries))
	}
}

func TestInitChecklistIsCopyPasteable(t *testing.T) {
	cases := []struct {
		shape     string
		startWith string
		extra     []string
	}{
		{
			shape:     shapeDocker,
			startWith: "docker run -d --name crawlping",
			extra:     []string{"-p 127.0.0.1:8080:8080"},
		},
		{
			shape:     shapeBinary,
			startWith: "crawlping serve -config ",
		},
		{
			shape:     shapeTunnel,
			startWith: "docker run -d --name crawlping",
			extra:     []string{"--network container:cloudflared", "no port"},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.shape, func(t *testing.T) {
			dir := t.TempDir()
			out := runInitInto(t, initOptions{dir: dir, host: "example.com", shape: testCase.shape})

			cfg, err := loadConfig(filepath.Join(dir, "config.yaml"))
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			keyURL := cfg.Sites[0].KeyLocation
			keyPath := filepath.Join(dir, filepath.Base(keyURL))
			configPath := filepath.Join(dir, "config.yaml")

			// Where the key file has to be served, and the two ways to serve it:
			// the checklist is useless without the host-side half.
			for _, want := range []string{
				keyURL,
				keyPath,
				"new URL(request.url).pathname",
				"cp " + keyPath,
				configPath,
				"grep token: " + configPath,
				testCase.startWith,
				// The next step is a command, not advice.
				"crawlping check -config ",
			} {
				if !strings.Contains(out, want) {
					t.Fatalf("the checklist does not mention %q:\n%s", want, out)
				}
			}

			for _, want := range testCase.extra {
				if !strings.Contains(out, want) {
					t.Fatalf("the checklist does not mention %q for the %s shape:\n%s", want, testCase.shape, out)
				}
			}
		})
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return string(raw)
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}

	return info.Mode().Perm()
}

func readConfigForTest(t *testing.T, dir string) *Config {
	t.Helper()

	cfg, err := loadConfig(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	return cfg
}
