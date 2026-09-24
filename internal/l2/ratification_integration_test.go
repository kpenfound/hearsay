//go:build integration

package l2_test

import (
	"slices"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
)

// Ratifications replays each topic's standing over what was recorded when:
// the clock starts the first time a topic stands unratified and stops the
// first time it stands ratified after that, by a person's gesture in force
// then or by evidence the policy ratifies on its own. Every time in the test
// is set on the rows, so the replay reads the recorded times and not the order
// the rows happened to be written in.
func TestRatificationsReplayStandingOverTheRecordedTimes(t *testing.T) {
	pool := scratchPool(t)
	store := l2.New(pool)
	repo := loadRepo(t, ratifiers)
	ctx := t.Context()
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	hour := func(h int) time.Time { return base.Add(time.Duration(h) * time.Hour) }

	putPR := func(native string, class config.ArtifactClass) string {
		t.Helper()
		doc := l1.Document{
			ID: l1.DocID("github", native), Kind: l1.KindPR, ArtifactClass: class,
			Source: l1.Source{System: "github", NativeID: native},
			L0Refs: []string{"evt:github:" + native}, Time: l1.Times{Created: base, Updated: base, LastActivity: base},
			ACL: public, Text: "text of " + native, RawText: "raw " + native,
			Body: l1.Body{Summary: "s", OutcomeKind: l1.OutcomeDecided},
		}
		if _, err := l1.New(pool).Put(ctx, doc); err != nil {
			t.Fatalf("Put(%s) = %v", doc.ID, err)
		}
		return doc.ID
	}
	// written appends a stance stated at hour stated, and records it as
	// written at hour h.
	written := func(topic l2.Topic, doc, position string, stated, h int) l2.Stance {
		t.Helper()
		st := addStance(t, store, topic, doc, position, stated)
		if _, err := pool.Exec(ctx, `UPDATE l2_stances SET created_at = $1 WHERE id = $2`, hour(h), st.ID); err != nil {
			t.Fatal(err)
		}
		return st
	}
	// read records that the assertion worker read a document's current
	// version at hour h.
	read := func(doc string, h int) {
		t.Helper()
		if err := store.MarkAsserted(ctx, doc, hour(h), 1); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE l2_asserted SET asserted_at = $1 WHERE doc_id = $2`, hour(h), doc); err != nil {
			t.Fatal(err)
		}
	}
	// gestured records a gesture as made at hour h and returns its event.
	gestured := func(req l2.GestureRequest, h int) string {
		t.Helper()
		req.Event, req.Principal = gestureEvent(), "kyle"
		g := gesture(t, pool, repo, req)
		if _, err := pool.Exec(ctx, `UPDATE l2_gestures SET created_at = $1 WHERE id = $2`, hour(h), g.ID); err != nil {
			t.Fatal(err)
		}
		return g.Event
	}
	ratify := func(doc string, h int) string {
		return gestured(l2.GestureRequest{Action: l2.GestureRatify, Documents: []string{doc}}, h)
	}
	demote := func(doc string, h int) string {
		return gestured(l2.GestureRequest{Action: l2.GestureDemote, Documents: []string{doc}}, h)
	}
	undo := func(event string, h int) {
		gestured(l2.GestureRequest{Action: l2.GestureUndo, Undoes: event}, h)
	}
	chat := func() string { return "l1:discord:" + unique() }

	// Ratified by hand at hour 10, ten hours after its stance was written.
	byHand := namedTopic(t, store, "eng", "by hand")
	byHandDoc := chat()
	written(byHand, byHandDoc, "the queue takes the lock", 1, 0)
	ratify(byHandDoc, 10)

	// A ratification stands from when it was made, whatever an undo later
	// did; the ratification made again after the undo is not the first.
	undone := namedTopic(t, store, "eng", "ratified then undone")
	undoneDoc := chat()
	written(undone, undoneDoc, "retry forever", 1, 1)
	undo(ratify(undoneDoc, 2), 3)
	ratify(undoneDoc, 5)

	// Ratified by evidence: a merged pull request's stance written at hour 4
	// outranks the chat thread's and ratifies on its own.
	byEvidence := namedTopic(t, store, "eng", "by evidence")
	written(byEvidence, chat(), "ship it on friday", 1, 0)
	merged := putPR("acme/api#1", config.ArtifactMergedPR)
	read(merged, 4)
	written(byEvidence, merged, "ship it on monday", 2, 4)

	// A pull request read open at hour 0 and read again merged at hour 6:
	// until the merged version was read, the stance rested on the open one,
	// and a merge since does not ratify it back then.
	reread := namedTopic(t, store, "eng", "read again merged")
	pr := putPR("acme/api#2", config.ArtifactPullRequest)
	read(pr, 0)
	written(reread, pr, "drop the cache", 1, 0)
	putPR("acme/api#2", config.ArtifactMergedPR)
	read(pr, 6)
	written(reread, pr, "drop the cache", 2, 6)

	// Ratified on arrival by a merged pull request, demoted at hour 3 and the
	// demotion undone at hour 5: the clock runs from the demotion to the undo.
	demoted := namedTopic(t, store, "eng", "demoted")
	landed := putPR("acme/api#3", config.ArtifactMergedPR)
	read(landed, 1)
	written(demoted, landed, "one binary", 1, 1)
	undo(demote(landed, 3), 5)

	// Never ratified, and ratified only at hour 9.
	open := namedTopic(t, store, "eng", "still open")
	written(open, chat(), "maybe later", 1, 2)
	late := namedTopic(t, store, "eng", "ratified late")
	lateDoc := chat()
	written(late, lateDoc, "later", 1, 2)
	ratify(lateDoc, 9)

	// Ratified on arrival and never otherwise.
	arrived := namedTopic(t, store, "eng", "arrived ratified")
	shipped := putPR("acme/api#4", config.ArtifactMergedPR)
	read(shipped, 7)
	written(arrived, shipped, "vendor it", 1, 7)

	// Ratified at hour 2 by a gesture whose event an operator deletion has
	// since deleted: out of force throughout, as it is for every read.
	voided := namedTopic(t, store, "eng", "voided")
	voidedDoc := chat()
	written(voided, voidedDoc, "a deleted ratification", 1, 1)
	event := ratify(voidedDoc, 2)
	if _, err := pool.Exec(ctx, `INSERT INTO l0_events (id, source, native_id, kind, artifact, occurred_at, payload, acl)
VALUES ($1, 'discord', $1, 'message', $1, $2, '{}', '[{"kind": "public"}]')`, event, hour(2)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO l0_deletions (id, operator, reason, selector, events, documents, retraction)
VALUES ('del_voided', 'kyle', 'test', '{}', ARRAY[$1], '{}', 'evt:hearsay:deletion-voided')`, event); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE l0_events SET deletion = 'del_voided' WHERE id = $1`, event); err != nil {
		t.Fatal(err)
	}

	// Merged away into byHand: it is read as the topic it went into, and has
	// no entry of its own.
	away := namedTopic(t, store, "eng", "merged away")
	// Stated before byHand's own stance, so byHand's stays the current one.
	written(away, chat(), "a duplicate question", 0, 1)
	operate(t, pool, repo, l2.OperationRequest{Kind: l2.OperationMerge, Principal: "kyle", Into: byHand.ID, From: away.ID})

	// Another scope, left out when one scope is asked for.
	elsewhere := namedTopic(t, store, "web", "elsewhere")
	written(elsewhere, chat(), "somewhere else", 1, 0)

	type want struct{ stood, unratified, ratified int }
	const never = -1
	tm := func(h int) time.Time {
		if h == never {
			return time.Time{}
		}
		return hour(h)
	}
	tests := []struct {
		name  string
		scope string
		until time.Time
		want  map[string]want
	}{
		{
			name: "every scope, no bound", scope: "",
			want: map[string]want{
				byHand.ID: {0, 0, 10}, undone.ID: {1, 1, 2}, byEvidence.ID: {0, 0, 4}, reread.ID: {0, 0, 6},
				demoted.ID: {1, 3, 5}, open.ID: {2, 2, never}, late.ID: {2, 2, 9}, arrived.ID: {7, never, never},
				voided.ID: {1, 1, never}, elsewhere.ID: {0, 0, never},
			},
		},
		{
			name: "one scope, until hour 9", scope: "eng", until: hour(9),
			want: map[string]want{
				byHand.ID: {0, 0, never}, undone.ID: {1, 1, 2}, byEvidence.ID: {0, 0, 4}, reread.ID: {0, 0, 6},
				demoted.ID: {1, 3, 5}, open.ID: {2, 2, never}, late.ID: {2, 2, never}, arrived.ID: {7, never, never},
				voided.ID: {1, 1, never},
			},
		},
		{
			name: "until hour 1: only what was written before", scope: "eng", until: hour(1),
			want: map[string]want{
				byHand.ID: {0, 0, never}, undone.ID: {never, never, never}, byEvidence.ID: {0, 0, never}, reread.ID: {0, 0, never},
				demoted.ID: {never, never, never}, open.ID: {never, never, never}, late.ID: {never, never, never},
				arrived.ID: {never, never, never}, voided.ID: {never, never, never},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := store.Ratifications(ctx, repo.Authority, tt.scope, tt.until)
			if err != nil {
				t.Fatalf("Ratifications() = %v", err)
			}
			var ids []string
			for _, r := range got {
				ids = append(ids, r.Topic)
				w, ok := tt.want[r.Topic]
				if !ok {
					t.Errorf("unexpected topic %s in scope %s", r.Topic, r.Scope)
					continue
				}
				if !r.Stood.Equal(tm(w.stood)) || !r.Unratified.Equal(tm(w.unratified)) || !r.Ratified.Equal(tm(w.ratified)) {
					t.Errorf("topic %s: stood %v, unratified %v, ratified %v; want %v, %v, %v", r.Topic,
						r.Stood, r.Unratified, r.Ratified, tm(w.stood), tm(w.unratified), tm(w.ratified))
				}
			}
			for id := range tt.want {
				if !slices.Contains(ids, id) {
					t.Errorf("topic %s is missing from %v", id, ids)
				}
			}
		})
	}

	t.Run("the policy in force now decides, not the tier on the row", func(t *testing.T) {
		// Under a policy where a merged pull request ratifies nothing, the
		// evidence topics are never ratified, and the one that arrived
		// ratified starts its clock when it first stood.
		strict := loadRepo(t, "scope: \"*\"\nratified_by:\n  principals: [kyle]\n  artifacts: []\n")
		got, err := store.Ratifications(ctx, strict.Authority, "eng", time.Time{})
		if err != nil {
			t.Fatalf("Ratifications() = %v", err)
		}
		for _, r := range got {
			switch r.Topic {
			case byEvidence.ID, reread.ID, arrived.ID:
				if !r.Ratified.IsZero() || r.Unratified.IsZero() {
					t.Errorf("topic %s under a policy no artifact ratifies: unratified %v, ratified %v", r.Topic, r.Unratified, r.Ratified)
				}
			case byHand.ID:
				if !r.Ratified.Equal(hour(10)) {
					t.Errorf("a ratification by hand under the strict policy = %v, want hour 10", r.Ratified)
				}
			}
		}
	})

	t.Run("topics opened", func(t *testing.T) {
		// Every topic opened at hour 0 but one, opened at hour 20.
		if _, err := pool.Exec(ctx, `UPDATE l2_topics SET created_at = CASE WHEN id = $2 THEN $1::timestamptz ELSE $3::timestamptz END`, hour(20), open.ID, hour(0)); err != nil {
			t.Fatal(err)
		}
		split := operate(t, pool, repo, l2.OperationRequest{Kind: l2.OperationSplit, Principal: "kyle", Topic: byHand.ID,
			Name: "the other half", Stances: []string{stanceOn(t, store, byHand.ID, "a duplicate question")}})
		// A stance landing on the split's topic gives it a row; a person made
		// that topic, and it is not counted.
		if _, err := store.Target(ctx, split.Topics[1], public, "l1:s:split"); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			name         string
			scope        string
			since, until time.Time
			want         int
		}{
			{"every scope", "", time.Time{}, time.Time{}, 11},
			{"one scope", "eng", time.Time{}, time.Time{}, 10},
			{"another scope", "web", time.Time{}, time.Time{}, 1},
			{"from hour 20", "eng", hour(20), time.Time{}, 1},
			{"before hour 20", "eng", time.Time{}, hour(20), 9},
			{"until is exclusive", "eng", hour(19), hour(20), 0},
		} {
			got, err := store.TopicsOpened(ctx, tc.scope, tc.since, tc.until)
			if err != nil || got != tc.want {
				t.Errorf("%s: TopicsOpened() = %d, %v; want %d", tc.name, got, err, tc.want)
			}
		}
	})
}

// stanceOn is the id of the stance on a topic, as the ledger makes it now,
// that takes a position.
func stanceOn(t *testing.T, store *l2.Store, topic, position string) string {
	t.Helper()
	history, err := store.StanceHistory(t.Context(), topic)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range history {
		if st.Position == position {
			return st.ID
		}
	}
	t.Fatalf("no stance %q on topic %s", position, topic)
	return ""
}
