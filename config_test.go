package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	configTestToken = "token-shared-for-two-sites"
	configTestKey   = "abcdefgh1234"
)

// singleSiteConfig renders a one-site config around the values under test, so a
// table case only has to name the field it is breaking.
func singleSiteConfig(host, key string) string {
	return fmt.Sprintf(`
sites:
  - host: %q
    indexnow_key: %q
    token: %q
`, host, key, configTestToken)
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	return path
}

// mustReject loads a config that has to fail and returns the error, after
// checking that its message carries every wanted fragment.
func mustReject(t *testing.T, body string, want ...string) error {
	t.Helper()

	_, err := loadConfig(writeConfig(t, body))
	if err == nil {
		t.Fatal("expected an error, got nil")
	}

	for _, fragment := range want {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("error %q does not mention %q", err, fragment)
		}
	}

	return err
}

func TestLoadConfigRejectsHostShape(t *testing.T) {
	// A host the engine will not agree with must fail at startup. Every case
	// here used to load, and two of them made the service unable to submit.
	cases := []struct {
		name string
		host string
		want string
	}{
		{"scheme", "https://example.com", "it starts with a scheme"},
		{"scheme without path", "http://example.com", "it starts with a scheme"},
		{"port", "example.com:8443", "it includes a port"},
		{"path", "example.com/path", "it includes a path, query or fragment"},
		{"trailing slash", "example.com/", "it includes a path, query or fragment"},
		{"query", "example.com?x=1", "it includes a path, query or fragment"},
		{"fragment", "example.com#top", "it includes a path, query or fragment"},
		{"trailing dot", "example.com.", "it ends with a dot"},
		{"single label", "localhost", "it needs at least two labels"},
		{"empty label", "example..com", "it has an empty label"},
		{"leading dot", ".example.com", "it has an empty label"},
		{"inner whitespace", "example .com", "it contains whitespace"},
		{"surrounding whitespace", " example.com", "it contains whitespace"},
		{"underscore label", "exam_ple.com", "it has characters that are not allowed in a hostname"},
		{"hyphen edge", "-example.com", "it has characters that are not allowed in a hostname"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			mustReject(t, singleSiteConfig(testCase.host, configTestKey),
				"site 1",
				testCase.want,
				"for example example.com",
			)
		})
	}
}

func TestLoadConfigRejectsKeyShape(t *testing.T) {
	// The key doubles as the key file name and the value the engine matches, so
	// a key outside the spec makes the local check pass while submissions come
	// back 403.
	const keyShape = "must be 8 to 128 characters of A-Z, a-z, 0-9 or -"

	cases := []struct {
		name string
		key  string
	}{
		{"short key", "abc"},
		{"one character short", "abcdefg"},
		{"space", "abcd efgh"},
		{"underscore", "abcd_efgh"},
		{"dot", "abcd.efgh"},
		{"slash", "abcd/efgh"},
		{"long key", strings.Repeat("a", 129)},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := mustReject(t, singleSiteConfig("example.com", testCase.key),
				"site example.com",
				keyShape,
			)

			// The key is a public value, but an error is not the place to copy
			// a long one, so only a few characters may be echoed.
			if len(testCase.key) > 8 && strings.Contains(err.Error(), testCase.key) {
				t.Fatalf("error echoes the whole key: %q", err)
			}
		})
	}
}

func TestLoadConfigNormalizesHost(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, singleSiteConfig("Example.COM", configTestKey)))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// The stored host is what the engine compares a URL's hostname against, so
	// it has to be the lowercase form.
	if got := cfg.Sites[0].Host; got != "example.com" {
		t.Fatalf("host = %q, want example.com", got)
	}

	want := "https://example.com/" + configTestKey + ".txt"
	if got := cfg.Sites[0].KeyLocation; got != want {
		t.Fatalf("key_location = %q, want %q", got, want)
	}
}

func TestLoadConfigKeepsExplicitKeyLocation(t *testing.T) {
	body := fmt.Sprintf(`
sites:
  - host: Example.com
    indexnow_key: %q
    key_location: "https://example.com/keys/%s.txt"
    token: %q
`, configTestKey, configTestKey, configTestToken)

	cfg, err := loadConfig(writeConfig(t, body))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if got := cfg.Sites[0].Host; got != "example.com" {
		t.Fatalf("host = %q, want example.com", got)
	}
	if got := cfg.Sites[0].KeyLocation; got != "https://example.com/keys/"+configTestKey+".txt" {
		t.Fatalf("key_location = %q, want the configured value", got)
	}
}

func TestLoadConfigAppliesDefaults(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, singleSiteConfig("example.com", configTestKey)))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Listen != defaultListen {
		t.Fatalf("listen = %q, want %q", cfg.Listen, defaultListen)
	}
	if cfg.IndexNowEndpoint != defaultEndpoint {
		t.Fatalf("endpoint = %q, want %q", cfg.IndexNowEndpoint, defaultEndpoint)
	}
	if got, want := cfg.Sites[0].KeyLocation, "https://example.com/"+configTestKey+".txt"; got != want {
		t.Fatalf("key_location = %q, want %q", got, want)
	}
}

func TestLoadConfigRejectsBadInput(t *testing.T) {
	// The checks that predate host and key validation still have to fire.
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "no sites",
			body: `
sites: []
`,
			want: "config lists no sites",
		},
		{
			name: "missing key",
			body: `
sites:
  - host: example.com
    token: "token-shared-for-two-sites"
`,
			want: "needs host and indexnow_key",
		},
		{
			name: "short token",
			body: fmt.Sprintf(`
sites:
  - host: example.com
    indexnow_key: %q
    token: short
`, configTestKey),
			want: "token of at least 16 characters",
		},
		{
			name: "duplicate token",
			body: fmt.Sprintf(`
sites:
  - host: a.com
    indexnow_key: %q
    token: %q
  - host: b.com
    indexnow_key: %q
    token: %q
`, configTestKey, configTestToken, configTestKey, configTestToken),
			want: "share a token",
		},
		{
			name: "duplicate host",
			body: fmt.Sprintf(`
sites:
  - host: a.com
    indexnow_key: %q
    token: %q
  - host: a.com
    indexnow_key: %q
    token: "another-token-for-the-same-run"
`, configTestKey, configTestToken, configTestKey),
			want: "is listed twice",
		},
		{
			name: "duplicate host in another case",
			body: fmt.Sprintf(`
sites:
  - host: Example.com
    indexnow_key: %q
    token: %q
  - host: example.com
    indexnow_key: %q
    token: "another-token-for-the-same-run"
`, configTestKey, configTestToken, configTestKey),
			want: "is listed twice",
		},
		{
			name: "unknown field",
			body: fmt.Sprintf(`
sites:
  - host: example.com
    indexnow_key: %q
    token: %q
    typo_here: 1
`, configTestKey, configTestToken),
			want: "parse config",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			mustReject(t, testCase.body, testCase.want)
		})
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	if _, err := loadConfig(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("expected an error")
	}
}

func TestLoadConfigRejectsAKeyLocationOnAnotherHost(t *testing.T) {
	// IndexNow fetches the key from the host being submitted for, so a key file
	// that lives somewhere else is refused with 422 whatever it contains. That
	// is knowable without any network access, so it must not survive loading.
	cases := map[string]string{
		"another host": `
sites:
  - host: example.com
    indexnow_key: good-key-1234
    key_location: https://elsewhere.test/key.txt
    token: token-shared-for-two-sites
`,
		"no scheme": `
sites:
  - host: example.com
    indexnow_key: good-key-1234
    key_location: example.com/key.txt
    token: token-shared-for-two-sites
`,
		"no host": `
sites:
  - host: example.com
    indexnow_key: good-key-1234
    key_location: "/key.txt"
    token: token-shared-for-two-sites
`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := loadConfig(writeConfig(t, body)); err == nil {
				t.Fatal("expected the key_location to be refused")
			}
		})
	}

	// The same host in another case, and an explicit key_location that agrees,
	// both have to load.
	for _, keyLocation := range []string{"https://example.com/other/path/key.txt", "https://EXAMPLE.com./key.txt"} {
		body := `
sites:
  - host: example.com
    indexnow_key: good-key-1234
    key_location: ` + keyLocation + `
    token: token-shared-for-two-sites
`
		if _, err := loadConfig(writeConfig(t, body)); err != nil {
			t.Fatalf("%s should load: %v", keyLocation, err)
		}
	}
}
