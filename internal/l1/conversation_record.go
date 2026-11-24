package l1

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
)

// ConversationDistillation is the published distillation of one conversation:
// the message revisions it was built from and the documents it made. It is
// written in the transaction that reconciles the conversation's documents
// ([Store.PutConversationDistillation]), so it describes exactly what that
// transaction left in L1 and exactly what the attempt read.
type ConversationDistillation struct {
	Source string
	// Thread is the conversation id.
	Thread string
	// Inputs are the L0 event ids of the current revision of every message the
	// distillation read, sorted. An event id names one revision: a message
	// rewritten since has a new id that is not among them.
	Inputs []string
	// Documents are the ids of the documents it produced, sorted. It is empty
	// where segmentation selected nothing, which is still a distillation of
	// every input.
	Documents []string
}

// Read reports whether the distillation read this exact revision of a message.
// A message it did not read is not covered by it, whatever its documents say.
func (c ConversationDistillation) Read(eventID string) bool {
	_, found := slices.BinarySearch(c.Inputs, eventID)
	return found
}

// PutConversationDistillation records a conversation's published distillation,
// replacing the one before it. Inputs and documents are stored sorted. It
// reports whether the record changed: publishing the same inputs and documents
// again writes nothing.
//
// It belongs in the transaction that wrote the documents and removed the ones
// that no longer exist, so that the record and the documents are one outcome.
func (s *Store) PutConversationDistillation(ctx context.Context, d ConversationDistillation) (bool, error) {
	inputs, documents := slices.Clone(d.Inputs), slices.Clone(d.Documents)
	slices.Sort(inputs)
	slices.Sort(documents)
	if inputs == nil {
		inputs = []string{}
	}
	if documents == nil {
		documents = []string{}
	}
	var published bool
	err := s.db.QueryRow(ctx, `
INSERT INTO l1_conversation_distillations (source, thread, inputs, documents)
VALUES ($1, $2, $3, $4)
ON CONFLICT (source, thread) DO UPDATE
   SET inputs = EXCLUDED.inputs, documents = EXCLUDED.documents, published_at = now()
 WHERE l1_conversation_distillations.inputs IS DISTINCT FROM EXCLUDED.inputs
    OR l1_conversation_distillations.documents IS DISTINCT FROM EXCLUDED.documents
RETURNING true`, d.Source, d.Thread, inputs, documents).Scan(&published)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("recording the distillation of conversation %s: %w", d.Thread, err)
	}
	return true, nil
}

// ConversationDistillationOf is the published distillation of one conversation,
// and false where none has been published.
func (s *Store) ConversationDistillationOf(ctx context.Context, source, thread string) (ConversationDistillation, bool, error) {
	d := ConversationDistillation{Source: source, Thread: thread}
	err := s.db.QueryRow(ctx,
		`SELECT inputs, documents FROM l1_conversation_distillations WHERE source = $1 AND thread = $2`,
		source, thread).Scan(&d.Inputs, &d.Documents)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ConversationDistillation{}, false, nil
	case err != nil:
		return ConversationDistillation{}, false, fmt.Errorf("reading the distillation of conversation %s: %w", thread, err)
	}
	return d, true, nil
}

// ConversationDistillationsReading is, for each of these event ids that a
// published distillation read, that distillation. An id no distillation read —
// a message not yet distilled, one rewritten since, one that does not exist —
// is absent, which is what a reader treats as not yet covered. It reads the
// record alone: no job state, no clock and no document.
func (s *Store) ConversationDistillationsReading(ctx context.Context, eventIDs []string) (map[string]ConversationDistillation, error) {
	out := map[string]ConversationDistillation{}
	if len(eventIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx,
		`SELECT source, thread, inputs, documents FROM l1_conversation_distillations WHERE inputs && $1::text[]`, eventIDs)
	if err != nil {
		return nil, fmt.Errorf("reading the published conversation distillations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var d ConversationDistillation
		if err := rows.Scan(&d.Source, &d.Thread, &d.Inputs, &d.Documents); err != nil {
			return nil, fmt.Errorf("reading the published conversation distillations: %w", err)
		}
		for _, id := range eventIDs {
			if d.Read(id) {
				out[id] = d
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the published conversation distillations: %w", err)
	}
	return out, nil
}
