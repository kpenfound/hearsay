package l1

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

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
// filter [Store.Search] and [Store.ListFor] apply in SQL — a scope the reader was
// granted, and an access list that allows them.
func (r Reader) MayRead(doc Document) bool {
	if !r.Allows(doc.ACL) {
		return false
	}
	scopes := r.Effective.Grant.Scopes
	if scopes.All {
		return true
	}
	return slices.ContainsFunc(doc.Scope, scopes.Has)
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
