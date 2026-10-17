package l2

import (
	"context"
	"fmt"
	"strings"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/telemetry"
)

// WorkPlacementOf describes a work source's project or workstream. The caller
// checks the source type; work declarations need no configured tracker mapping.
// Parents are resolved against existing entities when seeding or placing.
func WorkPlacementOf(ev connector.Event) (Placement, bool) {
	if ev.Kind != connector.KindIssue || ev.Payload.Artifact == "" {
		return Placement{}, false
	}
	artifact := ev.Payload.Artifact
	p := Placement{Item: Entity{ID: "work:" + ev.Source + ":" + artifact, Type: TypeProject, Name: ev.Payload.Title, Origin: OriginSource}}
	if project, _, stream := strings.Cut(artifact, "#"); stream {
		p.Item.Type = TypeTrackerItem
		p.Parents = []Entity{{ID: "work:" + ev.Source + ":" + project}}
	} else {
		for _, touch := range ev.Payload.Touches {
			p.Parents = append(p.Parents, Entity{ID: "code:" + touch.Project})
		}
	}
	return p, true
}

func existingWorkParents(ctx context.Context, id string, parents []string, known map[string]*Entity) []string {
	var out []string
	for _, parent := range parents {
		if _, ok := known[parent]; ok {
			out = append(out, parent)
		} else {
			telemetry.Logger(ctx).InfoContext(ctx, "work parent has no entity", "entity", id, "parent", parent)
		}
	}
	return out
}

// RemoveSourceEntity removes a runtime declaration and every edge to it. Run
// in a transaction, under the same hierarchy lock as incremental placement.
func (s *Store) RemoveSourceEntity(ctx context.Context, id string) error {
	if _, err := s.db.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, hierarchyLock); err != nil {
		return fmt.Errorf("locking the work hierarchy: %w", err)
	}
	tag, err := s.db.Exec(ctx, `DELETE FROM l2_entities WHERE id = $1 AND origin = 'source'`, id)
	if err != nil {
		return fmt.Errorf("removing work entity %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	if _, err := s.db.Exec(ctx, `UPDATE l2_entities SET part_of = array_remove(part_of, $1), updated_at = now() WHERE $1 = ANY(part_of)`, id); err != nil {
		return fmt.Errorf("removing edges to work entity %s: %w", id, err)
	}
	return nil
}
