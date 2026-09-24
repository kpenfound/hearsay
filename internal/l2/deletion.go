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
	return enqueueUnsupportedEvidence(ctx, tx, docIDs)
}

// EnqueueNonassertingEvidence repairs live stances after a changed document
// stops asserting. The caller has already stored the new L1 outcome in tx.
func EnqueueNonassertingEvidence(ctx context.Context, tx pgx.Tx, docID string) error {
	return enqueueUnsupportedEvidence(ctx, tx, []string{docID})
}

func enqueueUnsupportedEvidence(ctx context.Context, tx pgx.Tx, docIDs []string) error {
	if len(docIDs) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT s.id, t.scope FROM l2_stances s
JOIN l2_topics t ON t.id = s.topic_id
WHERE s.evidence && $1::text[] AND NOT s.withdrawn AND NOT `+RetiredSQL, docIDs)
	if err != nil {
		return fmt.Errorf("finding stances with unsupported evidence: %w", err)
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
// still asserting citations, or with an explicit withdrawal if none survive. The
// assertion worker calls it under the topic's serialized scope key.
func (s *Store) RerunDeletedEvidence(ctx context.Context, stanceID, scope string) (bool, error) {
	var live bool
	// A repair normally stays on its predecessor's row, so undoing a merge
	// returns both to that row. A split is different: its selected stances
	// moved without rewriting their rows. Materialize the split's row for a
	// repair, so the repair follows the split and folds back on undo.
	var row string
	err := s.db.QueryRow(ctx, `SELECT t.scope = $2 AND NOT s.withdrawn AND NOT `+RetiredSQL+`, s.topic_id
FROM l2_stances s JOIN l2_topics t ON t.id = s.topic_id WHERE s.id = $1`, stanceID, scope).Scan(&live, &row)
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
	// Follow a repair chain to the stance a split selected. This also keeps
	// the repair on the split's row when that split was subsequently merged.
	var ancestors []string
	if err := s.db.QueryRow(ctx, `WITH RECURSIVE chain AS (
    SELECT id, supersedes FROM l2_stances WHERE id = $1
    UNION ALL SELECT p.id, p.supersedes FROM l2_stances p JOIN chain c ON p.id = c.supersedes
) SELECT coalesce(array_agg(id), '{}') FROM chain`, stanceID).Scan(&ancestors); err != nil {
		return false, fmt.Errorf("reading the predecessors of stance %s: %w", stanceID, err)
	}
	ops, err := s.Operations(ctx, OperationFilter{Scope: scope})
	if err != nil {
		return false, err
	}
	for _, op := range ops {
		if op.Kind != OperationSplit || !op.InForce() {
			continue
		}
		if slices.ContainsFunc(op.Stances, func(id string) bool { return slices.Contains(ancestors, id) }) {
			row = op.Topics[1]
		}
	}
	rows, err := s.db.Query(ctx, `SELECT id FROM l1_docs WHERE id = ANY($1) AND outcome_kind IN ('decided', 'proposed', 'resolved')`, st.Evidence)
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
	if row != st.TopicID {
		for _, op := range ops {
			if op.Kind == OperationSplit && op.Topics[1] == row {
				if _, err := s.OpenTopic(ctx, Topic{ID: row, Scope: scope, Name: op.Name, ACL: st.ACL, OpenedBy: st.Evidence[0]}); err != nil {
					return false, err
				}
				break
			}
		}
	}
	slices.Sort(surviving)
	withdrawn := len(surviving) == 0
	evidence := surviving
	position := st.Position
	if withdrawn {
		evidence = st.Evidence // provenance remains in the historical row
		position = "evidence deleted"
		var nonasserting bool
		if err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM l1_docs WHERE id = ANY($1))`, st.Evidence).Scan(&nonasserting); err != nil {
			return false, err
		}
		if nonasserting {
			position = "evidence no longer asserts"
		}
	}
	acl, err := json.Marshal(st.ACL)
	if err != nil {
		return false, err
	}
	id := "stance:" + digest(append([]string{"evidence deletion", st.ID}, surviving...)...)
	tag, err := s.db.Exec(ctx, `INSERT INTO l2_stances
(id, topic_id, position, author, stated_at, evidence, supersedes, tier, acl, judgement, assertion, withdrawn)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NULLIF($11, ''), $12)
ON CONFLICT (id) DO NOTHING`, id, row, position, st.Author, st.StatedAt,
		evidence, st.ID, st.Tier, acl, nullableJudgement(JudgementRestates), st.Assertion, withdrawn)
	if err != nil {
		return false, fmt.Errorf("superseding stance %s after evidence withdrawal: %w", st.ID, err)
	}
	if err := s.redact(ctx, []string{row}); err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// Redacted is the text an operator deletion leaves in place of a superseded
// stance's position or a topic's name.
const Redacted = "[redacted]"

// redactStancesSQL replaces the position of every stance on these topics that
// is superseded and was written before an operator deletion rebuilt one of its
// documents: its text may have been read from what the operator deleted. A
// stance still current keeps its text until something supersedes it, since a
// stance is never overwritten while it stands. A withdrawal uses a fixed
// reason and is left alone. The earliest deletion is the one
// named.
const redactStancesSQL = `
UPDATE l2_stances s SET position = $2, redacted_by = r.deletion
FROM (
    SELECT DISTINCT ON (o.id) o.id, d.deletion
    FROM l2_stances o
    JOIN l0_deletion_rebuilds d ON d.document = ANY(o.evidence) AND d.rebuilt_at > o.created_at
    WHERE o.topic_id = ANY($1) AND o.redacted_by IS NULL AND NOT o.withdrawn
      AND EXISTS (SELECT 1 FROM l2_stances n WHERE n.supersedes = o.id)
    ORDER BY o.id, d.rebuilt_at, d.deletion
) r
WHERE s.id = r.id`

// redactTopicsSQL replaces the name of every topic on the list whose opening
// document an operator deletion removed, and which no document still in L1
// supports: a name read only from deleted content. A topic a surviving
// document still cites keeps its name.
const redactTopicsSQL = `
UPDATE l2_topics t SET name = $2, redacted_by = r.deletion
FROM (
    SELECT DISTINCT ON (o.id) o.id, d.deletion
    FROM l2_topics o
    JOIN l0_deletion_rebuilds d ON d.document = o.opened_by AND d.outcome = 'deleted' AND d.rebuilt_at > o.created_at
    WHERE o.id = ANY($1) AND o.redacted_by IS NULL
      AND NOT EXISTS (SELECT 1 FROM l1_docs l WHERE l.id = o.opened_by)
      AND NOT EXISTS (SELECT 1 FROM l2_stances s JOIN l1_docs l ON l.id = ANY(s.evidence) WHERE s.topic_id = o.id)
    ORDER BY o.id, d.rebuilt_at, d.deletion
) r
WHERE t.id = r.id`

// RedactDeleted redacts, on every topic these L1 documents opened or are
// evidence on, the text an operator deletion took the ground from: see
// redactStancesSQL and redactTopicsSQL. The distiller calls it in the
// transaction that records the documents' rebuild; the store calls it again
// whenever a stance is superseded, which is when a stance that was current at
// the rebuild becomes redactable. A source tombstone records no rebuild, so it
// redacts nothing.
func RedactDeleted(ctx context.Context, q Querier, docIDs []string) error {
	if len(docIDs) == 0 {
		return nil
	}
	var topics []string
	if err := q.QueryRow(ctx, `SELECT coalesce(array_agg(DISTINCT id), '{}') FROM (
    SELECT topic_id AS id FROM l2_stances WHERE evidence && $1::text[]
    UNION SELECT id FROM l2_topics WHERE opened_by = ANY($1::text[])) affected`, docIDs).Scan(&topics); err != nil {
		return fmt.Errorf("finding the topics of rebuilt documents: %w", err)
	}
	return New(q).redact(ctx, topics)
}

// redact applies an operator deletion's redactions on these topics.
func (s *Store) redact(ctx context.Context, topics []string) error {
	if len(topics) == 0 {
		return nil
	}
	if _, err := s.db.Exec(ctx, redactStancesSQL, topics, Redacted); err != nil {
		return fmt.Errorf("redacting superseded stances: %w", err)
	}
	if _, err := s.db.Exec(ctx, redactTopicsSQL, topics, Redacted); err != nil {
		return fmt.Errorf("redacting topic names: %w", err)
	}
	return nil
}
