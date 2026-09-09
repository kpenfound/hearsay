package l0

import (
	"fmt"
	"strconv"
	"strings"
)

// Cursor is a reader's position in the change feed. It is opaque to whoever
// holds it — a distiller stores the string and hands it back — and the zero
// value is the beginning of the feed.
//
// It is a pair rather than a single number because a sequence value is handed
// out before the transaction that took it commits, so sequence order is not the
// order rows become readable. See the comment on l0_events in migration 2.
type Cursor struct {
	// xact is the transaction that wrote the row, from Postgres's xid8.
	xact uint64
	// seq is the row's position within that transaction.
	seq int64
}

// String is the cursor's wire form, `<xact>.<seq>`, and the empty string for
// the zero cursor so that "no cursor yet" and "the beginning" are one value.
func (c Cursor) String() string {
	if c == (Cursor{}) {
		return ""
	}
	return strconv.FormatUint(c.xact, 10) + "." + strconv.FormatInt(c.seq, 10)
}

// IsZero reports whether the cursor is the beginning of the feed.
func (c Cursor) IsZero() bool { return c == Cursor{} }

// ParseCursor reads back what [Cursor.String] wrote. The empty string is the
// zero cursor.
func ParseCursor(s string) (Cursor, error) {
	if s == "" {
		return Cursor{}, nil
	}
	xact, seq, ok := strings.Cut(s, ".")
	if !ok {
		return Cursor{}, fmt.Errorf("cursor %q: want <xact>.<seq>", s)
	}
	x, err := strconv.ParseUint(xact, 10, 64)
	if err != nil {
		return Cursor{}, fmt.Errorf("cursor %q: transaction id: %w", s, err)
	}
	n, err := strconv.ParseInt(seq, 10, 64)
	if err != nil {
		return Cursor{}, fmt.Errorf("cursor %q: sequence: %w", s, err)
	}
	if x == 0 || n < 0 {
		return Cursor{}, fmt.Errorf("cursor %q: is not a position in the feed", s)
	}
	return Cursor{xact: x, seq: n}, nil
}
