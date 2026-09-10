package connector

import (
	"context"
	"sync"
)

// BackfillState is where a source's backfill has got to: what the runtime
// stores between one [Backfiller.Backfill] call and the next, and across a
// restart.
//
// It is the runtime's, not the connector's. A connector returns a
// [BackfillResult] and is told nothing about how it is kept.
type BackfillState struct {
	// Cursor is what the next call gets. The zero cursor is the beginning of
	// history, which is what a source that has never been backfilled gets.
	Cursor Cursor
	// Done reports that history is exhausted. A source whose backfill is done
	// is not driven again, so this is what stops a restart from walking the
	// whole of a source's history a second time.
	Done bool
	// Events is how many events the backfill has emitted in total, for progress
	// reporting. It is the connector's own count summed over the calls, so it
	// is what the connector claims rather than what reached L0.
	Events int64
}

// CursorStore is where the runtime keeps backfill positions. One row per
// source: a backfill is a walk through one source's history, and two replicas
// hosting the same source share the walk.
//
// It lives outside the process because a connector is restarted, scaled and
// replaced: a position held in one would start every source's history again on
// every deploy, and history is the expensive half of ingest. The implementation
// is [github.com/kpenfound/hearsay/internal/l0.BackfillCursors]; a test uses
// [MemoryCursors].
type CursorStore interface {
	// Load is where the source's backfill had got to. A source that has never
	// been backfilled gets the zero [BackfillState], whose cursor is the
	// beginning of history.
	Load(ctx context.Context, source string) (BackfillState, error)
	// Save records where the backfill has got to. It is called after every
	// Backfill call, so that a restart resumes rather than starting again.
	Save(ctx context.Context, source string, state BackfillState) error
}

// MemoryCursors is a [CursorStore] that keeps positions in memory, for a test
// that has no database. It records every save, because "the position is
// persisted after every call" is the property that makes a restart resume and
// there is otherwise nothing to assert it against.
//
// It is safe for concurrent use.
type MemoryCursors struct {
	// LoadErr, if set, is what Load returns instead of a position.
	LoadErr error
	// SaveErr, if set, is what Save returns instead of storing one.
	SaveErr error

	mu    sync.Mutex
	state map[string]BackfillState
	saves map[string][]BackfillState
}

// NewMemoryCursors returns an empty store.
func NewMemoryCursors() *MemoryCursors {
	return &MemoryCursors{state: map[string]BackfillState{}, saves: map[string][]BackfillState{}}
}

// Load implements [CursorStore].
func (m *MemoryCursors) Load(_ context.Context, source string) (BackfillState, error) {
	if m.LoadErr != nil {
		return BackfillState{}, m.LoadErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state[source], nil
}

// Save implements [CursorStore].
func (m *MemoryCursors) Save(_ context.Context, source string, state BackfillState) error {
	if m.SaveErr != nil {
		return m.SaveErr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state[source] = state
	m.saves[source] = append(m.saves[source], state)
	return nil
}

// Set puts a position in the store without recording a save, which is how a
// test says "this source was interrupted here" before starting a runtime.
func (m *MemoryCursors) Set(source string, state BackfillState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state[source] = state
}

// Saves is every position saved for a source, oldest first.
func (m *MemoryCursors) Saves(source string) []BackfillState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]BackfillState(nil), m.saves[source]...)
}
