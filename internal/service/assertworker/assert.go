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
	pool    *pgxpool.Pool
	docs    *l1.Store
	events  *l0.Store
	tier    llm.Completer
	repo    config.Repo
	timeout time.Duration
}

// New builds the asserter. The `assert` tier is resolved here, so a
// configuration that names none is a startup failure rather than a job that
// fails an hour later (ADR-0005).
func New(pool *pgxpool.Pool, registry llm.Registry, cfg *config.Config) (*Asserter, error) {
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
	return &Asserter{
		pool:    pool,
		docs:    l1.New(pool),
		events:  l0.New(pool),
		tier:    tier,
		repo:    cfg.Repo,
		timeout: CallTimeout,
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
func (a *Asserter) Handle(ctx context.Context, job queue.Job) error {
	result, err := a.Assert(ctx, job.TargetID, job.SerialKey)
	if err != nil {
		return err
	}
	log := telemetry.Logger(ctx)
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
	result := Result{DocID: docID}
	stored, err := a.docs.Get(ctx, docID)
	if errors.Is(err, l1.ErrNotFound) {
		result.Skipped = true
		return result, nil
	}
	if err != nil {
		return Result{}, err
	}
	doc := stored.Document
	if !doc.Body.OutcomeKind.Asserts() {
		result.Skipped = true
		return result, nil
	}
	graph := l2.New(a.pool)
	if at, ok, err := graph.Asserted(ctx, docID); err != nil {
		return Result{}, err
	} else if ok && at.Equal(stored.DistilledAt) {
		result.Unchanged = true
		return result, nil
	}

	keys := l2.JoinKeys(doc)
	topics, err := a.candidates(ctx, graph, scope, doc, keys)
	if err != nil {
		return Result{}, err
	}
	candidates := make([]Candidate, len(topics))
	for i, t := range topics {
		history, err := graph.StanceHistory(ctx, t.ID)
		if err != nil {
			return Result{}, err
		}
		candidates[i] = Candidate{Name: t.Name}
		if current, ok := l2.Current(history); ok {
			candidates[i].Current = current.Position
		}
	}

	ans, err := a.extract(ctx, doc, candidates)
	if err != nil {
		return Result{}, err
	}

	err = pgx.BeginFunc(ctx, a.pool, func(tx pgx.Tx) error {
		result.TopicsOpened, result.StancesWritten, result.Positions = 0, 0, 0
		w := l2.New(tx)
		items := l2.TrackerItems(a.repo, doc)
		about := slices.Clone(doc.Scope)
		for _, item := range items {
			if _, err := w.EnsureEntity(ctx, item); err != nil {
				return err
			}
			about = append(about, item.ID)
		}
		for i, as := range ans.Assertions {
			position := clean(as.Position)
			name := clean(as.TopicName)
			if position == "" {
				continue
			}
			var topicID string
			if as.Topic == NewTopic {
				if name == "" {
					continue
				}
				topic := l2.Topic{
					ID: l2.TopicID(scope, docID, i, name), Scope: scope, Name: name,
					About: about, JoinKeys: keys, ACL: doc.ACL, OpenedBy: docID,
				}
				opened, err := w.OpenTopic(ctx, topic)
				if err != nil {
					return err
				}
				if opened {
					result.TopicsOpened++
				}
				topicID = topic.ID
			} else {
				at := -1
				for j := range topics {
					if label(j) == as.Topic {
						at = j
					}
				}
				if at < 0 {
					// The schema's enum is the labels offered, so this is an
					// answer internal/llm would have refused.
					return fmt.Errorf("asserting %s: the %s answer names topic %q, which was not offered", docID, llm.TierAssert, as.Topic)
				}
				topicID = topics[at].ID
			}
			if err := w.ExtendTopic(ctx, topicID, about, keys); err != nil {
				return err
			}
			tier := l2.TierFor(doc)
			_, written, err := w.AppendStance(ctx, l2.Stance{
				ID:       l2.StanceID(topicID, docID, position, stored.DistilledAt, tier),
				TopicID:  topicID,
				Position: position,
				Author:   authorOf(doc),
				StatedAt: doc.Time.LastActivity,
				Evidence: []string{docID},
				Tier:     tier,
				ACL:      doc.ACL,
			}, stored.DistilledAt)
			if err != nil {
				return err
			}
			result.Positions++
			if written {
				result.StancesWritten++
			}
		}
		return w.MarkAsserted(ctx, docID, stored.DistilledAt, result.Positions)
	})
	if err != nil {
		return Result{}, fmt.Errorf("writing what %s asserts: %w", docID, err)
	}
	return result, nil
}

// candidates is the topics a document may be continuing: reference overlap
// first, then embedding similarity for what is left of the budget, each only
// among topics everyone who may read the document may read.
func (a *Asserter) candidates(ctx context.Context, graph *l2.Store, scope string, doc l1.Document, keys []string) ([]l2.Topic, error) {
	byKeys, err := graph.TopicsByJoinKeys(ctx, scope, keys, doc.ACL, MaxCandidates)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(byKeys))
	for i, t := range byKeys {
		ids[i] = t.ID
	}
	near, err := graph.TopicsBySimilarity(ctx, scope, doc.ID, doc.ACL, MaxSimilarityDistance, MaxCandidates-len(byKeys), ids)
	if err != nil {
		return nil, err
	}
	return append(byKeys, near...), nil
}

// extract is the one model call.
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
