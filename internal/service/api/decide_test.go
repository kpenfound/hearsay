package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l2"
)

// The event a decision is recorded under is a function of the human, the
// carrier and the decision id alone, and each of the three matters.
func TestDecideEventIsDerivedFromHumanCarrierAndDecision(t *testing.T) {
	base := decideEvent("kyle", "kern", "d1")
	if again := decideEvent("kyle", "kern", "d1"); again != base {
		t.Errorf("the same request derives %q then %q", base, again)
	}
	if source, _, err := connector.ParseEventID(base); err != nil || source != AuditSource {
		t.Errorf("event %q is not an id under Hearsay's own source: %v", base, err)
	}
	seen := map[string]string{base: "kyle/kern/d1"}
	for _, tt := range [][3]string{
		{"sam", "kern", "d1"}, {"kyle", "", "d1"}, {"kyle", "shed", "d1"}, {"kyle", "kern", "d2"},
		// Nothing runs one field into the next.
		{"kyle", "kern", "d1 "}, {"kyl", "ekern", "d1"}, {"kyle", "kern\x00d", "1"},
	} {
		got := decideEvent(tt[0], tt[1], tt[2])
		if other, dup := seen[got]; dup {
			t.Errorf("%v derives the same event as %s", tt, other)
		}
		seen[got] = fmt.Sprint(tt)
	}
}

// Every error the ledger can refuse with maps to one documented code and a
// fixed sentence that says nothing of what it was asked about.
func TestDecideRefusalMapsLedgerErrorsToReasonCodes(t *testing.T) {
	for _, tt := range []struct {
		name   string
		err    error
		code   string
		status int
	}{
		{"not allowed", fmt.Errorf("%w: sam may not ratify by hand in scope x", l2.ErrNotAllowed), ReasonNotAllowed, http.StatusForbidden},
		{"not found", fmt.Errorf("%w: stance stance:secret", l2.ErrNotFound), ReasonNotFound, http.StatusNotFound},
		{"not live", fmt.Errorf("%w: stance s", l2.ErrNotLive), ReasonNotLive, http.StatusConflict},
		{"not undoable, which is also invalid", errors.Join(l2.ErrNotUndoable, l2.ErrInvalid), ReasonNotUndoable, http.StatusConflict},
		{"a conflicting event, which is also invalid", errors.Join(l2.ErrEventConflict, l2.ErrInvalid), ReasonConflict, http.StatusConflict},
		{"invalid", fmt.Errorf("%w: documents in two scopes", l2.ErrInvalid), ReasonInvalid, http.StatusBadRequest},
	} {
		var got *Error
		ok := errors.As(decideRefusal(tt.err), &got)
		if !ok || got.Code != tt.code || got.Status != tt.status || got.Message == "" {
			t.Errorf("%s: decideRefusal = %+v, want %s with status %d and a message", tt.name, got, tt.code, tt.status)
		}
		if ok && (strings.Contains(got.Message, "secret") || strings.Contains(got.Message, "scope x")) {
			t.Errorf("%s: the message %q repeats what the ledger said", tt.name, got.Message)
		}
	}
	if err := errors.New("connection reset"); !errors.Is(decideRefusal(err), err) {
		t.Error("an unexpected failure is not mapped here: it becomes internal in decide")
	}
	if got := decideRefusal(fmt.Errorf("%w", l2.ErrNotFound)); !errors.Is(got, errDecideNotFound) {
		t.Errorf("a missing target is not the one not-found answer: %v", got)
	}
}

// The error body of a call that sets no code is what it always was.
func TestErrorBodyOmitsAnUnsetCode(t *testing.T) {
	if got := string(ErrorBody(fail(http.StatusNotFound, "no such call %q", "x"))); got != `{"error":"no such call \"x\""}` {
		t.Errorf("ErrorBody = %s", got)
	}
	if got := string(ErrorBody(errDecideNotFound)); got != `{"error":"no such stance or gesture that you may read","code":"not_found"}` {
		t.Errorf("ErrorBody with a code = %s", got)
	}
}
