package github

import "time"

func SetPageSize(n int) (restore func()) {
	old := perPage
	perPage = n
	return func() { perPage = old }
}

func SetReadTimeout(d time.Duration) (restore func()) {
	old := readTimeout
	readTimeout = d
	return func() { readTimeout = old }
}
