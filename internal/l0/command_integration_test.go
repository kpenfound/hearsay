//go:build integration

package l0_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l0"
)

// A command is recorded once, a command id on record with other content is
// the command on record, and a command an operator deleted says so.
func TestRecordCommandSaysWhenTheCommandWasDeleted(t *testing.T) {
	pool := newPool(t)
	fake, source := newFake(t)
	store := l0.New(pool)
	ctx := t.Context()
	cmd := fake.NewEvent(connector.KindCommand, "interaction:1", "/hearsay pin")
	id := connector.EventID(source, cmd.NativeID)

	rewritten := cmd
	rewritten.Payload.Text = "/hearsay pin again"
	for _, tt := range []struct {
		name string
		ev   connector.Event
	}{{"the command", cmd}, {"the same command again", cmd}, {"the same id with other content", rewritten}} {
		if deleted, err := store.RecordCommand(ctx, tt.ev); err != nil || deleted {
			t.Errorf("RecordCommand(%s) = %v, %v; want recorded", tt.name, deleted, err)
		}
	}
	if got, err := store.Get(ctx, id); err != nil || got.Kind != connector.KindCommand || got.Payload.Text != cmd.Payload.Text {
		t.Fatalf("Get() = %+v, %v; want the command as first given", got, err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l0.Delete(ctx, tx, l0.Deletion{ID: "del_" + source, Operator: "kyle", Reason: "a test",
		Selector: json.RawMessage(`{"event":"` + id + `"}`), Events: []string{id}, Time: time.Now()}); err != nil {
		t.Fatalf("Delete() = %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if deleted, err := store.RecordCommand(ctx, cmd); err != nil || !deleted {
		t.Errorf("RecordCommand(a deleted command) = %v, %v; want deleted", deleted, err)
	}
}
