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
	db   Querier
	docs *l1.Store
}

// New returns the views over a pool or a transaction. The caller owns it.
func New(q Querier) *Views { return &Views{db: q, docs: l1.New(q)} }

// CurrentStance is the stance a topic stands at now, for a reader.
type CurrentStance struct {
	Topic  l2.Topic
	Stance l2.Stance
	// Supersedes is the position the stance replaced, empty for the first
	// stance on a topic and for one the reader may not read.
	Supersedes string
	// Inherited reports a topic about none of the entities asked for, only
	// about an ancestor of one of them (docs/design.md#l3-derived-views).
	Inherited bool
}

// currentSQL is every topic about one of the entities or an ancestor of one,
// with the stance it stands at: the newest stated, which is the head of the
// supersession chain — a document read late forks the chain behind the head
// and never replaces it (internal/l2). The walk up `part_of` is a UNION, so a
// cycle ends it rather than looping.
const currentSQL = `
WITH RECURSIVE up(id) AS (
    SELECT unnest($1::text[])
    UNION
    SELECT unnest(e.part_of) FROM l2_entities e JOIN up ON e.id = up.id
)
SELECT t.id, t.scope, t.name, t.about, t.acl, t.opened_by, t.created_at,
       s.id, s.position, s.author, s.stated_at, s.evidence, coalesce(s.supersedes, ''), s.tier, s.acl, s.created_at,
       coalesce(p.position, ''), coalesce(p.acl, '[]'::jsonb)
FROM l2_topics t
JOIN LATERAL (
    SELECT * FROM l2_stances WHERE topic_id = t.id
    ORDER BY stated_at DESC, created_at DESC, id DESC LIMIT 1
) s ON true
LEFT JOIN l2_stances p ON p.id = s.supersedes
WHERE t.about && ARRAY(SELECT id FROM up)
ORDER BY s.stated_at DESC, t.id`

// CurrentStances is the stance every topic about these entities stands at, and
// the ones inherited from their ancestors, tagged as inherited — newest first.
//
// A topic the reader may not read, or whose current stance they may not read,
// is left out and counted in the second result. The older stance they may read
// is not offered in its place: it is not current, and a bundle that said it was
// would be wrong in a way the reader could not see.
func (v *Views) CurrentStances(ctx context.Context, reader l1.Reader, entities []string) ([]CurrentStance, int, error) {
	if len(entities) == 0 {
		return []CurrentStance{}, 0, nil
	}
	rows, err := v.db.Query(ctx, currentSQL, entities)
	if err != nil {
		return nil, 0, fmt.Errorf("reading current stances: %w", err)
	}
	defer rows.Close()
	out := []CurrentStance{}
	withheld := 0
	for rows.Next() {
		var (
			c                          CurrentStance
			tier                       string
			topicACL, stanceACL, prior []byte
		)
		if err := rows.Scan(&c.Topic.ID, &c.Topic.Scope, &c.Topic.Name, &c.Topic.About, &topicACL, &c.Topic.OpenedBy, &c.Topic.CreatedAt,
			&c.Stance.ID, &c.Stance.Position, &c.Stance.Author, &c.Stance.StatedAt, &c.Stance.Evidence, &c.Stance.Supersedes,
			&tier, &stanceACL, &c.Stance.CreatedAt, &c.Supersedes, &prior); err != nil {
			return nil, 0, fmt.Errorf("reading current stances: %w", err)
		}
		c.Stance.TopicID, c.Stance.Tier = c.Topic.ID, l2.Tier(tier)
		c.Topic.CreatedAt = c.Topic.CreatedAt.UTC()
		c.Stance.StatedAt, c.Stance.CreatedAt = c.Stance.StatedAt.UTC(), c.Stance.CreatedAt.UTC()
		var priorACL connector.ACL
		if err := errors.Join(
			json.Unmarshal(topicACL, &c.Topic.ACL),
			json.Unmarshal(stanceACL, &c.Stance.ACL),
			json.Unmarshal(prior, &priorACL),
		); err != nil {
			return nil, 0, fmt.Errorf("decoding the access lists of topic %s: %w", c.Topic.ID, err)
		}
		if !reader.Allows(c.Topic.ACL) || !reader.Allows(c.Stance.ACL) {
			withheld++
			continue
		}
		if !reader.Allows(priorACL) {
			c.Supersedes = ""
		}
		c.Inherited = !slices.ContainsFunc(c.Topic.About, func(id string) bool { return slices.Contains(entities, id) })
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("reading current stances: %w", err)
	}
	return out, withheld, nil
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
func (v *Views) Recent(ctx context.Context, reader l1.Reader, scope string, n int) (Activity, error) {
	if n <= 0 {
		return Activity{Items: []l1.Stored{}}, nil
	}
	candidates, err := v.docs.ListFor(ctx, reader, l1.ListOptions{Scope: scope, Limit: RecentCandidates})
	if err != nil {
		return Activity{}, err
	}
	out := Activity{Items: []l1.Stored{}}
	if len(candidates) == 0 {
		return out, nil
	}
	out.LastActivity = candidates[0].Time.LastActivity

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
