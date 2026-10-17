//go:build integration

package deletion_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/deletion"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/principal"
)

func TestOperatorDeletionRemovesJudgmentsAndObservations(t *testing.T) {
	for _, topology := range []string{"compared position", "merged candidate", "virtual split candidate", "concurrent journal commit"} {
		t.Run(topology, func(t *testing.T) { testJournalDeletion(t, topology) })
	}
}
func testJournalDeletion(t *testing.T, topology string) {
	pool := scratch(t)
	ctx := t.Context()
	event := connector.Event{Source: "chat", NativeID: "secret", Kind: connector.KindMessage, Time: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC), Payload: connector.Payload{Artifact: "secret", Author: &connector.Identity{Source: "chat", Kind: connector.IdentityUser, NativeID: "alice"}, Container: connector.Container{Kind: connector.ContainerChannel, NativeID: "channel"}, Text: "private-source-evidence"}, ACL: connector.ACL{{Kind: connector.ACLPublic}}}
	if _, err := l0.New(pool).Append(ctx, event); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO l1_docs(id,kind,source,source_native_id,l0_refs,created_at,updated_at,last_activity_at,acl,text,raw_text,body,outcome_kind,scope,artifact_class)
     VALUES('l1:chat:secret','chat_thread','chat','secret',ARRAY['evt:chat:secret'],now(),now(),now(),'[{"kind":"public"}]','private-derived-evidence','raw','{}','none',ARRAY['scope'],'chat_thread'),
       ('l1:chat:other','chat_thread','chat','other',ARRAY['evt:chat:other'],now(),now(),now(),'[{"kind":"public"}]','other','raw','{}','none',ARRAY['scope'],'chat_thread')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO l2_topics(id,scope,name,opened_by,acl) VALUES('candidate','scope','question','l1:chat:secret','[{"kind":"public"}]')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO l2_stances(id,topic_id,position,stated_at,evidence,tier,acl) VALUES('previous','candidate','private-position-evidence',now(),ARRAY['l1:chat:secret'],'inferred','[{"kind":"public"}]')`); err != nil {
		t.Fatal(err)
	}
	other := event
	other.NativeID = "other"
	other.Payload.Artifact = "other"
	other.Payload.Text = "other"
	if _, err := l0.New(pool).Append(ctx, other); err != nil {
		t.Fatal(err)
	}
	compared := []string{"previous"}
	if topology == "merged candidate" || topology == "virtual split candidate" {
		if _, err := pool.Exec(ctx, `INSERT INTO l2_topics(id,scope,name,opened_by,acl) VALUES('legacy','scope','legacy','l1:chat:secret','[{"kind":"public"}]');
          UPDATE l2_stances SET topic_id='legacy' WHERE id='previous';
          UPDATE l2_topics SET opened_by='l1:chat:other' WHERE id='candidate'`); err != nil {
			t.Fatal(err)
		}
		compared = nil
		if topology == "merged candidate" {
			if _, err := pool.Exec(ctx, `INSERT INTO l2_topic_operations(kind,scope,principal,topics,stances) VALUES('merge','scope','operator',ARRAY['candidate','legacy'],ARRAY['previous'])`); err != nil {
				t.Fatal(err)
			}
		} else {
			if _, err := pool.Exec(ctx, `DELETE FROM l2_topics WHERE id='candidate'; INSERT INTO l2_topic_operations(kind,scope,principal,topics,stances,name) VALUES('split','scope','operator',ARRAY['legacy','candidate'],ARRAY['previous'],'split')`); err != nil {
				t.Fatal(err)
			}
		}
	}
	store := l1.New(pool)
	private := connector.ACL{{Kind: connector.ACLIdentity, Source: "chat", NativeID: "alice"}}
	inputs, err := store.CaptureJudgmentInputs(ctx, l1.JudgmentInputSpec{DocumentID: "l1:chat:other", CandidateTopicIDs: []string{"candidate"}, ComparedStanceIDs: compared, Scope: []string{"scope"}, ACL: private})
	if err != nil {
		t.Fatal(err)
	}
	record := l1.JudgmentRecord{ID: "committed", Task: "stance_comparison", QuestionVersion: "v1", QuestionHash: "question-hash", Inputs: inputs, ModelVersion: "version", Mode: "accepted", Policy: "policy", Outcome: "answer", Label: "restates"}
	attempt := record
	attempt.ID = "interrupted"
	if _, err = store.BeginJudgment(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	observation := l1.PipelineObservation{ID: "stage", Stage: "assert", Cohort: "accepted", Inputs: inputs}
	if err = l1.CommitJudgments(ctx, tx, []l1.JudgmentRecord{record}, []l1.PipelineObservation{observation}, l1.JournalCommitOptions{}); err != nil {
		t.Fatal(err)
	}
	if topology == "concurrent journal commit" {
		repo := config.Repo{Principals: []principal.Principal{{ID: "operator", Kind: principal.KindHuman}}}
		finished := make(chan error, 1)
		deletionContext, cancelDeletion := context.WithCancel(ctx)
		var running sync.WaitGroup
		running.Add(1)
		defer func() { cancelDeletion(); _ = tx.Rollback(ctx); running.Wait() }()
		go func() {
			defer running.Done()
			_, e := deletion.Apply(deletionContext, pool, repo, deletion.Selector{Event: "evt:chat:secret"}, "remove evidence", "operator")
			finished <- e
		}()
		// Observe the actual waiter, rather than sleeping and assuming that
		// deletion has reached its lock or established its first snapshot.
		deadline := time.NewTimer(3 * time.Second)
		defer deadline.Stop()
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		waiting := false
		for !waiting {
			if err = pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE relation='judgment_input_guard'::regclass AND mode='RowExclusiveLock' AND NOT granted)`).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if !waiting {
				select {
				case <-deadline.C:
					t.Fatal("deletion never waited on the journal guard")
				case <-ticker.C:
				}
			}
		}
		if err = tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err = <-finished; err != nil {
			t.Fatal(err)
		}
		for _, table := range []string{"judgment_journal", "pipeline_observations"} {
			var n int
			if err = pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil || n != 0 {
				t.Fatalf("late-committed %s retained %d rows: %v", table, n, err)
			}
		}
		return
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	outside := l1.Reader{Effective: principal.Effective{Human: "bob", Grant: principal.Grant{Scopes: principal.Scopes{All: true}}}}
	inside := outside
	inside.Effective.Human = "alice"
	inside.Audience = private
	for _, reader := range []struct {
		name string
		r    l1.Reader
		want int
	}{{"inside", inside, 2}, {"outside", outside, 0}, {"unaccountable", l1.Reader{}, 0}} {
		t.Run(reader.name, func(t *testing.T) {
			records, err := store.JudgmentsFor(ctx, reader.r, record.Task)
			if err != nil || len(records) != reader.want {
				t.Fatalf("records = %d, %v", len(records), err)
			}
			obs, err := store.ObservationsFor(ctx, reader.r)
			if err != nil || len(obs) != reader.want/2 {
				t.Fatalf("observations = %d, %v", len(obs), err)
			}
			got, err := store.LookupJudgment(ctx, reader.r, record.Task, record.QuestionVersion, record.QuestionHash, inputs)
			if err != nil || (got != nil) != (reader.want > 0) {
				t.Fatalf("lookup = %+v, %v", got, err)
			}
		})
	}
	var raw string
	if err = pool.QueryRow(ctx, `SELECT jsonb_agg(j)::text FROM judgment_journal j`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-source-evidence", "private-derived-evidence", "private-position-evidence"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("journal retained evidence text %q", secret)
		}
	}
	repo := config.Repo{Principals: []principal.Principal{{ID: "operator", Kind: principal.KindHuman}}}
	if _, err = deletion.Apply(ctx, pool, repo, deletion.Selector{Event: "evt:chat:secret"}, "remove evidence", "operator"); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"judgment_journal", "pipeline_observations"} {
		var n int
		if err = pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s retained %d rows: %v", table, n, err)
		}
	}
	if got, err := store.LookupJudgment(ctx, inside, record.Task, record.QuestionVersion, record.QuestionHash, inputs); err != nil || got != nil {
		t.Fatalf("deleted lookup = %+v, %v", got, err)
	}
	if _, err = store.BeginJudgment(ctx, attempt); !errors.Is(err, l1.JournalInputsChanged) {
		t.Fatalf("deleted attempt recreated = %v", err)
	}
}
