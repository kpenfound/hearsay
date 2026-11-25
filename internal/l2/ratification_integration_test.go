//go:build integration

package l2_test

import (
	"slices"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/db/dbtest"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l1/l1test"
	"github.com/kpenfound/hearsay/internal/l2"
)

func TestRatificationsReplayStandingOverTheRecordedTimes(t *testing.T) {
	pool := dbtest.Scratch(t, "hearsay_l2")
	store := l2.New(pool)
	repo := loadRepo(t, ratifiers)
	ctx := t.Context()
	base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	hour := func(h int) time.Time { return base.Add(time.Duration(h) * time.Hour) }

	putPR := func(native string, class config.ArtifactClass) string {
		t.Helper()
		doc := issueDoc("github", native)
		doc.Kind, doc.ArtifactClass = l1.KindPR, class
		doc.Time = l1.Times{Created: base, Updated: base, LastActivity: base}
		return l1test.Put(t, pool, doc)
	}
	written := func(topic l2.Topic, doc, position string, stated, h int) l2.Stance {
		t.Helper()
		st := addStance(t, store, topic, doc, position, stated)
		if _, err := pool.Exec(ctx, `UPDATE l2_stances SET created_at = $1 WHERE id = $2`, hour(h), st.ID); err != nil {
			t.Fatal(err)
		}
		return st
	}
	read := func(doc string, h int) {
		t.Helper()
		if err := store.MarkAsserted(ctx, doc, hour(h), 1); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE l2_asserted SET asserted_at = $1 WHERE doc_id = $2`, hour(h), doc); err != nil {
			t.Fatal(err)
		}
	}
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
	chat := func() string { return "l1:discord:" + dbtest.UniqueID() }

	byHand := namedTopic(t, store, "eng", "by hand")
	byHandDoc := chat()
	written(byHand, byHandDoc, "the queue takes the lock", 1, 0)
	ratify(byHandDoc, 10)

	undone := namedTopic(t, store, "eng", "ratified then undone")
	undoneDoc := chat()
	written(undone, undoneDoc, "retry forever", 1, 1)
	undo(ratify(undoneDoc, 2), 3)
	ratify(undoneDoc, 5)

	byEvidence := namedTopic(t, store, "eng", "by evidence")
	written(byEvidence, chat(), "ship it on friday", 1, 0)
	merged := putPR("acme/api#1", config.ArtifactMergedPR)
	written(byEvidence, merged, "ship it on monday", 2, 4)

	reread := namedTopic(t, store, "eng", "read again merged")
	pr := putPR("acme/api#2", config.ArtifactPullRequest)
	read(pr, 0)
	written(reread, pr, "drop the cache", 1, 0)
	putPR("acme/api#2", config.ArtifactMergedPR)
	read(pr, 6)
	written(reread, pr, "drop the cache", 2, 6)

	demoted := namedTopic(t, store, "eng", "demoted")
	landed := putPR("acme/api#3", config.ArtifactMergedPR)
	read(landed, 1)
	written(demoted, landed, "one binary", 1, 1)
	undo(demote(landed, 3), 5)

	open := namedTopic(t, store, "eng", "still open")
	written(open, chat(), "maybe later", 1, 2)
	late := namedTopic(t, store, "eng", "ratified late")
	lateDoc := chat()
	written(late, lateDoc, "later", 1, 2)
	ratify(lateDoc, 9)

	arrived := namedTopic(t, store, "eng", "arrived ratified")
	shipped := putPR("acme/api#4", config.ArtifactMergedPR)
	read(shipped, 7)
	written(arrived, shipped, "vendor it", 1, 7)

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

	away := namedTopic(t, store, "eng", "merged away")
	written(away, chat(), "a duplicate question", 0, 1)
	operate(t, pool, repo, l2.OperationRequest{Kind: l2.OperationMerge, Principal: "kyle", Into: byHand.ID, From: away.ID})

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
		if _, err := pool.Exec(ctx, `UPDATE l2_topics SET created_at = CASE WHEN id = $2 THEN $1::timestamptz ELSE $3::timestamptz END`, hour(20), open.ID, hour(0)); err != nil {
			t.Fatal(err)
		}
		split := operate(t, pool, repo, l2.OperationRequest{Kind: l2.OperationSplit, Principal: "kyle", Topic: byHand.ID,
			Name: "the other half", Stances: []string{stanceOn(t, store, byHand.ID, "a duplicate question")}})
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
