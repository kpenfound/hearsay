//go:build integration

package l1_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
)

// The root can be a native thread or a message heading its replies. Both
// distiller reads use the same source/conversation membership predicate.
func scopedRecord(t *testing.T, pool *pgxpool.Pool, shape string) l1.JudgmentRecord {
	t.Helper()
	if shape == "stored" {
		return journalRecord(t, pool, "stored")
	}
	spec := l1.JudgmentInputSpec{DocumentID: "l1:test:one", SourceBuilt: true,
		EventIDs: []string{"evt:test:one"}, Scope: []string{"scope"}, ACL: connector.ACL{{Kind: connector.ACLPublic}}}
	if shape == "window" {
		spec.DocumentID = "l1:test:chat:channel:1767225600"
		journalExec(t, pool, `UPDATE l0_events SET occurred_at='2026-01-01 00:05:00Z',
      payload='{"container":{"kind":"channel","native_id":"channel"}}' WHERE id='evt:test:one'`)
	}
	if shape == "derived" {
		spec.DocumentID = "l1:test:wiki-section:3:one:intro"
	}
	in, err := l1.New(pool).CaptureJudgmentInputs(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	return l1.JudgmentRecord{ID: "scoped", Task: "outcome_classification", QuestionVersion: "v1", QuestionHash: "question",
		Inputs: in, Mode: "shadow", ModelVersion: "recorded", Outcome: "answer", Label: "none"}
}

const newThreadMember = `INSERT INTO l0_events(id,source,native_id,kind,artifact,occurred_at,payload,acl)
 VALUES('evt:test:new','test','new','message','new','2026-01-01 00:10:00Z','{"thread":"one"}','[{"kind":"public"}]')`
const newWindowMember = `INSERT INTO l0_events(id,source,native_id,kind,artifact,occurred_at,payload,acl)
 VALUES('evt:test:new','test','new','message','new','2026-01-01 00:10:00Z','{"container":{"kind":"channel","native_id":"channel"}}','[{"kind":"public"}]')`

func TestJournalScopedUnrelatedChangesSurviveCommitAndRestart(t *testing.T) {
	for _, shape := range []string{"thread", "window", "stored"} {
		t.Run(shape, func(t *testing.T) {
			pool := journalDB(t)
			r := scopedRecord(t, pool, shape)
			// Other artifact/thread, container, adjacent half-open window, and source.
			journalExec(t, pool, `INSERT INTO l0_events(id,source,native_id,kind,artifact,occurred_at,payload,acl) VALUES
    ('evt:test:unrelated','test','unrelated','message','unrelated','2026-01-01 00:10:00Z','{"thread":"another"}','[{"kind":"public"}]'),
    ('evt:test:container','test','container','message','container','2026-01-01 00:10:00Z','{"container":{"kind":"channel","native_id":"other"}}','[{"kind":"public"}]'),
    ('evt:test:adjacent','test','adjacent','message','adjacent','2026-01-01 00:30:00Z','{"container":{"kind":"channel","native_id":"channel"}}','[{"kind":"public"}]'),
    ('evt:elsewhere:one','elsewhere','one','message','one','2026-01-01 00:10:00Z','{"thread":"one","container":{"kind":"channel","native_id":"channel"}}','[{"kind":"public"}]')`)
			journalExec(t, pool, `INSERT INTO l2_topic_operations(kind,scope,principal,topics,stances)
    VALUES('merge','elsewhere','reader',ARRAY['unrelated-a','unrelated-b'],'{}')`)
			journalCommit(t, pool, []l1.JudgmentRecord{r}, nil)
			// Recapture through a new store, as after restart; exercise both current
			// identity hashing and Lookup's independent one-snapshot revalidation.
			in, err := l1.New(pool).CaptureJudgmentInputs(t.Context(), r.Inputs.Spec)
			if err != nil {
				t.Fatal(err)
			}
			if in.Hash != r.Inputs.Hash {
				t.Fatal("unrelated activity changed input identity")
			}
			r.Inputs = in
			if lookupJournal(t, pool, r) == nil {
				t.Fatal("unrelated writes stopped restart reuse")
			}
		})
	}
}

func TestJournalMembershipInvalidatesCommitAndRestart(t *testing.T) {
	for _, tc := range []struct{ shape, sql string }{
		{"thread", newThreadMember},
		{"stored", newThreadMember},
		{"thread", `INSERT INTO l0_events(id,source,native_id,kind,artifact,occurred_at,payload,acl)
    VALUES('evt:test:new','test','new','message','new',now(),'{"parent":"one"}','[{"kind":"public"}]')`},
		{"window", newWindowMember},
	} {
		t.Run(tc.shape, func(t *testing.T) {
			pool := journalDB(t)
			r := scopedRecord(t, pool, tc.shape)
			journalCommit(t, pool, []l1.JudgmentRecord{r}, nil)
			journalExec(t, pool, tc.sql)
			tx, err := pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			if err = l1.CommitJudgments(t.Context(), tx, []l1.JudgmentRecord{r}, nil, l1.JournalCommitOptions{}); !errors.Is(err, l1.JournalInputsChanged) {
				t.Fatalf("new member commit = %v", err)
			}
			if err = tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			if lookupJournal(t, pool, r) != nil {
				t.Fatal("old snapshot reused after new member")
			}
			in, err := l1.New(pool).CaptureJudgmentInputs(t.Context(), r.Inputs.Spec)
			if err != nil {
				t.Fatal(err)
			}
			r.Inputs = in
			if lookupJournal(t, pool, r) != nil {
				t.Fatal("restart reused after new member")
			}
		})
	}
}

func TestJournalMembershipWritersCannotPassValidationGuard(t *testing.T) {
	for _, tc := range []struct{ shape, sql string }{{"thread", newThreadMember}, {"window", newWindowMember}} {
		t.Run(tc.shape, func(t *testing.T) {
			pool := journalDB(t)
			r := scopedRecord(t, pool, tc.shape)
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
			if _, err = writer.Exec(t.Context(), tc.sql); err == nil {
				t.Fatal("new member passed held validation guard")
			}
			if err = writer.Rollback(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			journalExec(t, pool, tc.sql)
			if lookupJournal(t, pool, r) != nil {
				t.Fatal("member committed after guard did not invalidate reuse")
			}
		})
	}
}

func TestJournalUnknownSourceBuiltMembershipFallsBackToSource(t *testing.T) {
	pool := journalDB(t)
	r := scopedRecord(t, pool, "derived")
	journalCommit(t, pool, []l1.JudgmentRecord{r}, nil)
	journalExec(t, pool, newThreadMember)
	if lookupJournal(t, pool, r) != nil {
		t.Fatal("derived membership lost conservative source fallback")
	}
}

// A member omitted from the evidence (for example by the ACL fold) still
// belongs to the conversation's read set. Test changes under that unseen id,
// including tombstones with no membership payload and revisions moving away.
func TestJournalMembershipChangesCoverUncapturedMembers(t *testing.T) {
	for _, shape := range []string{"thread", "window"} {
		for _, mutation := range []string{"edit", "ACL", "tombstone", "delete", "move revision", "move update"} {
			t.Run(shape+"/"+mutation, func(t *testing.T) {
				pool := journalDB(t)
				_ = scopedRecord(t, pool, shape)
				memberSQL := newThreadMember
				if shape == "window" {
					memberSQL = newWindowMember
				}
				journalExec(t, pool, memberSQL)
				r := scopedRecord(t, pool, shape)
				journalCommit(t, pool, []l1.JudgmentRecord{r}, nil)
				switch mutation {
				case "edit":
					journalExec(t, pool, `UPDATE l0_events SET payload=payload || '{"text":"changed"}' WHERE artifact='new'`)
				case "ACL":
					journalExec(t, pool, `UPDATE l0_events SET acl='[{"kind":"identity","source":"test","native_id":"private"}]' WHERE artifact='new'`)
				case "tombstone":
					journalExec(t, pool, `INSERT INTO l0_events(id,source,native_id,kind,artifact,target,occurred_at,payload,acl)
      VALUES('evt:test:gone','test','gone','tombstone','gone','new',now(),'{}','[{"kind":"public"}]')`)
				case "delete":
					journalExec(t, pool, `DELETE FROM l0_events WHERE artifact='new'`)
				case "move revision":
					journalExec(t, pool, `INSERT INTO l0_events(id,source,native_id,kind,artifact,revision_token,occurred_at,payload,acl)
      VALUES('evt:test:new@v2','test','new@v2','message','new','v2','2026-01-01 00:30:00Z',
       '{"thread":"other","container":{"kind":"channel","native_id":"elsewhere"}}','[{"kind":"public"}]')`)
				case "move update":
					journalExec(t, pool, `UPDATE l0_events SET occurred_at='2026-01-01 00:30:00Z',
       payload='{"thread":"other","container":{"kind":"channel","native_id":"elsewhere"}}' WHERE artifact='new'`)
				}
				tx, err := pool.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(t.Context())
				if err = l1.CommitJudgments(t.Context(), tx, []l1.JudgmentRecord{r}, nil, l1.JournalCommitOptions{}); !errors.Is(err, l1.JournalInputsChanged) {
					t.Fatalf("member mutation commit=%v", err)
				}
				if err = tx.Commit(t.Context()); err != nil {
					t.Fatal(err)
				}
				if lookupJournal(t, pool, r) != nil {
					t.Fatal("member mutation reused stale result")
				}
			})
		}
	}
}

func TestJournalLedgerConnectedClosureIncludesUndoAndVirtualSplit(t *testing.T) {
	pool := journalDB(t)
	journalExec(t, pool, `INSERT INTO l2_topic_operations(kind,scope,principal,topics,stances,name)
   VALUES('split','scope','reader',ARRAY['topic','virtual'],ARRAY['position'],'Split')`)
	journalExec(t, pool, `INSERT INTO l2_topic_operations(kind,scope,principal,topics,stances,undoes)
   SELECT 'undo',scope,principal,topics,stances,id FROM l2_topic_operations WHERE kind='split'`)
	r := journalRecord(t, pool, "connected")
	journalCommit(t, pool, []l1.JudgmentRecord{r}, nil)
	journalExec(t, pool, `INSERT INTO l2_topic_operations(kind,scope,principal,topics,stances)
   VALUES('merge','scope','reader',ARRAY['virtual','unseen'],'{}')`)
	if lookupJournal(t, pool, r) != nil {
		t.Fatal("operation through undone virtual split reused stale ledger")
	}
}

func TestJournalLateMembershipCommitIsVisible(t *testing.T) {
	for _, tc := range []struct{ shape, sql string }{{"thread", newThreadMember}, {"window", newWindowMember}} {
		t.Run(tc.shape, func(t *testing.T) {
			pool := journalDB(t)
			_ = scopedRecord(t, pool, tc.shape)
			writer, err := pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Rollback(t.Context())
			journalExec(t, writer, tc.sql)
			r := scopedRecord(t, pool, tc.shape)
			if err = writer.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			tx, err := pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			if err = l1.CommitJudgments(t.Context(), tx, []l1.JudgmentRecord{r}, nil, l1.JournalCommitOptions{}); !errors.Is(err, l1.JournalInputsChanged) {
				t.Fatalf("late membership commit=%v", err)
			}
		})
	}
}

// The mutation trigger cannot see the new member until its writer commits.
// The member is private and deliberately absent from EventIDs, so artifact
// markers for captured evidence cannot mask missing membership invalidation.
func TestJournalOverlappingUncapturedMemberMutation(t *testing.T) {
	for _, shape := range []string{"thread", "window"} {
		for _, mutation := range []string{"tombstone", "move revision"} {
			t.Run(shape+"/"+mutation, func(t *testing.T) {
				pool := journalDB(t)
				_ = scopedRecord(t, pool, shape)
				member, err := pool.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer member.Rollback(t.Context())
				memberSQL := newThreadMember
				if shape == "window" {
					memberSQL = newWindowMember
				}
				journalExec(t, member, memberSQL)
				journalExec(t, member, `UPDATE l0_events SET acl='[{"kind":"identity","source":"test","native_id":"private"}]' WHERE artifact='new'`)
				writer, err := pool.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer writer.Rollback(t.Context())
				// Bound the test if a future change serializes L0 writers instead.
				journalExec(t, writer, `SET LOCAL lock_timeout='1s'`)
				if mutation == "tombstone" {
					journalExec(t, writer, `INSERT INTO l0_events(id,source,native_id,kind,artifact,target,occurred_at,payload,acl)
      VALUES('evt:test:gone','test','gone','tombstone','gone','new',now(),'{}','[{"kind":"public"}]')`)
				} else {
					journalExec(t, writer, `INSERT INTO l0_events(id,source,native_id,kind,artifact,revision_token,occurred_at,payload,acl)
      VALUES('evt:test:new@v2','test','new@v2','message','new','v2','2026-01-01 00:30:00Z','{}','[{"kind":"public"}]')`)
				}
				if err = member.Commit(t.Context()); err != nil {
					t.Fatal(err)
				}
				r := scopedRecord(t, pool, shape)
				if slices.Contains(r.Inputs.Events, "evt:test:new") || slices.Contains(r.Inputs.Spec.EventIDs, "evt:test:new") {
					t.Fatal("private member was included in captured evidence")
				}
				if err = writer.Commit(t.Context()); err != nil {
					t.Fatal(err)
				}
				tx, err := pool.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(t.Context())
				if err = l1.CommitJudgments(t.Context(), tx, []l1.JudgmentRecord{r}, nil, l1.JournalCommitOptions{}); !errors.Is(err, l1.JournalInputsChanged) {
					t.Errorf("overlapping member mutation commit = %v; want inputs_changed", err)
				}
				// Commit even on an unexpected acceptance, so the lookup checks
				// below also detect a wrongly committed reusable result.
				if err = tx.Commit(t.Context()); err != nil {
					t.Fatal(err)
				}
				if lookupJournal(t, pool, r) != nil {
					t.Error("overlapping member mutation reused stale captured inputs")
				}
				in, err := l1.New(pool).CaptureJudgmentInputs(t.Context(), r.Inputs.Spec)
				if err != nil {
					t.Fatal(err)
				}
				if in.Hash == r.Inputs.Hash {
					t.Error("overlapping member mutation left input identity unchanged")
				}
				r.Inputs = in
				if lookupJournal(t, pool, r) != nil {
					t.Error("restart reused judgment after overlapping member mutation")
				}
			})
		}
	}
}
