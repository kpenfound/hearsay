package connector_test

import (
	"context"
	"errors"
	"testing"

	"github.com/kpenfound/hearsay/internal/connector"
)

func TestReactionGesturesAreAnOptionalSourceCapability(t *testing.T) {
	r := connector.NewRegistry()
	const sourceType = "another-chat"
	factory := func(_ context.Context, src connector.SourceConfig) (connector.Connector, error) {
		return connector.NewFake(src), nil
	}
	if err := r.Register(sourceType, factory); err != nil {
		t.Fatal(err)
	}
	want := connector.ReactionGestures{Ratify: "yes", Demote: "no"}
	if err := r.RegisterReactionGestures(sourceType, func(src connector.SourceConfig) (connector.ReactionGestures, error) {
		if src.ID != "team" {
			return connector.ReactionGestures{}, errors.New("wrong source")
		}
		return want, nil
	}); err != nil {
		t.Fatal(err)
	}
	got, capable, err := r.ReactionGestures(connector.SourceConfig{ID: "team", Type: sourceType})
	if err != nil || !capable || got != want {
		t.Fatalf("configured gestures = %+v, %v, %v", got, capable, err)
	}
	_, capable, err = r.ReactionGestures(connector.SourceConfig{ID: "team", Type: sourceType, ReadOnly: true})
	if err != nil || capable {
		t.Fatalf("read-only gestures = %v, %v", capable, err)
	}
	_, capable, err = r.ReactionGestures(connector.SourceConfig{ID: "team", Type: "other"})
	if err != nil || capable {
		t.Fatalf("unregistered gestures = %v, %v", capable, err)
	}
}
