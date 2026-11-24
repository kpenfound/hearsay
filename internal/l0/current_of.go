package l0

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kpenfound/hearsay/internal/connector"
)

// Held is the revision of an artifact that L0 serves as current now, found by
// the id of any revision of it.
type Held struct {
	// Event is the id of the artifact's current revision. It is the id asked
	// for when that revision is the current one.
	Event string
	// Kind is the current revision's kind.
	Kind connector.Kind
	// ACL is the current revision's access list, and Asked is the access list
	// of the revision that was asked for. They are the same list unless the
	// artifact's access list changed between the two revisions.
	ACL, Asked connector.ACL
}

// CurrentOf is, for each of these event ids, the current revision of its
// artifact: the one [Store.Current] returns, which is what everything above L0
// reads. An id L0 does not hold, and an id whose artifact has no revision left
// that is neither deleted by an operator nor retracted by a tombstone, has no
// entry. A revision superseded by a newer one is answered with the newer
// revision, whether or not the superseded one was itself deleted.
//
// The entry says nothing of whether the caller may read the event; the access
// lists are the caller's to check.
func (s *Store) CurrentOf(ctx context.Context, ids []string) (map[string]Held, error) {
	out := make(map[string]Held, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `
SELECT a.id, e.id, e.kind, e.acl, a.acl
  FROM l0_events a
  JOIN LATERAL (
        SELECT e.id, e.kind, e.acl
          FROM l0_events e
         WHERE e.source = a.source AND e.artifact = a.artifact AND `+visibleSQL+`
         ORDER BY e.revision_edited_at DESC NULLS LAST, e.seq DESC
         LIMIT 1) AS e ON true
 WHERE a.id = ANY($1::text[])`, ids)
	if err != nil {
		return nil, fmt.Errorf("reading the current revision of %d events: %w", len(ids), err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			asked, kind        string
			held               Held
			currentACL, theirs []byte
		)
		if err := rows.Scan(&asked, &held.Event, &kind, &currentACL, &theirs); err != nil {
			return nil, fmt.Errorf("reading the current revision of %d events: %w", len(ids), err)
		}
		held.Kind = connector.Kind(kind)
		if err := decodeACL(currentACL, &held.ACL); err != nil {
			return nil, fmt.Errorf("reading the current revision of %s: %w", asked, err)
		}
		if err := decodeACL(theirs, &held.Asked); err != nil {
			return nil, fmt.Errorf("reading the current revision of %s: %w", asked, err)
		}
		out[asked] = held
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the current revision of %d events: %w", len(ids), err)
	}
	return out, nil
}

func decodeACL(raw []byte, into *connector.ACL) error {
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("decoding the acl: %w", err)
	}
	return nil
}
