package push

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l0"
)

// MaxRequest bounds one posted artifact or event to 4 MiB.
const MaxRequest = 4 << 20

// Request refusals may be wrapped with a source-specific reason.
var (
	ErrBadRequest = errors.New("bad request")
	ErrForbidden  = errors.New("forbidden")
	ErrConflict   = errors.New("conflict")
)

// Accepted names an event that was written or already held.
type Accepted struct {
	ID string `json:"id"`
}

// Result describes completed work. NoContent is used for a deletion of an
// artifact not held. Event also supplies the native id if work returns a rewrite.
type Result struct {
	Event     connector.Event
	NoContent bool
}

// Messages lets a connector keep its vocabulary while sharing status mapping.
// Empty fields use generic defaults. RewriteSuffix adds context to a 409.
type Messages struct {
	Method, Unauthorized, Invalid, One, Failure, RewriteSuffix string
}

// Options supplies source policy and ingestion. Process must finish all work
// (including emission and any propagation) before returning success. Observe,
// if set, records that outcome; it must not do additional ingestion.
type Options[T any] struct {
	Authenticate func(context.Context, string) (string, bool)
	Process      func(context.Context, string, T) (Result, error)
	Observe      func(context.Context, string, T, Result, error)
	Messages     Messages
}

// Handler accepts one JSON object per POST, authenticating before reading the
// body. It maps invalid requests to 400, unknown credentials to 401, forbidden
// actions to 403, conflicts and L0 rewrites to 409, oversize bodies to 413 and
// all other work errors to 500. Success is 202 (or 204 for NoContent).
func Handler[T any](opts Options[T]) http.Handler {
	msg := opts.Messages
	defaults := []struct {
		field *string
		value string
	}{
		{&msg.Method, "events are POSTed"}, {&msg.Unauthorized, "a bearer token is required"},
		{&msg.Invalid, "the request is not an event"}, {&msg.One, "the request is one event"},
		{&msg.Failure, "the request could not be ingested"},
	}
	for _, d := range defaults {
		if *d.field == "" {
			*d.field = d.value
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, msg.Method, http.StatusMethodNotAllowed)
			return
		}
		token, valid := Bearer(r.Header)
		poster, ok := opts.Authenticate(r.Context(), token)
		if !valid || !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="hearsay"`)
			http.Error(w, msg.Unauthorized, http.StatusUnauthorized)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxRequest))
		if err != nil {
			http.Error(w, "reading the request", http.StatusRequestEntityTooLarge)
			return
		}
		var req T
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("%s: %v", msg.Invalid, err), http.StatusBadRequest)
			return
		}
		// A second decode checks both trailing values and malformed trailing JSON.
		var extra any
		if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
			http.Error(w, msg.One, http.StatusBadRequest)
			return
		}
		if trimmed := bytes.TrimSpace(body); len(trimmed) == 0 || trimmed[0] != '{' {
			http.Error(w, msg.One, http.StatusBadRequest)
			return
		}
		result, err := opts.Process(r.Context(), poster, req)
		if opts.Observe != nil {
			opts.Observe(r.Context(), poster, req, result, err)
		}
		status := Status(err)
		if status != http.StatusAccepted {
			message := msg.Failure
			switch status {
			case http.StatusBadRequest, http.StatusForbidden, http.StatusConflict:
				message = err.Error()
				if errors.Is(err, l0.ErrRewrite) {
					message = fmt.Sprintf("%s already holds a different event%s", result.Event.NativeID, msg.RewriteSuffix)
				}
			}
			http.Error(w, message, status)
			return
		}
		if result.NoContent {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(Accepted{ID: result.Event.ID})
	})
}

// Status maps a work error to its HTTP response status; nil means accepted.
func Status(err error) int {
	switch {
	case err == nil:
		return http.StatusAccepted
	case errors.Is(err, ErrBadRequest), errors.Is(err, connector.ErrInvalidEvent):
		return http.StatusBadRequest
	case errors.Is(err, ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, ErrConflict), errors.Is(err, l0.ErrRewrite):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

// Refuse carries a reason verbatim while wrapping a refusal sentinel.
func Refuse(kind error, reason string) error { return refusal{kind: kind, reason: reason} }

type refusal struct {
	kind   error
	reason string
}

func (e refusal) Error() string { return e.reason }
func (e refusal) Unwrap() error { return e.kind }
