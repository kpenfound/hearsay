//go:build integration

package assertworker_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/service/assertworker"
)

func topicCapability(t *testing.T, cfg llm.JudgmentConfig, steps ...llm.JudgmentStep) (llm.Judgments, *llm.FakeJudgmentProvider) {
	t.Helper()
	fake := llm.NewFakeJudgmentProvider(steps...)
	j, err := llm.NewJudgments(cfg, func(llm.JudgmentConfig, string) llm.JudgmentProvider { return fake }, func(string) string { return "fixture-key" })
	if err != nil {
		t.Fatal(err)
	}
	return j, fake
}

func topicStep(t *testing.T, name string) llm.JudgmentStep {
	t.Helper()
	return llm.JudgmentStep{Response: topicFixture(t, name)}
}

// Topic rows originate in generative extraction. Additional fixture topics use
// the same provenance and join keys; order is read back through L2's matcher.
func topicSetup(t *testing.T, pool *pgxpool.Pool, src string, n int) (fixture, []l2.Topic, []assertworker.Candidate) {
	t.Helper()
	f := store(t, pool, src)
	assertDoc(t, newAsserter(t, pool, src, loadFixtures(t)), f.issueID, src)
	graph := l2.New(pool)
	topics, err := graph.Topics(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < n; i++ {
		tp := topics[0]
		tp.ID = fmt.Sprintf("%s-extra-%02d", tp.ID, i)
		tp.Name = fmt.Sprintf("Fixture question %d", i)
		if _, err = graph.OpenTopic(t.Context(), tp); err != nil {
			t.Fatal(err)
		}
		version := time.Now()
		if _, _, err = graph.AppendStance(t.Context(), l2.Stance{ID: l2.StanceID(tp.ID, f.issueID, issuePosition, version, l2.TierInferred), TopicID: tp.ID, Position: issuePosition, StatedAt: issueDoc(t, src).Time.LastActivity, Evidence: []string{f.issueID}, Tier: l2.TierInferred, ACL: tp.ACL}, version); err != nil {
			t.Fatal(err)
		}
	}
	offered, err := graph.TopicsByJoinKeys(t.Context(), src, l2.JoinKeys(prDoc(t, src)), prDoc(t, src).ACL, assertworker.MaxPoolCandidates)
	if err != nil {
		t.Fatal(err)
	}
	cs := make([]assertworker.Candidate, len(offered))
	for i, tp := range offered {
		cs[i] = assertworker.Candidate{Name: tp.Name, Current: issuePosition}
	}
	return f, offered, cs
}

func extractionFixture(t *testing.T, src string, candidates []assertworker.Candidate, assertions string) *llm.Fixtures {
	t.Helper()
	fx := llm.NewFixtures()
	if err := fx.Add(llm.CompletionFixture{Tier: llm.TierAssert, Request: assertworker.RequestFor(prDoc(t, src), candidates, assertBudget()), Response: llm.Response{JSON: []byte(`{"assertions":[` + assertions + `]}`)}}); err != nil {
		t.Fatal(err)
	}
	return fx
}

func oneAssertion(topic, position string) string {
	judgement := `,"judgement":"changes"`
	if topic == assertworker.NewTopic {
		judgement = ""
	}
	return fmt.Sprintf(`{"topic":%q,"topic_name":"Generative question","position":%q%s}`, topic, position, judgement)
}

func topicPolicy(cfg *llm.JudgmentConfig, task llm.JudgmentTask) {
	if cfg.Policies == nil {
		cfg.Policies = map[llm.JudgmentTask]llm.AcceptancePolicy{}
	}
	cfg.Policies[task] = llm.AcceptancePolicy{Name: "qualified-topic-fixture", ModelVersion: "jev-fixture-v1", Threshold: .9}
}

func TestTopicMatchingFixturesAndWholeExtractionRecovery(t *testing.T) {
	pool := scratchPool(t)
	for _, tc := range []struct {
		name, match                                  string
		shadow, noComparison, providerFailure, adopt bool
	}{
		{name: "shadow", match: "topic-T2-2", shadow: true},
		{name: "none fit", match: "topic-none-2"},
		{name: "uncertain", match: "topic-uncertain-2"},
		{name: "invalid", match: "topic-invalid-2"},
		{name: "provider failure", providerFailure: true},
		{name: "reassignment needs qualified comparison", match: "topic-T2-2", noComparison: true},
		{name: "qualified reassignment", match: "topic-T2-2", adopt: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := newSource(t)
			f, topics, cs := topicSetup(t, pool, src, 2)
			baseline := oneAssertion("T1", prPosition) + "," + oneAssertion("new", "Second generative position.")
			fx := extractionFixture(t, src, cs, baseline)
			cfg := judgmentConfig()
			topicPolicy(&cfg, llm.TaskTopicMatching)
			if tc.shadow {
				cfg.Policies = nil
			}
			if tc.noComparison {
				delete(cfg.Policies, llm.TaskStanceComparison)
			}
			step := topicStep(t, "topic-none-2")
			if tc.match != "" {
				step = topicStep(t, tc.match)
			}
			if tc.providerFailure {
				step = llm.JudgmentStep{Error: errors.New("fixture failure")}
			}
			steps := []llm.JudgmentStep{{Response: judgmentFixture(t, "changes")}, topicStep(t, "shortlist-none-2"), step}
			if tc.match == "topic-T2-2" && !tc.shadow {
				steps = append(steps, llm.JudgmentStep{Response: judgmentFixture(t, "restates")})
			}
			steps = append(steps, topicStep(t, "topic-none-2"))
			j, fake := topicCapability(t, cfg, steps...)
			r := assertDoc(t, boostedAsserter(t, pool, src, cfg, j, fx), f.prID, src)
			if r.StancesWritten != 2 || r.TopicsOpened != 1 {
				t.Fatalf("result=%+v", r)
			}
			written, err := l2.New(pool).StancesFrom(t.Context(), f.prID)
			if err != nil || len(written) != 2 {
				t.Fatalf("written=%+v, %v", written, err)
			}
			target, want := topics[0].ID, l2.JudgementChanges
			if tc.adopt {
				target, want = topics[1].ID, l2.JudgementRestates
			}
			for _, s := range written {
				if s.Position == prPosition && (s.TopicID != target || s.Judgement != want) {
					t.Fatalf("stance=%+v; target %s judgement %s", s, target, want)
				}
			}
			var ops int
			if err = pool.QueryRow(t.Context(), `SELECT count(*) FROM l2_topic_operations WHERE scope=$1`, src).Scan(&ops); err != nil || ops != 0 {
				t.Fatalf("automatic operations=%d %v", ops, err)
			}
			rs, err := l1.New(pool).JudgmentsFor(t.Context(), judgmentReader(), string(llm.TaskTopicMatching))
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for _, r := range rs {
				if r.Inputs.Spec.DocumentID == f.prID {
					n++
					if r.State != "committed" {
						t.Fatalf("uncommitted %+v", r)
					}
				}
			}
			if n != 2 {
				t.Fatalf("topic judgments=%d", n)
			}
			for _, req := range fake.Requests() {
				if req.Questions[0].ID != "stance" && !slices.Contains(req.Questions[0].Options, "none_fit") {
					t.Fatal("missing none-fit")
				}
			}
			// A normal restart cannot replay an unrecorded topic choice.
			retry := boostedAsserter(t, pool, src, cfg, j, llm.NewFixtures())
			before := fake.Calls()
			if r := assertDoc(t, retry, f.prID, src); !r.Unchanged || fake.Calls() != before {
				t.Fatal("restart reinterpreted topic decision")
			}
		})
	}
}

func TestTopicReassignmentJournalRefusalRestoresEntireJob(t *testing.T) {
	for _, kind := range []string{"write failure", "inputs changed", "observation refusal"} {
		t.Run(kind, func(t *testing.T) {
			pool := scratchPool(t)
			src := newSource(t)
			f, topics, cs := topicSetup(t, pool, src, 2)
			fx := extractionFixture(t, src, cs, oneAssertion("T1", prPosition)+","+oneAssertion("new", "Second generative position."))
			cfg := judgmentConfig()
			topicPolicy(&cfg, llm.TaskTopicMatching)
			j, _ := topicCapability(t, cfg, llm.JudgmentStep{Response: judgmentFixture(t, "restates")}, topicStep(t, "shortlist-none-2"), topicStep(t, "topic-T2-2"), llm.JudgmentStep{Response: judgmentFixture(t, "restates")}, topicStep(t, "topic-T2-2"), llm.JudgmentStep{Response: judgmentFixture(t, "restates")})
			a := boostedAsserter(t, pool, src, cfg, j, fx)
			accepted := false
			var refused error
			a.WithJournalCommit(func(ctx context.Context, tx pgx.Tx, rs []l1.JudgmentRecord, obs []l1.PipelineObservation, opts l1.JournalCommitOptions) error {
				for _, r := range rs {
					if r.Task == string(llm.TaskTopicMatching) && r.Label == "T2" && r.Mode == "accepted" {
						accepted = true
					}
				}
				if len(rs) > 0 {
					switch kind {
					case "write failure":
						if _, err := pool.Exec(ctx, `CREATE FUNCTION refuse_topic() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.state='committed' THEN RAISE EXCEPTION 'fixture failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER refuse_topic BEFORE INSERT OR UPDATE ON judgment_journal FOR EACH ROW EXECUTE FUNCTION refuse_topic()`); err != nil {
							t.Fatal(err)
						}
					case "inputs changed":
						if _, err := pool.Exec(ctx, `UPDATE l1_docs SET text=text || ' revised' WHERE id=$1`, f.prID); err != nil {
							t.Fatal(err)
						}
					}
				}
				if kind == "observation refusal" && len(obs) > 0 {
					return l1.JournalStorageFailure
				}
				err := l1.CommitJudgments(ctx, tx, rs, obs, opts)
				if err != nil {
					refused = err
				}
				return err
			})
			r := assertDoc(t, a, f.prID, src)
			if !accepted || r.TopicsOpened != 1 || r.StancesWritten != 2 {
				t.Fatalf("accepted=%v result=%+v", accepted, r)
			}
			if kind != "observation refusal" && refused == nil {
				t.Fatal("did not inject refusal")
			}
			ss, err := l2.New(pool).StancesFrom(t.Context(), f.prID)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range ss {
				if s.TopicID == topics[1].ID {
					t.Fatalf("boost survived: %+v", s)
				}
				if s.Position == prPosition && (s.TopicID != topics[0].ID || s.Judgement != l2.JudgementChanges) {
					t.Fatalf("partial baseline: %+v", s)
				}
			}
			var n int
			if err = pool.QueryRow(t.Context(), `SELECT count(*) FROM judgment_journal WHERE state='committed'`).Scan(&n); err != nil || n != 0 {
				t.Fatalf("committed after refusal=%d %v", n, err)
			}
		})
	}
}

func TestBroaderShortlistShadowAndQualifiedVariant(t *testing.T) {
	for _, tc := range []struct {
		name, terminal, reason            string
		qualified, refuse, failGeneration bool
	}{
		{name: "shadow"},
		{name: "qualified", qualified: true},
		{name: "qualified journal refusal", qualified: true, refuse: true},
		{name: "optional generation failure", qualified: true, failGeneration: true},
		{name: "uncertain termination", qualified: true, terminal: "shortlist-uncertain-none-after-T6", reason: "low_confidence"},
		{name: "mismatched model termination", qualified: true, terminal: "shortlist-mismatched-none-after-T6", reason: "model_version_mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := scratchPool(t)
			src := newSource(t)
			f, topics, cs := topicSetup(t, pool, src, 6)
			fx := extractionFixture(t, src, cs[:5], oneAssertion("T1", prPosition))
			if !tc.failGeneration {
				if err := fx.Add(llm.CompletionFixture{Tier: llm.TierAssert, Request: assertworker.RequestFor(prDoc(t, src), cs[5:], assertBudget()), Response: llm.Response{JSON: []byte(`{"assertions":[` + oneAssertion("T1", prPosition) + `]}`)}}); err != nil {
					t.Fatal(err)
				}
			}
			cfg := judgmentConfig()
			cfg.Policies = nil
			if tc.qualified {
				topicPolicy(&cfg, llm.TaskTopicShortlist)
			}
			terminal := tc.terminal
			if terminal == "" {
				terminal = "shortlist-none-after-T6"
			}
			steps := []llm.JudgmentStep{{Response: judgmentFixture(t, "changes")}, topicStep(t, "shortlist-T6-6"), topicStep(t, terminal)}
			adopt := tc.qualified && tc.reason == ""
			matchSize := 5
			if adopt && !tc.failGeneration {
				steps = append(steps, llm.JudgmentStep{Response: judgmentFixture(t, "changes")})
				matchSize = 1
			}
			steps = append(steps, topicStep(t, fmt.Sprintf("topic-none-%d", matchSize)))
			j, fake := topicCapability(t, cfg, steps...)
			a := boostedAsserter(t, pool, src, cfg, j, fx)
			if tc.refuse {
				a.WithJournalCommit(func(context.Context, pgx.Tx, []l1.JudgmentRecord, []l1.PipelineObservation, l1.JournalCommitOptions) error {
					return l1.JournalStorageFailure
				})
			}
			assertDoc(t, a, f.prID, src)
			ss, err := l2.New(pool).StancesFrom(t.Context(), f.prID)
			target := topics[0].ID
			if adopt && !tc.refuse && !tc.failGeneration {
				target = topics[5].ID
			}
			if err != nil || len(ss) != 1 || ss[0].TopicID != target {
				t.Fatalf("written=%+v %v; want %s", ss, err, target)
			}
			if req := fake.Requests()[1]; len(req.Questions[0].Options) != 7 {
				t.Fatalf("broader pool=%+v", req)
			}
			if tc.reason != "" {
				rs, err := l1.New(pool).JudgmentsFor(t.Context(), judgmentReader(), string(llm.TaskTopicShortlist))
				if err != nil {
					t.Fatal(err)
				}
				if len(rs) != 2 || rs[1].Label != "T6" || rs[1].Outcome != "answer" || rs[1].Mode != "accepted" || rs[0].FallbackReason != tc.reason {
					t.Fatalf("shortlist judgments=%+v; want accepted prefix and %s termination", rs, tc.reason)
				}
				if fake.Calls() != 4 {
					t.Fatalf("provider calls=%d; want 4 without alternate extraction comparisons", fake.Calls())
				}
			}
			if !tc.refuse {
				obs, err := l1.New(pool).ObservationsFor(t.Context(), judgmentReader())
				if err != nil {
					t.Fatal(err)
				}
				var found bool
				for _, o := range obs {
					if slices.Contains(o.Inputs.Documents, f.prID) {
						found = true
						calls := int64(1)
						if adopt {
							calls = 2
						}
						if o.CandidateCount == nil || *o.CandidateCount != 11 || o.GenerativeCalls == nil || *o.GenerativeCalls != calls {
							t.Fatalf("additional processing missing: %+v", o)
						}
					}
				}
				if !found {
					t.Fatal("no observation")
				}
			}
		})
	}
}

func TestTopicCandidatesRespectScopeLedgerAndEveryReader(t *testing.T) {
	for _, retrieval := range []string{"references", "similarity"} {
		t.Run(retrieval, func(t *testing.T) {
			pool := scratchPool(t)
			src := newSource(t)
			f, topics, _ := topicSetup(t, pool, src, 7)
			graph := l2.New(pool)
			// Keep a readable topic, but replace its current position with evidence
			// readable by only one of the incoming document's two readers.
			incoming := prDoc(t, src)
			incoming.ACL = connector.ACL{{Kind: connector.ACLIdentity, Source: src, NativeID: "reader-a"}, {Kind: connector.ACLIdentity, Source: src, NativeID: "reader-b"}}
			if _, err := l1.New(pool).Put(t.Context(), incoming); err != nil {
				t.Fatal(err)
			}
			private := issueDoc(t, src)
			private.ID = l1.DocID(src, "private-topic")
			private.Source.NativeID = "private-topic"
			private.ACL = incoming.ACL[:1]
			private.Text = "private evidence"
			if _, err := l1.New(pool).Put(t.Context(), private); err != nil {
				t.Fatal(err)
			}
			tp := topics[0]
			tp.ID += "-private"
			tp.OpenedBy = private.ID
			tp.Name = "PRIVATE TOPIC"
			if _, err := graph.OpenTopic(t.Context(), tp); err != nil {
				t.Fatal(err)
			}
			cross := topics[0]
			cross.ID += "-other-scope"
			cross.Scope = src + "-other"
			cross.Name = "OTHER SCOPE"
			if _, err := graph.OpenTopic(t.Context(), cross); err != nil {
				t.Fatal(err)
			}
			version := time.Now()
			hidden := topics[3]
			if _, _, err := graph.AppendStance(t.Context(), l2.Stance{ID: l2.StanceID(hidden.ID, private.ID, "SECRET POSITION", version, l2.TierInferred), TopicID: hidden.ID, Position: "SECRET POSITION", StatedAt: incoming.Time.LastActivity.Add(time.Hour), Evidence: []string{private.ID}, Tier: l2.TierInferred, ACL: private.ACL}, version); err != nil {
				t.Fatal(err)
			}
			// These are explicit human operations. Judgments must add no operations.
			if _, err := l2.Operate(t.Context(), pool, testRepo(src), l2.OperationRequest{Kind: l2.OperationMerge, Principal: "kyle", From: topics[0].ID, Into: topics[1].ID}); err != nil {
				t.Fatal(err)
			}
			history, err := graph.StanceHistory(t.Context(), topics[6].ID)
			if err != nil {
				t.Fatal(err)
			}
			surviving := issueDoc(t, src)
			surviving.ID, surviving.Source.NativeID = l1.DocID(src, "surviving"), "surviving"
			if _, err = l1.New(pool).Put(t.Context(), surviving); err != nil {
				t.Fatal(err)
			}
			version = time.Now()
			if _, _, err = graph.AppendStance(t.Context(), l2.Stance{ID: l2.StanceID(topics[6].ID, surviving.ID, "Surviving public position", version, l2.TierInferred), TopicID: topics[6].ID, Position: "Surviving public position", StatedAt: incoming.Time.LastActivity, Evidence: []string{surviving.ID}, Tier: l2.TierInferred, ACL: surviving.ACL}, version); err != nil {
				t.Fatal(err)
			}
			split, err := l2.Operate(t.Context(), pool, testRepo(src), l2.OperationRequest{Kind: l2.OperationSplit, Principal: "kyle", Topic: topics[6].ID, Name: "Human split question", Stances: []string{history[0].ID}})
			if err != nil {
				t.Fatal(err)
			}
			splitID := split.Topics[1]
			if retrieval == "similarity" {
				// Exercise the second existing matcher without bypassing its ACLs. The
				// fixture vector is the same for all evidence and the incoming document.
				vector := "[1," + strings.Repeat("0,", l1.EmbeddingDimensions-2) + "0]"
				if _, err = pool.Exec(t.Context(), `UPDATE l1_docs SET embedding=$1::vector WHERE id=ANY($2)`, vector, []string{f.issueID, f.prID, private.ID, surviving.ID}); err != nil {
					t.Fatal(err)
				}
				if _, err = pool.Exec(t.Context(), `UPDATE l2_topics SET join_keys='{}' WHERE scope=$1 OR scope=$2`, src, cross.Scope); err != nil {
					t.Fatal(err)
				}
			}
			get := func(limit int) []l2.Topic {
				ts, err := graph.TopicsByJoinKeys(t.Context(), src, l2.JoinKeys(incoming), incoming.ACL, limit)
				if err != nil {
					t.Fatal(err)
				}
				ids := []string{}
				for _, tp := range ts {
					ids = append(ids, tp.ID)
				}
				near, err := graph.TopicsBySimilarity(t.Context(), src, incoming.ID, incoming.ACL, assertworker.MaxSimilarityDistance, limit-len(ts), ids)
				if err != nil {
					t.Fatal(err)
				}
				return append(ts, near...)
			}
			baseline, poolTopics := get(5), get(20)
			if len(poolTopics) <= 5 {
				t.Fatalf("pool too small: %+v", poolTopics)
			}
			candidates := func(ts []l2.Topic) []assertworker.Candidate {
				cs := make([]assertworker.Candidate, len(ts))
				for i, tp := range ts {
					cs[i].Name = tp.Name
					hs, err := graph.StanceHistory(t.Context(), tp.ID)
					if err != nil {
						t.Fatal(err)
					}
					if cur, ok := l2.Current(hs); ok && tp.ID != hidden.ID {
						cs[i].Current = cur.Position
					}
				}
				return cs
			}
			cs := candidates(baseline)
			poolCS := candidates(poolTopics)
			cfg := judgmentConfig()
			cfg.Policies = nil
			steps := []llm.JudgmentStep{}
			if cs[0].Current != "" {
				steps = append(steps, llm.JudgmentStep{Response: judgmentFixture(t, "changes")})
			}
			steps = append(steps, topicStep(t, fmt.Sprintf("shortlist-none-%d", len(poolTopics))), topicStep(t, fmt.Sprintf("topic-none-%d", len(baseline))))
			j, fake := topicCapability(t, cfg, steps...)
			assertDoc(t, boostedAsserter(t, pool, src, cfg, j, extractionFixture(t, src, cs, oneAssertion("T1", prPosition))), f.prID, src)
			expected := map[string][]assertworker.Candidate{"shortlist": poolCS, "topic": cs}
			seen := map[string]bool{}
			for _, req := range fake.Requests() {
				if req.Questions[0].ID == "stance" {
					continue
				}
				id := req.Questions[0].ID
				seen[id] = true
				if strings.Contains(req.State, "SECRET") || strings.Contains(req.State, "PRIVATE TOPIC") || strings.Contains(req.State, "OTHER SCOPE") {
					t.Fatalf("unreadable evidence sent: %s", req.State)
				}
				var state struct {
					Candidates []struct{ Label, Name, Current string }
				}
				if err = json.Unmarshal([]byte(req.State), &state); err != nil {
					t.Fatal(err)
				}
				want := expected[id]
				if len(state.Candidates) != len(want) {
					t.Fatalf("%s candidates=%+v", id, state.Candidates)
				}
				for i, c := range state.Candidates {
					if c.Name != want[i].Name || c.Current != want[i].Current {
						t.Fatalf("candidate=%+v want=%+v", c, want[i])
					}
				}
			}
			if !seen["shortlist"] || !seen["topic"] {
				t.Fatalf("missing integrations: %+v", seen)
			}
			reader := judgmentReader()
			reader.Audience = incoming.ACL
			for _, task := range []llm.JudgmentTask{llm.TaskTopicMatching, llm.TaskTopicShortlist} {
				rs, err := l1.New(pool).JudgmentsFor(t.Context(), reader, string(task))
				if err != nil || len(rs) != 1 {
					t.Fatalf("journal=%+v %v", rs, err)
				}
				ids := rs[0].Inputs.Spec.CandidateTopicIDs
				if slices.Contains(ids, topics[0].ID) || slices.Contains(ids, tp.ID) || slices.Contains(ids, cross.ID) {
					t.Fatalf("unresolved/unreadable candidates: %v", ids)
				}
				if task == llm.TaskTopicShortlist {
					n := 0
					for _, id := range ids {
						if id == topics[1].ID {
							n++
						}
					}
					if n != 1 || !slices.Contains(ids, splitID) {
						t.Fatalf("effective ledger not followed: %v split=%s", ids, splitID)
					}
				}
				for _, id := range rs[0].Inputs.Spec.ComparedStanceIDs {
					if strings.Contains(id, "SECRET") {
						t.Fatal("private comparison identity")
					}
				}
			}
			var n int
			if err = pool.QueryRow(t.Context(), `SELECT count(*) FROM l2_topic_operations WHERE scope=$1`, src).Scan(&n); err != nil || n != 2 {
				t.Fatalf("judgments altered ledger: %d %v", n, err)
			}
		})
	}
}

func TestTopicReplacementWithheldPositionKeepsNullComparison(t *testing.T) {
	pool := scratchPool(t)
	src := newSource(t)
	f, topics, cs := topicSetup(t, pool, src, 2)
	private := issueDoc(t, src)
	private.ID = l1.DocID(src, "private-current")
	private.Source.NativeID = "private-current"
	private.ACL = connector.ACL{{Kind: connector.ACLIdentity, Source: src, NativeID: "private"}}
	if _, err := l1.New(pool).Put(t.Context(), private); err != nil {
		t.Fatal(err)
	}
	version := time.Now()
	if _, _, err := l2.New(pool).AppendStance(t.Context(), l2.Stance{ID: l2.StanceID(topics[1].ID, private.ID, "SECRET POSITION", version, l2.TierInferred), TopicID: topics[1].ID, Position: "SECRET POSITION", StatedAt: prDoc(t, src).Time.LastActivity.Add(time.Hour), Evidence: []string{private.ID}, Tier: l2.TierInferred, ACL: private.ACL}, version); err != nil {
		t.Fatal(err)
	}
	cs[1].Current = ""
	cfg := judgmentConfig()
	topicPolicy(&cfg, llm.TaskTopicMatching)
	j, fake := topicCapability(t, cfg, llm.JudgmentStep{Response: judgmentFixture(t, "changes")}, topicStep(t, "shortlist-none-2"), topicStep(t, "topic-T2-2"))
	assertDoc(t, boostedAsserter(t, pool, src, cfg, j, extractionFixture(t, src, cs, oneAssertion("T1", prPosition))), f.prID, src)
	ss, err := l2.New(pool).StancesFrom(t.Context(), f.prID)
	if err != nil || len(ss) != 1 || ss[0].TopicID != topics[1].ID || ss[0].Judgement != l2.JudgementUnknown {
		t.Fatalf("withheld result=%+v %v", ss, err)
	}
	if fake.Calls() != 3 {
		t.Fatalf("replacement comparison sent: %d", fake.Calls())
	}
	for _, req := range fake.Requests() {
		if strings.Contains(req.State, "SECRET") {
			t.Fatal("withheld position sent")
		}
	}
}

func TestAllAssertionJudgmentsShareOneBudget(t *testing.T) {
	pool := scratchPool(t)
	src := newSource(t)
	f, _, cs := topicSetup(t, pool, src, 4)
	assertions := []string{}
	for i := range 4 {
		assertions = append(assertions, oneAssertion(fmt.Sprintf("T%d", i+1), fmt.Sprintf("Generative position %d", i+1)))
	}
	cfg := judgmentConfig()
	topicPolicy(&cfg, llm.TaskTopicMatching)
	topicPolicy(&cfg, llm.TaskTopicShortlist)
	cfg.AssertionJobBudget = 500 * time.Millisecond
	steps := []llm.JudgmentStep{}
	for range 4 {
		steps = append(steps, llm.JudgmentStep{Delay: 40 * time.Millisecond, Response: judgmentFixture(t, "restates")})
	}
	steps = append(steps, llm.JudgmentStep{Delay: 40 * time.Millisecond, Response: topicFixture(t, "shortlist-none-4")}, llm.JudgmentStep{Delay: 40 * time.Millisecond, Response: topicFixture(t, "topic-T2-4")}, llm.JudgmentStep{Delay: time.Second, Response: judgmentFixture(t, "restates")})
	j, fake := topicCapability(t, cfg, steps...)
	started := time.Now()
	r := assertDoc(t, boostedAsserter(t, pool, src, cfg, j, extractionFixture(t, src, cs, strings.Join(assertions, ","))), f.prID, src)
	if elapsed := time.Since(started); elapsed > 1200*time.Millisecond || r.StancesWritten != 4 || fake.Calls() != 7 {
		t.Fatalf("result=%+v attempts=%d elapsed=%s", r, fake.Calls(), elapsed)
	}
	seen := map[string]bool{}
	for _, req := range fake.Requests() {
		seen[req.Questions[0].ID] = true
	}
	if !reflect.DeepEqual(seen, map[string]bool{"stance": true, "shortlist": true, "topic": true}) {
		t.Fatalf("integrations=%v", seen)
	}
	ss, err := l2.New(pool).StancesFrom(t.Context(), f.prID)
	if err != nil || len(ss) != 4 {
		t.Fatalf("stances=%+v %v", ss, err)
	}
	for _, s := range ss {
		if s.Judgement != l2.JudgementChanges {
			t.Fatalf("partial boost survived exhaustion: %+v", s)
		}
	}
	rs, err := l1.New(pool).JudgmentsFor(t.Context(), judgmentReader(), string(llm.TaskTopicMatching))
	if err != nil || len(rs) != 4 {
		t.Fatalf("matching records=%+v %v", rs, err)
	}
	exhausted := 0
	for _, r := range rs {
		if r.FallbackReason == "budget_exhausted" {
			exhausted++
			if r.Attempts == nil || *r.Attempts != 0 {
				t.Fatalf("provider called after budget: %+v", r)
			}
		}
	}
	if exhausted != 3 {
		t.Fatalf("remaining topic fallbacks=%d", exhausted)
	}
}

func TestFailedReplacementComparisonRestoresEarlierAcceptedChanges(t *testing.T) {
	for _, kind := range []string{"abstain", "uncertain", "invalid", "failure"} {
		t.Run(kind, func(t *testing.T) {
			pool := scratchPool(t)
			src := newSource(t)
			f, topics, cs := topicSetup(t, pool, src, 2)
			cfg := judgmentConfig()
			topicPolicy(&cfg, llm.TaskTopicMatching)
			replacement := llm.JudgmentStep{Response: judgmentFixture(t, "abstain")}
			if kind == "failure" {
				replacement = llm.JudgmentStep{Error: errors.New("fixture replacement failure")}
			} else {
				replacement.Response = judgmentFixture(t, kind)
			}
			j, _ := topicCapability(t, cfg, llm.JudgmentStep{Response: judgmentFixture(t, "restates")}, topicStep(t, "shortlist-none-2"), topicStep(t, "topic-T2-2"), llm.JudgmentStep{Response: judgmentFixture(t, "restates")}, topicStep(t, "topic-T1-2"), replacement)
			fx := extractionFixture(t, src, cs, oneAssertion("T1", prPosition)+","+oneAssertion("new", "Second generative position."))
			r := assertDoc(t, boostedAsserter(t, pool, src, cfg, j, fx), f.prID, src)
			if r.TopicsOpened != 1 || r.StancesWritten != 2 {
				t.Fatalf("result=%+v", r)
			}
			ss, err := l2.New(pool).StancesFrom(t.Context(), f.prID)
			if err != nil || len(ss) != 2 {
				t.Fatalf("stances=%+v %v", ss, err)
			}
			for _, s := range ss {
				if s.TopicID == topics[1].ID {
					t.Fatalf("earlier reassignment survived: %+v", s)
				}
				if s.Position == prPosition && s.Judgement != l2.JudgementChanges {
					t.Fatalf("earlier comparison survived: %+v", s)
				}
			}
		})
	}
}

func TestCommittedTopicAndReplacementDecisionsReuseExactInputs(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprintf("stale=%v", stale), func(t *testing.T) {
			pool := scratchPool(t)
			src := newSource(t)
			f, topics, cs := topicSetup(t, pool, src, 2)
			spec := l1.JudgmentInputSpec{DocumentID: f.prID, Scope: []string{src}, ACL: prDoc(t, src).ACL}
			for _, topic := range topics {
				spec.CandidateTopicIDs = append(spec.CandidateTopicIDs, topic.ID)
				history, err := l2.New(pool).StanceHistory(t.Context(), topic.ID)
				if err != nil {
					t.Fatal(err)
				}
				current, ok := l2.Current(history)
				if !ok {
					t.Fatal("fixture has no current position")
				}
				spec.ComparedStanceIDs = append(spec.ComparedStanceIDs, current.ID)
			}
			inputs, err := l1.New(pool).CaptureJudgmentInputs(t.Context(), spec)
			if err != nil {
				t.Fatal(err)
			}
			records := []l1.JudgmentRecord{}
			for _, tc := range []struct {
				task     llm.JudgmentTask
				version  string
				request  llm.JudgmentRequest
				response llm.JudgmentResponse
			}{
				{llm.TaskTopicMatching, "topic-matching-v1/existing", assertworker.TopicMatchingRequest("Generative question", prPosition, cs), topicFixture(t, "topic-T2-2")},
				{llm.TaskStanceComparison, "stance-comparison-v1", assertworker.StanceComparisonRequest(topics[1].Name, issuePosition, prPosition, prDoc(t, src).Body.Outcome), judgmentFixture(t, "restates")},
			} {
				raw, err := json.Marshal(tc.request)
				if err != nil {
					t.Fatal(err)
				}
				hash := sha256.Sum256(raw)
				answer := tc.response.Answers[0]
				attempts := int64(1)
				records = append(records, l1.JudgmentRecord{ID: fmt.Sprintf("saved-%s", tc.task), Task: string(tc.task), QuestionVersion: tc.version, QuestionHash: hex.EncodeToString(hash[:]), Inputs: inputs, Mode: "accepted", Policy: "historical-policy", ModelVersion: "historical-version", Outcome: "answer", Label: answer.Choice, Probabilities: answer.Probabilities, Confidence: &answer.Confidence, Attempts: &attempts})
			}
			if err = pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
				return l1.CommitJudgments(t.Context(), tx, records, nil, l1.JournalCommitOptions{})
			}); err != nil {
				t.Fatal(err)
			}
			if stale {
				if err = l2.New(pool).ExtendTopic(t.Context(), topics[1].ID, []string{"new-context"}, nil); err != nil {
					t.Fatal(err)
				}
			}
			cfg := judgmentConfig()
			cfg.Policies = nil
			steps := []llm.JudgmentStep{{Response: judgmentFixture(t, "changes")}, topicStep(t, "shortlist-none-2")}
			if stale {
				steps = append(steps, topicStep(t, "topic-none-2"))
			}
			j, fake := topicCapability(t, cfg, steps...)
			assertDoc(t, boostedAsserter(t, pool, src, cfg, j, extractionFixture(t, src, cs, oneAssertion("T1", prPosition))), f.prID, src)
			want, judgement, calls := topics[1].ID, l2.JudgementRestates, 2
			if stale {
				want, judgement, calls = topics[0].ID, l2.JudgementChanges, 3
			}
			ss, err := l2.New(pool).StancesFrom(t.Context(), f.prID)
			if err != nil || len(ss) != 1 || ss[0].TopicID != want || ss[0].Judgement != judgement || fake.Calls() != calls {
				t.Fatalf("stances=%+v %v calls=%d; want=%s %s calls=%d", ss, err, fake.Calls(), want, judgement, calls)
			}
		})
	}
}

// A committed terminal decision keeps its original policy semantics even when
// the worker now has a qualified policy or a different pinned model.
func TestBroaderShortlistReusesTerminalDecisionWithoutReinterpretation(t *testing.T) {
	for _, tc := range []struct {
		name, mode, outcome, reason string
		adopt                       bool
	}{
		{name: "saved shadow abstention", mode: "shadow", outcome: "abstain", reason: "none_fit"},
		{name: "saved shadow answer", mode: "shadow", outcome: "answer"},
		{name: "saved qualified answer", mode: "accepted", outcome: "answer", adopt: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := scratchPool(t)
			src := newSource(t)
			f, topics, cs := topicSetup(t, pool, src, 6)
			spec := l1.JudgmentInputSpec{DocumentID: f.prID, Scope: []string{src}, ACL: prDoc(t, src).ACL}
			for _, topic := range topics {
				spec.CandidateTopicIDs = append(spec.CandidateTopicIDs, topic.ID)
				history, err := l2.New(pool).StanceHistory(t.Context(), topic.ID)
				if err != nil {
					t.Fatal(err)
				}
				current, ok := l2.Current(history)
				if !ok {
					t.Fatal("fixture has no current position")
				}
				spec.ComparedStanceIDs = append(spec.ComparedStanceIDs, current.ID)
			}
			inputs, err := l1.New(pool).CaptureJudgmentInputs(t.Context(), spec)
			if err != nil {
				t.Fatal(err)
			}
			req := assertworker.TopicShortlistRequest(prDoc(t, src).Body.Outcome, cs, []int{5})
			raw, err := json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			hash := sha256.Sum256(raw)
			answer := topicFixture(t, "shortlist-none-after-T6").Answers[0]
			attempts := int64(1)
			r := l1.JudgmentRecord{ID: "saved-termination", Task: string(llm.TaskTopicShortlist), QuestionVersion: "topic-shortlist-v1/slot-2", QuestionHash: hex.EncodeToString(hash[:]), Inputs: inputs, Mode: tc.mode, ModelVersion: "historical-version", Outcome: tc.outcome, Label: answer.Choice, FallbackReason: tc.reason, Probabilities: answer.Probabilities, Confidence: &answer.Confidence, Attempts: &attempts}
			if tc.mode == "accepted" {
				r.Policy = "historical-policy"
			}
			if err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
				return l1.CommitJudgments(t.Context(), tx, []l1.JudgmentRecord{r}, nil, l1.JournalCommitOptions{})
			}); err != nil {
				t.Fatal(err)
			}
			cfg := judgmentConfig()
			cfg.Policies = nil
			topicPolicy(&cfg, llm.TaskTopicShortlist)
			steps := []llm.JudgmentStep{{Response: judgmentFixture(t, "changes")}, topicStep(t, "shortlist-T6-6")}
			matchSize, target, calls, generation := 5, topics[0].ID, 3, int64(1)
			if tc.adopt {
				steps = append(steps, llm.JudgmentStep{Response: judgmentFixture(t, "changes")})
				matchSize, target, calls, generation = 1, topics[5].ID, 4, 2
			}
			steps = append(steps, topicStep(t, fmt.Sprintf("topic-none-%d", matchSize)))
			j, fake := topicCapability(t, cfg, steps...)
			fx := extractionFixture(t, src, cs[:5], oneAssertion("T1", prPosition))
			if err := fx.Add(llm.CompletionFixture{Tier: llm.TierAssert, Request: assertworker.RequestFor(prDoc(t, src), cs[5:], assertBudget()), Response: llm.Response{JSON: []byte(`{"assertions":[` + oneAssertion("T1", prPosition) + `]}`)}}); err != nil {
				t.Fatal(err)
			}
			assertDoc(t, boostedAsserter(t, pool, src, cfg, j, fx), f.prID, src)
			ss, err := l2.New(pool).StancesFrom(t.Context(), f.prID)
			if err != nil || len(ss) != 1 || ss[0].TopicID != target || ss[0].Judgement != l2.JudgementChanges || fake.Calls() != calls {
				t.Fatalf("stances=%+v %v calls=%d; want=%s changes calls=%d", ss, err, fake.Calls(), target, calls)
			}
			rs, err := l1.New(pool).JudgmentsFor(t.Context(), judgmentReader(), string(llm.TaskTopicShortlist))
			if err != nil || len(rs) != 2 {
				t.Fatalf("shortlist judgments=%+v %v; want reused terminal and accepted prefix", rs, err)
			}
			reused := false
			for _, saved := range rs {
				if saved.ID == r.ID {
					reused = true
					if saved.Mode != r.Mode || saved.Outcome != r.Outcome || saved.ModelVersion != r.ModelVersion || saved.Policy != r.Policy || saved.FallbackReason != r.FallbackReason {
						t.Fatalf("reinterpreted saved decision: %+v", saved)
					}
				}
			}
			if !reused {
				t.Fatal("terminal decision was replaced instead of reused")
			}
			obs, err := l1.New(pool).ObservationsFor(t.Context(), judgmentReader())
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, o := range obs {
				if slices.Contains(o.Inputs.Documents, f.prID) {
					found = true
					if o.GenerativeCalls == nil || *o.GenerativeCalls != generation {
						t.Fatalf("observation=%+v; want %d generative calls", o, generation)
					}
				}
			}
			if !found {
				t.Fatal("no observation for the assertion job")
			}
		})
	}
}
