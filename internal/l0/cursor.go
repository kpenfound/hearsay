package l0

import (
	"fmt"
	"slices"
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

// Compare orders two cursors as the feed does: -1 when c is before other, 0
// when they are the same position, and 1 when c is after it.
func (c Cursor) Compare(other Cursor) int {
	switch {
	case c.xact < other.xact || c.xact == other.xact && c.seq < other.seq:
		return -1
	case c == other:
		return 0
	default:
		return 1
	}
}

// Mark is a moment on the change feed, as [Store.Now] takes it: the cursor to
// read the feed from, and the transactions after it that had already committed
// at that moment, whose events a reader passes over. It is opaque to whoever
// holds it, as a [Cursor] is, and the zero value is the beginning of the feed.
type Mark struct {
	// From is where the feed is read from.
	From Cursor
	// committed are transactions after From that had committed at the mark,
	// in order.
	committed []uint64
}

// Committed reports whether the event at c had committed when the mark was
// taken, and so is not new to a reader who started at the mark.
func (m Mark) Committed(c Cursor) bool {
	_, found := slices.BinarySearch(m.committed, c.xact)
	return found
}

// Past is the mark once its reader has read the feed to c: the feed is read
// from c, and the committed transactions wholly behind it are forgotten, which
// they all are once the reader has passed the last of them.
func (m Mark) Past(c Cursor) Mark {
	i, _ := slices.BinarySearch(m.committed, c.xact)
	return Mark{From: c, committed: slices.Clone(m.committed[i:])}
}

// String is the mark's wire form: its cursor's, then `+` and the committed
// transactions, comma-separated, when it lists any.
func (m Mark) String() string {
	if len(m.committed) == 0 {
		return m.From.String()
	}
	ids := make([]string, len(m.committed))
	for i, x := range m.committed {
		ids[i] = strconv.FormatUint(x, 10)
	}
	return m.From.String() + "+" + strings.Join(ids, ",")
}

// ParseMark reads back what [Mark.String] wrote.
func ParseMark(s string) (Mark, error) {
	cursor, list, listed := strings.Cut(s, "+")
	from, err := ParseCursor(cursor)
	if err != nil {
		return Mark{}, err
	}
	m := Mark{From: from}
	if !listed {
		return m, nil
	}
	for _, x := range strings.Split(list, ",") {
		id, err := strconv.ParseUint(x, 10, 64)
		if err != nil || id < from.xact || len(m.committed) > 0 && id <= m.committed[len(m.committed)-1] {
			return Mark{}, fmt.Errorf("mark %q: transaction %q is before the cursor or not after the one before it", s, x)
		}
		m.committed = append(m.committed, id)
	}
	if len(m.committed) > MaxMarkCommitted {
		return Mark{}, fmt.Errorf("mark %q: lists more than %d transactions", s, MaxMarkCommitted)
	}
	return m, nil
}
