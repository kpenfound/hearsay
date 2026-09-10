package distiller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/principal"
	"github.com/kpenfound/hearsay/internal/queue"
	"github.com/kpenfound/hearsay/internal/telemetry"
)

// CallTimeout bounds one distillation's model call, retries included.
//
// A handler owes its model call a deadline shorter than the lease on the job it
// is running (internal/llm): the abstraction honours a provider's Retry-After
// uncapped by design, so a rate-limited call with no deadline could outlive the
// lease, have the job reclaimed underneath it, and end with two workers writing
// one document. The queue's default lease is a minute and the worker heartbeats
// while the handler runs, so the lease is not the bound in practice — this is,
// and it is what makes a wedged provider a failed job that retries rather than
// a worker that stops working.
const CallTimeout = 2 * time.Minute

// Distiller turns one artifact into one L1 document. It is the whole of what a
// `distill` job does, and it is a value a test can drive directly: the worker
// loop around it is [Run]'s.
//
// It is stateless and idempotent. Running it twice over an artifact nothing has
// happened to writes the document that is already there, which the store
// notices and does not write at all.
type Distiller struct {
	events   *l0.Store
	docs     *l1.Store
	tier     llm.Completer
	resolver *principal.Resolver
	repo     config.Repo
	timeout  time.Duration
}

// New builds the distiller from what the process has: the database, the model
// tier registry and the configuration.
//
// It resolves the `distill` tier here rather than per job, because a process
// that cannot make a model call should not be running (ADR-0005): a tier the
// configuration does not name is a startup failure, not a job that fails five
// times an hour later.
func New(pool *pgxpool.Pool, registry llm.Registry, cfg *config.Config) (*Distiller, error) {
	if pool == nil {
		return nil, errors.New("the distiller needs a database")
	}
	if registry == nil {
		return nil, errors.New("the distiller needs the model tier registry")
	}
	tier, err := registry.Completer(llm.TierDistill)
	if err != nil {
		return nil, err
	}
	resolver, err := cfg.Repo.Resolver()
	if err != nil {
		return nil, fmt.Errorf("building the identity resolver: %w", err)
	}
	return &Distiller{
		events:   l0.New(pool),
		docs:     l1.New(pool),
		tier:     tier,
		resolver: resolver,
		repo:     cfg.Repo,
		timeout:  CallTimeout,
	}, nil
}

// Result is what one distillation did.
type Result struct {
	// DocID is the document the job was about.
	DocID string
	// Written reports that the document changed. False with no other flag set
	// is the ordinary outcome of re-distilling an artifact nothing has happened
	// to: the document was rebuilt and was the one already stored.
	Written bool
	// Deleted reports that the artifact has been retracted at the source and
	// the document went with it.
	Deleted bool
	// Skipped reports an artifact that makes no document of its own — a comment
	// whose parent the source did not name, a kind this build does not distil.
	Skipped bool
	// Superseded reports that L0 gained events for this artifact while the
	// model was answering, so what came back describes a conversation that has
	// moved on and was not written. The events that moved it have a job of
	// their own, so the document is not left behind.
	Superseded bool
	// Redacted names the shapes the scrub took out of what the model wrote,
	// sorted. It is the shapes and never the values (ADR-0008).
	Redacted []string
}

// Handle is the queue handler for the `distill` kind. It is safe to run twice
// on one job, which is what at-least-once delivery requires (ADR-0007).
func (d *Distiller) Handle(ctx context.Context, job queue.Job) error {
	result, err := d.Distill(ctx, job.TargetID)
	if err != nil {
		return err
	}
	log := telemetry.Logger(ctx)
	switch {
	case result.Skipped:
		log.DebugContext(ctx, "nothing to distil", "l1_id", result.DocID)
	case result.Deleted:
		log.InfoContext(ctx, "document removed: its artifact was retracted at the source", "l1_id", result.DocID)
	case result.Superseded:
		log.InfoContext(ctx, "distillation dropped: the conversation moved while the model was answering",
			"l1_id", result.DocID)
	case result.Written:
		log.InfoContext(ctx, "document distilled", "l1_id", result.DocID, "redacted", result.Redacted)
	default:
		log.DebugContext(ctx, "document unchanged", "l1_id", result.DocID)
	}
	return nil
}

// Distill rebuilds one document from L0 and writes it.
//
// Everything it needs is in the event store, so it is safe to run at any time,
// in any order, twice: the job carries a document id and nothing else, and the
// document is a function of the artifact's current revisions.
func (d *Distiller) Distill(ctx context.Context, docID string) (Result, error) {
	source, artifact, err := l1.ParseDocID(docID)
	if err != nil {
		return Result{}, err
	}
	result := Result{DocID: docID}

	roots, err := d.events.Current(ctx, l0.ListOptions{
		Filter: l0.Filter{Source: source, Artifact: artifact},
		Limit:  1,
	})
	if err != nil {
		return Result{}, fmt.Errorf("reading %s: %w", docID, err)
	}
	if len(roots) == 0 {
		// Either the artifact was retracted at the source — a tombstone hides
		// every event of it — or nothing was ever ingested under this id. A
		// document derived from nothing is not a document, and leaving one
		// standing would serve what the source deleted
		// (docs/design.md#deletion-and-provenance).
		deleted, err := d.docs.Delete(ctx, docID)
		if err != nil {
			return Result{}, err
		}
		result.Deleted = deleted
		result.Skipped = !deleted
		return result, nil
	}
	root := roots[0]
	if _, ok := l1.KindFor(root); !ok {
		// A job for something that is part of another artifact's document: a
		// comment whose parent the source did not name, or a kind no L1 kind
		// covers yet. Nothing to do, and nothing wrong.
		result.Skipped = true
		return result, nil
	}

	children, err := d.events.Current(ctx, l0.ListOptions{
		Filter: l0.Filter{Source: source, Thread: artifact},
		Limit:  l0.MaxLimit,
	})
	if err != nil {
		return Result{}, fmt.Errorf("reading the conversation on %s: %w", docID, err)
	}
	if len(children) >= l0.MaxLimit {
		// Silently distilling the first thousand would be a document that
		// claims to be the whole conversation and is not, and it would go on
		// being wrong every time it was rebuilt. A failed job is visible in the
		// queue; a truncated document is not.
		return Result{}, fmt.Errorf("%s has at least %d replies, which is more than one read returns", docID, l0.MaxLimit)
	}

	doc, err := l1.Build(l1.Input{
		Root:     root,
		Children: children,
		Resolver: d.resolver,
		Repo:     d.repo,
	})
	if errors.Is(err, l1.ErrNotDistilled) {
		result.Skipped = true
		return result, nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("building %s: %w", docID, err)
	}

	body, err := d.distil(ctx, doc)
	if err != nil {
		return Result{}, err
	}
	doc, redacted, err := doc.WithBody(body)
	if err != nil {
		return Result{}, err
	}

	// The model call is the slow part, and the queue's dedupe on (kind, target)
	// only covers jobs that are still pending: an event arriving while this one
	// runs gets a job of its own, and both can be in flight at once. The write
	// is an upsert, so the one that finishes last wins — and if that is this
	// one, built from the older read, the document loses the newer events and
	// nothing is left pending to put them back.
	//
	// So the provenance is checked against L0 again before the row is written,
	// and a distillation whose conversation has moved is dropped rather than
	// stored. Rebuilding is how the check is made, because it is the same code
	// that produced L0Refs in the first place and cannot drift from it; it
	// costs two reads and no model call. The job that carried those newer
	// events writes the document that includes them.
	current, err := d.provenanceNow(ctx, source, artifact)
	if err != nil {
		return Result{}, fmt.Errorf("re-reading %s: %w", docID, err)
	}
	if !slices.Equal(current, doc.L0Refs) {
		result.Superseded = true
		return result, nil
	}

	written, err := d.docs.Put(ctx, doc)
	if err != nil {
		return Result{}, err
	}
	result.Written = written
	result.Redacted = redacted
	return result, nil
}

// provenanceNow is the L0Refs a document built from L0 right now would carry,
// and nil for an artifact that would no longer make a document at all. It is
// the second half of the check described in Distill: same reads, same builder,
// so what it returns is comparable to what Build already produced.
func (d *Distiller) provenanceNow(ctx context.Context, source, artifact string) ([]string, error) {
	roots, err := d.events.Current(ctx, l0.ListOptions{
		Filter: l0.Filter{Source: source, Artifact: artifact},
		Limit:  1,
	})
	if err != nil || len(roots) == 0 {
		return nil, err
	}
	children, err := d.events.Current(ctx, l0.ListOptions{
		Filter: l0.Filter{Source: source, Thread: artifact},
		Limit:  l0.MaxLimit,
	})
	if err != nil {
		return nil, err
	}
	doc, err := l1.Build(l1.Input{
		Root:     roots[0],
		Children: children,
		Resolver: d.resolver,
		Repo:     d.repo,
	})
	if err != nil {
		// Including ErrNotDistilled: an artifact that no longer makes a
		// document is not one this distillation should write.
		return nil, nil //nolint:nilerr // a build that no longer succeeds is a changed provenance, not a failure
	}
	return doc.L0Refs, nil
}

// distil is the one model call: the document's own words in, the body out.
//
// The error it returns is a log line and a `queue_job.last_error` column, so it
// names the document, the tier and the shape that was asked for, and never a
// word of what was sent or what came back (ADR-0008). internal/llm's own errors
// hold to the same rule.
func (d *Distiller) distil(ctx context.Context, doc l1.Document) (l1.Body, error) {
	call, done := context.WithTimeout(ctx, d.timeout)
	defer done()

	// The budget is left zero: the registry fills in the tier's, which is
	// where a deployment sets it (ADR-0005).
	req := RequestFor(doc, 0)
	resp, err := d.tier.Complete(call, req)
	if err != nil {
		return l1.Body{}, fmt.Errorf("distilling %s: %w", doc.ID, err)
	}

	// The answer has already been checked against the schema by internal/llm,
	// so this decodes a shape that is known to fit. What it cannot check is
	// that the enum member is one this build knows, because the schema's enum
	// and the parser are two readings of one list — hence the parse.
	var a answer
	if err := json.Unmarshal(resp.JSON, &a); err != nil {
		return l1.Body{}, fmt.Errorf("distilling %s: the %s answer for %s did not decode: %w", doc.ID, llm.TierDistill, req.Schema.Name, err)
	}
	body, err := a.body()
	if err != nil {
		return l1.Body{}, fmt.Errorf("distilling %s: %w", doc.ID, err)
	}
	return body, nil
}
