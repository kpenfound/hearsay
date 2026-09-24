package l2

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/kpenfound/hearsay/internal/connector"
)

// Reads follow the topic ledger (ADR-0021). What the operations in force make
// of a scope — which topics stand and which topic each stance is on — is the
// ledger replayed over the rows ([ScopeState.arrange]), and every read serves
// that: a topic merged away reads as the topic it went into, a split's topic
// reads with exactly the stances it moved, and a stance carries the topic it
// is on now. Nothing here writes a topic or a stance row, so a read after an
// undo is the read before the operation.
//
// Only the scopes whose ledger names a topic being read are replayed, and
// only over the rows their operations name: a topic no operation touches is
// what its row says, at the cost of one indexed read of the ledger.

// StanceRef names a stance and the topic it is on now.
type StanceRef struct {
	Stance string
	Topic  string
}

// projection is what the ledgers of some scopes make of the topics their
// operations touch. A topic or a stance not in it is what its row says.
type projection struct {
	// effective maps each topic an operation names — a row, or a split's
	// topic — to the topic it is now.
	effective map[string]string
	// placed maps each stance on one of those rows to the topic it is on now.
	placed map[string]string
	// members is, for each topic an operation shaped, the rows whose stances
	// it holds or that were merged into it, itself first where it is a row.
	members map[string][]string
	// rows are the topic rows the operations name.
	rows map[string]Topic
	// splits are the splits in force, by the topic each created.
	splits map[string]Operation
	// shaping is, for each topic now, the operations in force that shaped it,
	// oldest first.
	shaping map[string][]Operation
}

// topicOf is the topic a topic id is now.
func (p projection) topicOf(id string) string {
	if e, ok := p.effective[id]; ok {
		return e
	}
	return id
}

// place is the topic a stance written on a row is on now. A stance written
// after the ledger was read is on its row's topic as it is now.
func (p projection) place(stance, row string) string {
	if t, ok := p.placed[stance]; ok {
		return t
	}
	return p.topicOf(row)
}

// rowsOf is the rows whose stances a topic may hold.
func (p projection) rowsOf(topic string) []string {
	if m, ok := p.members[topic]; ok {
		return m
	}
	return []string{topic}
}

// project reads the ledger of every scope that has an operation naming one of
// these topics, and replays it.
func (s *Store) project(ctx context.Context, topics []string) (projection, error) {
	p := projection{
		effective: map[string]string{}, placed: map[string]string{}, members: map[string][]string{},
		rows: map[string]Topic{}, splits: map[string]Operation{}, shaping: map[string][]Operation{},
	}
	if len(topics) == 0 {
		return p, nil
	}
	var scopes []string
	if err := s.db.QueryRow(ctx, `SELECT coalesce(array_agg(DISTINCT scope ORDER BY scope), '{}')
FROM l2_topic_operations WHERE topics && $1::text[]`, sortedUnique(topics)).Scan(&scopes); err != nil {
		return projection{}, fmt.Errorf("reading which topics the ledger touches: %w", err)
	}
	for _, scope := range scopes {
		if err := s.projectScope(ctx, &p, scope); err != nil {
			return projection{}, err
		}
	}
	return p, nil
}

func (s *Store) projectScope(ctx context.Context, p *projection, scope string) error {
	ops, err := s.Operations(ctx, OperationFilter{Scope: scope})
	if err != nil {
		return err
	}
	var touched []string
	for _, op := range ops {
		if op.Kind != OperationUndo {
			touched = append(touched, op.Topics...)
		}
	}
	touched = sortedUnique(touched)
	rows, err := s.topics(ctx, `SELECT `+topicColumns+` FROM l2_topics t WHERE t.id = ANY($1) ORDER BY t.id`, touched)
	if err != nil {
		return err
	}
	state := ScopeState{Scope: scope, Stances: map[string]string{}, Operations: ops}
	for _, row := range rows {
		state.Topics = append(state.Topics, row.ID)
		p.rows[row.ID] = row
	}
	stances, err := s.db.Query(ctx, `SELECT id, topic_id FROM l2_stances WHERE topic_id = ANY($1)`, touched)
	if err != nil {
		return fmt.Errorf("reading the stances the ledger of scope %q moves: %w", scope, err)
	}
	defer stances.Close()
	for stances.Next() {
		var id, topic string
		if err := stances.Scan(&id, &topic); err != nil {
			return fmt.Errorf("reading the stances the ledger of scope %q moves: %w", scope, err)
		}
		state.Stances[id] = topic
	}
	if err := stances.Err(); err != nil {
		return fmt.Errorf("reading the stances the ledger of scope %q moves: %w", scope, err)
	}

	a := state.arrange()
	for _, t := range touched {
		p.effective[t] = a.effective(t)
	}
	member := func(topic, row string) {
		if !slices.Contains(p.members[topic], row) {
			p.members[topic] = append(p.members[topic], row)
		}
	}
	for _, row := range state.Topics {
		if a.live[row] {
			member(row, row)
		}
	}
	for _, row := range state.Topics {
		member(a.effective(row), row)
	}
	for st, t := range a.stances {
		p.placed[st] = t
		member(t, state.Stances[st])
	}
	for _, op := range ops {
		if !op.InForce() {
			continue
		}
		if op.Kind == OperationSplit && a.live[op.Topics[1]] {
			p.splits[op.Topics[1]] = op
		}
		var shaped []string
		for _, t := range op.Topics {
			if e := a.effective(t); !slices.Contains(shaped, e) {
				shaped = append(shaped, e)
				p.shaping[e] = append(p.shaping[e], op)
			}
		}
	}
	return nil
}

// Topic returns a topic as the ledger makes it now ([Store.Topics]): a topic
// merged away is the topic it went into, and a split's topic is a topic of
// its own, though it has no row. [ErrNotFound] is a topic that neither a row
// nor a split in force holds.
func (s *Store) Topic(ctx context.Context, id string) (Topic, error) {
	p, err := s.project(ctx, []string{id})
	if err != nil {
		return Topic{}, err
	}
	topics, err := s.compose(ctx, p, []string{p.topicOf(id)})
	if err != nil {
		return Topic{}, err
	}
	if len(topics) == 0 {
		return Topic{}, fmt.Errorf("%w: topic %s", ErrNotFound, id)
	}
	return topics[0], nil
}

// Topics returns every topic in one scope as the ledger makes it now, oldest
// first: the topics that stand, including the ones splits created, and none
// that was merged away.
func (s *Store) Topics(ctx context.Context, scope string) ([]Topic, error) {
	var rows []string
	if err := s.db.QueryRow(ctx, `SELECT coalesce(array_agg(id), '{}') FROM l2_topics WHERE scope = $1`, scope).Scan(&rows); err != nil {
		return nil, fmt.Errorf("listing the topics of scope %q: %w", scope, err)
	}
	return s.TopicsOver(ctx, rows)
}

// TopicsOver is every topic, as the ledger makes it now, that one of these
// topic rows is or holds stances of: a row's own topic, the topic it was
// merged into, and every split's topic that took stances from it. It is how a
// read that finds rows — by entity, by join key, by embedding — finds the
// topics they are part of. They are oldest first.
func (s *Store) TopicsOver(ctx context.Context, rows []string) ([]Topic, error) {
	if len(rows) == 0 {
		return []Topic{}, nil
	}
	p, err := s.project(ctx, rows)
	if err != nil {
		return nil, err
	}
	topics, err := s.compose(ctx, p, p.over(rows))
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(topics, func(a, b Topic) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return topics, nil
}

// over is the topics now that these rows are or hold stances of, in the order
// the rows were given.
func (p projection) over(rows []string) []string {
	var out []string
	add := func(t string) {
		if !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	for _, row := range rows {
		add(p.topicOf(row))
		for _, topic := range sortedKeys(p.members) {
			if slices.Contains(p.members[topic], row) {
				add(topic)
			}
		}
	}
	return out
}

// compose builds these topics as they are now, in the order given, leaving
// out one that is neither a row nor a split in force. A topic an operation
// shaped is about, and joined by, everything its rows are, and carries the
// operations that shaped it. A split's topic is named by its split, was
// created when it was, and has no opening document: who may read it is
// decided by its live stances ([Access.Topic]).
func (s *Store) compose(ctx context.Context, p projection, ids []string) ([]Topic, error) {
	var missing []string
	for _, id := range ids {
		for _, row := range append([]string{id}, p.rowsOf(id)...) {
			if _, ok := p.rows[row]; !ok && !slices.Contains(missing, row) {
				missing = append(missing, row)
			}
		}
	}
	rows := map[string]Topic{}
	for id, row := range p.rows {
		rows[id] = row
	}
	if len(missing) > 0 {
		found, err := s.topics(ctx, `SELECT `+topicColumns+` FROM l2_topics t WHERE t.id = ANY($1)`, missing)
		if err != nil {
			return nil, err
		}
		for _, row := range found {
			rows[row.ID] = row
		}
	}
	out := make([]Topic, 0, len(ids))
	for _, id := range ids {
		var t Topic
		if op, ok := p.splits[id]; ok {
			t = Topic{ID: id, Scope: op.Scope, Name: op.Name, CreatedAt: op.At}
		} else if row, ok := rows[id]; ok {
			t = row
		} else {
			continue
		}
		if _, shaped := p.members[id]; shaped {
			t.About, t.JoinKeys = slices.Clone(t.About), slices.Clone(t.JoinKeys)
			for _, member := range p.rowsOf(id) {
				row, ok := rows[member]
				if !ok {
					continue
				}
				t.About = append(t.About, row.About...)
				t.JoinKeys = append(t.JoinKeys, row.JoinKeys...)
				if t.ACL == nil {
					t.ACL = row.ACL
				}
			}
			t.About, t.JoinKeys = sortedUnique(orEmpty(t.About)), sortedUnique(orEmpty(t.JoinKeys))
		}
		t.Operations = slices.Clone(p.shaping[id])
		out = append(out, t)
	}
	return out, nil
}

// histories is every stance on each of these topics as they are now, keyed
// by topic, each oldest stated first, with the topic each is on now and the
// supersession edges that cross to another topic marked.
func (s *Store) histories(ctx context.Context, p projection, topics []string) (map[string][]Stance, error) {
	out := map[string][]Stance{}
	if len(topics) == 0 {
		return out, nil
	}
	want := map[string]bool{}
	var rows []string
	for _, t := range topics {
		want[t] = true
		rows = append(rows, p.rowsOf(t)...)
	}
	stances, err := s.stanceRows(ctx, `SELECT `+stanceColumns+` FROM l2_stances s WHERE s.topic_id = ANY($1)
ORDER BY s.stated_at, s.created_at, s.id`, sortedUnique(rows))
	if err != nil {
		return nil, err
	}
	for _, row := range stances {
		st := p.stance(row)
		if want[st.TopicID] {
			out[st.TopicID] = append(out[st.TopicID], st)
		}
	}
	return out, nil
}

// stance is a stance row as the ledger places it.
func (p projection) stance(row stanceRow) Stance {
	st := row.Stance
	st.TopicID = p.place(st.ID, row.TopicID)
	if st.Supersedes != "" {
		if t := p.place(st.Supersedes, row.supersedesTopic); t != st.TopicID {
			st.SupersedesTopic = t
		}
	}
	for i, n := range row.successors {
		if t := p.place(n, row.successorTopics[i]); t != st.TopicID {
			st.SupersededAcross = append(st.SupersededAcross, StanceRef{Stance: n, Topic: t})
		}
	}
	return st
}

// placed is these stance rows as the ledger places them.
func (s *Store) placed(ctx context.Context, rows []stanceRow) ([]Stance, error) {
	var topics []string
	for _, row := range rows {
		topics = append(topics, row.TopicID)
	}
	p, err := s.project(ctx, topics)
	if err != nil {
		return nil, err
	}
	out := make([]Stance, len(rows))
	for i, row := range rows {
		out[i] = p.stance(row)
	}
	return out, nil
}

// Target is the topic a new stance on a topic is written on: the topic as it
// is now ([Store.Topic]), so a stance matched to a topic merged away goes to
// the topic it went into. A split's topic has no row until its first stance
// after the split is written; Target writes it, recording acl and openedBy as
// what opened it, the way [Store.OpenTopic] does. A read does not use them:
// the split names the topic, and its live stances decide who may read it.
//
// The caller holds the scope's serial key, as for [Store.AppendStance].
func (s *Store) Target(ctx context.Context, id string, acl connector.ACL, openedBy string) (Topic, error) {
	t, err := s.Topic(ctx, id)
	if err != nil {
		return Topic{}, err
	}
	var row bool
	if err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM l2_topics WHERE id = $1)`, t.ID).Scan(&row); err != nil {
		return Topic{}, fmt.Errorf("reading whether topic %s has a row: %w", t.ID, err)
	}
	if !row {
		if _, err := s.OpenTopic(ctx, Topic{
			ID: t.ID, Scope: t.Scope, Name: t.Name, About: t.About, JoinKeys: t.JoinKeys, ACL: acl, OpenedBy: openedBy,
		}); err != nil {
			return Topic{}, err
		}
	}
	return t, nil
}

// readableBy reports whether everyone who may read a document may read a
// topic as it is now: its opening document while that exists, and otherwise
// one of its live stances with every piece of its evidence. It is the test
// topicReadableBy makes of a row, made of a topic an operation shaped.
func (s *Store) readableBy(ctx context.Context, t Topic, history []Stance, readers connector.ACL) (bool, error) {
	if t.OpenedBy != "" {
		var exists bool
		if err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM l1_docs WHERE id = $1)`, t.OpenedBy).Scan(&exists); err != nil {
			return false, fmt.Errorf("reading whether topic %s's opening document exists: %w", t.ID, err)
		}
		if exists {
			return s.EvidenceReadableBy(ctx, []string{t.OpenedBy}, readers)
		}
	}
	retired := retiredIn(history)
	for _, st := range history {
		if retired[st.ID] || st.Withdrawn {
			continue
		}
		ok, err := s.EvidenceReadableBy(ctx, st.Evidence, readers)
		if err != nil || ok {
			return ok, err
		}
	}
	return false, nil
}

// matchable is the topics these rows are part of now that a document's
// readers may read, in order, at most limit of them and none in exclude. A
// row found by a query that tested the row is tested again as the topic it is
// part of, where an operation shaped that topic.
func (s *Store) matchable(ctx context.Context, rows []Topic, readers connector.ACL, limit int, exclude []string) ([]Topic, error) {
	ids := make([]string, len(rows))
	for i, row := range rows {
		ids[i] = row.ID
	}
	p, err := s.project(ctx, ids)
	if err != nil {
		return nil, err
	}
	topics, err := s.compose(ctx, p, p.over(ids))
	if err != nil {
		return nil, err
	}
	out := []Topic{}
	for _, t := range topics {
		if len(out) == limit {
			break
		}
		if slices.Contains(exclude, t.ID) {
			continue
		}
		if _, shaped := p.members[t.ID]; shaped {
			hist, err := s.histories(ctx, p, []string{t.ID})
			if err != nil {
				return nil, err
			}
			ok, err := s.readableBy(ctx, t, hist[t.ID], readers)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
		}
		out = append(out, t)
	}
	return out, nil
}

// AssertionAppended reports whether a stance was appended from an
// `assertion` event, on whatever topic.
func (s *Store) AssertionAppended(ctx context.Context, eventID string) (bool, error) {
	var appended bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM l2_stances WHERE assertion = $1)`, eventID).Scan(&appended)
	if err != nil {
		return false, fmt.Errorf("reading whether %s was appended: %w", eventID, err)
	}
	return appended, nil
}
