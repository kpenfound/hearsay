package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/telemetry"
)

// maxRequest bounds one posted event. A tool call's input and output are the
// large part, and a session's history is many events rather than one big one.
const maxRequest = 4 << 20

// Accepted is the response to an event that was written, or that L0 already
// held: the event id, which is how the agent names what it posted.
type Accepted struct {
	ID string `json:"id"`
}

// Handler implements [connector.Pusher]: one session event per POST, from the
// agent the bearer token authenticates.
//
// Nothing in the body is read before the token is checked, and nothing is
// written until the whole request is: an agent that is not who it says, acts
// for someone who is not a configured human, or posts an event that does not
// hold together gets an error and emits nothing.
func (c *Connector) Handler(sink connector.Sink) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		log := telemetry.Logger(ctx)
		if r.Method != http.MethodPost {
			http.Error(w, "session events are POSTed", http.StatusMethodNotAllowed)
			return
		}
		agent, ok := c.authenticate(r.Header)
		if !ok {
			log.WarnContext(ctx, "agent session event refused: the bearer token is not an agent's", "source", c.source)
			w.Header().Set("WWW-Authenticate", `Bearer realm="hearsay"`)
			http.Error(w, "an agent's bearer token is required", http.StatusUnauthorized)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequest))
		if err != nil {
			http.Error(w, "reading the request", http.StatusRequestEntityTooLarge)
			return
		}
		var req Request
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("the request is not a session event: %v", err), http.StatusBadRequest)
			return
		}
		if dec.More() {
			http.Error(w, "the request is one session event", http.StatusBadRequest)
			return
		}
		switch {
		case req.Agent != "" && req.Agent != agent:
			log.WarnContext(ctx, "agent session event refused: the body names another agent", "source", c.source, "agent", agent)
			http.Error(w, fmt.Sprintf("the token is %q's, and an agent posts only its own events", agent), http.StatusForbidden)
			return
		case !c.mayActFor(req.OnBehalfOf):
			log.WarnContext(ctx, "agent session event refused: on_behalf_of is not a configured human", "source", c.source, "agent", agent)
			http.Error(w, fmt.Sprintf("on_behalf_of %q is not a configured human principal", req.OnBehalfOf), http.StatusForbidden)
			return
		case !c.allow.Allows(c.source, agent):
			log.WarnContext(ctx, "agent session event refused: the agent is not in the source's containers", "source", c.source, "agent", agent)
			http.Error(w, fmt.Sprintf("agent %q may not post to this source", agent), http.StatusForbidden)
			return
		}
		ev, err := c.event(agent, req)
		if err == nil {
			err = sink.Emit(ctx, ev)
		}
		switch {
		case errors.Is(err, errBadRequest) || errors.Is(err, connector.ErrInvalidEvent):
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		case errors.Is(err, l0.ErrRewrite):
			http.Error(w, fmt.Sprintf("%s already holds a different event: an event key names one thing that happened", ev.NativeID), http.StatusConflict)
			return
		case err != nil:
			log.ErrorContext(ctx, "agent session event failed", "source", c.source, "agent", agent, "native_id", ev.NativeID, "error", err)
			http.Error(w, "the event could not be ingested", http.StatusInternalServerError)
			return
		}
		c.last.Store(time.Now().UnixNano())
		log.DebugContext(ctx, "agent session event ingested", "source", c.source, "agent", agent, "event", ev.ID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(Accepted{ID: ev.ID})
	})
}

// authenticate is the agent whose token the request carries: exactly one
// Authorization header, of the Bearer scheme, holding one token.
func (c *Connector) authenticate(h http.Header) (string, bool) {
	values := h.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	const prefix = "Bearer "
	value := values[0]
	if len(value) < len(prefix) || !strings.EqualFold(value[:len(prefix)], prefix) {
		return "", false
	}
	token := value[len(prefix):]
	if token == "" || strings.ContainsAny(token, " \t\r\n,") {
		return "", false
	}
	return c.agentFor(token)
}
