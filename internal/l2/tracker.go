package l2

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
)

// Placement is an item and the entities it is part of. It comes from a
// configured tracker or a runtime work declaration.
type Placement struct {
	Item    Entity
	Parents []Entity
}

// ParentIDs is the ids of the placement's parents, in order.
func (p Placement) ParentIDs() []string {
	ids := make([]string, 0, len(p.Parents))
	for _, e := range p.Parents {
		ids = append(ids, e.ID)
	}
	return ids
}

// PlacementOf is what one L0 event says about the tracker hierarchy: an issue
// in a repository a configured scope's tracker maps, part of the issue its
// `payload.part_of` names where a scope's tracker maps that one too. It is
// false for an event that is no issue and for an issue no tracker maps, and a
// parent no tracker maps is left out: neither is an entity, and no id is
// invented for them.
func PlacementOf(repo config.Repo, ev connector.Event) (Placement, bool) {
	if ev.Kind != connector.KindIssue && ev.Payload.BaseKind != connector.KindIssue {
		return Placement{}, false
	}
	item, ok := TrackerItem(repo, ev.Source, ev.Payload.Artifact)
	if !ok {
		return Placement{}, false
	}
	p := Placement{Item: item}
	if parent, ok := TrackerItem(repo, ev.Source, ev.Payload.PartOf); ok && parent.ID != item.ID {
		p.Parents = []Entity{parent}
	}
	return p, true
}

// TrackerItem is the `tracker_item` entity of item `<project>#<item>` of a
// source, where a configured scope's tracker is that project in that source,
// and false where none is.
func TrackerItem(repo config.Repo, source, item string) (Entity, bool) {
	project, number, ok := strings.Cut(item, "#")
	if !ok || project == "" || number == "" {
		return Entity{}, false
	}
	for _, s := range repo.Scopes {
		if s.Tracker.Source != source || s.Tracker.Project != project {
			continue
		}
		if id, ok := s.TrackerItemID(number); ok {
			return Entity{ID: id, Type: TypeTrackerItem, Name: item, Aliases: []string{item}, Origin: OriginReference}, true
		}
	}
	return Entity{}, false
}

// Configured is the part_of `code/` sets, per entity: the hierarchy's
// highest-ranked source (ADR-0016).
func Configured(repo config.Repo) Parents {
	out := Parents{}
	for _, c := range repo.Code {
		out[c.ID] = slices.Clone(c.PartOf)
	}
	return out
}

// hierarchyLock is the advisory lock [Store.Place] holds for its transaction,
// so that two items placed at once are merged one after the other.
const hierarchyLock int64 = 0x68656172_73617931 // "hearsay1"

// Place records where a tracker or work source puts an item now, replacing
// its former parents, and merges all four hierarchy sources under configuration
// (ADR-0016): the merge may keep or drop an edge of another item's that closes
// a cycle with this one's, and every item whose parents it changed is written.
//
// A work declaration always creates or renames its entity and only uses
// parents already stored. For trackers, an item and its parents are created
// when there is an edge to record; an
// item with no parent that is not stored yet is not created, because a tracker
// item with nothing to say is not worth a row. Every other item's parents are
// read as stored, so an edge a merge dropped stays dropped until that item is
// placed again or the worker reseeds at startup. Run it in a transaction: the
// lock is held until it ends.
func (s *Store) Place(ctx context.Context, configured Parents, p Placement) error {
	if _, err := s.db.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, hierarchyLock); err != nil {
		return fmt.Errorf("locking the tracker hierarchy: %w", err)
	}
	if p.Item.Origin != OriginSource && len(p.Parents) > 0 {
		for _, e := range append([]Entity{p.Item}, p.Parents...) {
			if _, err := s.EnsureEntity(ctx, e); err != nil {
				return err
			}
		}
	}
	entities, err := s.Entities(ctx)
	if err != nil {
		return err
	}
	stored, tracker, work, structure := Parents{}, Parents{}, Parents{}, Parents{}
	known := map[string]*Entity{}
	for i := range entities {
		e := &entities[i]
		known[e.ID] = e
		stored[e.ID] = e.PartOf
		switch {
		case e.Origin == OriginSource:
			work[e.ID] = e.PartOf
		case e.Type == TypeTrackerItem:
			tracker[e.ID] = e.PartOf
		default:
			structure[e.ID] = e.PartOf
		}
	}
	if p.Item.Origin == OriginSource {
		p.Item.PartOf = existingWorkParents(ctx, p.Item.ID, p.ParentIDs(), known)
		if err := s.PutEntity(ctx, p.Item); err != nil {
			return err
		}
		stored[p.Item.ID] = p.Item.PartOf
		work[p.Item.ID] = p.Item.PartOf
	} else {
		tracker[p.Item.ID] = p.ParentIDs()
	}
	merged := MergeHierarchy(ctx, HierarchyInputs{Config: configured, Tracker: tracker, Work: work, RepoStructure: structure})
	for _, id := range sortedKeys(stored) {
		if slices.Equal(stored[id], merged[id]) {
			continue
		}
		if _, err := s.db.Exec(ctx, `UPDATE l2_entities SET part_of = $2, updated_at = now() WHERE id = $1`, id, orEmpty(merged[id])); err != nil {
			return fmt.Errorf("placing entity %s: %w", id, err)
		}
	}
	return nil
}
