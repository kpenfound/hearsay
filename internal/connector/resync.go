package connector

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"
)

// ErrNoResyncStore is returned by a sink asked to record a re-sync when the
// runtime behind it has nowhere durable to keep one.
var ErrNoResyncStore = errors.New("the runtime has no store to record a re-sync in")

// Resyncer is a connector whose containers can stop being public without any
// artifact in them changing: a repository or a channel made private. Every
// artifact already in L0 carries the `public` ACL it was ingested with, so each
// has to be emitted again under the ACL it has now (docs/connector-contract.md,
// "When only the access list changes").
//
// The runtime drives a re-sync the way it drives a backfill, one bounded call
// at a time with a cursor it keeps durably, so a re-sync interrupted by a
// restart resumes. What says a container owes one is durable too, and comes
// from two places: a [Pusher] told that a container went private records it
// through [ResyncRequester], and at startup the runtime asks Public about every
// container L0 still serves as public, which catches a change no delivery ever
// reported to a running process.
type Resyncer interface {
	Connector
	// Public reports whether a container is readable by everyone at the source
	// now. It is a network call, made at startup per container L0 serves as
	// public, and never on the health path.
	Public(ctx context.Context, container string) (bool, error)
	// Resync emits one bounded piece of a container's artifacts again, under
	// the access list they have now, from the cursor the last call returned.
	// The first call gets the zero [Cursor]. Its result means what a
	// [Backfiller]'s does, for one container.
	Resync(ctx context.Context, sink Sink, container string, from Cursor) (BackfillResult, error)
}

// ResyncRequester is implemented by the sink the runtime hands a connector:
// RequestResync records, durably, that a container owes a re-sync, and wakes
// the runtime to run it. It returns once the record is written, so a push
// handler answers the source only after the obligation has outlived the
// request.
type ResyncRequester interface {
	RequestResync(ctx context.Context, container string) error
}

// Resync is the runtime's record of one container's re-sync.
type Resync struct {
	Container string
	// Owed reports that a re-sync is owed or under way.
	Owed bool
	// Cursor is where the owed re-sync has got to. The zero cursor is its
	// beginning.
	Cursor Cursor
	// Generation counts the requests. A request starts the walk again from
	// the beginning, and saving or finishing a walk compares it, so a walk that
	// began before a request can neither move the new walk's cursor nor settle
	// it.
	Generation int64
	// ResyncedAt is when the last re-sync of the container finished; zero is
	// never.
	ResyncedAt time.Time
}

// ResyncStore is where the runtime keeps re-syncs, one record per container of
// a source. It lives outside the process for the reason [CursorStore] does:
// what is owed, and how far a walk got, must survive a restart. The
// implementation is [github.com/kpenfound/hearsay/internal/l0.Resyncs]; a test
// uses [MemoryResyncs].
type ResyncStore interface {
	// Owe records that a container owes a re-sync, from the beginning, and
	// counts one more request. A walk already under way is started again: part
	// of it may have read the container before it changed.
	Owe(ctx context.Context, source, container string) error
	// Resyncs is every container of the source with a record, owed or not, in
	// container order.
	Resyncs(ctx context.Context, source string) ([]Resync, error)
	// Save records r.Cursor as where the walk of r.Container has got to. A
	// record that owes nothing, or whose generation is no longer r.Generation,
	// is left alone: another replica finished the walk, or a request started
	// it again.
	Save(ctx context.Context, source string, r Resync) error
	// Finish records that the walk r describes has reached the end. It reports
	// true when that settles the debt. When a request arrived since r was
	// read, the re-sync stays owed, from the beginning, and it reports false.
	Finish(ctx context.Context, source string, r Resync) (bool, error)
}

// Exposure is one container a source still holds public artifacts of.
type Exposure struct {
	Container string
	// LastPublic is when the newest of those artifacts' current revisions was
	// ingested. The runtime compares it with the container's last finished
	// re-sync: what was public before that walk and still is afterwards is an
	// artifact the walk could not reach, and walking again would not reach it
	// either.
	LastPublic time.Time
}

// ExposureReader answers which containers of a source L0 still serves as
// public. It is [github.com/kpenfound/hearsay/internal/l0.Store] in a process
// and [Recorder] in a test.
type ExposureReader interface {
	Exposed(ctx context.Context, source string) ([]Exposure, error)
}

// MemoryResyncs is a [ResyncStore] in memory, for a test that has no database.
// It is safe for concurrent use.
type MemoryResyncs struct {
	// err is what every method returns instead of reading or writing, set by
	// SetErr.
	err error

	mu    sync.Mutex
	state map[string]map[string]Resync
}

// NewMemoryResyncs returns an empty store.
func NewMemoryResyncs() *MemoryResyncs {
	return &MemoryResyncs{state: map[string]map[string]Resync{}}
}

// SetErr makes every later call fail with err, or work again with nil.
func (m *MemoryResyncs) SetErr(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.err = err
}

// Owe implements [ResyncStore].
func (m *MemoryResyncs) Owe(_ context.Context, source, container string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	if m.state[source] == nil {
		m.state[source] = map[string]Resync{}
	}
	r := m.state[source][container]
	r.Container = container
	r.Cursor = ""
	r.Owed = true
	r.Generation++
	m.state[source][container] = r
	return nil
}

// Resyncs implements [ResyncStore].
func (m *MemoryResyncs) Resyncs(_ context.Context, source string) ([]Resync, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	out := []Resync{}
	for _, r := range m.state[source] {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b Resync) int { return strings.Compare(a.Container, b.Container) })
	return out, nil
}

// Save implements [ResyncStore].
func (m *MemoryResyncs) Save(_ context.Context, source string, walk Resync) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	r, ok := m.state[source][walk.Container]
	if !ok || !r.Owed || r.Generation != walk.Generation {
		return nil
	}
	r.Cursor = walk.Cursor
	m.state[source][walk.Container] = r
	return nil
}

// Finish implements [ResyncStore].
func (m *MemoryResyncs) Finish(_ context.Context, source string, done Resync) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return false, m.err
	}
	r, ok := m.state[source][done.Container]
	if !ok {
		return true, nil
	}
	r.Cursor = ""
	finished := r.Generation == done.Generation
	if finished {
		r.Owed = false
		r.ResyncedAt = time.Now()
	}
	m.state[source][done.Container] = r
	return finished, nil
}

// Get is the record of one container, and the zero record for one with none.
func (m *MemoryResyncs) Get(source, container string) Resync {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state[source][container]
}

// Set puts a record in the store, which is how a test says "a process was
// interrupted here" before starting a runtime.
func (m *MemoryResyncs) Set(source string, r Resync) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state[source] == nil {
		m.state[source] = map[string]Resync{}
	}
	m.state[source][r.Container] = r
}
