package l0

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
)

// Resyncs is where the connector runtime keeps ACL re-syncs: one row per
// container of a source, saying whether one is owed, how far it has got and
// when the last one finished. It implements [connector.ResyncStore], the way
// [BackfillCursors] implements [connector.CursorStore].
//
// Every write is one statement, so two replicas hosting a source cannot lose a
// request between them: a request counts a generation, and a walk that
// finishes only settles the generation it read.
type Resyncs struct {
	db Querier
}

// NewResyncs returns the store over a pool or a transaction on one.
func NewResyncs(q Querier) *Resyncs { return &Resyncs{db: q} }

// Owe implements [connector.ResyncStore].
func (r *Resyncs) Owe(ctx context.Context, source, container string) error {
	if err := validResync(source, container); err != nil {
		return err
	}
	var saved string
	err := r.db.QueryRow(ctx, `
INSERT INTO l0_resyncs AS r (source, container)
VALUES ($1, $2)
ON CONFLICT (source, container) DO UPDATE
SET owed = true,
    cursor = '',
    generation = r.generation + 1,
    updated_at = now()
RETURNING container`, source, container).Scan(&saved)
	if err != nil {
		return fmt.Errorf("recording a re-sync of container %s of source %s: %w", container, source, err)
	}
	return nil
}

// Resyncs implements [connector.ResyncStore].
func (r *Resyncs) Resyncs(ctx context.Context, source string) ([]connector.Resync, error) {
	if err := validBackfillSource(source); err != nil {
		return nil, err
	}
	rows, err := r.db.Query(ctx, `
SELECT container, owed, cursor, generation, resynced_at
  FROM l0_resyncs
 WHERE source = $1
 ORDER BY container`, source)
	if err != nil {
		return nil, fmt.Errorf("reading the re-syncs of source %s: %w", source, err)
	}
	defer rows.Close()
	out := []connector.Resync{}
	for rows.Next() {
		var (
			rec connector.Resync
			at  *time.Time
		)
		if err := rows.Scan(&rec.Container, &rec.Owed, &rec.Cursor, &rec.Generation, &at); err != nil {
			return nil, fmt.Errorf("reading the re-syncs of source %s: %w", source, err)
		}
		if at != nil {
			rec.ResyncedAt = *at
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the re-syncs of source %s: %w", source, err)
	}
	return out, nil
}

// Save implements [connector.ResyncStore].
func (r *Resyncs) Save(ctx context.Context, source string, walk connector.Resync) error {
	container := walk.Container
	if err := validResync(source, container); err != nil {
		return err
	}
	if err := validCursor(walk.Cursor); err != nil {
		return fmt.Errorf("saving the re-sync position of container %s of source %s: %w", container, source, err)
	}
	// Query rather than QueryRow: a walk another replica finished, or a request
	// started again, updates no row, which is not an error.
	rows, err := r.db.Query(ctx, `
UPDATE l0_resyncs SET cursor = $4, updated_at = now()
 WHERE source = $1 AND container = $2 AND owed AND generation = $3
RETURNING container`, source, container, walk.Generation, string(walk.Cursor))
	if err != nil {
		return fmt.Errorf("saving the re-sync position of container %s of source %s: %w", container, source, err)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("saving the re-sync position of container %s of source %s: %w", container, source, err)
	}
	return nil
}

// Finish implements [connector.ResyncStore].
func (r *Resyncs) Finish(ctx context.Context, source string, done connector.Resync) (bool, error) {
	if err := validResync(source, done.Container); err != nil {
		return false, err
	}
	var owed bool
	err := r.db.QueryRow(ctx, `
UPDATE l0_resyncs
   SET owed = generation <> $3,
       cursor = '',
       resynced_at = CASE WHEN generation = $3 THEN now() ELSE resynced_at END,
       updated_at = now()
 WHERE source = $1 AND container = $2
RETURNING owed`, source, done.Container, done.Generation).Scan(&owed)
	if isNoRows(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("finishing the re-sync of container %s of source %s: %w", done.Container, source, err)
	}
	return !owed, nil
}

// validResync refuses a key the table cannot hold, before any SQL runs.
func validResync(source, container string) error {
	if err := validBackfillSource(source); err != nil {
		return err
	}
	if container == "" {
		return errors.New("a re-sync names a container, and this one is empty")
	}
	return nil
}

// Exposed implements [connector.ExposureReader]: every container of a source
// holding an artifact whose current revision is public, with when the newest
// such revision was ingested. Current is [Store.Current]'s revision order, a
// retracted artifact is not served and so not exposed, and a tombstone is not
// an artifact anyone reads.
//
// It reads the whole of a source's events, which is why the runtime asks once,
// at startup.
func (s *Store) Exposed(ctx context.Context, source string) ([]connector.Exposure, error) {
	if err := validBackfillSource(source); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `
SELECT e.payload->'container'->>'native_id', max(e.ingested_at)
  FROM (SELECT DISTINCT ON (e.artifact) e.kind, e.payload, e.acl, e.ingested_at
          FROM l0_events e
         WHERE `+visibleSQL+` AND e.source = $1
         ORDER BY e.artifact, e.revision_edited_at DESC NULLS LAST, e.seq DESC) AS e
 WHERE e.kind <> $2 AND e.acl @> $3::jsonb
 GROUP BY 1
 ORDER BY 1`, source, string(connector.KindTombstone), `[{"kind":"`+string(connector.ACLPublic)+`"}]`)
	if err != nil {
		return nil, fmt.Errorf("reading which containers of source %s are public: %w", source, err)
	}
	defer rows.Close()
	out := []connector.Exposure{}
	for rows.Next() {
		var e connector.Exposure
		if err := rows.Scan(&e.Container, &e.LastPublic); err != nil {
			return nil, fmt.Errorf("reading which containers of source %s are public: %w", source, err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading which containers of source %s are public: %w", source, err)
	}
	return out, nil
}
