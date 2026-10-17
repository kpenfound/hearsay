package distiller_test

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/service/distiller"
)

// These synthetic disagreements deliberately challenge the generator's label;
// they are plumbing fixtures, not evidence qualifying any production policy.
func TestRecordOutcomeRestatements(t *testing.T) {
	var issue l1.Document
	for _, doc := range documentsIn(t, fixtureEvents(source)) {
		if doc.ID == issueID {
			issue = doc
		}
	}
	for _, supported := range []bool{true, false} {
		directory := "testdata/outcome-restatements"
		if !supported {
			directory = "testdata/outcome-restatement-declined"
		}
		file := llm.FixtureFile{}
		for _, tc := range []struct {
			label   l1.OutcomeKind
			outcome string
		}{
			{l1.OutcomeProposed, "Taking the lock before writing was proposed."},
			{l1.OutcomeNone, ""},
			{l1.OutcomeOpen, "Whether to take the lock first remains open."},
		} {
			response, _ := json.Marshal(map[string]any{"supported": supported, "outcome_kind": tc.label, "outcome": tc.outcome})
			file.Completions = append(file.Completions, llm.CompletionFixture{Tier: llm.TierDistill,
				Request:  distiller.OutcomeRestatementRequest(issue, tc.label, distillBudget()),
				Response: llm.Response{JSON: response, StopReason: llm.StopEnd, Model: "recorded"}})
		}
		encoded, err := json.MarshalIndent(file, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		encoded = append(encoded, '\n')
		path := directory + "/recorded.json"
		if *record {
			if err := os.MkdirAll(directory, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, encoded, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		disk, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(disk) != string(encoded) {
			t.Fatal("outcome restatement recording drift; run with -record")
		}
		fx, err := llm.LoadFixtures(os.DirFS(directory))
		if err != nil {
			t.Fatal(err)
		}
		if fx.Len() != len(file.Completions) {
			t.Fatal("missing restatement fixtures")
		}
	}
}

func TestOutcomeRequestUsesOnlyScrubbedSource(t *testing.T) {
	doc := l1.Document{Kind: l1.KindIssue, RawText: "Should we do this? Email person@example.com", Body: l1.Body{Outcome: "DO NOT SEND THIS"}}
	req := distiller.OutcomeRequest(doc)
	scrubbed, _ := l1.Scrub(doc.RawText)
	if req.State != scrubbed || len(req.Questions) != 1 {
		t.Fatalf("request=%+v", req)
	}
	var options []string
	for _, kind := range l1.OutcomeKinds {
		options = append(options, string(kind))
	}
	if !reflect.DeepEqual(req.Questions[0].Options, options) {
		t.Fatalf("options=%v", req.Questions[0].Options)
	}
}

func outcomeStep(t *testing.T, label string) llm.JudgmentStep {
	t.Helper()
	raw, err := os.ReadFile("testdata/outcome-judgments/recorded.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures map[string]llm.JudgmentResponse
	if err = json.Unmarshal(raw, &fixtures); err != nil {
		t.Fatal(err)
	}
	response, ok := fixtures[label]
	if !ok {
		t.Fatalf("no judgment fixture for %s", label)
	}
	return llm.JudgmentStep{Response: response}
}

func TestRecordedOutcomeChoices(t *testing.T) {
	for _, kind := range l1.OutcomeKinds {
		t.Run(string(kind), func(t *testing.T) {
			step := outcomeStep(t, string(kind))
			fake := llm.NewFakeJudgmentProvider(step)
			j, err := llm.NewJudgments(llm.JudgmentConfig{Enabled: true, Model: "recorded-v1", MaxRetries: -1}, func(llm.JudgmentConfig, string) llm.JudgmentProvider { return fake }, func(string) string { return "fake-only" })
			if err != nil {
				t.Fatal(err)
			}
			result := j.Judge(t.Context(), distiller.OutcomeRequest(l1.Document{RawText: "Synthetic source evidence."}))
			if result.Fallback != "" || len(result.Answers) != 1 || result.Answers[0].Choice != string(kind) || fake.Calls() != 1 {
				t.Fatalf("result=%+v calls=%d", result, fake.Calls())
			}
			if result.Usage.CostUSD == nil || *result.Usage.CostUSD != 0 {
				t.Fatal("reported zero cost became unknown")
			}
		})
	}
}
