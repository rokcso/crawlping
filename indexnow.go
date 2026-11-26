package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Reasons a submission can fail. The caller decides what to do with them, so
// they are deliberately distinct: rate_limited is worth retrying later,
// key_rejected never is.
const (
	reasonAccepted     = ""
	reasonBadRequest   = "bad_request"
	reasonKeyRejected  = "key_rejected"
	reasonHostMismatch = "host_mismatch"
	reasonRateLimited  = "rate_limited"
	reasonEngineError  = "engine_error"
)

// maxURLsPerRequest keeps a single request small. IndexNow allows 10,000; a
// code path that sends more than a handful is almost certainly looping.
const maxURLsPerRequest = 100

type submissionResult struct {
	// Status is the engine's HTTP status, or 0 when the request never landed.
	Status int
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
	clean, offending := filterURLs(site.Host, urls)

	if offending != "" {
		return submissionResult{
			Status: http.StatusUnprocessableEntity,
			Reason: reasonHostMismatch,
			Detail: offending,
		}
	}
	if len(clean) == 0 {
		return submissionResult{
			Status: http.StatusBadRequest,
			Reason: reasonBadRequest,
			Detail: "no usable urls",
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
		// Never landed. Distinct from an engine verdict, which is why Status is 0.
		return submissionResult{Reason: reasonEngineError, Detail: err.Error()}
	}
	defer resp.Body.Close()
	// Drain so the connection can be reused; the body carries no useful detail
	// beyond the status for this endpoint.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))

	result := submissionResult{Status: resp.StatusCode}

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
		result.Reason = reasonEngineError
	default:
		result.Reason = reasonEngineError
	}

	return result
}

// filterURLs keeps only URLs on the site's own host, deduplicated and capped.
// It refuses the whole batch when one URL is foreign: a caller mixing hosts
// has a bug, and the engine would reject the batch anyway.
func filterURLs(host string, urls []string) (clean []string, offending string) {
	seen := make(map[string]bool, len(urls))

	for _, raw := range urls {
		parsed, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
			return nil, raw
		}

		if !strings.EqualFold(strings.TrimSuffix(parsed.Hostname(), "."), host) {
			return nil, raw
		}

		normalized := parsed.String()
		if seen[normalized] {
			continue
		}

		seen[normalized] = true
		clean = append(clean, normalized)

		if len(clean) > maxURLsPerRequest {
			return nil, fmt.Sprintf("more than %d urls", maxURLsPerRequest)
		}
	}

	return clean, ""
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
