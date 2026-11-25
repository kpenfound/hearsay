package assertworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/github"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/queue"
	"github.com/kpenfound/hearsay/internal/telemetry"
)

// CallTimeout bounds one document's model call, retries included.
const CallTimeout = 2 * time.Minute

// Topic matching's bounds.
const (
	// MaxCandidates is how many existing topics Jev is asked about for each
	// position of one document: every topic the matchers below find, up to
	// this many. The model is shown none of them.
	MaxCandidates = 20
	// MinTextSimilarity and MinSharedTerms are how much of a document's
	// distillation a topic's evidence must share, as lexeme sets
	// (l2.Store.TopicsByText), to be offered. Reference overlap is the first
	// matcher; this is the second, and the one a deployment with no `embed`
	// tier relies on.
	MinTextSimilarity = 0.2
	MinSharedTerms    = 3
	// MaxSimilarityDistance is the cosine distance within which a topic's
	// evidence counts as near a document by embedding, the third matcher, which
	// finds nothing without an `embed` tier.
	MaxSimilarityDistance = 0.25
)

// Asserter reads one L1 document and writes what it asserts into L2. It is the
// whole of what an `assert` job does, and a value a test can drive directly.
type Asserter struct {
	pool            *pgxpool.Pool
	docs            *l1.Store
	events          *l0.Store
	tier            llm.Completer
	repo            config.Repo
	connectors      *connector.Registry
	timeout         time.Duration
	replies         Replies
	judgments       llm.Judgments
	commitJudgments JournalCommit
}

// WithReplies sets the repliers GitHub commands are answered through, and
// returns the asserter. Without one for a source, its commands run and are
// not answered.
func (a *Asserter) WithReplies(replies Replies) *Asserter {
	a.replies = replies
	return a
}

// New builds the asserter. The `assert` tier is resolved here, so a
// configuration that names none is a startup failure.
func New(pool *pgxpool.Pool, registry llm.Registry, cfg *config.Config, registries ...*connector.Registry) (*Asserter, error) {
	if pool == nil {
		return nil, errors.New("the assertion worker needs a database")
	}
	if registry == nil {
		return nil, errors.New("the assertion worker needs the model tier registry")
	}
	tier, err := registry.Completer(llm.TierAssert)
	if err != nil {
		return nil, err
	}
	var connectors *connector.Registry
	if len(registries) > 0 {
		connectors = registries[0]
	}
	return &Asserter{
		pool:            pool,
		docs:            l1.New(pool),
		events:          l0.New(pool),
		tier:            llm.AccountCompletions(tier),
		repo:            cfg.Repo,
		connectors:      connectors,
		timeout:         CallTimeout,
		commitJudgments: l1.CommitJudgments,
	}, nil
}

// Result is what reading one document did.
type Result struct {
	DocID string
	// Skipped is a document that is gone, or whose outcome does not enter the
	// assertion pipeline.
	Skipped bool
	// Unchanged is a document whose current version has already been read. No
	// model call was made.
	Unchanged bool
	// TopicsOpened and StancesWritten count the rows this run wrote. Both are
	// zero when the document version was already asserted.
	TopicsOpened   int
	StancesWritten int
	// Positions is how many positions the answer took.
	Positions int
}

// Handle is the queue handler for the `assert` kind. The job's serial key is
// the scope it runs under.
//
// A job whose target is an `assertion` event is an agent's stance, appended
// with no model call ([AppendAssertion]). One whose target is a gesture's
// event ([l2.GestureTarget]) is a configured reaction or GitHub `/hearsay`
// command, or the deletion of one, handled by its source with no model call.
// One whose target is a topic operation's ([l2.OperationTarget]) or a gesture's
// ([l2.GestureHoldTarget]) is a hold that outlived its process, and is done.
func (a *Asserter) Handle(ctx context.Context, job queue.Job) error {
	log := telemetry.Logger(ctx)
	if strings.HasPrefix(job.TargetID, l2.GestureTarget) {
		id := strings.TrimPrefix(job.TargetID, l2.GestureTarget)
		ev, err := a.events.Get(ctx, id)
		if errors.Is(err, l0.ErrNotFound) || errors.Is(err, l0.ErrRetracted) || errors.Is(err, l0.ErrDeleted) {
			return nil
		}
		if err != nil {
			return err
		}
		src, ok := a.repo.Source(ev.Source)
		if !ok {
			return nil
		}
		if src.Type == github.Type {
			return a.githubGesture(ctx, job)
		}
		_, eligible, err := a.connectors.ReactionGestures(src)
		if err != nil {
			return err
		}
		if eligible {
			return a.applyReactionGesture(ctx, job)
		}
		return nil
	}
	if strings.HasPrefix(job.TargetID, l2.OperationTarget) || strings.HasPrefix(job.TargetID, l2.GestureHoldTarget) {
		log.InfoContext(ctx, "released a lapsed hold", "scope", job.SerialKey, "target", job.TargetID)
		return nil
	}
	if strings.HasPrefix(job.TargetID, l2.WithdrawalTarget) {
		return pgx.BeginFunc(ctx, a.pool, func(tx pgx.Tx) error {
			_, err := l2.New(tx).RerunDeletedEvidence(ctx, strings.TrimPrefix(job.TargetID, l2.WithdrawalTarget), job.SerialKey)
			return err
		})
	}
	if IsAssertion(job.TargetID) {
		written, err := AppendAssertion(ctx, a.pool, a.repo.Authority, job.TargetID, job.SerialKey)
		if err != nil {
			return err
		}
		log.InfoContext(ctx, "assertion appended", "l0_id", job.TargetID, "scope", job.SerialKey, "written", written)
		return nil
	}
	if decision, err := a.docs.Get(ctx, job.TargetID); err == nil && decision.Kind == l1.KindDecision {
		written, err := PlaceDecision(ctx, a.pool, a.repo.Authority, job.TargetID, job.SerialKey)
		if err != nil {
			return err
		}
		log.InfoContext(ctx, "decision placed", "l1_id", job.TargetID, "scope", job.SerialKey, "written", written)
		return nil
	} else if err != nil && !errors.Is(err, l1.ErrNotFound) {
		return err
	}
	result, err := a.assert(ctx, job.TargetID, job.SerialKey, jobQueueDelay(job))
	if err != nil {
		return err
	}
	switch {
	case result.Skipped:
		log.DebugContext(ctx, "nothing to assert", "l1_id", result.DocID)
	case result.Unchanged:
		log.DebugContext(ctx, "document already asserted", "l1_id", result.DocID)
	default:
		log.InfoContext(ctx, "document asserted", "l1_id", result.DocID, "scope", job.SerialKey,
			"positions", result.Positions, "topics_opened", result.TopicsOpened, "stances_written", result.StancesWritten)
	}
	return nil
}

// Assert reads one document under a scope: extracts its positions with the
// `assert` tier, finds the topics it may continue by reference overlap, then
// shared words, then embedding similarity, has Jev judge which topic each
// position continues and whether it changes the position there, and appends a
// stance or opens a topic.
//
// It is safe to run again: a document whose current version has been read is
// not read twice, and topics and stances have ids derived from what produced
// them.
func (a *Asserter) Assert(ctx context.Context, docID, scope string) (Result, error) {
	return a.assert(ctx, docID, scope, nil)
}

func jobQueueDelay(job queue.Job) *int64 {
	if job.EnqueuedAt.IsZero() || job.StartedAt.IsZero() || job.StartedAt.Before(job.EnqueuedAt) {
		return nil
	}
	return count(job.StartedAt.Sub(job.EnqueuedAt).Nanoseconds())
}

func (a *Asserter) assert(ctx context.Context, docID, scope string, queueDelay *int64) (Result, error) {
	ctx, accounting := llm.ObserveCompletions(ctx)
	started := time.Now()
	p, err := a.prepare(ctx, docID, scope)
	if err != nil {
		return Result{}, err
	}
	result := p.result
	if result.Skipped || result.Unchanged {
		return result, nil
	}
	ans, err := a.extract(ctx, p.stored.Document)
	if err != nil {
		return Result{}, err
	}
	batch, err := newJudgmentBatch(p.inputs, started, len(p.topics))
	if err != nil {
		return Result{}, err
	}
	batch.observation.QueueDelayNS = queueDelay
	placements, err := a.place(ctx, ans, p, &batch)
	if err != nil {
		return Result{}, err
	}
	batch.observation.GenerativeCalls, batch.observation.GenerativeAttempts, batch.observation.GenerativeUsage = accounting.Snapshot()

	err = pgx.BeginFunc(ctx, a.pool, func(tx pgx.Tx) error {
		if err := a.commitJudgments(ctx, tx, batch.records, nil, l1.JournalCommitOptions{}); err != nil {
			return err
		}
		if err := a.writeExtraction(ctx, tx, p.stored, scope, p.topics, placements, &result); err != nil {
			return err
		}
		return a.observeStage(ctx, tx, batch.observation, started)
	})
	if err != nil {
		return Result{}, fmt.Errorf("writing what %s asserts: %w", docID, err)
	}
	return result, nil
}

func (a *Asserter) candidates(ctx context.Context, graph *l2.Store, scope string, doc l1.Document, keys []string) ([]l2.Topic, error) {
	limit := MaxCandidates
	byKeys, err := graph.TopicsByJoinKeys(ctx, scope, keys, doc.ACL, limit)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(byKeys))
	for i, t := range byKeys {
		ids[i] = t.ID
	}
	found := byKeys
	byText, err := graph.TopicsByText(ctx, scope, doc.ID, doc.ACL, MinTextSimilarity, MinSharedTerms, limit-len(found), ids)
	if err != nil {
		return nil, err
	}
	found = append(found, byText...)
	for _, t := range byText {
		ids = append(ids, t.ID)
	}
	near, err := graph.TopicsBySimilarity(ctx, scope, doc.ID, doc.ACL, MaxSimilarityDistance, limit-len(found), ids)
	if err != nil {
		return nil, err
	}
	return append(found, near...), nil
}

func (a *Asserter) extract(ctx context.Context, doc l1.Document) (answer, error) {
	call, done := context.WithTimeout(ctx, a.timeout)
	defer done()
	resp, err := a.tier.Complete(call, RequestFor(doc, 0))
	if err != nil {
		return answer{}, fmt.Errorf("asserting %s: %w", doc.ID, err)
	}
	var ans answer
	if err := json.Unmarshal(resp.JSON, &ans); err != nil {
		return answer{}, fmt.Errorf("asserting %s: the %s answer did not decode: %w", doc.ID, llm.TierAssert, err)
	}
	return ans, nil
}

func clean(s string) string {
	s, _ = l1.Scrub(strings.TrimSpace(s))
	return strings.TrimSpace(s)
}

func authorOf(doc l1.Document) string {
	for _, p := range doc.Participants {
		if p.Role == connector.RoleAuthor {
			return p.PrincipalID
		}
	}
	return ""
}
