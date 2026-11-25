package conversation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/push"
	"github.com/kpenfound/hearsay/internal/telemetry"
)

// Accepted is the response to a message that was stored or already held.
type Accepted struct {
	EventID string  `json:"event_id"`
	Status  Outcome `json:"status"`
}

// Refusal is the body of every response that is not 202.
type Refusal struct {
	Error  string `json:"error,omitempty"`
	Code   string `json:"code"`
	Status string `json:"status,omitempty"`
}

// The refusal codes.
const (
	CodeInvalid      = "invalid_request"
	CodeUnauthorized = "unauthorized"
	CodeForbidden    = "forbidden"
	CodeTooLarge     = "too_large"
	CodeMethod       = "method_not_allowed"
	CodeDeleted      = "deleted"
	CodeInternal     = "internal"
)

// Handler implements [connector.Pusher]: one message per POST, from an agent
// the source allows. Authentication and the agent's permission precede the
// body, and the whole request is validated before anything is stored.
func (c *Connector) Handler(sink connector.Sink) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		log := telemetry.Logger(ctx)
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			respond(w, http.StatusMethodNotAllowed, Refusal{Error: "messages are POSTed", Code: CodeMethod})
			return
		}
		token, valid := push.Bearer(r.Header)
		agent, ok := c.tokens.Match(token)
		if !valid || !ok {
			log.WarnContext(ctx, "conversation message refused: the bearer token is not an agent's", "source", c.source)
			w.Header().Set("WWW-Authenticate", `Bearer realm="hearsay"`)
			respond(w, http.StatusUnauthorized, Refusal{Error: "an agent's bearer token is required", Code: CodeUnauthorized})
			return
		}
		if !c.agents[agent] {
			log.WarnContext(ctx, "conversation message refused: the agent is not allowed", "source", c.source, "agent", agent)
			respond(w, http.StatusForbidden, Refusal{Error: fmt.Sprintf("agent %q may not post to this source", agent), Code: CodeForbidden})
			return
		}

		var req Request
		if status, refusal := readRequest(w, r, &req); status != 0 {
			respond(w, status, refusal)
			return
		}
		if err := c.check(req); err != nil {
			log.WarnContext(ctx, "conversation message refused", "source", c.source, "agent", agent)
			status, code := statusOf(err)
			respond(w, status, Refusal{Error: err.Error(), Code: code})
			return
		}

		ev, outcome, err := c.store(ctx, sink, agent, req)
		switch {
		case err == nil:
			c.last.Store(time.Now().UnixNano())
			log.DebugContext(ctx, "conversation message ingested", "source", c.source, "agent", agent, "event", ev.ID, "outcome", string(outcome))
			respond(w, http.StatusAccepted, Accepted{EventID: ev.ID, Status: outcome})
		case errors.Is(err, ErrDeleted):
			respond(w, http.StatusGone, Refusal{Error: err.Error(), Code: CodeDeleted, Status: CodeDeleted})
		default:
			status, code := statusOf(err)
			if status == http.StatusInternalServerError {
				log.ErrorContext(ctx, "conversation message failed", "source", c.source, "agent", agent, "native_id", ev.NativeID, "error", err)
				respond(w, status, Refusal{Error: "the message could not be ingested", Code: code})
				return
			}
			respond(w, status, Refusal{Error: err.Error(), Code: code})
		}
	})
}

func readRequest(w http.ResponseWriter, r *http.Request, req *Request) (int, Refusal) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, push.MaxRequest))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return http.StatusRequestEntityTooLarge, Refusal{Error: "the request is too large", Code: CodeTooLarge}
		}
		return http.StatusBadRequest, Refusal{Error: "reading the request", Code: CodeInvalid}
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(req); err != nil {
		return http.StatusBadRequest, Refusal{Error: fmt.Sprintf("the request is not a message: %v", err), Code: CodeInvalid}
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return http.StatusBadRequest, Refusal{Error: "the request is one message", Code: CodeInvalid}
	}
	if trimmed := bytes.TrimSpace(body); len(trimmed) == 0 || trimmed[0] != '{' {
		return http.StatusBadRequest, Refusal{Error: "the request is one message", Code: CodeInvalid}
	}
	return 0, Refusal{}
}

func statusOf(err error) (int, string) {
	switch push.Status(err) {
	case http.StatusBadRequest:
		return http.StatusBadRequest, CodeInvalid
	case http.StatusForbidden:
		return http.StatusForbidden, CodeForbidden
	default:
		return http.StatusInternalServerError, CodeInternal
	}
}

func respond(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
