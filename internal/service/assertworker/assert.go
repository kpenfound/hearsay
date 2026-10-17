package assertworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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

// CallTimeout bounds one document's model call, retries included: shorter than
// anything that would let a rate-limited call outlive its job (internal/llm).
const CallTimeout = 2 * time.Minute

// Topic matching's bounds.
const (
	// MaxCandidates is how many existing topics one document is shown.
	MaxCandidates = 5
	// MaxSimilarityDistance is the cosine distance within which a topic's
	// evidence counts as near a document. Reference overlap is the first
	// matcher and a precise one; this is the fallback for a document that
	// shares no reference with the topic it continues, and a wide threshold
	// there would offer every topic in a scope.
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
// configuration that names none is a startup failure rather than a job that
// fails an hour later (ADR-0005).
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
	judgments, err := llm.NewJudgments(cfg.Repo.Boost, nil, nil)
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
		judgments:       judgments,
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

// Handle is the queue handler for the `assert` kind. The job's serial key is the
// scope it runs under, and it is the scope used — not one recomputed from a
// configuration that may have changed since the job was enqueued — because it
// is the key the queue is actually serializing on.
//
// A job whose target is an `assertion` event is an agent's stance, appended
// with no model call ([AppendAssertion]). One whose target is a gesture's
// event ([l2.GestureTarget]) is a configured reaction or GitHub `/hearsay`
// command, or the deletion of one, handled by its source with no model call.
// One whose
// target is a topic
// operation's ([l2.OperationTarget]) or a gesture's ([l2.GestureHoldTarget]) is
// a hold that outlived its process, and is done.
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
		// A topic operation or a gesture held the scope under this job and
		// died holding it. It committed with the job's completion or not at
		// all, so there is nothing left to do (l2.Operate, l2.RecordGesture).
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
// `assert` tier, matches each to a topic by reference overlap first and
// embedding similarity second, and appends a stance or opens a topic
// (docs/design.md#l2-write-path).
//
// It is safe to run again. A document whose current version has been read is
// not read twice, and one read again after a crash between the model call and
// the write writes the rows it would have — topics and stances have ids derived
// from what produced them, and a stance the topic already holds is not added.
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
	prepared, err := a.prepare(ctx, docID, scope)
	if err != nil {
		return Result{}, err
	}
	result := prepared.result
	if result.Skipped || result.Unchanged {
		return result, nil
	}
	stored, candidates := prepared.stored, prepared.candidates
	doc := stored.Document
	baseline, err := a.extract(ctx, doc, candidates)
	if err != nil {
		return Result{}, err
	}
	// One deadline for the entire optional stage. Topic matching and broader
	// shortlisting must use this same context, never create a fresh budget.
	optional, cancel := NewJobBudget(ctx, a.repo.Boost)
	defer cancel()
	batch := newJudgmentBatch(prepared.inputs, started, len(prepared.topics))
	batch.observation.QueueDelayNS = queueDelay
	ans := cloneAnswer(baseline)
	working := prepared
	if a.repo.Boost.Enabled && a.judgments != nil && prepared.inputs.Hash != "" {
		batch.observation.Cohort = "shadow"
		if a.repo.Boost.Policies[llm.TaskStanceComparison].Name != "" {
			batch.observation.Cohort = "accepted"
		}
		a.compareStances(optional, &ans, prepared, &batch)
		pool, poolErr := a.preparePool(optional, prepared, scope)
		if poolErr == nil {
			batch.observation.Inputs = pool.inputs
			batch.observation.CandidateCount = l1.AddPipelineCount(batch.observation.CandidateCount, count(int64(len(pool.topics))))
			selected := a.shortlist(optional, prepared, pool, &batch)
			if !slices.EqualFunc(selected.topics, prepared.topics, func(a, b l2.Topic) bool { return a.ID == b.ID }) && optional.Err() == nil {
				alternate, extractErr := a.extract(optional, doc, selected.candidates)
				if extractErr == nil {
					ans, working = alternate, selected
					batch.observation.Cohort = "accepted"
					a.compareStances(optional, &ans, working, &batch)
				}
			}
		}
		if !a.matchTopics(optional, &ans, working, &batch) || optional.Err() != nil || batch.refused {
			ans, working = cloneAnswer(baseline), prepared
			batch.observation.Cohort = "baseline"
		}
	}

	batch.observation.GenerativeCalls, batch.observation.GenerativeAttempts, batch.observation.GenerativeUsage = accounting.Snapshot()
	err = pgx.BeginFunc(ctx, a.pool, func(tx pgx.Tx) error {
		write := func(tx pgx.Tx, extraction answer, p preparedAssertion) error {
			return a.writeExtraction(ctx, tx, stored, scope, p.topics, p.candidates, extraction, &result)
		}
		// The savepoint covers BOTH the journal and consumer writes. Validate
		// judgments before changing inputs; measure the complete stage after
		// writing, then commit its observation against the post-write snapshot.
		// Any refusal, including the observation, rolls the whole pass back.
		var writeErr error
		optionalErr := pgx.BeginFunc(ctx, tx, func(sub pgx.Tx) error {
			if prepared.inputs.Hash == "" || batch.refused {
				return l1.JournalStorageFailure
			}
			if err := a.commitJudgments(ctx, sub, batch.records, nil, l1.JournalCommitOptions{}); err != nil {
				return err
			}
			if writeErr = write(sub, ans, working); writeErr != nil {
				return writeErr
			}
			return a.observeStage(ctx, sub, batch.observation, started)
		})
		if optionalErr == nil {
			return nil
		}
		if writeErr != nil {
			return writeErr
		}
		// The complete original extraction is the only input to this rebuild.
		if err := write(tx, baseline, prepared); err != nil {
			return err
		}
		// Optional recording can fail persistently. Observation refusal must
		// likewise leave these baseline writes usable.
		batch.observation.Cohort = "baseline"
		_ = a.observeStage(ctx, tx, batch.observation, started)
		return nil
	})
	if err != nil {
		return Result{}, fmt.Errorf("writing what %s asserts: %w", docID, err)
	}
	return result, nil
}

// candidates is the topics a document may be continuing: reference overlap
// first, then embedding similarity for what is left of the budget, each only
// among topics everyone who may read the document may read — decided by the
// opener while it exists, then by a surviving live stance. They are topics as
// the ledger makes them now: a topic merged away is offered as the topic it
// went into, and only once.
func (a *Asserter) candidates(ctx context.Context, graph *l2.Store, scope string, doc l1.Document, keys []string) ([]l2.Topic, error) {
	return a.candidatePool(ctx, graph, scope, doc, keys, MaxCandidates)
}

func (a *Asserter) candidatePool(ctx context.Context, graph *l2.Store, scope string, doc l1.Document, keys []string, limit int) ([]l2.Topic, error) {
	byKeys, err := graph.TopicsByJoinKeys(ctx, scope, keys, doc.ACL, limit)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(byKeys))
	for i, t := range byKeys {
		ids[i] = t.ID
	}
	near, err := graph.TopicsBySimilarity(ctx, scope, doc.ID, doc.ACL, MaxSimilarityDistance, limit-len(byKeys), ids)
	if err != nil {
		return nil, err
	}
	return append(byKeys, near...), nil
}

// extract calls the generative assert tier. A qualified shortlist may call it
// again; the original extraction always remains available for baseline recovery.
//
// The error it returns is a log line and a queue_job.last_error column, so it
// names the document and the tier and never a word of what was sent or what
// came back (ADR-0008).
func (a *Asserter) extract(ctx context.Context, doc l1.Document, candidates []Candidate) (answer, error) {
	call, done := context.WithTimeout(ctx, a.timeout)
	defer done()
	resp, err := a.tier.Complete(call, RequestFor(doc, candidates, 0))
	if err != nil {
		return answer{}, fmt.Errorf("asserting %s: %w", doc.ID, err)
	}
	var ans answer
	if err := json.Unmarshal(resp.JSON, &ans); err != nil {
		return answer{}, fmt.Errorf("asserting %s: the %s answer did not decode: %w", doc.ID, llm.TierAssert, err)
	}
	for i, as := range ans.Assertions {
		if as.Topic != NewTopic && topicIndex(as.Topic, len(candidates)) < 0 {
			return answer{}, fmt.Errorf("asserting %s: the %s answer names an unoffered topic for assertion %d", doc.ID, llm.TierAssert, i+1)
		}
		valid := as.Judgement == nil
		if as.Topic != NewTopic {
			valid = as.Judgement != nil && (*as.Judgement == l2.JudgementChanges || *as.Judgement == l2.JudgementRestates)
		}
		if !valid {
			return answer{}, fmt.Errorf("asserting %s: the %s answer has invalid judgement for assertion %d", doc.ID, llm.TierAssert, i+1)
		}
	}
	return ans, nil
}

// clean is a model's string made fit to store: trimmed, and scrubbed the way
// every string a person or a model wrote is before L1 stores it. The document
// the model read was scrubbed already; this is the net for a model that
// reconstructs something anyway.
func clean(s string) string {
	s, _ = l1.Scrub(strings.TrimSpace(s))
	return strings.TrimSpace(s)
}

// authorOf is the principal who authored the document, empty where nobody
// resolved.
func authorOf(doc l1.Document) string {
	for _, p := range doc.Participants {
		if p.Role == connector.RoleAuthor {
			return p.PrincipalID
		}
	}
	return ""
}
