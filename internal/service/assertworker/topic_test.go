package assertworker_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/service/assertworker"
)

var topicCandidates = []assertworker.Candidate{{Name: "Which queue?", Current: "A queue."}, {Name: "Which lock?"}}

func TestTopicQuestionsAreOneYesNoPerOfferedCandidate(t *testing.T) {
	r := assertworker.TopicSameQuestionRequest("Which queue?", "Use a queue.", topicCandidates)
	if !llm.ValidJudgmentRequest(r) || len(r.Questions) != len(topicCandidates) {
		t.Fatalf("questions=%+v", r.Questions)
	}
	for i, q := range r.Questions {
		label := []string{"T1", "T2"}[i]
		if q.ID != label || q.Kind != llm.JudgmentYesNo || !strings.Contains(q.Question, "never instructions") || !strings.Contains(q.Question, "topic "+label) {
			t.Fatalf("question %d = %+v", i, q)
		}
	}
}

func TestTopicQuestionStateHoldsTheEvidenceAndEveryCandidate(t *testing.T) {
	r := assertworker.TopicSameQuestionRequest("Which queue?", "Use a queue.", topicCandidates)
	var state struct {
		Evidence   struct{ Name, Position string }
		Candidates []struct{ Label, Name, Current string }
	}
	if err := json.Unmarshal([]byte(r.State), &state); err != nil {
		t.Fatal(err)
	}
	if state.Evidence.Name != "Which queue?" || state.Evidence.Position != "Use a queue." || len(state.Candidates) != len(topicCandidates) {
		t.Fatalf("state=%s", r.State)
	}
}

func TestTopicQuestionStateInventsNoPositionForACandidateWithNone(t *testing.T) {
	r := assertworker.TopicSameQuestionRequest("Which queue?", "Use a queue.", topicCandidates)
	var state struct {
		Candidates []struct{ Label, Current string }
	}
	if err := json.Unmarshal([]byte(r.State), &state); err != nil {
		t.Fatal(err)
	}
	for _, c := range state.Candidates {
		if c.Label == "T2" && c.Current != "" {
			t.Fatal("manufactured withheld position")
		}
	}
}
