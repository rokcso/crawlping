package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestFilterURLs(t *testing.T) {
	cases := []struct {
		name      string
		host      string
		urls      []string
		wantClean int
		wantBad   bool
	}{
		{
			name:      "keeps same-host urls",
			host:      "example.com",
			urls:      []string{"https://example.com/a", "http://example.com/b"},
			wantClean: 2,
		},
		{
			name:      "collapses duplicates",
			host:      "example.com",
			urls:      []string{"https://example.com/a", "https://example.com/a"},
			wantClean: 1,
		},
		{
			name:    "refuses a foreign host",
			host:    "example.com",
			urls:    []string{"https://example.com/a", "https://evil.test/b"},
			wantBad: true,
		},
		{
			name:    "refuses a lookalike subdomain",
			host:    "example.com",
			urls:    []string{"https://example.com.evil.test/a"},
			wantBad: true,
		},
		{
			name:    "refuses a non-http scheme",
			host:    "example.com",
			urls:    []string{"javascript:alert(1)"},
			wantBad: true,
		},
		{
			name:    "refuses garbage",
			host:    "example.com",
			urls:    []string{"not a url"},
			wantBad: true,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			clean, offending := filterURLs(testCase.host, testCase.urls)

			if testCase.wantBad {
				if offending == "" {
					t.Fatalf("expected the batch to be refused, got %v", clean)
				}
				return
			}

			if offending != "" {
				t.Fatalf("unexpected refusal: %q", offending)
			}
			if len(clean) != testCase.wantClean {
				t.Fatalf("clean = %d urls, want %d", len(clean), testCase.wantClean)
			}
		})
	}
}

func TestFilterURLsCapsBatchSize(t *testing.T) {
	urls := make([]string, 0, maxURLsPerRequest+1)
	for i := 0; i <= maxURLsPerRequest; i++ {
		urls = append(urls, fmt.Sprintf("https://example.com/page-%d", i))
	}

	if _, offending := filterURLs("example.com", urls); offending == "" {
		t.Fatal("expected an oversized batch to be refused")
	}
}

func TestParseRetryAfter(t *testing.T) {
	if got := parseRetryAfter("120"); got != 2*time.Minute {
		t.Fatalf("seconds form = %v", got)
	}
	if got := parseRetryAfter(""); got != time.Minute {
		t.Fatalf("empty form = %v", got)
	}
	if got := parseRetryAfter("not-a-value"); got != time.Minute {
		t.Fatalf("garbage form = %v", got)
	}
}

func TestHTTPStatusFor(t *testing.T) {
	cases := map[string]int{
		reasonAccepted:     http.StatusOK,
		reasonBadRequest:   http.StatusBadRequest,
		reasonKeyRejected:  http.StatusForbidden,
		reasonHostMismatch: http.StatusUnprocessableEntity,
		reasonRateLimited:  http.StatusTooManyRequests,
		reasonEngineError:  http.StatusBadGateway,
	}

	for reason, want := range cases {
		if got := httpStatusFor(submissionResult{Reason: reason}); got != want {
			t.Fatalf("%s: status = %d, want %d", reason, got, want)
		}
	}
}

func TestBearerToken(t *testing.T) {
	cases := map[string]string{
		"Bearer abc":   "abc",
		"bearer abc":   "abc",
		"Bearer  abc ": "abc",
		"":             "",
		"Basic abc":    "",
		"Bearer":       "",
	}

	for header, want := range cases {
		if got := bearerToken(header); got != want {
			t.Fatalf("%q: got %q, want %q", header, got, want)
		}
	}
}

func TestLocalHealthURL(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1:8080": "http://127.0.0.1:8080/health",
		":8080":          "http://127.0.0.1:8080/health",
		"0.0.0.0:9000":   "http://127.0.0.1:9000/health",
	}

	for listen, want := range cases {
		if got := localHealthURL(listen); got != want {
			t.Fatalf("%q: got %q, want %q", listen, got, want)
		}
	}
}

func TestUsageMentionsEveryCommand(t *testing.T) {
	// A cheap guard that the help text keeps up with the subcommands.
	text := "serve check healthcheck version"
	for _, command := range strings.Fields(text) {
		if !strings.Contains(usageText(), command) {
			t.Fatalf("usage text does not mention %q", command)
		}
	}
}
