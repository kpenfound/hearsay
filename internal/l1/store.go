package l1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Limits on how much one read returns.
const (
	// DefaultLimit is what a read with no limit returns.
	DefaultLimit = 100
	// MaxLimit is the most any read returns.
	MaxLimit = 1000
)

// Store is the L1 document table: what the distiller writes and what search,
// the assertion worker and bundle assembly read.
//
// It is safe for concurrent use, and every operation is one statement, so
// nothing here holds a transaction open.
type Store struct {
	db Querier
}

// Querier is what a Store runs its statements on: a pgx pool, or a transaction
// taken from one, so that writing a document and enqueueing the work that
// follows from it can be one transaction (ADR-0007).
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Both of pgx's are one.
var (
	_ Querier = (*pgxpool.Pool)(nil)
	_ Querier = (pgx.Tx)(nil)
)

// New returns a store over an open pool or a transaction on one. The caller
// owns it and closes it; the schema is the caller's to have checked
// (internal/db.Connect).
func New(q Querier) *Store { return &Store{db: q} }

// Stored is a document as the table holds it.
type Stored struct {
	Document
	// DistilledAt is when this row was last written. It does not move when a
	// re-distillation produces the document that is already there, which is
	// how "distilling the same artifact twice produces identical rows" is
	// checked rather than asserted.
	DistilledAt time.Time
}

// docColumns is a document as columns, in the order scanDoc reads them.
const docColumns = `id, kind, source, source_native_id, source_url, l0_refs,
	created_at, updated_at, last_activity_at, participants, scope, refs, acl,
	text, raw_text, body, outcome_kind, distilled_at`

// putSQL writes a document, and does nothing at all when the row already there
// is the same document.
//
// The WHERE on the update is what makes re-distilling free and checkable: the
// distiller is idempotent, so the ordinary outcome of running it again is a row
// that does not change, and a row that does not change keeps its distilled_at.
// Comparing the columns rather than the whole row is deliberate — comparing the
// whole row would compare distilled_at with now() and always differ.
//
// The comparison is Postgres's own, so the jsonb columns compare by value
// rather than by the bytes Go happened to marshal.
//
// The embedding is not one of the columns compared, because it is not part of
// the document: it is derived from `text`, and it is cleared here exactly when
// `text` changes.
const putSQL = `
INSERT INTO l1_docs (` + docColumns + `)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, now())
ON CONFLICT (id) DO UPDATE SET
    kind = excluded.kind,
    source = excluded.source,
    source_native_id = excluded.source_native_id,
    source_url = excluded.source_url,
    l0_refs = excluded.l0_refs,
    created_at = excluded.created_at,
    updated_at = excluded.updated_at,
    last_activity_at = excluded.last_activity_at,
    participants = excluded.participants,
    scope = excluded.scope,
    refs = excluded.refs,
    acl = excluded.acl,
    text = excluded.text,
    raw_text = excluded.raw_text,
    body = excluded.body,
    outcome_kind = excluded.outcome_kind,
    -- A vector is a function of the text it was made from, so text that
    -- changes takes its embedding with it rather than leaving a stale one
    -- standing: search's vector half would otherwise rank this document by
    -- words it no longer holds. NULL is "not embedded yet", which is what
    -- l1.Store.Embed looks for and what the full-text half is unaffected by.
    embedding = CASE WHEN l1_docs.text IS DISTINCT FROM excluded.text THEN NULL ELSE l1_docs.embedding END,
    distilled_at = now()
WHERE (l1_docs.kind, l1_docs.source, l1_docs.source_native_id, l1_docs.source_url,
       l1_docs.l0_refs, l1_docs.created_at, l1_docs.updated_at, l1_docs.last_activity_at,
       l1_docs.participants, l1_docs.scope, l1_docs.refs, l1_docs.acl,
       l1_docs.text, l1_docs.raw_text, l1_docs.body, l1_docs.outcome_kind)
   IS DISTINCT FROM
      (excluded.kind, excluded.source, excluded.source_native_id, excluded.source_url,
       excluded.l0_refs, excluded.created_at, excluded.updated_at, excluded.last_activity_at,
       excluded.participants, excluded.scope, excluded.refs, excluded.acl,
       excluded.text, excluded.raw_text, excluded.body, excluded.outcome_kind)
RETURNING distilled_at`

// Put writes a document, and reports whether that changed anything. False means
// the row already held exactly this document and nothing was written, which is
// the ordinary outcome of re-distilling an artifact nothing has happened to.
//
// A document that does not satisfy [Document.Validate] is refused before any
// SQL runs, so the table holds only documents the layer allows.
func (s *Store) Put(ctx context.Context, doc Document) (bool, error) {
	if err := doc.Validate(); err != nil {
		return false, err
	}
	row, err := rowOf(doc)
	if err != nil {
		return false, err
	}
	var distilledAt time.Time
	err = s.db.QueryRow(ctx, putSQL,
		doc.ID, string(doc.Kind), doc.Source.System, doc.Source.NativeID, doc.Source.URL,
		doc.L0Refs, doc.Time.Created, doc.Time.Updated, doc.Time.LastActivity,
		row.participants, orEmpty(doc.Scope), row.references, row.acl,
		doc.Text, doc.RawText, row.body, string(doc.Body.OutcomeKind),
	).Scan(&distilledAt)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("writing document %s: %w", doc.ID, err)
	}
	return true, nil
}

// Get returns one document by id, and [ErrNotFound] for an id the table does
// not hold.
func (s *Store) Get(ctx context.Context, id string) (Stored, error) {
	row := s.db.QueryRow(ctx, `SELECT `+docColumns+` FROM l1_docs WHERE id = $1`, id)
	doc, err := scanDoc(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Stored{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return Stored{}, fmt.Errorf("reading document %s: %w", id, err)
	}
	return doc, nil
}

// Delete removes a document, and reports whether there was one. It is how an
// artifact that has been retracted at the source leaves L1: the events stay in
// L0 behind a tombstone, and the document derived from them goes, because a
// document is derived and a retracted artifact derives nothing
// (docs/design.md#deletion-and-provenance).
func (s *Store) Delete(ctx context.Context, id string) (bool, error) {
	var deleted string
	err := s.db.QueryRow(ctx, `DELETE FROM l1_docs WHERE id = $1 RETURNING id`, id).Scan(&deleted)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("deleting document %s: %w", id, err)
	}
	return true, nil
}

// ListOptions narrows a listing. The zero value is the whole table, newest
// activity first, up to [DefaultLimit] documents.
type ListOptions struct {
	// Source reads one configured source only.
	Source string
	// Kind reads one document kind only.
	Kind Kind
	// OutcomeKind reads the documents that concluded one sort of thing. The
	// assertion worker's read is this, three times over — or once, with
	// Asserting.
	OutcomeKind OutcomeKind
	// Asserting reads only the documents that enter the assertion pipeline:
	// decided, proposed and resolved (docs/design.md#l1-distilled-documents).
	Asserting bool
	// Scope reads only the documents about one entity.
	Scope string
	// Limit is how many documents to return, capped at [MaxLimit].
	Limit int
	// Oldest reverses the order, which is what re-deriving everything wants:
	// oldest activity first, so a rebuild follows the order things happened.
	Oldest bool
}

// List returns the documents an option set selects, newest activity first.
func (s *Store) List(ctx context.Context, opts ListOptions) ([]Stored, error) {
	if opts.OutcomeKind != "" && !opts.OutcomeKind.Valid() {
		return nil, fmt.Errorf("%w: outcome kind %q", ErrInvalidDocument, opts.OutcomeKind)
	}
	q := &query{sql: `SELECT ` + docColumns + ` FROM l1_docs WHERE true`}
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
	if opts.Scope != "" {
		q.sql += " AND scope @> ARRAY[" + q.placeholder(opts.Scope) + "]::text[]"
	}
	if opts.Oldest {
		q.sql += " ORDER BY last_activity_at ASC, id ASC"
	} else {
		q.sql += " ORDER BY last_activity_at DESC, id DESC"
	}
	q.sql += " LIMIT " + q.placeholder(int64(Limit(opts.Limit)))

	rows, err := s.db.Query(ctx, q.sql, q.args...)
	if err != nil {
		return nil, fmt.Errorf("listing documents: %w", err)
	}
	defer rows.Close()

	docs := []Stored{}
	for rows.Next() {
		doc, err := scanDoc(rows)
		if err != nil {
			return nil, fmt.Errorf("listing documents: %w", err)
		}
		docs = append(docs, doc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing documents: %w", err)
	}
	return docs, nil
}

// Limit is how many documents a read with this limit actually returns: the
// default when none was asked for, the cap when too many were.
func Limit(limit int) int {
	switch {
	case limit <= 0:
		return DefaultLimit
	case limit > MaxLimit:
		return MaxLimit
	default:
		return limit
	}
}

// row is the parts of a document that are stored as JSON.
type row struct {
	participants []byte
	references   []byte
	acl          []byte
	body         []byte
}

func rowOf(doc Document) (row, error) {
	var r row
	var err error
	// A nil slice marshals as `null`, and the column holds an array: encoding
	// an empty document's participants as null would fail the constraint that
	// says these columns are arrays.
	if r.participants, err = json.Marshal(orEmpty(doc.Participants)); err != nil {
		return row{}, fmt.Errorf("encoding the participants of %s: %w", doc.ID, err)
	}
	if r.references, err = json.Marshal(orEmpty(doc.References)); err != nil {
		return row{}, fmt.Errorf("encoding the references of %s: %w", doc.ID, err)
	}
	if r.acl, err = json.Marshal(doc.ACL); err != nil {
		return row{}, fmt.Errorf("encoding the acl of %s: %w", doc.ID, err)
	}
	if r.body, err = json.Marshal(doc.Body); err != nil {
		return row{}, fmt.Errorf("encoding the body of %s: %w", doc.ID, err)
	}
	return r, nil
}

// orEmpty is what keeps a nil slice out of a NOT NULL column: pgx writes one as
// NULL, and a jsonb column here holds an array while a text[] holds a list.
func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// scanner is what both reads hand to scanDoc: a single row or one of many.
type scanner interface {
	Scan(dest ...any) error
}

// scanDoc reads one row back into a document.
func scanDoc(s scanner) (Stored, error) {
	var d docScan
	if err := s.Scan(d.dests()...); err != nil {
		return Stored{}, err
	}
	return d.done()
}

// docScan is one row of [docColumns] as Postgres hands it over: the columns
// that map straight onto a field, and the ones that have to be decoded.
//
// It is split from [scanDoc] because a read that selects more than a document —
// search, which returns the ranks a document was found at — has to scan the
// document's columns and its own in one call.
type docScan struct {
	doc          Stored
	kind         string
	outcome      string
	participants []byte
	references   []byte
	acl          []byte
	body         []byte
}

// dests is where each of [docColumns] is read into, in that order.
func (d *docScan) dests() []any {
	return []any{&d.doc.ID, &d.kind, &d.doc.Source.System, &d.doc.Source.NativeID, &d.doc.Source.URL,
		&d.doc.L0Refs, &d.doc.Time.Created, &d.doc.Time.Updated, &d.doc.Time.LastActivity,
		&d.participants, &d.doc.Scope, &d.references, &d.acl,
		&d.doc.Text, &d.doc.RawText, &d.body, &d.outcome, &d.doc.DistilledAt}
}

// done turns a scanned row into a document.
//
// The times come back in UTC. Postgres hands a timestamptz back in the
// session's time zone, which is the server's and has nothing to do with the
// artifact; the instant is the same either way, and reading it in one zone is
// what keeps a document that was written once from looking different when it is
// read somewhere else.
func (d *docScan) done() (Stored, error) {
	doc := d.doc
	doc.Kind = Kind(d.kind)
	doc.Time.Created = doc.Time.Created.UTC()
	doc.Time.Updated = doc.Time.Updated.UTC()
	doc.Time.LastActivity = doc.Time.LastActivity.UTC()
	doc.DistilledAt = doc.DistilledAt.UTC()
	if err := json.Unmarshal(d.participants, &doc.Participants); err != nil {
		return Stored{}, fmt.Errorf("decoding the participants of %s: %w", doc.ID, err)
	}
	if err := json.Unmarshal(d.references, &doc.References); err != nil {
		return Stored{}, fmt.Errorf("decoding the references of %s: %w", doc.ID, err)
	}
	if err := json.Unmarshal(d.acl, &doc.ACL); err != nil {
		return Stored{}, fmt.Errorf("decoding the acl of %s: %w", doc.ID, err)
	}
	if err := json.Unmarshal(d.body, &doc.Body); err != nil {
		return Stored{}, fmt.Errorf("decoding the body of %s: %w", doc.ID, err)
	}
	if d.outcome != string(doc.Body.OutcomeKind) {
		// The column is a copy of a field of the body, held to it by the write
		// path. A row where they disagree was written by something else, and
		// the L2 trigger reads the column while everything else reads the body.
		return Stored{}, fmt.Errorf("document %s: outcome_kind is %q and body.outcome_kind is %q", doc.ID, d.outcome, doc.Body.OutcomeKind)
	}
	if doc.Participants == nil {
		doc.Participants = []Participant{}
	}
	if doc.References == nil {
		doc.References = []Reference{}
	}
	return doc, nil
}

// query builds a statement with numbered placeholders, so that a filter nobody
// set contributes no predicate.
type query struct {
	sql  string
	args []any
}

func (q *query) placeholder(arg any) string {
	q.args = append(q.args, arg)
	return "$" + strconv.Itoa(len(q.args))
}

func (q *query) and(column string, arg any) {
	q.sql += " AND " + column + " = " + q.placeholder(arg)
}
