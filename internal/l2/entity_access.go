package l2

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
)

// WorkEntityVisibility records which existing work ids are hidden. An absent
// work id is hidden too, so scoped reads and historical listings cannot reveal
// existence by taking a different path. Other namespaces keep their behavior.
type WorkEntityVisibility map[string]bool

// Hidden reports whether an id must be treated as an absent work entity.
func (v WorkEntityVisibility) Hidden(id string) bool {
	hidden, known := v[id]
	return hidden || strings.HasPrefix(id, "work:") && !known
}

// HiddenWorkEntities reads which work ids this reader must not discover.
// Source declarations require readable current documents; other origins are
// recorded as visible and retain their reach-only behavior. One query reads
// all guards, so discovery never queries per entity.
// Missing documents fail closed; a tombstone or operator deletion also hides
// an entity while the distiller and hierarchy follower catch up.
func (s *Store) HiddenWorkEntities(ctx context.Context, reader l1.Reader) (WorkEntityVisibility, error) {
	rows, err := s.db.Query(ctx, `
SELECT e.id, e.origin, d.acl, d.scope
  FROM l2_entities e
  LEFT JOIN l1_docs d ON e.origin = 'source' AND d.id = 'l1:' || substr(e.id, 6)
   AND e.id = 'work:' || d.source || ':' || d.source_native_id
   AND NOT EXISTS (
       SELECT 1 FROM l0_events root
        WHERE root.id = ANY(d.l0_refs)
          AND (root.deletion IS NOT NULL OR EXISTS (
              SELECT 1 FROM l0_events tomb
               WHERE tomb.source = root.source AND tomb.target = root.artifact
                 AND tomb.seq > root.seq)))
 WHERE e.id LIKE 'work:%'`)
	if err != nil {
		return nil, fmt.Errorf("reading work entity access: %w", err)
	}
	defer rows.Close()
	hidden := WorkEntityVisibility{}
	for rows.Next() {
		var id, origin string
		var raw []byte
		var scope []string
		if err := rows.Scan(&id, &origin, &raw, &scope); err != nil {
			return nil, fmt.Errorf("reading work entity guard: %w", err)
		}
		var acl connector.ACL
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &acl); err != nil {
				return nil, fmt.Errorf("decoding work entity guard: %w", err)
			}
		}
		hidden[id] = Origin(origin) == OriginSource && (!reader.Allows(acl) || !reader.InReach(scope))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading work entity guards: %w", err)
	}
	return hidden, nil
}
