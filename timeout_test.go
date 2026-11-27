package main

import "testing"

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
