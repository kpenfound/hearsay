package l3

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/principal"
)

// ErrStartNotFound is shared by nonexistent and invisible starts. It carries
// no id, so the two cases have identical replies even for different ids.
var ErrStartNotFound = errors.New("no such traverse start")

// CandidateLimit bounds the raw candidate rows examined by one traversal.
const CandidateLimit = 5000

// Node is a graph handle and its type's one-line descriptive fields. Current
// is a position line for topics and a boolean for stances.
type Node struct {
	NodeRef
	Step         int    `json:"step"`
	EntityType   string `json:"entity_type,omitempty"`
	Name         string `json:"name,omitempty"`
	State        string `json:"state,omitempty"`
	Line         string `json:"line,omitempty"`
	LastActivity string `json:"last_activity,omitempty"`
	Current      any    `json:"current,omitempty"`
	Tier         string `json:"tier,omitempty"`
	Scope        string `json:"scope,omitempty"`
	TopicID      string `json:"topic_id,omitempty"`
	Position     string `json:"position,omitempty"`
	Author       string `json:"author,omitempty"`
	StatedAt     string `json:"stated_at,omitempty"`
	Kind         string `json:"kind,omitempty"`
	Class        string `json:"class,omitempty"`
}

// Edge connects two returned handles in the stored direction.
type Edge struct {
	From NodeRef `json:"from"`
	To   NodeRef `json:"to"`
	Edge string  `json:"edge"`
}

// Subgraph is an ordered, reader-scoped set of graph handles.
type Subgraph struct {
	Nodes     []Node `json:"nodes"`
	Edges     []Edge `json:"edges"`
	Truncated bool   `json:"truncated"`
}

// WithPrincipals gives principal handles their configured display names.
func (v *Views) WithPrincipals(resolver *principal.Resolver) *Views {
	c := *v
	c.principals = resolver
	return &c
}

type walkValue struct {
	node       Node
	at         time.Time
	visible    bool
	entity     l2.Entity
	doc        l1.Stored
	stance     l2.Stance
	assessment *l2.Assessment
}
type candidate struct {
	from, ref NodeRef
	at        time.Time
}
type traversal struct {
	v            *Views
	reader       l1.Reader
	values       map[NodeRef]*walkValue
	assessments  map[string]*l2.Assessment
	hiddenWork   l2.WorkEntityVisibility
	excludedWork []string
	examined     int
	out          Subgraph
	returned     map[NodeRef]bool
	edges        map[Edge]bool
}

// Traverse follows bounded typed steps, authorizing every node before using
// it as a frontier. All state belongs to this read; nothing is cached.
func (v *Views) Traverse(ctx context.Context, reader l1.Reader, q Query) (Subgraph, error) {
	if err := q.Validate(); err != nil {
		return Subgraph{}, err
	}
	hidden, err := v.graph.HiddenWorkEntities(ctx, reader)
	if err != nil {
		return Subgraph{}, err
	}
	w := traversal{v: v, reader: reader, values: map[NodeRef]*walkValue{}, assessments: map[string]*l2.Assessment{}, returned: map[NodeRef]bool{}, edges: map[Edge]bool{}, out: Subgraph{Nodes: []Node{}, Edges: []Edge{}}}
	w.hiddenWork = hidden
	for id, hidden := range hidden {
		if hidden {
			w.excludedWork = append(w.excludedWork, id)
		}
	}
	frontier := []NodeRef{}
	for _, ref := range q.Start {
		value, err := w.read(ctx, ref)
		if err != nil {
			return Subgraph{}, err
		}
		if !value.visible {
			return Subgraph{}, ErrStartNotFound
		}
		if !w.returned[value.node.NodeRef] {
			w.add(value, 0)
			frontier = append(frontier, value.node.NodeRef)
		}
	}
	limit := 50
	if q.Limit != nil {
		limit = *q.Limit
	}
	starts := len(w.out.Nodes)
	for stepIndex, step := range q.Steps {
		lo, hi, _ := step.depths()
		next := []NodeRef{}
		nextSeen := map[NodeRef]bool{}
		expanded := map[NodeRef]bool{}
		for depth := 1; depth <= hi && len(frontier) > 0; depth++ {
			candidates := []candidate{}
			rawRows := 0
			for _, from := range frontier {
				if expanded[from] {
					continue
				}
				expanded[from] = true
				remaining := CandidateLimit - w.examined - rawRows
				if remaining == 0 {
					break
				}
				rows, examined, err := w.candidates(ctx, from, step.Edge, remaining)
				if err != nil {
					return Subgraph{}, err
				}
				candidates = append(candidates, rows...)
				rawRows += examined
			}
			w.examined += rawRows
			if err := w.order(ctx, candidates); err != nil {
				return Subgraph{}, err
			}
			hop := []NodeRef{}
			hopSeen := map[NodeRef]bool{}
			for _, c := range candidates {
				value, err := w.read(ctx, c.ref)
				if err != nil {
					return Subgraph{}, err
				}
				if value.visible && matches(value, step.Where) {
					ref := value.node.NodeRef
					if !hopSeen[ref] {
						hop = append(hop, ref)
						hopSeen[ref] = true
					}
					if depth >= lo {
						if !w.returned[ref] {
							w.add(value, stepIndex+1)
						}
						if !nextSeen[ref] {
							next = append(next, ref)
							nextSeen[ref] = true
						}
					}
					edge := edgeTypes[step.Edge]
					link := Edge{From: c.from, To: ref, Edge: edge.stored}
					if edge.inverse {
						link.From, link.To = link.To, link.From
					}
					w.edges[link] = true
				}
				if len(w.out.Nodes)-starts >= limit {
					w.out.Truncated = true
					return w.finish(), nil
				}
			}
			if w.examined >= CandidateLimit {
				w.out.Truncated = true
				return w.finish(), nil
			}
			frontier = hop
		}
		frontier = next
	}
	return w.finish(), nil
}

func (w *traversal) add(value *walkValue, step int) {
	n := value.node
	n.Step = step
	w.out.Nodes = append(w.out.Nodes, n)
	w.returned[n.NodeRef] = true
}
func (w *traversal) finish() Subgraph {
	for e := range w.edges {
		if w.returned[e.From] && w.returned[e.To] {
			w.out.Edges = append(w.out.Edges, e)
		}
	}
	slices.SortFunc(w.out.Edges, func(a, b Edge) int {
		for _, pair := range [][2]string{{a.From.Type, b.From.Type}, {a.From.ID, b.From.ID}, {a.Edge, b.Edge}, {a.To.Type, b.To.Type}, {a.To.ID, b.To.ID}} {
			if c := strings.Compare(pair[0], pair[1]); c != 0 {
				return c
			}
		}
		return 0
	})
	return w.out
}

func (w *traversal) assess(ctx context.Context, ids []string) error {
	missing := []string{}
	for _, id := range ids {
		if _, ok := w.assessments[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	slices.Sort(missing)
	missing = slices.Compact(missing)
	topics, err := w.v.graph.TopicsOver(ctx, missing)
	if err != nil {
		return err
	}
	assessed, err := w.v.graph.Assess(ctx, w.v.authority, w.reader, topics)
	if err != nil {
		return err
	}
	for i := range assessed {
		a := &assessed[i]
		w.assessments[a.Topic.ID] = a
	}
	for _, id := range missing {
		if _, ok := w.assessments[id]; ok {
			continue
		}
		topic, err := w.v.graph.Topic(ctx, id)
		if errors.Is(err, l2.ErrNotFound) {
			w.assessments[id] = nil
			continue
		}
		if err != nil {
			return err
		}
		w.assessments[id] = w.assessments[topic.ID]
	}
	return nil
}

func (w *traversal) read(ctx context.Context, ref NodeRef) (*walkValue, error) {
	if value, ok := w.values[ref]; ok {
		return value, nil
	}
	value := &walkValue{node: Node{NodeRef: ref}}
	var err error
	switch ref.Type {
	case "entity":
		value.entity, err = w.v.graph.Entity(ctx, ref.ID)
		if err != nil {
			break
		}
		if w.hiddenWork.Hidden(ref.ID) || !w.reader.InReach([]string{ref.ID}) {
			break
		}
		value.visible = true
		value.node.EntityType = string(value.entity.Type)
		value.node.Name = Line(value.entity.Name)
		var subject l1.Stored
		var ok bool
		subject, ok, err = w.v.Subject(ctx, w.reader, ref.ID)
		if err != nil {
			break
		}
		if ok {
			value.node.Line = Line(subject.Body.Summary)
			if subject.State != nil {
				value.node.State = string(subject.State.Core)
			}
		}
		var docs []l1.Stored
		docs, err = w.v.docs.ListFor(ctx, w.reader, l1.ListOptions{Scope: ref.ID, Limit: 1})
		if len(docs) > 0 {
			value.node.LastActivity = stampLine(docs[0].Time.LastActivity)
		}
	case "document":
		value.doc, err = w.v.docs.Get(ctx, ref.ID)
		if err != nil {
			break
		}
		if !w.reader.MayRead(value.doc.Document) {
			break
		}
		d := value.doc
		value.visible = true
		value.at = d.Time.LastActivity
		value.node.Kind = string(d.Kind)
		value.node.Class = string(d.ArtifactClass)
		value.node.Line = Line(d.Body.Summary)
		value.node.LastActivity = stampLine(value.at)
		if d.State != nil {
			value.node.State = string(d.State.Core)
		}
	case "topic":
		if err = w.assess(ctx, []string{ref.ID}); err != nil {
			break
		}
		a := w.assessments[ref.ID]
		if a == nil || !a.Access.Topic(w.reader, a.Topic) {
			break
		}
		value.assessment = a
		value.visible = true
		value.node.ID = a.Topic.ID
		value.node.Name = Line(a.Topic.Name)
		// Scope is an entity handle too; readable topic evidence does not
		// authorize disclosure of a hidden work entity it names.
		if !w.hiddenWork.Hidden(a.Topic.Scope) {
			value.node.Scope = a.Topic.Scope
		}
		for _, st := range a.History {
			if (a.Access.Stance(w.reader, st) || a.Access.Withdrawal(w.reader, st)) && st.StatedAt.After(value.at) {
				value.at = st.StatedAt
			}
		}
		if a.Stands && a.Access.Stance(w.reader, a.Standing.Current) {
			value.node.Current = Line(a.Standing.Current.Position)
			value.node.Tier = string(a.Standing.Tier)
		}
	case "stance":
		value.stance, err = w.v.graph.Stance(ctx, ref.ID)
		if err != nil {
			break
		}
		if err = w.assess(ctx, []string{value.stance.TopicID}); err != nil {
			break
		}
		a := w.assessments[value.stance.TopicID]
		st := value.stance
		if a == nil || !a.Access.Topic(w.reader, a.Topic) || (!a.Access.Stance(w.reader, st) && !a.Access.Withdrawal(w.reader, st)) {
			break
		}
		value.assessment = a
		value.visible = true
		value.at = st.StatedAt
		value.node.TopicID = a.Topic.ID
		value.node.Position = Line(st.Position)
		value.node.Author = st.Author
		value.node.StatedAt = stampLine(st.StatedAt)
		value.node.Current = a.Stands && a.Standing.Current.ID == st.ID && a.Access.Stance(w.reader, a.Standing.Current)
	case "principal":
		value.visible = true
		value.node.Name = ref.ID
		if w.v.principals != nil {
			if p, ok := w.v.principals.Principal(ref.ID); ok && p.Name != "" {
				value.node.Name = Line(p.Name)
			}
		}
	}
	if err != nil && !errors.Is(err, l1.ErrNotFound) && !errors.Is(err, l2.ErrNotFound) {
		return nil, fmt.Errorf("reading traversal node: %w", err)
	}
	w.values[ref] = value
	if value.visible {
		w.values[value.node.NodeRef] = value
	}
	return value, nil
}

func stampLine(at time.Time) string { return at.UTC().Format(time.RFC3339Nano) }
