package l2

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/principal"
	"github.com/kpenfound/hearsay/internal/queue"
)

// OperationTarget starts the target of the assert job a topic operation holds
// its scope with ([Operate]). The assertion worker only ever sees one when
// the operation's process died holding the scope, and treats it as done.
const OperationTarget = "topic-operation:"

// CheckOperator refuses a principal who may not merge, split or undo in a
// scope: anyone who is not a configured human, and a human the scope's
// authority does not let ratify by hand (`ratified_by.principals`). No agent
// may, whatever its class.
func CheckOperator(repo config.Repo, scope, id string) error {
	return checkHand(repo, scope, id, "a topic operation", "changes topics")
}

// checkHand is the rule a correction by hand is held to: a configured human
// whom the scope's authority lets ratify by hand.
func checkHand(repo config.Repo, scope, id, what, does string) error {
	p, ok := repo.Principal(id)
	switch {
	case id == "":
		return fmt.Errorf("%w: %s names no principal", ErrNotAllowed, what)
	case !ok:
		return fmt.Errorf("%w: %q is not a configured principal", ErrNotAllowed, id)
	case p.Kind != principal.KindHuman:
		return fmt.Errorf("%w: %q is a %s, and only a person %s", ErrNotAllowed, id, p.Kind, does)
	case !repo.Authority.ForScope(scope).RatifiedByPrincipal(id):
		return fmt.Errorf("%w: %q may not ratify by hand in scope %q", ErrNotAllowed, id, scope)
	}
	return nil
}

// Operate records a merge, a split or an undo in the ledger of the scope its
// topics are in, as the principal the request names, and returns it as
// stored.
//
// It runs under the scope's serial key: it holds the key as an assert job
// would ([queue.Client.Hold]), waiting for an assert job already running on
// the scope, and records the operation in the transaction that releases the
// key, so nothing the assertion worker writes to the scope interleaves with
// it and two operations on one scope are decided one after the other. Other
// scopes are not held.
//
// A refused operation writes nothing to the ledger. Nothing here changes a
// topic or a stance row: what the ledger makes of them is read from it.
func Operate(ctx context.Context, pool *pgxpool.Pool, repo config.Repo, req OperationRequest) (Operation, error) {
	if err := req.Validate(); err != nil {
		return Operation{}, err
	}
	scope, err := New(pool).operationScope(ctx, req)
	if err != nil {
		return Operation{}, err
	}
	if err := CheckOperator(repo, scope, req.Principal); err != nil {
		return Operation{}, err
	}
	client, err := queue.New(pool, queue.Config{Kind: AssertKind()})
	if err != nil {
		return Operation{}, err
	}
	target, err := operationTarget()
	if err != nil {
		return Operation{}, err
	}
	job, err := client.Hold(ctx, scope, target)
	if err != nil {
		return Operation{}, err
	}
	var op Operation
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if op, err = New(tx).operate(ctx, scope, req); err != nil {
			return err
		}
		held, err := client.CompleteIn(ctx, tx, job)
		if err != nil {
			return err
		}
		if !held {
			return fmt.Errorf("recording a %s in scope %q: the hold on the scope expired, so the scope may have changed under it", req.Kind, scope)
		}
		return nil
	})
	if err != nil {
		// The transaction did not complete the hold; let the scope go.
		if _, released := client.Complete(context.WithoutCancel(ctx), job); released != nil {
			return Operation{}, errors.Join(err, released)
		}
		return Operation{}, err
	}
	return op, nil
}

// ApplyOperation is [Operate] in the transaction the store runs on, which
// must hold the serial key scope names: an assert job that runs a person's
// command, whose record commits with the operation. An operation on topics in
// another scope is refused, as is one [Operate] would refuse.
func (s *Store) ApplyOperation(ctx context.Context, repo config.Repo, scope string, req OperationRequest) (Operation, error) {
	if err := req.Validate(); err != nil {
		return Operation{}, err
	}
	in, err := s.operationScope(ctx, req)
	if err != nil {
		return Operation{}, err
	}
	if in != scope {
		return Operation{}, fmt.Errorf("%w: the %s is in scope %q, and is being recorded under %q", ErrInvalid, req.Kind, in, scope)
	}
	if err := CheckOperator(repo, scope, req.Principal); err != nil {
		return Operation{}, err
	}
	return s.operate(ctx, scope, req)
}

// operate decides a request against the scope's ledger and appends it, in the
// transaction holding the scope's key.
func (s *Store) operate(ctx context.Context, scope string, req OperationRequest) (Operation, error) {
	state, err := s.scopeState(ctx, scope)
	if err != nil {
		return Operation{}, err
	}
	decided, err := state.Decide(req)
	if err != nil {
		return Operation{}, err
	}
	return s.appendOperation(ctx, decided)
}

func operationTarget() (string, error) {
	return holdTarget(OperationTarget)
}

// holdTarget mints the target of a hold's assert job: prefix and 128 random
// bits, so no two holds share a job.
func holdTarget(prefix string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("minting a hold's job target: %w", err)
	}
	return prefix + hex.EncodeToString(b[:]), nil
}

// operationScope is the scope a request's topics are in, read before the
// scope is held: a topic never changes scope, and neither does an operation.
// A merge of topics in two scopes is refused here.
func (s *Store) operationScope(ctx context.Context, req OperationRequest) (string, error) {
	if req.Kind == OperationUndo {
		var scope string
		err := s.db.QueryRow(ctx, `SELECT scope FROM l2_topic_operations WHERE id = $1`, req.Undoes).Scan(&scope)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("%w: operation %d", ErrNotFound, req.Undoes)
		}
		if err != nil {
			return "", fmt.Errorf("reading the scope of operation %d: %w", req.Undoes, err)
		}
		return scope, nil
	}
	topics := []string{req.Topic}
	if req.Kind == OperationMerge {
		topics = []string{req.Into, req.From}
	}
	scopes := make([]string, len(topics))
	for i, t := range topics {
		scope, err := s.topicScope(ctx, t)
		if err != nil {
			return "", err
		}
		scopes[i] = scope
	}
	if len(scopes) == 2 && scopes[0] != scopes[1] {
		return "", fmt.Errorf("%w: topic %s is in scope %q and topic %s in scope %q, and a merge stays inside one scope",
			ErrInvalid, topics[0], scopes[0], topics[1], scopes[1])
	}
	return scopes[0], nil
}

// topicScope is the scope of a topic row, or of a topic a split created.
func (s *Store) topicScope(ctx context.Context, id string) (string, error) {
	var scope string
	err := s.db.QueryRow(ctx, `
SELECT scope FROM l2_topics WHERE id = $1
UNION ALL
SELECT scope FROM l2_topic_operations WHERE kind = 'split' AND topics && ARRAY[$1] AND topics[2] = $1
LIMIT 1`, id).Scan(&scope)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("%w: topic %s", ErrNotFound, id)
	}
	if err != nil {
		return "", fmt.Errorf("reading the scope of topic %s: %w", id, err)
	}
	return scope, nil
}

// scopeState reads what an operation on a scope is decided against.
func (s *Store) scopeState(ctx context.Context, scope string) (ScopeState, error) {
	state := ScopeState{Scope: scope, Stances: map[string]string{}}
	rows, err := s.db.Query(ctx, `
SELECT t.id, s.id FROM l2_topics t LEFT JOIN l2_stances s ON s.topic_id = t.id
WHERE t.scope = $1 ORDER BY t.id, s.id`, scope)
	if err != nil {
		return ScopeState{}, fmt.Errorf("reading the topics of scope %q: %w", scope, err)
	}
	defer rows.Close()
	for rows.Next() {
		var topic string
		var stance *string
		if err := rows.Scan(&topic, &stance); err != nil {
			return ScopeState{}, fmt.Errorf("reading the topics of scope %q: %w", scope, err)
		}
		if len(state.Topics) == 0 || state.Topics[len(state.Topics)-1] != topic {
			state.Topics = append(state.Topics, topic)
		}
		if stance != nil {
			state.Stances[*stance] = topic
		}
	}
	if err := rows.Err(); err != nil {
		return ScopeState{}, fmt.Errorf("reading the topics of scope %q: %w", scope, err)
	}
	if state.Operations, err = s.Operations(ctx, OperationFilter{Scope: scope}); err != nil {
		return ScopeState{}, err
	}
	return state, nil
}

// appendOperation writes a decided operation to the ledger.
func (s *Store) appendOperation(ctx context.Context, op Operation) (Operation, error) {
	var name *string
	if op.Name != "" {
		name = &op.Name
	}
	var undoes *int64
	if op.Undoes != 0 {
		undoes = &op.Undoes
	}
	err := s.db.QueryRow(ctx, `
INSERT INTO l2_topic_operations (kind, scope, principal, topics, stances, name, undoes)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id, created_at`, string(op.Kind), op.Scope, op.Principal, op.Topics, orEmpty(op.Stances), name, undoes).Scan(&op.ID, &op.At)
	if err != nil {
		return Operation{}, fmt.Errorf("recording a %s in scope %q: %w", op.Kind, op.Scope, err)
	}
	op.At = op.At.UTC()
	return op, nil
}

// OperationFilter narrows [Store.Operations]. A zero field does not filter.
type OperationFilter struct {
	Kind  OperationKind
	Scope string
	// Since and Until bound when the operation was recorded: at or after
	// Since, and before Until.
	Since, Until time.Time
}

const operationColumns = `o.id, o.kind, o.scope, o.principal, o.topics, o.stances, coalesce(o.name, ''),
    coalesce(o.undoes, 0), coalesce(u.id, 0), o.created_at`

// Operations is the ledger, oldest first: who did what to which topics and
// stances, when, and the undo that reversed it, if one has. It is what the
// `hearsay topics` log and the merge and split rate read.
func (s *Store) Operations(ctx context.Context, f OperationFilter) ([]Operation, error) {
	if f.Kind != "" && !f.Kind.Valid() {
		return nil, fmt.Errorf("%w: topic operation kind %q", ErrInvalid, f.Kind)
	}
	var since, until *time.Time
	if !f.Since.IsZero() {
		since = &f.Since
	}
	if !f.Until.IsZero() {
		until = &f.Until
	}
	rows, err := s.db.Query(ctx, `
SELECT `+operationColumns+`
FROM l2_topic_operations o LEFT JOIN l2_topic_operations u ON u.undoes = o.id
WHERE ($1 = '' OR o.kind = $1) AND ($2 = '' OR o.scope = $2)
  AND ($3::timestamptz IS NULL OR o.created_at >= $3) AND ($4::timestamptz IS NULL OR o.created_at < $4)
ORDER BY o.id`, string(f.Kind), f.Scope, since, until)
	if err != nil {
		return nil, fmt.Errorf("listing topic operations: %w", err)
	}
	defer rows.Close()
	out := []Operation{}
	for rows.Next() {
		op, err := scanOperation(rows)
		if err != nil {
			return nil, fmt.Errorf("listing topic operations: %w", err)
		}
		out = append(out, op)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing topic operations: %w", err)
	}
	return out, nil
}

// Operation returns one entry of the ledger by id.
func (s *Store) Operation(ctx context.Context, id int64) (Operation, error) {
	op, err := scanOperation(s.db.QueryRow(ctx, `
SELECT `+operationColumns+`
FROM l2_topic_operations o LEFT JOIN l2_topic_operations u ON u.undoes = o.id
WHERE o.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Operation{}, fmt.Errorf("%w: operation %d", ErrNotFound, id)
	}
	if err != nil {
		return Operation{}, fmt.Errorf("reading operation %d: %w", id, err)
	}
	return op, nil
}

func scanOperation(row scanner) (Operation, error) {
	var op Operation
	var kind string
	if err := row.Scan(&op.ID, &kind, &op.Scope, &op.Principal, &op.Topics, &op.Stances, &op.Name,
		&op.Undoes, &op.UndoneBy, &op.At); err != nil {
		return Operation{}, err
	}
	op.Kind = OperationKind(kind)
	op.At = op.At.UTC()
	return op, nil
}
