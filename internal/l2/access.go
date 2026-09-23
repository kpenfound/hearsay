package l2

import (
	"context"
	"fmt"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
)

// Access is the current access list of the L1 documents some topics and
// stances rest on, as [Store.Access] read it. It is how a read decides who may
// see a topic or a stance (docs/design.md#access-control, control point 3): a
// stance is as readable as the least readable of its evidence *now*, and a
// topic as its opening document is now.
//
// The access lists a topic and a stance were written with are what the
// document said when the worker read it. An access list re-synced since
// (ADR-0013), a second piece of evidence, or a document retracted since is not
// in them, so no read authorizes on them.
type Access map[string]connector.ACL

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
	acls, err := l1.New(s.db).ACLs(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("reading what the graph's evidence allows: %w", err)
	}
	return Access(acls), nil
}

// Topic reports whether a reader may read a topic: the document that opened it
// is still in L1 and its access list allows them. A topic whose document is
// gone, or was not read, is readable by nobody.
func (a Access) Topic(reader l1.Reader, t Topic) bool {
	return a.allows(reader, t.OpenedBy)
}

// Stance reports whether a reader may read a stance: every piece of its
// evidence is still in L1, and every one's access list allows them. A stance
// with no evidence is readable by nobody.
func (a Access) Stance(reader l1.Reader, st Stance) bool {
	if len(st.Evidence) == 0 {
		return false
	}
	for _, id := range st.Evidence {
		if !a.allows(reader, id) {
			return false
		}
	}
	return true
}

func (a Access) allows(reader l1.Reader, id string) bool {
	acl, ok := a[id]
	return ok && reader.Allows(acl)
}
