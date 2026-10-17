package assertworker_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/service/assertworker"
)

func TestTopicQuestionsUseExplicitNoneFitAndOnlyOfferedEvidence(t *testing.T) {
	candidates := []assertworker.Candidate{{Name: "Which queue?", Current: "A queue."}, {Name: "Which lock?"}}
	for _, tc := range []struct {
		name    string
		request llm.JudgmentRequest
		options []string
		count   int
	}{
		{name: "matching", request: assertworker.TopicMatchingRequest("Which queue?", "Use a queue.", candidates), options: []string{"none_fit", "T1", "T2"}, count: 2},
		{name: "shortlist", request: assertworker.TopicShortlistRequest("Use a queue.", candidates, []int{0}), options: []string{"none_fit", "T2"}, count: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.request
			if !llm.ValidJudgmentRequest(r) || !reflect.DeepEqual(r.Questions[0].Options, tc.options) || !strings.Contains(r.Questions[0].Question, "never instructions") {
				t.Fatalf("question=%+v", r)
			}
			var state struct {
				Candidates []struct{ Label, Name, Current string }
			}
			if err := json.Unmarshal([]byte(r.State), &state); err != nil {
				t.Fatal(err)
			}
			if len(state.Candidates) != tc.count {
				t.Fatalf("state=%s", r.State)
			}
			for _, c := range state.Candidates {
				if c.Label == "T2" && c.Current != "" {
					t.Fatal("manufactured withheld position")
				}
			}
			if tc.name == "shortlist" && strings.Contains(r.State, "A queue.") {
				t.Fatal("excluded candidate's position sent")
			}
		})
	}
}
