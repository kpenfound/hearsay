package l0

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/kpenfound/hearsay/internal/connector"
)

// BackfillCursors is where the connector runtime keeps each source's position
// in its backfill. It implements [connector.CursorStore], the way [Store]
// implements [connector.Sink]: the runtime holds the interface and this is what
// is behind it in a process.
//
// One row per source, not per replica or per process. A backfill is a walk
// through one source's history and the walk belongs to the source: two
// processes hosting it share the position, and a restart resumes rather than
// reading the whole of a source's history again.
//
// Unlike [Cursors], this moves in whatever direction the connector says. A
// backfill cursor is opaque — only the connector can compare two of them — so
// there is no "forwards" to enforce here, and a connector that re-reads a page
// costs nothing: ingest is idempotent on the event id.
type BackfillCursors struct {
	db Querier
}

// NewBackfillCursors returns the store over a pool or a transaction on one.
func NewBackfillCursors(q Querier) *BackfillCursors { return &BackfillCursors{db: q} }

// Load is where the source's backfill had got to, and the zero state — the
// beginning of history — for a source that has never backfilled.
func (c *BackfillCursors) Load(ctx context.Context, source string) (connector.BackfillState, error) {
	if err := validBackfillSource(source); err != nil {
		return connector.BackfillState{}, err
	}
	var state connector.BackfillState
	err := c.db.QueryRow(ctx,
		`SELECT cursor, done, events FROM l0_backfill_cursors WHERE source = $1`, source,
	).Scan(&state.Cursor, &state.Done, &state.Events)
	if err != nil {
		if isNoRows(err) {
			return connector.BackfillState{}, nil
		}
		return connector.BackfillState{}, fmt.Errorf("reading the backfill cursor of source %s: %w", source, err)
	}
	return state, nil
}

// Save records where the backfill has got to. It is called after every call a
// connector's Backfill returns from, so the write is one statement and holds no
// transaction open.
func (c *BackfillCursors) Save(ctx context.Context, source string, state connector.BackfillState) error {
	if err := validBackfillSource(source); err != nil {
		return err
	}
	if err := validCursor(state.Cursor); err != nil {
		return fmt.Errorf("saving the backfill cursor of source %s: %w", source, err)
	}
	// RETURNING, and QueryRow, because a Querier is a pool or a transaction and
	// the read half is all the two have in common.
	var saved string
	err := c.db.QueryRow(ctx, `
INSERT INTO l0_backfill_cursors (source, cursor, done, events)
VALUES ($1, $2, $3, $4)
ON CONFLICT (source) DO UPDATE
SET cursor = excluded.cursor, done = excluded.done, events = excluded.events, updated_at = now()
RETURNING source`,
		source, string(state.Cursor), state.Done, state.Events).Scan(&saved)
	if err != nil {
		return fmt.Errorf("saving the backfill cursor of source %s: %w", source, err)
	}
	return nil
}

// validBackfillSource refuses a source id the column cannot hold, before any
// SQL runs.
func validBackfillSource(source string) error {
	if !connector.ValidSourceID(source) {
		return fmt.Errorf("%q is not a source id: lowercase letters, digits, - and _, up to %d bytes", source, connector.MaxSourceIDLen)
	}
	return nil
}

// validCursor refuses a cursor the column cannot hold. A cursor is opaque, so
// nothing here looks at what is in it — but it is stored as text, and a
// connector that puts raw bytes in one finds out here rather than by aborting a
// statement (docs/connector-contract.md).
func validCursor(cursor connector.Cursor) error {
	s := string(cursor)
	switch {
	case len(s) > connector.MaxCursorLen:
		return fmt.Errorf("the cursor is %d bytes, the limit is %d", len(s), connector.MaxCursorLen)
	case !utf8.ValidString(s):
		return errors.New("the cursor is not valid UTF-8, which a text column cannot store: encode it")
	case strings.IndexByte(s, 0) >= 0:
		return errors.New("the cursor holds a NUL byte, which a text column cannot store: encode it")
	}
	return nil
}
