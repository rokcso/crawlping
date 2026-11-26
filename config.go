package main

import (
	"fmt"
	"os"
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
