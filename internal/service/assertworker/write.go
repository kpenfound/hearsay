package assertworker

import (
	"context"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
)

// writeExtraction writes where each position was placed: a stance on the
// candidate it continues, or a new topic and its first stance.
func (a *Asserter) writeExtraction(ctx context.Context, tx pgx.Tx, stored l1.Stored, scope string, topics []l2.Topic, placements []placement, result *Result) error {
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
	for _, pl := range placements {
		var topicID string
		if pl.at < 0 {
			topic := l2.Topic{
				ID: l2.TopicID(scope, docID, pl.index, pl.name), Scope: scope, Name: pl.name,
				About: about, JoinKeys: keys, ACL: doc.ACL, OpenedBy: docID,
			}
			opened, err := w.OpenTopic(ctx, topic)
			if err != nil {
				return err
			}
			if opened {
				result.TopicsOpened++
			}
			// A topic this document opened before, on an earlier run, may
			// have been merged away since: its stance goes where the
			// topic went.
			topicID = topic.ID
		} else {
			topicID = topics[pl.at].ID
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
		_, written, err := w.AppendStance(ctx, l2.Stance{
			ID:        l2.StanceID(topicID, docID, pl.position, stored.DistilledAt, tier),
			TopicID:   topicID,
			Position:  pl.position,
			Judgement: pl.judgement,
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
