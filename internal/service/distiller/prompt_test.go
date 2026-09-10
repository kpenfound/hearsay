package distiller_test

import (
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/service/distiller"
)

// Every kind's request has to be one the abstraction would send and one the
// registry can enforce: a schema outside the subset internal/llm understands is
// refused, and a constraint that is not enforced is one a caller would believe
// was.
func TestRequestForIsValidForEveryKind(t *testing.T) {
	doc := l1.Document{RawText: "what was said"}
	for _, kind := range l1.Kinds() {
		t.Run(string(kind), func(t *testing.T) {
			doc.Kind = kind
			req := distiller.RequestFor(doc, 2048)
			if err := req.Validate(); err != nil {
				t.Fatalf("RequestFor(%s) is not a request this abstraction would send: %v", kind, err)
			}
			if req.Schema == nil {
				t.Fatal("RequestFor() asked for no schema: a distillation is structured output")
			}
			if err := req.Schema.Validate(); err != nil {
				t.Fatalf("the %s schema is not one internal/llm can enforce: %v", kind, err)
			}
			if !strings.Contains(req.System, "outcome_kind") {
				t.Error("the prompt does not tell the model about outcome_kind, which every document has")
			}
			// The five values are the design's, and the enum a model answers
			// from is the same list the column accepts.
			for _, outcome := range l1.OutcomeKinds {
				if !strings.Contains(string(req.Schema.Definition), `"`+string(outcome)+`"`) {
					t.Errorf("the %s schema does not offer the outcome kind %q", kind, outcome)
				}
			}
		})
	}
}

// A per-kind field is declared only on the kind that has it, and the schema is
// closed, so a model cannot answer with a field this build would drop.
func TestSchemaDeclaresOnlyTheFieldsAKindHas(t *testing.T) {
	tests := []struct {
		kind    l1.Kind
		has     []string
		hasNot  []string
		answers map[string]string
	}{
		{kind: l1.KindIssue, has: []string{"question"}, hasNot: []string{"change"}},
		{kind: l1.KindPR, has: []string{"change"}, hasNot: []string{"question"}},
		{kind: l1.KindCommit, has: []string{"change"}, hasNot: []string{"question"}},
	}
	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			schema := distiller.RequestFor(l1.Document{Kind: tt.kind, RawText: "x"}, 0).Schema
			for _, field := range append([]string{"summary", "outcome", "outcome_kind", "open_questions"}, tt.has...) {
				if !strings.Contains(string(schema.Definition), `"`+field+`"`) {
					t.Errorf("the %s schema does not ask for %q", tt.kind, field)
				}
			}
			for _, field := range tt.hasNot {
				if strings.Contains(string(schema.Definition), `"`+field+`"`) {
					t.Errorf("the %s schema asks for %q, which that kind does not have", tt.kind, field)
				}
			}
			// A field the schema does not declare is refused rather than
			// dropped: additionalProperties is false.
			answer := `{"summary":"a summary","outcome_kind":"none","` + tt.hasNot[0] + `":"x"}`
			if err := schema.ValidateJSON([]byte(answer)); err == nil {
				t.Errorf("the %s schema accepted a %q it does not declare", tt.kind, tt.hasNot[0])
			}
			// And the two required fields are required.
			for _, missing := range []string{`{"outcome_kind":"none"}`, `{"summary":"a summary"}`} {
				if err := schema.ValidateJSON([]byte(missing)); err == nil {
					t.Errorf("the %s schema accepted %s", tt.kind, missing)
				}
			}
			if err := schema.ValidateJSON([]byte(`{"summary":"a summary","outcome_kind":"merged"}`)); err == nil {
				t.Errorf("the %s schema accepted an outcome kind that is not one of the five", tt.kind)
			}
		})
	}
}

// A conversation longer than what this package sends is cut the same way every
// time: a request that differed between two runs would miss its recording and
// distil the same artifact differently on every attempt.
func TestRequestForTruncatesLongDocumentsDeterministically(t *testing.T) {
	// Multi-byte runes, so that a naive cut would land inside one.
	long := strings.Repeat("日本語のテキスト ", distiller.MaxPromptBytes/8)
	doc := l1.Document{Kind: l1.KindPR, RawText: long}

	first := distiller.RequestFor(doc, 0).Messages[0].Text
	second := distiller.RequestFor(doc, 0).Messages[0].Text
	if first != second {
		t.Fatal("two requests for one document differ")
	}
	if len(first) > distiller.MaxPromptBytes+200 {
		t.Errorf("the prompt is %d bytes, want it cut to about %d", len(first), distiller.MaxPromptBytes)
	}
	if !strings.Contains(first, "left out") {
		t.Error("the truncated prompt does not say that it is truncated")
	}
	if !utf8Valid(first) {
		t.Error("the prompt was cut inside a rune")
	}

	// A document that fits is sent as it is.
	short := l1.Document{Kind: l1.KindPR, RawText: "what was said"}
	if got := distiller.RequestFor(short, 0).Messages[0].Text; got != short.RawText {
		t.Errorf("a short document was rewritten: %q", got)
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

// The distill tier is the one this service uses, and the request it sends is
// one the shipped configuration can answer.
func TestTheDistillTierAnswersEveryFixtureRequest(t *testing.T) {
	completer, err := newFakeRegistry(t).Completer(llm.TierDistill)
	if err != nil {
		t.Fatalf("Completer(distill) = %v", err)
	}
	for _, doc := range documentsIn(t, fixtureEvents(source)) {
		if _, err := completer.Complete(t.Context(), distiller.RequestFor(doc, 0)); err != nil {
			t.Errorf("Complete(%s) = %v", doc.ID, err)
		}
	}
}
