package l2

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/principal"
	"github.com/kpenfound/hearsay/internal/telemetry"
)

// Commands applies the chat slash commands a person gives Hearsay in a
// source — `pin` and `merge` — for whichever process received them: the API's
// Discord interaction adapter, or the connector runtime for a connector that
// receives commands itself (ADR-0025). The command's L0 `command` event is
// recorded before it gets here.
//
// It maps the invoker's source identity to a configured human, reads as that
// person through their [View], and writes a pin through the gesture ledger
// ([RecordGesture], keyed by the command event) and a merge through the topic
// ledger ([Operate]). Both hold the scope's serial key and refuse a person its
// `ratified_by.principals` does not name. What it did, or why it did nothing,
// is a [connector.CommandResult]; the words a person reads are the caller's.
type Commands struct {
	db       *pgxpool.Pool
	repo     config.Repo
	resolver *principal.Resolver
}

// NewCommands builds the applier for a configuration.
func NewCommands(pool *pgxpool.Pool, repo config.Repo) (*Commands, error) {
	resolver, err := repo.Resolver()
	if err != nil {
		return nil, fmt.Errorf("building the identity resolver: %w", err)
	}
	return &Commands{db: pool, repo: repo, resolver: resolver}, nil
}

// Apply implements [connector.CommandApplier].
func (c *Commands) Apply(ctx context.Context, req connector.CommandRequest) connector.CommandResult {
	if req.Verb != connector.CommandPin && req.Verb != connector.CommandMerge {
		return connector.CommandResult{Outcome: connector.CommandUnknown}
	}
	human, refusal := c.person(req.Invoker)
	if refusal.Outcome != "" {
		return refusal
	}
	res := connector.CommandResult{Principal: human.ID, Kind: string(human.Kind)}
	view, err := NewView(ctx, New(c.db), c.repo, human)
	if err != nil {
		telemetry.Logger(ctx).ErrorContext(ctx, "reading as a command's invoker", "principal", human.ID, "error", err)
		res.Outcome = connector.CommandViewFailed
		return res
	}
	if req.Verb == connector.CommandPin {
		return c.pin(ctx, req, view, res)
	}
	return c.merge(ctx, req, view, res)
}

// person is the configured human an invoker maps to, or the refusal that
// says why there is none.
func (c *Commands) person(invoker connector.Identity) (principal.Principal, connector.CommandResult) {
	res := c.resolver.Resolve(invoker)
	switch res.Status {
	case principal.Resolved:
	case principal.Ambiguous:
		return principal.Principal{}, connector.CommandResult{Outcome: connector.CommandAmbiguous, Candidates: res.Candidates}
	default:
		return principal.Principal{}, connector.CommandResult{Outcome: connector.CommandUnmapped}
	}
	if res.Principal.Kind != principal.KindHuman {
		return principal.Principal{}, connector.CommandResult{Outcome: connector.CommandNotHuman, Principal: res.Principal.ID, Kind: string(res.Principal.Kind)}
	}
	return res.Principal, connector.CommandResult{}
}

// pin pins the L1 document of the command's target artifact, as an anchor in
// its scope.
func (c *Commands) pin(ctx context.Context, req connector.CommandRequest, view *View, res connector.CommandResult) connector.CommandResult {
	if req.Target == "" {
		res.Outcome = connector.CommandNoTarget
		return res
	}
	doc := l1.DocID(req.Source, req.Target)
	got, err := l1.New(c.db).Get(ctx, doc)
	switch {
	case errors.Is(err, l1.ErrNotFound):
		res.Outcome = connector.CommandUndistilled
		return res
	case err != nil:
		telemetry.Logger(ctx).ErrorContext(ctx, "reading a document to pin", "document", doc, "error", err)
		res.Outcome = connector.CommandReadFailed
		return res
	case !view.Reader().MayRead(got.Document):
		res.Outcome = connector.CommandUndistilled
		return res
	}
	g, recorded, err := RecordGesture(ctx, c.db, c.repo, GestureRequest{Event: req.Event, Principal: res.Principal, Action: GesturePin, Documents: []string{doc}})
	if err != nil {
		return ledgerRefusal(ctx, res, err)
	}
	res.Outcome, res.Scope, res.Gesture = connector.CommandPinned, g.Scope, g.ID
	if !recorded {
		res.Outcome = connector.CommandAlreadyPinned
	}
	return res
}

// merge merges one topic the person may read into another.
func (c *Commands) merge(ctx context.Context, req connector.CommandRequest, view *View, res connector.CommandResult) connector.CommandResult {
	if req.From == req.Into {
		res.Outcome = connector.CommandSameTopic
		return res
	}
	names := map[string]string{}
	for _, id := range []string{req.From, req.Into} {
		a, err := view.Topic(ctx, id)
		if err != nil {
			telemetry.Logger(ctx).ErrorContext(ctx, "reading a topic to merge", "topic", id, "error", err)
			res.Outcome = connector.CommandReadFailed
			return res
		}
		if a == nil {
			res.Outcome, res.Topic = connector.CommandNoSuchTopic, id
			return res
		}
		names[id] = a.Topic.Name
	}
	op, err := Operate(ctx, c.db, c.repo, OperationRequest{Kind: OperationMerge, Principal: res.Principal, From: req.From, Into: req.Into})
	if err != nil {
		return ledgerRefusal(ctx, res, err)
	}
	res.Outcome, res.Scope, res.Operation = connector.CommandMerged, op.Scope, op.ID
	res.FromName, res.IntoName = names[req.From], names[req.Into]
	return res
}

// ledgerRefusal is an error from a ledger write as a result: a refusal
// carries the ledger's reason, and anything else is logged and is a failure.
func ledgerRefusal(ctx context.Context, res connector.CommandResult, err error) connector.CommandResult {
	switch {
	case errors.Is(err, ErrNotAllowed):
		res.Outcome, res.Reason = connector.CommandNotAllowed, err.Error()
	case errors.Is(err, ErrInvalid), errors.Is(err, ErrNotFound), errors.Is(err, ErrConflict):
		res.Outcome, res.Reason = connector.CommandRejected, err.Error()
	default:
		telemetry.Logger(ctx).ErrorContext(ctx, "applying a command", "principal", res.Principal, "error", err)
		res.Outcome = connector.CommandFailed
	}
	return res
}

// TopicChoice is a topic offered for a merge argument.
type TopicChoice struct {
	ID, Name, Scope string
}

// ChoiceRequest asks which topics a merge argument may take.
type ChoiceRequest struct {
	// Invoker is who is filling the command in.
	Invoker connector.Identity
	// Typed is what they have typed so far; it matches a topic's name, case
	// folded, or its id.
	Typed string
	// From is, when choosing the topic to merge into, the topic already
	// chosen to merge away. Empty when choosing that one.
	From string
	// Limit is how many to collect: whole scopes are read until at least this
	// many are found. Zero is no limit.
	Limit int
}

// MergeChoices are the topics a merge argument may take: those the invoker's
// person may read, through the same [View] [Commands.Apply] reads through,
// whose name or id holds what they have typed. When From names a topic they
// may read, only the other topics in its scope are offered, since a merge
// stays inside one scope. An invoker who maps to no person is offered nothing,
// and so is a request out of time.
func (c *Commands) MergeChoices(ctx context.Context, req ChoiceRequest) []TopicChoice {
	human, refusal := c.person(req.Invoker)
	if refusal.Outcome != "" {
		return nil
	}
	log := telemetry.Logger(ctx)
	view, err := NewView(ctx, New(c.db), c.repo, human)
	if err != nil {
		log.ErrorContext(ctx, "reading as a command's invoker", "principal", human.ID, "error", err)
		return nil
	}
	typed := strings.ToLower(strings.TrimSpace(req.Typed))
	scopes := ScopeKeys(c.repo)
	exclude := ""
	if req.From != "" {
		a, err := view.Topic(ctx, req.From)
		if err != nil {
			log.ErrorContext(ctx, "reading a topic to merge", "topic", req.From, "error", err)
			return nil
		}
		if a != nil {
			scopes, exclude = []string{a.Topic.Scope}, a.Topic.ID
		}
	}
	var out []TopicChoice
	for _, scope := range scopes {
		topics, err := view.TopicsWhere(ctx, scope, func(t Topic) bool {
			return t.ID != exclude && (typed == "" || strings.Contains(strings.ToLower(t.Name), typed) || strings.Contains(t.ID, typed))
		})
		if err != nil {
			if ctx.Err() == nil {
				log.ErrorContext(ctx, "listing topics to merge", "scope", scope, "error", err)
			}
			break
		}
		for _, a := range topics {
			out = append(out, TopicChoice{ID: a.Topic.ID, Name: a.Topic.Name, Scope: scope})
		}
		if req.Limit > 0 && len(out) >= req.Limit {
			break
		}
	}
	return out
}
