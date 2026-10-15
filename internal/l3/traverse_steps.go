package l3

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"
)

func (w *traversal) candidates(ctx context.Context, from NodeRef, edge string, remaining int) ([]candidate, int, error) {
	value, err := w.read(ctx, from)
	if err != nil {
		return nil, 0, err
	}
	out := []candidate{}
	rawRows := -1
	add := func(id string, at time.Time) {
		if id != "" {
			out = append(out, candidate{from: from, ref: NodeRef{Type: edgeTypes[edge].to, ID: id}, at: at})
		}
	}
	ids := []string{}
	switch edge {
	case "part_of":
		ids = value.entity.PartOf
	case "has_part":
		ids, err = w.v.graph.Children(ctx, from.ID, remaining)
	case "owners":
		ids = value.entity.Owners
	case "topics":
		related := []string{from.ID}
		subject, ok, e := w.v.Subject(ctx, w.reader, from.ID)
		if e != nil {
			return nil, 0, e
		}
		if ok {
			related = append(related, subject.Scope...)
		}
		topics, examined, e := w.v.topicsWithin(ctx, related, remaining)
		rawRows = examined
		if e != nil {
			return nil, 0, e
		}
		for _, t := range topics {
			add(t.ID, time.Time{})
		}
	case "about":
		ids = value.assessment.Topic.About
	case "stances":
		for _, st := range value.assessment.History {
			add(st.ID, st.StatedAt)
		}
	case "topic":
		ids = []string{value.stance.TopicID}
	case "documents":
		docs, e := w.v.docs.Candidates(ctx, from.ID, remaining)
		if e != nil {
			return nil, 0, e
		}
		for _, d := range docs {
			add(d.ID, d.LastActivity)
		}
	case "anchors":
		anchors, examined, e := w.v.anchorsWithin(ctx, w.reader, from.ID, remaining, remaining)
		rawRows = examined
		if e != nil {
			return nil, 0, e
		}
		for _, a := range anchors {
			add(a.Doc.ID, a.Doc.Time.LastActivity)
		}
	case "scope":
		ids = value.doc.Scope
	case "evidence":
		// Withdrawals may have deleted evidence; missing documents stay absent.
		// Hydrate through the node read before sorting or cutting the raw budget,
		// just as documents and anchors carry their activity into the frontier.
		for _, id := range value.stance.Evidence {
			doc, e := w.read(ctx, NodeRef{Type: "document", ID: id})
			if e != nil {
				return nil, 0, e
			}
			add(id, doc.at)
		}
	case "cited_by":
		stances, e := w.v.graph.Citing(ctx, from.ID, remaining)
		if e != nil {
			return nil, 0, e
		}
		for _, st := range stances {
			add(st.ID, st.StatedAt)
		}
	case "supersedes":
		if value.stance.Supersedes != "" {
			st, e := w.v.graph.Stance(ctx, value.stance.Supersedes)
			if e != nil {
				return nil, 0, e
			}
			add(st.ID, st.StatedAt)
		}
	case "superseded_by":
		stances, e := w.v.graph.Successors(ctx, from.ID, remaining)
		if e != nil {
			return nil, 0, e
		}
		for _, st := range stances {
			add(st.ID, st.StatedAt)
		}
	case "author":
		ids = []string{value.stance.Author}
	case "participants":
		for _, p := range value.doc.Participants {
			ids = append(ids, p.PrincipalID)
		}
	}
	for _, id := range ids {
		add(id, time.Time{})
	}
	slices.SortFunc(out, compareCandidates)
	if len(out) > remaining {
		out = out[:remaining]
	}
	if rawRows < 0 {
		rawRows = len(out)
	}
	return out, rawRows, err
}

func (w *traversal) order(ctx context.Context, candidates []candidate) error {
	// Topic order is reader-specific. Assess in one batch and use only readable
	// stances, so a hidden stance never changes where a visible topic sorts.
	if len(candidates) > 0 && candidates[0].ref.Type == "topic" {
		ids := make([]string, len(candidates))
		for i, c := range candidates {
			ids[i] = c.ref.ID
		}
		if err := w.assess(ctx, ids); err != nil {
			return err
		}
		for i := range candidates {
			value, err := w.read(ctx, candidates[i].ref)
			if err != nil {
				return err
			}
			candidates[i].ref = value.node.NodeRef
			candidates[i].at = value.at
		}
	}
	slices.SortFunc(candidates, compareCandidates)
	return nil
}

func compareCandidates(a, b candidate) int {
	if a.ref.Type == "document" || a.ref.Type == "topic" || a.ref.Type == "stance" {
		if c := b.at.Compare(a.at); c != 0 {
			return c
		}
	}
	if c := strings.Compare(a.ref.ID, b.ref.ID); c != 0 {
		return c
	}
	return strings.Compare(a.from.ID, b.from.ID)
}

func sortedWhere(where map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(where))
	for key := range where {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func matches(value *walkValue, where map[string]json.RawMessage) bool {
	for _, key := range sortedWhere(where) {
		raw := where[key]
		if key == "current" {
			var want bool
			_ = json.Unmarshal(raw, &want)
			if value.node.Current != want {
				return false
			}
			continue
		}
		var want string
		_ = json.Unmarshal(raw, &want)
		got := ""
		switch key {
		case "type":
			got = value.node.EntityType
		case "state":
			got = value.node.State
		case "kind":
			got = value.node.Kind
		case "class":
			got = value.node.Class
		case "tier":
			got = value.node.Tier
		case "since":
			since, _ := time.Parse(time.RFC3339, want)
			if value.at.IsZero() || value.at.Before(since) {
				return false
			}
			continue
		}
		if got != want {
			return false
		}
	}
	return true
}
