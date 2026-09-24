package l0

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kpenfound/hearsay/internal/connector"
)

// ErrNothingToDelete is returned by [Delete] when every event it was given is
// already deleted.
var ErrNothingToDelete = errors.New("every selected event is already deleted")

// Deletion is an operator deletion as L0 records it (ADR-0018).
type Deletion struct {
	// ID is the deletion's id, `del_` and 32 hex digits.
	ID string
	// Operator is the configured human principal applying it. The caller has
	// checked that it is one; L0 stores what it is given.
	Operator string
	// Reason is why, and may not be empty.
	Reason string
	// Selector is what the operator asked for, stored as given.
	Selector json.RawMessage
	// Events are the L0 events to redact. Ones already deleted are left alone,
	// and so are Hearsay's own `deletion` events: a deletion does not erase
	// the record of another.
	Events []string
	// Documents are the L1 documents built from them, recorded for the audit
	// trail. Enqueueing their re-distillation is the caller's.
	Documents []string
	// Time is when it was applied.
	Time time.Time
}

// Deleted is what [Delete] did.
type Deleted struct {
	// Events are the events redacted, sorted.
	Events []string
	// Retraction is the id of the `deletion` event written under source
	// `hearsay`.
	Retraction string
}

// Delete applies an operator deletion: it records it, writes Hearsay's own
// `deletion` event naming it, and redacts every covered row in place. It takes
// a transaction because those three writes stand or fall together, and so do
// whatever the caller does beside them — the re-distillation jobs, above all.
//
// A redacted row keeps its id, source, native id, kind, artifact, times,
// revision, target, ACL and its place in the change feed. Its payload keeps
// the artifact, revision, target, thread, parent, part_of, base_kind and the
// container's kind and id — the fields that say which conversation the event
// was part of, which a later tombstone for the artifact still needs — and a
// `redacted` marker naming the deletion. Everything else, the content and
// who wrote it, is gone.
func Delete(ctx context.Context, tx pgx.Tx, d Deletion) (Deleted, error) {
	if d.ID == "" || d.Operator == "" || d.Reason == "" || len(d.Selector) == 0 || d.Time.IsZero() {
		return Deleted{}, errors.New("a deletion needs an id, an operator, a reason, a selector and a time")
	}
	rows, err := tx.Query(ctx, `
SELECT id FROM l0_events
 WHERE id = ANY($1) AND deletion IS NULL AND kind <> $2
 ORDER BY id
   FOR UPDATE`, d.Events, string(connector.KindDeletion))
	if err != nil {
		return Deleted{}, fmt.Errorf("locking the events deletion %s covers: %w", d.ID, err)
	}
	covered, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return Deleted{}, fmt.Errorf("locking the events deletion %s covers: %w", d.ID, err)
	}
	if len(covered) == 0 {
		return Deleted{}, ErrNothingToDelete
	}
	documents := d.Documents
	if documents == nil {
		documents = []string{}
	}

	record, err := json.Marshal(struct {
		Deletion string          `json:"deletion"`
		Operator string          `json:"operator"`
		Reason   string          `json:"reason"`
		Selector json.RawMessage `json:"selector"`
		Events   []string        `json:"events"`
	}{d.ID, d.Operator, d.Reason, d.Selector, covered})
	if err != nil {
		return Deleted{}, fmt.Errorf("encoding deletion %s: %w", d.ID, err)
	}
	artifact := "deletion:" + d.ID
	operator := connector.Identity{Source: connector.SelfSource, Kind: connector.IdentityUser, NativeID: d.Operator}
	appended, err := New(tx).Append(ctx, connector.Event{
		Source:   connector.SelfSource,
		NativeID: artifact,
		Kind:     connector.KindDeletion,
		Time:     d.Time,
		Payload: connector.Payload{
			Artifact:  artifact,
			Container: connector.Container{Kind: connector.ContainerWorkspace, NativeID: "operator"},
			Author:    &operator,
			Native:    record,
		},
		// Like an audit event: an entry in Hearsay's own source, which no
		// configured identity is in, so no reader's audience holds it.
		ACL: connector.ACL{{Kind: connector.ACLIdentity, Source: connector.SelfSource, NativeID: d.Operator}},
	})
	if err != nil {
		return Deleted{}, fmt.Errorf("writing the event for deletion %s: %w", d.ID, err)
	}

	if _, err := tx.Exec(ctx, `
INSERT INTO l0_deletions (id, operator, reason, selector, deleted_at, events, documents, retraction)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		d.ID, d.Operator, d.Reason, d.Selector, d.Time, covered, documents, appended.ID); err != nil {
		return Deleted{}, fmt.Errorf("recording deletion %s: %w", d.ID, err)
	}
	if _, err := tx.Exec(ctx, redactSQL, d.ID, covered); err != nil {
		return Deleted{}, fmt.Errorf("redacting the events deletion %s covers: %w", d.ID, err)
	}
	return Deleted{Events: covered, Retraction: appended.ID}, nil
}

// redactSQL replaces a payload with the fields that place the event and a
// marker naming the deletion. jsonb_strip_nulls drops the fields the event
// never had, so a redacted payload claims nothing it did not.
const redactSQL = `
UPDATE l0_events e
   SET deletion = $1,
       payload = jsonb_strip_nulls(jsonb_build_object(
           'artifact', e.payload->'artifact',
           'revision', e.payload->'revision',
           'target', e.payload->'target',
           'thread', e.payload->'thread',
           'parent', e.payload->'parent',
           'part_of', e.payload->'part_of',
           'base_kind', e.payload->'base_kind',
           'container', jsonb_build_object(
               'kind', e.payload->'container'->'kind',
               'native_id', e.payload->'container'->'native_id'),
           'redacted', jsonb_build_object('deletion', $1::text)))
 WHERE e.id = ANY($2)`
