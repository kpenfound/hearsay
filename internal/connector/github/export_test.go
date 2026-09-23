package github

import "time"

// SetPageSize sets the page size of every list call, so a test walks pages of
// a few items, and returns what restores it.
func SetPageSize(n int) (restore func()) {
	old := perPage
	perPage = n
	return func() { perPage = old }
}

// SetReadTimeout sets how long a delivery's REST reads may take, and returns
// what restores it.
func SetReadTimeout(d time.Duration) (restore func()) {
	old := readTimeout
	readTimeout = d
	return func() { readTimeout = old }
}
