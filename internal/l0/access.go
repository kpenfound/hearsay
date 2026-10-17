package l0

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kpenfound/hearsay/internal/connector"
)

// ContainerAccess returns every distinct access list carried by the current
// visible revisions in a source's container. It has no page limit: proving
// access requires every list, including both public and private lists during
// an ACL re-sync. No current revisions means no lists, never public access.
func (s *Store) ContainerAccess(ctx context.Context, source, container string) ([]connector.ACL, error) {
	rows, err := s.db.Query(ctx, containerAccessSQL, source, container)
	if err != nil {
		return nil, fmt.Errorf("reading access lists of %s container %s: %w", source, container, err)
	}
	defer rows.Close()
	lists := []connector.ACL{}
	for rows.Next() {
		var encoded []byte
		if err := rows.Scan(&encoded); err != nil {
			return nil, fmt.Errorf("reading container access list: %w", err)
		}
		var acl connector.ACL
		if err := json.Unmarshal(encoded, &acl); err != nil {
			return nil, fmt.Errorf("decoding container access list: %w", err)
		}
		lists = append(lists, acl)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading container access lists: %w", err)
	}
	return lists, nil
}

// Find candidate artifacts by container, then take their current revision
// across all containers. Filtering only before the revision fold would retain
// an old revision of an artifact that has moved out of this container.
const containerAccessSQL = `
SELECT DISTINCT current.acl
  FROM (SELECT DISTINCT artifact FROM l0_events
         WHERE source = $1 AND payload->'container'->>'native_id' = $2) candidates
 CROSS JOIN LATERAL (
    SELECT e.acl, e.payload FROM l0_events e
     WHERE e.source = $1 AND e.artifact = candidates.artifact
       AND e.kind <> 'tombstone' AND ` + visibleSQL + `
     ORDER BY e.revision_edited_at DESC NULLS LAST, e.seq DESC LIMIT 1
 ) current
 WHERE current.payload->'container'->>'native_id' = $2`
