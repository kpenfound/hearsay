package l0

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/connector"
)

// The errors a caller distinguishes. Everything else a Store returns is an IO
// error or a rejected event, and [connector.ErrInvalidEvent] is what the
// latter wraps.
var (
	// ErrNotFound is returned for an event id L0 has never seen.
	ErrNotFound = errors.New("no such event")

	// ErrRetracted is returned for an event a tombstone covers. It is distinct
	// from ErrNotFound because a provenance walk arriving at deleted evidence
	// has learned something, and "unknown id" would say the opposite
	// (docs/design.md#deletion-and-provenance).
	ErrRetracted = errors.New("the event has been retracted by a tombstone")

	// ErrRewrite is returned when an event id already in L0 is re-emitted with
	// different content. L0 is append-only, so the stored row wins and nothing
	// is written. It means the source changed an artifact without changing the
	// revision token in its native id, which docs/connector-contract.md forbids
	// — the token changes whenever the payload or the ACL does — so it is a
	// connector to fix rather than a state to tolerate.
	ErrRewrite = errors.New("the event id already holds different content")
)

// Limits on how much one read returns.
const (
	// DefaultLimit is what a read with no limit returns.
	DefaultLimit = 100
	// MaxLimit is the most any read returns, so that a caller asking for
	// everything gets a page instead of the table.
	MaxLimit = 1000
)

// Store is the L0 event store: the append-only table every connector writes to
// and everything above L0 reads from. It implements [connector.Sink], so a
// connector's Emit lands here.
//
// It is safe for concurrent use. Every write is one statement, so nothing here
// holds a transaction open — which matters, because the change feed waits
// behind long-running write transactions rather than reading past them.
type Store struct {
	db Querier
}

// Querier is what a Store runs its statements on: a pgx pool, or a transaction
// taken from one. It is an interface so that an event can be appended in the
// same transaction as whatever else a caller is doing — enqueuing the
// distillation job for it, in particular (ADR-0007).
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

// Appended is what one write did.
type Appended struct {
	// ID is the event id, derived from the source and native id.
	ID string
	// Stored reports that this call wrote the row. False means the event was
	// already in L0 with exactly this content and nothing was written, which is
	// the ordinary outcome of a re-poll.
	Stored bool
	// Cursor is where the row sits in the change feed, whether or not this call
	// wrote it.
	Cursor Cursor
}

// Emit implements [connector.Sink]: it appends the event and reports only
// whether that worked, because a connector has no use for the difference
// between a write and a replay.
func (s *Store) Emit(ctx context.Context, ev connector.Event) error {
	_, err := s.Append(ctx, ev)
	return err
}

// Append writes one event, and is idempotent on the event id: the id is derived
// from the source and the native id (connector.EventID), an artifact that
// changed carries a new revision token in its native id, and so re-emitting an
// unchanged artifact writes nothing and re-emitting an edited one writes a new
// row. Nothing is ever updated.
//
// An event that does not satisfy [connector.Event.Validate] is rejected before
// any SQL runs, so the table holds only events the contract allows. An event id
// already present with different content returns [ErrRewrite].
func (s *Store) Append(ctx context.Context, ev connector.Event) (Appended, error) {
	if err := ev.Validate(); err != nil {
		return Appended{}, err
	}
	row, err := rowOf(ev)
	if err != nil {
		return Appended{}, err
	}

	// Two attempts, because of one race. When a concurrent transaction inserts
	// the same id first, ON CONFLICT DO NOTHING waits for it, then returns
	// nothing — and the read half of this statement runs on a snapshot taken
	// before that commit, so it does not see the row either and the statement
	// returns no rows at all. The second attempt gets a fresh snapshot and
	// finds it.
	for attempt := range 2 {
		var (
			xact   string
			seq    int64
			stored bool
			same   bool
		)
		err := s.db.QueryRow(ctx, appendSQL,
			row.id, row.source, row.nativeID, string(row.kind), row.artifact,
			row.revisionToken, row.revisionEditedAt, row.target,
			row.occurredAt, row.payload, row.acl,
		).Scan(&xact, &seq, &stored, &same)
		if errors.Is(err, pgx.ErrNoRows) {
			if attempt == 0 {
				continue
			}
			return Appended{}, fmt.Errorf("appending event %s: the row neither inserted nor read back", row.id)
		}
		if err != nil {
			return Appended{}, fmt.Errorf("appending event %s: %w", row.id, err)
		}
		if !stored && !same {
			return Appended{}, fmt.Errorf("%w: %s", ErrRewrite, row.id)
		}
		cursor, err := cursorOf(xact, seq)
		if err != nil {
			return Appended{}, fmt.Errorf("appending event %s: %w", row.id, err)
		}
		return Appended{ID: row.id, Stored: stored, Cursor: cursor}, nil
	}
	return Appended{}, fmt.Errorf("appending event %s: gave up after a concurrent write of the same id", row.id)
}

// appendSQL inserts the event, or reads back the row that is already there and
// says whether it holds the same content. The comparison is Postgres's own
// jsonb equality rather than a hash of the bytes a connector marshalled,
// because jsonb ignores key order and whitespace: two spellings of one payload
// are a replay, not a rewrite.
const appendSQL = `
WITH inserted AS (
    INSERT INTO l0_events (
        id, source, native_id, kind, artifact,
        revision_token, revision_edited_at, target,
        occurred_at, payload, acl
    )
    VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
    ON CONFLICT (id) DO NOTHING
    RETURNING xact_id, seq
)
SELECT inserted.xact_id::text, inserted.seq, true, true
  FROM inserted
UNION ALL
SELECT e.xact_id::text, e.seq, false,
       e.kind = $4 AND e.occurred_at = $9 AND e.payload = $10 AND e.acl = $11
  FROM l0_events e
 WHERE e.id = $1 AND NOT EXISTS (SELECT 1 FROM inserted)`

// retractedSQL is the visibility rule, and the only one L0 has: an event is hidden
// once a tombstone in the same source names its artifact. The row stays — L0 is
// append-only — and every read carries the negation of this predicate.
const retractedSQL = `EXISTS (
    SELECT 1 FROM l0_events tomb
     WHERE tomb.source = e.source AND tomb.target = e.artifact
)`

// notRetractedSQL is what a read admits.
const notRetractedSQL = `NOT ` + retractedSQL

// eventColumns is everything an event is reconstructed from.
const eventColumns = `e.id, e.source, e.native_id, e.kind, e.occurred_at, e.payload, e.acl`

// Get returns one event by id. It returns [ErrRetracted] for an event a
// tombstone covers and [ErrNotFound] for an id L0 does not hold.
func (s *Store) Get(ctx context.Context, id string) (connector.Event, error) {
	var (
		ev        connector.Event
		kind      string
		payload   []byte
		acl       []byte
		retracted bool
	)
	err := s.db.QueryRow(ctx,
		`SELECT `+eventColumns+`, `+retractedSQL+` FROM l0_events e WHERE e.id = $1`, id,
	).Scan(&ev.ID, &ev.Source, &ev.NativeID, &kind, &ev.Time, &payload, &acl, &retracted)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return connector.Event{}, fmt.Errorf("%w: %s", ErrNotFound, id)
	case err != nil:
		return connector.Event{}, fmt.Errorf("reading event %s: %w", id, err)
	case retracted:
		return connector.Event{}, fmt.Errorf("%w: %s", ErrRetracted, id)
	}
	if err := decodeInto(&ev, kind, payload, acl); err != nil {
		return connector.Event{}, fmt.Errorf("reading event %s: %w", id, err)
	}
	return ev, nil
}

// Filter narrows a read to part of the store. The zero value is all of it. Both
// reads take one, so "only this source" means the same thing on a listing and on
// the feed.
type Filter struct {
	// Source reads one configured source only.
	Source string
	// Kind reads one kind only.
	Kind connector.Kind
	// Artifact reads one artifact's history — its first appearance and every
	// revision of it — and requires Source, because an artifact id means
	// nothing outside the source that minted it.
	Artifact string
}

// Validate reports a filter that cannot mean what it says. Both reads call it,
// and so does anything that wants to refuse a filter before opening a database.
func (f Filter) Validate() error {
	if f.Artifact != "" && f.Source == "" {
		return errors.New("reading by artifact needs a source: an artifact id is only unique within one")
	}
	return nil
}

// where adds the filter's predicates to a query.
func (f Filter) where(q *query) {
	if f.Source != "" {
		q.and("e.source", f.Source)
	}
	if f.Kind != "" {
		q.and("e.kind", string(f.Kind))
	}
	if f.Artifact != "" {
		q.and("e.artifact", f.Artifact)
	}
}

// ListOptions narrows and orders a listing. The zero value is the whole store,
// oldest first, up to [DefaultLimit] events.
type ListOptions struct {
	Filter
	// Limit is how many events to return, capped at [MaxLimit].
	Limit int
	// Newest reverses the order, which is what an operator asking "what has
	// arrived" wants — and, for one artifact, it is the order
	// docs/connector-contract.md puts its revisions in.
	Newest bool
}

// List returns the events an option set selects, excluding everything a
// tombstone covers.
//
// The default order is oldest first: by the artifact's own time, then the
// observation with no edit time (the first appearance) before those that have
// one, then by edit time, then by arrival. That is
// docs/connector-contract.md's ordering of an artifact's revisions **reversed**;
// Newest is the contract's own direction, so with Newest the current revision of
// an artifact is the first result. Whoever wants the current one asks for
// Newest — an ACL re-sync is a new revision, and taking the first of the default
// order would read the access list the re-sync replaced.
func (s *Store) List(ctx context.Context, opts ListOptions) ([]connector.Event, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	q := &query{sql: `SELECT ` + eventColumns + ` FROM l0_events e WHERE ` + notRetractedSQL}
	opts.where(q)
	if opts.Newest {
		q.sql += ` ORDER BY e.occurred_at DESC, e.revision_edited_at DESC NULLS LAST, e.seq DESC`
	} else {
		q.sql += ` ORDER BY e.occurred_at ASC, e.revision_edited_at ASC NULLS FIRST, e.seq ASC`
	}
	q.sql += " LIMIT " + q.placeholder(int64(Limit(opts.Limit)))

	rows, err := s.db.Query(ctx, q.sql, q.args...)
	if err != nil {
		return nil, fmt.Errorf("listing events: %w", err)
	}
	defer rows.Close()

	events := []connector.Event{}
	for rows.Next() {
		var (
			ev      connector.Event
			kind    string
			payload []byte
			acl     []byte
		)
		if err := rows.Scan(&ev.ID, &ev.Source, &ev.NativeID, &kind, &ev.Time, &payload, &acl); err != nil {
			return nil, fmt.Errorf("listing events: %w", err)
		}
		if err := decodeInto(&ev, kind, payload, acl); err != nil {
			return nil, fmt.Errorf("listing events: %w", err)
		}
		events = append(events, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing events: %w", err)
	}
	return events, nil
}

// Count is how many events one source holds of one kind.
type Count struct {
	Source string
	Kind   connector.Kind
	// Events is every row, retracted ones included: L0 is append-only, so this
	// is what was ever ingested.
	Events int64
	// Visible is what a read returns — Events less what tombstones cover.
	Visible int64
}

// Counts is every (source, kind) pair in the store with how many events it
// holds, sorted. It reports both totals so that a tombstoned event is visibly
// still there, which is what append-only means.
func (s *Store) Counts(ctx context.Context) ([]Count, error) {
	rows, err := s.db.Query(ctx, `
SELECT e.source, e.kind, count(*), count(*) FILTER (WHERE `+notRetractedSQL+`)
  FROM l0_events e
 GROUP BY e.source, e.kind
 ORDER BY e.source, e.kind`)
	if err != nil {
		return nil, fmt.Errorf("counting events: %w", err)
	}
	defer rows.Close()

	counts := []Count{}
	for rows.Next() {
		var c Count
		var kind string
		if err := rows.Scan(&c.Source, &kind, &c.Events, &c.Visible); err != nil {
			return nil, fmt.Errorf("counting events: %w", err)
		}
		c.Kind = connector.Kind(kind)
		counts = append(counts, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("counting events: %w", err)
	}
	return counts, nil
}

// Change is one event on the feed, with the cursor to resume after it.
type Change struct {
	Cursor Cursor
	Event  connector.Event
	// IngestedAt is when Hearsay wrote the row, as against Event.Time, which is
	// when the artifact happened at the source.
	IngestedAt time.Time
}

// Changes is the change feed: the events that arrived after a cursor, oldest
// first, for the distiller to consume and for `watch` later. The filter narrows
// what comes back and nothing else — a cursor is a position in the whole feed,
// so the same one means the same place whatever a reader is watching.
//
// It never skips an event. A row becomes readable here only once the
// transaction that wrote it has finished, and cursors only move forward through
// finished transactions, so an event that a reader has not reached cannot
// become unreachable. The cost is that a long-running write transaction holds
// the feed at its own position rather than letting readers past it.
//
// Tombstoned events are not on the feed; the tombstones themselves are, which
// is how a consumer learns to walk provenance forward and re-derive without
// them.
func (s *Store) Changes(ctx context.Context, from Cursor, filter Filter, limit int) ([]Change, error) {
	if err := filter.Validate(); err != nil {
		return nil, err
	}
	q := &query{sql: `
SELECT e.xact_id::text, e.seq, e.ingested_at, ` + eventColumns + `
  FROM l0_events e
 WHERE (e.xact_id, e.seq) > (` + `$1::xid8, $2::bigint)
   AND e.xact_id < pg_snapshot_xmin(pg_current_snapshot())
   AND ` + notRetractedSQL}
	q.args = []any{strconv.FormatUint(from.xact, 10), from.seq}
	filter.where(q)
	q.sql += `
 ORDER BY e.xact_id, e.seq
 LIMIT ` + q.placeholder(int64(Limit(limit)))

	rows, err := s.db.Query(ctx, q.sql, q.args...)
	if err != nil {
		return nil, fmt.Errorf("reading the change feed: %w", err)
	}
	defer rows.Close()

	changes := []Change{}
	for rows.Next() {
		var (
			c       Change
			xact    string
			seq     int64
			kind    string
			payload []byte
			acl     []byte
		)
		if err := rows.Scan(&xact, &seq, &c.IngestedAt,
			&c.Event.ID, &c.Event.Source, &c.Event.NativeID, &kind, &c.Event.Time, &payload, &acl); err != nil {
			return nil, fmt.Errorf("reading the change feed: %w", err)
		}
		if err := decodeInto(&c.Event, kind, payload, acl); err != nil {
			return nil, fmt.Errorf("reading the change feed: %w", err)
		}
		c.IngestedAt = c.IngestedAt.UTC()
		if c.Cursor, err = cursorOf(xact, seq); err != nil {
			return nil, fmt.Errorf("reading the change feed: %w", err)
		}
		changes = append(changes, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the change feed: %w", err)
	}
	return changes, nil
}

// row is one event as columns.
type row struct {
	id               string
	source           string
	nativeID         string
	kind             connector.Kind
	artifact         string
	revisionToken    *string
	revisionEditedAt *time.Time
	target           *string
	occurredAt       time.Time
	payload          []byte
	acl              []byte
}

// rowOf spreads an event across the columns reads index on. Every one of them
// is a copy of something in the payload, which is why the table constrains them
// against each other.
func rowOf(ev connector.Event) (row, error) {
	payload, err := json.Marshal(ev.Payload)
	if err != nil {
		return row{}, fmt.Errorf("encoding the payload of %s: %w", ev.NativeID, err)
	}
	acl, err := json.Marshal(ev.ACL)
	if err != nil {
		return row{}, fmt.Errorf("encoding the acl of %s: %w", ev.NativeID, err)
	}
	r := row{
		id:         connector.EventID(ev.Source, ev.NativeID),
		source:     ev.Source,
		nativeID:   ev.NativeID,
		kind:       ev.Kind,
		artifact:   ev.Payload.Artifact,
		occurredAt: ev.Time,
		payload:    payload,
		acl:        acl,
	}
	if rev := ev.Payload.Revision; rev != nil {
		r.revisionToken = &rev.Token
		if !rev.EditedAt.IsZero() {
			edited := rev.EditedAt
			r.revisionEditedAt = &edited
		}
	}
	if ev.Payload.Target != "" {
		target := ev.Payload.Target
		r.target = &target
	}
	return r, nil
}

// decodeInto fills in the parts of an event that are stored as JSON, and puts
// the event's time in UTC.
//
// Postgres hands a timestamptz back in the session's time zone, which is the
// server's and has nothing to do with the event; the instant is the same either
// way, and reading it back in one zone is what keeps two events from looking
// different when they are not. The resolution is Postgres's microsecond, so an
// event written with nanoseconds comes back truncated.
func decodeInto(ev *connector.Event, kind string, payload, acl []byte) error {
	ev.Kind = connector.Kind(kind)
	ev.Time = ev.Time.UTC()
	if err := json.Unmarshal(payload, &ev.Payload); err != nil {
		return fmt.Errorf("decoding the payload: %w", err)
	}
	if err := json.Unmarshal(acl, &ev.ACL); err != nil {
		return fmt.Errorf("decoding the acl: %w", err)
	}
	return nil
}

func cursorOf(xact string, seq int64) (Cursor, error) {
	x, err := strconv.ParseUint(xact, 10, 64)
	if err != nil {
		return Cursor{}, fmt.Errorf("transaction id %q: %w", xact, err)
	}
	return Cursor{xact: x, seq: seq}, nil
}

// Limit is how many events a read with this limit actually returns: the
// default when none was asked for, the cap when too many were. It is exported
// so that a caller paging the feed can tell a full page from a caught-up one
// without keeping its own copy of the numbers.
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

// query builds a statement with numbered placeholders, so that a filter nobody
// set contributes no predicate and the planner sees the real one rather than
// `$1 = ” OR ...`.
type query struct {
	sql  string
	args []any
}

// placeholder records an argument and returns the $n that refers to it.
func (q *query) placeholder(arg any) string {
	q.args = append(q.args, arg)
	return "$" + strconv.Itoa(len(q.args))
}

// and adds an equality predicate on a column.
func (q *query) and(column string, arg any) {
	q.sql += " AND " + column + " = " + q.placeholder(arg)
}
