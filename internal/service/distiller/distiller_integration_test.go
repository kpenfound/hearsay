//go:build integration

package distiller_test

import (
	"context"
	"os"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/queue"
	"github.com/kpenfound/hearsay/internal/service/distiller"
)

func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("HEARSAY_DATABASE_URL")
	if url == "" {
		t.Skip("HEARSAY_DATABASE_URL is not set")
	}
	pool, err := db.Connect(context.Background(), url)
	if err != nil {
		t.Fatalf("connecting to postgres: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// sources keeps tests out of each other's way: the database outlives one test,
// and L0 is append-only, so a test takes a source id nothing else uses rather
// than truncating a table another test is reading.
var sources atomic.Int64

func newSource(t *testing.T) string {
	t.Helper()
	return "t" + strconv.FormatInt(time.Now().UnixNano(), 36) + "x" + strconv.FormatInt(sources.Add(1), 36)
}

// ingest writes the fixture repository into L0, the way a connector would.
func ingest(t *testing.T, pool *pgxpool.Pool, events []connector.Event) {
	t.Helper()
	store := l0.New(pool)
	for _, ev := range events {
		if _, err := store.Append(t.Context(), ev); err != nil {
			t.Fatalf("Append(%s) = %v", ev.NativeID, err)
		}
	}
}

// newDistiller builds the distiller over the fake registry, which answers from
// the recorded fixtures and reaches no network.
func newDistiller(t *testing.T, pool *pgxpool.Pool, src string) *distiller.Distiller {
	t.Helper()
	registry, err := llm.NewFake(testRepo(src).LLM, loadFixtures(t))
	if err != nil {
		t.Fatalf("building the fake registry: %v", err)
	}
	d, err := distiller.New(pool, registry, testConfig(src))
	if err != nil {
		t.Fatalf("distiller.New() = %v", err)
	}
	return d
}

// The acceptance criterion: distilling a fixture repository twice produces
// identical L1 rows.
//
// It is the whole of what "the distiller is stateless and idempotent" means. A
// second run rebuilds every document from the same events and writes nothing,
// so a re-distillation costs a model call and no row churn — and anything that
// crept into a document from outside its events, an iteration order or a clock,
// would show up here as a row that moved.
func TestDistillingTwiceProducesIdenticalRows(t *testing.T) {
	pool := newPool(t)
	src := newSource(t)
	ingest(t, pool, fixtureEvents(src))
	d := newDistiller(t, pool, src)
	docs := l1.New(pool)

	ids := []string{
		l1.DocID(src, repo+"#12"),
		l1.DocID(src, repo+"#31"),
		l1.DocID(src, commit),
	}
	first := map[string]l1.Stored{}
	for _, id := range ids {
		result, err := d.Distill(t.Context(), id)
		if err != nil {
			t.Fatalf("Distill(%s) = %v", id, err)
		}
		if !result.Written {
			t.Errorf("Distill(%s) wrote nothing the first time", id)
		}
		stored, err := docs.Get(t.Context(), id)
		if err != nil {
			t.Fatalf("Get(%s) = %v", id, err)
		}
		first[id] = stored
	}

	for _, id := range ids {
		result, err := d.Distill(t.Context(), id)
		if err != nil {
			t.Fatalf("Distill(%s, again) = %v", id, err)
		}
		if result.Written {
			t.Errorf("Distill(%s, again) rewrote the row, and nothing had changed", id)
		}
		stored, err := docs.Get(t.Context(), id)
		if err != nil {
			t.Fatalf("Get(%s, again) = %v", id, err)
		}
		assertSameStoredDocument(t, stored, first[id])
	}
}

// The other two acceptance criteria, on the documents the fixture repository
// makes: every one has an outcome kind, and it is one of the design's five.
func TestEveryDocumentCarriesAnOutcomeKind(t *testing.T) {
	pool := newPool(t)
	src := newSource(t)
	ingest(t, pool, fixtureEvents(src))
	d := newDistiller(t, pool, src)

	for _, artifact := range []string{repo + "#12", repo + "#31", commit} {
		id := l1.DocID(src, artifact)
		if _, err := d.Distill(t.Context(), id); err != nil {
			t.Fatalf("Distill(%s) = %v", id, err)
		}
	}
	docs, err := l1.New(pool).List(t.Context(), l1.ListOptions{Source: src})
	if err != nil {
		t.Fatalf("List() = %v", err)
	}
	if len(docs) != 3 {
		t.Fatalf("List() returned %d documents, want the three the fixture repository makes", len(docs))
	}
	for _, doc := range docs {
		if !doc.Body.OutcomeKind.Valid() {
			t.Errorf("%s has outcome kind %q, which is not one of the five", doc.ID, doc.Body.OutcomeKind)
		}
		if doc.Text == "" || doc.RawText == "" {
			t.Errorf("%s has nothing to embed or nothing to search", doc.ID)
		}
		if len(doc.L0Refs) == 0 {
			t.Errorf("%s has no provenance", doc.ID)
		}
	}

	// The change proposal is one document with its review and its review
	// comment in it, which is what grouping a conversation means.
	pr, err := l1.New(pool).Get(t.Context(), l1.DocID(src, repo+"#31"))
	if err != nil {
		t.Fatalf("Get(the change proposal) = %v", err)
	}
	if len(pr.L0Refs) != 3 {
		t.Errorf("the change proposal names %d events, want the artifact, the review and the review comment", len(pr.L0Refs))
	}
	if pr.Kind != l1.KindPR {
		t.Errorf("Kind = %q, want %q", pr.Kind, l1.KindPR)
	}
	wantParticipants := []l1.Participant{
		{PrincipalID: "kyle", Role: connector.RoleAuthor},
		{PrincipalID: "sam", Role: connector.RoleReviewer},
	}
	if !slices.Equal(pr.Participants, wantParticipants) {
		t.Errorf("Participants = %v, want %v", pr.Participants, wantParticipants)
	}
	// The tracker item the artifact is, and the code entities the scope and the
	// conversation name.
	if !slices.Contains(pr.Scope, "tracker:"+src+":"+repo+"#31") {
		t.Errorf("Scope = %v, want the tracker item the artifact is", pr.Scope)
	}
	if !slices.Contains(pr.References, l1.Reference{Type: l1.RefIssue, ID: repo + "#12"}) {
		t.Errorf("References = %v, want the issue the change proposal links to", pr.References)
	}
}

// A new event on an artifact that already has a document re-distils it: the
// document grows the event, and the row is written again.
func TestANewEventRedistillsTheDocument(t *testing.T) {
	pool := newPool(t)
	src := newSource(t)
	ingest(t, pool, fixtureEvents(src))
	d := newDistiller(t, pool, src)
	docs := l1.New(pool)
	id := l1.DocID(src, repo+"#31")

	if _, err := d.Distill(t.Context(), id); err != nil {
		t.Fatalf("Distill() = %v", err)
	}
	before, err := docs.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("Get() = %v", err)
	}

	ingest(t, pool, []connector.Event{laterComment(src)})
	result, err := d.Distill(t.Context(), id)
	if err != nil {
		t.Fatalf("Distill(after a new comment) = %v", err)
	}
	if !result.Written {
		t.Fatal("Distill(after a new comment) wrote nothing")
	}
	after, err := docs.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("Get(after) = %v", err)
	}
	if len(after.L0Refs) != len(before.L0Refs)+1 {
		t.Errorf("L0Refs = %v, want the new comment added to %v", after.L0Refs, before.L0Refs)
	}
	if !after.Time.LastActivity.After(before.Time.LastActivity) {
		t.Errorf("LastActivity = %s, want it to have moved past %s", after.Time.LastActivity, before.Time.LastActivity)
	}
	if !after.DistilledAt.After(before.DistilledAt) {
		t.Errorf("DistilledAt = %s, want it to have moved past %s", after.DistilledAt, before.DistilledAt)
	}
	// The artifact's own time does not move: it happened when it happened.
	if !after.Time.Created.Equal(before.Time.Created) {
		t.Errorf("Created moved from %s to %s", before.Time.Created, after.Time.Created)
	}
}

// An artifact retracted at the source leaves L1. The events stay in L0 behind
// the tombstone, and the document derived from them goes.
func TestATombstonedArtifactLosesItsDocument(t *testing.T) {
	pool := newPool(t)
	src := newSource(t)
	ingest(t, pool, fixtureEvents(src))
	d := newDistiller(t, pool, src)
	docs := l1.New(pool)
	id := l1.DocID(src, repo+"#12")

	if _, err := d.Distill(t.Context(), id); err != nil {
		t.Fatalf("Distill() = %v", err)
	}
	if _, err := docs.Get(t.Context(), id); err != nil {
		t.Fatalf("Get() = %v", err)
	}

	tombstone := event(src, connector.KindTombstone, repo+"#12:tombstone", at(9), nil, "", "")
	tombstone.Payload.Target = repo + "#12"
	ingest(t, pool, []connector.Event{tombstone})

	result, err := d.Distill(t.Context(), id)
	if err != nil {
		t.Fatalf("Distill(retracted) = %v", err)
	}
	if !result.Deleted {
		t.Error("Distill(retracted).Deleted = false, want the document removed")
	}
	if _, err := docs.Get(t.Context(), id); err == nil {
		t.Error("the document for a retracted artifact is still there")
	}
	// And running it again is not an error: the job is at-least-once.
	if _, err := d.Distill(t.Context(), id); err != nil {
		t.Errorf("Distill(retracted, again) = %v", err)
	}
}

// The pump turns the change feed into jobs: one per document, whatever a burst
// of events on one document looks like.
func TestThePumpEnqueuesOneJobPerDocument(t *testing.T) {
	pool := newPool(t)
	src := newSource(t)

	// A cursor of its own, so that what this test reads is what it wrote and
	// not what another test left on the shared feed.
	pump := distiller.NewPump(pool, distiller.PumpOptions{Batch: l0.MaxLimit})
	if _, err := pump.Once(t.Context()); err != nil {
		t.Fatalf("Once(catching up) = %v", err)
	}
	client, err := queue.New(pool, queue.Config{Kind: distiller.JobKind()})
	if err != nil {
		t.Fatalf("queue.New() = %v", err)
	}
	before := pendingTargets(t, client)

	ingest(t, pool, fixtureEvents(src))
	// The feed serves rows only once the transaction that wrote them has
	// finished, so read until it has caught up rather than once.
	deadline := time.Now().Add(30 * time.Second)
	var targets []string
	for {
		if _, err := pump.Once(t.Context()); err != nil {
			t.Fatalf("Once() = %v", err)
		}
		targets = newTargets(pendingTargets(t, client), before)
		if len(targets) >= 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	want := []string{l1.DocID(src, commit), l1.DocID(src, repo+"#12"), l1.DocID(src, repo+"#31")}
	slices.Sort(want)
	slices.Sort(targets)
	if !slices.Equal(targets, want) {
		t.Fatalf("the pump enqueued %v, want one job per document: %v", targets, want)
	}

	// Reading the feed again enqueues nothing: the cursor moved with the jobs.
	if progress, err := pump.Once(t.Context()); err != nil {
		t.Fatalf("Once(again) = %v", err)
	} else if progress.Jobs != 0 {
		t.Errorf("Once(again) enqueued %d jobs, want none", progress.Jobs)
	}
}

// The two halves together: events in, documents out, through the queue.
func TestRunDistillsWhatArrivesAndStopsWhenCancelled(t *testing.T) {
	pool := newPool(t)
	src := newSource(t)
	ingest(t, pool, fixtureEvents(src))

	registry, err := llm.NewFake(testRepo(src).LLM, loadFixtures(t))
	if err != nil {
		t.Fatalf("building the fake registry: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- distiller.Run(ctx, testConfig(src), distiller.Deps{
			Pool: pool,
			LLM:  registry,
			Pump: distiller.PumpOptions{Interval: 100 * time.Millisecond, Batch: l0.MaxLimit},
		})
	}()

	docs := l1.New(pool)
	deadline := time.Now().Add(60 * time.Second)
	var stored []l1.Stored
	for time.Now().Before(deadline) {
		stored, err = docs.List(t.Context(), l1.ListOptions{Source: src})
		if err != nil {
			t.Fatalf("List() = %v", err)
		}
		if len(stored) == 3 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(stored) != 3 {
		t.Errorf("the distiller wrote %d documents, want the three the fixture repository makes", len(stored))
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run() after cancellation = %v, want nil", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run() did not return within 30s of cancellation")
	}
}

// A process with no configuration has no model tiers, so there is nothing to
// distil and nothing to distil it with. It starts and waits to be stopped,
// which is what every other service does without a configuration (ADR-0009) —
// and it is what makes `hearsay all` on a machine with nothing set up
// something a person can look at.
func TestRunWithNoConfigurationIdlesUntilItIsStopped(t *testing.T) {
	pool := newPool(t)
	cfg := config.Default() // no configuration repository

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- distiller.Run(ctx, &cfg, distiller.Deps{Pool: pool}) }()

	select {
	case err := <-done:
		t.Fatalf("Run() returned %v before it was stopped", err)
	case <-time.After(200 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run() after cancellation = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run() did not return within 10s of cancellation")
	}
}

// The request a document produces does not depend on which configured source
// instance it came from. It is why an integration test can take a source id
// nothing else uses and still hit a recording made under another one — and if
// it stops being true, every one of these tests fails on a fixture miss with no
// clue why. So it is said here rather than left to be discovered.
func TestTheRequestDoesNotDependOnTheSourceID(t *testing.T) {
	one := documentsIn(t, fixtureEvents("github-one"))
	two := documentsIn(t, fixtureEvents("github-two"))
	if len(one) != len(two) || len(one) == 0 {
		t.Fatalf("the fixture repository made %d and %d documents", len(one), len(two))
	}
	for i := range one {
		a := llm.FixtureKey(llm.TierDistill, distiller.RequestFor(one[i], 2048))
		b := llm.FixtureKey(llm.TierDistill, distiller.RequestFor(two[i], 2048))
		if a != b {
			t.Errorf("the request for %s depends on the source id", one[i].Source.NativeID)
		}
	}
}

func pendingTargets(t *testing.T, client *queue.Client) []string {
	t.Helper()
	jobs, err := client.List(t.Context(), queue.StatePending, queue.MaxLimit)
	if err != nil {
		t.Fatalf("List(pending) = %v", err)
	}
	targets := make([]string, 0, len(jobs))
	for _, job := range jobs {
		targets = append(targets, job.TargetID)
	}
	return targets
}

func newTargets(now, before []string) []string {
	var out []string
	for _, target := range now {
		if !slices.Contains(before, target) {
			out = append(out, target)
		}
	}
	return out
}

// assertSameStoredDocument compares two readings of one row, field by field, so
// that a column nothing else looks at is still checked.
func assertSameStoredDocument(t *testing.T, got, want l1.Stored) {
	t.Helper()
	if !got.DistilledAt.Equal(want.DistilledAt) {
		t.Errorf("%s: distilled_at moved from %s to %s", want.ID, want.DistilledAt, got.DistilledAt)
	}
	if got.ID != want.ID || got.Kind != want.Kind || got.Source != want.Source {
		t.Errorf("%s: envelope = %+v, want %+v", want.ID, got.Source, want.Source)
	}
	if !slices.Equal(got.L0Refs, want.L0Refs) {
		t.Errorf("%s: l0_refs = %v, want %v", want.ID, got.L0Refs, want.L0Refs)
	}
	if !got.Time.Created.Equal(want.Time.Created) || !got.Time.Updated.Equal(want.Time.Updated) ||
		!got.Time.LastActivity.Equal(want.Time.LastActivity) {
		t.Errorf("%s: time = %+v, want %+v", want.ID, got.Time, want.Time)
	}
	if !slices.Equal(got.Participants, want.Participants) {
		t.Errorf("%s: participants = %v, want %v", want.ID, got.Participants, want.Participants)
	}
	if !slices.Equal(got.Scope, want.Scope) {
		t.Errorf("%s: scope = %v, want %v", want.ID, got.Scope, want.Scope)
	}
	if !slices.Equal(got.References, want.References) {
		t.Errorf("%s: references = %v, want %v", want.ID, got.References, want.References)
	}
	if !slices.Equal(got.ACL, want.ACL) {
		t.Errorf("%s: acl = %v, want %v", want.ID, got.ACL, want.ACL)
	}
	if got.Text != want.Text {
		t.Errorf("%s: text = %q, want %q", want.ID, got.Text, want.Text)
	}
	if got.RawText != want.RawText {
		t.Errorf("%s: raw_text = %q, want %q", want.ID, got.RawText, want.RawText)
	}
	if got.Body.Summary != want.Body.Summary || got.Body.Question != want.Body.Question ||
		got.Body.Outcome != want.Body.Outcome || got.Body.OutcomeKind != want.Body.OutcomeKind ||
		got.Body.Change != want.Body.Change || !slices.Equal(got.Body.OpenQuestions, want.Body.OpenQuestions) {
		t.Errorf("%s: body = %+v, want %+v", want.ID, got.Body, want.Body)
	}
}
