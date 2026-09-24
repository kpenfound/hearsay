//go:build integration

package l2_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
)

// read is what the store's reads say of one topic id: the topic it is, its
// history and the topic each stance is on, where it stands, and how many
// operations shaped it.
type read struct {
	topic, name, history, on, current string
	operations                        int
}

func readTopic(t *testing.T, store *l2.Store, id string) read {
	t.Helper()
	topic, err := store.Topic(t.Context(), id)
	if err != nil {
		t.Fatalf("Topic(%s) = %v", id, err)
	}
	history, err := store.StanceHistory(t.Context(), id)
	if err != nil {
		t.Fatalf("StanceHistory(%s) = %v", id, err)
	}
	assessed, err := store.Assess(t.Context(), config.Authority{}, l1.Reader{}, []l2.Topic{topic})
	if err != nil {
		t.Fatalf("Assess(%s) = %v", id, err)
	}
	r := read{topic: topic.ID, name: topic.Name, operations: len(topic.Operations)}
	var ids, on []string
	for _, st := range history {
		ids, on = append(ids, st.ID), append(on, st.TopicID)
	}
	r.history, r.on = strings.Join(ids, " "), strings.Join(on, " ")
	if assessed[0].Stands {
		r.current = assessed[0].Standing.Current.ID
	}
	return r
}

func topicIDs(t *testing.T, topics []l2.Topic, err error) []string {
	t.Helper()
	if err != nil {
		t.Fatalf("listing topics: %v", err)
	}
	out := make([]string, len(topics))
	for i, topic := range topics {
		out[i] = topic.ID
	}
	slices.Sort(out)
	return out
}

func joined(ids ...string) string { return strings.Join(ids, " ") }

func sortedIDs(ids ...string) []string { return slices.Sorted(slices.Values(ids)) }

func TestAMergeIsReadThroughEitherTopicAndItsUndoReadsAsBefore(t *testing.T) {
	pool := newPool(t)
	store := l2.New(pool)
	repo := loadRepo(t, "")
	scope := unique()
	a := namedTopic(t, store, scope, "the lock")
	b := namedTopic(t, store, scope, "who holds the lock")
	a1 := addStance(t, store, a, "l1:s:a1", "the queue takes the lock", 1)
	b1 := addStance(t, store, b, "l1:s:b1", "the engine takes the lock", 2)
	b2 := addStance(t, store, b, "l1:s:b2", "the engine keeps the lock", 3)
	before := []read{readTopic(t, store, a.ID), readTopic(t, store, b.ID)}
	if before[0].current != a1.ID || before[1].current != b2.ID {
		t.Fatalf("before the merge the topics stand at %s and %s, want %s and %s", before[0].current, before[1].current, a1.ID, b2.ID)
	}

	merged := operate(t, pool, repo, l2.OperationRequest{Kind: l2.OperationMerge, Principal: "kyle", Into: a.ID, From: b.ID})
	want := read{
		topic: a.ID, name: a.Name, operations: 1,
		history: joined(a1.ID, b1.ID, b2.ID), on: joined(a.ID, a.ID, a.ID),
		// The newest of every stance on both: b's, not a's own.
		current: b2.ID,
	}
	for _, id := range []string{a.ID, b.ID} {
		if got := readTopic(t, store, id); got != want {
			t.Errorf("read through %s = %+v, want %+v", id, got, want)
		}
	}
	if topic, err := store.Topic(t.Context(), b.ID); err != nil || topic.Operations[0].ID != merged.ID {
		t.Errorf("Topic(b).Operations = %+v, %v, want the merge", topic.Operations, err)
	}
	topics, err := store.Topics(t.Context(), scope)
	if got := topicIDs(t, topics, err); !slices.Equal(got, []string{a.ID}) {
		t.Errorf("Topics() = %v, want only the topic merged into", got)
	}
	if got, err := store.Stance(t.Context(), b1.ID); err != nil || got.TopicID != a.ID {
		t.Errorf("Stance(b1) = %+v, %v, want it on %s", got, err, a.ID)
	}

	operate(t, pool, repo, l2.OperationRequest{Kind: l2.OperationUndo, Principal: "kyle", Undoes: merged.ID})
	after := []read{readTopic(t, store, a.ID), readTopic(t, store, b.ID)}
	if !slices.Equal(before, after) {
		t.Errorf("after the undo the reads are %+v, want them as before the merge: %+v", after, before)
	}
	topics, err = store.Topics(t.Context(), scope)
	if got := topicIDs(t, topics, err); !slices.Equal(got, sortedIDs(a.ID, b.ID)) {
		t.Errorf("Topics(after the undo) = %v, want both", got)
	}
}

func TestAStanceOnAMergedTopicGoesWhereTheTopicWent(t *testing.T) {
	pool := newPool(t)
	store := l2.New(pool)
	repo := loadRepo(t, "")
	ctx := t.Context()
	scope := unique()
	a := namedTopic(t, store, scope, "the lock")
	b := namedTopic(t, store, scope, "who holds the lock")
	a1 := addStance(t, store, a, "l1:s:a1", "the queue takes the lock", 1)
	b1 := addStance(t, store, b, "l1:s:b1", "the engine takes the lock", 2)
	merged := operate(t, pool, repo, l2.OperationRequest{Kind: l2.OperationMerge, Principal: "kyle", Into: a.ID, From: b.ID})

	// Nothing is written on a topic merged away.
	onB := stance(b, "l1:s:b1", "the engine drops the lock", 4)
	if _, _, err := store.AppendStance(ctx, onB, onB.StatedAt); !errors.Is(err, l2.ErrInvalid) {
		t.Fatalf("AppendStance(on the topic merged away) = %v, want ErrInvalid", err)
	}
	target, err := store.Target(ctx, b.ID, public, "l1:s:b1")
	if err != nil || target.ID != a.ID {
		t.Fatalf("Target(b) = %+v, %v, want %s", target, err, a.ID)
	}
	// b1's document read again: its earlier reading, on the other row, is what
	// the new one replaces.
	again := addStance(t, store, target, "l1:s:b1", "the engine drops the lock", 4)
	if again.TopicID != a.ID || again.Supersedes != b1.ID || again.SupersedesTopic != "" {
		t.Fatalf("the new reading = %+v, want it on %s superseding %s", again, a.ID, b1.ID)
	}
	if got := readTopic(t, store, b.ID); got.history != joined(a1.ID, b1.ID, again.ID) || got.current != again.ID {
		t.Errorf("read through b = %+v, want the new reading current", got)
	}

	// Undone, b stands again on its own row; the reading that replaced its
	// stance stays where it was written, and the edge between them says it
	// crosses, from both ends.
	operate(t, pool, repo, l2.OperationRequest{Kind: l2.OperationUndo, Principal: "kyle", Undoes: merged.ID})
	hb, err := store.StanceHistory(ctx, b.ID)
	if err != nil || len(hb) != 1 || hb[0].ID != b1.ID || !hb[0].Retired ||
		!slices.Equal(hb[0].SupersededAcross, []l2.StanceRef{{Stance: again.ID, Topic: a.ID}}) {
		t.Errorf("StanceHistory(b) = %+v, %v, want b1 retired by %s on %s", hb, err, again.ID, a.ID)
	}
	ha, err := store.StanceHistory(ctx, a.ID)
	if err != nil || len(ha) != 2 || ha[1].ID != again.ID || ha[1].SupersedesTopic != b.ID {
		t.Errorf("StanceHistory(a) = %+v, %v, want %s superseding across to %s", ha, err, again.ID, b.ID)
	}
	if got := readTopic(t, store, b.ID); got.current != "" {
		t.Errorf("b stands at %s, want nowhere: its one stance was read again", got.current)
	}
}

func TestASplitReadsWithExactlyTheStancesItMoved(t *testing.T) {
	pool := newPool(t)
	store := l2.New(pool)
	repo := loadRepo(t, "")
	ctx := t.Context()
	scope := unique()
	a := namedTopic(t, store, scope, "the lock")
	c := namedTopic(t, store, scope, "the keys")
	a1 := addStance(t, store, a, "l1:s:d1", "the queue takes the lock", 1)
	a2 := addStance(t, store, a, "l1:s:d2", "the keys are separate", 2)
	a3 := addStance(t, store, a, "l1:s:d3", "the engine takes the lock", 3)
	a4 := addStance(t, store, a, "l1:s:d1", "the keys are per tenant", 4)
	if a2.Supersedes != a1.ID || a3.Supersedes != a2.ID || a4.Supersedes != a1.ID {
		t.Fatalf("the chain is %s<-%s, %s<-%s, %s<-%s, want a2 and a4 on a1 and a3 on a2",
			a2.Supersedes, a2.ID, a3.Supersedes, a3.ID, a4.Supersedes, a4.ID)
	}
	before := readTopic(t, store, a.ID)
	req := l2.OperationRequest{Kind: l2.OperationSplit, Principal: "kyle", Topic: a.ID, Name: "the keys again", Stances: []string{a2.ID, a4.ID}}
	split := operate(t, pool, repo, req)
	s := split.Topics[1]

	if got, want := readTopic(t, store, s), (read{
		topic: s, name: "the keys again", operations: 1, history: joined(a2.ID, a4.ID), on: joined(s, s), current: a4.ID,
	}); got != want {
		t.Errorf("read through the split's topic = %+v, want %+v", got, want)
	}
	// a1 was read again as a4, which moved: a1 is still retired, so a stands
	// at a3 alone.
	if got, want := readTopic(t, store, a.ID), (read{
		topic: a.ID, name: a.Name, operations: 1, history: joined(a1.ID, a3.ID), on: joined(a.ID, a.ID), current: a3.ID,
	}); got != want {
		t.Errorf("read through the split topic = %+v, want %+v", got, want)
	}
	if topic, err := store.Topic(ctx, s); err != nil || topic.OpenedBy != "" || topic.Scope != scope || !topic.CreatedAt.Equal(split.At) {
		t.Errorf("Topic(split) = %+v, %v, want no opening document, the scope and the split's time", topic, err)
	}
	topics, err := store.Topics(ctx, scope)
	if got := topicIDs(t, topics, err); !slices.Equal(got, sortedIDs(a.ID, c.ID, s)) {
		t.Errorf("Topics() = %v, want the split's topic too", got)
	}

	// Every edge the split cut is marked from both ends.
	hs, err := store.StanceHistory(ctx, s)
	if err != nil || len(hs) != 2 || hs[0].SupersedesTopic != a.ID || hs[1].SupersedesTopic != a.ID ||
		!slices.Equal(hs[0].SupersededAcross, []l2.StanceRef{{Stance: a3.ID, Topic: a.ID}}) {
		t.Errorf("StanceHistory(split) = %+v, %v, want both crossing back to %s and a2 superseded by a3 there", hs, err, a.ID)
	}
	ha, err := store.StanceHistory(ctx, a.ID)
	wantAcross := []l2.StanceRef{{Stance: a2.ID, Topic: s}, {Stance: a4.ID, Topic: s}}
	slices.SortFunc(wantAcross, func(x, y l2.StanceRef) int { return strings.Compare(x.Stance, y.Stance) })
	if err != nil || len(ha) != 2 || !ha[0].Retired || !slices.Equal(ha[0].SupersededAcross, wantAcross) || ha[1].SupersedesTopic != s {
		t.Errorf("StanceHistory(a) = %+v, %v, want a1 retired across the split and a3 superseding across it", ha, err)
	}

	// Undone, every read is as it was.
	operate(t, pool, repo, l2.OperationRequest{Kind: l2.OperationUndo, Principal: "kyle", Undoes: split.ID})
	if got := readTopic(t, store, a.ID); got != before {
		t.Errorf("read through a after the undo = %+v, want %+v", got, before)
	}
	if got := readTopic(t, store, s); got != before {
		t.Errorf("read through the undone split's topic = %+v, want a as before: %+v", got, before)
	}

	// Made again, the split's topic is the same one, and a stance can be
	// written on it: it gets a row, and supersedes what the topic holds.
	redo := operate(t, pool, repo, req)
	if redo.Topics[1] != s {
		t.Fatalf("the split made again created %s, want %s", redo.Topics[1], s)
	}
	target, err := store.Target(ctx, s, public, "l1:s:d5")
	if err != nil || target.ID != s || target.Name != "the keys again" {
		t.Fatalf("Target(split) = %+v, %v, want the split's topic", target, err)
	}
	d5 := addStance(t, store, target, "l1:s:d5", "the keys are per region", 5)
	if d5.TopicID != s || d5.Supersedes != a4.ID {
		t.Errorf("the stance on the split's topic = %+v, want it to supersede %s", d5, a4.ID)
	}
	if got := readTopic(t, store, s); got.history != joined(a2.ID, a4.ID, d5.ID) || got.current != d5.ID {
		t.Errorf("read through the split's topic = %+v, want the new stance on it", got)
	}
	// Undone again, what was written on the split's topic is on a.
	operate(t, pool, repo, l2.OperationRequest{Kind: l2.OperationUndo, Principal: "kyle", Undoes: redo.ID})
	if got := readTopic(t, store, s); got.topic != a.ID || got.history != joined(a1.ID, a2.ID, a3.ID, a4.ID, d5.ID) || got.current != d5.ID {
		t.Errorf("read through the split's topic after its undo = %+v, want a with every stance", got)
	}
	topics, err = store.Topics(ctx, scope)
	if got := topicIDs(t, topics, err); !slices.Equal(got, sortedIDs(a.ID, c.ID)) {
		t.Errorf("Topics(after the undo) = %v, want the split's row to be no topic", got)
	}
}

func TestMatchingFindsTopicsAsTheLedgerMakesThem(t *testing.T) {
	pool := newPool(t)
	store := l2.New(pool)
	repo := loadRepo(t, "")
	ctx := t.Context()
	scope := unique()
	private := connector.ACL{{Kind: connector.ACLIdentity, Source: "gh", NativeID: "kyle-node"}}
	a := openTopicFrom(t, pool, scope, "a", public, "item:1")
	b := openTopicFrom(t, pool, scope, "b", public, "item:2")
	p := openTopicFrom(t, pool, scope, "p", private, "item:3")
	q := openTopicFrom(t, pool, scope, "q", public, "item:4")
	addStance(t, store, a, putDoc(t, pool, scope, "d1", public, nil), "the queue takes the lock", 1)
	a2 := addStance(t, store, a, putDoc(t, pool, scope, "d2", public, nil), "the keys are separate", 2)
	secret := addStance(t, store, a, putDoc(t, pool, scope, "d3", private, nil), "the keys are in the vault", 3)
	addStance(t, store, b, putDoc(t, pool, scope, "d4", public, nil), "the engine takes the lock", 4)
	addStance(t, store, q, putDoc(t, pool, scope, "d5", public, nil), "the queue drops the lock", 5)
	operate(t, pool, repo, l2.OperationRequest{Kind: l2.OperationMerge, Principal: "kyle", Into: a.ID, From: b.ID})
	operate(t, pool, repo, l2.OperationRequest{Kind: l2.OperationMerge, Principal: "kyle", Into: p.ID, From: q.ID})
	open := operate(t, pool, repo, l2.OperationRequest{Kind: l2.OperationSplit, Principal: "kyle", Topic: a.ID, Name: "the keys", Stances: []string{a2.ID}})
	closed := operate(t, pool, repo, l2.OperationRequest{Kind: l2.OperationSplit, Principal: "kyle", Topic: a.ID, Name: "the vault", Stances: []string{secret.ID}})

	for _, tc := range []struct {
		name    string
		keys    []string
		readers connector.ACL
		want    []string
	}{
		{"a topic merged away is found as the topic it went into", []string{"item:2"}, public, []string{a.ID}},
		// The split's topic is found by the row it took its stances from.
		{"and only once", []string{"item:1", "item:2"}, public, []string{a.ID, open.Topics[1]}},
		{"a topic merged into one its readers may not read is not offered", []string{"item:4"}, public, []string{}},
		{"to those who may, it is", []string{"item:4"}, private, []string{p.ID}},
		{"a split's topic is offered only where one of its stances may be read", []string{"item:1"}, private,
			sortedIDs(a.ID, open.Topics[1], closed.Topics[1])},
	} {
		t.Run(tc.name, func(t *testing.T) {
			found, err := store.TopicsByJoinKeys(ctx, scope, tc.keys, tc.readers, 5)
			got := topicIDs(t, found, err)
			if !slices.Equal(got, sortedIDs(tc.want...)) {
				t.Errorf("TopicsByJoinKeys(%v) = %v, want %v", tc.keys, got, sortedIDs(tc.want...))
			}
		})
	}
}

// A deletion repair of a stance a split moved is written on the row the
// stance was written on, which the split's topic may not have.
func TestADeletionRepairOfAStanceASplitMovedIsWritten(t *testing.T) {
	pool := newPool(t)
	store := l2.New(pool)
	repo := loadRepo(t, "")
	ctx := t.Context()
	scope := unique()
	a := namedTopic(t, store, scope, "the lock")
	addStance(t, store, a, putDoc(t, pool, scope, "d1", public, nil), "the queue takes the lock", 1)
	gone := putDoc(t, pool, scope, "d2", public, nil)
	moved := addStance(t, store, a, gone, "the keys are separate", 2)
	split := operate(t, pool, repo, l2.OperationRequest{Kind: l2.OperationSplit, Principal: "kyle", Topic: a.ID, Name: "the keys", Stances: []string{moved.ID}})
	if _, err := l1.New(pool).Delete(ctx, gone); err != nil {
		t.Fatal(err)
	}
	if written, err := store.RerunDeletedEvidence(ctx, moved.ID, scope); err != nil || !written {
		t.Fatalf("RerunDeletedEvidence() = %v, %v, want the withdrawal written", written, err)
	}
	history, err := store.StanceHistory(ctx, split.Topics[1])
	if err != nil || len(history) != 1 || !history[0].Retired {
		t.Errorf("StanceHistory(the split's topic) = %+v, %v, want the moved stance retired by its withdrawal", history, err)
	}
}
