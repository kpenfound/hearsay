package distiller

import (
	"testing"

	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/llm"
)

const conversationText = "Owner: Which cache?\nKern: Use Redis, I decided.\nOwner (interrupted): Fine, use Redis, that is"

func TestAnOutcomeStandsOnlyOnTheOwnersLines(t *testing.T) {
	doc := l1.Document{Kind: l1.KindConversationSegment, RawText: conversationText}
	for _, tc := range []struct {
		name  string
		cited []int
		keep  bool
	}{
		{"an owner line", []int{1}, true},
		{"an interrupted owner line", []int{3}, true},
		{"several owner lines", []int{1, 3}, true},
		{"a Kern line", []int{2}, false},
		{"an owner line and a Kern line", []int{1, 2}, false},
		{"no line", nil, false},
		{"a line the segment does not have", []int{4}, false},
		{"line zero", []int{0}, false},
		{"a negative line", []int{-1}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := ownerOutcome(doc, answer{Summary: "s", Outcome: "The cache is Redis.", OutcomeLines: tc.cited})
			if kept := body.Outcome != ""; kept != tc.keep {
				t.Errorf("outcome %q with lines %v: kept = %v, want %v", body.Outcome, tc.cited, kept, tc.keep)
			}
		})
	}
}

func TestTheSummaryOfAConversationSegmentIsNeverCut(t *testing.T) {
	doc := l1.Document{Kind: l1.KindConversationSegment, RawText: conversationText}
	body := ownerOutcome(doc, answer{Summary: "A summary of both.", Outcome: "The cache is Redis.", OutcomeLines: []int{2}})
	if body.Summary != "A summary of both." {
		t.Errorf("the summary describes the whole topic and is never cut: %q", body.Summary)
	}
}

func TestAKernOnlySegmentKeepsNoOutcome(t *testing.T) {
	doc := l1.Document{Kind: l1.KindConversationSegment, RawText: "Kern: I decided on Redis."}
	if body := ownerOutcome(doc, answer{Summary: "s", Outcome: "Redis.", OutcomeLines: []int{1}}); body.Outcome != "" {
		t.Errorf("outcome = %q, want none", body.Outcome)
	}
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
			got := decideOutcome(llm.JudgmentConfig{}, tt.records, tt.body)
			if got.OutcomeKind != tt.want || got.Outcome != tt.outcome || got.Summary != tt.body.Summary {
				t.Fatalf("decideOutcome() = %+v, want %s %q", got, tt.want, tt.outcome)
			}
		})
	}
}

func TestTheConfiguredThresholdDecidesWhatIsConfident(t *testing.T) {
	no, unsure := judged("no", 0.96), judged("yes", 0.55)
	stated := l1.Body{Summary: "s", Outcome: "Take the lock before the write."}
	got := decideOutcome(llm.JudgmentConfig{Threshold: 0.5}, []l1.JudgmentRecord{no, unsure, no, no}, stated)
	if got.OutcomeKind != l1.OutcomeDecided {
		t.Fatalf("a yes at 0.55 under a 0.5 threshold = %s", got.OutcomeKind)
	}
}

func TestThePumpBatchIsHeldToWhatOneReadReturns(t *testing.T) {
	for _, tt := range []struct{ asked, want int }{
		{0, DefaultPumpBatch},
		{-1, DefaultPumpBatch},
		{10, 10},
		{l0.MaxLimit, l0.MaxLimit},
		{l0.MaxLimit + 1, l0.MaxLimit},
		{1_000_000, l0.MaxLimit},
	} {
		if got := (PumpOptions{Batch: tt.asked}).withDefaults().Batch; got != tt.want {
			t.Errorf("a pump asked for a batch of %d uses %d, want %d", tt.asked, got, tt.want)
		}
	}
}

func judged(label string, confidence float64) l1.JudgmentRecord {
	return l1.JudgmentRecord{Label: label, Confidence: &confidence}
}
