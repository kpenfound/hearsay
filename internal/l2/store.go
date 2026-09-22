package l2

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/connector"
)

// Querier is what a Store runs its statements on: a pgx pool, or a transaction
// on one, so that everything one document asserts is written together or not
// at all.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Both of pgx's are one.
var (
	_ Querier = (*pgxpool.Pool)(nil)
	_ Querier = (pgx.Tx)(nil)
)

// Store is the graph's tables. It is safe for concurrent use; it does not
// serialize anything itself — one scope's writes are serialized by the queue
// that runs the assertion worker (ADR-0007), and every read-then-write here
// assumes it.
type Store struct {
	db Querier
}

// New returns a store over a pool or a transaction. The caller owns it.
func New(q Querier) *Store { return &Store{db: q} }

// --- entities ---

const entityColumns = `id, type, name, aliases, path_patterns, part_of, owners, origin`

// PutEntity writes an entity, replacing whatever the table held under its id.
func (s *Store) PutEntity(ctx context.Context, e Entity) error {
	if err := e.Validate(); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `
INSERT INTO l2_entities (`+entityColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (id) DO UPDATE SET
    type = excluded.type, name = excluded.name, aliases = excluded.aliases,
    path_patterns = excluded.path_patterns, part_of = excluded.part_of,
    owners = excluded.owners, origin = excluded.origin, updated_at = now()
WHERE (l2_entities.type, l2_entities.name, l2_entities.aliases, l2_entities.path_patterns,
       l2_entities.part_of, l2_entities.owners, l2_entities.origin)
   IS DISTINCT FROM
      (excluded.type, excluded.name, excluded.aliases, excluded.path_patterns,
       excluded.part_of, excluded.owners, excluded.origin)`,
		e.ID, string(e.Type), e.Name, orEmpty(e.Aliases), orEmpty(e.PathPatterns),
		orEmpty(e.PartOf), orEmpty(e.Owners), string(e.Origin))
	if err != nil {
		return fmt.Errorf("writing entity %s: %w", e.ID, err)
	}
	return nil
}

// EnsureEntity writes an entity only if nothing is stored under its id yet, and
// reports whether it did. It is how a `tracker_item` is created on first
// reference without overwriting an entity a person or a seed put there.
func (s *Store) EnsureEntity(ctx context.Context, e Entity) (bool, error) {
	if err := e.Validate(); err != nil {
		return false, err
	}
	tag, err := s.db.Exec(ctx, `
INSERT INTO l2_entities (`+entityColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (id) DO NOTHING`,
		e.ID, string(e.Type), e.Name, orEmpty(e.Aliases), orEmpty(e.PathPatterns),
		orEmpty(e.PartOf), orEmpty(e.Owners), string(e.Origin))
	if err != nil {
		return false, fmt.Errorf("writing entity %s: %w", e.ID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// ReplaceSeeded makes the seeded entities exactly these: it writes every one of
// them and deletes a `config` or `repo_structure` row that is no longer among
// them, which is what removing an entry from `code/` means. A `reference` row
// is never deleted here — a document pointed at it. Run it in a transaction.
func (s *Store) ReplaceSeeded(ctx context.Context, entities []Entity) error {
	ids := make([]string, 0, len(entities))
	for _, e := range entities {
		if e.Origin == OriginReference {
			return fmt.Errorf("%w: entity %s has origin %s, which seeding never produces", ErrInvalid, e.ID, e.Origin)
		}
		ids = append(ids, e.ID)
	}
	if _, err := s.db.Exec(ctx,
		`DELETE FROM l2_entities WHERE origin IN ('config', 'repo_structure') AND NOT (id = ANY($1))`, ids); err != nil {
		return fmt.Errorf("removing entities configuration no longer names: %w", err)
	}
	for _, e := range entities {
		if err := s.PutEntity(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

// Entity returns one entity by id.
func (s *Store) Entity(ctx context.Context, id string) (Entity, error) {
	e, err := scanEntity(s.db.QueryRow(ctx, `SELECT `+entityColumns+` FROM l2_entities WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Entity{}, fmt.Errorf("%w: entity %s", ErrNotFound, id)
	}
	if err != nil {
		return Entity{}, fmt.Errorf("reading entity %s: %w", id, err)
	}
	return e, nil
}

// Entities returns every entity, sorted by id. The table is a map of how a team
// talks about its code and its tracker: hundreds of rows, not millions.
func (s *Store) Entities(ctx context.Context) ([]Entity, error) {
	rows, err := s.db.Query(ctx, `SELECT `+entityColumns+` FROM l2_entities ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("listing entities: %w", err)
	}
	defer rows.Close()
	out := []Entity{}
	for rows.Next() {
		e, err := scanEntity(rows)
		if err != nil {
			return nil, fmt.Errorf("listing entities: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing entities: %w", err)
	}
	return out, nil
}

// Resolve is [Resolve] over every stored entity: design.md's resolve(text).
func (s *Store) Resolve(ctx context.Context, text string) ([]Match, error) {
	entities, err := s.Entities(ctx)
	if err != nil {
		return nil, err
	}
	return Resolve(entities, text), nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanEntity(row scanner) (Entity, error) {
	var e Entity
	var typ, origin string
	if err := row.Scan(&e.ID, &typ, &e.Name, &e.Aliases, &e.PathPatterns, &e.PartOf, &e.Owners, &origin); err != nil {
		return Entity{}, err
	}
	e.Type, e.Origin = EntityType(typ), Origin(origin)
	return e, nil
}

// --- topics ---

const topicColumns = `t.id, t.scope, t.name, t.about, t.join_keys, t.acl, t.opened_by, t.created_at`

// OpenTopic writes a new topic, and reports whether it did: a topic already
// stored under the id is left as it is, because the id is derived from what
// opened it ([TopicID]) and opening it again is a re-run.
func (s *Store) OpenTopic(ctx context.Context, t Topic) (bool, error) {
	if err := t.Validate(); err != nil {
		return false, err
	}
	acl, err := json.Marshal(t.ACL)
	if err != nil {
		return false, fmt.Errorf("encoding the acl of topic %s: %w", t.ID, err)
	}
	tag, err := s.db.Exec(ctx, `
INSERT INTO l2_topics (id, scope, name, about, join_keys, acl, opened_by)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (id) DO NOTHING`,
		t.ID, t.Scope, t.Name, sortedUnique(orEmpty(t.About)), sortedUnique(orEmpty(t.JoinKeys)), acl, t.OpenedBy)
	if err != nil {
		return false, fmt.Errorf("opening topic %s: %w", t.ID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// ExtendTopic adds entity ids and join keys to a topic: what a document with a
// stance on it is about, and what it can be found by.
func (s *Store) ExtendTopic(ctx context.Context, id string, about, joinKeys []string) error {
	tag, err := s.db.Exec(ctx, `
UPDATE l2_topics SET
    about = ARRAY(SELECT DISTINCT unnest(about || $2::text[]) ORDER BY 1),
    join_keys = ARRAY(SELECT DISTINCT unnest(join_keys || $3::text[]) ORDER BY 1)
WHERE id = $1`, id, orEmpty(about), orEmpty(joinKeys))
	if err != nil {
		return fmt.Errorf("extending topic %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: topic %s", ErrNotFound, id)
	}
	return nil
}

// Topic returns one topic by id.
func (s *Store) Topic(ctx context.Context, id string) (Topic, error) {
	t, err := scanTopic(s.db.QueryRow(ctx, `SELECT `+topicColumns+` FROM l2_topics t WHERE t.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Topic{}, fmt.Errorf("%w: topic %s", ErrNotFound, id)
	}
	if err != nil {
		return Topic{}, fmt.Errorf("reading topic %s: %w", id, err)
	}
	return t, nil
}

// Topics returns every topic in one scope, oldest first.
func (s *Store) Topics(ctx context.Context, scope string) ([]Topic, error) {
	return s.topics(ctx, `SELECT `+topicColumns+` FROM l2_topics t WHERE t.scope = $1 ORDER BY t.created_at, t.id`, scope)
}

// readableBy is the predicate that offers a topic to a document: everyone who
// may read the document may read the topic. That holds for a public topic, and
// for one whose access list carries every grant the document's does — an entry
// is compared on kind, source and native id, never the label, as in
// internal/l1. It is what keeps the name of a private topic out of a prompt
// about a public document, and out of the stance that prompt produces.
const readableBy = `(t.acl @> '[{"kind":"public"}]'::jsonb OR t.acl @> %s::jsonb)`

// TopicsByJoinKeys is the first half of topic matching: the topics in one scope
// that share a join key with a document and that its readers may read, most
// shared keys first, then oldest.
func (s *Store) TopicsByJoinKeys(ctx context.Context, scope string, keys []string, readers connector.ACL, limit int) ([]Topic, error) {
	if len(keys) == 0 || limit <= 0 {
		return []Topic{}, nil
	}
	return s.topics(ctx, `
SELECT `+topicColumns+` FROM l2_topics t
WHERE t.scope = $1 AND t.join_keys && $2::text[] AND `+fmt.Sprintf(readableBy, "$3")+`
ORDER BY cardinality(ARRAY(SELECT unnest(t.join_keys) INTERSECT SELECT unnest($2::text[]))) DESC, t.created_at, t.id
LIMIT $4`, scope, keys, aclJSON(readers), limit)
}

// TopicsBySimilarity is the second half: the topics in one scope whose evidence
// is nearest the document by embedding, within a cosine distance, excluding the
// ones already found. It reads the vectors #50 stores on L1 and makes no model
// call, and it finds nothing for a document that has not been embedded — which
// is every document in a deployment with no `embed` tier.
func (s *Store) TopicsBySimilarity(ctx context.Context, scope, docID string, readers connector.ACL, maxDistance float64, limit int, exclude []string) ([]Topic, error) {
	if limit <= 0 {
		return []Topic{}, nil
	}
	return s.topics(ctx, `
SELECT `+topicColumns+` FROM l2_topics t
JOIN l2_stances s ON s.topic_id = t.id
JOIN l1_docs d ON d.id = s.evidence[1]
CROSS JOIN (SELECT embedding FROM l1_docs WHERE id = $2 AND embedding IS NOT NULL) q
WHERE t.scope = $1 AND d.id <> $2 AND d.embedding IS NOT NULL
  AND NOT (t.id = ANY($5::text[])) AND `+fmt.Sprintf(readableBy, "$6")+`
GROUP BY t.id
HAVING min(d.embedding <=> q.embedding) <= $3
ORDER BY min(d.embedding <=> q.embedding), t.id
LIMIT $4`, scope, docID, maxDistance, limit, orEmpty(exclude), aclJSON(readers))
}

func (s *Store) topics(ctx context.Context, sql string, args ...any) ([]Topic, error) {
	rows, err := s.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("listing topics: %w", err)
	}
	defer rows.Close()
	out := []Topic{}
	for rows.Next() {
		t, err := scanTopic(rows)
		if err != nil {
			return nil, fmt.Errorf("listing topics: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing topics: %w", err)
	}
	return out, nil
}

func scanTopic(row scanner) (Topic, error) {
	var t Topic
	var acl []byte
	if err := row.Scan(&t.ID, &t.Scope, &t.Name, &t.About, &t.JoinKeys, &acl, &t.OpenedBy, &t.CreatedAt); err != nil {
		return Topic{}, err
	}
	if err := json.Unmarshal(acl, &t.ACL); err != nil {
		return Topic{}, fmt.Errorf("decoding the acl of topic %s: %w", t.ID, err)
	}
	t.CreatedAt = t.CreatedAt.UTC()
	return t, nil
}

// aclJSON is an access list as the containment operator takes it, without
// labels: a document is not less readable for having been labelled differently.
func aclJSON(acl connector.ACL) string {
	stripped := make(connector.ACL, len(acl))
	for i, entry := range acl {
		entry.Label = ""
		stripped[i] = entry
	}
	body, err := json.Marshal(stripped)
	if err != nil {
		// Four strings per entry; nothing here fails to encode. An access list
		// that matches no topic is the fail-closed answer if that changes.
		return `[{"kind":"unencodable"}]`
	}
	return string(body)
}

// --- stances ---

const stanceColumns = `id, topic_id, position, author, stated_at, evidence, coalesce(supersedes, ''), tier, acl, created_at`

// RetiredSQL is the predicate, over a stance aliased `s`, that a later reading
// of its own document replaced it: a stance from the same document on the same
// topic supersedes it. A retired stance is never a topic's current one, and
// never a predecessor again. It reads through the topic's index, not the whole
// table. internal/l3 uses it too, so the head the views serve and the head the
// store appends behind are the same stance.
const RetiredSQL = `EXISTS (
    SELECT 1 FROM l2_stances n
    WHERE n.topic_id = s.topic_id AND n.supersedes = s.id AND n.evidence[1] = s.evidence[1])`

// predecessorSQL is the stance a new one supersedes.
//
// A document that already holds a live stance on the topic is being read again
// in a new version, and the new stance replaces that one, whenever either was
// stated: one document holds at most one live stance per topic, and a
// re-distilled document's restatement is recorded as a change to what that
// document said, not as a reply to whatever is newest on the topic.
//
// Otherwise it is the newest live stance on the topic stated no later than the
// new one. A stance stated earlier than the topic's current one — a document
// that was read late — supersedes what came before it and is superseded by
// nothing, so the chain forks rather than a later stance being rewritten to
// point at it. A stance is never overwritten.
const predecessorSQL = `
SELECT s.id FROM l2_stances s
WHERE s.topic_id = $1 AND s.id <> $3 AND (s.evidence[1] = $4 OR s.stated_at <= $2)
  AND NOT ` + RetiredSQL + `
ORDER BY s.evidence[1] = $4 DESC, s.stated_at DESC, s.created_at DESC, s.id DESC
LIMIT 1`

// AppendStance adds a stance to its topic, superseding the one before it — its
// own document's earlier stance on the topic where there is one — and returns
// the stance as stored with whether this call wrote it. A stance's id is
// derived from its topic, document, position, document version and tier
// ([StanceID]). The same reading is not written twice and is returned as first
// stored; a new reading can supersede it even when the position is unchanged.
//
// The predecessor is read and the row written in two statements, which is safe
// only because one scope's writes are serialized (ADR-0007); the caller runs it
// in the transaction that holds the rest of what the document asserted.
func (s *Store) AppendStance(ctx context.Context, st Stance, distilledAt time.Time) (Stance, bool, error) {
	if err := st.Validate(); err != nil {
		return Stance{}, false, err
	}
	if st.Supersedes != "" {
		return Stance{}, false, fmt.Errorf("%w: stance %s names what it supersedes, which the store decides", ErrInvalid, st.ID)
	}
	if distilledAt.IsZero() || st.ID != StanceID(st.TopicID, st.Evidence[0], st.Position, distilledAt, st.Tier) {
		return Stance{}, false, fmt.Errorf("%w: stance %s is not the id derived from its topic, document, position, version and tier", ErrInvalid, st.ID)
	}
	var predecessor *string
	err := s.db.QueryRow(ctx, predecessorSQL, st.TopicID, st.StatedAt, st.ID, st.Evidence[0]).Scan(&predecessor)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Stance{}, false, fmt.Errorf("finding what stance %s supersedes: %w", st.ID, err)
	}
	acl, err := json.Marshal(st.ACL)
	if err != nil {
		return Stance{}, false, fmt.Errorf("encoding the acl of stance %s: %w", st.ID, err)
	}
	tag, err := s.db.Exec(ctx, `
INSERT INTO l2_stances (id, topic_id, position, author, stated_at, evidence, supersedes, tier, acl)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (id) DO NOTHING`,
		st.ID, st.TopicID, st.Position, st.Author, st.StatedAt, st.Evidence, predecessor, string(st.Tier), acl)
	if err != nil {
		return Stance{}, false, fmt.Errorf("appending stance %s: %w", st.ID, err)
	}
	stored, err := s.Stance(ctx, st.ID)
	if err != nil {
		return Stance{}, false, err
	}
	return stored, tag.RowsAffected() == 1, nil
}

// Stance returns one stance by id.
func (s *Store) Stance(ctx context.Context, id string) (Stance, error) {
	return s.stance(ctx, `SELECT `+stanceColumns+` FROM l2_stances WHERE id = $1`, id)
}

func (s *Store) stance(ctx context.Context, sql string, args ...any) (Stance, error) {
	st, err := scanStance(s.db.QueryRow(ctx, sql, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return Stance{}, fmt.Errorf("%w: stance", ErrNotFound)
	}
	if err != nil {
		return Stance{}, fmt.Errorf("reading a stance: %w", err)
	}
	return st, nil
}

// StanceHistory is design.md's stance_history(topic): every stance on a topic,
// in the order they were stated.
func (s *Store) StanceHistory(ctx context.Context, topicID string) ([]Stance, error) {
	return s.stances(ctx, `SELECT `+stanceColumns+` FROM l2_stances WHERE topic_id = $1
ORDER BY stated_at, created_at, id`, topicID)
}

// StancesFrom is every stance read from one document, oldest first.
func (s *Store) StancesFrom(ctx context.Context, docID string) ([]Stance, error) {
	return s.stances(ctx, `SELECT `+stanceColumns+` FROM l2_stances WHERE evidence[1] = $1
ORDER BY stated_at, created_at, id`, docID)
}

func (s *Store) stances(ctx context.Context, sql string, args ...any) ([]Stance, error) {
	rows, err := s.db.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("listing stances: %w", err)
	}
	defer rows.Close()
	out := []Stance{}
	for rows.Next() {
		st, err := scanStance(rows)
		if err != nil {
			return nil, fmt.Errorf("listing stances: %w", err)
		}
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing stances: %w", err)
	}
	return out, nil
}

func scanStance(row scanner) (Stance, error) {
	var st Stance
	var tier string
	var acl []byte
	if err := row.Scan(&st.ID, &st.TopicID, &st.Position, &st.Author, &st.StatedAt, &st.Evidence,
		&st.Supersedes, &tier, &acl, &st.CreatedAt); err != nil {
		return Stance{}, err
	}
	st.Tier = Tier(tier)
	if err := json.Unmarshal(acl, &st.ACL); err != nil {
		return Stance{}, fmt.Errorf("decoding the acl of stance %s: %w", st.ID, err)
	}
	st.StatedAt, st.CreatedAt = st.StatedAt.UTC(), st.CreatedAt.UTC()
	return st, nil
}

// --- what has been read ---

// Asserted reports which version of a document the assertion worker last read:
// the document's distilled_at at the time, and false for one it never read.
func (s *Store) Asserted(ctx context.Context, docID string) (time.Time, bool, error) {
	var at time.Time
	err := s.db.QueryRow(ctx, `SELECT distilled_at FROM l2_asserted WHERE doc_id = $1`, docID).Scan(&at)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return time.Time{}, false, nil
	case err != nil:
		return time.Time{}, false, fmt.Errorf("reading whether %s was asserted: %w", docID, err)
	}
	return at.UTC(), true, nil
}

// MarkAsserted records that a version of a document has been read and how many
// stances it produced. Written in the transaction that wrote the stances, it is
// what lets a re-run skip the model call rather than depend on the model
// answering the same way twice.
func (s *Store) MarkAsserted(ctx context.Context, docID string, distilledAt time.Time, stances int) error {
	_, err := s.db.Exec(ctx, `
INSERT INTO l2_asserted (doc_id, distilled_at, stances) VALUES ($1, $2, $3)
ON CONFLICT (doc_id) DO UPDATE SET distilled_at = excluded.distilled_at,
    stances = excluded.stances, asserted_at = now()`, docID, distilledAt, stances)
	if err != nil {
		return fmt.Errorf("recording that %s was asserted: %w", docID, err)
	}
	return nil
}

// Pending is a document the assertion worker has not read in its current
// version, and the event its artifact is rooted in.
type Pending struct {
	ID string
	// RootEvent is the document's first l0_ref, the artifact's current
	// revision, which is where its container is read from.
	RootEvent string
}

// Unasserted is every document whose outcome enters the assertion pipeline and
// whose current version the worker has not read, oldest activity first. The
// version is compared for equality, never ordered (see the migration).
func (s *Store) Unasserted(ctx context.Context) ([]Pending, error) {
	rows, err := s.db.Query(ctx, `
SELECT d.id, d.l0_refs[1] FROM l1_docs d
LEFT JOIN l2_asserted a ON a.doc_id = d.id
WHERE d.outcome_kind IN ('decided', 'proposed', 'resolved')
  AND (a.doc_id IS NULL OR a.distilled_at <> d.distilled_at)
ORDER BY d.last_activity_at, d.id`)
	if err != nil {
		return nil, fmt.Errorf("listing unasserted documents: %w", err)
	}
	defer rows.Close()
	out := []Pending{}
	for rows.Next() {
		var p Pending
		if err := rows.Scan(&p.ID, &p.RootEvent); err != nil {
			return nil, fmt.Errorf("listing unasserted documents: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing unasserted documents: %w", err)
	}
	return out, nil
}

// orEmpty keeps a nil slice out of a NOT NULL array column: pgx writes one as
// NULL.
func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
