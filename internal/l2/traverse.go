package l2

import (
	"context"
	"fmt"
)

// Children returns the direct part_of children, in id order. Traversal walks
// one level at a time so a node outside reach cannot act as a bridge.
func (s *Store) Children(ctx context.Context, id string, limit int) ([]string, error) {
	ids, err := s.ids(ctx, `SELECT id FROM l2_entities WHERE part_of @> ARRAY[$1] ORDER BY id LIMIT $2`, id, limit)
	if err != nil {
		return nil, fmt.Errorf("reading entity children: %w", err)
	}
	return ids, nil
}

// Citing returns stances citing a document, including asserted stances and
// citations beyond the origin document. Every row is placed through the ledger.
func (s *Store) Citing(ctx context.Context, id string, limit int) ([]Stance, error) {
	rows, err := s.stanceRows(ctx, `SELECT `+stanceColumns+` FROM l2_stances s WHERE s.evidence @> ARRAY[$1] ORDER BY s.stated_at DESC, s.id LIMIT $2`, id, limit)
	if err != nil {
		return nil, fmt.Errorf("reading document citations: %w", err)
	}
	return s.placed(ctx, rows)
}

// Successors returns the direct supersession successors, including edges a
// split placed on another topic. Each stance follows the current ledger.
func (s *Store) Successors(ctx context.Context, id string, limit int) ([]Stance, error) {
	rows, err := s.stanceRows(ctx, `SELECT `+stanceColumns+` FROM l2_stances s WHERE s.supersedes = $1 ORDER BY s.stated_at DESC, s.id LIMIT $2`, id, limit)
	if err != nil {
		return nil, fmt.Errorf("reading supersession successors: %w", err)
	}
	return s.placed(ctx, rows)
}
