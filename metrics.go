package main

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The exposition below is written by hand in the Prometheus text format 0.0.4,
// for the same reason the log lines are slog and the config is yaml: this
// project's point is that it needs nothing beyond the standard library, and the
// format is a handful of lines of text.
//
// Two rules decide what may be labelled, and both are about who chooses the
// value.
//
//   - No value taken from a request is ever a label. A URL, a path, a query, a
//     user agent or a remote address would each be a map keyed by something the
//     caller writes, which is a memory leak with an attacker-supplied size. Only
//     the config (host), this package's closed reason vocabulary, the engine's
//     HTTP status and the two guard kinds are labels.
//   - Host names are not exposed without a token. /metrics has to answer
//     unauthenticated - that is what a scrape usually is - and a Cloudflare
//     Tunnel makes it public, so the domain list stays behind the same site
//     token as /health?detail=1. Without one, every family is aggregated and the
//     host label is omitted rather than blanked.
const metricsContentType = "text/plain; version=0.0.4; charset=utf-8"

// reasonLabelAccepted is the label an accepted batch gets. Accepted is the empty
// string internally, because a response with no reason field means accepted; an
// empty label value would render as reason="" and read as a bug, so the case is
// spelled out here.
const reasonLabelAccepted = "accepted"

// engineDurationBuckets are the upper bounds of the engine latency histogram, in
// seconds. IndexNow does not publish its latency, so these span the range from a
// healthy answer (tens of milliseconds) to a request that very nearly hit
// engineTimeout. The bounds are a variable rather than a slice literal in the
// renderer so the histogram's storage and its `le` labels cannot disagree.
var engineDurationBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 15}

// engineHistogram is one host's engine latency distribution.
//
// counts is non-cumulative and has one entry per boundary plus one for everything
// past the last, because that is the shape one observation updates. The
// cumulative `le` buckets the format wants are summed at render time, so the
// buckets and the count cannot drift while a scrape reads them: the snapshot is
// already a copy by then.
type engineHistogram struct {
	counts []uint64
	sum    float64
	count  uint64
}

func newEngineHistogram() engineHistogram {
	return engineHistogram{counts: make([]uint64, len(engineDurationBuckets)+1)}
}

// observe records one engine call that took seconds.
func (h *engineHistogram) observe(seconds float64) {
	h.count++
	h.sum += seconds

	for i, upper := range engineDurationBuckets {
		if seconds <= upper {
			h.counts[i]++
			return
		}
	}

	h.counts[len(engineDurationBuckets)]++
}

// clone copies the distribution, slice included, so a snapshot can be rendered
// without holding the lock that updates it.
func (h engineHistogram) clone() engineHistogram {
	return engineHistogram{
		counts: append([]uint64(nil), h.counts...),
		sum:    h.sum,
		count:  h.count,
	}
}

// merge adds another host's distribution into this one, for the aggregated view.
func (h *engineHistogram) merge(other engineHistogram) {
	for i, count := range other.counts {
		if i < len(h.counts) {
			h.counts[i] += count
		}
	}

	h.sum += other.sum
	h.count += other.count
}

// hostMetrics is every counter kept for one configured host. Grouping them in
// one record means an update takes one lock and the exposition reads one
// consistent record per host.
type hostMetrics struct {
	submissions   int64
	reasons       map[string]int64
	engineStatus  map[string]int64
	duration      engineHistogram
	urlsSubmitted int64
	urlsSkipped   int64
	guardRefusals map[string]int64
}

func newHostMetrics() *hostMetrics {
	return &hostMetrics{
		reasons:       make(map[string]int64, 4),
		engineStatus:  make(map[string]int64, 4),
		duration:      newEngineHistogram(),
		guardRefusals: make(map[string]int64, 2),
	}
}

// clone copies the record deeply: the maps and the histogram slice are rebuilt,
// so nothing the renderer touches is shared with a request that is still
// counting.
func (h hostMetrics) clone() hostMetrics {
	cloned := hostMetrics{
		submissions:   h.submissions,
		reasons:       make(map[string]int64, len(h.reasons)),
		engineStatus:  make(map[string]int64, len(h.engineStatus)),
		duration:      h.duration.clone(),
		urlsSubmitted: h.urlsSubmitted,
		urlsSkipped:   h.urlsSkipped,
		guardRefusals: make(map[string]int64, len(h.guardRefusals)),
	}

	for reason, count := range h.reasons {
		cloned.reasons[reason] = count
	}
	for status, count := range h.engineStatus {
		cloned.engineStatus[status] = count
	}
	for kind, count := range h.guardRefusals {
		cloned.guardRefusals[kind] = count
	}

	return cloned
}

// merge adds another host's counters into this one.
func (h *hostMetrics) merge(other hostMetrics) {
	h.submissions += other.submissions
	for reason, count := range other.reasons {
		h.reasons[reason] += count
	}
	for status, count := range other.engineStatus {
		h.engineStatus[status] += count
	}
	h.duration.merge(other.duration)
	h.urlsSubmitted += other.urlsSubmitted
	h.urlsSkipped += other.urlsSkipped
	for kind, count := range other.guardRefusals {
		h.guardRefusals[kind] += count
	}
}

// submissionMetrics is what this instance has done, counted: the numbers that
// used to exist only as one JSON log line per submit.
//
// One mutex covers every map, so an update is atomic from the point of view of
// the exposition: a scrape never sees the submissions counter of a request whose
// reason has not been counted yet. It is never held across an engine call, and
// nothing in this file takes any other lock while holding it.
type submissionMetrics struct {
	mu    sync.Mutex
	hosts map[string]*hostMetrics
}

func newSubmissionMetrics() *submissionMetrics {
	return &submissionMetrics{hosts: make(map[string]*hostMetrics, 4)}
}

// hostFor returns one host's counters, creating them on first use. The caller
// holds the lock. The key is always a host from the config, never a value out of
// a request, so this map is bounded by the config.
func (m *submissionMetrics) hostFor(host string) *hostMetrics {
	counters, known := m.hosts[host]
	if !known {
		counters = newHostMetrics()
		m.hosts[host] = counters
	}

	return counters
}

// record accumulates one handled submission. engineCall is how long the engine
// took and invoked says whether a request was actually attempted, because the
// histogram has to take exactly one sample per engine call and none for a batch
// that never left this process.
//
// Every request this handler handled lands here exactly once, so submissions_total
// is the whole of what it answered - accepted, refused locally, or sent to the
// engine - and both the reason family and the guard family are breakdowns of that
// one count rather than totals of their own.
//
// A submission with no engine status contributes no engine series: reporting a
// status no engine sent would be the metrics version of the wrong engine_status
// in the response.
func (m *submissionMetrics) record(host string, result submissionResult, engineCall time.Duration, invoked bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.recordLocked(host, result, engineCall, invoked)
}

// recordLocked is record under a lock the caller already holds, for a caller that
// has a second counter to raise on the same record. One handler event is one lock
// acquisition, so a scrape never sees a submission whose guard kind has not been
// counted yet. It returns the host's counters so the caller adds to them under the
// lock it took rather than taking it again.
func (m *submissionMetrics) recordLocked(host string, result submissionResult, engineCall time.Duration, invoked bool) *hostMetrics {
	counters := m.hostFor(host)
	counters.submissions++
	counters.reasons[reasonLabel(result.Reason)]++
	counters.urlsSubmitted += int64(result.Submitted)
	counters.urlsSkipped += int64(result.Skipped)

	if result.EngineStatus != 0 {
		counters.engineStatus[strconv.Itoa(result.EngineStatus)]++
	}

	if invoked {
		counters.duration.observe(engineCall.Seconds())
	}

	return counters
}

// recordRefusal accumulates a submission refused before a result exists: the body
// could not be read, was too large, or did not parse. It is still a submission
// this instance handled, with no engine call behind it.
func (m *submissionMetrics) recordRefusal(host, reason string) {
	m.record(host, submissionResult{Reason: reason}, 0, false)
}

// recordGuardRefusal accumulates one submission the guard refused before the
// engine was called. The guard answered it, so it is one of the requests
// submissions_total counts, with the reason the caller sees: counting it only
// under guardRefusals would leave the submissions family missing exactly the
// throttled and overloaded traffic an operator opens /metrics to find, and would
// leave submissions and reasons as two numbers that do not add up.
//
// No engine status and no latency sample, because nothing was sent. Only the
// guard's two kinds are recorded here; they are the same strings as the reasons
// the caller sees, on purpose, because one refusal is one reason and one kind and
// reusing the constants keeps them from drifting apart.
func (m *submissionMetrics) recordGuardRefusal(host, kind string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	counters := m.recordLocked(host, submissionResult{Reason: kind}, 0, false)
	counters.guardRefusals[kind]++
}

// metricsSnapshot is one consistent reading of every counter: the families in a
// single scrape describe the same instant, which a per-family lock would not
// give.
type metricsSnapshot struct {
	hosts  []string
	byHost map[string]hostMetrics
}

// snapshot copies every counter under the lock, sorted by host so the exposition
// is stable from one scrape to the next.
func (m *submissionMetrics) snapshot() metricsSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()

	snapshot := metricsSnapshot{
		hosts:  make([]string, 0, len(m.hosts)),
		byHost: make(map[string]hostMetrics, len(m.hosts)),
	}

	for host, counters := range m.hosts {
		snapshot.hosts = append(snapshot.hosts, host)
		snapshot.byHost[host] = counters.clone()
	}
	sort.Strings(snapshot.hosts)

	return snapshot
}

// metricsSeries is one record to render: one host's counters, or the sum of
// every host when the caller has no token. An empty host means "no host label".
type metricsSeries struct {
	host   string
	record hostMetrics
}

// series returns what to render. With perHost every host is its own series; with
// no token they are summed into one series that carries no host label at all.
func (s metricsSnapshot) series(perHost bool) []metricsSeries {
	if perHost {
		series := make([]metricsSeries, 0, len(s.hosts))
		for _, host := range s.hosts {
			series = append(series, metricsSeries{host: host, record: s.byHost[host]})
		}

		return series
	}

	if len(s.hosts) == 0 {
		// Nothing has happened yet and there is no host to name: no series. The
		// families still carry their HELP and TYPE lines.
		return nil
	}

	total := newHostMetrics()
	for _, host := range s.hosts {
		total.merge(s.byHost[host])
	}

	return []metricsSeries{{record: *total}}
}

// label is one label pair. Labels are built as a slice so the host label can be
// absent entirely rather than empty.
type label struct {
	name  string
	value string
}

// labelValueEscaper escapes the three characters a label value may not carry
// raw: a backslash, a double quote and a newline.
var labelValueEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

// labelBlock renders a label set. An empty set renders nothing at all: `{}` is
// legal but says something different from "no labels" to a reader.
func labelBlock(labels []label) string {
	if len(labels) == 0 {
		return ""
	}

	var block strings.Builder
	block.WriteByte('{')

	for i, current := range labels {
		if i > 0 {
			block.WriteByte(',')
		}
		block.WriteString(current.name)
		block.WriteString(`="`)
		block.WriteString(labelValueEscaper.Replace(current.value))
		block.WriteByte('"')
	}

	block.WriteByte('}')

	return block.String()
}

// hostLabels is the host label for one series, or nothing at all when the host
// is unknown to the caller. The label is omitted rather than blanked, so an
// unauthenticated series is one series and not one per anonymous bucket.
func hostLabels(host string) []label {
	if host == "" {
		return nil
	}

	return []label{{name: "host", value: host}}
}

// exposition writes families in the Prometheus text format: a HELP line, a TYPE
// line and the family's series, in that order. Write errors are ignored, for the
// same reason writeJSON ignores them: a scrape that dies mid-body is a broken
// connection, not something a half-answer can fix.
type exposition struct {
	w io.Writer
}

// header writes one family's HELP and TYPE lines. The help text is a constant of
// this package with no backslash or newline in it - the two characters the
// format requires escaping - so it is written as it is.
func (e exposition) header(name, help, kind string) {
	_, _ = fmt.Fprintf(e.w, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
}

func (e exposition) sample(name, value string, labels ...label) {
	_, _ = fmt.Fprintf(e.w, "%s%s %s\n", name, labelBlock(labels), value)
}

// writeMetrics renders every family. keyStates comes from the server's own
// snapshot, so a host is present from startup rather than only after a decision,
// and the gauge always covers every configured site.
func writeMetrics(w io.Writer, snapshot metricsSnapshot, keyStates map[string]keyRecord, perHost bool) {
	series := snapshot.series(perHost)
	e := exposition{w: w}

	e.header("crawlping_submissions_total",
		"Every request this handler handled, per host - accepted, refused locally, or answered by the engine.", "counter")
	for _, current := range series {
		e.sample("crawlping_submissions_total", formatInt(current.record.submissions), hostLabels(current.host)...)
	}

	e.header("crawlping_submission_reasons_total",
		"Submissions handled, per host and reason; an accepted batch is labelled "+reasonLabelAccepted+".", "counter")
	for _, current := range series {
		for _, reason := range sortedCounters(current.record.reasons) {
			e.sample("crawlping_submission_reasons_total", formatInt(current.record.reasons[reason]),
				append(hostLabels(current.host), label{name: "reason", value: reason})...)
		}
	}

	e.header("crawlping_engine_requests_total",
		"Engine HTTP responses, per host and status. A batch refused here never reached the engine and has no series.",
		"counter")
	for _, current := range series {
		for _, status := range sortedCounters(current.record.engineStatus) {
			e.sample("crawlping_engine_requests_total", formatInt(current.record.engineStatus[status]),
				append(hostLabels(current.host), label{name: "status", value: status})...)
		}
	}

	e.header("crawlping_engine_duration_seconds", "Time one engine request took, per host.", "histogram")
	for _, current := range series {
		var cumulative uint64

		for i, upper := range engineDurationBuckets {
			cumulative += current.record.duration.counts[i]
			e.sample("crawlping_engine_duration_seconds_bucket", formatUint(cumulative),
				append(hostLabels(current.host), label{name: "le", value: formatFloat(upper)})...)
		}

		cumulative += current.record.duration.counts[len(engineDurationBuckets)]
		e.sample("crawlping_engine_duration_seconds_bucket", formatUint(cumulative),
			append(hostLabels(current.host), label{name: "le", value: "+Inf"})...)
		e.sample("crawlping_engine_duration_seconds_sum", formatFloat(current.record.duration.sum), hostLabels(current.host)...)
		e.sample("crawlping_engine_duration_seconds_count", formatUint(current.record.duration.count), hostLabels(current.host)...)
	}

	e.header("crawlping_urls_submitted_total", "URLs the engine accepted, per host.", "counter")
	for _, current := range series {
		e.sample("crawlping_urls_submitted_total", formatInt(current.record.urlsSubmitted), hostLabels(current.host)...)
	}

	e.header("crawlping_urls_skipped_total", "URLs dropped by the five-minute resubmission window, per host.", "counter")
	for _, current := range series {
		e.sample("crawlping_urls_skipped_total", formatInt(current.record.urlsSkipped), hostLabels(current.host)...)
	}

	e.header("crawlping_guard_refusals_total",
		"Locally refused requests, per host and kind - the per-kind breakdown of the ones already in crawlping_submissions_total, not a separate total.",
		"counter")
	for _, current := range series {
		for _, kind := range sortedCounters(current.record.guardRefusals) {
			e.sample("crawlping_guard_refusals_total", formatInt(current.record.guardRefusals[kind]),
				append(hostLabels(current.host), label{name: "kind", value: kind})...)
		}
	}

	e.header("crawlping_key_state", "Key state per host: 0 pending, 1 verified, 2 failed.", "gauge")
	writeKeyStates(e, keyStates, perHost)
}

// writeKeyStates renders the gauge. With a token it is one series per host.
// Without one it is a count per state, because the sum of state numbers would be
// a number nothing can interpret - and the count per state is the same shape
// /health already reports unauthenticated.
func writeKeyStates(e exposition, keyStates map[string]keyRecord, perHost bool) {
	if perHost {
		hosts := make([]string, 0, len(keyStates))
		for host := range keyStates {
			hosts = append(hosts, host)
		}
		sort.Strings(hosts)

		for _, host := range hosts {
			e.sample("crawlping_key_state", formatInt(int64(keyStateGauge(keyStates[host].State))),
				label{name: "host", value: host})
		}

		return
	}

	for _, state := range []keyState{keyStatePending, keyStateVerified, keyStateFailed} {
		count := 0
		for _, record := range keyStates {
			if record.State == state {
				count++
			}
		}

		e.sample("crawlping_key_state", formatInt(int64(count)), label{name: "state", value: string(state)})
	}
}

// keyStateGauge is the number the gauge reports for a state: 0 pending, 1
// verified, 2 failed. A number rather than a label keeps the series set fixed,
// and the order is the order of increasing knowledge.
func keyStateGauge(state keyState) int {
	switch state {
	case keyStateVerified:
		return 1
	case keyStateFailed:
		return 2
	default:
		return 0
	}
}

// reasonLabel turns a submission's reason into its label value. The reason
// vocabulary is closed in this package, so this cannot produce an unbounded
// label: it is a mapping, not a filter.
func reasonLabel(reason string) string {
	if reason == reasonAccepted {
		return reasonLabelAccepted
	}

	return reason
}

// engineInvoked reports whether a result involved a request to the engine, which
// is what decides whether the latency histogram takes a sample.
//
// It is read from the result rather than reported by the client because the
// client is not the place these counters live. A status means the engine
// answered; engine_error with no status means the request was attempted and never
// landed, which is exactly the latency worth seeing. Every other status-free
// outcome - a URL refused, a batch over the limit, one the resubmission window
// emptied - never left this process.
//
// The one imprecision is a client that fails before sending at all - a payload
// that will not marshal, an endpoint that will not parse - which lands in the
// same bucket as a call that never landed. Neither can happen with a validated
// config, and a sample taken for one would only ever add a zero to the histogram.
func engineInvoked(result submissionResult) bool {
	if result.EngineStatus != 0 {
		return true
	}

	return result.Reason == reasonEngineError
}

// sortedCounters returns a map's keys in order, so the exposition is byte-for-byte
// stable between scrapes that saw the same counts.
func sortedCounters(counters map[string]int64) []string {
	keys := make([]string, 0, len(counters))
	for key := range counters {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	return keys
}

func formatInt(value int64) string {
	return strconv.FormatInt(value, 10)
}

func formatUint(value uint64) string {
	return strconv.FormatUint(value, 10)
}

// formatFloat is the shortest representation that round-trips, which is what the
// format wants for a sum and for a bucket bound: 0.05 stays "0.05" and 2.5 stays
// "2.5", rather than being padded or truncated.
func formatFloat(value float64) string {
	return strconv.FormatFloat(value, 'g', -1, 64)
}
