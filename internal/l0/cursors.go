package l0

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// MaxConsumerLen bounds the name of a feed consumer. A consumer is a service,
// so the name is a word rather than an identifier of anything.
const MaxConsumerLen = 64

// Cursors is where the consumers of the change feed keep their positions.
//
// A position lives in the database rather than in the process because a
// consumer is stateless and is restarted, scaled and replaced: a cursor held in
// one would put every reader back at the beginning of the feed on every
// restart, and for the distiller that means re-distilling everything Hearsay
// has ever ingested — a model call per artifact.
//
// One row per consumer, not per replica. Two replicas of one service share a
// position and both move it forward; [Cursors.Save] never moves one backwards,
// so the slower of the two cannot pull the faster one back over ground it has
// already covered.
type Cursors struct {
	db Querier
}

// NewCursors returns the cursor store over a pool or a transaction on one.
// Saving in the caller's transaction is the point of the interface: a consumer
// commits the work it derived from a batch and its new position together, so a
// crash between the two cannot lose either.
func NewCursors(q Querier) *Cursors { return &Cursors{db: q} }

// Load is where a consumer had got to, and the beginning of the feed for one
// that has never saved a position.
func (c *Cursors) Load(ctx context.Context, consumer string) (Cursor, error) {
	if err := validConsumer(consumer); err != nil {
		return Cursor{}, err
	}
	var (
		xact string
		seq  int64
	)
	err := c.db.QueryRow(ctx,
		`SELECT xact_id::text, seq FROM l0_feed_cursors WHERE consumer = $1`, consumer,
	).Scan(&xact, &seq)
	if err != nil {
		if isNoRows(err) {
			return Cursor{}, nil
		}
		return Cursor{}, fmt.Errorf("reading the feed cursor of %s: %w", consumer, err)
	}
	cursor, err := cursorOf(xact, seq)
	if err != nil {
		return Cursor{}, fmt.Errorf("reading the feed cursor of %s: %w", consumer, err)
	}
	return cursor, nil
}

// saveSQL moves a consumer's position, and only forwards.
//
// The comparison is the one the feed itself orders by, on the same two columns,
// which is why the cursor is stored as its parts rather than as its text form:
// `'100.2' < '99.1'` is true as text and false as a position.
const saveSQL = `
INSERT INTO l0_feed_cursors (consumer, xact_id, seq)
VALUES ($1, $2::xid8, $3)
ON CONFLICT (consumer) DO UPDATE SET xact_id = excluded.xact_id, seq = excluded.seq, updated_at = now()
WHERE (l0_feed_cursors.xact_id, l0_feed_cursors.seq) < (excluded.xact_id, excluded.seq)
RETURNING consumer`

// Save records where a consumer has got to, and reports whether that moved the
// position. False means another replica of the same consumer is already further
// on and the row was left alone: a cursor only ever moves forward, so a reader
// that was overtaken cannot make the feed be re-read.
//
// Resetting a consumer is deleting its row, which is a deliberate operator
// action and not something a running process can do by accident.
func (c *Cursors) Save(ctx context.Context, consumer string, cursor Cursor) (bool, error) {
	if err := validConsumer(consumer); err != nil {
		return false, err
	}
	if cursor.IsZero() {
		// The zero cursor is the beginning of the feed. Saving it would be
		// asking to move backwards, which this never does, so it is a no-op
		// rather than a row that says a consumer has read nothing.
		return false, nil
	}
	var saved string
	err := c.db.QueryRow(ctx, saveSQL, consumer, strconv.FormatUint(cursor.xact, 10), cursor.seq).Scan(&saved)
	if err != nil {
		if isNoRows(err) {
			return false, nil
		}
		return false, fmt.Errorf("saving the feed cursor of %s: %w", consumer, err)
	}
	return true, nil
}

// validConsumer refuses a name the column cannot hold, before any SQL runs: a
// cursor is often saved inside a caller's transaction, and a statement error
// there would abort the work the cursor is being saved for.
func validConsumer(consumer string) error {
	switch {
	case consumer == "":
		return errors.New("a feed consumer has no name")
	case len(consumer) > MaxConsumerLen:
		return fmt.Errorf("feed consumer %q is longer than %d bytes", consumer, MaxConsumerLen)
	case !utf8.ValidString(consumer):
		return errors.New("a feed consumer's name is not valid UTF-8, which Postgres would refuse mid-transaction")
	case strings.IndexByte(consumer, 0) >= 0:
		return errors.New("a feed consumer's name holds a NUL byte, which a text column cannot store")
	}
	return nil
}
