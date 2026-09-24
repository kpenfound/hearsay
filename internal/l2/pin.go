package l2

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Pin is a person saying that one L1 document is an anchor of a scope
// (docs/design.md#anchors). It is a record, like a stance: nothing re-derives
// it, and it outlives the document it names being distilled again. A pin
// gesture writes one through [Store.Pin], and its undo takes it out
// ([RecordGesture]).
type Pin struct {
	// Scope is the entity id the document anchors.
	Scope string
	// L1 is the pinned document's id.
	L1 string
	// PinnedBy is the principal id of whoever pinned it.
	PinnedBy string
	// PinnedAt is when, and it is what orders a scope's pins: first pinned
	// first, so a later pin never pushes an earlier one out of the cap.
	PinnedAt time.Time
}

// Validate reports a pin the store refuses.
func (p Pin) Validate() error {
	switch {
	case p.Scope == "":
		return fmt.Errorf("%w: a pin has no scope", ErrInvalid)
	case !strings.HasPrefix(p.L1, "l1:"):
		return fmt.Errorf("%w: the pin on %s names %q, which is not a document id", ErrInvalid, p.Scope, p.L1)
	case p.PinnedBy == "":
		return fmt.Errorf("%w: the pin of %s on %s names nobody who pinned it", ErrInvalid, p.L1, p.Scope)
	case p.PinnedAt.IsZero():
		return fmt.Errorf("%w: the pin of %s on %s has no time", ErrInvalid, p.L1, p.Scope)
	}
	return nil
}

// Pin records a pin, and reports whether it did. Pinning a document that is
// already pinned to the scope changes nothing: the first person to pin it, and
// when, stay the record.
func (s *Store) Pin(ctx context.Context, p Pin) (bool, error) {
	if err := p.Validate(); err != nil {
		return false, err
	}
	tag, err := s.db.Exec(ctx, `
INSERT INTO l2_pins (scope, l1, pinned_by, pinned_at) VALUES ($1, $2, $3, $4)
ON CONFLICT (scope, l1) DO NOTHING`, p.Scope, p.L1, p.PinnedBy, p.PinnedAt)
	if err != nil {
		return false, fmt.Errorf("pinning %s to %s: %w", p.L1, p.Scope, err)
	}
	return tag.RowsAffected() == 1, nil
}

// Unpin removes a pin, and reports whether there was one.
func (s *Store) Unpin(ctx context.Context, scope, doc string) (bool, error) {
	var removed string
	err := s.db.QueryRow(ctx, `DELETE FROM l2_pins WHERE scope = $1 AND l1 = $2 RETURNING l1`, scope, doc).Scan(&removed)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("unpinning %s from %s: %w", doc, scope, err)
	}
	return true, nil
}

// Pins is every pin on a scope, first pinned first; two pinned at the same
// instant are in document id order. Nothing here filters by reader: a pin names
// a document, and whether the reader may read it is the caller's to decide.
func (s *Store) Pins(ctx context.Context, scope string) ([]Pin, error) {
	rows, err := s.db.Query(ctx, `
SELECT scope, l1, pinned_by, pinned_at FROM l2_pins WHERE scope = $1 ORDER BY pinned_at, l1`, scope)
	if err != nil {
		return nil, fmt.Errorf("reading the pins on %s: %w", scope, err)
	}
	defer rows.Close()
	out := []Pin{}
	for rows.Next() {
		var p Pin
		if err := rows.Scan(&p.Scope, &p.L1, &p.PinnedBy, &p.PinnedAt); err != nil {
			return nil, fmt.Errorf("reading the pins on %s: %w", scope, err)
		}
		p.PinnedAt = p.PinnedAt.UTC()
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the pins on %s: %w", scope, err)
	}
	return out, nil
}
