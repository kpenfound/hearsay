package distiller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
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

// Distiller turns one artifact or channel conversation into L1 documents. A
// long chat may also produce burst documents. It is the whole of what a
// `distill` job does, and it is a value a test can drive directly: the worker
// loop around it is [Run]'s.
//
// It is stateless and idempotent. Running it twice over an artifact nothing has
// happened to writes the document that is already there, which the store
// notices and does not write at all.
type Distiller struct {
	pool     *pgxpool.Pool
	events   *l0.Store
	docs     *l1.Store
	tier     llm.Completer
	embedder llm.Embedder
	resolver *principal.Resolver
	repo     config.Repo
	timeout  time.Duration
}

// The embed tier is used through internal/l1's own interface, so that storing a
// vector does not depend on the provider abstraction. This is the one place
// that says the two are the same shape.
var _ l1.Embedder = (llm.Embedder)(nil)

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
	embedder, err := embedTier(registry)
	if err != nil {
		return nil, err
	}
	return &Distiller{
		pool:     pool,
		events:   l0.New(pool),
		docs:     l1.New(pool),
		tier:     tier,
		embedder: embedder,
		resolver: resolver,
		repo:     cfg.Repo,
		timeout:  CallTimeout,
	}, nil
}

// embedTier is the `embed` tier, or nil where the configuration names none.
//
// A configuration with no embed tier is the shipped one (ADR-0005 ships no
// embedding provider), and it is not an error: documents are written without a
// vector and search finds them by their words alone. What is an error is a tier
// that produces vectors of the wrong width — that is a migration and a re-embed
// of every row rather than a configuration edit, so it stops the process at
// startup instead of writing rows the column would refuse.
func embedTier(registry llm.Registry) (llm.Embedder, error) {
	embedder, err := registry.Embedder()
	if errors.Is(err, llm.ErrTierNotConfigured) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := llm.CheckDimensions(embedder, l1.EmbeddingDimensions); err != nil {
		return nil, err
	}
	return embedder, nil
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
	// Asserting reports that the write enqueued an `assert` job: the document
	// changed and its outcome enters the assertion pipeline.
	Asserting bool
	// Embedded reports that the document was given the vector its text asks
	// for. It is false where there is no embed tier configured, and where the
	// document already had one — which is the ordinary outcome, because a
	// re-distillation that changes nothing leaves the vector standing.
	Embedded bool
}

// Handle is the queue handler for the `distill` kind. It is safe to run twice
// on one job, which is what at-least-once delivery requires (ADR-0007).
func (d *Distiller) Handle(ctx context.Context, job queue.Job) error {
	result, err := d.Distill(ctx, job.TargetID)
	if err != nil {
		return err
	}
	if !result.Superseded {
		// A job that wrote nothing still finished the document's rebuild, and
		// an operator deletion waiting on it would otherwise wait for good.
		if err := d.settled(ctx, job.TargetID); err != nil {
			return err
		}
	}
	log := telemetry.Logger(ctx)
	switch {
	case result.Skipped:
		log.DebugContext(ctx, "nothing to distil", "l1_id", result.DocID)
	case result.Written:
		log.InfoContext(ctx, "document distilled", "l1_id", result.DocID,
			"redacted", result.Redacted, "embedded", result.Embedded, "asserting", result.Asserting)
	case result.Deleted:
		log.InfoContext(ctx, "derived document removed", "l1_id", result.DocID)
	case result.Superseded:
		log.InfoContext(ctx, "distillation dropped: the conversation moved while the model was answering",
			"l1_id", result.DocID)
	default:
		log.DebugContext(ctx, "document unchanged", "l1_id", result.DocID, "embedded", result.Embedded)
	}
	return nil
}

// settled records a document's rebuild as it stands, for a job whose outcome
// wrote no transaction of its own: re-distilled if it is still in L1, deleted
// if it is not. A rebuild already recorded is left as it was.
func (d *Distiller) settled(ctx context.Context, docID string) error {
	_, err := d.docs.Get(ctx, docID)
	if err != nil && !errors.Is(err, l1.ErrNotFound) {
		return err
	}
	redistilled, deleted := []string{docID}, []string(nil)
	if err != nil {
		redistilled, deleted = nil, redistilled
	}
	return pgx.BeginFunc(ctx, d.pool, func(tx pgx.Tx) error {
		return rebuilt(ctx, tx, redistilled, deleted)
	})
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
	if container, start, ok := l1.ParseChatWindowKey(artifact); ok {
		return d.distillChatWindow(ctx, result, source, artifact, container, start)
	}

	roots, err := d.events.Current(ctx, l0.ListOptions{
		Filter: l0.Filter{Source: source, Artifact: artifact},
		Limit:  1,
	})
	if err != nil {
		return Result{}, fmt.Errorf("reading %s: %w", docID, err)
	}
	if len(roots) == 0 {
		// Either the artifact was retracted at the source — a tombstone hides
		// its earlier revisions — or nothing was ever ingested under this id. A
		// document derived from nothing is not a document, and leaving one
		// standing would serve what the source deleted
		// (docs/design.md#deletion-and-provenance).
		deleted, err := d.deleteConversation(ctx, source, artifact, docID)
		if err != nil {
			return Result{}, err
		}
		result.Deleted = deleted
		result.Skipped = !deleted
		return result, nil
	}
	root := roots[0]
	if root.Kind == connector.KindTranscript || root.Payload.BaseKind == connector.KindTranscript {
		return d.distillMeeting(ctx, result, root)
	}
	if root.Kind == connector.KindDocument || root.Payload.BaseKind == connector.KindDocument {
		return d.distillWikiSections(ctx, result, root)
	}
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

	repo, err := d.referenceRepo(ctx)
	if err != nil {
		return Result{}, err
	}
	doc, err := l1.Build(l1.Input{
		Root:     root,
		Children: children,
		Resolver: d.resolver,
		Repo:     repo,
	})
	if errors.Is(err, l1.ErrNotDistilled) {
		result.Skipped = true
		return result, nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("building %s: %w", docID, err)
	}
	if doc.Kind == l1.KindChatThread {
		return d.writeThread(ctx, result, source, artifact, root.Payload.Container.NativeID, doc, children)
	}
	return d.writeDocument(ctx, result, source, artifact, root.Payload.Container.NativeID, doc, nil)
}

// referenceRepo is the current vocabulary for extracting L1 references.
// Decisions are read for each build so an operator's confirmation takes effect
// on the next distillation without restarting the service.
func (d *Distiller) referenceRepo(ctx context.Context) (config.Repo, error) {
	confirmed, err := l2.New(d.pool).ConfirmedAliases(ctx)
	if err != nil {
		return config.Repo{}, err
	}
	repo := d.repo
	repo.Code = slices.Clone(d.repo.Code)
	owners := map[string]map[string]bool{}
	learned := map[string]bool{}
	add := func(id, name string) {
		key := l2.NormalizeAlias(name)
		if key == "" {
			return
		}
		if owners[key] == nil {
			owners[key] = map[string]bool{}
		}
		owners[key][id] = true
	}
	for _, e := range repo.Code {
		add(e.ID, e.Name)
		for _, name := range e.Aliases {
			add(e.ID, name)
		}
		for _, name := range confirmed[e.ID] {
			add(e.ID, name)
			learned[l2.NormalizeAlias(name)] = true
		}
	}
	for i := range repo.Code {
		e := &repo.Code[i]
		e.Aliases = slices.Clone(e.Aliases)
		if learned[l2.NormalizeAlias(e.Name)] && len(owners[l2.NormalizeAlias(e.Name)]) > 1 {
			e.Name = ""
		}
		e.Aliases = slices.DeleteFunc(e.Aliases, func(name string) bool {
			return learned[l2.NormalizeAlias(name)] && len(owners[l2.NormalizeAlias(name)]) > 1
		})
		for _, name := range confirmed[e.ID] {
			if len(owners[l2.NormalizeAlias(name)]) == 1 {
				e.Aliases = append(e.Aliases, name)
			}
		}
	}
	return repo, nil
}

// chatWindow reads current revisions in one indexed container/time range.
func (d *Distiller) chatWindowMessages(ctx context.Context, source, key, container string, start time.Time) (l1.Document, []connector.Event, error) {
	events, err := d.events.Current(ctx, l0.ListOptions{
		Filter: l0.Filter{Source: source, BaseKind: connector.KindMessage, Container: container, Since: start, Before: start.Add(l1.ChatWindow)},
		Limit:  l0.MaxLimit,
	})
	if err != nil {
		return l1.Document{}, nil, err
	}
	if len(events) >= l0.MaxLimit {
		return l1.Document{}, nil, fmt.Errorf("chat window %s has at least %d messages", key, l0.MaxLimit)
	}
	var messages []connector.Event
	for _, ev := range events {
		if ev.Payload.Thread == "" && ev.Payload.Container.Kind == connector.ContainerChannel {
			messages = append(messages, ev)
		}
	}
	repo, err := d.referenceRepo(ctx)
	if err != nil {
		return l1.Document{}, nil, err
	}
	doc, err := l1.BuildChatWindow(key, messages, d.resolver, repo)
	return doc, messages, err
}

func (d *Distiller) chatWindow(ctx context.Context, source, key, container string, start time.Time) (l1.Document, error) {
	doc, _, err := d.chatWindowMessages(ctx, source, key, container, start)
	return doc, err
}

func (d *Distiller) distillChatWindow(ctx context.Context, result Result, source, key, container string, start time.Time) (Result, error) {
	doc, messages, err := d.chatWindowMessages(ctx, source, key, container, start)
	if errors.Is(err, l1.ErrNotDistilled) {
		deleted, err := d.deleteConversation(ctx, source, key, result.DocID)
		if err != nil {
			return Result{}, err
		}
		result.Deleted, result.Skipped = deleted, !deleted
		return result, nil
	}
	if err != nil {
		return Result{}, err
	}
	return d.writeThread(ctx, result, source, key, container, doc, messages)
}

func (d *Distiller) deleteConversation(ctx context.Context, source, artifact, docID string) (bool, error) {
	var deleted bool
	err := pgx.BeginFunc(ctx, d.pool, func(tx pgx.Tx) error {
		var err error
		deleted, err = l1.New(tx).Delete(ctx, docID)
		if err != nil {
			return err
		}
		var removedIDs []string
		if deleted {
			removedIDs = append(removedIDs, docID)
		}
		ids, err := deleteDerived(ctx, tx, `DELETE FROM l1_docs WHERE source = $1 AND kind = $2 AND left(source_native_id, length($3)) = $3`, source, l1.KindChatBurst, l1.BurstPrefix(artifact))
		if err != nil {
			return err
		}
		removedIDs = append(removedIDs, ids...)
		ids, err = deleteDerived(ctx, tx, `DELETE FROM l1_docs WHERE source = $1 AND kind = $2 AND left(source_native_id, length($3)) = $3`, source, l1.KindWikiSection, l1.WikiSectionPrefix(artifact))
		if err != nil {
			return err
		}
		removedIDs = append(removedIDs, ids...)
		ids, err = deleteDerived(ctx, tx, `DELETE FROM l1_docs WHERE source = $1 AND kind = $2 AND left(source_native_id, length($3)) = $3`, source, l1.KindMeetingSegment, l1.MeetingSegmentPrefix(artifact))
		if err != nil {
			return err
		}
		removedIDs = append(removedIDs, ids...)
		deleted = len(removedIDs) > 0
		if err := l2.EnqueueDeletedEvidence(ctx, tx, removedIDs); err != nil {
			return err
		}
		// The document is gone whether this job removed it or an earlier one
		// did, and an operator deletion that queued it is told so.
		return rebuilt(ctx, tx, nil, append(removedIDs, docID))
	})
	if err != nil {
		return false, fmt.Errorf("removing conversation %s: %w", docID, err)
	}
	return deleted, nil
}

// rebuilt tells any operator deletion that queued these documents that they
// were re-distilled or deleted, and redacts the L2 text the deletion took the
// ground from (l2.RedactDeleted). It runs in the transaction that wrote or
// removed them. A document no deletion is waiting on costs one insert that
// matches nothing.
func rebuilt(ctx context.Context, tx pgx.Tx, redistilled, deleted []string) error {
	recorded, err := l0.RecordRebuilt(ctx, tx, redistilled, deleted)
	if err != nil || recorded == 0 {
		return err
	}
	return l2.RedactDeleted(ctx, tx, append(slices.Clone(redistilled), deleted...))
}

// deleteDerived returns the removed L1 ids so their graph repair jobs can be
// enqueued in the same transaction.
func deleteDerived(ctx context.Context, tx pgx.Tx, sql string, args ...any) ([]string, error) {
	rows, err := tx.Query(ctx, sql+` RETURNING id`, args...)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// distillWikiSections models each bounded part independently, then reconciles
// the entire derived set in one transaction. A failed call cannot leave half a
// new page current or retract the prior version of an unchanged section.
func (d *Distiller) distillWikiSections(ctx context.Context, result Result, root connector.Event) (Result, error) {
	repo, err := d.referenceRepo(ctx)
	if err != nil {
		return Result{}, err
	}
	sections, err := l1.BuildWikiSections(root, d.resolver, repo)
	if err != nil {
		return Result{}, fmt.Errorf("building sections of %s: %w", result.DocID, err)
	}
	for i := range sections {
		previous, err := d.docs.Get(ctx, sections[i].ID)
		if err != nil && !errors.Is(err, l1.ErrNotFound) {
			return Result{}, err
		}
		if err == nil && previous.Kind == l1.KindWikiSection && previous.RawText == sections[i].RawText {
			if previous.Source.URL == sections[i].Source.URL && slices.Equal(previous.ACL, sections[i].ACL) &&
				slices.Equal(previous.Participants, sections[i].Participants) && slices.Equal(previous.References, sections[i].References) && slices.Equal(previous.Scope, sections[i].Scope) {
				// The prior L0 revision still proves this exact text, so an edit to
				// another section need not rewrite this row or its outcome.
				sections[i] = previous.Document
				continue
			}
			sections[i], _, err = sections[i].WithBody(previous.Body)
			if err != nil {
				return Result{}, err
			}
			continue
		}
		body, err := d.distil(ctx, sections[i])
		if err != nil {
			return Result{}, err
		}
		sections[i], _, err = sections[i].WithBody(body)
		if err != nil {
			return Result{}, err
		}
	}
	current, err := d.events.Current(ctx, l0.ListOptions{Filter: l0.Filter{Source: root.Source, Artifact: root.Payload.Artifact}, Limit: 1})
	if err != nil {
		return Result{}, fmt.Errorf("re-reading %s: %w", result.DocID, err)
	}
	if len(current) != 1 || current[0].NativeID != root.NativeID {
		result.Superseded = true
		return result, nil
	}
	ids := make([]string, len(sections))
	for i, section := range sections {
		ids[i] = section.ID
	}
	err = pgx.BeginFunc(ctx, d.pool, func(tx pgx.Tx) error {
		removed, err := deleteDerived(ctx, tx, `DELETE FROM l1_docs WHERE source = $1 AND kind = $2 AND left(source_native_id, length($3)) = $3 AND NOT (id = ANY($4))`, root.Source, l1.KindWikiSection, l1.WikiSectionPrefix(root.Payload.Artifact), ids)
		if err != nil {
			return fmt.Errorf("reconciling sections of %s: %w", result.DocID, err)
		}
		result.Deleted = len(removed) > 0
		if err := l2.EnqueueDeletedEvidence(ctx, tx, removed); err != nil {
			return err
		}
		for _, section := range sections {
			written, err := l1.New(tx).Put(ctx, section)
			if err != nil {
				return err
			}
			result.Written = result.Written || written
			if written {
				asserting, err := l2.EnqueueAssertion(ctx, tx, d.repo, section, root.Payload.Container.NativeID)
				if err != nil {
					return err
				}
				result.Asserting = result.Asserting || asserting
				if !asserting {
					if err := l2.EnqueueNonassertingEvidence(ctx, tx, section.ID); err != nil {
						return err
					}
				}
			}
		}
		return rebuilt(ctx, tx, ids, removed)
	})
	if err != nil {
		return Result{}, err
	}
	for _, section := range sections {
		embedded, err := d.embed(ctx, section.ID)
		if err != nil {
			return Result{}, err
		}
		result.Embedded = result.Embedded || embedded
	}
	result.Skipped = len(sections) == 0 && !result.Deleted
	return result, nil
}

func (d *Distiller) writeThread(ctx context.Context, result Result, source, artifact, container string, doc l1.Document, messages []connector.Event) (Result, error) {
	var eligible []connector.Event
	for _, ev := range messages {
		if ev.Kind == connector.KindMessage || ev.Payload.BaseKind == connector.KindMessage {
			eligible = append(eligible, ev)
		}
	}
	repo, err := d.referenceRepo(ctx)
	if err != nil {
		return Result{}, err
	}
	bursts, err := l1.BuildChatBursts(artifact, eligible, d.resolver, repo)
	if err != nil {
		return Result{}, err
	}
	return d.writeDocument(ctx, result, source, artifact, container, doc, bursts)
}

func (d *Distiller) writeDocument(ctx context.Context, result Result, source, artifact, container string, doc l1.Document, bursts []l1.Document) (Result, error) {
	docID := result.DocID

	body, err := d.distil(ctx, doc)
	if err != nil {
		return Result{}, err
	}
	doc, redacted, err := doc.WithBody(body)
	if err != nil {
		return Result{}, err
	}
	for i := range bursts {
		burstBody, err := d.distil(ctx, bursts[i])
		if err != nil {
			return Result{}, err
		}
		// The whole thread owns the conclusion. A burst preserves its tangent
		// only, and must not assert the same decision a second time.
		burstBody.Outcome, burstBody.Question = "", ""
		burstBody.OutcomeKind = l1.OutcomeNone
		burstBody.OpenQuestions = nil
		bursts[i], _, err = bursts[i].WithBody(burstBody)
		if err != nil {
			return Result{}, err
		}
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

	// The document and the assert job it causes are one transaction (ADR-0007):
	// either a document whose outcome enters the assertion pipeline is stored
	// with its job, or neither is and the retry writes both. Only a write that
	// changed the row asks for one — a re-distillation that changed nothing has
	// nothing new for L2 to read.
	err = pgx.BeginFunc(ctx, d.pool, func(tx pgx.Tx) error {
		written, err := l1.New(tx).Put(ctx, doc)
		if err != nil {
			return err
		}
		if written {
			result.Written = true
			if err := d.voteAliases(ctx, tx, doc); err != nil {
				return err
			}
			result.Asserting, err = l2.EnqueueAssertion(ctx, tx, d.repo, doc, container)
			if err != nil {
				return err
			}
			if !result.Asserting {
				if err := l2.EnqueueNonassertingEvidence(ctx, tx, doc.ID); err != nil {
					return err
				}
			}
		}
		if doc.Kind != l1.KindChatThread {
			return rebuilt(ctx, tx, []string{docID}, nil)
		}
		prefix := l1.BurstPrefix(artifact)
		ids := make([]string, len(bursts))
		for i, burst := range bursts {
			ids[i] = burst.ID
		}
		removed, err := deleteDerived(ctx, tx, `DELETE FROM l1_docs WHERE source = $1 AND kind = $2 AND left(source_native_id, length($3)) = $3 AND NOT (id = ANY($4))`, source, l1.KindChatBurst, prefix, ids)
		if err != nil {
			return fmt.Errorf("reconciling bursts of %s: %w", docID, err)
		}
		if err := l2.EnqueueDeletedEvidence(ctx, tx, removed); err != nil {
			return err
		}
		for _, burst := range bursts {
			changed, err := l1.New(tx).Put(ctx, burst)
			if err != nil {
				return err
			}
			result.Written = result.Written || changed
		}
		return rebuilt(ctx, tx, append(ids, docID), removed)
	})
	if err != nil {
		return Result{}, err
	}
	result.Redacted = redacted

	// The vector comes after the row, and asks the table what it needs rather
	// than assuming: Put clears the embedding whenever it writes different
	// text, so what needs embedding is what has no vector — a document this
	// job just changed, one written before an embed tier was configured, or one
	// whose embedding failed last time. A re-distillation that changed nothing
	// makes no embedding call.
	//
	// A failure here fails the job, and the retry re-distils from scratch: a
	// document that is in the table but not in the vector index is invisible to
	// half of search, and the model call the retry spends is the price of that
	// not being a silent state. The tier has already retried by then
	// (internal/llm), so a failure that reaches here is a persistent one.
	embedded, err := d.embed(ctx, docID)
	if err != nil {
		return Result{}, err
	}
	result.Embedded = embedded
	for _, burst := range bursts {
		if _, err := d.embed(ctx, burst.ID); err != nil {
			return Result{}, err
		}
	}
	return result, nil
}

// voteAliases uses the existing distill result and structured references. A
// PR's leading system reference is the most specific touched code entity.
func (d *Distiller) voteAliases(ctx context.Context, tx pgx.Tx, doc l1.Document) error {
	switch doc.Kind {
	case l1.KindChatThread:
		if len(doc.Body.CodeNames) == 0 {
			return nil
		}
		for _, ref := range doc.References {
			if ref.Type != l1.RefPR {
				continue
			}
			prs, err := l1.New(tx).PRsByReference(ctx, ref.ID)
			if err != nil {
				return err
			}
			for _, pr := range prs {
				if err := voteForPair(ctx, tx, doc, pr.Document); err != nil {
					return err
				}
			}
		}
	case l1.KindPR:
		threads, err := l1.New(tx).ThreadsReferencingPR(ctx, doc.Source.NativeID)
		if err != nil {
			return err
		}
		for _, thread := range threads {
			if err := voteForPair(ctx, tx, thread.Document, doc); err != nil {
				return err
			}
		}
	}
	return nil
}

func voteForPair(ctx context.Context, tx pgx.Tx, thread, pr l1.Document) error {
	// References orders touched entities by path specificity. A name without
	// an explicit target is assigned only to the leading, most specific one.
	for _, touched := range pr.References {
		if touched.Type != l1.RefSystem {
			continue
		}
		for _, name := range thread.Body.CodeNames {
			if l2.NormalizeAlias(name) == "" {
				continue
			}
			if err := l2.New(tx).VoteAlias(ctx, touched.ID, name, thread.ID, pr.ID, thread.ACL, pr.ACL); err != nil {
				return err
			}
		}
		break
	}
	return nil
}

// embed gives a document the vector its text asks for, where there is a tier to
// make one.
func (d *Distiller) embed(ctx context.Context, docID string) (bool, error) {
	if d.embedder == nil {
		return false, nil
	}
	call, done := context.WithTimeout(ctx, d.timeout)
	defer done()
	return d.docs.Embed(call, d.embedder, docID)
}

// provenanceNow is the L0Refs a document built from L0 right now would carry,
// and nil for an artifact that would no longer make a document at all. It is
// the second half of the check described in Distill: same reads, same builder,
// so what it returns is comparable to what Build already produced.
func (d *Distiller) provenanceNow(ctx context.Context, source, artifact string) ([]string, error) {
	if container, start, ok := l1.ParseChatWindowKey(artifact); ok {
		doc, err := d.chatWindow(ctx, source, artifact, container, start)
		if errors.Is(err, l1.ErrNotDistilled) {
			return nil, nil
		}
		return doc.L0Refs, err
	}
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
