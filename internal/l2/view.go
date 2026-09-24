package l2

import (
	"context"
	"errors"
	"slices"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/principal"
)

// View is the graph as one configured human reads it when they call on
// Hearsay directly, with no agent: `hearsay topics`, `hearsay gestures` and
// chat commands ([Commands]) all read topics through it, so a topic one
// of them offers is one the others would. It is the reader the API builds for
// the same person (`stance_history`): their configured scopes and the code
// those link to, expanded over the graph, and their identities for the access
// lists. A topic they may not read and one that does not exist are one answer.
//
// A View remembers the topics it has assessed and is not safe for concurrent
// use; build one per request.
type View struct {
	store     *Store
	authority config.Authority
	reader    l1.Reader
	topics    map[string]*Assessment
}

// NewView builds the view of a configured human. A principal of another kind
// is refused.
func NewView(ctx context.Context, store *Store, repo config.Repo, human principal.Principal) (*View, error) {
	eff, err := principal.HumanRead(human, human.Grant)
	if err != nil {
		return nil, err
	}
	if eff, err = principal.Reach(ctx, store, eff, human.Grant.Scopes, principal.Scopes{}); err != nil {
		return nil, err
	}
	resolver, err := repo.Resolver()
	if err != nil {
		return nil, err
	}
	reader, err := l1.ReaderFor(resolver, eff)
	if err != nil {
		return nil, err
	}
	return &View{store: store, authority: repo.Authority, reader: reader, topics: map[string]*Assessment{}}, nil
}

// Store is the store the view reads.
func (v *View) Store() *Store { return v.store }

// Reader is the person's reader: their reach and their identities.
func (v *View) Reader() l1.Reader { return v.reader }

// Topic is the topic an id reads as now, with its history, or nil where the
// person may not read it or there is no such topic.
func (v *View) Topic(ctx context.Context, id string) (*Assessment, error) {
	if a, done := v.topics[id]; done {
		return a, nil
	}
	t, err := v.store.Topic(ctx, id)
	if errors.Is(err, ErrNotFound) {
		v.topics[id] = nil
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	assessed, err := v.store.Assess(ctx, v.authority, v.reader, []Topic{t})
	if err != nil {
		return nil, err
	}
	var a *Assessment
	if assessed[0].Access.Topic(v.reader, t) {
		a = &assessed[0]
	}
	v.topics[id] = a
	return a, nil
}

// Topics are the topics in a scope the person may read, as the ledger makes
// them now, oldest first. A scope that does not exist and one with nothing
// they may read both have none.
func (v *View) Topics(ctx context.Context, scope string) ([]Assessment, error) {
	return v.TopicsWhere(ctx, scope, nil)
}

// TopicsWhere are the topics in a scope the person may read and keep takes,
// oldest first. keep sees each topic before it is assessed, so a caller
// looking for a few topics by name does not assess the rest; nil keeps every
// one.
func (v *View) TopicsWhere(ctx context.Context, scope string, keep func(Topic) bool) ([]Assessment, error) {
	topics, err := v.store.Topics(ctx, scope)
	if err != nil {
		return nil, err
	}
	if keep != nil {
		topics = slices.DeleteFunc(topics, func(t Topic) bool { return !keep(t) })
	}
	assessed, err := v.store.Assess(ctx, v.authority, v.reader, topics)
	if err != nil {
		return nil, err
	}
	readable := assessed[:0]
	for _, a := range assessed {
		if a.Access.Topic(v.reader, a.Topic) {
			readable = append(readable, a)
		}
	}
	return readable, nil
}

// Visible is whether the person may see a stance in a topic's history: what
// `stance_history` would show them.
func (v *View) Visible(a *Assessment, st Stance) bool {
	return a.Access.Stance(v.reader, st) || a.Access.Withdrawal(v.reader, st)
}
