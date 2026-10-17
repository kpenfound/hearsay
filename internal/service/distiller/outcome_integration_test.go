//go:build integration

package distiller_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/principal"
	"github.com/kpenfound/hearsay/internal/service/distiller"
)

func outcomeRegistry(t *testing.T, restatement bool, declined ...bool) llm.Registry {
	t.Helper()
	fx := loadFixtures(t)
	if restatement {
		path := "testdata/outcome-restatements/recorded.json"
		if len(declined) > 0 && declined[0] {
			path = "testdata/outcome-restatement-declined/recorded.json"
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var file llm.FixtureFile
		if err = json.Unmarshal(raw, &file); err != nil {
			t.Fatal(err)
		}
		for _, completion := range file.Completions {
			if err = fx.Add(completion); err != nil {
				t.Fatal(err)
			}
		}
	}
	registry, err := llm.NewFake(testRepo(source).LLM, fx)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func outcomeCapability(t *testing.T, cfg llm.JudgmentConfig, provider llm.JudgmentProvider) llm.Judgments {
	t.Helper()
	cfg.Enabled, cfg.Model = true, "recorded-v1"
	j, err := llm.NewJudgments(cfg, func(llm.JudgmentConfig, string) llm.JudgmentProvider { return provider }, func(string) string { return "fake-only" })
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func boostedDistiller(t *testing.T, pool *pgxpool.Pool, src string, registry llm.Registry, j llm.Judgments, qualified bool) *distiller.Distiller {
	t.Helper()
	cfg := testConfig(src)
	cfg.Repo.Boost.Enabled = true
	if qualified {
		cfg.Repo.Boost.Policies = map[llm.JudgmentTask]llm.AcceptancePolicy{llm.TaskOutcomeClassification: {Name: "test-only", ModelVersion: "recorded-v1", Threshold: 0.9}}
	}
	d, err := distiller.New(pool, registry, cfg, distiller.WithJudgments(j))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func judgmentReader() l1.Reader {
	return l1.Reader{Effective: principal.Effective{Human: "reader", Grant: principal.Grant{Scopes: principal.Scopes{All: true}}}}
}

func assertBaselineOutcome(t *testing.T, pool *pgxpool.Pool, id string) l1.Stored {
	t.Helper()
	doc, err := l1.New(pool).Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Body.OutcomeKind != l1.OutcomeDecided || doc.Body.Outcome != answers[issueID]["outcome"] {
		t.Fatalf("lost baseline outcome: %+v", doc.Body)
	}
	return doc
}

func TestOutcomeShadowIsIdenticalToBaseline(t *testing.T) {
	pool := newScratchPool(t)
	src := newSource(t)
	ingest(t, pool, fixtureEvents(src))
	id := l1.DocID(src, repo+"#12")
	baseline, err := newDistiller(t, pool, src).Distill(t.Context(), id)
	if err != nil || !baseline.Asserting {
		t.Fatalf("baseline=%+v %v", baseline, err)
	}
	before := assertBaselineOutcome(t, pool, id)
	var jobsBefore int
	if err = pool.QueryRow(t.Context(), `SELECT count(*) FROM queue_job`).Scan(&jobsBefore); err != nil {
		t.Fatal(err)
	}
	fake := llm.NewFakeJudgmentProvider(outcomeStep(t, "none"))
	j := outcomeCapability(t, llm.JudgmentConfig{MaxRetries: -1}, fake)
	d := boostedDistiller(t, pool, src, outcomeRegistry(t, false), j, false)
	result, err := d.Distill(t.Context(), id)
	if err != nil || result.Written || result.Asserting {
		t.Fatalf("shadow=%+v %v", result, err)
	}
	after := assertBaselineOutcome(t, pool, id)
	if !reflect.DeepEqual(before, after) {
		t.Fatal("shadow changed the complete L1 row")
	}
	var jobsAfter int
	if err = pool.QueryRow(t.Context(), `SELECT count(*) FROM queue_job`).Scan(&jobsAfter); err != nil {
		t.Fatal(err)
	}
	if jobsAfter != jobsBefore {
		t.Fatalf("assert queue changed: %d -> %d", jobsBefore, jobsAfter)
	}
	records, err := l1.New(pool).JudgmentsFor(t.Context(), judgmentReader(), string(llm.TaskOutcomeClassification))
	if err != nil || len(records) != 1 || records[0].Mode != "shadow" || records[0].Label != "none" {
		t.Fatalf("records=%+v %v", records, err)
	}
	observations, err := l1.New(pool).ObservationsFor(t.Context(), judgmentReader())
	if err != nil || len(observations) != 2 {
		t.Fatalf("observations=%+v %v", observations, err)
	}
	if observations[0].Cohort != "baseline" || observations[1].Cohort != "shadow" {
		t.Fatalf("cohorts=%+v", observations)
	}
	// Even installing a new acceptance policy cannot reinterpret this result.
	restarted := boostedDistiller(t, pool, src, outcomeRegistry(t, true), j, true)
	result, err = restarted.Distill(t.Context(), id)
	if err != nil || result.Written || fake.Calls() != 1 {
		t.Fatalf("restart=%+v %v calls=%d", result, err, fake.Calls())
	}
	assertBaselineOutcome(t, pool, id)
}

func TestQualifiedOutcomeRequiresGenerativeAgreement(t *testing.T) {
	for _, tc := range []struct {
		name, label      string
		restatement      bool
		want             l1.OutcomeKind
		wantText, reason string
		asserts          bool
		declined         bool
	}{
		{"coherent proposal", "proposed", true, l1.OutcomeProposed, "Taking the lock before writing was proposed.", "", true, false},
		{"none still writes L1", "none", true, l1.OutcomeNone, "", "", false, false},
		{"open still writes L1", "open", true, l1.OutcomeOpen, "Whether to take the lock first remains open.", "", false, false},
		{"disagreement without restatement", "proposed", false, l1.OutcomeDecided, answers[issueID]["outcome"].(string), "restatement_failed", true, false},
		{"generator declines classification", "proposed", true, l1.OutcomeDecided, answers[issueID]["outcome"].(string), "restatement_unsupported", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := newScratchPool(t)
			src := newSource(t)
			ingest(t, pool, fixtureEvents(src))
			id := l1.DocID(src, repo+"#12")
			fake := llm.NewFakeJudgmentProvider(outcomeStep(t, tc.label))
			j := outcomeCapability(t, llm.JudgmentConfig{MaxRetries: -1}, fake)
			registry := outcomeRegistry(t, tc.restatement, tc.declined)
			d := boostedDistiller(t, pool, src, registry, j, true)
			result, err := d.Distill(t.Context(), id)
			if err != nil || !result.Written || result.Asserting != tc.asserts {
				t.Fatalf("result=%+v %v", result, err)
			}
			doc, err := l1.New(pool).Get(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if doc.Body.OutcomeKind != tc.want || doc.Body.Outcome != tc.wantText {
				t.Fatalf("incoherent pair: %+v", doc.Body)
			}
			records, err := l1.New(pool).JudgmentsFor(t.Context(), judgmentReader(), string(llm.TaskOutcomeClassification))
			if err != nil || len(records) != 1 || records[0].FallbackReason != tc.reason {
				t.Fatalf("records=%+v %v", records, err)
			}
			// New worker and model configuration reuse the committed decision. No
			// second Jev call or second generative reconciliation is permitted.
			restart := boostedDistiller(t, pool, src, outcomeRegistry(t, false), j, true)
			result, err = restart.Distill(t.Context(), id)
			if err != nil || result.Written || fake.Calls() != 1 {
				t.Fatalf("restart=%+v %v calls=%d", result, err, fake.Calls())
			}
		})
	}
}

func TestOutcomeFailuresKeepBaselineAndRecordReason(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		step         llm.JudgmentStep
		cfg          llm.JudgmentConfig
		warm         bool
	}{
		{name: "error", reason: "unavailable", step: llm.JudgmentStep{Error: errors.New("private provider failure")}},
		{name: "timeout", reason: "timeout", step: llm.JudgmentStep{Delay: time.Second}, cfg: llm.JudgmentConfig{Timeout: time.Millisecond}},
		{name: "rate limit", reason: "rate_limited", step: llm.JudgmentStep{Error: &llm.Error{Status: 429, RetryAfter: time.Hour}}},
		{name: "invalid", reason: "invalid_response", step: llm.JudgmentStep{}},
		{name: "cooldown", reason: "cooldown", step: llm.JudgmentStep{Error: errors.New("unavailable")}, cfg: llm.JudgmentConfig{CooldownFailures: 1}, warm: true},
		{name: "low confidence", reason: "policy_not_met", step: func() llm.JudgmentStep { s := outcomeStep(t, "none"); s.Response.Answers[0].Confidence = 0.5; return s }()},
		{name: "model mismatch", reason: "policy_not_met", step: func() llm.JudgmentStep {
			s := outcomeStep(t, "none")
			s.Response.ModelVersion = "other-version"
			return s
		}()},
		{name: "document budget", reason: "budget_exhausted", step: llm.JudgmentStep{Delay: 10 * time.Second}, cfg: llm.JudgmentConfig{Timeout: 10 * time.Second, TotalTimeout: 10 * time.Second}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := newScratchPool(t)
			src := newSource(t)
			ingest(t, pool, fixtureEvents(src))
			id := l1.DocID(src, repo+"#12")
			fake := llm.NewFakeJudgmentProvider(tc.step)
			tc.cfg.MaxRetries = -1
			j := outcomeCapability(t, tc.cfg, fake)
			if tc.warm {
				j.Judge(t.Context(), llm.JudgmentRequest{State: "warm", Questions: []llm.JudgmentQuestion{{ID: "warm", Question: "Warm the outage state", Kind: llm.JudgmentChoice, Options: []string{"a", "b"}}}})
			}
			d := boostedDistiller(t, pool, src, outcomeRegistry(t, true), j, true)
			started := time.Now()
			result, err := d.Distill(t.Context(), id)
			if err != nil || !result.Written || !result.Asserting {
				t.Fatalf("result=%+v %v", result, err)
			}
			if time.Since(started) > distiller.OutcomeWorkBudget+time.Second {
				t.Fatal("optional work exceeded document budget")
			}
			assertBaselineOutcome(t, pool, id)
			records, err := l1.New(pool).JudgmentsFor(t.Context(), judgmentReader(), string(llm.TaskOutcomeClassification))
			if err != nil || len(records) != 1 || records[0].FallbackReason != tc.reason {
				t.Fatalf("records=%+v %v", records, err)
			}
			observations, err := l1.New(pool).ObservationsFor(t.Context(), judgmentReader())
			if err != nil || len(observations) != 1 || observations[0].JudgmentFallbacks == nil || *observations[0].JudgmentFallbacks != 1 {
				t.Fatalf("observations=%+v %v", observations, err)
			}
		})
	}
}

// afterJudgment injects a race after the fake answered but before the consumer
// commits. It never replaces fake model output or touches a live provider.
type afterJudgment struct {
	llm.JudgmentProvider
	after func()
}

func (p afterJudgment) Judge(ctx context.Context, req llm.JudgmentRequest) (llm.JudgmentResponse, error) {
	r, e := p.JudgmentProvider.Judge(ctx, req)
	p.after()
	return r, e
}

func TestJournalRefusalsWriteBaselineAndNeverReuseAttempt(t *testing.T) {
	for _, kind := range []string{"write failure", "inputs changed", "statement timeout", "observation failure"} {
		t.Run(kind, func(t *testing.T) {
			pool := newScratchPool(t)
			src := newSource(t)
			ingest(t, pool, fixtureEvents(src))
			id := l1.DocID(src, repo+"#12")
			fake := llm.NewFakeJudgmentProvider(outcomeStep(t, "none"))
			hook := func() {
				var sql string
				switch kind {
				case "write failure":
					sql = `CREATE FUNCTION refuse_judgment() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected'; END $$; CREATE TRIGGER refuse BEFORE UPDATE ON judgment_journal FOR EACH ROW EXECUTE FUNCTION refuse_judgment()`
				case "observation failure":
					sql = `CREATE FUNCTION refuse_judgment() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected'; END $$; CREATE TRIGGER refuse BEFORE INSERT ON pipeline_observations FOR EACH ROW EXECUTE FUNCTION refuse_judgment()`
				case "statement timeout":
					sql = `CREATE FUNCTION refuse_judgment() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_sleep(1); RETURN NEW; END $$; CREATE TRIGGER refuse BEFORE UPDATE ON judgment_journal FOR EACH ROW EXECUTE FUNCTION refuse_judgment()`
				case "inputs changed":
					sql = `UPDATE l0_events SET payload=jsonb_set(payload,'{url}','"https://example.test/source-metadata-changed"') WHERE source=$1`
				}
				var err error
				if kind == "inputs changed" {
					_, err = pool.Exec(t.Context(), sql, src)
				} else {
					_, err = pool.Exec(t.Context(), sql)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			j := outcomeCapability(t, llm.JudgmentConfig{MaxRetries: -1}, afterJudgment{fake, hook})
			d := boostedDistiller(t, pool, src, outcomeRegistry(t, true), j, true)
			result, err := d.Distill(t.Context(), id)
			if err != nil || !result.Written || !result.Asserting {
				t.Fatalf("refusal=%+v %v", result, err)
			}
			assertBaselineOutcome(t, pool, id)
			var committed int
			if err = pool.QueryRow(t.Context(), `SELECT count(*) FROM judgment_journal WHERE state='committed'`).Scan(&committed); err != nil || committed != 0 {
				t.Fatalf("committed=%d %v", committed, err)
			}
			if kind != "inputs changed" {
				table := "judgment_journal"
				if kind == "observation failure" {
					table = "pipeline_observations"
				}
				if _, err = pool.Exec(t.Context(), "DROP TRIGGER refuse ON "+table); err != nil {
					t.Fatal(err)
				}
			}
			// Retry has no usable new recording. The interrupted attempt must never
			// be mistaken for a committed classification.
			restarted := boostedDistiller(t, pool, src, outcomeRegistry(t, true), outcomeCapability(t, llm.JudgmentConfig{MaxRetries: -1}, fake), true)
			result, err = restarted.Distill(t.Context(), id)
			if err != nil || fake.Calls() != 2 {
				t.Fatalf("retry=%+v %v calls=%d", result, err, fake.Calls())
			}
			assertBaselineOutcome(t, pool, id)
		})
	}
}

func TestOutcomeJudgmentDoesNotWriteSupersededAnswer(t *testing.T) {
	pool := newScratchPool(t)
	src := newSource(t)
	ingest(t, pool, fixtureEvents(src))
	id := l1.DocID(src, repo+"#12")
	fake := llm.NewFakeJudgmentProvider(outcomeStep(t, "none"))
	provider := afterJudgment{fake, func() {
		ingest(t, pool, []connector.Event{reply(event(src, connector.KindMessage, repo+"#12:late", at(10), who(src, "u2", "samr"), "", "A newer source revision."), repo+"#12")})
	}}
	d := boostedDistiller(t, pool, src, outcomeRegistry(t, true), outcomeCapability(t, llm.JudgmentConfig{MaxRetries: -1}, provider), true)
	result, err := d.Distill(t.Context(), id)
	if err != nil || !result.Superseded || result.Written {
		t.Fatalf("result=%+v %v", result, err)
	}
	if _, err = l1.New(pool).Get(t.Context(), id); !errors.Is(err, l1.ErrNotFound) {
		t.Fatalf("superseded row exists: %v", err)
	}
	var committed int
	if err = pool.QueryRow(t.Context(), `SELECT count(*) FROM judgment_journal WHERE state='committed'`).Scan(&committed); err != nil || committed != 0 {
		t.Fatalf("committed=%d %v", committed, err)
	}
}

func TestOutcomeShadowFirstWriteAndDisabledDependency(t *testing.T) {
	for _, mode := range []string{"shadow", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			baselinePool, boostPool := newScratchPool(t), newScratchPool(t)
			src := newSource(t)
			for _, pool := range []*pgxpool.Pool{baselinePool, boostPool} {
				ingest(t, pool, fixtureEvents(src))
			}
			id := l1.DocID(src, repo+"#12")
			baseline, err := newDistiller(t, baselinePool, src).Distill(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			fake := llm.NewFakeJudgmentProvider(outcomeStep(t, "none"))
			j := outcomeCapability(t, llm.JudgmentConfig{MaxRetries: -1}, fake)
			cfg := testConfig(src)
			cfg.Repo.Boost.Enabled = mode == "shadow"
			d, err := distiller.New(boostPool, outcomeRegistry(t, false), cfg, distiller.WithJudgments(j))
			if err != nil {
				t.Fatal(err)
			}
			result, err := d.Distill(t.Context(), id)
			if err != nil || !reflect.DeepEqual(result, baseline) {
				t.Fatalf("result=%+v baseline=%+v %v", result, baseline, err)
			}
			before, err := l1.New(baselinePool).Get(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			after, err := l1.New(boostPool).Get(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before.Document, after.Document) {
				t.Fatal("boost changed baseline document")
			}
			if (fake.Calls() == 1) != (mode == "shadow") {
				t.Fatalf("mode=%s calls=%d", mode, fake.Calls())
			}
			var baselineJobs, boostJobs int
			for _, q := range []struct {
				pool *pgxpool.Pool
				n    *int
			}{{baselinePool, &baselineJobs}, {boostPool, &boostJobs}} {
				if err = q.pool.QueryRow(t.Context(), `SELECT count(*) FROM queue_job WHERE kind='assert'`).Scan(q.n); err != nil {
					t.Fatal(err)
				}
			}
			if baselineJobs != boostJobs || baselineJobs != 1 {
				t.Fatalf("assert jobs: baseline=%d boost=%d", baselineJobs, boostJobs)
			}
		})
	}
}

func TestOutcomeControlTrafficNeverReachesJudgments(t *testing.T) {
	pool := newScratchPool(t)
	src := newSource(t)
	fake := llm.NewFakeJudgmentProvider(outcomeStep(t, "decided"))
	d := boostedDistiller(t, pool, src, outcomeRegistry(t, false), outcomeCapability(t, llm.JudgmentConfig{MaxRetries: -1}, fake), true)
	for _, kind := range []connector.Kind{connector.KindCommand, "github.reply"} {
		control := event(src, kind, string(kind), at(1), who(src, "u1", "kpenfound"), "", "CONTROL TRAFFIC MUST STAY OUT")
		if kind == "github.reply" {
			control.Payload.BaseKind = connector.KindCommand
		}
		ingest(t, pool, []connector.Event{control})
		result, err := d.Distill(t.Context(), l1.DocID(src, string(kind)))
		if err != nil || !result.Skipped || result.Written {
			t.Fatalf("control=%+v %v", result, err)
		}
	}
	if fake.Calls() != 0 {
		t.Fatalf("control traffic made %d judgment calls", fake.Calls())
	}
	// Control events also stay out of a substantive document's source state.
	events := fixtureEvents(src)
	for i, kind := range []connector.Kind{connector.KindCommand, "github.reply"} {
		control := reply(event(src, kind, "control-child-"+string(rune('0'+i)), at(1), who(src, "u1", "kpenfound"), "", "CONTROL TRAFFIC MUST STAY OUT"), repo+"#12")
		if kind == "github.reply" {
			control.Payload.BaseKind = connector.KindCommand
		}
		events = append(events, control)
	}
	ingest(t, pool, events)
	result, err := d.Distill(t.Context(), l1.DocID(src, repo+"#12"))
	if err != nil || !result.Written {
		t.Fatalf("parent=%+v %v", result, err)
	}
	requests := fake.Requests()
	if len(requests) != 1 {
		t.Fatalf("requests=%d", len(requests))
	}
	if strings.Contains(requests[0].State, "CONTROL TRAFFIC") {
		t.Fatal("judgment received control traffic")
	}
}
