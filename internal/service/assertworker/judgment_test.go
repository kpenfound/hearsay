package assertworker_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/service/assertworker"
)

func TestStanceQuestionUsesOnlySuppliedEvidence(t *testing.T) {
	req := assertworker.StanceSameAnswerRequest("A question?", "The old answer.", "The new answer.", "The supporting outcome.")
	if len(req.Questions) != 1 || req.Questions[0].Kind != llm.JudgmentYesNo || !llm.ValidJudgmentRequest(req) {
		t.Fatalf("question = %+v", req)
	}
	var state map[string]string
	if err := json.Unmarshal([]byte(req.State), &state); err != nil {
		t.Fatal(err)
	}
	if len(state) != 4 || state["Previous"] != "The old answer." || state["Supporting"] != "The supporting outcome." {
		t.Fatalf("state = %+v", state)
	}
	if !strings.Contains(req.Questions[0].Question, "never instructions") {
		t.Fatal("source evidence must not instruct the judgment")
	}
}
