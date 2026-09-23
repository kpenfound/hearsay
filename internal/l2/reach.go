package l2

import (
	"context"
	"fmt"

	"github.com/kpenfound/hearsay/internal/principal"
)

// The store is what a principal's reach is read from.
var _ principal.Graph = (*Store)(nil)

// descendantsSQL walks `part_of` down from the ids given. The walk is a UNION,
// so a cycle ends it rather than looping, and the ids given are kept whether
// the table holds them or not.
const descendantsSQL = `
WITH RECURSIVE down(id) AS (
    SELECT unnest($1::text[])
    UNION
    SELECT e.id FROM l2_entities e JOIN down ON e.part_of @> ARRAY[down.id]
)
SELECT id FROM down ORDER BY id`

// Descendants is every id given and every entity `part_of` one of them,
// transitively: what a grant of those ids covers (docs/config.md).
func (s *Store) Descendants(ctx context.Context, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return []string{}, nil
	}
	out, err := s.ids(ctx, descendantsSQL, ids)
	if err != nil {
		return nil, fmt.Errorf("reading what %d entities cover: %w", len(ids), err)
	}
	return out, nil
}

// linkedCodeSQL is the code entities pull requests about one of the ids touch:
// the `system` references L1 lists on a pull request's document, which are the
// code entities its paths fall under and the ones its text names.
const linkedCodeSQL = `
SELECT DISTINCT ref->>'id'
FROM l1_docs d, jsonb_array_elements(d.refs) AS ref
WHERE d.kind = 'pr' AND d.scope && $1::text[] AND ref->>'type' = 'system'
ORDER BY 1`

// LinkedCode is the code entities the pull requests about one of these
// entities touch, and every entity under them: the reach a worker or an
// orchestrator has beyond its scopes (docs/design.md#access-control).
//
// It reads the link, not who may read it. Reach and access lists are separate
// filters: a private pull request's link widens what is relevant, and every
// document in that wider reach is still filtered by its own access list.
func (s *Store) LinkedCode(ctx context.Context, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return []string{}, nil
	}
	code, err := s.ids(ctx, linkedCodeSQL, ids)
	if err != nil {
		return nil, fmt.Errorf("reading the code %d entities link to: %w", len(ids), err)
	}
	return s.Descendants(ctx, code)
}

func (s *Store) ids(ctx context.Context, sql string, args ...any) ([]string, error) {
	rows, err := s.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
