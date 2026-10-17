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
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/principal"
	"github.com/kpenfound/hearsay/internal/queue"
	"github.com/kpenfound/hearsay/internal/service/assertworker"
)

func judgmentReader() l1.Reader {
	return l1.Reader{Effective: principal.Effective{Human: "kyle", Grant: principal.Grant{Scopes: principal.AllScopes()}}}
}

func boostedAsserter(t *testing.T, pool *pgxpool.Pool, src string, cfg llm.JudgmentConfig, j llm.Judgments, fx *llm.Fixtures) *assertworker.Asserter {
	t.Helper()
	config := testConfig(src)
	config.Repo.Boost = cfg
	registry, err := llm.NewFake(llm.Default(), fx)
	if err != nil {
		t.Fatal(err)
	}
	a, err := assertworker.New(pool, registry, config)
	if err != nil {
		t.Fatal(err)
	}
	return a.WithJudgments(j)
}

func writtenJudgment(t *testing.T, pool *pgxpool.Pool, doc string, want l2.Judgement) {
	t.Helper()
	stances, err := l2.New(pool).StancesFrom(t.Context(), doc)
	if err != nil || len(stances) != 1 || stances[0].Judgement != want {
		t.Fatalf("stances = %+v, %v; want %s", stances, err, want)
	}
}

func TestStanceBoostRecordedFallbacksAndPolicies(t *testing.T) {
	pool := scratchPool(t)
	for _, tc := range []struct {
		name, fixture, reason string
		shadow                bool
		step                  llm.JudgmentStep
		timeout               bool
		cooldown              bool
		want                  l2.Judgement
	}{
		{name: "shadow", fixture: "restates", shadow: true, want: l2.JudgementChanges},
		{name: "accepted changes", fixture: "changes", want: l2.JudgementChanges},
		{name: "accepted restates", fixture: "restates", want: l2.JudgementRestates},
		{name: "abstain", fixture: "abstain", reason: "abstain", want: l2.JudgementChanges},
		{name: "low confidence", fixture: "uncertain", reason: "low_confidence", want: l2.JudgementChanges},
		{name: "provider error", step: llm.JudgmentStep{Error: errors.New("fixture error")}, reason: "unavailable", want: l2.JudgementChanges},
		{name: "timeout", fixture: "restates", step: llm.JudgmentStep{Delay: time.Second}, timeout: true, reason: "timeout", want: l2.JudgementChanges},
		{name: "rate limit", step: llm.JudgmentStep{Error: &llm.Error{Status: 429, RetryAfter: time.Second}}, reason: "rate_limited", want: l2.JudgementChanges},
		{name: "invalid response", fixture: "invalid", reason: "invalid_response", want: l2.JudgementChanges},
		{name: "cooldown", cooldown: true, reason: "cooldown", want: l2.JudgementChanges},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := newSource(t)
			f := store(t, pool, src)
			baseline := newAsserter(t, pool, src, loadFixtures(t))
			assertDoc(t, baseline, f.issueID, src)
			cfg := judgmentConfig()
			if tc.shadow {
				cfg.Policies = nil
			}
			if tc.timeout {
				cfg.Timeout = 5 * time.Millisecond
			}
			if tc.fixture != "" {
				tc.step.Response = judgmentFixture(t, tc.fixture)
			}
			if tc.cooldown {
				cfg.CooldownFailures = 1
				tc.step.Error = errors.New("fixture outage")
			}
			j, fake := judgmentCapability(t, cfg, tc.step)
			if tc.cooldown {
				_ = j.Judge(t.Context(), assertworker.StanceComparisonRequest("q", "a", "b", "c"))
			}
			before := fake.Calls()
			fx := loadFixtures(t)
			if tc.name == "accepted changes" {
				// Prove adoption in both directions, not just restates.
				fx = llm.NewFixtures()
				if err := fx.Add(llm.CompletionFixture{Tier: llm.TierAssert, Request: assertworker.RequestFor(prDoc(t, src), []assertworker.Candidate{{Name: topicName, Current: issuePosition}}, assertBudget()), Response: llm.Response{JSON: []byte(`{"assertions":[{"topic":"T1","topic_name":"` + topicName + `","position":"` + prPosition + `","judgement":"restates"}]}`)}}); err != nil {
					t.Fatal(err)
				}
			}
			a := boostedAsserter(t, pool, src, cfg, j, fx)
			assertDoc(t, a, f.prID, src)
			writtenJudgment(t, pool, f.prID, tc.want)
			records, err := l1.New(pool).JudgmentsFor(t.Context(), judgmentReader(), string(llm.TaskStanceComparison))
			if err != nil {
				t.Fatal(err)
			}
			var found []l1.JudgmentRecord
			for _, r := range records {
				if r.Inputs.Spec.DocumentID == f.prID {
					found = append(found, r)
				}
			}
			if len(found) != 1 {
				t.Fatalf("records = %+v", found)
			}
			r := found[0]
			if r.State != "committed" || r.FallbackReason != tc.reason || r.Inputs.Spec.ComparedStanceIDs == nil {
				t.Fatalf("record = %+v", r)
			}
			if tc.shadow && r.Mode != "shadow" {
				t.Fatalf("mode = %s", r.Mode)
			}
			calls := 1
			if tc.cooldown {
				calls = 0
			}
			if fake.Calls()-before != calls {
				t.Fatalf("calls = %d, want %d", fake.Calls()-before, calls)
			}
			if calls == 1 {
				req := fake.Requests()[len(fake.Requests())-1]
				want := assertworker.StanceComparisonRequest(topicName, issuePosition, prPosition, prDoc(t, src).Body.Outcome)
				if !reflect.DeepEqual(req, want) {
					t.Fatalf("request differs from minimized shown evidence: %+v", req)
				}
			}
			// A process restart with changed policy and no fixture cannot reinterpret
			// a consumed judgment, or make a second provider call.
			retryCfg := cfg
			retryCfg.Policies = nil
			restartJ, restartFake := judgmentCapability(t, retryCfg)
			restarted := boostedAsserter(t, pool, src, retryCfg, restartJ, llm.NewFixtures())
			if r := assertDoc(t, restarted, f.prID, src); !r.Unchanged {
				t.Fatalf("retry = %+v", r)
			}
			if restartFake.Calls() != 0 {
				t.Fatal("restart called provider")
			}
			writtenJudgment(t, pool, f.prID, tc.want)
		})
	}
}

func TestStanceBoostWithheldPositionSendsNothing(t *testing.T) {
	pool := scratchPool(t)
	src := newSource(t)
	f := store(t, pool, src)
	assertDoc(t, newAsserter(t, pool, src, loadFixtures(t)), f.issueID, src)
	topics, err := l2.New(pool).Topics(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	note := issueDoc(t, src)
	note.ID = l1.DocID(src, "private-note")
	note.Source.NativeID = "private-note"
	note.ACL = connector.ACL{{Kind: connector.ACLIdentity, Source: src, NativeID: "private"}}
	if _, err = l1.New(pool).Put(t.Context(), note); err != nil {
		t.Fatal(err)
	}
	when := note.Time.LastActivity.Add(time.Hour)
	if _, _, err = l2.New(pool).AppendStance(t.Context(), l2.Stance{ID: l2.StanceID(topics[0].ID, note.ID, "secret-position", when, l2.TierInferred), TopicID: topics[0].ID, Position: "secret-position", StatedAt: when, Evidence: []string{note.ID}, Tier: l2.TierInferred, ACL: note.ACL}, when); err != nil {
		t.Fatal(err)
	}
	fx := llm.NewFixtures()
	if err = fx.Add(llm.CompletionFixture{Tier: llm.TierAssert, Request: assertworker.RequestFor(prDoc(t, src), []assertworker.Candidate{{Name: topicName}}, assertBudget()), Response: llm.Response{JSON: []byte(`{"assertions":[{"topic":"T1","topic_name":"` + topicName + `","position":"` + prPosition + `","judgement":"changes"}]}`)}}); err != nil {
		t.Fatal(err)
	}
	cfg := judgmentConfig()
	j, fake := judgmentCapability(t, cfg)
	assertDoc(t, boostedAsserter(t, pool, src, cfg, j, fx), f.prID, src)
	writtenJudgment(t, pool, f.prID, l2.JudgementUnknown)
	if fake.Calls() != 0 {
		t.Fatal("withheld position sent to provider")
	}
	var n int
	if err = pool.QueryRow(t.Context(), `SELECT count(*) FROM judgment_journal WHERE task='stance_comparison' AND inputs->'spec'->>'document_id'=$1`, f.prID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("withheld judgments = %d, %v", n, err)
	}
}

func TestStanceJournalRefusalWritesBaselineAndRetryDoesNotAdopt(t *testing.T) {
	for _, kind := range []string{"storage failure", "inputs changed", "timeout"} {
		t.Run(kind, func(t *testing.T) {
			pool := scratchPool(t)
			src := newSource(t)
			f := store(t, pool, src)
			assertDoc(t, newAsserter(t, pool, src, loadFixtures(t)), f.issueID, src)
			cfg := judgmentConfig()
			j, fake := judgmentCapability(t, cfg, llm.JudgmentStep{Response: judgmentFixture(t, "restates")})
			a := boostedAsserter(t, pool, src, cfg, j, loadFixtures(t))
			var refused error
			a.WithJournalCommit(func(ctx context.Context, tx pgx.Tx, rs []l1.JudgmentRecord, obs []l1.PipelineObservation, opts l1.JournalCommitOptions) error {
				if len(rs) > 0 && (rs[0].Label != "restates" || rs[0].Outcome != "answer") {
					t.Fatalf("expected accepted result before refusal: %+v", rs)
				}
				if len(rs) > 0 {
					switch kind {
					case "storage failure":
						_, err := pool.Exec(ctx, `CREATE FUNCTION refuse_judgment() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.state='committed' THEN RAISE EXCEPTION 'injected write failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER refuse_judgment BEFORE INSERT OR UPDATE ON judgment_journal FOR EACH ROW EXECUTE FUNCTION refuse_judgment()`)
						if err != nil {
							t.Fatal(err)
						}
					case "inputs changed":
						_, err := pool.Exec(ctx, `UPDATE l1_docs SET text=text || ' revised' WHERE id=$1`, f.prID)
						if err != nil {
							t.Fatal(err)
						}
					case "timeout":
						_, err := tx.Exec(ctx, `CREATE FUNCTION refuse_observation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_sleep(1); RETURN NEW; END $$; CREATE TRIGGER refuse_observation BEFORE INSERT ON pipeline_observations FOR EACH ROW EXECUTE FUNCTION refuse_observation()`)
						if err != nil {
							t.Fatal(err)
						}
						opts.StatementTimeout = 5 * time.Millisecond
					}
				}
				if kind == "timeout" {
					opts.StatementTimeout = 5 * time.Millisecond
				}
				err := l1.CommitJudgments(ctx, tx, rs, obs, opts)
				if err != nil {
					refused = err
				}
				return err
			})
			assertDoc(t, a, f.prID, src)
			want := l1.JournalStorageFailure
			if kind == "inputs changed" {
				want = l1.JournalInputsChanged
			}
			if kind == "timeout" {
				want = l1.JournalTimeout
			}
			if !errors.Is(refused, want) {
				t.Fatalf("refused = %v, want %v", refused, want)
			}
			writtenJudgment(t, pool, f.prID, l2.JudgementChanges)
			var n int
			if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM judgment_journal WHERE state='committed'`).Scan(&n); err != nil || n != 0 {
				t.Fatalf("accepted journal records survived refusal: %d, %v", n, err)
			}
			restarted := boostedAsserter(t, pool, src, cfg, j, llm.NewFixtures())
			if r := assertDoc(t, restarted, f.prID, src); !r.Unchanged {
				t.Fatalf("retry = %+v", r)
			}
			if fake.Calls() != 1 {
				t.Fatalf("retry called provider: %d", fake.Calls())
			}
			writtenJudgment(t, pool, f.prID, l2.JudgementChanges)
		})
	}
}

// Several positions share the one optional deadline; no remaining call starts
// a new timeout, and the completed baseline write uses the live parent context.
func TestStanceJobBudgetWithSeveralPositions(t *testing.T) {
	pool := scratchPool(t)
	src := newSource(t)
	f := store(t, pool, src)
	assertDoc(t, newAsserter(t, pool, src, loadFixtures(t)), f.issueID, src)
	topics, err := l2.New(pool).Topics(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	first, err := l2.New(pool).StanceHistory(t.Context(), topics[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	candidates := []assertworker.Candidate{{Name: topicName, Current: issuePosition}}
	for i := 1; i < 4; i++ {
		topic := topics[0]
		topic.ID = fmt.Sprintf("%s-extra-%d", topic.ID, i)
		topic.Name = fmt.Sprintf("%s %d", topicName, i)
		if _, err = l2.New(pool).OpenTopic(t.Context(), topic); err != nil {
			t.Fatal(err)
		}
		stance := first[0]
		stance.TopicID = topic.ID
		version := time.Now()
		stance.ID = l2.StanceID(topic.ID, f.issueID, stance.Position, version, stance.Tier)
		if _, _, err = l2.New(pool).AppendStance(t.Context(), stance, version); err != nil {
			t.Fatal(err)
		}
		candidates = append(candidates, assertworker.Candidate{Name: topic.Name, Current: issuePosition})
	}
	// Retrieval order is defined by the store, not insertion order.
	offered, err := l2.New(pool).TopicsByJoinKeys(t.Context(), src, l2.JoinKeys(prDoc(t, src)), prDoc(t, src).ACL, assertworker.MaxCandidates)
	if err != nil {
		t.Fatal(err)
	}
	for i, t := range offered {
		candidates[i].Name = t.Name
	}
	assertions := make([]string, len(candidates))
	for i, c := range candidates {
		assertions[i] = fmt.Sprintf(`{"topic":"T%d","topic_name":%q,"position":%q,"judgement":"changes"}`, i+1, c.Name, prPosition)
	}
	fx := llm.NewFixtures()
	if err = fx.Add(llm.CompletionFixture{Tier: llm.TierAssert, Request: assertworker.RequestFor(prDoc(t, src), candidates, assertBudget()), Response: llm.Response{JSON: []byte(`{"assertions":[` + strings.Join(assertions, ",") + `]}`)}}); err != nil {
		t.Fatal(err)
	}
	cfg := judgmentConfig()
	cfg.AssertionJobBudget = 300 * time.Millisecond
	j, fake := judgmentCapability(t, cfg, llm.JudgmentStep{Delay: time.Second, Response: judgmentFixture(t, "restates")})
	a := boostedAsserter(t, pool, src, cfg, j, fx)
	started := time.Now()
	r := assertDoc(t, a, f.prID, src)
	elapsed := time.Since(started)
	if elapsed > time.Second || r.StancesWritten != 4 || fake.Calls() != 1 {
		t.Fatalf("job = %+v, calls=%d, duration=%s", r, fake.Calls(), elapsed)
	}
	stances, err := l2.New(pool).StancesFrom(t.Context(), f.prID)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range stances {
		if st.Judgement != l2.JudgementChanges {
			t.Fatalf("non-baseline stance: %+v", st)
		}
	}
	records, err := l1.New(pool).JudgmentsFor(t.Context(), judgmentReader(), string(llm.TaskStanceComparison))
	if err != nil || len(records) != 4 {
		t.Fatalf("records = %+v, %v", records, err)
	}
	for _, r := range records {
		if r.State != "committed" || r.FallbackReason != "budget_exhausted" {
			t.Fatalf("budget fallback = %+v", r)
		}
	}
	obs, err := l1.New(pool).ObservationsFor(t.Context(), judgmentReader())
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, o := range obs {
		if o.Inputs.Documents[0] == f.prID || len(o.Inputs.Documents) > 1 && o.Inputs.Documents[1] == f.prID {
			if o.JudgmentAttempts != nil && *o.JudgmentAttempts == 1 && o.JudgmentFallbacks != nil && *o.JudgmentFallbacks >= 4 {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("missing budget observation: %+v", obs)
	}
}

// A durable decision can outlive a process; lookup uses question/input identity,
// rather than applying today's threshold or asking a replacement model.
func TestCommittedStanceDecisionIsReusedWithoutReinterpretation(t *testing.T) {
	pool := scratchPool(t)
	src := newSource(t)
	f := store(t, pool, src)
	assertDoc(t, newAsserter(t, pool, src, loadFixtures(t)), f.issueID, src)
	topics, err := l2.New(pool).Topics(t.Context(), src)
	if err != nil {
		t.Fatal(err)
	}
	history, err := l2.New(pool).StanceHistory(t.Context(), topics[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := l1.New(pool).CaptureJudgmentInputs(t.Context(), l1.JudgmentInputSpec{DocumentID: f.prID, CandidateTopicIDs: []string{topics[0].ID}, ComparedStanceIDs: []string{history[0].ID}, Scope: []string{src}, ACL: prDoc(t, src).ACL})
	if err != nil {
		t.Fatal(err)
	}
	request := assertworker.StanceComparisonRequest(topicName, issuePosition, prPosition, prDoc(t, src).Body.Outcome)
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(raw)
	confidence, attempts := 0.96, int64(1)
	r := l1.JudgmentRecord{ID: "saved-decision", Task: string(llm.TaskStanceComparison), QuestionVersion: "stance-comparison-v1", QuestionHash: hex.EncodeToString(hash[:]), Inputs: inputs, Mode: "accepted", Policy: "old-qualified-policy", ModelVersion: "old-version", Outcome: "answer", Label: "restates", Probabilities: map[string]float64{"changes": 0.03, "restates": 0.96, "abstain": 0.01}, Confidence: &confidence, Attempts: &attempts}
	if err = pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		return l1.CommitJudgments(t.Context(), tx, []l1.JudgmentRecord{r}, nil, l1.JournalCommitOptions{})
	}); err != nil {
		t.Fatal(err)
	}
	cfg := judgmentConfig()
	cfg.Policies = nil // today's configuration is shadow.
	j, fake := judgmentCapability(t, cfg)
	assertDoc(t, boostedAsserter(t, pool, src, cfg, j, loadFixtures(t)), f.prID, src)
	writtenJudgment(t, pool, f.prID, l2.JudgementRestates)
	if fake.Calls() != 0 {
		t.Fatal("committed decision called provider again")
	}
	records, err := l1.New(pool).JudgmentsFor(t.Context(), judgmentReader(), r.Task)
	if err != nil || len(records) != 1 || records[0].ModelVersion != "old-version" || records[0].Policy != "old-qualified-policy" {
		t.Fatalf("reinterpreted committed decision: %+v, %v", records, err)
	}
}

func TestHumanGesturesDetermineStandingAfterJudgment(t *testing.T) {
	for _, choice := range []string{"changes", "restates"} {
		t.Run(choice, func(t *testing.T) {
			pool := scratchPool(t)
			src := newSource(t)
			f := store(t, pool, src)
			assertDoc(t, newAsserter(t, pool, src, loadFixtures(t)), f.issueID, src)
			cfg := judgmentConfig()
			j, fake := judgmentCapability(t, cfg, llm.JudgmentStep{Response: judgmentFixture(t, choice)})
			assertDoc(t, boostedAsserter(t, pool, src, cfg, j, loadFixtures(t)), f.prID, src)
			graph := l2.New(pool)
			topics, err := graph.Topics(t.Context(), src)
			if err != nil {
				t.Fatal(err)
			}
			for i, action := range []l2.GestureAction{l2.GestureDemote, l2.GestureRatify} {
				ev := event(src, connector.KindCommand, fmt.Sprintf("gesture-%d", i), at(10+i), "kpenfound", "u1", "gesture", "gesture")
				stored, err := l0.New(pool).Append(t.Context(), ev)
				if err != nil {
					t.Fatal(err)
				}
				req := l2.GestureRequest{Event: stored.ID, Principal: "kyle", Action: action, Documents: []string{f.prID}}
				if err = pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
					_, _, e := l2.New(tx).ApplyGesture(t.Context(), testRepo(src), src, req)
					return e
				}); err != nil {
					t.Fatal(err)
				}
				assessed, err := graph.Assess(t.Context(), testRepo(src).Authority, judgmentReader(), topics)
				if err != nil || len(assessed) != 1 {
					t.Fatalf("Assess = %+v, %v", assessed, err)
				}
				want := l2.TierContested
				if action == l2.GestureRatify {
					want = l2.TierRatified
				}
				if assessed[0].Standing.Tier != want {
					t.Fatalf("after %s: %s, want %s", action, assessed[0].Standing.Tier, want)
				}
				if fake.Calls() != 1 {
					t.Fatal("gesture or standing called judgment capability")
				}
			}
		})
	}
}

func TestOptionalJournalWaitCannotOutliveTheJobBudget(t *testing.T) {
	pool := scratchPool(t)
	src := newSource(t)
	f := store(t, pool, src)
	assertDoc(t, newAsserter(t, pool, src, loadFixtures(t)), f.issueID, src)
	cfg := judgmentConfig()
	cfg.AssertionJobBudget = 50 * time.Millisecond
	j, fake := judgmentCapability(t, cfg, llm.JudgmentStep{Response: judgmentFixture(t, "restates")})
	a := boostedAsserter(t, pool, src, cfg, j, loadFixtures(t))
	// An effective-input writer holds this compatible baseline lock. Attempt
	// storage takes SHARE, so only the optional journal path has to wait.
	writer, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(context.Background())
	if _, err = writer.Exec(t.Context(), `LOCK TABLE judgment_input_guard IN ROW EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	started := time.Now()
	result, err := a.Assert(ctx, f.prID, src)
	if err != nil || result.StancesWritten != 1 {
		t.Fatalf("baseline while optional journal waits = %+v, %v", result, err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("optional journal wait overran bounded budget and commit timeouts")
	}
	writtenJudgment(t, pool, f.prID, l2.JudgementChanges)
	if fake.Calls() != 0 {
		t.Fatal("provider called after attempt storage exhausted budget")
	}
}

func TestAssertHandlerRecordsKnownQueueDelay(t *testing.T) {
	pool := scratchPool(t)
	src := newSource(t)
	f := store(t, pool, src)
	a := newAsserter(t, pool, src, loadFixtures(t))
	job := queue.Job{TargetID: f.issueID, SerialKey: src, EnqueuedAt: at(0), StartedAt: at(0).Add(3 * time.Second)}
	if err := a.Handle(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	observations, err := l1.New(pool).ObservationsFor(t.Context(), judgmentReader())
	if err != nil || len(observations) != 1 {
		t.Fatalf("observations = %+v, %v", observations, err)
	}
	observation := observations[0]
	if observation.QueueDelayNS == nil || *observation.QueueDelayNS != int64(3*time.Second) || observation.Cohort != "baseline" || observation.HandlerNS == nil || *observation.HandlerNS <= 0 {
		t.Fatalf("stage timing = %+v", observation)
	}
}
