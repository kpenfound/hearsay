package l1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/kpenfound/hearsay/internal/connector"
)

// Allows reports whether this reader may read something carrying this access
// list: somebody is accountable for the read, and the list says `public` or
// holds an entry the reader satisfies. Entries are compared on kind, source and
// native id, never the label, the way the SQL filter compares them.
//
// It is the access-list half of the filter in Go, for the objects that are not
// rows of this table — a stance, a topic, an event — and it fails closed the
// same way: the zero reader is allowed nothing, and an empty list allows nobody.
func (r Reader) Allows(acl connector.ACL) bool {
	if r.Effective.Human == "" {
		return false
	}
	for _, entry := range acl {
		if entry.Kind == connector.ACLPublic {
			return true
		}
		if slices.ContainsFunc(r.Audience, func(a connector.ACLEntry) bool {
			return a.Kind == entry.Kind && a.Source == entry.Source && a.NativeID == entry.NativeID
		}) {
			return true
		}
	}
	return false
}

// MayRead reports whether this reader may read a document: both halves of the
// filter [Store.Search] and [Store.ListFor] apply in SQL — a document in the
// reader's reach, and an access list that allows them.
func (r Reader) MayRead(doc Document) bool {
	return r.Allows(doc.ACL) && r.InReach(doc.Scope)
}

// InReach reports whether something about these entities is in this reader's
// reach (docs/design.md#access-control): the reach is every entity, or holds
// one of them. Something about no entity is in reach only of a reach that is
// every entity, and the zero reader reaches nothing.
//
// It is the reach half of the filter in Go, as [Reader.Allows] is the
// access-list half: a document by its scope, an entity by its id, a topic by
// what it is about.
func (r Reader) InReach(about []string) bool {
	if r.Effective.Human == "" {
		return false
	}
	scopes := r.Effective.Grant.Scopes
	return scopes.All || slices.ContainsFunc(about, scopes.Has)
}

// Guard is what decides who may read one document: its access list, and the
// entities it is about, which decide whose reach it is in.
type Guard struct {
	ACL   connector.ACL
	Scope []string
}

// Guards is the current [Guard] of every one of these documents the table
// still holds, by id. A document that was never stored, or that left L1, has
// none, and whoever reads the map fails closed on it, as with [Store.ACLs].
func (s *Store) Guards(ctx context.Context, ids []string) (map[string]Guard, error) {
	out := make(map[string]Guard, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `SELECT id, acl, scope FROM l1_docs WHERE id = ANY($1::text[])`, ids)
	if err != nil {
		return nil, fmt.Errorf("reading the guards of %d documents: %w", len(ids), err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id  string
			raw []byte
			g   Guard
		)
		if err := rows.Scan(&id, &raw, &g.Scope); err != nil {
			return nil, fmt.Errorf("reading the guards of %d documents: %w", len(ids), err)
		}
		if err := json.Unmarshal(raw, &g.ACL); err != nil {
			return nil, fmt.Errorf("decoding the access list of %s: %w", id, err)
		}
		out[id] = g
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the guards of %d documents: %w", len(ids), err)
	}
	return out, nil
}

// artifactScopesSQL is the scope of every document built from any revision of
// one artifact: its own document, a conversation's that holds it, or a
// section's of it.
const artifactScopesSQL = `
SELECT scope FROM l1_docs
WHERE source = $1 AND l0_refs && ARRAY(SELECT id FROM l0_events WHERE source = $1 AND artifact = $2)`

// ArtifactInReach reports whether an L0 artifact is in this reader's reach:
// a document built from it is (docs/design.md#access-control). An artifact no
// document was built from — not distilled yet, or never distilled — is in
// reach only of a reach that is every entity. The access list is not
// consulted: the event's own is the caller's to check.
func (s *Store) ArtifactInReach(ctx context.Context, reader Reader, source, artifact string) (bool, error) {
	if reader.Effective.Human == "" {
		return false, nil
	}
	if reader.Effective.Grant.Scopes.All {
		return true, nil
	}
	rows, err := s.db.Query(ctx, artifactScopesSQL, source, artifact)
	if err != nil {
		return false, fmt.Errorf("reading the documents built from an artifact of %s: %w", source, err)
	}
	defer rows.Close()
	in := false
	for rows.Next() {
		var scope []string
		if err := rows.Scan(&scope); err != nil {
			return false, fmt.Errorf("reading the documents built from an artifact of %s: %w", source, err)
		}
		in = in || reader.InReach(scope)
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("reading the documents built from an artifact of %s: %w", source, err)
	}
	return in, nil
}

// ACLs is the current access list of every one of these documents the table
// still holds, by id. A document that was never stored, or that left L1 because
// its artifact was retracted, has no entry, and whoever reads the map fails
// closed on it: what derives from a document is readable no longer than the
// document is.
func (s *Store) ACLs(ctx context.Context, ids []string) (map[string]connector.ACL, error) {
	out := make(map[string]connector.ACL, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `SELECT id, acl FROM l1_docs WHERE id = ANY($1::text[])`, ids)
	if err != nil {
		return nil, fmt.Errorf("reading the access lists of %d documents: %w", len(ids), err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id  string
			raw []byte
			acl connector.ACL
		)
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, fmt.Errorf("reading the access lists of %d documents: %w", len(ids), err)
		}
		if err := json.Unmarshal(raw, &acl); err != nil {
			return nil, fmt.Errorf("decoding the access list of %s: %w", id, err)
		}
		out[id] = acl
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the access lists of %d documents: %w", len(ids), err)
	}
	return out, nil
}

// ListFor is [Store.List] for a reader: only the documents they may read, filtered
// before the limit is applied, so a document they may not read never takes one
// of the places a listing returns. A reader who may read nothing gets nothing
// and no query runs.
func (s *Store) ListFor(ctx context.Context, reader Reader, opts ListOptions) ([]Stored, error) {
	if opts.OutcomeKind != "" && !opts.OutcomeKind.Valid() {
		return nil, fmt.Errorf("%w: outcome kind %q", ErrInvalidDocument, opts.OutcomeKind)
	}
	if opts.Class != "" && !opts.Class.Valid() {
		return nil, fmt.Errorf("%w: artifact class %q", ErrInvalidDocument, opts.Class)
	}
	q := &query{}
	filter, ok := reader.predicate(q, opts.Scope)
	if !ok {
		return []Stored{}, nil
	}
	q.sql = `SELECT ` + docColumns + ` FROM l1_docs WHERE ` + filter
	if opts.Source != "" {
		q.and("source", opts.Source)
	}
	if opts.Kind != "" {
		q.and("kind", string(opts.Kind))
	}
	if opts.Class != "" {
		q.and("artifact_class", string(opts.Class))
	}
	if opts.OutcomeKind != "" {
		q.and("outcome_kind", string(opts.OutcomeKind))
	}
	if opts.Asserting {
		q.sql += " AND outcome_kind IN ('decided', 'proposed', 'resolved')"
	}
	if opts.OpenQuestions {
		q.sql += " AND jsonb_array_length(coalesce(body->'open_questions', '[]'::jsonb)) > 0"
	}
	if opts.Oldest {
		q.sql += " ORDER BY last_activity_at ASC, id ASC"
	} else {
		q.sql += " ORDER BY last_activity_at DESC, id DESC"
	}
	q.sql += " LIMIT " + q.placeholder(int64(Limit(opts.Limit)))

	rows, err := s.db.Query(ctx, q.sql, q.args...)
	if err != nil {
		return nil, fmt.Errorf("listing readable documents: %w", err)
	}
	defer rows.Close()
	docs := []Stored{}
	for rows.Next() {
		doc, err := scanDoc(rows)
		if err != nil {
			return nil, fmt.Errorf("listing readable documents: %w", err)
		}
		docs = append(docs, doc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing readable documents: %w", err)
	}
	return docs, nil
}

// mostReferencedSQL ranks the documents about one entity by how many other
// documents about it point at them, both sides filtered by the reader first.
// A reference points at a document when it names what the document is: an
// issue, a pull request or a bare tracker item names an issue or pull request
// by its artifact id, a commit a commit by its own, and a link a document by
// its permalink. The ids are compared without the source, as L2's join keys
// compare them: a chat that links an issue is in another source than the issue.
const mostReferencedSQL = `
WITH readable AS (
    SELECT id, kind, source_native_id, source_url, refs FROM l1_docs WHERE %s
), pointed AS (
    SELECT DISTINCT r.id AS referrer, ref->>'type' AS type, ref->>'id' AS target
    FROM readable r, jsonb_array_elements(r.refs) AS ref
), ranked AS (
    SELECT t.id AS doc, count(DISTINCT p.referrer) AS pointers
    FROM readable t JOIN pointed p ON p.referrer <> t.id AND (
           (t.kind IN ('issue', 'pr') AND p.type IN ('issue', 'pr', 'tracker_item') AND p.target = t.source_native_id)
        OR (t.kind = 'commit' AND p.type = 'commit' AND p.target = t.source_native_id)
        OR (p.type = 'url' AND t.source_url <> '' AND p.target = t.source_url))
    GROUP BY t.id
    ORDER BY pointers DESC, t.id ASC
    LIMIT 1
)
SELECT %s, ranked.pointers FROM l1_docs JOIN ranked ON l1_docs.id = ranked.doc`

// MostReferencedFor is the document about an entity that the most other
// documents about it point at, counting only documents this reader may read on
// either end: one they may not read neither wins nor counts towards another
// winning. It returns how many point at it, and false where no readable
// document is pointed at by another. A tie goes to the lower document id, so
// the same graph and the same reader always give the same document.
func (s *Store) MostReferencedFor(ctx context.Context, reader Reader, scope string) (Stored, int, bool, error) {
	if scope == "" {
		return Stored{}, 0, false, fmt.Errorf("%w: the most referenced document is read for one entity", ErrInvalidDocument)
	}
	q := &query{}
	filter, ok := reader.predicate(q, scope)
	if !ok {
		return Stored{}, 0, false, nil
	}
	var d docScan
	var pointers int
	err := s.db.QueryRow(ctx, fmt.Sprintf(mostReferencedSQL, filter, docColumns), q.args...).Scan(append(d.dests(), &pointers)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Stored{}, 0, false, nil
	}
	if err != nil {
		return Stored{}, 0, false, fmt.Errorf("reading the most referenced document about %s: %w", scope, err)
	}
	doc, err := d.done()
	if err != nil {
		return Stored{}, 0, false, fmt.Errorf("reading the most referenced document about %s: %w", scope, err)
	}
	return doc, pointers, true, nil
}

// About is how many documents are about one entity, whoever may read them. It
// is what an audit record says reach withheld when the entity itself is out of
// the reader's reach.
func (s *Store) About(ctx context.Context, scope string) (int, error) {
	var n int
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM l1_docs WHERE scope @> ARRAY[$1]::text[]`, scope).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting documents about an entity: %w", err)
	}
	return n, nil
}

// Withheld is how many documents about one entity this reader may not read. It
// is what an audit record says was filtered, and it is a count and nothing
// else: which documents they were is exactly what the reader may not learn.
func (s *Store) Withheld(ctx context.Context, reader Reader, scope string) (int, error) {
	q := &query{}
	q.sql = `SELECT count(*) FROM l1_docs WHERE scope @> ARRAY[` + q.placeholder(scope) + `]::text[]`
	if filter, ok := reader.predicate(q, scope); ok {
		q.sql += ` AND NOT (` + filter + `)`
	}
	var n int
	if err := s.db.QueryRow(ctx, q.sql, q.args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting withheld documents: %w", err)
	}
	return n, nil
}
