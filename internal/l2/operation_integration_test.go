//go:build integration

package l2_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/queue"
)

// namedTopic opens a topic called name in scope.
func namedTopic(t *testing.T, store *l2.Store, scope, name string) l2.Topic {
	t.Helper()
	doc := "l1:s:" + scope + ":" + name
	topic := l2.Topic{ID: l2.TopicID(scope, doc, 0, name), Scope: scope, Name: name, ACL: public, OpenedBy: doc}
	if opened, err := store.OpenTopic(t.Context(), topic); err != nil || !opened {
		t.Fatalf("OpenTopic(%s) = %v, %v", name, opened, err)
	}
	return topic
}

func addStance(t *testing.T, store *l2.Store, topic l2.Topic, doc, position string, hour int) l2.Stance {
	t.Helper()
	st := stance(topic, doc, position, hour)
	stored, _, err := store.AppendStance(t.Context(), st, st.StatedAt)
	if err != nil {
		t.Fatalf("AppendStance(%s) = %v", position, err)
	}
	return stored
}

// rows is every topic and stance row of a scope, as the tables hold them:
// reads follow the ledger, so this reads the tables.
func rows(t *testing.T, pool *pgxpool.Pool, scope string) ([]l2.Topic, map[string][]l2.Stance) {
	t.Helper()
	var topics []l2.Topic
	rows, err := pool.Query(t.Context(), `SELECT id, scope, name, opened_by, about, join_keys, created_at FROM l2_topics
WHERE scope = $1 ORDER BY id`, scope)
	if err != nil {
		t.Fatalf("reading the topic rows: %v", err)
	}
	for rows.Next() {
		var topic l2.Topic
		if err := rows.Scan(&topic.ID, &topic.Scope, &topic.Name, &topic.OpenedBy, &topic.About, &topic.JoinKeys, &topic.CreatedAt); err != nil {
			t.Fatalf("reading the topic rows: %v", err)
		}
		topics = append(topics, topic)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the topic rows: %v", err)
	}
	stances := map[string][]l2.Stance{}
	rows, err = pool.Query(t.Context(), `SELECT s.id, s.topic_id, s.position, coalesce(s.supersedes, ''), s.withdrawn, s.evidence, s.created_at
FROM l2_stances s JOIN l2_topics t ON t.id = s.topic_id WHERE t.scope = $1 ORDER BY s.id`, scope)
	if err != nil {
		t.Fatalf("reading the stance rows: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var st l2.Stance
		if err := rows.Scan(&st.ID, &st.TopicID, &st.Position, &st.Supersedes, &st.Withdrawn, &st.Evidence, &st.CreatedAt); err != nil {
			t.Fatalf("reading the stance rows: %v", err)
		}
		stances[st.TopicID] = append(stances[st.TopicID], st)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading the stance rows: %v", err)
	}
	return topics, stances
}

func operate(t *testing.T, pool *pgxpool.Pool, repo config.Repo, req l2.OperationRequest) l2.Operation {
	t.Helper()
	// Bounded, so a scope a bug left held fails here rather than at the
	// test binary's timeout.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	op, err := l2.Operate(ctx, pool, repo, req)
	if err != nil {
		t.Fatalf("Operate(%+v) = %v", req, err)
	}
	return op
}

func history(t *testing.T, store *l2.Store, f l2.OperationFilter) []int64 {
	t.Helper()
	ops, err := store.Operations(t.Context(), f)
	if err != nil {
		t.Fatalf("Operations(%+v) = %v", f, err)
	}
	ids := make([]int64, len(ops))
	for i, op := range ops {
		ids[i] = op.ID
	}
	return ids
}

func TestMergeSplitAndUndoAreRecordedAndTheRowsStay(t *testing.T) {
	pool := newPool(t)
	store := l2.New(pool)
	repo := loadRepo(t, "")
	scope, elsewhere := unique(), unique()
	a := namedTopic(t, store, scope, "the lock")
	b := namedTopic(t, store, scope, "who holds the lock")
	a1 := addStance(t, store, a, "l1:s:a1", "the queue takes the lock", 1)
	b1 := addStance(t, store, b, "l1:s:b1", "the engine takes the lock", 2)
	b2 := addStance(t, store, b, "l1:s:b2", "the engine keeps the lock", 3)
	other := namedTopic(t, store, elsewhere, "the lock")
	beforeTopics, beforeStances := rows(t, pool, scope)

	merged := operate(t, pool, repo, l2.OperationRequest{Kind: l2.OperationMerge, Principal: "kyle", Into: a.ID, From: b.ID})
	if merged.ID == 0 || merged.Kind != l2.OperationMerge || merged.Scope != scope || merged.Principal != "kyle" ||
		!slices.Equal(merged.Topics, []string{a.ID, b.ID}) || !slices.Equal(merged.Stances, slices.Sorted(slices.Values([]string{b1.ID, b2.ID}))) ||
		merged.At.IsZero() {
		t.Fatalf("the merge = %+v, want it to cover both topics and every stance on %s", merged, b.ID)
	}
	split := operate(t, pool, repo, l2.OperationRequest{Kind: l2.OperationSplit, Principal: "kyle", Topic: a.ID, Name: "who keeps the lock", Stances: []string{b2.ID}})
	if split.Name != "who keeps the lock" || !slices.Equal(split.Stances, []string{b2.ID}) ||
		!slices.Equal(split.Topics, []string{a.ID, l2.SplitTopicID(scope, a.ID, "who keeps the lock", []string{b2.ID})}) {
		t.Fatalf("the split = %+v, want the new name and the stance it moved", split)
	}

	// Undoing the merge while the split that took part of it back stands is
	// ambiguous: refused, naming the split, and nothing written.
	_, err := l2.Operate(t.Context(), pool, repo, l2.OperationRequest{Kind: l2.OperationUndo, Principal: "kyle", Undoes: merged.ID})
	var conflict *l2.ConflictError
	if !errors.As(err, &conflict) || !slices.Equal(conflict.Conflicting, []int64{split.ID}) {
		t.Fatalf("Operate(undo the merge under the split) = %v, want a conflict naming %d", err, split.ID)
	}
	if got := history(t, store, l2.OperationFilter{Scope: scope}); !slices.Equal(got, []int64{merged.ID, split.ID}) {
		t.Fatalf("the ledger after a refused undo = %v, want only the merge and the split", got)
	}

	undoSplit := operate(t, pool, repo, l2.OperationRequest{Kind: l2.OperationUndo, Principal: "sam", Undoes: split.ID})
	undoMerge := operate(t, pool, repo, l2.OperationRequest{Kind: l2.OperationUndo, Principal: "kyle", Undoes: merged.ID})
	if undoMerge.Undoes != merged.ID || !slices.Equal(undoMerge.Topics, merged.Topics) || !slices.Equal(undoMerge.Stances, merged.Stances) {
		t.Fatalf("the undo = %+v, want it to restore the merge's inputs", undoMerge)
	}
	_, err = l2.Operate(t.Context(), pool, repo, l2.OperationRequest{Kind: l2.OperationUndo, Principal: "kyle", Undoes: merged.ID})
	if !errors.As(err, &conflict) || !slices.Equal(conflict.Conflicting, []int64{undoMerge.ID}) {
		t.Fatalf("Operate(undo the merge again) = %v, want a conflict naming %d", err, undoMerge.ID)
	}

	// Refused on their face, or by who asked: nothing written.
	refused := []struct {
		req  l2.OperationRequest
		want error
		says string
	}{
		{l2.OperationRequest{Kind: l2.OperationMerge, Principal: "kyle", Into: a.ID, From: other.ID}, l2.ErrInvalid, "stays inside one scope"},
		{l2.OperationRequest{Kind: l2.OperationMerge, Principal: "kyle", Into: a.ID, From: "topic:nothing"}, l2.ErrNotFound, "topic:nothing"},
		{l2.OperationRequest{Kind: l2.OperationSplit, Principal: "kyle", Topic: a.ID, Name: "x", Stances: []string{b1.ID}}, l2.ErrInvalid, "is not on topic"},
		{l2.OperationRequest{Kind: l2.OperationMerge, Principal: "bot", Into: a.ID, From: b.ID}, l2.ErrNotAllowed, "only a person"},
		{l2.OperationRequest{Kind: l2.OperationUndo, Principal: "kyle", Undoes: undoMerge.ID}, l2.ErrInvalid, "is an undo"},
	}
	for _, r := range refused {
		if _, err := l2.Operate(t.Context(), pool, repo, r.req); !errors.Is(err, r.want) || !strings.Contains(err.Error(), r.says) {
			t.Errorf("Operate(%+v) = %v, want %v saying %q", r.req, err, r.want, r.says)
		}
	}
	all := []int64{merged.ID, split.ID, undoSplit.ID, undoMerge.ID}
	if got := history(t, store, l2.OperationFilter{Scope: scope}); !slices.Equal(got, all) {
		t.Fatalf("the ledger = %v, want %v", got, all)
	}
	if got := history(t, store, l2.OperationFilter{Scope: elsewhere}); len(got) != 0 {
		t.Fatalf("the other scope's ledger = %v, want nothing", got)
	}

	// The rows every operation was about are as they were.
	afterTopics, afterStances := rows(t, pool, scope)
	if !topicsEqual(beforeTopics, afterTopics) || !stancesEqual(beforeStances, afterStances) {
		t.Errorf("the rows changed:\nbefore %+v %+v\nafter  %+v %+v", beforeTopics, beforeStances, afterTopics, afterStances)
	}
	if got, err := store.Stance(t.Context(), a1.ID); err != nil || got.TopicID != a.ID {
		t.Errorf("Stance(a1) = %+v, %v, want it on its own topic", got, err)
	}

	// History says who, when, what and whether it was undone.
	ops, err := store.Operations(t.Context(), l2.OperationFilter{Scope: scope, Kind: l2.OperationMerge})
	if err != nil || len(ops) != 1 || ops[0].ID != merged.ID || ops[0].UndoneBy != undoMerge.ID || ops[0].Principal != "kyle" ||
		!ops[0].At.Equal(merged.At) || !slices.Equal(ops[0].Stances, merged.Stances) {
		t.Fatalf("Operations(merges) = %+v, %v, want the merge, undone by %d", ops, err, undoMerge.ID)
	}
	if got, err := store.Operation(t.Context(), split.ID); err != nil || got.UndoneBy != undoSplit.ID || got.Name != split.Name {
		t.Errorf("Operation(split) = %+v, %v, want it undone by %d", got, err, undoSplit.ID)
	}
	if got, err := store.Operation(t.Context(), undoSplit.ID); err != nil || got.Principal != "sam" || got.Undoes != split.ID || got.UndoneBy != 0 {
		t.Errorf("Operation(undo) = %+v, %v, want sam's undo of %d", got, err, split.ID)
	}
	if got := history(t, store, l2.OperationFilter{Scope: scope, Kind: l2.OperationUndo}); !slices.Equal(got, []int64{undoSplit.ID, undoMerge.ID}) {
		t.Errorf("Operations(undos) = %v", got)
	}
	if got := history(t, store, l2.OperationFilter{Scope: scope, Since: undoSplit.At}); !slices.Equal(got, []int64{undoSplit.ID, undoMerge.ID}) {
		t.Errorf("Operations(since the first undo) = %v", got)
	}
	if got := history(t, store, l2.OperationFilter{Scope: scope, Until: split.At}); !slices.Equal(got, []int64{merged.ID}) {
		t.Errorf("Operations(until the split) = %v", got)
	}
	if got := history(t, store, l2.OperationFilter{Kind: l2.OperationSplit, Since: merged.At, Until: undoMerge.At}); !slices.Contains(got, split.ID) || slices.Contains(got, merged.ID) {
		t.Errorf("Operations(splits in any scope, in the window) = %v, want the split and no merge", got)
	}
	if _, err := store.Operations(t.Context(), l2.OperationFilter{Kind: "rename"}); !errors.Is(err, l2.ErrInvalid) {
		t.Errorf("Operations(kind rename) = %v, want ErrInvalid", err)
	}
}

// A split's new topic has no row, and is still a topic of its scope that a
// later operation can name.
func TestASplitsTopicCanBeOperatedOn(t *testing.T) {
	pool := newPool(t)
	store := l2.New(pool)
	repo := loadRepo(t, "")
	scope := unique()
	a := namedTopic(t, store, scope, "the lock")
	c := namedTopic(t, store, scope, "the keys")
	addStance(t, store, a, "l1:s:a1", "the queue takes the lock", 1)
	a2 := addStance(t, store, a, "l1:s:a2", "the keys are separate", 2)
	split := operate(t, pool, repo, l2.OperationRequest{Kind: l2.OperationSplit, Principal: "kyle", Topic: a.ID, Name: "the keys again", Stances: []string{a2.ID}})
	merged := operate(t, pool, repo, l2.OperationRequest{Kind: l2.OperationMerge, Principal: "kyle", Into: c.ID, From: split.Topics[1]})
	if !slices.Equal(merged.Stances, []string{a2.ID}) {
		t.Errorf("the merge of the split's topic covers %v, want %v", merged.Stances, []string{a2.ID})
	}
	var row bool
	if err := pool.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM l2_topics WHERE id = $1)`, split.Topics[1]).Scan(&row); err != nil || row {
		t.Errorf("the split's topic has a row = %v, %v, want none", row, err)
	}
	// Merged away, it reads as the topic it went into.
	if got, err := store.Topic(t.Context(), split.Topics[1]); err != nil || got.ID != c.ID {
		t.Errorf("Topic(the split's topic) = %+v, %v, want %s", got, err, c.ID)
	}
}

func topicsEqual(a, b []l2.Topic) bool {
	return slices.EqualFunc(a, b, func(x, y l2.Topic) bool {
		return x.ID == y.ID && x.Scope == y.Scope && x.Name == y.Name && x.OpenedBy == y.OpenedBy &&
			slices.Equal(x.About, y.About) && slices.Equal(x.JoinKeys, y.JoinKeys) && x.CreatedAt.Equal(y.CreatedAt)
	})
}

func stancesEqual(a, b map[string][]l2.Stance) bool {
	if len(a) != len(b) {
		return false
	}
	for topic, x := range a {
		if !slices.EqualFunc(x, b[topic], func(s, u l2.Stance) bool {
			return s.ID == u.ID && s.TopicID == u.TopicID && s.Position == u.Position && s.Supersedes == u.Supersedes &&
				s.Withdrawn == u.Withdrawn && slices.Equal(s.Evidence, u.Evidence) && s.CreatedAt.Equal(u.CreatedAt)
		}) {
			return false
		}
	}
	return true
}

// A failure after the ledger row is written rolls the whole operation back,
// and the scope is let go.
func TestAFailedOperationWritesNothingAndLetsTheScopeGo(t *testing.T) {
	pool := scratchPool(t)
	store := l2.New(pool)
	repo := loadRepo(t, "")
	scope := unique()
	a := namedTopic(t, store, scope, "the lock")
	b := namedTopic(t, store, scope, "who holds the lock")
	ctx := t.Context()
	if _, err := pool.Exec(ctx, `
CREATE FUNCTION refuse_operation() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'injected failure'; END $$;
CREATE TRIGGER refuse_operation AFTER INSERT ON l2_topic_operations FOR EACH ROW EXECUTE FUNCTION refuse_operation();`); err != nil {
		t.Fatalf("installing the failure: %v", err)
	}
	req := l2.OperationRequest{Kind: l2.OperationMerge, Principal: "kyle", Into: a.ID, From: b.ID}
	if _, err := l2.Operate(ctx, pool, repo, req); err == nil || !strings.Contains(err.Error(), "injected failure") {
		t.Fatalf("Operate(with the insert failing) = %v, want the failure", err)
	}
	if got := history(t, store, l2.OperationFilter{}); len(got) != 0 {
		t.Fatalf("the ledger after a failed operation = %v, want nothing", got)
	}
	client, err := queue.New(pool, queue.Config{Kind: l2.AssertKind()})
	if err != nil {
		t.Fatal(err)
	}
	if running, err := client.List(ctx, queue.StateRunning, 0); err != nil || len(running) != 0 {
		t.Fatalf("running assert jobs after a failed operation = %v, %v, want the hold let go", running, err)
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER refuse_operation ON l2_topic_operations`); err != nil {
		t.Fatal(err)
	}
	quick, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := l2.Operate(quick, pool, repo, req); err != nil {
		t.Fatalf("Operate(again) = %v, want the scope free", err)
	}
}

// An operation takes its turn among the assert jobs of its scope: it waits for
// the one running, decides on what that job committed, and holds the scope
// against the next until it has recorded. Another scope does not wait.
func TestOperationsSerializeWithAssertJobsPerScope(t *testing.T) {
	pool := scratchPool(t)
	store := l2.New(pool)
	repo := loadRepo(t, "")
	ctx := t.Context()
	busy, idle := unique(), unique()
	a := namedTopic(t, store, busy, "the lock")
	b := namedTopic(t, store, busy, "who holds the lock")
	b1 := addStance(t, store, b, "l1:s:b1", "the engine takes the lock", 1)
	c := namedTopic(t, store, idle, "the lock")
	d := namedTopic(t, store, idle, "who holds the lock")

	client, err := queue.New(pool, queue.Config{Kind: l2.AssertKind(), Lease: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"l1:first", "l1:second"} {
		if _, err := queue.Enqueue(ctx, pool, queue.Request{Kind: l2.AssertKind(), TargetID: target, SerialKey: busy}); err != nil {
			t.Fatal(err)
		}
	}
	running, err := client.Claim(ctx)
	if err != nil || len(running) != 1 || running[0].TargetID != "l1:first" {
		t.Fatalf("Claim() = %v, %v, want the first job", running, err)
	}

	type result struct {
		op  l2.Operation
		err error
	}
	done := make(chan result, 1)
	go func() {
		op, err := l2.Operate(ctx, pool, repo, l2.OperationRequest{Kind: l2.OperationMerge, Principal: "kyle", Into: a.ID, From: b.ID})
		done <- result{op, err}
	}()
	select {
	case r := <-done:
		t.Fatalf("Operate() = %+v, %v while an assert job runs on its scope, want it to wait", r.op, r.err)
	case <-time.After(4 * queue.HoldPoll):
	}

	// Another scope is not held up.
	quick, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := l2.Operate(quick, pool, repo, l2.OperationRequest{Kind: l2.OperationMerge, Principal: "kyle", Into: c.ID, From: d.ID}); err != nil {
		t.Fatalf("Operate(another scope) = %v, want it to run at once", err)
	}

	// What the running job writes before it finishes is what the merge covers.
	b2 := addStance(t, store, b, "l1:s:b2", "the engine keeps the lock", 2)
	if ok, err := client.Complete(ctx, running[0]); err != nil || !ok {
		t.Fatalf("Complete() = %v, %v", ok, err)
	}
	var r result
	select {
	case r = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Operate() did not run once the scope was free")
	}
	if r.err != nil {
		t.Fatalf("Operate() = %v", r.err)
	}
	if want := slices.Sorted(slices.Values([]string{b1.ID, b2.ID})); !slices.Equal(r.op.Stances, want) {
		t.Errorf("the merge covers %v, want %v: the stance the job wrote before it finished", r.op.Stances, want)
	}

	// And the next job on the scope runs after the operation, not during it.
	next, err := client.Claim(ctx)
	if err != nil || len(next) != 1 || next[0].TargetID != "l1:second" {
		t.Fatalf("Claim() after the operation = %v, %v, want the second job", next, err)
	}
}
