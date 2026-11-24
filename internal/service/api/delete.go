package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/kpenfound/hearsay/internal/connector/conversation"
	"github.com/kpenfound/hearsay/internal/deletion"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/principal"
	"github.com/kpenfound/hearsay/internal/telemetry"
)

// The most event ids one `delete` names, and the longest idempotency id.
const (
	maxDeleteEvents  = 100
	maxIdempotencyID = 256
)

// kept is what a deletion leaves in Hearsay, which a re-sent copy of a deleted
// message is recognised by: the redaction keeps these and nothing of the words.
var kept = []string{"ids", "times", "kinds"}

var deleteTool = Tool{"delete", "Delete messages of the caller's own conversation, by event id: the owner of a conversation source, calling directly or with that source's allowed agent carrying the request. Any revision's event id names the whole message, and every revision of it is redacted, under one deletion record and one deletion event for the request; the documents built from it are queued for rebuild. idempotency_id is the request's own id: repeating a request, sequentially, concurrently or after a lost answer, returns the original answer and records nothing more. An event that is not in the caller's own conversation sources, or does not exist, refuses the whole request as not_found and deletes nothing. Returns the deletion id, the event ids deleted, and kept: the ids, times and kinds that stay so that a re-sent copy stays deleted. Refusals carry a code: invalid_request, not_allowed, not_found, conflict or internal. Makes no model call.",
	schema(`{"event_ids":{"type":"array","items":{"type":"string"},"minItems":1,"maxItems":100,"description":"L0 event ids of the messages to delete; any revision's id names the message"},"idempotency_id":{"type":"string","description":"your id for this request; opaque, at most 256 bytes; reuse it to retry the same request"}}`, "event_ids", "idempotency_id")}

// Deleted is what `delete` returns.
type Deleted struct {
	// DeletionID is the deletion record of the request.
	DeletionID string `json:"deletion_id"`
	// Deleted are the events redacted: every revision of each named message.
	Deleted []string `json:"deleted"`
	// Kept is what the deletion leaves: ids, times and kinds.
	Kept []string `json:"kept"`
}

// errDeleteNotFound is the one answer for an event that does not exist, one in
// a source that is not the caller's conversation, and one in another person's
// conversation.
var errDeleteNotFound = refusal(http.StatusNotFound, ReasonNotFound, "no such event in your own conversation")

func deleteEvents(ctx context.Context, c *Calls, caller Caller, _ l1.Reader, raw json.RawMessage) (any, error) {
	out, err := c.deleteEvents(ctx, caller, raw)
	if err == nil {
		return out, nil
	}
	var e *Error
	if errors.As(err, &e) {
		if e.Code == "" {
			e = &Error{Status: e.Status, Message: e.Message, Code: ReasonInvalidRequest}
		}
		return nil, e
	}
	telemetry.Logger(ctx).ErrorContext(ctx, "delete failed", "principal", caller.Principal, "agent", caller.Agent, "error", err)
	return nil, refusal(errInternal.Status, ReasonInternal, errInternal.Message)
}

// deletionID is the id of the deletion record a request is recorded under,
// which is its idempotency key: derived from the human, the carrier and the
// caller's idempotency id, so another person's ids never collide with this
// one's, in the form `del_` and 128 bits in hex.
func deletionID(human, carrier, idempotency string) string {
	key, _ := json.Marshal([]string{human, carrier, idempotency})
	sum := sha256.Sum256(key)
	return "del_" + hex.EncodeToString(sum[:16])
}

// ownSources are the conversation sources the caller may delete from: those
// whose configured owner is the person, and, when an agent carries the request,
// that allow the agent to post.
func (c *Calls) ownSources(caller Caller) map[string]bool {
	own := map[string]bool{}
	for _, src := range c.repo.Sources {
		owner, agents, ok := conversation.OwnerOf(src)
		if !ok || owner != caller.Principal {
			continue
		}
		if caller.Agent == "" || slices.Contains(agents, caller.Agent) {
			own[src.ID] = true
		}
	}
	return own
}

// deleteEvents is `delete(event_ids, idempotency_id)`
// (docs/design.md#read-and-assert-api): the configured owner of a conversation
// source deletes messages of it, through the operator deletion, as one
// request. No model is called.
//
// The request is authorized as a whole: every named event must exist in a
// conversation source the person owns and, if an agent carries it, allows that
// agent, or the call is refused as not found and nothing is written. The
// events' artifacts are then deleted, with the one deletion record, the one
// `deletion` event and the queued rebuilds, in one transaction that also
// checks the authorization again. A request whose idempotency id is already
// recorded is answered with the original deletion and writes nothing.
func (c *Calls) deleteEvents(ctx context.Context, caller Caller, raw json.RawMessage) (*Deleted, error) {
	var args struct {
		EventIDs      []string `json:"event_ids"`
		IdempotencyID string   `json:"idempotency_id"`
	}
	if err := decode(raw, &args); err != nil {
		var e *Error
		if errors.As(err, &e) {
			return nil, invalidRequest(e.Message)
		}
		return nil, err
	}
	switch {
	case len(args.EventIDs) == 0:
		return nil, invalidRequest("event_ids needs at least one event id")
	case len(args.EventIDs) > maxDeleteEvents:
		return nil, invalidRequest("event_ids is over " + strconv.Itoa(maxDeleteEvents) + " ids")
	case strings.TrimSpace(args.IdempotencyID) == "":
		return nil, invalidRequest("idempotency_id is required")
	case len(args.IdempotencyID) > maxIdempotencyID:
		return nil, invalidRequest("idempotency_id is over " + strconv.Itoa(maxIdempotencyID) + " bytes")
	}
	for _, id := range args.EventIDs {
		if strings.TrimSpace(id) == "" || len(id) > maxTargetID {
			return nil, invalidRequest("an event id is empty or over " + strconv.Itoa(maxTargetID) + " bytes")
		}
	}
	if caller.Agent != "" {
		agent, ok := c.resolver.Principal(caller.Agent)
		if !ok || !agent.Class.Rights().Write.Has(principal.WriteAssert) {
			return nil, refusal(http.StatusForbidden, ReasonNotAllowed, "agent "+strconv.Quote(caller.Agent)+" is of class "+strconv.Quote(string(agent.Class))+", which may not carry a deletion")
		}
	}
	if c.pool == nil {
		return nil, errors.New("the call layer has no database pool to record a deletion with")
	}

	named := slices.Clone(args.EventIDs)
	slices.Sort(named)
	named = slices.Compact(named)
	own := c.ownSources(caller)
	selectors, err := c.deleteSelectors(ctx, own, named)
	if err != nil {
		return nil, err
	}

	call := deletion.Call{Events: named, IdempotencyID: args.IdempotencyID, Carrier: caller.Agent}
	recorded := deletion.Selector{Call: &call}
	applied, err := deletion.ApplyRequest(ctx, c.pool, c.repo, deletion.Request{
		ID:        deletionID(caller.Principal, caller.Agent, args.IdempotencyID),
		Selectors: selectors,
		Recorded:  &recorded,
		Reason:    "the owner's delete call",
		Operator:  caller.Principal,
		Guard: func(ctx context.Context, tx pgx.Tx, p deletion.Preview) error {
			return authorizeDeletion(ctx, tx, own, named, p.Events)
		},
	})
	switch {
	case errors.Is(err, errNotOwned):
		return nil, errDeleteNotFound
	case errors.Is(err, l0.ErrNothingToDelete):
		return c.alreadyDeleted(ctx, named)
	case err != nil:
		return nil, err
	}
	if applied.Replay {
		prior := applied.Preview.Selector.Call
		if prior == nil || !slices.Equal(prior.Events, named) || prior.Carrier != caller.Agent {
			return nil, refusal(http.StatusConflict, ReasonConflict, "this idempotency_id was already used for a different request")
		}
	}
	return &Deleted{DeletionID: applied.Deletion, Deleted: nonNil(applied.Events), Kept: kept}, nil
}

func invalidRequest(message string) *Error {
	return refusal(http.StatusBadRequest, ReasonInvalidRequest, message)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// errNotOwned is returned inside the deletion's transaction for an event the
// caller does not own.
var errNotOwned = errors.New("an event is not in the caller's own conversation")

// deleteSelectors resolves the named events to one artifact selector per
// message, refusing the request as not found if any does not exist or is not
// in a source the caller owns.
func (c *Calls) deleteSelectors(ctx context.Context, own map[string]bool, named []string) ([]deletion.Selector, error) {
	rows, err := c.pool.Query(ctx, `SELECT id, source, artifact FROM l0_events WHERE id = ANY($1)`, named)
	if err != nil {
		return nil, err
	}
	type ref struct{ id, source, artifact string }
	found, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ref, error) {
		var x ref
		return x, r.Scan(&x.id, &x.source, &x.artifact)
	})
	if err != nil {
		return nil, err
	}
	if len(found) != len(named) {
		return nil, errDeleteNotFound
	}
	var selectors []deletion.Selector
	for _, x := range found {
		if !own[x.source] {
			return nil, errDeleteNotFound
		}
		selectors = append(selectors, deletion.Selector{ArtifactSource: x.source, ArtifactID: x.artifact})
	}
	slices.SortFunc(selectors, func(a, b deletion.Selector) int {
		return strings.Compare(a.ArtifactSource+"\x00"+a.ArtifactID, b.ArtifactSource+"\x00"+b.ArtifactID)
	})
	return slices.Compact(selectors), nil
}

// authorizeDeletion is the check that stands with the deletion: every event
// named is among those to be redacted, and every event to be redacted is in a
// conversation source the caller owns.
func authorizeDeletion(ctx context.Context, tx pgx.Tx, own map[string]bool, named, events []string) error {
	for _, id := range named {
		if !slices.Contains(events, id) {
			return errNotOwned
		}
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT source FROM l0_events WHERE id = ANY($1)`, events)
	if err != nil {
		return err
	}
	sources, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, source := range sources {
		if !own[source] {
			return errNotOwned
		}
	}
	return nil
}

// alreadyDeleted answers a request whose every message an earlier deletion
// had redacted: it names that deletion, and deletes and records nothing.
func (c *Calls) alreadyDeleted(ctx context.Context, named []string) (*Deleted, error) {
	var id string
	err := c.pool.QueryRow(ctx, `SELECT deletion FROM l0_events WHERE id = ANY($1) AND deletion IS NOT NULL ORDER BY deletion LIMIT 1`, named).Scan(&id)
	if err != nil {
		return nil, err
	}
	return &Deleted{DeletionID: id, Deleted: []string{}, Kept: kept}, nil
}
