//go:build integration

package l1_test

import (
	"testing"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
)

func TestSourceBuiltJournalSurvivesItsOutputAndInvalidatesEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, mutation string
		changeACL      bool
	}{
		{name: "output write"},
		{name: "source edit", mutation: `UPDATE l0_events SET payload='{"text":"edited"}' WHERE id='evt:test:one'`},
		{name: "source ACL", mutation: `UPDATE l0_events SET acl='[{"kind":"identity","source":"test","native_id":"private"}]' WHERE id='evt:test:one'`},
		{name: "captured ACL", changeACL: true},
		{name: "deletion", mutation: `INSERT INTO l0_deletions(id,operator,reason,selector,events,documents,retraction) VALUES('source-delete','operator','test','{}',ARRAY['evt:test:one'],ARRAY['l1:test:one'],'retraction')`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := journalDB(t)
			spec := l1.JudgmentInputSpec{DocumentID: "l1:test:one", SourceBuilt: true, EventIDs: []string{"evt:test:one"}, Scope: []string{"scope"}, ACL: connector.ACL{{Kind: connector.ACLPublic}}}
			store := l1.New(pool)
			inputs, err := store.CaptureJudgmentInputs(t.Context(), spec)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := inputs.DocumentRevision(); ok {
				t.Fatal("derived L1 output is still an input")
			}
			r := l1.JudgmentRecord{ID: "source-judgment", Task: "outcome_classification", QuestionVersion: "v1", QuestionHash: "source-question", Inputs: inputs, Mode: "accepted", Policy: "test-only", ModelVersion: "recorded-v1", Outcome: "answer", Label: "none"}
			tx, err := pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			if err = l1.CommitJudgments(t.Context(), tx, []l1.JudgmentRecord{r}, nil, l1.JournalCommitOptions{}); err != nil {
				t.Fatal(err)
			}
			journalExec(t, tx, `UPDATE l1_docs SET body='{"summary":"new output","outcome_kind":"none"}',text='new output',distilled_at=now() WHERE id='l1:test:one'`)
			if err = tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			got := lookupJournal(t, pool, r)
			if got == nil || got.Label != "none" {
				t.Fatal("writing L1 invalidated its committed source judgment")
			}
			if tc.mutation != "" {
				journalExec(t, pool, tc.mutation)
			}
			if tc.changeACL {
				spec.ACL = connector.ACL{{Kind: connector.ACLIdentity, Source: "test", NativeID: "changed"}}
			}
			current, err := store.CaptureJudgmentInputs(t.Context(), spec)
			if err != nil {
				t.Fatal(err)
			}
			found, err := store.LookupJudgment(t.Context(), journalReader(), r.Task, r.QuestionVersion, r.QuestionHash, current)
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "output write" {
				if found == nil {
					t.Fatal("committed result was not reusable")
				}
			} else if found != nil {
				t.Fatal("changed or deleted evidence reused a judgment")
			}
		})
	}
}
