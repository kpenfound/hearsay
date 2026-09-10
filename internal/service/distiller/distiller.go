// Package distiller turns L0 events into L1 documents. It is a stateless,
// parallel, retryable worker on the `distill` model tier (ADR-0005): every job
// it runs can be run again, and re-running one for an artifact produces the
// same document.
//
// It is two loops over one database. The pump reads the L0 change feed and
// enqueues a `distill` job for the document each event belongs to; the worker
// claims those jobs and distils. They are separate because events arrive in
// bursts and documents do not: the queue collapses a burst of comments on one
// pull request into one pending job, so the model is called once per document
// rather than once per event (ADR-0007).
//
// What a document is, and what may be stored in one, is internal/l1's. What
// this package owns is the prompts, the schema an answer has to satisfy, and
// the loops.
package distiller

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/queue"
	"github.com/kpenfound/hearsay/internal/telemetry"
)

// Name is the subcommand this service runs as (ADR-0003).
const Name = "distiller"

// JobKindName is the queue kind this service consumes. It is a contract with
// whatever enqueues one, and with an operator reading `queue_job`.
const JobKindName = "distill"

// DefaultConcurrency is how many documents one process distils at once. Each is
// a model call measured in seconds, so the number is about how much of a
// provider's rate limit one replica takes rather than about CPU.
const DefaultConcurrency = 4

// JobKind is the kind both ends of a distill job share. It is not serialized:
// two documents have nothing to do with each other, and the ordering that does
// matter — a burst of events on one document — is handled by the queue's
// dedupe on (kind, target) rather than by a serial key (ADR-0007).
func JobKind() queue.Kind { return queue.Kind{Name: JobKindName} }

// Deps are what the process builds and hands to [Run].
type Deps struct {
	// Pool is the database (ADR-0004). The caller owns it and closes it.
	Pool *pgxpool.Pool
	// LLM is the model tier registry (ADR-0005). The distiller uses the
	// `distill` tier and resolves it at startup, so a configuration that names
	// no `distill` tier is a startup failure rather than a job that fails an
	// hour later.
	//
	// A nil registry is a process that was given no configuration at all: it
	// starts, says there is nothing to distil, and waits to be stopped, the
	// same as every other service without one (ADR-0009). A process with a
	// configuration always has a registry, because the loader merges the
	// shipped tiers in.
	LLM llm.Registry
	// Pump is what the feed reader is turned down to, for a deployment that
	// wants it quieter and for a test that wants it immediate. The zero value
	// is the defaults.
	Pump PumpOptions
	// Concurrency is how many jobs this process runs at once. Zero is
	// [DefaultConcurrency].
	Concurrency int
}

// Run works the queue and reads the feed until ctx is cancelled, and returns
// nil when it stops that way.
//
// The two loops stop together: a process running one of them is not doing the
// job — a pump with no worker fills the queue, and a worker with no pump runs
// dry — so the first to return stops the other, the way `hearsay all` treats
// the four services.
func Run(ctx context.Context, cfg *config.Config, deps Deps) error {
	if deps.Pool == nil {
		return errors.New("the distiller needs a database: pass --database-url or set HEARSAY_DATABASE_URL")
	}
	if deps.LLM == nil {
		// No configuration, so there is nothing to distil and no tier to
		// distil it with. Idling rather than exiting is what makes
		// `hearsay all` on an empty machine something a person can look at.
		log := telemetry.Logger(ctx)
		log.WarnContext(ctx, "no model tiers: nothing is distilled, pass --config")
		<-ctx.Done()
		log.InfoContext(ctx, "distiller stopped")
		return nil
	}
	distiller, err := New(deps.Pool, deps.LLM, cfg)
	if err != nil {
		return err
	}
	concurrency := deps.Concurrency
	if concurrency <= 0 {
		concurrency = DefaultConcurrency
	}
	worker, err := queue.NewWorker(deps.Pool, queue.Config{
		Kind:        JobKind(),
		Concurrency: concurrency,
	}, distiller.Handle)
	if err != nil {
		return err
	}
	pump := NewPump(deps.Pool, deps.Pump)

	log := telemetry.Logger(ctx)
	log.InfoContext(ctx, "distiller started",
		"concurrency", concurrency, "config_digest", cfg.Repo.Digest,
		"embedding", distiller.embedder != nil)
	if distiller.embedder == nil {
		// Without it every document is written with no vector, so search finds
		// documents by the words in them and never by what they are about
		// (docs/config.md).
		log.WarnContext(ctx, "no embed tier: documents are not embedded and search runs on full text alone")
	}

	ctx, stop := context.WithCancel(ctx)
	defer stop()
	loops := map[string]func(context.Context) error{
		"pump":   pump.Run,
		"worker": worker.Run,
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
