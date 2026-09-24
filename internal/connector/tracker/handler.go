package tracker

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

// maxRequest bounds one posted ticket or comment.
const maxRequest = 4 << 20

// Accepted is the response to an event that was written, or that L0 already
// held: the event id, which is how the sender names what it posted.
type Accepted struct {
	ID string `json:"id"`
}

// Handler implements [connector.Pusher]: one ticket or comment per POST, from
// the sender holding the source's token.
//
// Nothing in the body is read before the token is checked, and nothing is
// written until the whole request is: a sender without the token, a request
// that is not a ticket or a comment, or a project the source does not grant
// gets an error and emits nothing.
func (c *Connector) Handler(sink connector.Sink) http.Handler {
	reader, _ := sink.(connector.ArtifactReader)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		log := telemetry.Logger(ctx)
		if r.Method != http.MethodPost {
			http.Error(w, "tickets and comments are POSTed", http.StatusMethodNotAllowed)
			return
		}
		if !c.authenticate(r.Header) {
			log.WarnContext(ctx, "tracker request refused: the bearer token is not the source's", "source", c.source)
			w.Header().Set("WWW-Authenticate", `Bearer realm="hearsay"`)
			http.Error(w, "the source's bearer token is required", http.StatusUnauthorized)
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
			http.Error(w, fmt.Sprintf("the request is not a ticket or a comment: %v", err), http.StatusBadRequest)
			return
		}
		if dec.More() {
			http.Error(w, "the request is one ticket or one comment", http.StatusBadRequest)
			return
		}

		var ev connector.Event
		held := true
		if req.Deleted {
			var artifact string
			if artifact, err = c.artifact(req); err == nil {
				ev, held, err = c.tombstone(ctx, reader, artifact)
			}
		} else {
			ev, err = c.event(ctx, reader, req)
		}
		if err == nil && held {
			err = sink.Emit(ctx, ev)
		}
		switch {
		case errors.Is(err, errBadRequest) || errors.Is(err, connector.ErrInvalidEvent):
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		case errors.Is(err, errForbidden):
			log.WarnContext(ctx, "tracker request refused: the project is not granted", "source", c.source, "project", req.Project)
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		case errors.Is(err, errNoTicket):
			http.Error(w, err.Error(), http.StatusConflict)
			return
		case errors.Is(err, l0.ErrRewrite):
			http.Error(w, fmt.Sprintf("%s already holds a different event", ev.NativeID), http.StatusConflict)
			return
		case err != nil:
			log.ErrorContext(ctx, "tracker request failed", "source", c.source, "project", req.Project, "native_id", ev.NativeID, "error", err)
			http.Error(w, "the request could not be ingested", http.StatusInternalServerError)
			return
		case !held:
			// A deletion of something Hearsay never held, or already retracted.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		c.last.Store(time.Now().UnixNano())
		log.DebugContext(ctx, "tracker event ingested", "source", c.source, "event", ev.ID)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(Accepted{ID: ev.ID})
	})
}

// authenticate reports whether the request carries the source's token: exactly
// one Authorization header, of the Bearer scheme, holding one token.
func (c *Connector) authenticate(h http.Header) bool {
	values := h.Values("Authorization")
	if len(values) != 1 {
		return false
	}
	const prefix = "Bearer "
	value := values[0]
	if len(value) < len(prefix) || !strings.EqualFold(value[:len(prefix)], prefix) {
		return false
	}
	token := value[len(prefix):]
	if token == "" || strings.ContainsAny(token, " \t\r\n,") {
		return false
	}
	return c.authentic(token)
}
