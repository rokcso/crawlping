package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Reasons a submission can fail. The caller decides what to do with them, so
// they are deliberately distinct: rate_limited and engine_error are worth
// retrying later, key_rejected and engine_rejected never are.
const (
	// reasonAccepted means the engine took the batch (200 or 202).
	reasonAccepted = ""
	// reasonBadRequest means the request was malformed, or carried no URLs.
	reasonBadRequest = "bad_request"
	// reasonInvalidURL means one entry is not an absolute http or https URL.
	reasonInvalidURL = "invalid_url"
	// reasonTooMany means the batch carried more than maxURLsPerRequest URLs.
	reasonTooMany = "too_many"
	// reasonNonDefaultPort means one entry names a port the engine will not accept.
	reasonNonDefaultPort = "non_default_port"
	// reasonKeyRejected means the engine did not accept the key file.
	reasonKeyRejected = "key_rejected"
	// reasonHostMismatch means one entry is not on the site's own host.
	reasonHostMismatch = "host_mismatch"
	// reasonRateLimited is the engine throttling; retry later.
	reasonRateLimited = "rate_limited"
	// reasonEngineRejected means the engine refused the request in a way that
	// has no retry rule. Sending the same request again cannot help.
	reasonEngineRejected = "engine_rejected"
	// reasonEngineError means the engine failed or the request never landed.
	reasonEngineError = "engine_error"
)

// maxURLsPerRequest keeps a single request small. IndexNow allows 10,000; a
// code path that sends more than a handful is almost certainly looping.
const maxURLsPerRequest = 100

// maxEngineDetailBytes bounds how much of an engine error body reaches a log
// line or an API response.
const maxEngineDetailBytes = 512

// Refusals filterURLs can return. Each one is a request problem, so the caller
// gets a 4xx and an error that names the entry that caused it. They are
// separate values because the caller has to tell them apart: a too-large batch
// is not a host problem, and an empty entry is not "no usable urls".
var (
	// errNoURLs means nothing usable was submitted.
	errNoURLs = errors.New("no usable urls")
	// errTooManyURLs means the batch is larger than maxURLsPerRequest.
	errTooManyURLs = fmt.Errorf("more than %d urls", maxURLsPerRequest)
	// errInvalidURL means an entry is empty or not an absolute http/https URL.
	errInvalidURL = errors.New("invalid url")
	// errForeignHost means an entry is not on the site's own host.
	errForeignHost = errors.New("foreign host")
	// errNonDefaultPort means an entry names a port IndexNow will not accept.
	errNonDefaultPort = errors.New("non-default port")
)

type submissionResult struct {
	// EngineStatus is the engine's HTTP status, or 0 when the engine was never
	// called: a local refusal or a request that never landed leaves it at 0
	// rather than inventing a status the engine never sent.
	EngineStatus int
	// Reason is empty when the engine accepted the batch.
	Reason     string
	RetryAfter time.Duration
	Detail     string
	Submitted  int
}

func (r submissionResult) accepted() bool {
	return r.Reason == reasonAccepted
}

type indexNowClient struct {
	endpoint string
	http     *http.Client
}

func newIndexNowClient(endpoint string) *indexNowClient {
	return &indexNowClient{
		endpoint: endpoint,
		http:     &http.Client{Timeout: 15 * time.Second},
	}
}

// verifyKey fetches the key file the engine will fetch. A mismatch here means
// every submission will come back 403, so it is worth failing loudly at
// startup rather than silently per request.
func (c *indexNowClient) verifyKey(ctx context.Context, site Site) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, site.KeyLocation, nil)
	if err != nil {
		return fmt.Errorf("build key request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("fetch key file: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("key file returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 512))
	if err != nil {
		return fmt.Errorf("read key file: %w", err)
	}

	if strings.TrimSpace(string(body)) != site.IndexNowKey {
		return fmt.Errorf("key file does not match indexnow_key")
	}

	return nil
}

// submit posts the batch and translates the engine's status into a reason.
func (c *indexNowClient) submit(ctx context.Context, site Site, urls []string) submissionResult {
	clean, err := filterURLs(site.Host, urls)
	if err != nil {
		// Locally refused: the engine was never called, so EngineStatus stays
		// 0 and the reason plus detail say what the caller has to fix.
		return submissionResult{
			Reason: reasonForURLError(err),
			Detail: err.Error(),
		}
	}

	payload, err := json.Marshal(map[string]any{
		"host":        site.Host,
		"key":         site.IndexNowKey,
		"keyLocation": site.KeyLocation,
		"urlList":     clean,
	})
	if err != nil {
		return submissionResult{Reason: reasonEngineError, Detail: err.Error()}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return submissionResult{Reason: reasonEngineError, Detail: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")

	resp, err := c.http.Do(req)
	if err != nil {
		// Never landed. Distinct from an engine verdict, which is why
		// EngineStatus stays 0.
		return submissionResult{Reason: reasonEngineError, Detail: err.Error()}
	}
	defer resp.Body.Close()

	result := submissionResult{EngineStatus: resp.StatusCode}

	switch {
	case resp.StatusCode == http.StatusOK, resp.StatusCode == http.StatusAccepted:
		result.Submitted = len(clean)
	case resp.StatusCode == http.StatusBadRequest:
		result.Reason = reasonBadRequest
	case resp.StatusCode == http.StatusForbidden:
		result.Reason = reasonKeyRejected
	case resp.StatusCode == http.StatusUnprocessableEntity:
		result.Reason = reasonHostMismatch
	case resp.StatusCode == http.StatusTooManyRequests:
		result.Reason = reasonRateLimited
		result.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
	case resp.StatusCode >= 500:
		// The engine broke or is briefly unavailable: retry later.
		result.Reason = reasonEngineError
	default:
		// Any other status is a verdict this service has no retry rule for.
		// Reporting it as engine_error would tell the caller to retry a
		// request the engine will refuse every time.
		result.Reason = reasonEngineRejected
	}

	// Read a bounded prefix whatever the status is: it is the only place the
	// engine explains a refusal, and reading it lets the connection be reused
	// instead of being closed mid-body.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxEngineDetailBytes))
	if !result.accepted() {
		result.Detail = summarizeEngineBody(body)
	}

	return result
}

// filterURLs keeps only URLs on the site's own host, deduplicated and capped.
// It refuses the whole batch when one URL is bad: a caller mixing hosts has a
// bug, and the engine would reject the batch anyway. The error always names
// the entry that caused the refusal, and a batch with one unusable entry is
// never reported as "no usable urls".
func filterURLs(host string, urls []string) (clean []string, err error) {
	want := normalizeHost(host)
	seen := make(map[string]bool, len(urls))

	for i, raw := range urls {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			return nil, fmt.Errorf("%w: entry %d is empty", errInvalidURL, i+1)
		}

		parsed, parseErr := url.Parse(trimmed)
		if parseErr != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
			return nil, fmt.Errorf("%w: %q is not an absolute http or https URL", errInvalidURL, trimmed)
		}

		if port := parsed.Port(); port != "" && port != defaultPort(parsed.Scheme) {
			return nil, fmt.Errorf(
				"%w: %q uses port %q, and IndexNow accepts the default port only",
				errNonDefaultPort, trimmed, port,
			)
		}

		if normalizeHost(parsed.Hostname()) != want {
			return nil, fmt.Errorf("%w: %q is not on %s", errForeignHost, trimmed, want)
		}

		// A fragment marks a place inside a document, not another resource, and
		// it is never sent in an HTTP request. Two URLs that differ only by
		// fragment are one target, so drop it before deduplicating.
		parsed.Fragment = ""
		parsed.RawFragment = ""

		normalized := parsed.String()
		if seen[normalized] {
			continue
		}

		seen[normalized] = true
		clean = append(clean, normalized)
	}

	if len(clean) == 0 {
		return nil, errNoURLs
	}

	if len(clean) > maxURLsPerRequest {
		return nil, errTooManyURLs
	}

	return clean, nil
}

// normalizeHost folds the two things that are not part of a host's identity:
// its case and the trailing dot of an absolute DNS name. Both sides of the
// comparison go through it, so a configured "Example.com." still matches
// https://example.com/...
func normalizeHost(host string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
}

// defaultPort is the only port IndexNow accepts for a scheme.
func defaultPort(scheme string) string {
	if scheme == "http" {
		return "80"
	}
	return "443"
}

// reasonForURLError maps a filterURLs refusal to the reason the caller sees.
// Every one of these is a request problem: reporting a too-large batch or a
// bad port as host_mismatch would send the caller looking at their domain.
func reasonForURLError(err error) string {
	switch {
	case errors.Is(err, errNoURLs):
		return reasonBadRequest
	case errors.Is(err, errTooManyURLs):
		return reasonTooMany
	case errors.Is(err, errNonDefaultPort):
		return reasonNonDefaultPort
	case errors.Is(err, errForeignHost):
		return reasonHostMismatch
	case errors.Is(err, errInvalidURL):
		return reasonInvalidURL
	default:
		return reasonBadRequest
	}
}

// summarizeEngineBody turns an engine error body into one short line fit for a
// log: at most maxEngineDetailBytes of text with every whitespace run collapsed
// and control characters dropped, so a multi-line or binary body cannot flood
// or corrupt a log line.
func summarizeEngineBody(body []byte) string {
	printable := strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t':
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, string(body))

	return strings.TrimSpace(strings.Join(strings.Fields(printable), " "))
}

// parseRetryAfter accepts both forms the header allows: delay-seconds and a
// HTTP date. Anything unparseable falls back to a minute.
func parseRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Minute
	}

	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds < 0 {
			return time.Minute
		}
		return time.Duration(seconds) * time.Second
	}

	if when, err := http.ParseTime(value); err == nil {
		if delay := time.Until(when); delay > 0 {
			return delay
		}
		return time.Minute
	}

	return time.Minute
}
