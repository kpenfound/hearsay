//go:build integration

package deletion_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/deletion"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/principal"
	"github.com/kpenfound/hearsay/internal/queue"
	"github.com/kpenfound/hearsay/internal/service/assertworker"
	"github.com/kpenfound/hearsay/internal/service/distiller"
)

// secret is the text the deleted issue carried, and every piece of L1 and L2
// derived from it quotes it, so a search of the rows for it is a search for
// what the deletion left behind.
const secret = "tangerine-otter-42"

const (
	src     = "gh"
	project = "acme/api"
	scope   = "gh"
)

var public = connector.ACL{{Kind: connector.ACLPublic}}

// graphWorld is an issue that quoted the secret and one that did not, their
// documents, and three topics:
//
//   - only: opened by the secret issue, and nothing else supports it;
//   - shared: opened by the secret issue, then continued by the other one,
//     whose stance superseded the first;
//   - later: opened by the other issue, where the secret issue's stance is the
//     current one.
type graphWorld struct {
	pool                     *pgxpool.Pool
	secretDoc, otherDoc      string
	only, shared, later      string
	onlyStance, sharedStance string
	laterStance              string
}

func newGraphWorld(t *testing.T) graphWorld {
	t.Helper()
	pool := scratch(t)
	ctx := t.Context()
	day := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	put := func(artifact, title string, hour int) string {
		t.Helper()
		at := day.Add(time.Duration(hour) * time.Hour)
		if _, err := l0.New(pool).Append(ctx, connector.Event{
			Source: src, NativeID: artifact, Kind: connector.KindIssue, Time: at,
			Payload: connector.Payload{
				Artifact: artifact, Title: title,
				Container: connector.Container{Kind: connector.ContainerRepository, NativeID: project},
				Author:    &connector.Identity{Source: src, Kind: connector.IdentityUser, NativeID: "u1"},
			},
			ACL: public,
		}); err != nil {
			t.Fatal(err)
		}
		doc := l1.Document{
			ID: l1.DocID(src, artifact), Kind: l1.KindIssue, ArtifactClass: config.ArtifactIssue,
			Source: l1.Source{System: src, NativeID: artifact},
			L0Refs: []string{connector.EventID(src, artifact)},
			Time:   l1.Times{Created: at, Updated: at, LastActivity: at},
			Scope:  []string{"code:" + project},
			ACL:    public, Text: title, RawText: title,
			Body: l1.Body{Summary: title, OutcomeKind: l1.OutcomeDecided},
		}
		if _, err := l1.New(pool).Put(ctx, doc); err != nil {
			t.Fatal(err)
		}
		return doc.ID
	}
	w := graphWorld{pool: pool}
	w.secretDoc = put(project+"#1", "Rotate the key "+secret+" before Friday.", 1)
	w.otherDoc = put(project+"#2", "Rotate keys on a schedule.", 2)

	graph := l2.New(pool)
	topic := func(doc, name string) string {
		t.Helper()
		tp := l2.Topic{ID: l2.TopicID(scope, doc, 0, name), Scope: scope, Name: name, ACL: public, OpenedBy: doc}
		if _, err := graph.OpenTopic(ctx, tp); err != nil {
			t.Fatal(err)
		}
		return tp.ID
	}
	stance := func(topic, doc, position string, hour int) string {
		t.Helper()
		at := day.Add(time.Duration(hour) * time.Hour)
		st, _, err := graph.AppendStance(ctx, l2.Stance{
			ID: l2.StanceID(topic, doc, position, at, l2.TierInferred), TopicID: topic, Position: position,
			StatedAt: at, Evidence: []string{doc}, Tier: l2.TierInferred, ACL: public,
		}, at)
		if err != nil {
			t.Fatal(err)
		}
		return st.ID
	}
	w.only = topic(w.secretDoc, "whether "+secret+" is rotated")
	w.onlyStance = stance(w.only, w.secretDoc, "rotate "+secret+" by Friday", 1)
	w.shared = topic(w.secretDoc, "how often keys rotate")
	w.sharedStance = stance(w.shared, w.secretDoc, "rotate "+secret+" once", 1)
	stance(w.shared, w.otherDoc, "rotate on a schedule", 2)
	w.later = topic(w.otherDoc, "the rotation schedule")
	stance(w.later, w.otherDoc, "weekly", 2)
	w.laterStance = stance(w.later, w.secretDoc, "weekly, starting with "+secret, 3)
	return w
}

var repo = config.Repo{Principals: []principal.Principal{{ID: "pat", Kind: principal.KindHuman}}}

// drain runs every job of the kind until none is left, the way a worker would,
// so that what the queue reports afterwards is what a running service leaves.
func drain(t *testing.T, pool *pgxpool.Pool, kind queue.Kind, handle func(queue.Job) error) int {
	t.Helper()
	client, err := queue.New(pool, queue.Config{Kind: kind})
	if err != nil {
		t.Fatal(err)
	}
	ran := 0
	for {
		jobs, err := client.Claim(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(jobs) == 0 {
			return ran
		}
		for _, job := range jobs {
			if err := handle(job); err != nil {
				t.Fatalf("%s job on %s: %v", kind.Name, job.TargetID, err)
			}
			if _, err := client.Complete(t.Context(), job); err != nil {
				t.Fatal(err)
			}
			ran++
		}
	}
}

// workers are the distiller and the assertion worker. Neither makes a model
// call here: a deleted document and a stance repair need none.
func workers(t *testing.T, pool *pgxpool.Pool) (distil, assert func(queue.Job) error) {
	t.Helper()
	registry, err := llm.NewFake(llm.Default(), llm.NewFixtures())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Repo = repo
	d, err := distiller.New(pool, registry, &cfg)
	if err != nil {
		t.Fatal(err)
	}
	a, err := assertworker.New(pool, registry, &cfg)
	if err != nil {
		t.Fatal(err)
	}
	return func(job queue.Job) error { return d.Handle(t.Context(), job) },
		func(job queue.Job) error { return a.Handle(t.Context(), job) }
}

// rebuild runs the distiller and the assertion worker over whatever is queued,
// until neither has anything left.
func rebuild(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	distil, assert := workers(t, pool)
	for {
		ran := drain(t, pool, distiller.JobKind(), distil)
		ran += drain(t, pool, l2.AssertKind(), assert)
		if ran == 0 {
			return
		}
	}
}

// leftovers is every row of L0, L1 and L2 that still holds the secret.
func leftovers(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(), `
SELECT 'l0 ' || id FROM l0_events e WHERE e::text LIKE '%' || $1 || '%'
UNION ALL SELECT 'l1 ' || id FROM l1_docs d WHERE d::text LIKE '%' || $1 || '%'
UNION ALL SELECT 'stance ' || id FROM l2_stances s WHERE s::text LIKE '%' || $1 || '%'
UNION ALL SELECT 'topic ' || id FROM l2_topics p WHERE p::text LIKE '%' || $1 || '%'
ORDER BY 1`, secret)
	if err != nil {
		t.Fatal(err)
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func history(t *testing.T, pool *pgxpool.Pool, topic string) map[string]l2.Stance {
	t.Helper()
	h, err := l2.New(pool).StanceHistory(t.Context(), topic)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]l2.Stance{}
	for _, st := range h {
		out[st.ID] = st
	}
	return out
}

// successor is the stance that supersedes id on the topic, or "".
func successor(h map[string]l2.Stance, id string, withdrawn bool) string {
	for _, st := range h {
		if st.Supersedes == id && st.Withdrawn == withdrawn {
			return st.ID
		}
	}
	return ""
}

func topicName(t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	var name string
	if err := pool.QueryRow(t.Context(), `SELECT name FROM l2_topics WHERE id = $1`, id).Scan(&name); err != nil {
		t.Fatal(err)
	}
	return name
}

// The acceptance criteria end to end: an operator deletes the issue that
// quoted a secret, the distiller and the assertion worker rebuild what rested
// on it, and afterwards `show` reports complete and names the stances
// superseded and the topic redacted. No row of L0, L1 or L2 holds the secret,
// while every stance id and supersession edge is still there.
func TestAnOperatorDeletionRedactsL2AndRecordsTheRebuild(t *testing.T) {
	w := newGraphWorld(t)
	ctx := t.Context()
	pool := w.pool
	sel := deletion.Selector{ArtifactSource: src, ArtifactID: project + "#1"}
	applied, err := deletion.Apply(ctx, pool, repo, sel, "a pasted key", "pat")
	if err != nil {
		t.Fatal(err)
	}

	pending, err := deletion.Show(ctx, pool, applied.Deletion)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Status != deletion.StatusRebuilding || len(pending.Documents) != 1 ||
		pending.Documents[0] != (deletion.Rebuilt{ID: w.secretDoc, Outcome: deletion.OutcomePending}) {
		t.Errorf("Show(before the rebuild) = %+v, want the document pending and the deletion rebuilding", pending)
	}

	// The distiller's half: the document is deleted, and what was already
	// superseded goes with it, as does the name of the topic nothing else
	// supports. A current stance keeps its text until its withdrawal.
	distil, _ := workers(t, pool)
	drain(t, pool, distiller.JobKind(), distil)
	if st := history(t, pool, w.shared)[w.sharedStance]; st.Position != l2.Redacted {
		t.Errorf("after the distiller, the superseded stance's position = %q, want it redacted", st.Position)
	}
	if st := history(t, pool, w.only)[w.onlyStance]; st.Position == l2.Redacted {
		t.Error("the distiller redacted a current stance before its withdrawal superseded it")
	}
	if got := topicName(t, pool, w.only); got != l2.Redacted {
		t.Errorf("after the distiller, only's name = %q, want it redacted", got)
	}
	if mid, err := deletion.Show(ctx, pool, applied.Deletion); err != nil || mid.Status != deletion.StatusRebuilding {
		t.Errorf("Show(withdrawals queued) = %+v, %v, want rebuilding", mid, err)
	}

	rebuild(t, pool)

	rec, err := deletion.Show(ctx, pool, applied.Deletion)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != deletion.StatusComplete {
		t.Errorf("Status = %s, want complete", rec.Status)
	}
	if len(rec.Documents) != 1 || rec.Documents[0].ID != w.secretDoc || rec.Documents[0].Outcome != l0.RebuiltDeleted || rec.Documents[0].At == nil {
		t.Errorf("Documents = %+v, want %s deleted", rec.Documents, w.secretDoc)
	}
	wantStances := []string{w.onlyStance, w.sharedStance, w.laterStance}
	slices.Sort(wantStances)
	if !slices.Equal(rec.Stances, wantStances) {
		t.Errorf("Stances = %v, want %v", rec.Stances, wantStances)
	}
	if !slices.Equal(rec.Topics, []string{w.only}) {
		t.Errorf("Topics = %v, want only %s: the others have a surviving document", rec.Topics, w.only)
	}
	if rec.Operator != "pat" || rec.Reason != "a pasted key" || rec.Selector != sel || rec.Retraction != applied.Retraction ||
		!slices.Equal(rec.Events, applied.Events) {
		t.Errorf("Show() = %+v, want the record Apply wrote (%+v)", rec, applied)
	}
	list, err := deletion.List(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != rec.ID || list[0].Status != deletion.StatusComplete || !list[0].Time.Equal(rec.Time) ||
		list[0].Operator != "pat" || list[0].Reason != rec.Reason || list[0].Selector != sel {
		t.Errorf("List() = %+v, want %+v", list, rec.Summary)
	}

	if left := leftovers(t, pool); len(left) != 0 {
		t.Errorf("rows still holding the deleted text: %v", left)
	}

	// History keeps every id and edge; the text is what went.
	for _, tt := range []struct{ topic, stance string }{
		{w.only, w.onlyStance}, {w.shared, w.sharedStance}, {w.later, w.laterStance},
	} {
		h := history(t, pool, tt.topic)
		st, ok := h[tt.stance]
		if !ok {
			t.Errorf("stance %s is gone from %s's history", tt.stance, tt.topic)
			continue
		}
		if st.Position != l2.Redacted || !slices.Equal(st.Evidence, []string{w.secretDoc}) {
			t.Errorf("stance %s = %+v, want its position redacted and its evidence kept", st.ID, st)
		}
		if successor(h, tt.stance, true) == "" && tt.topic != w.shared {
			t.Errorf("no withdrawal supersedes %s in %+v", tt.stance, h)
		}
		if successor(h, tt.stance, false) == "" && tt.topic == w.shared {
			t.Errorf("the other document's stance no longer supersedes %s in %+v", tt.stance, h)
		}
	}
	if got := topicName(t, pool, w.only); got != l2.Redacted {
		t.Errorf("only's name = %q, want it redacted", got)
	}
	if got := topicName(t, pool, w.later); got != "the rotation schedule" {
		t.Errorf("later's name = %q, want it kept", got)
	}
	// Opened by the deleted issue, but still supported by the other one.
	if got := topicName(t, pool, w.shared); got != "how often keys rotate" {
		t.Errorf("shared's name = %q, want it kept: %s still supports it", got, w.otherDoc)
	}
}

// A tombstone from the source withdraws the stances exactly as an operator
// deletion does, and records the withdrawal, but it is not an operator's
// deletion: no text is redacted and there is no record to show.
func TestASourceTombstoneRedactsNothing(t *testing.T) {
	w := newGraphWorld(t)
	ctx := t.Context()
	artifact := project + "#1"
	if _, err := l0.New(w.pool).Append(ctx, connector.Event{
		Source: src, NativeID: artifact + ":tombstone", Kind: connector.KindTombstone, Time: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC),
		Payload: connector.Payload{Artifact: artifact + ":tombstone", Target: artifact,
			Container: connector.Container{Kind: connector.ContainerRepository, NativeID: project}},
		ACL: public,
	}); err != nil {
		t.Fatal(err)
	}
	// What the pump would enqueue for the tombstone.
	if _, err := queue.Enqueue(ctx, w.pool, queue.Request{Kind: distiller.JobKind(), TargetID: w.secretDoc}); err != nil {
		t.Fatal(err)
	}
	rebuild(t, w.pool)

	if _, err := l1.New(w.pool).Get(ctx, w.secretDoc); !errors.Is(err, l1.ErrNotFound) {
		t.Fatalf("the tombstoned document: %v, want it gone", err)
	}
	h := history(t, w.pool, w.only)
	if st := h[w.onlyStance]; st.Position != "rotate "+secret+" by Friday" {
		t.Errorf("the withdrawn stance's position = %q, want it kept", st.Position)
	}
	if successor(h, w.onlyStance, true) == "" {
		t.Errorf("no withdrawal supersedes %s: %+v", w.onlyStance, h)
	}
	if got := topicName(t, w.pool, w.only); got != "whether "+secret+" is rotated" {
		t.Errorf("the topic's name = %q, want it kept", got)
	}
	var redacted int
	if err := w.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM l2_stances WHERE redacted_by IS NOT NULL) +
  (SELECT count(*) FROM l2_topics WHERE redacted_by IS NOT NULL) + (SELECT count(*) FROM l0_deletion_rebuilds)`).Scan(&redacted); err != nil || redacted != 0 {
		t.Errorf("rows marked by a deletion = %d, %v, want none", redacted, err)
	}
	if list, err := deletion.List(ctx, w.pool); err != nil || len(list) != 0 {
		t.Errorf("List() = %+v, %v, want no deletions", list, err)
	}
}

// A document the deletion re-distilled keeps its stance, text and all, while
// it is current: a stance is not overwritten. The next reading of the rebuilt
// document supersedes it, and that is when its text goes.
func TestARereadOfARebuiltDocumentRedactsTheStanceItReplaces(t *testing.T) {
	w := newGraphWorld(t)
	ctx := t.Context()
	applied, err := deletion.Apply(ctx, w.pool, repo, deletion.Selector{Event: connector.EventID(src, project+"#1")}, "a pasted key", "pat")
	if err != nil {
		t.Fatal(err)
	}
	// What the distiller does for a document that survives the deletion: it
	// writes the new version and records the rebuild in one transaction. The
	// model call it would make to get the new text is not the point here.
	stored, err := l1.New(w.pool).Get(ctx, w.secretDoc)
	if err != nil {
		t.Fatal(err)
	}
	doc := stored.Document
	doc.Text, doc.RawText, doc.Body.Summary = "Rotate the key before Friday.", "Rotate the key before Friday.", "Rotate the key before Friday."
	if err := pgx.BeginFunc(ctx, w.pool, func(tx pgx.Tx) error {
		if _, err := l1.New(tx).Put(ctx, doc); err != nil {
			return err
		}
		if _, err := l0.RecordRebuilt(ctx, tx, []string{doc.ID}, nil); err != nil {
			return err
		}
		return l2.RedactDeleted(ctx, tx, []string{doc.ID})
	}); err != nil {
		t.Fatal(err)
	}
	if st := history(t, w.pool, w.only)[w.onlyStance]; st.Position == l2.Redacted {
		t.Fatal("the current stance was redacted before anything superseded it")
	}

	rebuilt, err := l1.New(w.pool).Get(ctx, w.secretDoc)
	if err != nil {
		t.Fatal(err)
	}
	at := rebuilt.DistilledAt
	position := "rotate the key by Friday"
	if _, _, err := l2.New(w.pool).AppendStance(ctx, l2.Stance{
		ID: l2.StanceID(w.only, w.secretDoc, position, at, l2.TierInferred), TopicID: w.only, Position: position,
		StatedAt: time.Date(2026, 9, 1, 1, 0, 0, 0, time.UTC), Evidence: []string{w.secretDoc}, Tier: l2.TierInferred, ACL: public,
	}, at); err != nil {
		t.Fatal(err)
	}
	h := history(t, w.pool, w.only)
	if st := h[w.onlyStance]; st.Position != l2.Redacted {
		t.Errorf("the replaced stance's position = %q, want it redacted", st.Position)
	}
	if successor(h, w.onlyStance, false) == "" {
		t.Errorf("the new reading does not supersede %s: %+v", w.onlyStance, h)
	}
	rec, err := deletion.Show(ctx, w.pool, applied.Deletion)
	if err != nil {
		t.Fatal(err)
	}
	// The shared topic's stance was already superseded when the document was
	// rebuilt, so it went then; the later topic's is still current.
	want := []string{w.onlyStance, w.sharedStance}
	slices.Sort(want)
	if !slices.Equal(rec.Stances, want) || len(rec.Topics) != 0 {
		t.Errorf("Show() = %+v, want stances %v and no topic redacted", rec, want)
	}
	if st := history(t, w.pool, w.later)[w.laterStance]; st.Position == l2.Redacted {
		t.Error("the later topic's current stance was redacted")
	}
	if len(rec.Documents) != 1 || rec.Documents[0].Outcome != l0.RebuiltRedistilled {
		t.Errorf("Documents = %+v, want the document re-distilled", rec.Documents)
	}
}
