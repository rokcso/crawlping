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
// line or an API response. It is deliberately small, which means a body larger
// than this is abandoned mid-stream and its connection cannot be reused;
// draining a whole engine error page just to keep one connection alive is not
// worth the allocation.
const maxEngineDetailBytes = 512

// engineTimeout bounds one request to the engine, headers and body. It is also
// the client's timeout, so the two cannot disagree: the cancel comes from this
// context, not from the caller's, because a caller that gives up must not be
// able to recall a notification that may already have been received.
const engineTimeout = 15 * time.Second

// Why a key-file check reached no answer, separated from the check reaching a
// bad answer. Only the second is evidence that the key is broken: an origin
// that is down, and a key file that is wrong, need different things from the
// operator and must not be reported as the same result.
var (
	// errKeyFileUnreachable means the key file was never read, so nothing is
	// known about the key.
	errKeyFileUnreachable = errors.New("key file unreachable")
	// errKeyFileRejected means the key file was read and does not reproduce the
	// configured key.
	errKeyFileRejected = errors.New("key file rejected")
)

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

// keyStateAfter reports what a submission proves about the site's key. An empty
// state means it proves nothing, so the recorded state must be left alone: an
// engine error or a rate limit is not evidence that anything is wrong with the
// key file. An accepted batch proves a working key only when the engine answered
// 200; 202 says the batch was received and key validation is still pending, so
// it records "unknown" rather than claiming the key works.
func (r submissionResult) keyStateAfter() keyState {
	if r.accepted() {
		switch r.EngineStatus {
		case http.StatusOK:
			return keyStateVerified
		case http.StatusAccepted:
			return keyStatePending
		}

		return ""
	}

	// The engine fetched the key file itself and refused it.
	if r.Reason == reasonKeyRejected {
		return keyStateFailed
	}

	return ""
}

type indexNowClient struct {
	endpoint string
	http     *http.Client
}

func newIndexNowClient(endpoint string) *indexNowClient {
	return &indexNowClient{
		endpoint: endpoint,
		http:     &http.Client{Timeout: engineTimeout},
	}
}

// verifyKey fetches the key file the engine will fetch. A mismatch here means
// every submission will come back 403, so it is worth reporting rather than
// discovering per request. Every error says which of the two things happened:
// the file was read and is wrong, or it was never read at all.
func (c *indexNowClient) verifyKey(ctx context.Context, site Site) error {
	// The key_location/host agreement is enforced in loadConfig, not here: it is
	// a property of the configuration, it needs no network, and checking it on
	// this path made every test fixture that serves its key from 127.0.0.1 look
	// like a broken site.

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, site.KeyLocation, nil)
	if err != nil {
		return fmt.Errorf("%w: build key request: %w", errKeyFileUnreachable, err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// Includes a cancelled or timed-out check: no key file was read.
		return fmt.Errorf("%w: fetch key file: %w", errKeyFileUnreachable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: key file returned %d", errKeyFileRejected, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 512))
	if err != nil {
		return fmt.Errorf("%w: read key file: %w", errKeyFileRejected, err)
	}

	if strings.TrimSpace(string(body)) != site.IndexNowKey {
		return fmt.Errorf("%w: key file does not match indexnow_key", errKeyFileRejected)
	}

	return nil
}

// keyStateAfterCheck maps a finished key check to the state it proves. An empty
// state means the check never reached an answer and the recorded state must be
// left alone: shutdown cancelling the check is not a faulty key file, and
// reporting it as one is what used to mark every remaining site failed - at
// ERROR level - on SIGTERM.
func keyStateAfterCheck(err error) keyState {
	switch {
	case err == nil:
		return keyStateVerified
	case errors.Is(err, context.Canceled):
		return ""
	case errors.Is(err, errKeyFileRejected):
		return keyStateFailed
	default:
		// A deadline, a refused connection, or a name that does not resolve:
		// the key file was never read, so we do not know. Not knowing is
		// pending, not failed.
		return keyStatePending
	}
}

// submit posts the batch and translates the engine's status into a reason.
//
// It takes no caller context on purpose. The URL set is already decided by the
// time this runs, so the caller's patience cannot change the outcome - and
// cancelling the POST because the caller timed out would not un-send a notice
// the engine may already have received. The call therefore gets its own timeout
// from context.Background and cancels it when it returns.
func (c *indexNowClient) submit(site Site, urls []string) submissionResult {
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

	reqCtx, cancel := context.WithTimeout(context.Background(), engineTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
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
	// engine explains a refusal. A body larger than maxEngineDetailBytes is
	// abandoned mid-stream, so that connection is not reused; the cap is worth
	// more than the connection, since the detail is a log line.
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
	want := canonicalHost(host)
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

		if canonicalHost(parsed.Hostname()) != want {
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

// canonicalHost folds the two things that are not part of a host's identity:
// its case and the trailing dot of an absolute DNS name. Both sides of the
// comparison go through it, so a configured "Example.com." still matches
// https://example.com/... This is the comparison form only; config's
// normalizeHost is the validating one, and a configured host has already been
// through it before a request is served.
func canonicalHost(host string) string {
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
