package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/kpenfound/hearsay/internal/l1"
)

// MaxCoverageEvents is how many event ids one `coverage` call takes.
const MaxCoverageEvents = 100

const maxCoverageEventID = 1024

var coverageTool = Tool{"coverage", "Whether Hearsay covers each of up to 100 L0 event ids: a message of a conversation source. pending: held, and no published distillation of its conversation has read its current revision (not consumed yet, an attempt failed or was stopped, or it was rewritten or arrived since). covered: the published distillation that read its current revision quotes it, with the ids of the current L1 documents whose L0 refs cite it. excluded: deleted, left out of every document by the distillation of its current revision, or an id that does not exist or that the caller may not read (those two are answered identically). An id superseded by a newer revision is answered for the artifact's current revision. Results are in request order, one per id as asked. Decided from the recorded distillations, current documents and deletions, never from job state or timing; no model call.",
	schema(`{"event_ids":{"type":"array","minItems":1,"maxItems":100,"items":{"type":"string"},"description":"L0 event ids, as get_l0 and watch name them"}}`, "event_ids")}

// CoverageResult is one id's answer. Documents is present only when State is
// covered.
type CoverageResult struct {
	EventID   string   `json:"event_id"`
	State     string   `json:"state"`
	Documents []string `json:"documents,omitempty"`
}

// Covered is what `coverage` returns.
type Covered struct {
	Results []CoverageResult `json:"results"`
}

func coverage(ctx context.Context, c *Calls, _ Caller, reader l1.Reader, raw json.RawMessage) (any, error) {
	var args struct {
		EventIDs []string `json:"event_ids"`
	}
	if err := decode(raw, &args); err != nil {
		return nil, refusal(http.StatusBadRequest, ReasonInvalidRequest, err.Error())
	}
	if n := len(args.EventIDs); n == 0 || n > MaxCoverageEvents {
		return nil, refusal(http.StatusBadRequest, ReasonInvalidRequest, fmt.Sprintf("event_ids takes 1 to %d ids, not %d", MaxCoverageEvents, n))
	}
	for _, id := range args.EventIDs {
		if id == "" || len(id) > maxCoverageEventID {
			return nil, refusal(http.StatusBadRequest, ReasonInvalidRequest, "an event id is empty or too long")
		}
	}

	tx, err := c.db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading coverage: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY`); err != nil {
		return nil, fmt.Errorf("reading coverage: %w", err)
	}
	answers, err := l1.New(tx).Coverage(ctx, reader, args.EventIDs)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("reading coverage: %w", err)
	}
	out := Covered{Results: make([]CoverageResult, len(answers))}
	for i, a := range answers {
		out.Results[i] = CoverageResult{EventID: a.Event, State: string(a.State), Documents: a.Documents}
	}
	return out, nil
}
