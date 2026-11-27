package main

import (
	"strings"
	"testing"
)

// The write budget has to outlast the worst case it wraps. WriteTimeout starts
// when the request headers are done, so the body read, the wait for an engine
// slot and the engine call all happen inside it. When it is shorter than their
// sum the connection is closed mid-write, and an answer the caller can act on -
// overloaded, engine_error - arrives as nothing at all instead.

func TestServerWriteTimeoutCoversWorstCase(t *testing.T) {
	worst := serverReadTimeout + inFlightWait + engineTimeout

	if serverWriteTimeout <= worst {
		t.Fatalf("serverWriteTimeout = %s does not cover the worst case: serverReadTimeout (%s) + inFlightWait (%s) + engineTimeout (%s) = %s; raise serverWriteTimeout in main.go above that sum",
			serverWriteTimeout, serverReadTimeout, inFlightWait, engineTimeout, worst)
	}

	// The body is read inside both budgets, so a write budget at or below the
	// read budget could expire before the body is even in hand.
	if serverWriteTimeout <= serverReadTimeout {
		t.Fatalf("serverWriteTimeout = %s is not longer than serverReadTimeout = %s",
			serverWriteTimeout, serverReadTimeout)
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
	text := "init serve check healthcheck version"
	for _, command := range strings.Fields(text) {
		if !strings.Contains(usageText(), command) {
			t.Fatalf("usage text does not mention %q", command)
		}
	}
}
