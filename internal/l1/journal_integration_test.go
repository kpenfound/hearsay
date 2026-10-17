//go:build integration

package l1_test

import (
	"context"
	"errors"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/principal"
)

func journalDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("HEARSAY_DATABASE_URL")
	if url == "" {
		t.Skip("HEARSAY_DATABASE_URL is not set")
	}
	admin, err := db.Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	name := "journal_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err = admin.Exec(t.Context(), "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	admin.Close()
	base, query, hasQuery := strings.Cut(url, "?")
	scratchURL := base[:strings.LastIndex(base, "/")+1] + name
	if hasQuery {
		scratchURL += "?" + query
	}
	t.Cleanup(func() {
		admin, err := db.Open(context.Background(), url)
		if err != nil {
			t.Error(err)
			return
		}
		defer admin.Close()
		_, err = admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
		if err != nil {
			t.Error(err)
		}
	})
	m, err := db.NewMigrator(t.Context(), scratchURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Connect(t.Context(), scratchURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	journalExec(t, pool, `INSERT INTO l0_events(id,source,native_id,kind,artifact,occurred_at,payload,acl)
      VALUES('evt:test:one','test','one','message','one',now(),'{}','[{"kind":"public"}]')`)
	journalExec(t, pool, `INSERT INTO l1_docs(id,kind,source,source_native_id,l0_refs,created_at,updated_at,last_activity_at,acl,text,raw_text,body,outcome_kind,scope,artifact_class)
      VALUES('l1:test:one','chat_thread','test','one',ARRAY['evt:test:one'],now(),now(),now(),'[{"kind":"public"}]','text','raw','{}','none',ARRAY['scope'],'chat_thread')`)
	journalExec(t, pool, `INSERT INTO l2_topics(id,scope,name,opened_by,acl) VALUES('topic','scope','question','l1:test:one','[{"kind":"public"}]'),('other','scope','another','l1:test:one','[{"kind":"public"}]')`)
	journalExec(t, pool, `INSERT INTO l2_stances(id,topic_id,position,stated_at,evidence,tier,acl) VALUES('position','topic','one',now(),ARRAY['l1:test:one'],'inferred','[{"kind":"public"}]')`)
	return pool
}

type journalExecutor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func journalExec(t *testing.T, q journalExecutor, sql string, args ...any) {
	t.Helper()
	if _, err := q.Exec(t.Context(), sql, args...); err != nil {
		t.Fatal(err)
	}
}
func journalReader() l1.Reader {
	return l1.Reader{Effective: principal.Effective{Human: "reader", Grant: principal.Grant{Scopes: principal.Scopes{All: true}}}}
}
func journalSpec() l1.JudgmentInputSpec {
	return l1.JudgmentInputSpec{DocumentID: "l1:test:one", CandidateTopicIDs: []string{"topic"}, ComparedStanceIDs: []string{"position"}, Scope: []string{"scope"}, ACL: connector.ACL{{Kind: connector.ACLPublic}}}
}
func journalRecord(t *testing.T, pool *pgxpool.Pool, id string) l1.JudgmentRecord {
	t.Helper()
	inputs, err := l1.New(pool).CaptureJudgmentInputs(t.Context(), journalSpec())
	if err != nil {
		t.Fatal(err)
	}
	confidence := 0.9
	attempts := int64(1)
	return l1.JudgmentRecord{ID: id, Task: "stance_comparison", QuestionVersion: "v1", QuestionHash: "hash-of-question", Inputs: inputs, Mode: "accepted", Policy: "reviewed-policy", ModelVersion: "pinned-version", Outcome: "answer", Label: "restates", Probabilities: map[string]float64{"restates": 0.9, "changes": 0.1}, Confidence: &confidence, Attempts: &attempts}
}
func journalCommit(t *testing.T, pool *pgxpool.Pool, rs []l1.JudgmentRecord, obs []l1.PipelineObservation) {
	t.Helper()
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err = l1.CommitJudgments(t.Context(), tx, rs, obs, l1.JournalCommitOptions{}); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}
func lookupJournal(t *testing.T, pool *pgxpool.Pool, r l1.JudgmentRecord) *l1.JudgmentRecord {
	t.Helper()
	got, err := l1.New(pool).LookupJudgment(t.Context(), journalReader(), r.Task, r.QuestionVersion, r.QuestionHash, r.Inputs)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestJournalAttemptsAndRestart(t *testing.T) {
	pool := journalDB(t)
	r := journalRecord(t, pool, "attempt")
	if _, err := l1.New(pool).BeginJudgment(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if got := lookupJournal(t, pool, r); got != nil {
		t.Fatal("interrupted attempt was reusable")
	}
	records, err := l1.New(pool).JudgmentsFor(t.Context(), journalReader(), r.Task)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].State != "attempt" || records[0].CommittedAt != nil {
		t.Fatalf("interrupted attempt = %+v", records)
	}
	journalCommit(t, pool, []l1.JudgmentRecord{r}, nil)
	got := lookupJournal(t, pool, r)
	if got == nil || got.State != "committed" || got.CommittedAt == nil || got.Label != "restates" || got.ModelVersion != "pinned-version" {
		t.Fatalf("restart lookup = %+v", got)
	}
	journalCommit(t, pool, []l1.JudgmentRecord{*got}, nil)
	again := lookupJournal(t, pool, r)
	if again == nil || !again.CommittedAt.Equal(*got.CommittedAt) {
		t.Fatal("reuse changed the committed record")
	}
	// Changing model configuration is absent from the lookup key, and a
	// committed record cannot be silently overwritten or reinterpreted.
	changed := r
	changed.Label = "changes"
	changed.ModelVersion = "new-version"
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err = l1.CommitJudgments(t.Context(), tx, []l1.JudgmentRecord{changed}, nil, l1.JournalCommitOptions{}); !errors.Is(err, l1.JournalStorageFailure) {
		t.Fatalf("overwrite = %v", err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got = lookupJournal(t, pool, r); got == nil || got.Label != "restates" {
		t.Fatalf("record changed = %+v", got)
	}
}

func TestJournalInputInvalidation(t *testing.T) {
	for _, tt := range []struct {
		name, sql string
		change    func(*l1.JudgmentRecord)
	}{
		{name: "document revision", sql: `UPDATE l1_docs SET distilled_at=distilled_at+interval '1 second' WHERE id='l1:test:one'`},
		{name: "document content", sql: `UPDATE l1_docs SET text='edited' WHERE id='l1:test:one'`},
		{name: "source revision", sql: `INSERT INTO l0_events(id,source,native_id,kind,artifact,revision_token,occurred_at,payload,acl) VALUES('evt:test:one@v2','test','one@v2','message','one','v2',now(),'{}','[{"kind":"public"}]')`},
		{name: "candidate set", change: func(r *l1.JudgmentRecord) {
			r.Inputs.Spec.CandidateTopicIDs = append(r.Inputs.Spec.CandidateTopicIDs, "other")
		}},
		{name: "ledger state", sql: `INSERT INTO l2_topic_operations(kind,scope,principal,topics,stances) VALUES('merge','scope','reader',ARRAY['topic','other'],'{}')`},
		{name: "position version", sql: `UPDATE l2_stances SET position='edited' WHERE id='position'`},
		{name: "candidate state", sql: `UPDATE l2_topics SET join_keys=ARRAY['new'] WHERE id='topic'`},
		{name: "ACL", sql: `UPDATE l1_docs SET acl='[{"kind":"identity","source":"test","native_id":"private"}]' WHERE id='l1:test:one'`},
		{name: "computed audience", change: func(r *l1.JudgmentRecord) {
			r.Inputs.Spec.ACL = connector.ACL{{Kind: connector.ACLIdentity, Source: "test", NativeID: "private"}}
		}},
		{name: "source tombstone", sql: `INSERT INTO l0_events(id,source,native_id,kind,artifact,target,occurred_at,payload,acl) VALUES('evt:test:gone','test','gone','tombstone','gone','one',now(),'{}','[{"kind":"public"}]')`},
		{name: "document deleted", sql: `DELETE FROM l1_docs WHERE id='l1:test:one'`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pool := journalDB(t)
			r := journalRecord(t, pool, "record")
			journalCommit(t, pool, []l1.JudgmentRecord{r}, nil)
			if lookupJournal(t, pool, r) == nil {
				t.Fatal("identical inputs missed")
			}
			if tt.sql != "" {
				journalExec(t, pool, tt.sql)
			}
			if tt.change != nil {
				tt.change(&r)
			}
			if got := lookupJournal(t, pool, r); got != nil {
				t.Fatalf("stale result reused: %+v", got)
			}
		})
	}
}

func TestJournalRefusalsPreserveBaselineTransaction(t *testing.T) {
	for _, name := range []string{"write failure", "statement timeout", "lock timeout", "inputs changed", "observation failure", "repeatable read"} {
		t.Run(name, func(t *testing.T) {
			pool := journalDB(t)
			first := journalRecord(t, pool, "first")
			second := journalRecord(t, pool, "second")
			// An interrupted attempt survives a failed optional batch as an attempt.
			if _, err := l1.New(pool).BeginJudgment(t.Context(), first); err != nil {
				t.Fatal(err)
			}
			want := l1.JournalStorageFailure
			opts := l1.JournalCommitOptions{StatementTimeout: 200 * time.Millisecond, LockTimeout: 25 * time.Millisecond}
			var holder pgx.Tx
			switch name {
			case "write failure":
				journalExec(t, pool, `CREATE FUNCTION fail_journal() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.id='second' THEN RAISE EXCEPTION 'private diagnostic'; END IF; RETURN NEW; END $$`)
				journalExec(t, pool, `CREATE TRIGGER fail_journal BEFORE INSERT ON judgment_journal FOR EACH ROW EXECUTE FUNCTION fail_journal()`)
			case "statement timeout":
				want = l1.JournalTimeout
				opts.StatementTimeout = 20 * time.Millisecond
				journalExec(t, pool, `CREATE FUNCTION slow_journal() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.id='second' THEN PERFORM pg_sleep(1); END IF; RETURN NEW; END $$`)
				journalExec(t, pool, `CREATE TRIGGER slow_journal BEFORE INSERT ON judgment_journal FOR EACH ROW EXECUTE FUNCTION slow_journal()`)
			case "lock timeout":
				want = l1.JournalTimeout
				var err error
				holder, err = pool.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer holder.Rollback(t.Context())
				journalExec(t, holder, `LOCK TABLE judgment_journal IN ACCESS EXCLUSIVE MODE`)
			case "inputs changed":
				want = l1.JournalInputsChanged
				journalExec(t, pool, `UPDATE l1_docs SET text='changed' WHERE id='l1:test:one'`)
			case "repeatable read":
				want = l1.JournalInputsChanged
			}
			txOpts := pgx.TxOptions{}
			if name == "repeatable read" {
				txOpts.IsoLevel = pgx.RepeatableRead
			}
			tx, err := pool.BeginTx(t.Context(), txOpts)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			journalExec(t, tx, `INSERT INTO l2_asserted(doc_id,distilled_at) VALUES('baseline-before',now())`)
			observations := []l1.PipelineObservation(nil)
			if name == "observation failure" {
				observations = []l1.PipelineObservation{{ID: "bad", Stage: "assert", Cohort: "invalid", Inputs: first.Inputs}}
			}
			err = l1.CommitJudgments(t.Context(), tx, []l1.JudgmentRecord{first, second}, observations, opts)
			if !errors.Is(err, want) {
				t.Fatalf("refusal = %v, want %s", err, want)
			}
			if strings.Contains(err.Error(), "private diagnostic") {
				t.Fatal("storage error exposed diagnostic")
			}
			if holder != nil {
				if err = holder.Rollback(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			journalExec(t, tx, `INSERT INTO l2_asserted(doc_id,distilled_at) VALUES('baseline-after',now())`)
			// Optional server timeout settings never leak into baseline writes.
			journalExec(t, tx, `SELECT pg_sleep(0.03)`)
			var n int
			if err = tx.QueryRow(t.Context(), `SELECT count(*) FROM l2_asserted`).Scan(&n); err != nil || n != 2 {
				t.Fatalf("baseline writes = %d, %v", n, err)
			}
			if err = tx.Commit(t.Context()); err != nil {
				t.Fatalf("baseline commit: %v", err)
			}
			if err = pool.QueryRow(t.Context(), `SELECT count(*) FROM l2_asserted`).Scan(&n); err != nil || n != 2 {
				t.Fatalf("durable baseline writes = %d, %v", n, err)
			}
			if err = pool.QueryRow(t.Context(), `SELECT count(*) FROM judgment_journal WHERE state='committed'`).Scan(&n); err != nil || n != 0 {
				t.Fatalf("partial batch = %d, %v", n, err)
			}
			var state string
			if err = pool.QueryRow(t.Context(), `SELECT state FROM judgment_journal WHERE id='first'`).Scan(&state); err != nil || state != "attempt" {
				t.Fatalf("attempt lost = %q, %v", state, err)
			}
		})
	}
}

func TestJournalCommitClosesConcurrentChangeGap(t *testing.T) {
	for _, table := range []string{"l0_events", "l1_docs", "l2_topics", "l2_stances", "l2_topic_operations", "l0_deletions"} {
		t.Run(table, func(t *testing.T) {
			pool := journalDB(t)
			r := journalRecord(t, pool, "record")
			tx, err := pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			if err = l1.CommitJudgments(t.Context(), tx, []l1.JudgmentRecord{r}, nil, l1.JournalCommitOptions{}); err != nil {
				t.Fatal(err)
			}
			writer, err := pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Rollback(t.Context())
			journalExec(t, writer, `SET LOCAL lock_timeout='25ms'`)
			// Even a no-op DELETE invokes the statement guard before it can mutate
			// effective inputs, proving no concurrent change slips past validation.
			if _, err = writer.Exec(t.Context(), "DELETE FROM "+table+" WHERE false"); err == nil {
				t.Fatal("input writer passed the commit guard")
			}
			if err = writer.Rollback(t.Context()); err != nil {
				t.Fatal(err)
			}
			// The guard allows this caller's own baseline/accepted consumer writes.
			journalExec(t, tx, `UPDATE l1_docs SET distilled_at=distilled_at WHERE id='l1:test:one'`)
			if err = tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			journalExec(t, pool, "DELETE FROM "+table+" WHERE false")
		})
	}
}

func TestPipelineAccountingUnknownAndZero(t *testing.T) {
	pool := journalDB(t)
	r := journalRecord(t, pool, "record")
	zero := int64(0)
	zeroCost := 0.0
	huge := int64(math.MaxInt64)
	one := int64(1)
	overflow := l1.AddPipelineCount(&huge, &one)
	usage := llm.AddJudgmentUsage(llm.JudgmentUsage{InputTokens: &huge, CostUSD: ptrJournal(math.MaxFloat64)}, llm.JudgmentUsage{InputTokens: &one, CostUSD: ptrJournal(math.MaxFloat64)})
	obs := []l1.PipelineObservation{
		{ID: "unknown", Stage: "assert", Cohort: "baseline", Inputs: r.Inputs},
		{ID: "zero", Stage: "assert", Cohort: "shadow", Inputs: r.Inputs, HandlerNS: &zero, QueueDelayNS: &zero, GenerativeCalls: &zero, GenerativeUsage: llm.JudgmentUsage{InputTokens: &zero, OutputTokens: &zero, CostUSD: &zeroCost}, JudgmentAttempts: &zero, JudgmentFallbacks: &zero, CandidateCount: &zero},
		{ID: "overflow", Stage: "assert", Cohort: "accepted", Inputs: r.Inputs, GenerativeCalls: overflow, JudgmentUsage: usage},
	}
	r.Usage = llm.JudgmentUsage{InputTokens: &zero, CostUSD: &zeroCost}
	journalCommit(t, pool, []l1.JudgmentRecord{r}, obs)
	got, err := l1.New(pool).ObservationsFor(t.Context(), journalReader())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("observations = %+v", got)
	}
	for _, o := range got {
		switch o.ID {
		case "unknown":
			if o.HandlerNS != nil || o.GenerativeCalls != nil || o.GenerativeUsage.CostUSD != nil {
				t.Fatal("unknown became zero")
			}
		case "zero":
			if o.HandlerNS == nil || *o.HandlerNS != 0 || o.GenerativeUsage.CostUSD == nil || *o.GenerativeUsage.CostUSD != 0 {
				t.Fatal("reported zero became unknown")
			}
		case "overflow":
			if o.GenerativeCalls != nil || o.JudgmentUsage.InputTokens != nil || o.JudgmentUsage.CostUSD != nil {
				t.Fatal("overflow became known")
			}
		}
	}
	record := lookupJournal(t, pool, r)
	if record == nil || record.Usage.InputTokens == nil || *record.Usage.InputTokens != 0 || record.Usage.OutputTokens != nil || record.Usage.CostUSD == nil {
		t.Fatalf("record accounting = %+v", record)
	}
}
func ptrJournal[T any](v T) *T { return &v }

func TestJournalGuardPreservesConcurrentSourceWrites(t *testing.T) {
	pool := journalDB(t)
	first, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Rollback(t.Context())
	second, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Rollback(t.Context())
	journalExec(t, first, `INSERT INTO l0_events(id,source,native_id,kind,artifact,revision_token,occurred_at,payload,acl) VALUES('evt:test:one@late','test','one@late','message','one','late',now(),'{}','[{"kind":"public"}]')`)
	journalExec(t, second, `SET LOCAL lock_timeout='100ms'`)
	journalExec(t, second, `INSERT INTO l0_events(id,source,native_id,kind,artifact,occurred_at,payload,acl) VALUES('evt:test:second','test','second','message','second',now(),'{}','[{"kind":"public"}]')`)
	if err = second.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	during := journalRecord(t, pool, "during")
	if err = first.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err = l1.CommitJudgments(t.Context(), tx, []l1.JudgmentRecord{during}, nil, l1.JournalCommitOptions{}); !errors.Is(err, l1.JournalInputsChanged) {
		t.Fatalf("late commit was not invalidated: %v", err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestJournalHistoricalReadsUseCurrentSourceAccess(t *testing.T) {
	for _, tt := range []struct{ name, sql string }{
		{"ACL resync before L1 rebuild", `INSERT INTO l0_events(id,source,native_id,kind,artifact,revision_token,occurred_at,payload,acl) VALUES('evt:test:one@private','test','one@private','message','one','private',now(),'{}','[{"kind":"identity","source":"test","native_id":"private"}]')`},
		{"source tombstone before L1 rebuild", `INSERT INTO l0_events(id,source,native_id,kind,artifact,target,occurred_at,payload,acl) VALUES('evt:test:gone','test','gone','tombstone','gone','one',now(),'{}','[{"kind":"public"}]')`},
		{"document access narrowed", `UPDATE l1_docs SET acl='[{"kind":"identity","source":"test","native_id":"private"}]' WHERE id='l1:test:one'`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pool := journalDB(t)
			r := journalRecord(t, pool, "record")
			journalCommit(t, pool, []l1.JudgmentRecord{r}, []l1.PipelineObservation{{ID: "stage", Stage: "assert", Cohort: "accepted", Inputs: r.Inputs}})
			journalExec(t, pool, tt.sql)
			records, err := l1.New(pool).JudgmentsFor(t.Context(), journalReader(), "")
			if err != nil || len(records) != 0 {
				t.Fatalf("hidden records = %d, %v", len(records), err)
			}
			observations, err := l1.New(pool).ObservationsFor(t.Context(), journalReader())
			if err != nil || len(observations) != 0 {
				t.Fatalf("hidden observations = %d, %v", len(observations), err)
			}
		})
	}
}

func TestJournalAttemptBeforeFirstDocument(t *testing.T) {
	pool := journalDB(t)
	spec := journalSpec()
	spec.DocumentID = "l1:test:not-yet-written"
	spec.EventIDs = []string{"evt:test:one"}
	spec.CandidateTopicIDs = nil
	spec.ComparedStanceIDs = nil
	inputs, err := l1.New(pool).CaptureJudgmentInputs(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := inputs.DocumentRevision(); exists {
		t.Fatal("unwritten document had a revision")
	}
	r := l1.JudgmentRecord{Task: "outcome_classification", QuestionVersion: "v1", QuestionHash: "hash-of-question", Mode: "shadow", Inputs: inputs}
	id, err := l1.New(pool).BeginJudgment(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	records, err := l1.New(pool).JudgmentsFor(t.Context(), journalReader(), r.Task)
	if err != nil || len(records) != 1 || records[0].ID != id || records[0].State != "attempt" {
		t.Fatalf("pending attempt = %+v, %v", records, err)
	}
}

func TestJournalOutcomeKinds(t *testing.T) {
	pool := journalDB(t)
	for _, tt := range []struct {
		outcome, mode, label, reason string
		attempts                     int64
	}{
		{"answer", "accepted", "restates", "", 1},
		{"abstain", "shadow", "abstain", "", 1},
		{"fallback", "shadow", "", "cooldown", 0},
	} {
		t.Run(tt.outcome, func(t *testing.T) {
			r := journalRecord(t, pool, tt.outcome)
			r.QuestionHash = tt.outcome
			r.Mode = tt.mode
			r.Outcome = tt.outcome
			r.Label = tt.label
			r.FallbackReason = tt.reason
			r.Attempts = &tt.attempts
			if tt.mode == "shadow" {
				r.Policy = ""
			}
			if tt.outcome == "fallback" {
				r.ModelVersion = ""
				r.Confidence = nil
				r.Probabilities = nil
			}
			journalCommit(t, pool, []l1.JudgmentRecord{r}, nil)
			got := lookupJournal(t, pool, r)
			if got == nil || got.Outcome != tt.outcome || got.Mode != tt.mode || got.Label != tt.label || got.FallbackReason != tt.reason || got.Attempts == nil || *got.Attempts != tt.attempts {
				t.Fatalf("outcome = %+v", got)
			}
		})
	}
}

// The aggregate-only export preserves nullable counts and costs through the
// same persistence path workers use; it does not expose evidence identities.
func TestPipelineAccountingExport(t *testing.T) {
	pool := journalDB(t)
	record := journalRecord(t, pool, "accounting-input")
	zero, one, three := int64(0), int64(1), int64(3)
	cost := .5
	observation := l1.PipelineObservation{ID: "accounting", Stage: "assert", Cohort: "accepted", Inputs: record.Inputs, HandlerNS: &one, QueueDelayNS: nil, GenerativeCalls: &one, GenerativeUsage: llm.JudgmentUsage{InputTokens: &three, OutputTokens: &one, CostUSD: &cost}, JudgmentRequests: &three, JudgmentAttempts: &one, JudgmentFallbacks: &three, JudgmentUsage: llm.JudgmentUsage{InputTokens: &zero, OutputTokens: &zero}, CandidateCount: &three}
	since := time.Now().Add(-time.Minute)
	journalCommit(t, pool, nil, []l1.PipelineObservation{observation})
	store := l1.New(pool)
	rows, err := store.PipelineAccounting(t.Context(), since, time.Now().Add(time.Minute), "scope")
	if err != nil || len(rows) != 1 {
		t.Fatalf("export=%+v err=%v", rows, err)
	}
	got := rows[0]
	if got.JudgmentRequests == nil || *got.JudgmentRequests != 3 || got.JudgmentAttempts == nil || *got.JudgmentAttempts != 1 || got.JudgmentFallbacks == nil || *got.JudgmentFallbacks != 3 || got.JudgmentUsage.CostUSD != nil || got.QueueDelayNS != nil || got.GenerativeUsage.CostUSD == nil || *got.GenerativeUsage.CostUSD != .5 {
		t.Fatalf("nullable export changed: %+v", got)
	}
	if got.ID != "" || len(got.Inputs.Documents) != 0 || len(got.Inputs.Events) != 0 || len(got.Inputs.Spec.ACL) != 0 {
		t.Fatal("aggregate accounting returned evidence identities")
	}
	readable, err := store.ObservationsFor(t.Context(), journalReader())
	if err != nil || len(readable) != 1 || readable[0].JudgmentRequests == nil || *readable[0].JudgmentRequests != 3 {
		t.Fatalf("reader export=%+v err=%v", readable, err)
	}
	for _, filter := range []struct {
		since, until time.Time
		scope        string
	}{{since, time.Now().Add(time.Minute), "elsewhere"}, {time.Now().Add(time.Minute), time.Now().Add(2 * time.Minute), "scope"}, {since, since.Add(time.Second), "scope"}} {
		rows, err := store.PipelineAccounting(t.Context(), filter.since, filter.until, filter.scope)
		if err != nil || len(rows) != 0 {
			t.Fatalf("window/scope filter returned %+v, %v", rows, err)
		}
	}
}
