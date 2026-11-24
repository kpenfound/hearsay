package l0

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/connector"
)

// ArtifactRef names one artifact of one source.
type ArtifactRef struct {
	Source, Artifact string
}

// artifactLockKey is the advisory-lock key an artifact is serialized under. A
// collision between two artifacts only makes them wait for each other.
func artifactLockKey(a ArtifactRef) int64 {
	sum := sha256.Sum256([]byte("hearsay/l0/artifact\x00" + a.Source + "\x00" + a.Artifact))
	return int64(binary.BigEndian.Uint64(sum[:8]))
}

// HoldArtifacts takes the per-artifact locks [Store.SerializeArtifact] writes
// under, for every artifact given, in one order so that two holders never wait
// for each other. The caller holds them until the returned function runs, which
// is after the transaction that read or deleted the artifacts has finished: a
// transaction that starts after the locks are held sees every write that
// completed before them, and a writer that waits for them sees the deletion.
//
// The locks belong to the connection, not to a transaction, so that the
// deletion's snapshot is taken after them.
func HoldArtifacts(ctx context.Context, conn *pgxpool.Conn, artifacts []ArtifactRef) (func(), error) {
	keys := make([]int64, 0, len(artifacts))
	for _, a := range artifacts {
		keys = append(keys, artifactLockKey(a))
	}
	slices.Sort(keys)
	keys = slices.Compact(keys)
	held := make([]int64, 0, len(keys))
	release := func() {
		ctx := context.WithoutCancel(ctx)
		for _, key := range slices.Backward(held) {
			if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, key); err != nil {
				// A lock that cannot be released dies with its session.
				_ = conn.Conn().Close(ctx)
				return
			}
		}
	}
	for _, key := range keys {
		if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, key); err != nil {
			release()
			return nil, fmt.Errorf("locking an artifact: %w", err)
		}
		held = append(held, key)
	}
	return release, nil
}

type beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// SerializeArtifact implements [connector.ArtifactSerializer]: fn runs in one
// transaction holding the artifact's lock, so no other writer of the artifact
// and no operator deletion of it interleaves with what fn reads and writes.
func (s *Store) SerializeArtifact(ctx context.Context, source, artifact string, fn func(context.Context, connector.LockedArtifact) error) error {
	b, ok := s.db.(beginner)
	if !ok {
		return errors.New("the store cannot begin a transaction to serialize an artifact")
	}
	tx, err := b.Begin(ctx)
	if err != nil {
		return fmt.Errorf("serializing %s: %w", artifact, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, artifactLockKey(ArtifactRef{source, artifact})); err != nil {
		return fmt.Errorf("locking %s: %w", artifact, err)
	}
	inside := New(tx)
	view := &lockedArtifact{store: inside, source: source, artifact: artifact}
	if err := tx.QueryRow(ctx, `
SELECT EXISTS (SELECT 1 FROM l0_events WHERE source = $1 AND artifact = $2 AND deletion IS NOT NULL)`,
		source, artifact).Scan(&view.deleted); err != nil {
		return fmt.Errorf("reading whether %s was deleted: %w", artifact, err)
	}
	if view.current, view.held, err = inside.CurrentArtifact(ctx, source, artifact); err != nil {
		return err
	}
	if err := fn(ctx, view); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing %s: %w", artifact, err)
	}
	return nil
}

type lockedArtifact struct {
	store            *Store
	source, artifact string
	current          connector.Event
	held, deleted    bool
}

func (a *lockedArtifact) Current() (connector.Event, bool) { return a.current, a.held }
func (a *lockedArtifact) Deleted() bool                    { return a.deleted }
func (a *lockedArtifact) Emit(ctx context.Context, ev connector.Event) error {
	if ev.Source != a.source || ev.Payload.Artifact != a.artifact {
		return fmt.Errorf("%w: %s holds the lock on %s/%s", connector.ErrForeignSource, ev.NativeID, a.source, a.artifact)
	}
	return a.store.Emit(ctx, ev)
}

var _ connector.ArtifactSerializer = (*Store)(nil)
