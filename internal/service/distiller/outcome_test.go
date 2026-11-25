package distiller_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/llm/jev"
	"github.com/kpenfound/hearsay/internal/service/distiller"
)

func TestOutcomeRequestIsFourYesNoQuestionsOnScrubbedSource(t *testing.T) {
	doc := l1.Document{Kind: l1.KindIssue, RawText: "Should we do this? Email person@example.com", Body: l1.Body{Outcome: "DO NOT SEND THIS"}}
	req := distiller.OutcomeRequest(doc, llm.JudgmentConfig{})
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

func TestALongDocumentIsJudgedFromWhatFitsJevsBudget(t *testing.T) {
	line := "- \"quoted\" <tags> & a decision: we merged the retry change\n"
	doc := l1.Document{Kind: l1.KindIssue, RawText: strings.Repeat(line, 100_000/len(line))}
	var states []string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct{ State string }
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Error(err)
		}
		states = append(states, b.State)
		fmt.Fprint(w, `{"model":"jev-1.13.0","answers":{"work_finished":{"type":"noul","noul":0.9},"binding_agreement":{"type":"noul","noul":0.1},"unagreed_proposal":{"type":"noul","noul":0.1},"still_open":{"type":"noul","noul":0.1}}}`)
	}))
	defer s.Close()
	cfg := llm.JudgmentConfig{Model: "jev-latest", BaseURL: s.URL}
	j, err := llm.NewJudgments(cfg, func(c llm.JudgmentConfig, key string) llm.JudgmentProvider { return jev.New(c, key) }, func(string) string { return "key" })
	if err != nil {
		t.Fatal(err)
	}
	req := distiller.OutcomeRequest(doc, j.Config())
	out := j.Judge(t.Context(), req)
	if out.Failure != "" || len(states) != 1 {
		t.Fatalf("failure=%q calls=%d", out.Failure, len(states))
	}
	if !strings.HasPrefix(doc.RawText, strings.SplitN(states[0], "\n\n[", 2)[0]) || !strings.Contains(states[0], "left out") {
		t.Fatalf("Jev was not sent the start of the document with the cut marked")
	}
}

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
