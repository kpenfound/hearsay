package l2

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
)

// Ratification is when one topic, as the ledger makes it now, first stood at a
// stance not served as ratified, and when it first stood ratified after that
// (docs/design.md#evaluation). Every time is one Postgres recorded: a stance
// row's, a gesture's, an undo's, or the reading of a document's current
// version. A zero time is a moment that has not come by the time the replay
// was asked to stop at.
type Ratification struct {
	Topic string
	Scope string
	// Stood is the first moment the topic stood at all.
	Stood time.Time
	// Unratified is the first moment it stood at a stance not served as
	// ratified — inferred or contested. Zero for a topic that stood ratified
	// throughout.
	Unratified time.Time
	// Ratified is the first moment at or after Unratified that it stood
	// ratified. Zero while Unratified is, and for a topic not yet ratified.
	Ratified time.Time
}

// Ratifications replays where every topic in a scope stands, as the ledger
// makes the topics now, over the moments before until, and returns when each
// first stood unratified and then first stood ratified. An empty scope is
// every scope's; a zero until is no bound. The topics are in the order
// [Store.Topics] returns each scope's, scopes by id.
//
// Each moment is a [Stand] under the authority configured now, for a reader
// who may read everything, with what the database recorded up to it:
//
//   - the stances written by then, each retired only once a later reading of
//     its own origin was written;
//   - the ratifications and demotions recorded by then and not undone by
//     then; a gesture whose event an operator deletion deleted is out of force
//     throughout, as it is for every read;
//   - the artifact class and source of each document as L1 holds it now, from
//     the moment the assertion worker read that version of it
//     (`l2_asserted.asserted_at`). Before then a stance rests on an earlier
//     version whose class nothing recorded, which ratifies nothing and ranks
//     as a document L1 does not hold: a pull request's stance is not ratified
//     from the moment it was opened because the pull request has since
//     merged.
//
// The tier stored on a stance row is not read ([RecordedTier]).
func (s *Store) Ratifications(ctx context.Context, authority config.Authority, scope string, until time.Time) ([]Ratification, error) {
	var scopes []string
	if err := s.db.QueryRow(ctx, `SELECT coalesce(array_agg(DISTINCT scope ORDER BY scope), '{}')
FROM l2_topics WHERE $1 = '' OR scope = $1`, scope).Scan(&scopes); err != nil {
		return nil, fmt.Errorf("listing the scopes that hold topics: %w", err)
	}
	var topics []Topic
	for _, sc := range scopes {
		in, err := s.Topics(ctx, sc)
		if err != nil {
			return nil, err
		}
		topics = append(topics, in...)
	}
	if len(topics) == 0 {
		return []Ratification{}, nil
	}
	ids := make([]string, len(topics))
	for i, t := range topics {
		ids[i] = t.ID
	}
	histories, err := s.StanceHistories(ctx, ids)
	if err != nil {
		return nil, err
	}
	var stanceIDs, docs []string
	for _, history := range histories {
		for _, st := range history {
			stanceIDs = append(stanceIDs, st.ID)
			docs = append(docs, st.Evidence...)
		}
	}
	stanceIDs, docs = sortedUnique(stanceIDs), sortedUnique(docs)
	evidence, err := s.Evidence(ctx, docs)
	if err != nil {
		return nil, err
	}
	read, err := s.assertedAt(ctx, docs)
	if err != nil {
		return nil, err
	}
	retired, err := s.retiredAt(ctx, stanceIDs)
	if err != nil {
		return nil, err
	}
	gestures, err := s.corrections(ctx, stanceIDs)
	if err != nil {
		return nil, err
	}

	out := make([]Ratification, 0, len(topics))
	for _, t := range topics {
		r := Ratification{Topic: t.ID, Scope: t.Scope}
		history := histories[t.ID]
		policy := authority.ForScope(t.Scope)
		for _, at := range moments(history, gestures, read, until) {
			standing, ok := Stand(TierInputs{
				History:  historyAt(history, retired, at),
				Evidence: evidenceAt(evidence, read, at),
				Policy:   policy,
				Ratified: gestures.at(at, GestureRatify), Demoted: gestures.at(at, GestureDemote),
			})
			if !ok {
				continue
			}
			if r.Stood.IsZero() {
				r.Stood = at
			}
			if standing.Tier != TierRatified && r.Unratified.IsZero() {
				r.Unratified = at
			}
			if standing.Tier == TierRatified && !r.Unratified.IsZero() {
				r.Ratified = at
				break
			}
		}
		out = append(out, r)
	}
	return out, nil
}

// moments are the times a topic's standing can change at, before until,
// oldest first: a stance on it written, a gesture on one of its stances
// recorded or undone, one of its documents read in its current version.
func moments(history []Stance, gestures timedGestures, read map[string]time.Time, until time.Time) []time.Time {
	var out []time.Time
	add := func(at time.Time) {
		if !at.IsZero() && (until.IsZero() || at.Before(until)) {
			out = append(out, at)
		}
	}
	for _, st := range history {
		add(st.CreatedAt)
		for _, doc := range st.Evidence {
			add(read[doc])
		}
		for _, g := range gestures[st.ID] {
			add(g.at)
			add(g.undoneAt)
		}
	}
	slices.SortFunc(out, func(a, b time.Time) int { return a.Compare(b) })
	return slices.CompactFunc(out, time.Time.Equal)
}

// historyAt is a history as it stood at a moment: the stances written by
// then, in the order the history holds them, each retired only if a later
// reading of its own origin had been written by then.
func historyAt(history []Stance, retired map[string]time.Time, at time.Time) []Stance {
	var out []Stance
	for _, st := range history {
		if st.CreatedAt.After(at) {
			continue
		}
		by, ok := retired[st.ID]
		st.Retired = ok && !by.After(at)
		out = append(out, st)
	}
	return out
}

// evidenceAt is the evidence known at a moment: a document read in its current
// version after it is left out.
func evidenceAt(evidence map[string]Evidence, read map[string]time.Time, at time.Time) map[string]Evidence {
	out := make(map[string]Evidence, len(evidence))
	for id, ev := range evidence {
		if r, ok := read[id]; ok && r.After(at) {
			continue
		}
		out[id] = ev
	}
	return out
}

// assertedAt is when the assertion worker read each of these documents in the
// version it last read, which is the version L1 holds once it has caught up.
// A document it never read has no entry, and its class counts throughout.
func (s *Store) assertedAt(ctx context.Context, docs []string) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	if len(docs) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `SELECT doc_id, asserted_at FROM l2_asserted WHERE doc_id = ANY($1)`, docs)
	if err != nil {
		return nil, fmt.Errorf("reading when documents were asserted: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var at time.Time
		if err := rows.Scan(&id, &at); err != nil {
			return nil, fmt.Errorf("reading when documents were asserted: %w", err)
		}
		out[id] = at.UTC()
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading when documents were asserted: %w", err)
	}
	return out, nil
}

// retiredAt is when each of these stances that is retired now was retired:
// when the first later reading of its own origin was written ([RetiredSQL]).
func (s *Store) retiredAt(ctx context.Context, stances []string) (map[string]time.Time, error) {
	out := map[string]time.Time{}
	if len(stances) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `
SELECT s.id, min(n.created_at) FROM l2_stances s
JOIN l2_stances n ON n.supersedes = s.id
  AND coalesce(n.assertion, n.evidence[1]) = coalesce(s.assertion, s.evidence[1])
WHERE s.id = ANY($1) GROUP BY s.id`, stances)
	if err != nil {
		return nil, fmt.Errorf("reading when stances were retired: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var at time.Time
		if err := rows.Scan(&id, &at); err != nil {
			return nil, fmt.Errorf("reading when stances were retired: %w", err)
		}
		out[id] = at.UTC()
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading when stances were retired: %w", err)
	}
	return out, nil
}

// timedGesture is a ratify or a demote on a stance, with when it was recorded
// and, where an undo reversed it, when that was.
type timedGesture struct {
	id       int64
	action   GestureAction
	at       time.Time
	undoneAt time.Time
}

// timedGestures are the ratifies and demotes on each stance, oldest first.
type timedGestures map[string][]timedGesture

// corrections reads every ratify and demote on these stances whose event no
// operator deletion deleted, with when each was undone.
func (s *Store) corrections(ctx context.Context, stances []string) (timedGestures, error) {
	out := timedGestures{}
	if len(stances) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `
SELECT g.id, g.action, g.stances, g.created_at, u.created_at
FROM l2_gestures g LEFT JOIN l2_gestures u ON u.undoes = g.id
WHERE g.action IN ('ratify', 'demote') AND g.stances && $1::text[]
  AND NOT EXISTS (SELECT 1 FROM l0_events e WHERE e.id = g.event AND e.deletion IS NOT NULL)
ORDER BY g.id`, stances)
	if err != nil {
		return nil, fmt.Errorf("reading the gestures on stances: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var g timedGesture
		var action string
		var on []string
		var undone *time.Time
		if err := rows.Scan(&g.id, &action, &on, &g.at, &undone); err != nil {
			return nil, fmt.Errorf("reading the gestures on stances: %w", err)
		}
		g.action, g.at = GestureAction(action), g.at.UTC()
		if undone != nil {
			g.undoneAt = undone.UTC()
		}
		for _, st := range on {
			out[st] = append(out[st], g)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading the gestures on stances: %w", err)
	}
	return out, nil
}

// at is the stances whose latest gesture in force at a moment is action: what
// [Store.Corrections] would have read then.
func (gs timedGestures) at(at time.Time, action GestureAction) []string {
	var out []string
	for st, on := range gs {
		var latest timedGesture
		for _, g := range on {
			if g.at.After(at) || !g.undoneAt.IsZero() && !g.undoneAt.After(at) {
				continue
			}
			if g.id > latest.id {
				latest = g
			}
		}
		if latest.action == action {
			out = append(out, st)
		}
	}
	slices.Sort(out)
	return out
}

// TopicsOpened counts the topics the assertion worker opened in a scope at or
// after since and before until: topic rows, whether a merge has since folded
// them into another, and not the rows a split's topic gets when its first new
// stance lands, which a person made. An empty scope is every scope, and a
// zero time does not bound.
func (s *Store) TopicsOpened(ctx context.Context, scope string, since, until time.Time) (int, error) {
	var from, to *time.Time
	if !since.IsZero() {
		from = &since
	}
	if !until.IsZero() {
		to = &until
	}
	var n int
	err := s.db.QueryRow(ctx, `
SELECT count(*) FROM l2_topics t
WHERE ($1 = '' OR t.scope = $1)
  AND ($2::timestamptz IS NULL OR t.created_at >= $2) AND ($3::timestamptz IS NULL OR t.created_at < $3)
  AND NOT EXISTS (SELECT 1 FROM l2_topic_operations o WHERE o.kind = 'split' AND o.topics[2] = t.id)`,
		scope, from, to).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("counting the topics opened: %w", err)
	}
	return n, nil
}
