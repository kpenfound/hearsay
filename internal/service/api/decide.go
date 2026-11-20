package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/principal"
	"github.com/kpenfound/hearsay/internal/telemetry"
)

// The reason codes a refused `decide` carries beside its message
// (docs/design.md#read-and-assert-api). The set is closed: a caller branches on
// the code, never on the status or the words. Failures every call shares and
// that happen before a request reaches `decide` — credentials, an oversized
// body, session and reach resolution — carry none.
const (
	// ReasonNotAllowed: the human may not decide in the stance's scope, or the
	// carrying agent's class may not carry a decision.
	ReasonNotAllowed = "not_allowed"
	// ReasonNotFound: no such stance or gesture that the caller may read within
	// their reach. Nonexistent, unreadable and out-of-reach are one answer.
	ReasonNotFound = "not_found"
	// ReasonNotLive: the stance is superseded, retired or withdrawn.
	ReasonNotLive = "not_live"
	// ReasonNotUndoable: the gesture is already undone, is itself an undo, or is
	// not a ratify or a demote.
	ReasonNotUndoable = "not_undoable"
	// ReasonConflict: the decision id is already used for a different decision.
	ReasonConflict = "conflict"
	// ReasonInvalid: malformed arguments or an action `decide` does not take.
	ReasonInvalid = "invalid"
	// ReasonInternal: an unexpected failure inside the call.
	ReasonInternal = "internal"
)

// maxDecisionID bounds the opaque id a caller supplies; Kern's is 64 hex
// characters.
const maxDecisionID = 256

// maxTargetID bounds a stance id.
const maxTargetID = 512

var decideTool = Tool{"decide", "Make the person's own decision on a stance: ratify or demote one live stance by its id, or undo an earlier ratify or demote by its gesture id. The decision is the human's in the Hearsay-Principal header, checked against the scope's ratified_by policy; an agent named in Hearsay-Agent only carries it, is recorded as the carrier, and needs class worker or above. decision_id is the idempotency key, from the human, the carrier and that id: repeating a request returns the original gesture with recorded false, and the same id for a different decision is refused. Returns the gesture id, the action, the stance or the gesture undone, and whether this call recorded it. Refusals carry a code: not_allowed, not_found, not_live, not_undoable, conflict, invalid or internal. Pins, merges, splits and alias decisions are not offered.",
	schema(`{"decision_id":{"type":"string","description":"your id for this decision; opaque, at most 256 bytes; reuse it to retry the same decision"},"action":{"type":"string","enum":["ratify","demote","undo"]},"stance_id":{"type":"string","description":"for ratify and demote: a stance id from get_bundle, stance_history or traverse"},"gesture_id":{"type":"string","description":"for undo: the gesture id an earlier decide answered with, or hearsay gestures list shows"}}`, "decision_id", "action")}

// Decided is what `decide` returns: the gesture, and whether this call
// recorded it. StanceID is set for a ratify or a demote, UndoneGestureID for an
// undo.
type Decided struct {
	GestureID       string `json:"gesture_id"`
	Action          string `json:"action"`
	StanceID        string `json:"stance_id,omitempty"`
	UndoneGestureID string `json:"undone_gesture_id,omitempty"`
	Recorded        bool   `json:"recorded"`
}

// refusal is a `decide` refusal: a status, a fixed sentence and its reason code.
func refusal(status int, code, message string) *Error {
	return &Error{Status: status, Message: message, Code: code}
}

func invalid(message string) *Error { return refusal(http.StatusBadRequest, ReasonInvalid, message) }

// errDecideNotFound is the one answer for a stance or gesture that does not
// exist, one the caller may not read, one out of their reach, and a replay
// whose target they have since lost: a refusal that told them apart would say
// which private document exists.
var errDecideNotFound = refusal(http.StatusNotFound, ReasonNotFound, "no such stance or gesture that you may read")

// decideEvent is the L0 event id the gesture is recorded under, which is its
// idempotency key: derived from the human, the carrier and the caller's
// decision id, so another person's decision ids never collide with this one's.
// Nothing is written to L0 under it; it only names the ledger entry.
func decideEvent(human, carrier, decision string) string {
	key, _ := json.Marshal([]string{human, carrier, decision})
	sum := sha256.Sum256(key)
	return connector.EventID(AuditSource, "decision-"+hex.EncodeToString(sum[:]))
}

// decide is `decide(decision_id, action, stance_id | gesture_id)`
// (docs/design.md#human-feedback-loop): the named human's ratify, demote or
// undo of one stance, recorded in the gesture ledger through the same entry
// point a reaction or the CLI uses, with the carrying agent beside the human.
// No model is called. The scope's ratify policy is checked against the human
// alone; the agent's class only decides whether it may carry.
//
// A request is handled in this order: its arguments and its carrier; a
// decision already recorded under the key, answered as a replay (the same
// content, whose target the caller can still read and reach) or refused as a
// conflict (another content), neither looking at liveness, undoability or
// policy; and otherwise a new decision, checked for read and reach, policy and
// liveness or undoability, in that order, by the ledger.
func decide(ctx context.Context, c *Calls, caller Caller, reader l1.Reader, raw json.RawMessage) (any, error) {
	out, err := c.decide(ctx, caller, reader, raw)
	if err == nil {
		return out, nil
	}
	var e *Error
	if errors.As(err, &e) {
		if e.Code == "" {
			// A shared failure raised below the call, such as a malformed body.
			e = &Error{Status: e.Status, Message: e.Message, Code: ReasonInvalid}
		}
		return nil, e
	}
	telemetry.Logger(ctx).ErrorContext(ctx, "decide failed", "principal", caller.Principal, "agent", caller.Agent, "error", err)
	return nil, refusal(errInternal.Status, ReasonInternal, errInternal.Message)
}

func (c *Calls) decide(ctx context.Context, caller Caller, reader l1.Reader, raw json.RawMessage) (*Decided, error) {
	var args struct {
		DecisionID string `json:"decision_id"`
		Action     string `json:"action"`
		StanceID   string `json:"stance_id"`
		GestureID  string `json:"gesture_id"`
	}
	if err := decode(raw, &args); err != nil {
		return nil, invalidFrom(err)
	}
	action := l2.GestureAction(args.Action)
	var undoes int64
	switch {
	case strings.TrimSpace(args.DecisionID) == "":
		return nil, invalid("decision_id is required")
	case len(args.DecisionID) > maxDecisionID:
		return nil, invalid("decision_id is over " + strconv.Itoa(maxDecisionID) + " bytes")
	case action != l2.GestureRatify && action != l2.GestureDemote && action != l2.GestureUndo:
		return nil, invalid("action is ratify, demote or undo: this call takes no other decision")
	case action == l2.GestureUndo && args.StanceID != "":
		return nil, invalid("an undo names a gesture_id, not a stance_id")
	case action == l2.GestureUndo && strings.TrimSpace(args.GestureID) == "":
		return nil, invalid("gesture_id is required for an undo")
	case action != l2.GestureUndo && args.GestureID != "":
		return nil, invalid("a " + args.Action + " names a stance_id, not a gesture_id")
	case action != l2.GestureUndo && strings.TrimSpace(args.StanceID) == "":
		return nil, invalid("stance_id is required for a " + args.Action)
	case len(args.StanceID) > maxTargetID:
		return nil, invalid("stance_id is over " + strconv.Itoa(maxTargetID) + " bytes")
	}
	if action == l2.GestureUndo {
		id, err := strconv.ParseInt(args.GestureID, 10, 64)
		if err != nil || id <= 0 {
			return nil, invalid("gesture_id is the number a decision answered with")
		}
		undoes = id
	}
	if err := c.mayCarry(caller); err != nil {
		return nil, err
	}
	if c.pool == nil {
		return nil, errors.New("the call layer has no database pool to record a decision with")
	}

	event := decideEvent(caller.Principal, caller.Agent, args.DecisionID)
	out := Decided{Action: args.Action, StanceID: args.StanceID}
	if undoes != 0 {
		out.UndoneGestureID = args.GestureID
	}
	// A decision already recorded under the key.
	prior, err := c.graph.GestureByEvent(ctx, event)
	switch {
	case err == nil:
		same := prior.Action == action && prior.Principal == caller.Principal && prior.Carrier == caller.Agent &&
			prior.Stance == args.StanceID && prior.Undoes == undoes
		if !same {
			return nil, refusal(http.StatusConflict, ReasonConflict, "this decision_id was already used for a different decision")
		}
		ok, err := c.decisionReadable(ctx, reader, prior)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errDecideNotFound
		}
		out.GestureID = strconv.FormatInt(prior.ID, 10)
		return &out, nil
	case !errors.Is(err, l2.ErrNotFound):
		return nil, err
	}

	// A new decision: read and reach first, so a target the caller may not read
	// is answered as one that does not exist whatever else is wrong with it.
	req := l2.GestureRequest{Event: event, Principal: caller.Principal, Action: action, Carrier: caller.Agent, DecisionsOnly: true}
	if action == l2.GestureUndo {
		target, err := c.graph.GestureByID(ctx, undoes)
		if errors.Is(err, l2.ErrNotFound) {
			return nil, errDecideNotFound
		}
		if err != nil {
			return nil, err
		}
		ok, err := c.gestureReadable(ctx, reader, target)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errDecideNotFound
		}
		req.Undoes = target.Event
	} else {
		ok, err := c.stanceReadable(ctx, reader, args.StanceID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errDecideNotFound
		}
		req.Stance = args.StanceID
	}
	g, recorded, err := l2.RecordGesture(ctx, c.pool, c.repo, req)
	if err != nil {
		return nil, decideRefusal(err)
	}
	out.GestureID = strconv.FormatInt(g.ID, 10)
	out.Recorded = recorded
	return &out, nil
}

// invalidFrom is a malformed-arguments refusal for an error the argument
// decoder raised.
func invalidFrom(err error) error {
	var e *Error
	if errors.As(err, &e) {
		return invalid(e.Message)
	}
	return err
}

// mayCarry refuses an agent whose class writes nothing: an observer may not
// carry a person's decision, as it may not assert. No class gains authority by
// carrying; the policy is the human's.
func (c *Calls) mayCarry(caller Caller) error {
	if caller.Agent == "" {
		return nil
	}
	agent, ok := c.resolver.Principal(caller.Agent)
	if !ok || !agent.Class.Rights().Write.Has(principal.WriteAssert) {
		return refusal(http.StatusForbidden, ReasonNotAllowed, "agent "+strconv.Quote(caller.Agent)+" is of class "+strconv.Quote(string(agent.Class))+", which may not carry a decision")
	}
	return nil
}

// decideRefusal maps what the ledger refused with to a refusal `decide`
// serves. Messages are fixed sentences: nothing the ledger says, which may name
// events and scopes, is served.
func decideRefusal(err error) error {
	switch {
	case errors.Is(err, l2.ErrEventConflict):
		return refusal(http.StatusConflict, ReasonConflict, "this decision_id was already used for a different decision")
	case errors.Is(err, l2.ErrNotUndoable):
		return refusal(http.StatusConflict, ReasonNotUndoable, "that gesture cannot be undone: it is already undone, is an undo, or is not a ratify or a demote")
	case errors.Is(err, l2.ErrNotLive):
		return refusal(http.StatusConflict, ReasonNotLive, "that stance is not live: a later reading superseded it, or it is withdrawn")
	case errors.Is(err, l2.ErrNotAllowed):
		return refusal(http.StatusForbidden, ReasonNotAllowed, "you may not ratify or demote by hand in that scope")
	case errors.Is(err, l2.ErrNotFound):
		return errDecideNotFound
	case errors.Is(err, l2.ErrInvalid):
		return invalid("the decision is not one that can be recorded")
	}
	return err
}

// decisionReadable is whether the caller may still read and reach what a
// recorded decision targeted: the stance it named, or what the gesture it
// undid covered.
func (c *Calls) decisionReadable(ctx context.Context, reader l1.Reader, g l2.Gesture) (bool, error) {
	if g.Action != l2.GestureUndo {
		return c.stanceReadable(ctx, reader, g.Stance)
	}
	target, err := c.graph.GestureByID(ctx, g.Undoes)
	if errors.Is(err, l2.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return c.gestureReadable(ctx, reader, target)
}

// stanceReadable is whether the reader may read a stance within their reach
// and the access lists of what it rests on, as `stance_history` would show it.
// A withdrawn stance is readable by those the withdrawal's audience names, so
// that it is refused as not live, not as missing.
func (c *Calls) stanceReadable(ctx context.Context, reader l1.Reader, id string) (bool, error) {
	st, err := c.graph.Stance(ctx, id)
	if errors.Is(err, l2.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	topic, err := c.graph.Topic(ctx, st.TopicID)
	if errors.Is(err, l2.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	assessed, err := c.graph.Assess(ctx, c.authority, reader, []l2.Topic{topic})
	if err != nil {
		return false, err
	}
	a := assessed[0]
	return a.Access.Topic(reader, topic) && (a.Access.Stance(reader, st) || a.Access.Withdrawal(reader, st)), nil
}

// gestureReadable is whether the reader may read what a gesture covered: every
// document it pointed at, and the stance it named.
func (c *Calls) gestureReadable(ctx context.Context, reader l1.Reader, g l2.Gesture) (bool, error) {
	for _, id := range g.Documents {
		doc, err := c.docs.Get(ctx, id)
		if errors.Is(err, l1.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !reader.MayRead(doc.Document) {
			return false, nil
		}
	}
	if g.Stance != "" {
		return c.stanceReadable(ctx, reader, g.Stance)
	}
	return true, nil
}
