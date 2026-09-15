package github

import "time"

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
