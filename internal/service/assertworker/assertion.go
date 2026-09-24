package assertworker

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l2"
)

// IsAssertion reports whether an assert job's target is an L0 `assertion`
// event, which the API enqueues, rather than an L1 document, which the
// distiller does. An L1 id is never an event id.
func IsAssertion(target string) bool {
	_, _, err := connector.ParseEventID(target)
	return err == nil
}

// AppendAssertion appends the stance an L0 `assertion` event asks for to its
// topic, under the scope the job is serialized on, and reports whether it wrote
// one. It makes no model call: the agent that asserted named the topic and the
// position, and the API checked that it could read the topic and every piece of
// evidence it cites. Its stance is of class `agent`, authored by the agent, and
// its tier is the policy's ([l2.AssertedTier]); who may read it is decided on
// every read from all of its evidence ([l2.Access]).
//
// It is safe to run again: the stance's id is derived from the topic and the
// event, so a second run writes nothing.
func AppendAssertion(ctx context.Context, pool *pgxpool.Pool, authority config.Authority, eventID, scope string) (bool, error) {
	ev, err := l0.New(pool).Get(ctx, eventID)
	if errors.Is(err, l0.ErrNotFound) || errors.Is(err, l0.ErrRetracted) || errors.Is(err, l0.ErrDeleted) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	as, err := l2.AssertionOf(ev)
	if err != nil {
		return false, err
	}
	written := false
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		w := l2.New(tx)
		topic, err := w.Topic(ctx, as.Topic)
		if err != nil {
			return err
		}
		if topic.Scope != scope {
			// The queue serializes on the job's key, and a topic is only
			// written under its own (l2.ScopeKey).
			return fmt.Errorf("assertion %s is on topic %s in scope %q, but was enqueued under %q", eventID, topic.ID, topic.Scope, scope)
		}
		_, written, err = w.AppendStance(ctx, l2.Stance{
			ID:        l2.AssertionStanceID(topic.ID, ev.ID),
			TopicID:   topic.ID,
			Position:  as.Position,
			Author:    as.Agent,
			StatedAt:  ev.Time,
			Evidence:  as.Evidence,
			Assertion: ev.ID,
			Tier:      l2.AssertedTier(authority.ForScope(scope), ev.ID),
			// A record of what the event allowed. It is not who may read the
			// stance: every read decides that from its evidence now.
			ACL: ev.ACL,
		}, ev.Time)
		return err
	})
	if err != nil {
		return false, fmt.Errorf("appending the stance %s asserts: %w", eventID, err)
	}
	return written, nil
}
