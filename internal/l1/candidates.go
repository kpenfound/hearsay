package l1

import (
	"context"
	"fmt"
	"time"
)

// Candidate is a document handle before access filtering. Only bounded graph
// traversal uses these: its raw-row budget deliberately includes hidden rows.
type Candidate struct {
	ID           string
	LastActivity time.Time
}

// Candidates returns document handles about a scope, in traversal order.
// Callers must authorize every handle with Reader.MayRead before serving it.
func (s *Store) Candidates(ctx context.Context, scope string, limit int) ([]Candidate, error) {
	rows, err := s.db.Query(ctx, `SELECT id, last_activity_at FROM l1_docs WHERE scope @> ARRAY[$1] ORDER BY last_activity_at DESC, id LIMIT $2`, scope, limit)
	if err != nil {
		return nil, fmt.Errorf("reading document candidates: %w", err)
	}
	defer rows.Close()
	out := []Candidate{}
	for rows.Next() {
		var c Candidate
		if err := rows.Scan(&c.ID, &c.LastActivity); err != nil {
			return nil, fmt.Errorf("reading document candidate: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
