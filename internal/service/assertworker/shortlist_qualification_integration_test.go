//go:build integration

package assertworker_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/eval"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/service/assertworker"
)

func readShortlistEvaluation(t *testing.T, name string, v any) {
	t.Helper()
	f, err := os.Open("../../eval/testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err = eval.DecodeEvaluation(f, v); err != nil {
		t.Fatal(err)
	}
}

// The real service consumes exactly the sequence evaluated by Qualify. The
// deliberately permissive test limits allow the known premature stop, proving
// it was measured as an error even when an operator chooses to tolerate it.
func TestQualifiedShortlistPolicyInRunningWorker(t *testing.T) {
	if eval.ShortlistCapacity != assertworker.MaxCandidates {
		t.Fatal("evaluation shortlist capacity drifted from worker")
	}
	for _, tc := range []struct {
		name, id                                              string
		lowPrefix, lowStop, emptyStop, mismatchedStop, shadow bool
	}{
		{name: "correct terminal stop", id: "held_out-08"},
		{name: "premature stop is scored", id: "held_out-shortlist-premature"},
		{name: "capacity selection", id: "held_out-07"},
		{name: "unqualified prefix keeps baseline", id: "held_out-08", lowPrefix: true},
		{name: "unqualified stop keeps baseline", id: "held_out-08", lowStop: true},
		{name: "empty prefix keeps baseline", id: "held_out-08", emptyStop: true},
		{name: "model mismatch keeps baseline", id: "held_out-08", mismatchedStop: true},
		{name: "shadow keeps baseline", id: "held_out-08", shadow: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var c eval.Corpus
			var ps eval.Predictions
			var responses []llm.JudgmentResponse
			readShortlistEvaluation(t, "seed-corpus.json", &c)
			readShortlistEvaluation(t, "seed-predictions.json", &ps)
			readShortlistEvaluation(t, "seed-judgments.json", &responses)
			at := slices.IndexFunc(c.Examples, func(e eval.Example) bool { return e.ID == tc.id })
			p := ps.Jev[at]
			if tc.lowPrefix || tc.lowStop {
				confidence := .55
				p.Confidence = &confidence
			}
			if tc.emptyStop {
				p.Shortlist = &eval.Shortlist{Selected: []string{}, End: "none_fit"}
			}
			if tc.mismatchedStop {
				p.Shortlist = nil
				p.Label = "abstain"
				p.Confidence = nil
			}
			ps.Jev[at] = p
			q, err := eval.Qualify(c, ps, eval.QualificationOptions{Task: llm.TaskTopicShortlist, Name: "connecting-test-only", ModelVersion: "fixture-judge-v1", Limits: map[string]float64{"errors": 1, "shortlist_omissions": 1}, MinimumPerClass: map[string]int{"existing": 1, "none_fit": 1}})
			if err != nil || q.Status != "qualified" {
				t.Fatalf("qualification=%+v %v", q, err)
			}
			if q.HeldOut.Errors.Count == 0 || q.HeldOut.ShortlistOmissions.Count == 0 {
				t.Fatalf("premature stop disappeared: %+v", q)
			}
			policy := q.Policies[llm.TaskTopicShortlist]
			adopt := !tc.lowPrefix && !tc.lowStop && !tc.emptyStop && !tc.mismatchedStop && !tc.shadow
			// Independently compare the effective prediction, including fallback,
			// to the exact metrics used to issue the worker's policy.
			effective := ps
			effective.Jev = slices.Clone(ps.Jev)
			if !adopt && !tc.shadow {
				effective.Jev[at] = ps.Baseline[at]
			}
			comp, err := eval.CompareJudgments(c, effective, "held_out")
			if err != nil || !reflect.DeepEqual(q.HeldOut, comp[llm.TaskTopicShortlist].Jev) {
				t.Fatalf("policy did not score worker output: %+v %v", q, err)
			}

			pool := scratchPool(t)
			src := newSource(t)
			f, topics, cs := topicSetup(t, pool, src, 6)
			baselineIndex := 0
			if tc.id == "held_out-shortlist-premature" {
				baselineIndex = 1
			}
			fx := extractionFixture(t, src, cs[:5], oneAssertion(fmt.Sprintf("T%d", baselineIndex+1), prPosition))
			selected := []assertworker.Candidate{}
			selectedTopics := []string{}
			if adopt {
				for _, id := range p.Shortlist.Selected {
					i := slices.Index(c.Examples[at].Candidates, id)
					selected = append(selected, cs[i])
					selectedTopics = append(selectedTopics, topics[i].ID)
				}
			}
			changesPrompt := adopt && !slices.Equal(selectedTopics, []string{topics[0].ID, topics[1].ID, topics[2].ID, topics[3].ID, topics[4].ID})
			if changesPrompt {
				if err = fx.Add(llm.CompletionFixture{Tier: llm.TierAssert, Request: assertworker.RequestFor(prDoc(t, src), selected, assertBudget()), Response: llm.Response{JSON: []byte(`{"assertions":[` + oneAssertion("T1", prPosition) + `]}`)}}); err != nil {
					t.Fatal(err)
				}
			}
			steps := []llm.JudgmentStep{{Response: judgmentFixture(t, "changes")}}
			var sequence []llm.JudgmentResponse
			for _, r := range responses {
				if r.Answers[0].ID != tc.id {
					continue
				}
				a := r.Answers[0]
				a.ID = "shortlist"
				if a.Choice != "none_fit" {
					a.Choice = "T" + strings.TrimPrefix(a.Choice, "topic-")
				}
				probs := map[string]float64{}
				for id, v := range a.Probabilities {
					if id != "none_fit" {
						id = "T" + strings.TrimPrefix(id, "topic-")
					}
					probs[id] = v
				}
				a.Probabilities = probs
				r.Answers = []llm.JudgmentAnswer{a}
				sequence = append(sequence, r)
			}
			if tc.emptyStop {
				r := sequence[len(sequence)-1]
				r.Answers[0].Probabilities["T6"] = 0
				sequence = []llm.JudgmentResponse{r}
			}
			if tc.lowPrefix {
				sequence[0].Answers[0].Confidence = .55
				sequence = sequence[:1]
			}
			if tc.lowStop {
				sequence[len(sequence)-1].Answers[0].Confidence = .55
			}
			if tc.mismatchedStop {
				sequence[len(sequence)-1].ModelVersion = "different-version"
			}
			for _, r := range sequence {
				steps = append(steps, llm.JudgmentStep{Response: r})
			}
			matchSize := 5
			if changesPrompt {
				steps = append(steps, llm.JudgmentStep{Response: judgmentFixture(t, "changes")})
				matchSize = len(selected)
			}
			steps = append(steps, topicStep(t, fmt.Sprintf("topic-none-%d", matchSize)))
			cfg := judgmentConfig()
			cfg.Model = policy.ModelVersion
			cfg.Policies = map[llm.JudgmentTask]llm.AcceptancePolicy{}
			if !tc.shadow {
				cfg.Policies[llm.TaskTopicShortlist] = llm.AcceptancePolicy{Name: policy.Name, ModelVersion: policy.ModelVersion, Threshold: policy.Threshold}
			}
			j, fake := topicCapability(t, cfg, steps...)
			registry, err := llm.NewFake(llm.Default(), fx)
			if err != nil {
				t.Fatal(err)
			}
			config := testConfig(src)
			config.Repo.Boost = cfg
			listenConfig := net.ListenConfig{}
			listener, err := listenConfig.Listen(t.Context(), "tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			finished := make(chan error, 1)
			go func() {
				finished <- assertworker.Run(ctx, config, assertworker.Deps{Pool: pool, LLM: registry, Judgments: j, Listener: listener})
			}()
			t.Cleanup(func() {
				cancel()
				select {
				case err := <-finished:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(10 * time.Second):
					t.Error("worker did not stop")
				}
			})
			waitFor(t, "qualified shortlist written", func() bool {
				ss, err := l2.New(pool).StancesFrom(t.Context(), f.prID)
				return err == nil && len(ss) == 1
			})
			ss, err := l2.New(pool).StancesFrom(t.Context(), f.prID)
			want := topics[baselineIndex].ID
			if changesPrompt {
				want = selectedTopics[0]
			}
			if err != nil || len(ss) != 1 || ss[0].TopicID != want || ss[0].Position != prPosition {
				t.Fatalf("written=%+v %v; want %s", ss, err, want)
			}
			rs, err := l1.New(pool).JudgmentsFor(t.Context(), judgmentReader(), string(llm.TaskTopicShortlist))
			if err != nil {
				t.Fatal(err)
			}
			if len(rs) != len(sequence) {
				t.Fatalf("journal records=%d; sequence=%d", len(rs), len(sequence))
			}
			for _, r := range rs {
				slot := slices.IndexFunc(sequence, func(s llm.JudgmentResponse) bool { return s.Answers[0].Choice == r.Label })
				if slot < 0 || r.State != "committed" {
					t.Fatalf("unscored decision: %+v", r)
				}
				if adopt && (r.Mode != "accepted" || r.Outcome != "answer" || r.FallbackReason != "") {
					t.Fatalf("qualified sequence refused: %+v", r)
				}
			}
			if fake.Calls() != len(steps) {
				t.Fatalf("calls=%d; want %d", fake.Calls(), len(steps))
			}
			observations, err := l1.New(pool).ObservationsFor(t.Context(), judgmentReader())
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, o := range observations {
				if !slices.Contains(o.Inputs.Documents, f.prID) {
					continue
				}
				found = true
				calls := int64(1)
				if changesPrompt {
					calls = 2
				}
				// A missing optional completion fixture must not hide an
				// attempt to generate from an unqualified shortlist.
				if o.GenerativeCalls == nil || *o.GenerativeCalls != calls {
					t.Fatalf("generation calls=%v; want %d", o.GenerativeCalls, calls)
				}
			}
			if !found {
				t.Fatal("missing worker observation")
			}
		})
	}
}
