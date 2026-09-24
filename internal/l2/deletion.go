package l2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/kpenfound/hearsay/internal/queue"
)

// WithdrawalTarget distinguishes an evidence repair from a document assertion.
const WithdrawalTarget = "withdraw:"

// EnqueueDeletedEvidence schedules each live stance affected by an L1 deletion
// on its topic's serial key. The caller uses the transaction that deleted L1.
func EnqueueDeletedEvidence(ctx context.Context, tx pgx.Tx, docIDs []string) error {
	if len(docIDs) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT s.id, t.scope FROM l2_stances s
JOIN l2_topics t ON t.id = s.topic_id
WHERE s.evidence && $1::text[] AND NOT s.withdrawn AND NOT `+RetiredSQL, docIDs)
	if err != nil {
		return fmt.Errorf("finding stances with deleted evidence: %w", err)
	}
	type affected struct{ id, scope string }
	var found []affected
	for rows.Next() {
		var a affected
		if err := rows.Scan(&a.id, &a.scope); err != nil {
			rows.Close()
			return err
		}
		found = append(found, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, a := range found {
		if _, err := queue.Enqueue(ctx, tx, queue.Request{Kind: AssertKind(), TargetID: WithdrawalTarget + a.id, SerialKey: a.scope}); err != nil {
			return err
		}
	}
	return nil
}

// RerunDeletedEvidence replaces a live stance with one supported by the
// surviving citations, or with an explicit withdrawal if none survive. The
// assertion worker calls it under the topic's serialized scope key.
func (s *Store) RerunDeletedEvidence(ctx context.Context, stanceID, scope string) (bool, error) {
	var live bool
	err := s.db.QueryRow(ctx, `SELECT t.scope = $2 AND NOT s.withdrawn AND NOT `+RetiredSQL+`
FROM l2_stances s JOIN l2_topics t ON t.id = s.topic_id WHERE s.id = $1`, stanceID, scope).Scan(&live)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil || !live {
		return false, err
	}
	st, err := s.Stance(ctx, stanceID)
	if err != nil {
		return false, err
	}
	rows, err := s.db.Query(ctx, `SELECT id FROM l1_docs WHERE id = ANY($1)`, st.Evidence)
	if err != nil {
		return false, err
	}
	var surviving []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return false, err
		}
		surviving = append(surviving, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	if len(surviving) == len(st.Evidence) {
		return false, nil
	}
	slices.Sort(surviving)
	withdrawn := len(surviving) == 0
	evidence := surviving
	position := st.Position
	if withdrawn {
		evidence = st.Evidence // provenance remains in the historical row
		position = "evidence deleted"
	}
	acl, err := json.Marshal(st.ACL)
	if err != nil {
		return false, err
	}
	id := "stance:" + digest(append([]string{"evidence deletion", st.ID}, surviving...)...)
	tag, err := s.db.Exec(ctx, `INSERT INTO l2_stances
(id, topic_id, position, author, stated_at, evidence, supersedes, tier, acl, judgement, assertion, withdrawn)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NULLIF($11, ''), $12)
ON CONFLICT (id) DO NOTHING`, id, st.TopicID, position, st.Author, st.StatedAt,
		evidence, st.ID, st.Tier, acl, nullableJudgement(JudgementRestates), st.Assertion, withdrawn)
	if err != nil {
		return false, fmt.Errorf("superseding stance %s after evidence deletion: %w", st.ID, err)
	}
	return tag.RowsAffected() == 1, nil
}
