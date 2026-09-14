package github

import "time"

// SetResyncRetry shortens the first wait between re-sync attempts for a test,
// and returns what restores it. Set it before the connector starts a re-sync,
// and restore it after the connector is closed.
func SetResyncRetry(d time.Duration) (restore func()) {
	old := resyncRetry
	resyncRetry = d
	return func() { resyncRetry = old }
}
