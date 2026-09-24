package l2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/queue"
)

// GestureAction is what a person's gesture does (docs/design.md#human-feedback-loop).
type GestureAction string

// The gesture actions.
const (
	// GestureRatify confirms every live stance drawn from the documents: the
	// one a topic stands at is served as ratified ([Stand]).
	GestureRatify GestureAction = "ratify"
	// GestureDemote disputes them: the one a topic stands at is served as
	// contested, until a person ratifies it or a newer stance supersedes it.
	GestureDemote GestureAction = "demote"
	// GesturePin anchors each document in every entity its L1 scope names
	// ([Store.Pin]).
	GesturePin GestureAction = "pin"
	// GestureUndo reverses an earlier gesture.
	GestureUndo GestureAction = "undo"
)

// Valid reports whether a is one of the four.
func (a GestureAction) Valid() bool {
	return a == GestureRatify || a == GestureDemote || a == GesturePin || a == GestureUndo
}

// GestureHoldTarget starts the target of the assert job [RecordGesture] holds
// its scope with. The assertion worker only ever sees one when the gesture's
// process died holding the scope, and treats it as done.
const GestureHoldTarget = "gesture-hold:"

// GestureRequest is a person's gesture, as its source's caller read it: who
// made it, from which L0 event, and what it points at. Parsing the source's
// reaction or command, and mapping its actor to a principal, are the caller's.
type GestureRequest struct {
	// Event is the L0 event the gesture came from. It is the gesture's
	// idempotency key: a request naming an event already recorded records
	// nothing and returns what was recorded.
	Event string
	// Principal is the principal the source actor maps to.
	Principal string
	Action    GestureAction
	// Documents are the L1 documents the person pointed at, for a ratify, a
	// demote or a pin: the thread or burst holding the message they reacted
	// to, the issue they commented on.
	Documents []string
	// Undoes is, for an undo, the event of the gesture it reverses.
	Undoes string
}

// Validate reports a request that is refused before anything is read.
func (r GestureRequest) Validate() error {
	if _, _, err := connector.ParseEventID(r.Event); err != nil {
		return fmt.Errorf("%w: a gesture's event: %w", ErrInvalid, err)
	}
	if !r.Action.Valid() {
		return fmt.Errorf("%w: gesture action %q", ErrInvalid, r.Action)
	}
	if r.Action == GestureUndo {
		switch _, _, err := connector.ParseEventID(r.Undoes); {
		case err != nil:
			return fmt.Errorf("%w: an undo names the event of the gesture it undoes: %w", ErrInvalid, err)
		case r.Undoes == r.Event:
			return fmt.Errorf("%w: event %s cannot undo itself", ErrInvalid, r.Event)
		case len(r.Documents) > 0:
			return fmt.Errorf("%w: an undo covers what the gesture it undoes did, and names no documents", ErrInvalid)
		}
		return nil
	}
	if r.Undoes != "" {
		return fmt.Errorf("%w: a %s undoes nothing", ErrInvalid, r.Action)
	}
	if len(r.Documents) == 0 {
		return fmt.Errorf("%w: a %s names no document", ErrInvalid, r.Action)
	}
	for _, doc := range r.Documents {
		if !strings.HasPrefix(doc, "l1:") {
			return fmt.Errorf("%w: a %s names %q, which is not a document id", ErrInvalid, r.Action, doc)
		}
	}
	return nil
}

// Gesture is one entry of the gesture ledger, `l2_gestures`.
type Gesture struct {
	ID        int64
	Event     string
	Action    GestureAction
	Scope     string
	Principal string
	// Documents are the documents the gesture pointed at, sorted; an undo
	// repeats those of the gesture it undoes.
	Documents []string
	// Stances are the live stances drawn from the documents when a ratify or
	// a demote was recorded, sorted: what it applies to, then and after. An
	// undo repeats them.
	Stances []string
	// Pins are what a pin pinned, each with the gesture's principal and time;
	// an undo repeats them with neither.
	Pins []Pin
	// Undoes is the gesture an undo reverses, and UndoneBy the undo that
	// reversed this one; zero where there is none.
	Undoes, UndoneBy int64
	At               time.Time

	undoesEvent string
}

// CheckGesturer refuses a principal whose gesture is not recorded in a scope:
// anyone who is not a configured human, and a human the scope's authority does
// not let ratify by hand (`ratified_by.principals`). The error wraps
// [ErrNotAllowed] and says which, so a caller can explain the refusal.
func CheckGesturer(repo config.Repo, scope, id string) error {
	return checkHand(repo, scope, id, "a gesture", "ratifies, demotes or pins")
}

// RecordGesture records a person's gesture in the ledger of the scope its
// documents are in, and applies it, and returns it as stored and whether this
// call recorded it. A request whose event is already recorded records nothing
// and returns that gesture, and false.
//
// It runs under the scope's serial key as [Operate] does: it holds the key as
// an assert job would ([queue.Client.Hold]) and records the gesture in the
// transaction that releases it, so nothing the assertion worker writes to the
// scope interleaves with it. A caller already running under the key — an
// assert job for the gesture's event — calls [Store.ApplyGesture] in its own
// transaction instead.
//
// A refused gesture writes nothing: an unknown or unauthorized principal
// ([ErrNotAllowed]), documents no live stance is drawn from, or a pin of a
// document L1 does not hold ([ErrNotFound]), documents in two scopes and other
// malformed requests ([ErrInvalid]).
func RecordGesture(ctx context.Context, pool *pgxpool.Pool, repo config.Repo, req GestureRequest) (Gesture, bool, error) {
	if err := req.Validate(); err != nil {
		return Gesture{}, false, err
	}
	store := New(pool)
	if g, found, err := store.recorded(ctx, req); err != nil || found {
		return g, false, err
	}
	plan, err := store.planGesture(ctx, req)
	if err != nil {
		return Gesture{}, false, err
	}
	scope, err := plan.scopeKey(repo)
	if err != nil {
		return Gesture{}, false, err
	}
	if err := CheckGesturer(repo, scope, req.Principal); err != nil {
		return Gesture{}, false, err
	}
	client, err := queue.New(pool, queue.Config{Kind: AssertKind()})
	if err != nil {
		return Gesture{}, false, err
	}
	target, err := holdTarget(GestureHoldTarget)
	if err != nil {
		return Gesture{}, false, err
	}
	job, err := client.Hold(ctx, scope, target)
	if err != nil {
		return Gesture{}, false, err
	}
	var g Gesture
	var recorded bool
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if g, recorded, err = New(tx).ApplyGesture(ctx, repo, scope, req); err != nil {
			return err
		}
		held, err := client.CompleteIn(ctx, tx, job)
		if err != nil {
			return err
		}
		if !held {
			return fmt.Errorf("recording a %s in scope %q: the hold on the scope expired, so the scope may have changed under it", req.Action, scope)
		}
		return nil
	})
	if err != nil {
		// The transaction did not complete the hold; let the scope go.
		if _, released := client.Complete(context.WithoutCancel(ctx), job); released != nil {
			return Gesture{}, false, errors.Join(err, released)
		}
		return Gesture{}, false, err
	}
	return g, recorded, nil
}

// ApplyGesture is [RecordGesture] in the transaction the store runs on, which
// must hold the serial key scope names: it is decided from what the
// transaction reads, and the ledger entry, the pins and whatever else the
// caller writes commit together. A gesture whose documents are in another
// scope is refused.
func (s *Store) ApplyGesture(ctx context.Context, repo config.Repo, scope string, req GestureRequest) (Gesture, bool, error) {
	if err := req.Validate(); err != nil {
		return Gesture{}, false, err
	}
	if g, found, err := s.recorded(ctx, req); err != nil || found {
		return g, false, err
	}
	plan, err := s.planGesture(ctx, req)
	if err != nil {
		return Gesture{}, false, err
	}
	in, err := plan.scopeKey(repo)
	if err != nil {
		return Gesture{}, false, err
	}
	if in != scope {
		return Gesture{}, false, fmt.Errorf("%w: the %s from %s is in scope %q, and is being recorded under %q", ErrInvalid, req.Action, req.Event, in, scope)
	}
	if err := CheckGesturer(repo, scope, req.Principal); err != nil {
		return Gesture{}, false, err
	}
	pinning := req.Action == GesturePin || plan.undoes.Action == GesturePin
	if pinning {
		if err := lockPins(ctx, s.db); err != nil {
			return Gesture{}, false, err
		}
	}
	g := Gesture{
		Event: req.Event, Action: req.Action, Scope: scope, Principal: req.Principal,
		Documents: sortedUnique(req.Documents), Stances: plan.stances, Pins: plan.pins,
	}
	if req.Action == GestureUndo {
		g.Documents, g.Undoes = plan.undoes.Documents, plan.undoes.ID
	}
	if g, err = s.appendGesture(ctx, g); err != nil {
		return Gesture{}, false, err
	}
	switch {
	case req.Action == GesturePin:
		for _, p := range g.Pins {
			if _, err := s.Pin(ctx, p); err != nil {
				return Gesture{}, false, err
			}
		}
	case plan.undoes.Action == GesturePin:
		for _, p := range g.Pins {
			if err := s.resyncPin(ctx, p.Scope, p.L1); err != nil {
				return Gesture{}, false, err
			}
		}
	}
	return g, true, nil
}

// recorded is the gesture already recorded from the request's event, if there
// is one. A retry of the same gesture finds it; a different gesture from the
// same event is refused, because an event is one gesture.
func (s *Store) recorded(ctx context.Context, req GestureRequest) (Gesture, bool, error) {
	g, err := s.GestureByEvent(ctx, req.Event)
	if errors.Is(err, ErrNotFound) {
		return Gesture{}, false, nil
	}
	if err != nil {
		return Gesture{}, false, err
	}
	same := g.Action == req.Action && g.Principal == req.Principal && g.undoesEvent == req.Undoes &&
		(req.Action == GestureUndo || slices.Equal(g.Documents, sortedUnique(req.Documents)))
	if !same {
		return Gesture{}, false, fmt.Errorf("%w: event %s is already gesture %d, a %s by %q", ErrInvalid, req.Event, g.ID, g.Action, g.Principal)
	}
	return g, true, nil
}

// gesturePlan is what a gesture would record, read before it is authorized.
type gesturePlan struct {
	// scope is the serial key of the stances' topics or of the gesture undone;
	// empty for a pin, whose key is its documents'.
	scope   string
	stances []string
	pins    []Pin
	// keys are the pinned documents' source and container, from which a
	// pin's serial key is computed ([ScopeKey]).
	keys   [][2]string
	undoes Gesture
}

// scopeKey is the serial key the gesture is recorded under. Documents to pin
// in two scopes are refused, as stances in two are.
func (p gesturePlan) scopeKey(repo config.Repo) (string, error) {
	if p.scope != "" {
		return p.scope, nil
	}
	scope := ScopeKey(repo, p.keys[0][0], p.keys[0][1])
	for _, k := range p.keys[1:] {
		if other := ScopeKey(repo, k[0], k[1]); other != scope {
			return "", fmt.Errorf("%w: the documents to pin are in scopes %q and %q, and a gesture stays inside one scope", ErrInvalid, scope, other)
		}
	}
	return scope, nil
}

func (s *Store) planGesture(ctx context.Context, req GestureRequest) (gesturePlan, error) {
	switch req.Action {
	case GestureUndo:
		g, err := s.GestureByEvent(ctx, req.Undoes)
		switch {
		case errors.Is(err, ErrNotFound):
			return gesturePlan{}, fmt.Errorf("%w: no gesture was recorded from event %s", ErrNotFound, req.Undoes)
		case err != nil:
			return gesturePlan{}, err
		case g.Action == GestureUndo:
			return gesturePlan{}, fmt.Errorf("%w: gesture %d is an undo, and an undo is not undone: make the gesture again", ErrInvalid, g.ID)
		case g.UndoneBy != 0:
			return gesturePlan{}, fmt.Errorf("%w: gesture %d is already undone, by gesture %d", ErrInvalid, g.ID, g.UndoneBy)
		}
		pins := make([]Pin, len(g.Pins))
		for i, p := range g.Pins {
			pins[i] = Pin{Scope: p.Scope, L1: p.L1}
		}
		return gesturePlan{scope: g.Scope, stances: g.Stances, pins: pins, undoes: g}, nil
	case GesturePin:
		return s.planPin(ctx, sortedUnique(req.Documents))
	}
	return s.planStances(ctx, sortedUnique(req.Documents))
}

// planStances finds the live stances drawn from the documents: every stance
// that cites one of them, that a later reading of its own document has not
// retired and that is not a withdrawal. A stance an agent asserted citing one
// is the agent's, not the document's, and is not among them.
func (s *Store) planStances(ctx context.Context, docs []string) (gesturePlan, error) {
	rows, err := s.db.Query(ctx, `
SELECT s.id, t.scope FROM l2_stances s JOIN l2_topics t ON t.id = s.topic_id
WHERE s.evidence && $1::text[] AND s.assertion IS NULL AND NOT s.withdrawn AND NOT `+RetiredSQL+`
ORDER BY s.id`, docs)
	if err != nil {
		return gesturePlan{}, fmt.Errorf("finding the live stances drawn from %v: %w", docs, err)
	}
	defer rows.Close()
	var plan gesturePlan
	for rows.Next() {
		var id, scope string
		if err := rows.Scan(&id, &scope); err != nil {
			return gesturePlan{}, fmt.Errorf("finding the live stances drawn from %v: %w", docs, err)
		}
		if plan.scope != "" && scope != plan.scope {
			return gesturePlan{}, fmt.Errorf("%w: the stances drawn from %v are in scopes %q and %q, and a gesture stays inside one scope",
				ErrInvalid, docs, plan.scope, scope)
		}
		plan.scope = scope
		plan.stances = append(plan.stances, id)
	}
	if err := rows.Err(); err != nil {
		return gesturePlan{}, fmt.Errorf("finding the live stances drawn from %v: %w", docs, err)
	}
	if len(plan.stances) == 0 {
		return gesturePlan{}, fmt.Errorf("%w: no live stance is drawn from %v", ErrNotFound, docs)
	}
	return plan, nil
}

// planPin reads the documents a pin names as L1 holds them now: the entities
// each one is about, where it is pinned, and the source and container of its
// artifact, which decide its serial key.
func (s *Store) planPin(ctx context.Context, docs []string) (gesturePlan, error) {
	rows, err := s.db.Query(ctx, `
SELECT d.id, d.scope, d.source, coalesce(e.payload->'container'->>'native_id', '')
FROM l1_docs d LEFT JOIN l0_events e ON e.id = d.l0_refs[1]
WHERE d.id = ANY($1) ORDER BY d.id`, docs)
	if err != nil {
		return gesturePlan{}, fmt.Errorf("reading the documents to pin: %w", err)
	}
	defer rows.Close()
	var plan gesturePlan
	var found []string
	for rows.Next() {
		var id, source, container string
		var entities []string
		if err := rows.Scan(&id, &entities, &source, &container); err != nil {
			return gesturePlan{}, fmt.Errorf("reading the documents to pin: %w", err)
		}
		if len(entities) == 0 {
			return gesturePlan{}, fmt.Errorf("%w: document %s is about no entity, so there is nowhere to pin it", ErrInvalid, id)
		}
		found = append(found, id)
		plan.keys = append(plan.keys, [2]string{source, container})
		for _, entity := range entities {
			plan.pins = append(plan.pins, Pin{Scope: entity, L1: id})
		}
	}
	if err := rows.Err(); err != nil {
		return gesturePlan{}, fmt.Errorf("reading the documents to pin: %w", err)
	}
	if missing := slices.DeleteFunc(slices.Clone(docs), func(d string) bool { return slices.Contains(found, d) }); len(missing) > 0 {
		return gesturePlan{}, fmt.Errorf("%w: document %s", ErrNotFound, strings.Join(missing, ", "))
	}
	return plan, nil
}

// pinRef is a pin as the ledger records it.
type pinRef struct {
	Scope string `json:"scope"`
	L1    string `json:"l1"`
}

func (s *Store) appendGesture(ctx context.Context, g Gesture) (Gesture, error) {
	refs := make([]pinRef, len(g.Pins))
	for i, p := range g.Pins {
		refs[i] = pinRef{Scope: p.Scope, L1: p.L1}
	}
	pins, err := json.Marshal(refs)
	if err != nil {
		return Gesture{}, fmt.Errorf("encoding the pins of a %s: %w", g.Action, err)
	}
	var undoes *int64
	if g.Undoes != 0 {
		undoes = &g.Undoes
	}
	err = s.db.QueryRow(ctx, `
INSERT INTO l2_gestures (event, action, scope, principal, documents, stances, pins, undoes)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id, created_at`, g.Event, string(g.Action), g.Scope, g.Principal, orEmpty(g.Documents), orEmpty(g.Stances), pins, undoes).Scan(&g.ID, &g.At)
	if err != nil {
		return Gesture{}, fmt.Errorf("recording a %s in scope %q: %w", g.Action, g.Scope, err)
	}
	g.At = g.At.UTC()
	if g.Action == GesturePin {
		for i := range g.Pins {
			g.Pins[i].PinnedBy, g.Pins[i].PinnedAt = g.Principal, g.At
		}
	}
	return g, nil
}

// gestureInForceSQL is whether the gesture g is in force: no undo reverses
// it, and an operator deletion has not deleted the event it came from
// (ADR-0018), which takes the gesture back with it.
const gestureInForceSQL = `(NOT EXISTS (SELECT 1 FROM l2_gestures x WHERE x.undoes = g.id)
  AND NOT EXISTS (SELECT 1 FROM l0_events e WHERE e.id = g.event AND e.deletion IS NOT NULL))`

const gestureColumns = `g.id, g.event, g.action, g.scope, g.principal, g.documents, g.stances, g.pins,
    coalesce(g.undoes, 0), coalesce(v.event, ''), coalesce(u.id, 0), g.created_at`

const gestureFrom = `l2_gestures g LEFT JOIN l2_gestures u ON u.undoes = g.id LEFT JOIN l2_gestures v ON v.id = g.undoes`

// GestureByEvent returns the gesture recorded from an L0 event.
func (s *Store) GestureByEvent(ctx context.Context, event string) (Gesture, error) {
	g, err := scanGesture(s.db.QueryRow(ctx, `SELECT `+gestureColumns+` FROM `+gestureFrom+` WHERE g.event = $1`, event))
	if errors.Is(err, pgx.ErrNoRows) {
		return Gesture{}, fmt.Errorf("%w: no gesture from event %s", ErrNotFound, event)
	}
	if err != nil {
		return Gesture{}, fmt.Errorf("reading the gesture from event %s: %w", event, err)
	}
	return g, nil
}

// Gestures is a scope's gesture ledger, oldest first: who ratified, demoted,
// pinned or undid what, when, and the undo that reversed each, if one has.
// An empty scope is every scope's.
func (s *Store) Gestures(ctx context.Context, scope string) ([]Gesture, error) {
	rows, err := s.db.Query(ctx, `SELECT `+gestureColumns+` FROM `+gestureFrom+`
WHERE $1 = '' OR g.scope = $1 ORDER BY g.id`, scope)
	if err != nil {
		return nil, fmt.Errorf("listing gestures: %w", err)
	}
	defer rows.Close()
	out := []Gesture{}
	for rows.Next() {
		g, err := scanGesture(rows)
		if err != nil {
			return nil, fmt.Errorf("listing gestures: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing gestures: %w", err)
	}
	return out, nil
}

func scanGesture(row scanner) (Gesture, error) {
	var g Gesture
	var action string
	var pins []byte
	if err := row.Scan(&g.ID, &g.Event, &action, &g.Scope, &g.Principal, &g.Documents, &g.Stances, &pins,
		&g.Undoes, &g.undoesEvent, &g.UndoneBy, &g.At); err != nil {
		return Gesture{}, err
	}
	g.Action = GestureAction(action)
	g.At = g.At.UTC()
	var refs []pinRef
	if err := json.Unmarshal(pins, &refs); err != nil {
		return Gesture{}, fmt.Errorf("decoding the pins of gesture %d: %w", g.ID, err)
	}
	g.Pins = make([]Pin, len(refs))
	for i, r := range refs {
		g.Pins[i] = Pin{Scope: r.Scope, L1: r.L1}
		if g.Action == GesturePin {
			g.Pins[i].PinnedBy, g.Pins[i].PinnedAt = g.Principal, g.At
		}
	}
	return g, nil
}

// Corrections is what the gestures in force say about some stances: which a
// person ratified and which one demoted. Each stance is in at most one of the
// two, that of the latest gesture in force that applied to it.
type Corrections struct {
	Ratified, Demoted []string
}

// Corrections reads the gestures in force on these stances ([TierInputs]).
// A gesture an undo reversed, or whose event an operator deletion deleted, is
// not in force.
func (s *Store) Corrections(ctx context.Context, stanceIDs []string) (Corrections, error) {
	out := Corrections{Ratified: []string{}, Demoted: []string{}}
	if len(stanceIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `
SELECT g.action, g.stances FROM l2_gestures g
WHERE g.action IN ('ratify', 'demote') AND g.stances && $1::text[] AND `+gestureInForceSQL+`
ORDER BY g.id`, stanceIDs)
	if err != nil {
		return out, fmt.Errorf("reading the gestures on stances: %w", err)
	}
	defer rows.Close()
	latest := map[string]GestureAction{}
	for rows.Next() {
		var action string
		var stances []string
		if err := rows.Scan(&action, &stances); err != nil {
			return out, fmt.Errorf("reading the gestures on stances: %w", err)
		}
		for _, id := range stances {
			latest[id] = GestureAction(action)
		}
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("reading the gestures on stances: %w", err)
	}
	for _, id := range sortedUnique(stanceIDs) {
		switch latest[id] {
		case GestureRatify:
			out.Ratified = append(out.Ratified, id)
		case GestureDemote:
			out.Demoted = append(out.Demoted, id)
		}
	}
	return out, nil
}

// pinLock is the advisory lock every change to the pins gestures made takes,
// so that a pin, its undo and an operator deletion decide a document's pin
// one after the other. It is taken for a transaction ([lockPins]) or, by a
// deletion, for the session its transaction runs on ([HoldPins]).
const pinLock int64 = 0x68656172_73617932 // "hearsay2"

func lockPins(ctx context.Context, q Querier) error {
	if _, err := q.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, pinLock); err != nil {
		return fmt.Errorf("locking the pins: %w", err)
	}
	return nil
}

// HoldPins takes the pin lock on a connection until the returned function
// lets it go. An operator deletion takes it before it begins its transaction,
// whose snapshot then already sees every gesture recorded before it, and
// calls [Store.ResyncDeletedPins] in it. Letting go closes a connection that
// cannot unlock, so that no lock goes back into the pool.
func HoldPins(ctx context.Context, conn *pgxpool.Conn) (func(), error) {
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, pinLock); err != nil {
		return nil, fmt.Errorf("locking the pins: %w", err)
	}
	return func() {
		ctx := context.WithoutCancel(ctx)
		if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, pinLock); err != nil {
			_ = conn.Conn().Close(ctx)
		}
	}, nil
}

// ResyncDeletedPins takes out every pin that a gesture from one of these
// events made, now that an operator deletion has deleted them and taken the
// gestures out of force, and pins each document again as the first gesture
// still in force that pinned it did. Call it in the deletion's transaction,
// after the events are deleted, holding [HoldPins].
func (s *Store) ResyncDeletedPins(ctx context.Context, events []string) error {
	if len(events) == 0 {
		return nil
	}
	rows, err := s.db.Query(ctx, `
SELECT DISTINCT p->>'scope', p->>'l1' FROM l2_gestures g, jsonb_array_elements(g.pins) p
WHERE g.action = 'pin' AND g.event = ANY($1) ORDER BY 1, 2`, events)
	if err != nil {
		return fmt.Errorf("finding the pins deleted gestures made: %w", err)
	}
	pins, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Pin, error) {
		var p Pin
		return p, row.Scan(&p.Scope, &p.L1)
	})
	if err != nil {
		return fmt.Errorf("finding the pins deleted gestures made: %w", err)
	}
	for _, p := range pins {
		if err := s.resyncPin(ctx, p.Scope, p.L1); err != nil {
			return err
		}
	}
	return nil
}

// resyncPin makes a document's pin on an entity what the pin gestures in force
// say: a pin a gesture made that is no longer in force is taken out, and a
// document no pin holds is pinned as the first gesture in force that pinned it
// did. A pin no gesture made is left as it is.
func (s *Store) resyncPin(ctx context.Context, scope, doc string) error {
	ref, err := json.Marshal([]pinRef{{Scope: scope, L1: doc}})
	if err != nil {
		return fmt.Errorf("encoding the pin of %s on %s: %w", doc, scope, err)
	}
	var by string
	var at time.Time
	err = s.db.QueryRow(ctx, `SELECT pinned_by, pinned_at FROM l2_pins WHERE scope = $1 AND l1 = $2`, scope, doc).Scan(&by, &at)
	pinned := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("reading the pin of %s on %s: %w", doc, scope, err)
	}
	if pinned {
		var inForce *bool
		if err := s.db.QueryRow(ctx, `
SELECT bool_or(`+gestureInForceSQL+`) FROM l2_gestures g
WHERE g.action = 'pin' AND g.pins @> $1::jsonb AND g.principal = $2 AND g.created_at = $3`, ref, by, at).Scan(&inForce); err != nil {
			return fmt.Errorf("reading the gesture behind the pin of %s on %s: %w", doc, scope, err)
		}
		if inForce == nil || *inForce {
			return nil
		}
		if _, err := s.Unpin(ctx, scope, doc); err != nil {
			return err
		}
	}
	var next Pin
	err = s.db.QueryRow(ctx, `
SELECT g.principal, g.created_at FROM l2_gestures g
WHERE g.action = 'pin' AND g.pins @> $1::jsonb AND `+gestureInForceSQL+`
ORDER BY g.id LIMIT 1`, ref).Scan(&next.PinnedBy, &next.PinnedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading the pin gestures on %s in %s: %w", doc, scope, err)
	}
	next.Scope, next.L1 = scope, doc
	_, err = s.Pin(ctx, next)
	return err
}
