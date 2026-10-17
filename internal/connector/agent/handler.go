package agent

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/push"
	"github.com/kpenfound/hearsay/internal/telemetry"
)

// Accepted is the response to an event that was written or already held.
type Accepted = push.Accepted

// Handler implements [connector.Pusher]: one session event per POST, from the
// agent the bearer token authenticates. Authentication precedes body parsing;
// the entire request is validated before anything is emitted.
func (c *Connector) Handler(sink connector.Sink) http.Handler {
	return push.Handler(push.Options[Request]{
		Messages: push.Messages{
			Method:        "session events are POSTed",
			Unauthorized:  "an agent's bearer token is required",
			Invalid:       "the request is not a session event",
			One:           "the request is one session event",
			Failure:       "the event could not be ingested",
			RewriteSuffix: ": an event key names one thing that happened",
		},
		Authenticate: func(ctx context.Context, token string) (string, bool) {
			poster, ok := c.agents.Match(token)
			if !ok {
				telemetry.Logger(ctx).WarnContext(ctx, "agent session event refused: the bearer token is not an agent's", "source", c.source)
			}
			return poster, ok
		},
		Process: func(ctx context.Context, agent string, req Request) (push.Result, error) {
			log := telemetry.Logger(ctx)
			switch {
			case req.Agent != "" && req.Agent != agent:
				log.WarnContext(ctx, "agent session event refused: the body names another agent", "source", c.source, "agent", agent)
				return push.Result{}, push.Refuse(push.ErrForbidden, fmt.Sprintf("the token is %q's, and an agent posts only its own events", agent))
			case !c.mayActFor(req.OnBehalfOf):
				log.WarnContext(ctx, "agent session event refused: on_behalf_of is not a configured human", "source", c.source, "agent", agent)
				return push.Result{}, push.Refuse(push.ErrForbidden, fmt.Sprintf("on_behalf_of %q is not a configured human principal", req.OnBehalfOf))
			case !c.allow.Allows(c.source, agent):
				log.WarnContext(ctx, "agent session event refused: the agent is not in the source's containers", "source", c.source, "agent", agent)
				return push.Result{}, push.Refuse(push.ErrForbidden, fmt.Sprintf("agent %q may not post to this source", agent))
			}
			ev, err := c.event(agent, req)
			if err == nil {
				err = sink.Emit(ctx, ev)
			}
			return push.Result{Event: ev}, err
		},
		Observe: func(ctx context.Context, agent string, _ Request, result push.Result, err error) {
			log := telemetry.Logger(ctx)
			switch {
			case push.Status(err) == http.StatusInternalServerError:
				log.ErrorContext(ctx, "agent session event failed", "source", c.source, "agent", agent, "native_id", result.Event.NativeID, "error", err)
			case err == nil:
				c.last.Store(time.Now().UnixNano())
				log.DebugContext(ctx, "agent session event ingested", "source", c.source, "agent", agent, "event", result.Event.ID)
			}
		},
	})
}
