package assertworker

import (
	"context"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/llm"
)

// writeExtraction is shared by the optional pass and a complete baseline
// rebuild after any journal refusal. No partial boosted extraction survives.
func (a *Asserter) writeExtraction(ctx context.Context, tx pgx.Tx, stored l1.Stored, scope string, topics []l2.Topic, candidates []Candidate, ans answer, result *Result) error {
	doc, docID := stored.Document, stored.ID
	keys := l2.JoinKeys(doc)
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
		// shown is whether the model was shown a position to judge this
		// one against; a position withheld from it was not.
		shown := false
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
			// A topic this answer opened before, on an earlier run, may
			// have been merged away since: its stance goes where the
			// topic went.
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
			shown = candidates[at].Current != ""
		}
		// The topic as the ledger makes it now, with a row to write on:
		// never one merged away, and a split's topic gets its row here.
		target, err := w.Target(ctx, topicID, doc.ACL, docID)
		if err != nil {
			return err
		}
		topicID = target.ID
		if err := w.ExtendTopic(ctx, topicID, about, keys); err != nil {
			return err
		}
		tier := l2.RecordedTier(a.repo.Authority.ForScope(scope), doc)
		judgement := l2.JudgementUnknown
		if as.Judgement != nil && shown {
			judgement = *as.Judgement
		}
		_, written, err := w.AppendStance(ctx, l2.Stance{
			ID:        l2.StanceID(topicID, docID, position, stored.DistilledAt, tier),
			TopicID:   topicID,
			Position:  position,
			Judgement: judgement,
			Author:    authorOf(doc),
			StatedAt:  doc.Time.LastActivity,
			Evidence:  []string{docID},
			Tier:      tier,
			ACL:       doc.ACL,
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
}
