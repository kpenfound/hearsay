package l2

import (
	"context"
	"fmt"

	"github.com/kpenfound/hearsay/internal/l1"
)

// Access is the current access list and scope of the L1 documents some topics
// and stances rest on, as [Store.Access] read it. It is how a read decides who
// may see a topic or a stance (docs/design.md#access-control, control point 3):
// a stance is as readable as the least readable of its evidence *now*. A
// topic uses its opening document while it exists, then a live surviving
// stance if that document was deleted. Reach uses the same evidence.
//
// The access lists a topic and a stance were written with are what the
// document said when the worker read it. An access list re-synced since
// (ADR-0013), a second piece of evidence, or a document retracted since is not
// in them, so no read authorizes on them.
type Access struct {
	guards   map[string]l1.Guard
	fallback map[string][]Stance
}

// Access reads the current access lists of every document these topics were
// opened from and these stances rest on.
func (s *Store) Access(ctx context.Context, topics []Topic, stances []Stance) (Access, error) {
	seen := map[string]bool{}
	var ids []string
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, t := range topics {
		add(t.OpenedBy)
	}
	for _, st := range stances {
		for _, id := range st.Evidence {
			add(id)
		}
	}
	guards, err := l1.New(s.db).Guards(ctx, ids)
	if err != nil {
		return Access{}, fmt.Errorf("reading what the graph's evidence allows: %w", err)
	}
	a := Access{guards: guards, fallback: map[string][]Stance{}}
	byTopic := map[string][]Stance{}
	for _, st := range stances {
		byTopic[st.TopicID] = append(byTopic[st.TopicID], st)
	}
	for id, history := range byTopic {
		retired := retiredIn(history)
		for _, st := range history {
			if !retired[st.ID] && !st.Withdrawn {
				a.fallback[id] = append(a.fallback[id], st)
			}
		}
	}
	return a, nil
}

// Topic reports whether a reader may read a topic: its opening document's
// current guard decides while it exists. If it was deleted, any readable live
// stance keeps the topic available. A topic with no surviving stance is closed.
func (a Access) Topic(reader l1.Reader, t Topic) bool {
	if !reader.InReach(t.About) {
		return false
	}
	if _, ok := a.guards[t.OpenedBy]; ok {
		return a.allows(reader, t.OpenedBy) && a.inReach(reader, t.OpenedBy)
	}
	for _, st := range a.fallback[t.ID] {
		if a.Stance(reader, st) {
			return true
		}
	}
	return false
}

// TopicInReach reports whether a topic is about an entity in reach and its
// opening document, or a surviving live stance when the opener is gone, is
// in reach too.
func (a Access) TopicInReach(reader l1.Reader, t Topic) bool {
	if !reader.InReach(t.About) {
		return false
	}
	if _, ok := a.guards[t.OpenedBy]; ok {
		return a.inReach(reader, t.OpenedBy)
	}
	for _, st := range a.fallback[t.ID] {
		if a.StanceInReach(reader, st) {
			return true
		}
	}
	return false
}

// Stance reports whether a reader may read a stance: every piece of its
// evidence is in their reach, still in L1, and allows them. A stance with no
// evidence is readable by nobody.
func (a Access) Stance(reader l1.Reader, st Stance) bool {
	if len(st.Evidence) == 0 || !a.StanceInReach(reader, st) {
		return false
	}
	for _, id := range st.Evidence {
		if !a.allows(reader, id) {
			return false
		}
	}
	return true
}

// Withdrawal exposes only its fixed explanation to readers who belonged to
// the stance's recorded audience. Its former evidence no longer exists to
// authorize a normal stance read.
func (a Access) Withdrawal(reader l1.Reader, st Stance) bool {
	return st.Withdrawn && reader.Allows(st.ACL)
}

// StanceInReach reports whether every piece of a stance's evidence that is
// still in L1 is in a reader's reach.
func (a Access) StanceInReach(reader l1.Reader, st Stance) bool {
	for _, id := range st.Evidence {
		if !a.inReach(reader, id) {
			return false
		}
	}
	return true
}

func (a Access) allows(reader l1.Reader, id string) bool {
	g, ok := a.guards[id]
	return ok && reader.Allows(g.ACL)
}

func (a Access) inReach(reader l1.Reader, id string) bool {
	g, ok := a.guards[id]
	return !ok || reader.InReach(g.Scope)
}
