//go:build integration

package distiller_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/db/dbtest"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/principal"
	"github.com/kpenfound/hearsay/internal/service/distiller"
)

func jevDistiller(t *testing.T, pool *pgxpool.Pool, src string, j llm.Judgments) *distiller.Distiller {
	t.Helper()
	d, err := distiller.New(pool, newFakeRegistry(t), j, testConfig(src))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func outcomeRecords(t *testing.T, pool *pgxpool.Pool) []l1.JudgmentRecord {
	t.Helper()
	var out []l1.JudgmentRecord
	for _, task := range []llm.JudgmentTask{llm.TaskOutcomeWorkFinished, llm.TaskOutcomeBindingAgreement, llm.TaskOutcomeUnagreedProposal, llm.TaskOutcomeStillOpen} {
		records, err := l1.New(pool).JudgmentsFor(t.Context(), judgmentReader(), string(task))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, records...)
	}
	return out
}

func judgmentReader() l1.Reader {
	return l1.Reader{Effective: principal.Effective{Human: "reader", Grant: principal.Grant{Scopes: principal.Scopes{All: true}}}}
}

func committedJudgments(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM judgment_journal WHERE state='committed'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestJevDecidesTheOutcome(t *testing.T) {
	decided := answers[issueID]["outcome"].(string)
	for _, tc := range []struct {
		label    string
		want     l1.OutcomeKind
		wantText string
		asserts  bool
	}{
		{"resolved", l1.OutcomeResolved, decided, true},
		{"decided", l1.OutcomeDecided, decided, true},
		{"proposed", l1.OutcomeProposed, decided, true},
		{"open", l1.OutcomeOpen, decided, false},
		{"none", l1.OutcomeNone, "", false},
		{"uncertain", l1.OutcomeNone, "", false},
	} {
		t.Run(tc.label, func(t *testing.T) {
			pool := dbtest.Scratch(t, "distiller_none")
			src := dbtest.UniqueID()
			ingest(t, pool, fixtureEvents(src))
			id := l1.DocID(src, repo+"#12")
			jev := fixtureJev(t)
			for _, doc := range documentsIn(t, fixtureEvents(source)) {
				if doc.ID == issueID {
					jev.classify(doc, tc.label)
				}
			}
			result, err := jevDistiller(t, pool, src, jev.judgments()).Distill(t.Context(), id)
			if err != nil || !result.Written || result.Asserting != tc.asserts {
				t.Fatalf("result=%+v %v", result, err)
			}
			doc, err := l1.New(pool).Get(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if doc.Body.OutcomeKind != tc.want || doc.Body.Outcome != tc.wantText {
				t.Fatalf("outcome = %s %q, want %s %q", doc.Body.OutcomeKind, doc.Body.Outcome, tc.want, tc.wantText)
			}
			records := outcomeRecords(t, pool)
			if len(records) != 4 {
				t.Fatalf("records=%+v", records)
			}
			for _, r := range records {
				if r.State != "committed" || r.Mode != "accepted" || r.Policy != "threshold" || r.Outcome != "answer" || r.FallbackReason != "" {
					t.Fatalf("record=%+v", r)
				}
			}
			observations, err := l1.New(pool).ObservationsFor(t.Context(), judgmentReader())
			if err != nil || len(observations) != 1 || observations[0].Cohort != "accepted" {
				t.Fatalf("observations=%+v %v", observations, err)
			}

			calls := jev.Calls()
			mute := newOutcomeJev(t)
			mute.failWith(errors.New("must not be asked"))
			result, err = jevDistiller(t, pool, src, mute.judgments()).Distill(t.Context(), id)
			if err != nil || result.Written || mute.Calls() != 0 || jev.Calls() != calls {
				t.Fatalf("restart=%+v %v calls=%d", result, err, mute.Calls())
			}
		})
	}
}

func TestAFailedJudgmentFailsTheJob(t *testing.T) {
	for _, tc := range []struct {
		name string
		step llm.JudgmentStep
		cfg  llm.JudgmentConfig
		want llm.JudgmentFailure
	}{
		{name: "error", step: llm.JudgmentStep{Error: errors.New("private provider failure")}, want: llm.JudgmentUnavailable},
		{name: "timeout", step: llm.JudgmentStep{Delay: time.Second}, cfg: llm.JudgmentConfig{Timeout: time.Millisecond}, want: llm.JudgmentTimeout},
		{name: "rate limit", step: llm.JudgmentStep{Error: &llm.Error{Status: 429, RetryAfter: time.Hour}}, want: llm.JudgmentRateLimited},
		{name: "invalid", step: llm.JudgmentStep{}, want: llm.JudgmentInvalidResponse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := dbtest.Scratch(t, "distiller_none")
			src := dbtest.UniqueID()
			ingest(t, pool, fixtureEvents(src))
			id := l1.DocID(src, repo+"#12")
			fake := llm.NewFakeJudgmentProvider(tc.step)
			_, err := jevDistiller(t, pool, src, jevOver(t, fake, tc.cfg)).Distill(t.Context(), id)
			var je *llm.JudgmentError
			if !errors.As(err, &je) || je.Failure != tc.want || strings.Contains(err.Error(), "private") {
				t.Fatalf("Distill() = %v, want a %s judgment error", err, tc.want)
			}
			if _, err := l1.New(pool).Get(t.Context(), id); !errors.Is(err, l1.ErrNotFound) {
				t.Fatalf("a document was written: %v", err)
			}
			if n := committedJudgments(t, pool); n != 0 {
				t.Fatalf("committed=%d", n)
			}

			jev := fixtureJev(t)
			result, err := jevDistiller(t, pool, src, jev.judgments()).Distill(t.Context(), id)
			if err != nil || !result.Written || jev.Calls() != 1 {
				t.Fatalf("retry=%+v %v calls=%d", result, err, jev.Calls())
			}
			doc, err := l1.New(pool).Get(t.Context(), id)
			if err != nil || doc.Body.OutcomeKind != l1.OutcomeDecided {
				t.Fatalf("retry wrote %+v %v", doc.Body, err)
			}
		})
	}
}

type afterJudgment struct {
	llm.JudgmentProvider
	after func()
}

func (p afterJudgment) Judge(ctx context.Context, req llm.JudgmentRequest) (llm.JudgmentResponse, error) {
	r, e := p.JudgmentProvider.Judge(ctx, req)
	p.after()
	return r, e
}

func TestAJournalRefusalFailsTheJob(t *testing.T) {
	for _, kind := range []string{"write failure", "inputs changed", "statement timeout", "observation failure"} {
		t.Run(kind, func(t *testing.T) {
			pool := dbtest.Scratch(t, "distiller_none")
			src := dbtest.UniqueID()
			ingest(t, pool, fixtureEvents(src))
			id := l1.DocID(src, repo+"#12")
			jev := fixtureJev(t)
			hook := func() {
				var sql string
				switch kind {
				case "write failure":
					sql = `CREATE FUNCTION refuse_judgment() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected'; END $$; CREATE TRIGGER refuse BEFORE UPDATE ON judgment_journal FOR EACH ROW EXECUTE FUNCTION refuse_judgment()`
				case "observation failure":
					sql = `CREATE FUNCTION refuse_judgment() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected'; END $$; CREATE TRIGGER refuse BEFORE INSERT ON pipeline_observations FOR EACH ROW EXECUTE FUNCTION refuse_judgment()`
				case "statement timeout":

					sql = fmt.Sprintf(`CREATE FUNCTION refuse_judgment() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_sleep(%g); RETURN NEW; END $$; CREATE TRIGGER refuse BEFORE UPDATE ON judgment_journal FOR EACH ROW EXECUTE FUNCTION refuse_judgment()`, l1.DefaultJournalStatementTimeout.Seconds()+1)
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
			_, err := jevDistiller(t, pool, src, jevOver(t, afterJudgment{jev, hook}, llm.JudgmentConfig{})).Distill(t.Context(), id)
			if err == nil {
				t.Fatal("a refused journal did not fail the job")
			}
			if _, err := l1.New(pool).Get(t.Context(), id); !errors.Is(err, l1.ErrNotFound) {
				t.Fatalf("a document was written: %v", err)
			}
			if n := committedJudgments(t, pool); n != 0 {
				t.Fatalf("committed=%d", n)
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

			calls := jev.Calls()
			result, err := jevDistiller(t, pool, src, jev.judgments()).Distill(t.Context(), id)
			if err != nil || !result.Written || jev.Calls() != calls+1 {
				t.Fatalf("retry=%+v %v calls=%d", result, err, jev.Calls())
			}
		})
	}
}

func TestOutcomeJudgmentDoesNotWriteSupersededAnswer(t *testing.T) {
	pool := dbtest.Scratch(t, "distiller_none")
	src := dbtest.UniqueID()
	ingest(t, pool, fixtureEvents(src))
	id := l1.DocID(src, repo+"#12")
	provider := afterJudgment{fixtureJev(t), func() {
		ingest(t, pool, []connector.Event{reply(event(src, connector.KindMessage, repo+"#12:late", at(10), who(src, "u2", "samr"), "", "A newer source revision."), repo+"#12")})
	}}
	result, err := jevDistiller(t, pool, src, jevOver(t, provider, llm.JudgmentConfig{})).Distill(t.Context(), id)
	if err != nil || !result.Superseded || result.Written {
		t.Fatalf("result=%+v %v", result, err)
	}
	if _, err = l1.New(pool).Get(t.Context(), id); !errors.Is(err, l1.ErrNotFound) {
		t.Fatalf("superseded row exists: %v", err)
	}
	if n := committedJudgments(t, pool); n != 0 {
		t.Fatalf("committed=%d", n)
	}
}

func TestOutcomeControlTrafficNeverReachesJudgments(t *testing.T) {
	pool := dbtest.Scratch(t, "distiller_none")
	src := dbtest.UniqueID()
	jev := fixtureJev(t)
	d := jevDistiller(t, pool, src, jev.judgments())
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
	if jev.Calls() != 0 {
		t.Fatalf("control traffic made %d judgment calls", jev.Calls())
	}

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
	requests := jev.Requests()
	if len(requests) != 1 {
		t.Fatalf("requests=%d", len(requests))
	}
	if strings.Contains(requests[0].State, "CONTROL TRAFFIC") {
		t.Fatal("judgment received control traffic")
	}
}
