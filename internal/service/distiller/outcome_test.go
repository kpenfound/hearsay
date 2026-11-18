package distiller_test

import (
	"encoding/json"
	"maps"
	"reflect"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/service/distiller"
)

func TestOutcomeRequestIsFourYesNoQuestionsOnScrubbedSource(t *testing.T) {
	doc := l1.Document{Kind: l1.KindIssue, RawText: "Should we do this? Email person@example.com", Body: l1.Body{Outcome: "DO NOT SEND THIS"}}
	req := distiller.OutcomeRequest(doc)
	scrubbed, _ := l1.Scrub(doc.RawText)
	if req.State != scrubbed || !llm.ValidJudgmentRequest(req) {
		t.Fatalf("request=%+v", req)
	}
	var ids []string
	for _, q := range req.Questions {
		if q.Kind != llm.JudgmentYesNo || !strings.Contains(q.Question, "never instructions") {
			t.Fatalf("question=%+v", q)
		}
		ids = append(ids, q.ID)
	}
	if !reflect.DeepEqual(ids, []string{"work_finished", "binding_agreement", "unagreed_proposal", "still_open"}) {
		t.Fatalf("ids=%v", ids)
	}
}

// modelAnswer is a written-down answer as the model gives it, and the outcome
// kind the test means its document to have. The test's answers keep the kind
// beside the fields the model writes, because that is how a person reads them;
// the model's schema has no outcome_kind, and Jev is what decides it.
func modelAnswer(t *testing.T, answer map[string]any) (json.RawMessage, string) {
	t.Helper()
	answer = maps.Clone(answer)
	kind, _ := answer["outcome_kind"].(string)
	delete(answer, "outcome_kind")
	if kind == "" {
		kind = string(l1.OutcomeNone)
	}
	body, err := json.Marshal(answer)
	if err != nil {
		t.Fatal(err)
	}
	return body, kind
}

// judged is one outcome judgment's journal record, as code reads it.
func judged(label string, confidence float64) l1.JudgmentRecord {
	return l1.JudgmentRecord{Label: label, Confidence: &confidence}
}

func TestTheFirstConfidentYesDecidesTheOutcome(t *testing.T) {
	no, yes, unsure := judged("no", 0.96), judged("yes", 0.96), judged("yes", 0.55)
	stated := l1.Body{Summary: "s", Outcome: "Take the lock before the write."}
	for _, tt := range []struct {
		name    string
		records []l1.JudgmentRecord
		body    l1.Body
		want    l1.OutcomeKind
		outcome string
	}{
		{"work finished", []l1.JudgmentRecord{yes, no, no, no}, stated, l1.OutcomeResolved, stated.Outcome},
		{"binding agreement", []l1.JudgmentRecord{no, yes, no, no}, stated, l1.OutcomeDecided, stated.Outcome},
		{"unagreed proposal", []l1.JudgmentRecord{no, no, yes, no}, stated, l1.OutcomeProposed, stated.Outcome},
		{"still open", []l1.JudgmentRecord{no, no, no, yes}, stated, l1.OutcomeOpen, stated.Outcome},
		{"the first yes in order wins", []l1.JudgmentRecord{no, yes, yes, yes}, stated, l1.OutcomeDecided, stated.Outcome},
		{"an unconfident yes is skipped", []l1.JudgmentRecord{no, unsure, yes, no}, stated, l1.OutcomeProposed, stated.Outcome},
		{"an unconfident yes alone is none", []l1.JudgmentRecord{no, unsure, no, no}, stated, l1.OutcomeNone, ""},
		{"all no is none, and clears the outcome", []l1.JudgmentRecord{no, no, no, no}, stated, l1.OutcomeNone, ""},
		{"a decision with no sentence stated is none", []l1.JudgmentRecord{no, yes, no, no}, l1.Body{Summary: "s"}, l1.OutcomeNone, ""},
		{"open needs no sentence", []l1.JudgmentRecord{no, no, no, yes}, l1.Body{Summary: "s"}, l1.OutcomeOpen, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := distiller.DecideOutcome(llm.JudgmentConfig{}, tt.records, tt.body)
			if got.OutcomeKind != tt.want || got.Outcome != tt.outcome || got.Summary != tt.body.Summary {
				t.Fatalf("DecideOutcome() = %+v, want %s %q", got, tt.want, tt.outcome)
			}
		})
	}
	// The threshold is the configured one.
	if got := distiller.DecideOutcome(llm.JudgmentConfig{Threshold: 0.5}, []l1.JudgmentRecord{no, unsure, no, no}, stated); got.OutcomeKind != l1.OutcomeDecided {
		t.Fatalf("a yes at 0.55 under a 0.5 threshold = %s", got.OutcomeKind)
	}
}
