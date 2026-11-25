// Package poll waits in tests for a condition that nothing signals: state in
// Postgres, a counter, a health report.
package poll

import (
	"testing"
	"time"
)

const (
	every = time.Millisecond

	// Bound is how long Until waits. It only fails a test that is already
	// broken, so it is generous.
	Bound = 20 * time.Second
)

// Check reports whether cond held within the given time, trying it every
// millisecond.
func Check(within time.Duration, cond func() bool) bool {
	if cond() {
		return true
	}
	deadline := time.NewTimer(within)
	defer deadline.Stop()
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-deadline.C:
			return false
		case <-tick.C:
			if cond() {
				return true
			}
		}
	}
}

// Within fails the test unless cond holds within the given time.
func Within(t testing.TB, what string, within time.Duration, cond func() bool) {
	t.Helper()
	if !Check(within, cond) {
		t.Fatalf("timed out after %s waiting for %s", within, what)
	}
}

// Until fails the test unless cond holds within [Bound].
func Until(t testing.TB, what string, cond func() bool) {
	t.Helper()
	Within(t, what, Bound, cond)
}
