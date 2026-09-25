// Package assertworker turns L1 documents into L2: entities, topics, stances
// and the edges between them. It runs on the `assert` model tier (ADR-0005) and
// is serialized per scope, because two workers deciding the current stance for
// one topic at the same time is how supersession chains get corrupted. The
// queue's serial_key is what enforces that (ADR-0007).
//
// The distiller enqueues an `assert` job in the transaction that writes a
// document whose outcome enters the pipeline, and the API one in the
// transaction that writes an agent's L0 `assertion` event, keyed by its topic's
// scope; this package claims them. An assertion becomes a stance with no model
// call. At
// startup it seeds the entity map from configuration, the repositories it
// names ([Repos]) and the tracker hierarchy L0 holds ([Placements]), and
// enqueues every such document it has not read yet, so documents written
// before it was deployed, and jobs that ran out of attempts, are picked up by a
// restart. Between startups the [Follower] keeps the tracker hierarchy in step
// with the L0 change feed, and the [CommandFollower] puts each GitHub
// `/hearsay` comment command, and each deletion of one, on the queue, where
// it is run under its scope's key and answered with one reply comment
// (ADR-0022).
//
// What a topic and a stance are is internal/l2's. What this package owns is the
// prompt, the schema an answer has to satisfy, and the loop.
// Health probes are served on :8083 by default; readiness checks the database
// and schema.
package assertworker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/queue"
	"github.com/kpenfound/hearsay/internal/service"
	"github.com/kpenfound/hearsay/internal/telemetry"
)

// Name is the subcommand this service runs as (ADR-0003).
const Name = "assert-worker"

// DefaultListen is where assertion worker serves health probes.
const DefaultListen = ":8083"

// Deps are what the process builds and hands to [Run].
type Deps struct {
	// Connectors describes optional source capabilities to the worker.
	Connectors *connector.Registry
	// Listener is an optional pre-bound probe listener, useful in tests.
	Listener net.Listener
	// Listen is the probe address when Listener is nil; empty uses DefaultListen.
	Listen string
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
	// Replies answer GitHub `/hearsay` commands; the binary builds a
	// github.Replier with the token of each GitHub source that is not
	// read-only. A source with none runs its commands and does not answer
	// them; a read-only source runs none.
	Replies Replies
}

// Run seeds the entity map, enqueues what has not been read, and works the
// queue, follows the tracker hierarchy and GitHub commands until ctx is
// cancelled. It returns nil when it stops that way.
func Run(ctx context.Context, cfg *config.Config, deps Deps) error {
	if deps.Pool == nil {
		return errors.New("the assertion worker needs a database: pass --database-url or set HEARSAY_DATABASE_URL")
	}
	log := telemetry.Logger(ctx)
	addr := deps.Listen
	if addr == "" {
		addr = DefaultListen
	}
	probes := func(ctx context.Context) error { return service.RunProbes(ctx, Name, addr, deps.Listener, deps.Pool) }
	if deps.LLM == nil {
		log.WarnContext(ctx, "no model tiers: nothing is asserted, pass --config")
		if err := probes(ctx); err != nil {
			return err
		}
		log.InfoContext(ctx, "assertion worker stopped")
		return nil
	}
	asserter, err := New(deps.Pool, deps.LLM, cfg, deps.Connectors)
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
	asserter.WithReplies(deps.Replies)
	follower := NewFollower(deps.Pool, cfg.Repo, 0, 0)
	gestures := NewGestureFollower(deps.Pool, cfg.Repo, deps.Connectors)
	commands := NewCommandFollower(deps.Pool, cfg.Repo, 0, 0)
	log.InfoContext(ctx, "assertion worker started", "config_digest", cfg.Repo.Digest, "swept", enqueued)

	// The loops stop together, as the distiller's do: the first to return
	// stops the other.
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	loops := map[string]func(context.Context) error{
		"probes":    probes,
		"worker":    worker.Run,
		"hierarchy": follower.Run,
		"gestures":  gestures.Run,
		"commands":  commands.Run,
	}
	errs := make([]error, 0, len(loops))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for name, loop := range loops {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer stop()
			if err := loop(ctx); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// stopping turns a failure caused by the process being stopped into a clean
// stop.
func stopping(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// SeedEntities writes the entity map configuration describes, with the tracker
// hierarchy L0 holds now ([l2.Seed], [Placements]), in one transaction, and
// returns how many entities it holds.
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
	placements, err := Placements(ctx, pool, repo)
	if err != nil {
		return 0, err
	}
	entities, err := l2.Seed(ctx, repo, reader, placements)
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
// pipeline and whose current version has not been read, and for every
// `assertion` event no stance was appended from, and for every GitHub command
// whose reply is still owed, and returns how many it asked for. It is what makes a restart pick up documents written before the
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
		if errors.Is(err, l0.ErrNotFound) || errors.Is(err, l0.ErrRetracted) || errors.Is(err, l0.ErrDeleted) {
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
	assertions, err := sweepAssertions(ctx, pool)
	if err != nil {
		return enqueued + assertions, err
	}
	replies, err := sweepReplies(ctx, pool, repo)
	return enqueued + assertions + replies, err
}

// sweepAssertions enqueues a job for every `assertion` event no stance was
// appended from, under its topic's scope.
func sweepAssertions(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	graph := l2.New(pool)
	ids, err := graph.UnappendedAssertions(ctx)
	if err != nil {
		return 0, err
	}
	events := l0.New(pool)
	enqueued := 0
	for _, id := range ids {
		ev, err := events.Get(ctx, id)
		if errors.Is(err, l0.ErrNotFound) || errors.Is(err, l0.ErrRetracted) || errors.Is(err, l0.ErrDeleted) {
			continue
		}
		if err != nil {
			return enqueued, err
		}
		as, err := l2.AssertionOf(ev)
		if err != nil {
			return enqueued, err
		}
		topic, err := graph.Topic(ctx, as.Topic)
		if err != nil {
			return enqueued, err
		}
		_, err = queue.Enqueue(ctx, pool, queue.Request{Kind: l2.AssertKind(), TargetID: id, SerialKey: topic.Scope})
		if err != nil {
			return enqueued, err
		}
		enqueued++
	}
	return enqueued, nil
}
