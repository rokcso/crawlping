package main

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Site is one origin this instance may submit for. The token is what
// authorizes a caller to submit on the site's behalf; the host is never taken
// from the request, so a leaked token cannot be used against another domain.
type Site struct {
	Host        string `yaml:"host"`
	IndexNowKey string `yaml:"indexnow_key"`
	KeyLocation string `yaml:"key_location"`
	Token       string `yaml:"token"`
}

// Config is the mounted file. There is no environment layer: one file, read
// once at startup, 0600 on disk.
type Config struct {
	Listen           string `yaml:"listen"`
	IndexNowEndpoint string `yaml:"indexnow_endpoint"`
	Sites            []Site `yaml:"sites"`
}

const (
	defaultListen   = "127.0.0.1:8080"
	defaultEndpoint = "https://api.indexnow.org/indexnow"
	// 16 hex chars is the floor we accept for a bearer token. Anything shorter
	// is almost certainly a placeholder that was never replaced.
	minTokenLength = 16
	// The IndexNow spec allows 8-128 characters of [A-Za-z0-9-] for a key, and
	// the same string names the key file. A key outside that shape is refused
	// by the engine with 403 even when the local key check passes, so a config
	// that carries one is rejected here instead of failing per request.
	minKeyLength = 8
	maxKeyLength = 128
	// maxValueHint bounds how much of a rejected value an error echoes, so a
	// pasted blob cannot flood the log.
	maxValueHint = 64
	// maxKeyHint keeps a key echo to a few characters: enough to find the
	// entry, not enough to be a copy of the key.
	maxKeyHint = 4
)

var (
	// indexNowKeyPattern is the key shape the IndexNow spec allows. It is built
	// from the length constants so the two cannot drift apart.
	indexNowKeyPattern = regexp.MustCompile(fmt.Sprintf(`^[A-Za-z0-9-]{%d,%d}$`, minKeyLength, maxKeyLength))
	// hostLabelPattern matches one DNS label: letters, digits and inner
	// hyphens, never a leading or trailing hyphen.
	hostLabelPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
)

func loadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	decoder := yaml.NewDecoder(strings.NewReader(string(raw)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if cfg.Listen == "" {
		cfg.Listen = defaultListen
	}
	if cfg.IndexNowEndpoint == "" {
		cfg.IndexNowEndpoint = defaultEndpoint
	}
	if len(cfg.Sites) == 0 {
		return nil, fmt.Errorf("config lists no sites")
	}

	seenToken := make(map[string]string, len(cfg.Sites))
	seenHost := make(map[string]bool, len(cfg.Sites))

	for i := range cfg.Sites {
		site := &cfg.Sites[i]

		if site.Host == "" || site.IndexNowKey == "" {
			return nil, fmt.Errorf("site %d needs host and indexnow_key", i+1)
		}

		// A host with a scheme, port or path still produces a plausible
		// key_location, but the URLs the engine compares against parse to a
		// different hostname, so every submission comes back host_mismatch
		// while the local check stays green. Refuse it at startup.
		host, err := normalizeHost(site.Host)
		if err != nil {
			return nil, fmt.Errorf(
				"site %d: host %q is not a bare hostname (%v); use only the hostname, for example example.com",
				i+1, hint(site.Host, maxValueHint), err,
			)
		}

		if !indexNowKeyPattern.MatchString(site.IndexNowKey) {
			return nil, fmt.Errorf(
				"site %s: indexnow_key %q must be %d to %d characters of A-Z, a-z, 0-9 or -",
				host, hint(site.IndexNowKey, maxKeyHint), minKeyLength, maxKeyLength,
			)
		}

		// Store the lowercase form: it is what the engine compares a URL's
		// hostname against, and it keeps the duplicate check honest.
		site.Host = host

		if len(site.Token) < minTokenLength {
			return nil, fmt.Errorf(
				"site %s needs a token of at least %d characters (generate one with: openssl rand -hex 32)",
				site.Host, minTokenLength,
			)
		}
		if other, taken := seenToken[site.Token]; taken {
			return nil, fmt.Errorf("sites %s and %s share a token", other, site.Host)
		}
		if seenHost[site.Host] {
			return nil, fmt.Errorf("host %s is listed twice", site.Host)
		}

		seenToken[site.Token] = site.Host
		seenHost[site.Host] = true

		if site.KeyLocation == "" {
			site.KeyLocation = fmt.Sprintf("https://%s/%s.txt", site.Host, site.IndexNowKey)
		}
	}

	return &cfg, nil
}

// normalizeHost accepts a bare DNS hostname and returns it lowercased. Anything
// that would make key_location or the engine's host comparison wrong is
// refused, and the returned error says which part of the value is at fault.
func normalizeHost(raw string) (string, error) {
	if raw != strings.TrimSpace(raw) || strings.ContainsAny(raw, " \t\r\n") {
		return "", errors.New("it contains whitespace")
	}

	host := strings.ToLower(raw)

	if strings.Contains(host, "://") {
		return "", errors.New("it starts with a scheme")
	}
	if strings.Contains(host, ":") {
		return "", errors.New("it includes a port")
	}
	if strings.ContainsAny(host, "/?#") {
		return "", errors.New("it includes a path, query or fragment")
	}
	if strings.HasSuffix(host, ".") {
		return "", errors.New("it ends with a dot")
	}

	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return "", errors.New("it needs at least two labels")
	}
	for _, label := range labels {
		if label == "" {
			return "", errors.New("it has an empty label")
		}
		if !hostLabelPattern.MatchString(label) {
			return "", errors.New("it has characters that are not allowed in a hostname")
		}
	}

	return host, nil
}

// hint truncates a value for an error message. The ellipsis makes the
// truncation visible, so a shortened value is never mistaken for the real one.
func hint(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max] + "..."
}
