package tracker

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/push"
	"github.com/kpenfound/hearsay/internal/telemetry"
)

// Accepted is the response to an event that was written or already held.
type Accepted = push.Accepted

// Handler implements [connector.Pusher]: one ticket or comment per POST, from
// the sender holding the source's token. Authentication precedes body parsing;
// the entire request is validated before anything is emitted.
func (c *Connector) Handler(sink connector.Sink) http.Handler {
	reader, _ := sink.(connector.ArtifactReader)
	return push.Handler(push.Options[Request]{
		Messages: push.Messages{
			Method:       "tickets and comments are POSTed",
			Unauthorized: "the source's bearer token is required",
			Invalid:      "the request is not a ticket or a comment",
			One:          "the request is one ticket or one comment",
		},
		Authenticate: func(ctx context.Context, token string) (string, bool) {
			poster, ok := c.tokens.Match(token)
			if !ok {
				telemetry.Logger(ctx).WarnContext(ctx, "tracker request refused: the bearer token is not the source's", "source", c.source)
			}
			return poster, ok
		},
		Process: func(ctx context.Context, _ string, req Request) (push.Result, error) {
			var ev connector.Event
			var err error
			held := true
			if req.Deleted {
				var artifact string
				if artifact, err = c.artifact(req); err == nil {
					ev, held, err = push.Tombstone(ctx, reader, c.source, artifact)
				}
			} else {
				ev, err = c.event(ctx, reader, req)
			}
			if err == nil && held {
				err = sink.Emit(ctx, ev)
			}
			return push.Result{Event: ev, NoContent: !held}, err
		},
		Observe: func(ctx context.Context, _ string, req Request, result push.Result, err error) {
			log := telemetry.Logger(ctx)
			switch {
			case errors.Is(err, errForbidden):
				log.WarnContext(ctx, "tracker request refused: the project is not granted", "source", c.source, "project", req.Project)
			case push.Status(err) == http.StatusInternalServerError:
				log.ErrorContext(ctx, "tracker request failed", "source", c.source, "project", req.Project, "native_id", result.Event.NativeID, "error", err)
			case err == nil && !result.NoContent:
				c.last.Store(time.Now().UnixNano())
				log.DebugContext(ctx, "tracker event ingested", "source", c.source, "event", result.Event.ID)
			}
		},
	})
}
