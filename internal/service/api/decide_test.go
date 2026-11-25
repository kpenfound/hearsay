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

func TestDecideEventIsStableForTheSameRequest(t *testing.T) {
	base := decideEvent("kyle", "kern", "d1")
	if again := decideEvent("kyle", "kern", "d1"); again != base {
		t.Errorf("the same request derives %q then %q", base, again)
	}
}

func TestDecideEventIsAnIDUnderHearsaysOwnSource(t *testing.T) {
	event := decideEvent("kyle", "kern", "d1")
	if source, _, err := connector.ParseEventID(event); err != nil || source != AuditSource {
		t.Errorf("event %q is not an id under Hearsay's own source: %v", event, err)
	}
}

func TestDecideEventDiffersForEachInputAndNeverRunsFieldsTogether(t *testing.T) {
	base := decideEvent("kyle", "kern", "d1")
	seen := map[string]string{base: "kyle/kern/d1"}
	for _, tt := range [][3]string{
		{"sam", "kern", "d1"}, {"kyle", "", "d1"}, {"kyle", "shed", "d1"}, {"kyle", "kern", "d2"},
		{"kyle", "kern", "d1 "}, {"kyl", "ekern", "d1"}, {"kyle", "kern\x00d", "1"},
	} {
		got := decideEvent(tt[0], tt[1], tt[2])
		if other, dup := seen[got]; dup {
			t.Errorf("%v derives the same event as %s", tt, other)
		}
		seen[got] = fmt.Sprint(tt)
	}
}

func TestDecideRefusalMapsLedgerErrorsToFixedReasonCodes(t *testing.T) {
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
}

func TestDecideRefusalLeavesAnUnexpectedFailureUnmapped(t *testing.T) {
	if err := errors.New("connection reset"); !errors.Is(decideRefusal(err), err) {
		t.Error("an unexpected failure is not mapped here: it becomes internal in decide")
	}
}

func TestDecideRefusalAnswersAMissingTargetWithTheOneNotFound(t *testing.T) {
	if got := decideRefusal(fmt.Errorf("%w", l2.ErrNotFound)); !errors.Is(got, errDecideNotFound) {
		t.Errorf("a missing target is not the one not-found answer: %v", got)
	}
}

func TestErrorBodyOmitsAnUnsetCode(t *testing.T) {
	if got := string(ErrorBody(fail(http.StatusNotFound, "no such call %q", "x"))); got != `{"error":"no such call \"x\""}` {
		t.Errorf("ErrorBody = %s", got)
	}
}

func TestErrorBodyCarriesASetCode(t *testing.T) {
	if got := string(ErrorBody(errDecideNotFound)); got != `{"error":"no such stance or gesture that you may read","code":"not_found"}` {
		t.Errorf("ErrorBody with a code = %s", got)
	}
}
