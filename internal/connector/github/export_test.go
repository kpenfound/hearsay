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

// SetPageSize sets the page size of every list call, so a test walks pages of
// a few items, and returns what restores it.
func SetPageSize(n int) (restore func()) {
	old := perPage
	perPage = n
	return func() { perPage = old }
}

// SetPushReadTimeout sets how long a push delivery's REST read may take, and
// returns what restores it.
func SetPushReadTimeout(d time.Duration) (restore func()) {
	old := pushReadTimeout
	pushReadTimeout = d
	return func() { pushReadTimeout = old }
}
