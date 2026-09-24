package l3

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
)

// Querier is what the views run on: a pool, or a transaction on one, so that
// every view one bundle reads can see the same snapshot. It is the graph's,
// because the views read entities through [l2.Store]; nothing here writes.
type Querier = l2.Querier

// Both of pgx's are one.
var (
	_ Querier = (*pgxpool.Pool)(nil)
	_ Querier = (pgx.Tx)(nil)
)

// RecentCandidates is how many of a scope's newest readable documents
// [Views.Recent] looks at before it dedupes by kind. It is the reach of the
// dedupe: a kind with no document among this many cannot win a place.
const RecentCandidates = 50

// Views are the derived views over one database. Nothing here writes.
type Views struct {
	db        Querier
	docs      *l1.Store
	graph     *l2.Store
	authority config.Authority
}

// New returns the views over a pool or a transaction, under the built-in
// authority policy. The caller owns it.
func New(q Querier) *Views { return &Views{db: q, docs: l1.New(q), graph: l2.New(q)} }

// WithAuthority returns a copy of the views that computes tiers under a
// configuration's authority policies rather than the built-in one.
func (v *Views) WithAuthority(a config.Authority) *Views {
	c := *v
	c.authority = a
	return &c
}

// CurrentStance is the stance a topic stands at now, for a reader.
type CurrentStance struct {
	Topic l2.Topic
	// Stance is the topic's current stance, computed under the policy in
	// force for its scope ([l2.Stand]). Its Tier is the tier it was written
	// with; the tier it is served at is Tier below.
	Stance l2.Stance
	// Tier is the computed tier: ratified, inferred or contested.
	Tier l2.Tier
	// Supersedes is the position the stance replaced, empty for the first
	// stance on a topic and for one the reader may not read.
	Supersedes string
	// Inherited reports a topic that is not about the entity asked for itself:
	// it is about a related entity or an ancestor of one
	// (docs/design.md#l3-derived-views).
	Inherited bool
}

// topicsSQL is every topic about one of the entities or an ancestor of one. The
// walk up `part_of` is a UNION, so a cycle ends it rather than looping.
const topicsSQL = `
WITH RECURSIVE up(id) AS (
    SELECT unnest($1::text[])
    UNION
    SELECT unnest(e.part_of) FROM l2_entities e JOIN up ON e.id = up.id
)
SELECT t.id, t.scope, t.name, t.about, t.acl, t.opened_by, t.created_at
FROM l2_topics t
WHERE t.about && ARRAY(SELECT id FROM up)
ORDER BY t.id`

// CurrentStances is the stance every topic about one entity stands at, then the
// ones it inherits: topics about the related entities — for a tracker item, the
// code entities its own document is about — and about the ancestors of either.
// Only a topic whose `about` names the entity itself is its own; every other is
// tagged inherited, however it was reached. Own stances come first and inherited
// ones after, each newest first, so a caller cutting from the bottom cuts what
// is inherited before what is the entity's.
//
// The line is drawn at the entity itself because a document's scope is broad:
// every document in a repository is about that repository's code entity, and
// so is every topic one of them opened. Counting those as the item's own would
// make every topic in the repository a stance of every item in it.
//
// Where a topic stands — its current stance and tier — is computed under the
// policy in force for its scope ([l2.Store.Assess]). The current stance is
// chosen from every stance on the topic, not from what the reader may read: it
// is the topic's, and a bundle that offered an older one in its place would be
// wrong in a way the reader could not see. A topic the reader may not read, or
// whose current stance they may not read, is left out and counted in the second
// result. A stance they may not read does not make the topic contested for them.
//
// Reach is applied before the access lists, and counted apart from them in the
// third result: a topic out of the reader's reach, or whose current stance
// rests on a document out of it, is left out whatever its access list says —
// which is how a stance inherited from an ancestor outside the reach is
// withheld ([l2.Access.TopicInReach]).
//
// Who may read is decided from L1 as it is now ([l2.Access]): a topic by its
// opening document while it exists, then by a surviving live stance; a stance
// by every piece of its evidence. A document re-synced private or retracted
// since the worker read it removes access to stances that rest only on it.
func (v *Views) CurrentStances(ctx context.Context, reader l1.Reader, own string, related []string) ([]CurrentStance, int, int, error) {
	if own == "" {
		return []CurrentStance{}, 0, 0, nil
	}
	entities := append([]string{own}, related...)
	topics, err := v.topics(ctx, entities)
	if err != nil {
		return nil, 0, 0, err
	}
	assessed, err := v.graph.Assess(ctx, v.authority, reader, topics)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("reading current stances: %w", err)
	}
	out := []CurrentStance{}
	withheld, outOfReach := 0, 0
	for _, a := range assessed {
		if !a.Stands {
			continue
		}
		current := a.Standing.Current
		if !a.Access.TopicInReach(reader, a.Topic) || !a.Access.StanceInReach(reader, current) {
			outOfReach++
			continue
		}
		if !a.Access.Topic(reader, a.Topic) || !a.Access.Stance(reader, current) {
			withheld++
			continue
		}
		c := CurrentStance{Topic: a.Topic, Stance: current, Tier: a.Standing.Tier, Inherited: !slices.Contains(a.Topic.About, own)}
		for _, st := range a.History {
			if st.ID == current.Supersedes && a.Access.Stance(reader, st) {
				c.Supersedes = st.Position
			}
		}
		out = append(out, c)
	}
	// Own before inherited, and each newest stated first.
	slices.SortStableFunc(out, func(a, b CurrentStance) int {
		if a.Inherited != b.Inherited {
			if b.Inherited {
				return -1
			}
			return 1
		}
		if c := b.Stance.StatedAt.Compare(a.Stance.StatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.Topic.ID, b.Topic.ID)
	})
	return out, withheld, outOfReach, nil
}

func (v *Views) topics(ctx context.Context, entities []string) ([]l2.Topic, error) {
	rows, err := v.db.Query(ctx, topicsSQL, entities)
	if err != nil {
		return nil, fmt.Errorf("reading current stances: %w", err)
	}
	defer rows.Close()
	var out []l2.Topic
	for rows.Next() {
		var t l2.Topic
		var acl []byte
		if err := rows.Scan(&t.ID, &t.Scope, &t.Name, &t.About, &acl, &t.OpenedBy, &t.CreatedAt); err != nil {
			return nil, fmt.Errorf("reading current stances: %w", err)
		}
		if err := json.Unmarshal(acl, &t.ACL); err != nil {
			return nil, fmt.Errorf("decoding the access list of topic %s: %w", t.ID, err)
		}
		t.CreatedAt = t.CreatedAt.UTC()
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading current stances: %w", err)
	}
	return out, nil
}

// Activity is the recent activity on a scope.
type Activity struct {
	// LastActivity is the newest activity the reader may see on the scope,
	// zero where there is none. It is surfaced, never used as a filter.
	LastActivity time.Time
	// Items are the documents, newest first.
	Items []l1.Stored
}

// Recent is the last n documents about an entity the reader may read, capped by
// count and never by age (docs/design.md#the-context-bundle). Before it cuts by
// recency it dedupes by kind: the newest document of every kind among the
// [RecentCandidates] newest takes a place first, and the rest of the places go
// by recency, so five commits do not crowd out the one issue that changed the
// plan.
//
// The documents in except — a bundle's anchors — take no place and are not
// counted among the candidates, so an anchor is never repeated as activity and
// never costs `recent` one of its places. They still count as activity on the
// scope: an edit to an anchor can be the scope's last activity.
func (v *Views) Recent(ctx context.Context, reader l1.Reader, scope string, n int, except []string) (Activity, error) {
	if n <= 0 {
		return Activity{Items: []l1.Stored{}}, nil
	}
	listed, err := v.docs.ListFor(ctx, reader, l1.ListOptions{Scope: scope, Limit: RecentCandidates + len(except)})
	if err != nil {
		return Activity{}, err
	}
	out := Activity{Items: []l1.Stored{}}
	if len(listed) == 0 {
		return out, nil
	}
	out.LastActivity = listed[0].Time.LastActivity
	candidates := slices.DeleteFunc(listed, func(doc l1.Stored) bool { return slices.Contains(except, doc.ID) })
	if len(candidates) > RecentCandidates {
		candidates = candidates[:RecentCandidates]
	}

	taken := make([]bool, len(candidates))
	seen := map[l1.Kind]bool{}
	picked := 0
	for i, doc := range candidates {
		if picked == n {
			break
		}
		if !seen[doc.Kind] {
			seen[doc.Kind], taken[i] = true, true
			picked++
		}
	}
	for i := range candidates {
		if picked == n {
			break
		}
		if !taken[i] {
			taken[i] = true
			picked++
		}
	}
	for i, doc := range candidates {
		if taken[i] {
			out.Items = append(out.Items, doc)
		}
	}
	return out, nil
}

// Anchor is a durable document that defines a scope (docs/design.md#anchors).
type Anchor struct {
	Doc l1.Stored
	// PinnedBy is the principal who pinned it, empty for an anchor that was
	// inferred or defaulted.
	PinnedBy string
}

// Anchors is up to n documents that define an entity, for a reader: the ones
// people pinned to it, first pinned first; then the one inferred as the most
// referenced by the other documents about it ([l1.Store.MostReferencedFor]);
// then its `spec` documents, newest activity first. A document is an anchor
// once, where it first qualifies, and nothing configured adds one (docs/config.md).
//
// Every one is a document the reader may read. A pinned document they may not
// read — or one no longer in L1 — is left out and takes no place, and the
// inference and the defaults only ever see what they may read, so what is
// hidden from them neither takes a place nor decides which document does.
func (v *Views) Anchors(ctx context.Context, reader l1.Reader, scope string, n int) ([]Anchor, error) {
	out := []Anchor{}
	if n <= 0 || scope == "" || reader.Effective.Human == "" || !reader.Effective.Grant.Scopes.Has(scope) {
		return out, nil
	}
	add := func(doc l1.Stored, pinnedBy string) {
		if len(out) < n && !slices.ContainsFunc(out, func(a Anchor) bool { return a.Doc.ID == doc.ID }) {
			out = append(out, Anchor{Doc: doc, PinnedBy: pinnedBy})
		}
	}

	pins, err := v.graph.Pins(ctx, scope)
	if err != nil {
		return nil, err
	}
	for _, pin := range pins {
		if len(out) == n {
			return out, nil
		}
		doc, err := v.docs.Get(ctx, pin.L1)
		if errors.Is(err, l1.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if reader.MayRead(doc.Document) {
			add(doc, pin.PinnedBy)
		}
	}
	if len(out) == n {
		return out, nil
	}

	inferred, _, ok, err := v.docs.MostReferencedFor(ctx, reader, scope)
	if err != nil {
		return nil, err
	}
	if ok {
		add(inferred, "")
	}
	if len(out) == n {
		return out, nil
	}

	// Enough that the ones already taken cannot leave a place unfilled.
	specs, err := v.docs.ListFor(ctx, reader, l1.ListOptions{Scope: scope, Class: config.ArtifactSpec, Limit: n + len(out)})
	if err != nil {
		return nil, err
	}
	for _, doc := range specs {
		add(doc, "")
	}
	return out, nil
}

// Question is one open question on a scope and the documents that leave it
// open.
type Question struct {
	Text     string
	Evidence []string
}

// OpenQuestions is what the newest n documents about an entity that leave
// something unanswered leave unanswered, newest document first. A question two
// documents ask in the same words is one question with both as evidence.
func (v *Views) OpenQuestions(ctx context.Context, reader l1.Reader, scope string, n int) ([]Question, error) {
	if n <= 0 {
		return []Question{}, nil
	}
	docs, err := v.docs.ListFor(ctx, reader, l1.ListOptions{Scope: scope, OpenQuestions: true, Limit: n})
	if err != nil {
		return nil, err
	}
	out := []Question{}
	at := map[string]int{}
	for _, doc := range docs {
		for _, q := range doc.Body.OpenQuestions {
			q = strings.TrimSpace(q)
			if q == "" {
				continue
			}
			i, ok := at[q]
			if !ok {
				i = len(out)
				at[q] = i
				out = append(out, Question{Text: q})
			}
			if !slices.Contains(out[i].Evidence, doc.ID) {
				out[i].Evidence = append(out[i].Evidence, doc.ID)
			}
		}
	}
	return out, nil
}

// Subject is the document an entity is, where it is one the reader may read: a
// tracker item's own issue or pull request. A tracker item's id is
// `tracker:<source>:<project>#<item>` (docs/config.md), which is the artifact id
// a tracker whose artifacts are `<project>#<item>` gives its document, so the
// document is read by id and kept only if it says it is about the entity. An
// entity that is no document, or one this derivation does not reach, has none.
func (v *Views) Subject(ctx context.Context, reader l1.Reader, entity string) (l1.Stored, bool, error) {
	rest, ok := strings.CutPrefix(entity, "tracker:")
	if !ok {
		return l1.Stored{}, false, nil
	}
	source, artifact, ok := strings.Cut(rest, ":")
	if !ok || !connector.ValidSourceID(source) || artifact == "" {
		return l1.Stored{}, false, nil
	}
	doc, err := v.docs.Get(ctx, l1.DocID(source, artifact))
	if errors.Is(err, l1.ErrNotFound) {
		return l1.Stored{}, false, nil
	}
	if err != nil {
		return l1.Stored{}, false, err
	}
	if !slices.Contains(doc.Scope, entity) || !reader.MayRead(doc.Document) {
		return l1.Stored{}, false, nil
	}
	return doc, true, nil
}

// Entities returns the entities the graph holds under these ids, in the order
// asked for; an id it does not hold is left out.
func (v *Views) Entities(ctx context.Context, ids []string) ([]l2.Entity, error) {
	graph := l2.New(v.db)
	out := make([]l2.Entity, 0, len(ids))
	for _, id := range ids {
		e, err := graph.Entity(ctx, id)
		if errors.Is(err, l2.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}
