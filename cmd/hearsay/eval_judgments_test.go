package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/eval"
	"github.com/kpenfound/hearsay/internal/llm"
)

func writeEvalJSON(t *testing.T, name string, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func cliCorpus() (eval.Corpus, eval.Predictions) {
	c := eval.Corpus{Version: 1, Provenance: "synthetic CLI fixture"}
	p := eval.Predictions{}
	for _, split := range []string{"tuning", "held_out"} {
		for _, label := range []string{"no", "yes"} {
			id := split + "-" + label
			confidence := .9
			c.Examples = append(c.Examples, eval.Example{ID: id, Group: id, Task: llm.TaskStanceSameAnswer, Split: split, Label: label, Sources: []eval.CorpusSource{{ID: id, Revision: "r1", URI: "fixture://" + id, ACL: []string{"public"}, Text: "PRIVATE SOURCE TEXT MUST NOT APPEAR"}}, Review: eval.Review{Adjudicator: "test-reviewer", Revision: "r1", Rationale: "fixture"}})
			p.Jev = append(p.Jev, eval.Prediction{ExampleID: id, ModelVersion: "test-v1", Label: label, Confidence: &confidence})
			p.Baseline = append(p.Baseline, eval.Prediction{ExampleID: id, Label: label})
		}
	}
	return c, p
}

func TestEvalJudgmentsOfflineCLI(t *testing.T) {
	t.Setenv("HEARSAY_DATABASE_URL", "postgres://unreachable.invalid/hearsay")
	t.Setenv("HEARSAY_JEV_API_KEY", "")
	c, p := cliCorpus()
	cp, pp := writeEvalJSON(t, "corpus.json", c), writeEvalJSON(t, "predictions.json", p)
	var out, stderr bytes.Buffer
	if err := run(t.Context(), []string{"eval", "judgments", "--corpus", cp, "--predictions", pp, "--json"}, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	var report map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"since", "until", "scope", "time_to_ratification", "topic_operations", "drill_down", "conflict_flags", "next_actions", "pipelines", "judgments"} {
		if _, ok := report[field]; !ok {
			t.Errorf("missing stable/additive field %s", field)
		}
	}
	if strings.Contains(out.String(), "PRIVATE SOURCE") {
		t.Fatal("source leaked")
	}
}

func TestEvalJudgmentCLIRefusals(t *testing.T) {
	for _, args := range [][]string{{"judgments"}, {"judgments", "--corpus", "missing"}, {"judgments", "--corpus", "missing", "--predictions", "missing", "--split", "typo"}, {"judgments", "extra"}} {
		var out, stderr bytes.Buffer
		if err := run(t.Context(), append([]string{"eval"}, args...), &out, &stderr); err == nil || out.Len() != 0 {
			t.Fatalf("args=%v err=%v out=%s", args, err, out.String())
		}
	}
}
