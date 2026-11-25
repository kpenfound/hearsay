//go:build integration

package l1_test

import (
	"slices"
	"testing"

	"github.com/kpenfound/hearsay/internal/db/dbtest"
	"github.com/kpenfound/hearsay/internal/l1"
)

func TestAConversationDistillationRecordIsExactAndReplaced(t *testing.T) {
	pool := dbtest.Pool(t)
	store := l1.New(pool)
	src, thread, other := newSource(t), "c-1", "c-2"
	first := l1.ConversationDistillation{Source: src, Thread: thread, Inputs: []string{"evt:b", "evt:a"}, Documents: []string{"l1:z", "l1:y"}}

	if _, ok, err := store.ConversationDistillationOf(t.Context(), src, thread); err != nil || ok {
		t.Fatalf("before any: %v, %v", ok, err)
	}
	if changed, err := store.PutConversationDistillation(t.Context(), first); err != nil || !changed {
		t.Fatalf("first = %v, %v", changed, err)
	}
	if changed, err := store.PutConversationDistillation(t.Context(), first); err != nil || changed {
		t.Fatalf("the same record again = %v, %v; want nothing written", changed, err)
	}
	got, ok, err := store.ConversationDistillationOf(t.Context(), src, thread)
	if err != nil || !ok || !slices.Equal(got.Inputs, []string{"evt:a", "evt:b"}) || !slices.Equal(got.Documents, []string{"l1:y", "l1:z"}) {
		t.Fatalf("read = %+v, %v, %v; want it sorted", got, ok, err)
	}
	if !got.Read("evt:a") || got.Read("evt:c") {
		t.Errorf("Read() disagrees with Inputs %v", got.Inputs)
	}

	second := l1.ConversationDistillation{Source: src, Thread: thread, Inputs: []string{"evt:a", "evt:c"}}
	if changed, err := store.PutConversationDistillation(t.Context(), second); err != nil || !changed {
		t.Fatalf("second = %v, %v", changed, err)
	}
	if _, err := store.PutConversationDistillation(t.Context(), l1.ConversationDistillation{Source: src, Thread: other, Inputs: []string{"evt:z"}}); err != nil {
		t.Fatal(err)
	}
	reading, err := store.ConversationDistillationsReading(t.Context(), []string{"evt:a", "evt:b", "evt:c", "evt:z", "evt:nobody"})
	if err != nil {
		t.Fatal(err)
	}
	if len(reading) != 3 || reading["evt:a"].Thread != thread || reading["evt:c"].Thread != thread || reading["evt:z"].Thread != other {
		t.Errorf("reading = %+v", reading)
	}
	if _, ok := reading["evt:b"]; ok {
		t.Errorf("a revision the latest distillation did not read is covered by it: %+v", reading["evt:b"])
	}
	if documents := reading["evt:a"].Documents; len(documents) != 0 {
		t.Errorf("documents = %v, want none", documents)
	}
	if none, err := store.ConversationDistillationsReading(t.Context(), nil); err != nil || len(none) != 0 {
		t.Errorf("reading nothing = %v, %v", none, err)
	}
}
