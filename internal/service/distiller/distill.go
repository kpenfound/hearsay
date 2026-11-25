package distiller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
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

// CallTimeout bounds one distillation's model call, retries included. A wedged
// provider becomes a failed job that retries.
const CallTimeout = 2 * time.Minute

// Distiller turns one artifact or channel conversation into L1 documents. A
// long chat may also produce burst documents. It is the whole of what a
// `distill` job does; the worker loop around it is [Run]'s.
//
// It is stateless and idempotent: running it twice over an artifact nothing has
// happened to writes nothing the second time.
type Distiller struct {
	pool      *pgxpool.Pool
	events    *l0.Store
	docs      *l1.Store
	tier      llm.Completer
	embedder  llm.Embedder
	resolver  *principal.Resolver
	repo      config.Repo
	timeout   time.Duration
	judgments llm.Judgments
}

var _ l1.Embedder = (llm.Embedder)(nil)

// New builds the distiller from what the process has: the database, the model
// tier registry, Jev and the configuration.
//
// It resolves the `distill` tier here, so a tier the configuration does not
// name is a startup failure. Jev is required: every outcome is decided from its
// judgments.
func New(pool *pgxpool.Pool, registry llm.Registry, judgments llm.Judgments, cfg *config.Config) (*Distiller, error) {
	if pool == nil {
		return nil, errors.New("the distiller needs a database")
	}
	if registry == nil {
		return nil, errors.New("the distiller needs the model tier registry")
	}
	if judgments == nil {
		return nil, errors.New("the distiller needs Jev: configure llm.judgments and its API key")
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
		pool:      pool,
		events:    l0.New(pool),
		docs:      l1.New(pool),
		tier:      llm.AccountCompletions(tier),
		embedder:  embedder,
		resolver:  resolver,
		repo:      cfg.Repo,
		timeout:   CallTimeout,
		judgments: judgments,
	}, nil
}

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
	// means the rebuilt document was the one already stored.
	Written bool
	// Deleted reports that the artifact has been retracted at the source and
	// the document went with it.
	Deleted bool
	// Skipped reports an artifact that makes no document of its own.
	Skipped bool
	// Superseded reports that L0 gained events for this artifact while the
	// model was answering, so the answer was not written. The new events have a
	// job of their own.
	Superseded bool
	// Redacted names the shapes the scrub took out of what the model wrote,
	// sorted. It is the shapes and never the values.
	Redacted []string
	// Asserting reports that the write enqueued an `assert` job: the document
	// changed and its outcome enters the assertion pipeline.
	Asserting bool
	// Recorded reports that the conversation's completion record changed: it
	// names message revisions, or documents, that the one before did not.
	Recorded bool
	// Embedded reports that the document was given the vector its text asks
	// for. It is false where no embed tier is configured and where the
	// document already had one.
	Embedded bool
}

// Handle is the queue handler for the `distill` kind. It is safe to run twice
// on one job.
func (d *Distiller) Handle(ctx context.Context, job queue.Job) error {
	if !job.EnqueuedAt.IsZero() {
		ctx = context.WithValue(ctx, queueDelayKey{}, max(int64(0), time.Since(job.EnqueuedAt).Nanoseconds()))
	}
	result, err := d.Distill(ctx, job.TargetID)
	if err != nil {
		return err
	}
	if !result.Superseded {
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
// The job carries a document id and nothing else, and the document is a
// function of the artifact's current revisions, so Distill is safe to run at
// any time, in any order, twice.
func (d *Distiller) Distill(ctx context.Context, docID string) (Result, error) {
	source, artifact, err := l1.ParseDocID(docID)
	if err != nil {
		return Result{}, err
	}
	result := Result{DocID: docID}
	if thread, ok := l1.ParseConversationSegmentKey(artifact); ok {
		return d.distillConversation(ctx, result, source, thread)
	}
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
		if conversation, err := d.isConversation(ctx, source, artifact); err != nil {
			return Result{}, err
		} else if conversation {
			return d.distillConversation(ctx, result, source, artifact)
		}

		deleted, err := d.deleteConversation(ctx, source, artifact, docID)
		if err != nil {
			return Result{}, err
		}
		result.Deleted = deleted
		result.Skipped = !deleted
		return result, nil
	}
	root := roots[0]
	if l1.IsDecision(root) {
		return d.distillDecision(ctx, result, root)
	}
	if root.Kind == connector.KindTranscript || root.Payload.BaseKind == connector.KindTranscript {
		return d.distillMeeting(ctx, result, root)
	}
	if root.Kind == connector.KindDocument || root.Payload.BaseKind == connector.KindDocument {
		return d.distillWikiSections(ctx, result, root)
	}
	if _, ok := l1.KindFor(root); !ok {
		if l1.ChatRoot(root) {
			return d.distillReplies(ctx, result, source, artifact, root)
		}

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
		return d.writeThread(ctx, result, source, artifact, root.Payload.Container.NativeID, doc, children, root.Payload.Touches)
	}
	return d.writeDocument(ctx, result, source, artifact, root.Payload.Container.NativeID, doc, nil, root.Payload.Touches)
}

func (d *Distiller) distillReplies(ctx context.Context, result Result, source, artifact string, root connector.Event) (Result, error) {
	repo, err := d.referenceRepo(ctx)
	if err != nil {
		return Result{}, err
	}
	doc, replies, err := d.replies(ctx, source, root, repo)
	if errors.Is(err, l1.ErrNotDistilled) {
		deleted, err := d.deleteConversation(ctx, source, artifact, result.DocID)
		if err != nil {
			return Result{}, err
		}
		result.Deleted = deleted
		result.Skipped = !deleted
		return result, nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("building %s: %w", result.DocID, err)
	}
	return d.writeThread(ctx, result, source, artifact, root.Payload.Container.NativeID, doc, append(replies, root), root.Payload.Touches)
}

func (d *Distiller) replies(ctx context.Context, source string, root connector.Event, repo config.Repo) (l1.Document, []connector.Event, error) {
	children, err := d.events.Current(ctx, l0.ListOptions{
		Filter: l0.Filter{Source: source, Thread: root.Payload.Artifact},
		Limit:  l0.MaxLimit,
	})
	if err != nil {
		return l1.Document{}, nil, fmt.Errorf("reading the replies to %s: %w", root.Payload.Artifact, err)
	}
	if len(children) >= l0.MaxLimit {
		return l1.Document{}, nil, fmt.Errorf("%s has at least %d replies, which is more than one read returns", root.Payload.Artifact, l0.MaxLimit)
	}
	return l1.BuildChatReplies(root, children, d.resolver, repo)
}

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

	configured := make(map[string]bool, len(d.repo.Code))
	for _, e := range d.repo.Code {
		configured[e.ID] = true
	}
	seen := map[config.SourceRef]bool{}
	for _, e := range d.repo.Code {
		if e.Repo.Source == "" || e.Repo.Project == "" || seen[e.Repo] {
			continue
		}
		seen[e.Repo] = true
		id := "code:" + e.Repo.Project
		if configured[id] {
			continue
		}
		project, err := l2.New(d.pool).Entity(ctx, id)
		if errors.Is(err, l2.ErrNotFound) {
			continue
		}
		if err != nil {
			return config.Repo{}, fmt.Errorf("reading repository project %s for references: %w", id, err)
		}
		if project.Type == l2.TypeProject {
			repo.Code = append(repo.Code, config.CodeEntity{ID: id, Type: config.TypeProject, Repo: e.Repo})
		}
	}
	return repo, nil
}

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

	for _, message := range messages {
		if message.ID == doc.L0Refs[0] {
			return d.writeThread(ctx, result, source, key, container, doc, messages, message.Payload.Touches)
		}
	}
	return Result{}, fmt.Errorf("finding chat window %s root %s in its messages", key, doc.L0Refs[0])
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

		return rebuilt(ctx, tx, nil, append(removedIDs, docID))
	})
	if err != nil {
		return false, fmt.Errorf("removing conversation %s: %w", docID, err)
	}
	return deleted, nil
}

func rebuilt(ctx context.Context, tx pgx.Tx, redistilled, deleted []string) error {
	recorded, err := l0.RecordRebuilt(ctx, tx, redistilled, deleted)
	if err != nil || recorded == 0 {
		return err
	}
	return l2.RedactDeleted(ctx, tx, append(slices.Clone(redistilled), deleted...))
}

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

func (d *Distiller) distillWikiSections(ctx context.Context, result Result, root connector.Event) (Result, error) {
	repo, err := d.referenceRepo(ctx)
	if err != nil {
		return Result{}, err
	}
	sections, err := l1.BuildWikiSections(root, d.resolver, repo)
	if err != nil {
		return Result{}, fmt.Errorf("building sections of %s: %w", result.DocID, err)
	}
	runs := make(map[int]*outcomeRun)
	for i := range sections {
		previous, err := d.docs.Get(ctx, sections[i].ID)
		if err != nil && !errors.Is(err, l1.ErrNotFound) {
			return Result{}, err
		}
		if err == nil && previous.Kind == l1.KindWikiSection && previous.RawText == sections[i].RawText {
			if reflect.DeepEqual(previous.State, sections[i].State) && previous.Source.URL == sections[i].Source.URL && previous.ACL.Equal(sections[i].ACL) &&
				slices.Equal(previous.Participants, sections[i].Participants) && slices.Equal(previous.References, sections[i].References) && slices.Equal(previous.Scope, sections[i].Scope) {
				sections[i] = previous.Document
				continue
			}
			sections[i], _, err = sections[i].WithBody(previous.Body)
			if err != nil {
				return Result{}, err
			}
			continue
		}
		run, err := d.modelDocument(ctx, sections[i])
		if err != nil {
			return Result{}, err
		}
		runs[i], sections[i] = run, run.doc
	}
	current, err := d.events.Current(ctx, l0.ListOptions{Filter: l0.Filter{Source: root.Source, Artifact: root.Payload.Artifact}, Limit: 1})
	if err != nil {
		return Result{}, fmt.Errorf("re-reading %s: %w", result.DocID, err)
	}
	if len(current) != 1 || current[0].NativeID != root.NativeID {
		result.Superseded = true
		return result, nil
	}
	for _, run := range runs {
		if err := stale(run); err != nil {
			return Result{}, err
		}
	}
	ids := make([]string, len(sections))
	for i, section := range sections {
		ids[i] = section.ID
	}
	err = pgx.BeginFunc(ctx, d.pool, func(tx pgx.Tx) error {
		batch := make([]*outcomeRun, 0, len(runs))
		for _, run := range runs {
			batch = append(batch, run)
		}
		if err := commitOutcomes(ctx, tx, batch...); err != nil {
			return err
		}

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
				asserting, err := l2.EnqueueAssertion(ctx, tx, d.repo, section, root.Payload.Container.NativeID, root.Payload.Touches)
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

func (d *Distiller) writeThread(ctx context.Context, result Result, source, artifact, container string, doc l1.Document, messages []connector.Event, touches []connector.Touch) (Result, error) {
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
	return d.writeDocument(ctx, result, source, artifact, container, doc, bursts, touches)
}

func (d *Distiller) writeDocument(ctx context.Context, result Result, source, artifact, container string, doc l1.Document, bursts []l1.Document, touches []connector.Touch) (Result, error) {
	docID := result.DocID

	run, err := d.modelDocument(ctx, doc)
	if err != nil {
		return Result{}, err
	}
	doc = run.doc
	redacted := run.redacted
	runs := []*outcomeRun{run}
	for i := range bursts {
		burstRun, err := d.modelDocument(ctx, bursts[i])
		if err != nil {
			return Result{}, err
		}
		runs = append(runs, burstRun)
		bursts[i] = burstRun.doc
	}

	current, err := d.provenanceNow(ctx, source, artifact)
	if err != nil {
		return Result{}, fmt.Errorf("re-reading %s: %w", docID, err)
	}
	if !slices.Equal(current, doc.L0Refs) {
		result.Superseded = true
		return result, nil
	}
	if err := stale(runs...); err != nil {
		return Result{}, err
	}

	err = pgx.BeginFunc(ctx, d.pool, func(tx pgx.Tx) error {
		if err := commitOutcomes(ctx, tx, runs...); err != nil {
			return err
		}

		written, err := l1.New(tx).Put(ctx, doc)
		if err != nil {
			return err
		}
		if written {
			result.Written = true
			if err := d.voteAliases(ctx, tx, doc); err != nil {
				return err
			}
			result.Asserting, err = l2.EnqueueAssertion(ctx, tx, d.repo, doc, container, touches)
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

func (d *Distiller) embed(ctx context.Context, docID string) (bool, error) {
	if d.embedder == nil {
		return false, nil
	}
	call, done := context.WithTimeout(ctx, d.timeout)
	defer done()
	return d.docs.Embed(call, d.embedder, docID)
}

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
	if _, ok := l1.KindFor(roots[0]); !ok && l1.ChatRoot(roots[0]) {
		doc, _, err := d.replies(ctx, source, roots[0], d.repo)
		if errors.Is(err, l1.ErrNotDistilled) {
			return nil, nil
		}
		return doc.L0Refs, err
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
		return nil, nil //nolint:nilerr // a build that no longer succeeds is a changed provenance, not a failure
	}
	return doc.L0Refs, nil
}

func (d *Distiller) distil(ctx context.Context, doc l1.Document) (l1.Body, error) {
	a, err := d.distilAnswer(ctx, doc)
	return a.body(), err
}

func (d *Distiller) distilAnswer(ctx context.Context, doc l1.Document) (answer, error) {
	call, done := context.WithTimeout(ctx, d.timeout)
	defer done()

	req := RequestFor(doc, 0)
	resp, err := d.tier.Complete(call, req)
	if err != nil {
		return answer{}, fmt.Errorf("distilling %s: %w", doc.ID, err)
	}

	var a answer
	if err := json.Unmarshal(resp.JSON, &a); err != nil {
		return answer{}, fmt.Errorf("distilling %s: the %s answer for %s did not decode: %w", doc.ID, llm.TierDistill, req.Schema.Name, err)
	}
	return a, nil
}

func (d *Distiller) distillDecision(ctx context.Context, result Result, root connector.Event) (Result, error) {
	repo, err := d.referenceRepo(ctx)
	if err != nil {
		return Result{}, err
	}
	doc, err := l1.BuildDecision(root, d.resolver, repo)
	if errors.Is(err, l1.ErrNotDistilled) {
		deleted, err := d.deleteConversation(ctx, root.Source, root.Payload.Artifact, result.DocID)
		if err != nil {
			return Result{}, err
		}
		result.Deleted, result.Skipped = deleted, !deleted
		return result, nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("building %s: %w", result.DocID, err)
	}

	err = pgx.BeginFunc(ctx, d.pool, func(tx pgx.Tx) error {
		current, err := l0.New(tx).Current(ctx, l0.ListOptions{Filter: l0.Filter{Source: root.Source, Artifact: root.Payload.Artifact}, Limit: 1})
		if err != nil {
			return fmt.Errorf("re-reading %s: %w", result.DocID, err)
		}
		if len(current) != 1 || current[0].NativeID != root.NativeID {
			result.Superseded = true
			return nil
		}
		written, err := l1.New(tx).Put(ctx, doc)
		if err != nil {
			return err
		}
		if written {
			result.Written = true
			if err := l2.EnqueueDecision(ctx, tx, d.repo, doc); err != nil {
				return err
			}
		}
		return rebuilt(ctx, tx, []string{doc.ID}, nil)
	})
	if err != nil {
		return Result{}, err
	}
	return result, nil
}
