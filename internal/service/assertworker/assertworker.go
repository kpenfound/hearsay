// Package assertworker turns L1 documents into L2: entities, topics, stances
// and the edges between them. It runs on the `assert` model tier (ADR-0005) and
// is serialized per scope, because two workers deciding the current stance for
// one topic at the same time is how supersession chains get corrupted. The
// queue's serial_key is what enforces that (ADR-0007).
//
// The distiller enqueues an `assert` job in the transaction that writes a
// document whose outcome enters the pipeline; this package claims them. At
// startup it seeds the entity map from configuration and the repositories it
// names ([Repos]), and enqueues every such document it has not read yet, so
// documents written before it was deployed, and jobs that ran out of attempts,
// are picked up by a restart.
//
// What a topic and a stance are is internal/l2's. What this package owns is the
// prompt, the schema an answer has to satisfy, and the loop.
package assertworker

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/queue"
	"github.com/kpenfound/hearsay/internal/telemetry"
)

// Name is the subcommand this service runs as (ADR-0003).
const Name = "assert-worker"

// Deps are what the process builds and hands to [Run].
type Deps struct {
	// Pool is the database (ADR-0004). The caller owns it and closes it.
	Pool *pgxpool.Pool
	// LLM is the model tier registry (ADR-0005). The worker uses the `assert`
	// tier and resolves it at startup. A nil registry is a process given no
	// configuration: it starts, says there is nothing to assert, and waits to
	// be stopped.
	LLM llm.Registry
	// Repos reads CODEOWNERS files and the top level of the repositories
	// `code/` names, for seeding; the binary hands it [Repos] with a reader per
	// GitHub source those entries name. Nil seeds from `code/` alone, and says
	// so when an entry names a CODEOWNERS file.
	Repos l2.RepoReader
}

// Run seeds the entity map, enqueues what has not been read, and works the
// queue until ctx is cancelled. It returns nil when it stops that way.
func Run(ctx context.Context, cfg *config.Config, deps Deps) error {
	if deps.Pool == nil {
		return errors.New("the assertion worker needs a database: pass --database-url or set HEARSAY_DATABASE_URL")
	}
	log := telemetry.Logger(ctx)
	if deps.LLM == nil {
		log.WarnContext(ctx, "no model tiers: nothing is asserted, pass --config")
		<-ctx.Done()
		log.InfoContext(ctx, "assertion worker stopped")
		return nil
	}
	asserter, err := New(deps.Pool, deps.LLM, cfg)
	if err != nil {
		return err
	}
	if _, err := SeedEntities(ctx, deps.Pool, cfg.Repo, deps.Repos); err != nil {
		return stopping(ctx, err)
	}
	enqueued, err := Sweep(ctx, deps.Pool, cfg.Repo)
	if err != nil {
		return stopping(ctx, err)
	}
	worker, err := queue.NewWorker(deps.Pool, queue.Config{Kind: l2.AssertKind()}, asserter.Handle)
	if err != nil {
		return err
	}
	log.InfoContext(ctx, "assertion worker started", "config_digest", cfg.Repo.Digest, "swept", enqueued)
	return worker.Run(ctx)
}

// stopping turns a failure caused by the process being stopped into a clean
// stop.
func stopping(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// SeedEntities writes the entity map configuration describes ([l2.Seed]) in one
// transaction, and returns how many entities it holds.
//
// A `code/` entry that names a CODEOWNERS file with no reader to read it is
// said out loud: configuration asked for something this process cannot do, and
// a warning is the difference between that and a silently ignored field.
func SeedEntities(ctx context.Context, pool *pgxpool.Pool, repo config.Repo, reader l2.RepoReader) (int, error) {
	log := telemetry.Logger(ctx)
	if reader == nil {
		unread := 0
		for _, c := range repo.Code {
			if c.CodeOwners != "" {
				unread++
			}
		}
		if unread > 0 {
			log.WarnContext(ctx, "no repository reader: CODEOWNERS files and repository layout are not imported", "code_entities", unread)
		}
	}
	entities, err := l2.Seed(ctx, repo, reader)
	if err != nil {
		return 0, err
	}
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return l2.New(tx).ReplaceSeeded(ctx, entities)
	})
	if err != nil {
		return 0, fmt.Errorf("seeding entities: %w", err)
	}
	return len(entities), nil
}

// Sweep enqueues an assert job for every document whose outcome enters the
// pipeline and whose current version has not been read, and returns how many it
// asked for. It is what makes a restart pick up documents written before the
// worker was deployed and jobs that failed for good. A pending job for a
// document collapses the enqueue (ADR-0007), so sweeping twice costs a
// statement per document and no model call.
func Sweep(ctx context.Context, pool *pgxpool.Pool, repo config.Repo) (int, error) {
	pending, err := l2.New(pool).Unasserted(ctx)
	if err != nil {
		return 0, err
	}
	events := l0.New(pool)
	enqueued := 0
	for _, doc := range pending {
		root, err := events.Get(ctx, doc.RootEvent)
		if errors.Is(err, l0.ErrNotFound) || errors.Is(err, l0.ErrRetracted) {
			// The distiller removes the document of a retracted artifact; until
			// it has, there is nothing to read from it.
			continue
		}
		if err != nil {
			return enqueued, err
		}
		_, err = queue.Enqueue(ctx, pool, queue.Request{
			Kind:      l2.AssertKind(),
			TargetID:  doc.ID,
			SerialKey: l2.ScopeKey(repo, root.Source, root.Payload.Container.NativeID),
		})
		if err != nil {
			return enqueued, err
		}
		enqueued++
	}
	return enqueued, nil
}
