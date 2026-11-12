package l2

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/queue"
)

// The decision log is the decisions of a `work` source's project, each one a
// topic of its own with one chain of stances: a revision of the decision is a
// new stance that replaces the last one, and a deletion withdraws it. Nothing
// here calls a model — the topic, the position, the evidence and its class and
// source all come from the record's fields — and a decision is never matched to
// another topic, so no decision is counted twice.
//
// Whether a decision is superseded is not stored. It is read, from the
// decisions L1 holds for the project ([Store.DecisionMarks]), so it follows
// the latest revision of every held decision and cannot depend on the order the
// two sources' records were placed in.

// DecisionKey is the serial key every decision of one project is placed under,
// whichever source of the work/owner pair posted it, and the scope its topics
// belong to. It is [ScopeKey] of the project's container on the work source,
// never of the posting source (which [ScopeKey] maps an owner source to anyway),
// so that the two sources' decisions of a project can neither run at once nor
// land in different scopes: a configured scope covering the work source's
// project is the key, and `source:<work source>` otherwise. Evidence keeps the
// posting source, which is what authority reads.
func DecisionKey(repo config.Repo, d l1.Decision) string {
	return ScopeKey(repo, d.WorkSource, d.Project, nil)
}

// DecisionTopicID is the id of the topic a decision document is the only
// stance-maker of. It names the document and nothing else, so a restart under
// another scope configuration reads the same topic rather than opening a
// second.
func DecisionTopicID(docID string) string { return "topic:" + digest("decision", docID) }

// DecisionEntity is the `work:` entity a decision belongs to: its workstream's
// when it has one and its project's otherwise, named after the work source
// that holds the project even when an owner source recorded it. A decision's
// topic is about that entity alone, so a read of the entity serves the
// decisions made on it, a workstream's read also inherits its project's, as it
// does everything about an ancestor, and a project's read does not serve its
// workstreams' decisions: each is served on the entity it belongs to.
func DecisionEntity(d l1.Decision) string {
	id := "work:" + d.WorkSource + ":" + d.Project
	if d.Workstream != "" {
		id += "#" + d.Workstream
	}
	return id
}

// DecisionName is a decision topic's name: fixed by the decision's id, so a
// revised outcome does not leave the topic named after the old one.
func DecisionName(d l1.Decision) string { return "Decision " + d.ID }

// EnqueueDecision adds the job that places a decision document, on the
// project's decision key, inside the caller's transaction.
func EnqueueDecision(ctx context.Context, q queue.Querier, repo config.Repo, doc l1.Document) error {
	if doc.Body.Decision == nil {
		return fmt.Errorf("%w: document %s is not a decision", ErrInvalid, doc.ID)
	}
	_, err := queue.Enqueue(ctx, q, queue.Request{Kind: AssertKind(), TargetID: doc.ID, SerialKey: DecisionKey(repo, *doc.Body.Decision)})
	return err
}

// PlaceDecision writes what a decision document says into L2 under the
// project's decision key: its topic, about the same entities as the document,
// and a stance carrying its outcome whose evidence is the document alone, so
// the tier is the policy's for the document's class and posting source
// ([RecordedTier], [Stand]). A decision read again in a new revision replaces
// its stance on the topic, the way any document's re-read does. It reports
// whether a stance was written.
//
// The caller holds the key, and runs it in one transaction with whatever else
// the job writes. It is safe to run again.
func (s *Store) PlaceDecision(ctx context.Context, stored l1.Stored, authority config.Authority, scope string) (bool, error) {
	doc := stored.Document
	d := doc.Body.Decision
	if doc.Kind != l1.KindDecision || d == nil {
		return false, fmt.Errorf("%w: document %s is not a decision", ErrInvalid, doc.ID)
	}
	about := []string{DecisionEntity(*d)}
	topic := Topic{ID: DecisionTopicID(doc.ID), Scope: scope, Name: DecisionName(*d), About: about, ACL: doc.ACL, OpenedBy: doc.ID}
	if _, err := s.OpenTopic(ctx, topic); err != nil {
		return false, err
	}
	target, err := s.Target(ctx, topic.ID, doc.ACL, doc.ID)
	if err != nil {
		return false, err
	}
	if err := s.ExtendTopic(ctx, target.ID, about, nil); err != nil {
		return false, err
	}
	position := doc.Body.Outcome
	if len(position) > MaxPosition {
		position = strings.ToValidUTF8(position[:MaxPosition], "")
	}
	author := ""
	for _, p := range doc.Participants {
		if p.Role == connector.RoleAuthor {
			author = p.PrincipalID
			break
		}
	}
	tier := RecordedTier(authority.ForScope(scope), doc)
	_, written, err := s.AppendStance(ctx, Stance{
		ID:       StanceID(target.ID, doc.ID, position, stored.DistilledAt, tier),
		TopicID:  target.ID,
		Position: position,
		Author:   author,
		StatedAt: doc.Time.LastActivity,
		Evidence: []string{doc.ID},
		Tier:     tier,
		ACL:      doc.ACL,
	}, stored.DistilledAt)
	if err != nil {
		return false, err
	}
	if err := s.MarkAsserted(ctx, doc.ID, stored.DistilledAt, 1); err != nil {
		return false, err
	}
	return written, nil
}

// UnplacedDecisions is every decision document whose current version has not
// been placed, oldest first: what a restart enqueues so that a decision whose
// job ran out of attempts, or that was distilled before the worker ran, is
// placed. The version is compared for equality, as for [Store.Unasserted].
func (s *Store) UnplacedDecisions(ctx context.Context) ([]string, error) {
	rows, err := s.db.Query(ctx, `
SELECT d.id FROM l1_docs d
LEFT JOIN l2_asserted a ON a.doc_id = d.id
WHERE d.kind = 'decision' AND (a.doc_id IS NULL OR a.distilled_at <> d.distilled_at)
ORDER BY d.last_activity_at, d.id`)
	if err != nil {
		return nil, fmt.Errorf("listing unplaced decisions: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("listing unplaced decisions: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Decision statuses.
const (
	DecisionCurrent    = "current"
	DecisionSuperseded = "superseded"
)

// DecisionMark is what a read serves beside a decision's stance: who decided,
// where its full record is, and whether it is current or superseded and by
// which decision.
type DecisionMark struct {
	// ID is the decision's id and L1 the document holding its full record
	// (context, rationale, rejected options, links), which `get_l1` serves.
	ID, L1 string
	// Decider is who decided it.
	Decider string
	// Status is [DecisionCurrent] or [DecisionSuperseded].
	Status string
	// SupersededBy and SupersededByL1 name the decision that replaced it and its
	// document, when Status is [DecisionSuperseded].
	SupersededBy, SupersededByL1 string
}

// DecisionMarks reads the marks of decision documents for a reader. A held
// decision is superseded exactly when another held decision of the same
// project, on either source of the pair and on any workstream, names it in its
// latest revision's `supersedes`, and current otherwise. Only a successor the
// reader may read supersedes it for them; where several name it, the newest
// decides. An id that is not a decision document has no mark.
//
// The decisions of a project are the ones L1 holds with its work source and
// project in their structured fields, and `supersedes` is a decision id, so the
// resolution is the project-wide id rule intake enforces
// (docs/connector-contract.md#decision-records).
func (s *Store) DecisionMarks(ctx context.Context, reader l1.Reader, docIDs []string) (map[string]DecisionMark, error) {
	out := map[string]DecisionMark{}
	if len(docIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `
SELECT d.id, d.body->'decision'->>'id', d.body->'decision'->>'decider',
       s.id, s.body->'decision'->>'id', s.acl, s.scope
FROM l1_docs d
LEFT JOIN l1_docs s ON s.kind = 'decision' AND s.id <> d.id
  AND s.body->'decision'->>'work_source' = d.body->'decision'->>'work_source'
  AND s.body->'decision'->>'project' = d.body->'decision'->>'project'
  AND s.body->'decision'->>'supersedes' = d.body->'decision'->>'id'
WHERE d.kind = 'decision' AND d.id = ANY($1)
ORDER BY d.id, s.created_at DESC, s.id DESC`, docIDs)
	if err != nil {
		return nil, fmt.Errorf("reading decision marks: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, decision, decider string
		var succ, succDecision *string
		var raw []byte
		var scope []string
		if err := rows.Scan(&id, &decision, &decider, &succ, &succDecision, &raw, &scope); err != nil {
			return nil, fmt.Errorf("reading decision marks: %w", err)
		}
		mark, seen := out[id]
		if !seen {
			mark = DecisionMark{ID: decision, L1: id, Decider: decider, Status: DecisionCurrent}
		}
		if succ != nil && mark.Status == DecisionCurrent {
			var acl connector.ACL
			if len(raw) > 0 {
				if err := json.Unmarshal(raw, &acl); err != nil {
					return nil, fmt.Errorf("decoding the access list of %s: %w", *succ, err)
				}
			}
			if reader.Allows(acl) && reader.InReach(scope) {
				mark.Status, mark.SupersededBy, mark.SupersededByL1 = DecisionSuperseded, *succDecision, *succ
			}
		}
		out[id] = mark
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading decision marks: %w", err)
	}
	return out, nil
}
