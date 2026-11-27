package main

// `crawlping doctor` answers the question `check` deliberately does not: is this
// deployment usable, not just is each key file right. It exists because a
// deployment that looks fine can fail in ways that are invisible from inside:
// the key file is served by the site's fallback page (a 200 that is HTML), the
// config file sits at 0644 so every token in it is readable by anyone on the
// box, the port the config names is held by something else, or the machine
// cannot reach the engine at all.
//
// It is read-only on purpose. Every check here is a fact about the machine it
// runs on, and nothing is submitted: doctor must be safe to run on a production
// instance, at any time, without spending crawl quota or the operator's egress
// reputation.

import (
	"context"
	"crypto/tls"

	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	// doctorDialTimeout bounds one connection attempt. Short, because doctor is
	// usually run while a deployment is being debugged and a three-minute wait
	// per check is not an answer.
	doctorDialTimeout = 5 * time.Second
)

func cmdDoctor(args []string) error {
	flags := flag.NewFlagSet("doctor", flag.ExitOnError)
	configPath := flags.String("config", defaultConfigPath(), "path to config.yaml")
	_ = flags.Parse(args)

	return runDoctor(os.Stdout, *configPath)
}

// doctorReport counts the outcomes so the exit status can describe them the way
// `check` does, keeping "this is broken" apart from "this could not be tested".
type doctorReport struct {
	failed  int
	unknown int
}

func (r *doctorReport) ok(out io.Writer, label, format string, args ...any) {
	fmt.Fprintf(out, "ok    %-8s %s\n", label, fmt.Sprintf(format, args...))
}

func (r *doctorReport) fail(out io.Writer, label, format string, args ...any) {
	r.failed++
	fmt.Fprintf(out, "FAIL  %-8s %s\n", label, fmt.Sprintf(format, args...))
}

func (r *doctorReport) unknownOutcome(out io.Writer, label, format string, args ...any) {
	r.unknown++
	fmt.Fprintf(out, "????  %-8s %s\n", label, fmt.Sprintf(format, args...))
}

// note prints the second line of a result: the sentence that says what to do
// about it. It is indented to the label column so a result and its explanation
// read as one block, which is how `check` already prints a failure.
func (r *doctorReport) note(out io.Writer, format string, args ...any) {
	fmt.Fprintf(out, "      %s\n", fmt.Sprintf(format, args...))
}

func runDoctor(out io.Writer, configPath string) error {
	// The config first: every other check needs it, and a config the service
	// would refuse to start on makes the rest of this output noise.
	cfg, err := loadConfig(configPath)
	if err != nil {
		report := &doctorReport{}
		report.fail(out, "config", "%s does not load: %v", configPath, err)
		report.note(out, "the service would exit at startup with this file; fix it before anything else")
		return fmt.Errorf("config: %w", err)
	}

	return doctorConfig(out, cfg, configPath)
}

// doctorConfig runs every check against a config that has already loaded. It is
// separate from runDoctor so a test can point a loaded config at a local origin:
// the host check in loadConfig refuses 127.0.0.1, and rightly so, which means the
// only way to exercise these checks is to hand them a Config rather than a path.
func doctorConfig(out io.Writer, cfg *Config, configPath string) error {
	report := &doctorReport{}
	report.ok(out, "config", "%s, %d sites, endpoint %s", configPath, len(cfg.Sites), cfg.IndexNowEndpoint)

	// Only the path, but it belongs between the config that loaded and the checks
	// that need the machine: the order of the report is only readable here.
	checkConfigMode(report, out, configPath)

	checkListen(report, out, cfg)
	checkEngine(report, out, cfg.IndexNowEndpoint)
	checkKeyFiles(report, out, cfg)

	switch {
	case report.failed > 0 && report.unknown > 0:
		return fmt.Errorf("%d checks failed and %d could not be decided", report.failed, report.unknown)
	case report.failed > 0:
		return fmt.Errorf("%d checks failed", report.failed)
	case report.unknown > 0:
		return fmt.Errorf("%d checks could not be decided", report.unknown)
	}

	return nil
}

// checkConfigMode reports who else can read the file every site's token is in.
//
// init and site add write it 0600 and the README's Docker story depends on that,
// but nothing ever checked it: an editor that replaces the file, a cp from a
// backup or a chmod -R leaves it 0644, and both serve and doctor read it as if
// nothing happened. Only the group and other bits are judged, because 0400 and
// 0700 are private too and failing them would be wrong.
func checkConfigMode(report *doctorReport, out io.Writer, path string) {
	info, err := os.Stat(path)
	if err != nil {
		// loadConfig has already opened this file, so a stat that fails here says
		// nothing about its mode; doctor does not print a line for a file it was
		// never given.
		return
	}

	perm := info.Mode().Perm()
	if perm&0o077 == 0 {
		report.ok(out, "mode", "%04o, readable only by the owner", perm)
		return
	}

	report.fail(out, "mode", "%04o, so group or other has access to the config", perm)
	report.note(out, "run chmod 600 %s: the file holds every site's bearer token", path)
}

// checkListen reports whether the configured address can be bound.
//
// The honest limit, stated here rather than discovered later: this describes the
// network namespace doctor runs in. In a one-shot container that is the
// container's, not the host's, so a busy host port can still look free. Point
// doctor at the running instance instead - the second branch below is the one
// that matters in production, and it asks the process rather than the kernel.
func checkListen(report *doctorReport, out io.Writer, cfg *Config) {
	listener, err := net.Listen("tcp", cfg.Listen)
	if err == nil {
		listener.Close()
		report.ok(out, "listen", "%s is free", cfg.Listen)
		return
	}

	// A port already held by this instance is not a problem, and it is the
	// normal state of a deployed instance. Ask the process on that address
	// rather than concluding from the bind failure.
	client := &http.Client{Timeout: doctorDialTimeout}
	resp, healthErr := client.Get(localHealthURL(cfg.Listen))
	if healthErr == nil {
		resp.Body.Close()
		if resp.StatusCode == 200 {
			report.ok(out, "listen", "%s is answering /health 200; the address is held by this instance", cfg.Listen)
			return
		}
		report.fail(out, "listen", "%s answers /health %d, so it is not crawlping", cfg.Listen, resp.StatusCode)
		return
	}

	report.fail(out, "listen", "cannot bind %s: %v", cfg.Listen, err)
	report.note(out, "another process holds the address; change listen: in the config, or stop that process")
}

// checkEngine proves the machine can reach the engine, without submitting
// anything: DNS, TCP and a TLS handshake. The request that matters is a POST the
// caller makes, and a doctor that posted a probe URL would be spending the
// operator's crawl quota to answer a question about reachability.
func checkEngine(report *doctorReport, out io.Writer, endpoint string) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" {
		report.fail(out, "engine", "indexnow_endpoint %q is not a URL", hint(endpoint, maxValueHint))
		return
	}

	port := parsed.Port()
	if port == "" {
		port = "443"
		if parsed.Scheme == "http" {
			port = "80"
		}
	}
	address := net.JoinHostPort(parsed.Hostname(), port)

	start := time.Now()
	dialer := &net.Dialer{Timeout: doctorDialTimeout}
	conn, err := dialer.Dial("tcp", address)
	if err != nil {
		report.fail(out, "engine", "cannot reach %s: %v", address, err)
		report.note(out, "submissions cannot leave this machine; check its egress and DNS")
		return
	}
	defer conn.Close()

	if parsed.Scheme == "https" {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: parsed.Hostname(), MinVersion: tls.VersionTLS12})
		if err := tlsConn.Handshake(); err != nil {
			report.fail(out, "engine", "TLS to %s failed: %v", address, err)
			return
		}
	}

	report.ok(out, "engine", "%s reachable in %s", address, time.Since(start).Round(time.Millisecond))
}

func checkKeyFiles(report *doctorReport, out io.Writer, cfg *Config) {
	client := newIndexNowClient(cfg.IndexNowEndpoint)

	for _, site := range cfg.Sites {
		ctx, cancel := context.WithTimeout(context.Background(), checkSiteTimeout)
		probe, err := client.probeKeyFile(ctx, site)
		cancel()

		switch {
		case err != nil:
			// Never read, so nothing is known. The exit status keeps this apart
			// from a key file that was read and refused.
			report.unknownOutcome(out, "key", "%s %s", site.Host, site.KeyLocation)
			report.note(out, "could not determine: %v", err)

		case probe.Status != 200:
			report.fail(out, "key", "%s %s", site.Host, site.KeyLocation)
			report.note(out, "the origin answered %d; the key file has to be served with 200", probe.Status)

		case probe.matches(site.IndexNowKey):
			report.ok(out, "key", "%s %s", site.Host, site.KeyLocation)

		case looksLikeHTML(probe):
			// The failure that looks like success, and the reason doctor exists
			// next to check: a static site with a fallback route answers 200 for
			// every path, so "curl it and see 200" is not evidence. The engine
			// will read this page and refuse the key.
			report.fail(out, "key", "%s %s", site.Host, site.KeyLocation)
			report.note(out, "200 but %s, which is a page, not a key file", describeBody(probe))
			report.note(out, "the site is answering for a path it does not have; publish %s.txt at the host root", site.IndexNowKey)

		default:
			report.fail(out, "key", "%s %s", site.Host, site.KeyLocation)
			report.note(out, "the file was served but holds something else: %s", describeBody(probe))
			report.note(out, "its content has to be the key itself, %s", site.IndexNowKey)
		}
	}
}

// looksLikeHTML is the check that separates a real key file from a site's
// catch-all route. Content-Type is the site's own claim, so the body is
// inspected too: a fallback page is HTML whatever it is served as.
func looksLikeHTML(probe keyFileProbe) bool {
	if strings.Contains(strings.ToLower(probe.ContentType), "html") {
		return true
	}

	head := strings.ToLower(strings.TrimSpace(string(probe.Body)))
	return strings.HasPrefix(head, "<!doctype html") || strings.HasPrefix(head, "<html")
}

// describeBody says what was served in one line: its type and size, which is
// usually enough to recognise a fallback page without a second request.
func describeBody(probe keyFileProbe) string {
	contentType := probe.ContentType
	if contentType == "" {
		contentType = "no content-type"
	}

	return fmt.Sprintf("%s, %d bytes", contentType, len(probe.Body))
}
