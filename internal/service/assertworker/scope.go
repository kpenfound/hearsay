package assertworker

import (
	"context"
	"errors"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l0"
)

// scopeRoot follows a control event to its artifact's root before computing a
// scope key. Commands and reactions need the root's touches, like distillation.
func scopeRoot(ctx context.Context, events *l0.Store, ev connector.Event) (connector.Event, error) {
	if ev.Kind == connector.KindTombstone {
		retracted, err := events.Retracted(ctx, ev.Source, ev.Payload.Target)
		if err != nil && !errors.Is(err, l0.ErrNotFound) {
			return connector.Event{}, err
		}
		if err == nil {
			ev = retracted
		}
	}
	seen := map[string]bool{ev.Payload.Artifact: true}
	for {
		parent := ev.Payload.Thread
		if parent == "" {
			parent = ev.Payload.Parent
		}
		if parent == "" || seen[parent] {
			return ev, nil
		}
		seen[parent] = true
		root, found, err := events.CurrentArtifact(ctx, ev.Source, parent)
		if err != nil {
			return connector.Event{}, err
		}
		if !found {
			return ev, nil
		}
		ev = root
	}
}
