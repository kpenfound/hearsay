// Package logtest captures log output in a test.
package logtest

import (
	"bytes"
	"sync"
	"time"
)

// Buffer is a writer that is safe to read while a service under test writes to
// it.
type Buffer struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	written chan struct{}
}

// Write implements [io.Writer].
func (b *Buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.written != nil {
		close(b.written)
		b.written = nil
	}
	return b.buf.Write(p)
}

// String is everything written so far.
func (b *Buffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// WaitFor reports whether holds is true of the output within the given time,
// rechecking after every write.
func (b *Buffer) WaitFor(within time.Duration, holds func(string) bool) bool {
	timeout := time.NewTimer(within)
	defer timeout.Stop()
	for {
		b.mu.Lock()
		if holds(b.buf.String()) {
			b.mu.Unlock()
			return true
		}
		if b.written == nil {
			b.written = make(chan struct{})
		}
		written := b.written
		b.mu.Unlock()
		select {
		case <-written:
		case <-timeout.C:
			return false
		}
	}
}
