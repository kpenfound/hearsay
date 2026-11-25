//go:build integration

package distiller_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/db/dbtest"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l1/l1test"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/poll"
	"github.com/kpenfound/hearsay/internal/queue"
	"github.com/kpenfound/hearsay/internal/service/assertworker"
	"github.com/kpenfound/hearsay/internal/service/distiller"
	"github.com/kpenfound/hearsay/internal/telemetry"
)

func testConfig(src string) *config.Config {
	cfg := config.Default()
	cfg.Repo = testRepo(src)
	return &cfg
}

func startDistiller(t *testing.T, pool *pgxpool.Pool, src string) (stop func() error) {
	t.Helper()
	registry, err := llm.NewFake(testRepo(src).LLM, loadFixtures(t))
	if err != nil {
		t.Fatalf("building the fake registry: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- distiller.Run(ctx, testConfig(src), distiller.Deps{
			Pool:      pool,
			LLM:       registry,
			Judgments: fixtureJev(t).judgments(),
			Pump:      distiller.PumpOptions{Quiet: 0, Interval: 100 * time.Millisecond, Batch: l0.MaxLimit},
		})
	}()
	return func() error {
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(30 * time.Second):
			return errors.New("Run() did not return within 30s of cancellation")
		}
	}
}

func ingest(t *testing.T, pool *pgxpool.Pool, events []connector.Event) {
	t.Helper()
	store := l0.New(pool)
	for _, ev := range events {
		if _, err := store.Append(t.Context(), ev); err != nil {
			t.Fatalf("Append(%s) = %v", ev.NativeID, err)
		}
	}
}

func newDistiller(t *testing.T, pool *pgxpool.Pool, src string) *distiller.Distiller {
	t.Helper()
	registry, err := llm.NewFake(testRepo(src).LLM, loadFixtures(t))
	if err != nil {
		t.Fatalf("building the fake registry: %v", err)
	}
	d, err := distiller.New(pool, registry, fixtureJev(t).judgments(), testConfig(src))
	if err != nil {
		t.Fatalf("distiller.New() = %v", err)
	}
	return d
}

func TestDistillingTwiceProducesIdenticalRows(t *testing.T) {
	pool := dbtest.Pool(t)
	src := dbtest.UniqueID()
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

func TestHandlingAJobLogsIdsAndNoContentAtInfoLevel(t *testing.T) {
	pool := dbtest.Pool(t)
	src := dbtest.UniqueID()
	ingest(t, pool, fixtureEvents(src))
	d := newDistiller(t, pool, src)
	id := l1.DocID(src, repo+"#12")

	var logs bytes.Buffer
	ctx := telemetry.WithLogger(t.Context(), slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	if err := d.Handle(ctx, queue.Job{Kind: distiller.JobKind(), TargetID: id}); err != nil {
		t.Fatalf("Handle() = %v", err)
	}

	out := logs.String()
	if !strings.Contains(out, "document distilled") || !strings.Contains(out, id) {
		t.Fatalf("the distillation was not logged by id:\n%s", out)
	}
	stored, err := l1.New(pool).Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"the raw text": stored.RawText, "the distilled text": stored.Text, "the summary": stored.Body.Summary} {
		if content != "" && strings.Contains(out, content) {
			t.Errorf("info-level output carries %s:\n%s", name, out)
		}
	}
	if strings.Contains(out, "The engine writes before it takes the lock") {
		t.Errorf("info-level output carries the L0 payload:\n%s", out)
	}
}

func TestPRMergeRefreshesArtifactClass(t *testing.T) {
	pool := dbtest.Pool(t)
	src := dbtest.UniqueID()
	events := fixtureEvents(src)
	ingest(t, pool, events)
	d := newDistiller(t, pool, src)
	id := l1.DocID(src, repo+"#31")
	first, err := d.Distill(t.Context(), id)
	if err != nil || !first.Written {
		t.Fatalf("initial Distill = %+v, %v", first, err)
	}
	store := l1.New(pool)
	before, err := store.Get(t.Context(), id)
	if err != nil || before.ArtifactClass != config.ArtifactPullRequest {
		t.Fatalf("initial class = %q, %v", before.ArtifactClass, err)
	}
	var changed connector.Event
	for _, ev := range events {
		if ev.Kind == connector.KindPullRequest {
			changed = ev
			break
		}
	}
	changed = revised(changed, "2026-09-09T19:00:00Z", at(7))
	changed.Payload.Native = []byte(`{"state":"closed","merged_at":"2026-09-09T19:00:00Z"}`)
	ingest(t, pool, []connector.Event{changed})
	second, err := d.Distill(t.Context(), id)
	if err != nil || !second.Written {
		t.Fatalf("merged Distill = %+v, %v", second, err)
	}
	merged, err := store.Get(t.Context(), id)
	if err != nil || merged.ArtifactClass != config.ArtifactMergedPR {
		t.Fatalf("merged class = %q, %v", merged.ArtifactClass, err)
	}
	third, err := d.Distill(t.Context(), id)
	if err != nil || third.Written {
		t.Fatalf("repeat Distill = %+v, %v", third, err)
	}
}

func TestDistillingEmbedsTheDocumentItWrote(t *testing.T) {
	pool := dbtest.Pool(t)
	src := dbtest.UniqueID()
	ingest(t, pool, fixtureEvents(src))
	docs := l1.New(pool)
	id := l1.DocID(src, repo+"#31")

	result, err := newDistiller(t, pool, src).Distill(t.Context(), id)
	if err != nil {
		t.Fatalf("Distill(%s) = %v", id, err)
	}
	if !result.Written || result.Embedded {
		t.Fatalf("Distill(%s) = %+v, want the document written and not embedded", id, result)
	}
	if l1test.Embedded(t, pool, id) {
		t.Error("a document was embedded with no embed tier configured")
	}

	stored, err := docs.Get(t.Context(), id)
	if err != nil {
		t.Fatalf("Get(%s) = %v", id, err)
	}
	d := embeddingDistiller(t, pool, src, l1.EmbedText(stored.Text))

	result, err = d.Distill(t.Context(), id)
	if err != nil {
		t.Fatalf("Distill(%s, with an embed tier) = %v", id, err)
	}
	if result.Written || !result.Embedded {
		t.Fatalf("Distill(%s, with an embed tier) = %+v, want the row untouched and a vector written", id, result)
	}
	if !l1test.Embedded(t, pool, id) {
		t.Fatal("the distiller reported an embedding and the row has none")
	}

	result, err = d.Distill(t.Context(), id)
	if err != nil {
		t.Fatalf("Distill(%s, again) = %v", id, err)
	}
	if result.Embedded {
		t.Error("a document that had not changed was embedded a second time")
	}
}

func TestADistillerRefusesAnEmbedTierOfTheWrongWidth(t *testing.T) {
	pool := dbtest.Pool(t)
	src := dbtest.UniqueID()
	cfg := testConfig(src)
	cfg.Repo.LLM = withEmbedTier(cfg.Repo.LLM, l1.EmbeddingDimensions/2)
	registry, err := llm.NewFake(cfg.Repo.LLM, loadFixtures(t))
	if err != nil {
		t.Fatalf("building the fake registry: %v", err)
	}
	if _, err := distiller.New(pool, registry, newOutcomeJev(t).judgments(), cfg); !errors.Is(err, llm.ErrDimensions) {
		t.Fatalf("distiller.New() = %v, want llm.ErrDimensions", err)
	}
}

func embeddingDistiller(t *testing.T, pool *pgxpool.Pool, src, text string) *distiller.Distiller {
	t.Helper()
	fixtures := loadFixtures(t)
	vector := make([]float32, l1.EmbeddingDimensions)
	vector[0] = 1
	if err := fixtures.AddEmbedding(llm.EmbeddingFixture{Texts: []string{text}, Vectors: [][]float32{vector}}); err != nil {
		t.Fatalf("recording the embedding: %v", err)
	}
	cfg := testConfig(src)
	cfg.Repo.LLM = withEmbedTier(cfg.Repo.LLM, l1.EmbeddingDimensions)
	registry, err := llm.NewFake(cfg.Repo.LLM, fixtures)
	if err != nil {
		t.Fatalf("building the fake registry: %v", err)
	}
	d, err := distiller.New(pool, registry, fixtureJev(t).judgments(), cfg)
	if err != nil {
		t.Fatalf("distiller.New() = %v", err)
	}
	return d
}

func withEmbedTier(cfg llm.Config, dimensions int) llm.Config {
	cfg = cfg.Clone()
	cfg.Tiers[llm.TierEmbed] = llm.TierConfig{Provider: "acme", Model: "embed-1", Dimensions: dimensions}
	return cfg
}

func TestEveryDocumentCarriesAnOutcomeKind(t *testing.T) {
	pool := dbtest.Pool(t)
	src := dbtest.UniqueID()
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

	if !slices.Contains(pr.Scope, "tracker:"+src+":"+repo+"#31") {
		t.Errorf("Scope = %v, want the tracker item the artifact is", pr.Scope)
	}
	if !slices.Contains(pr.References, l1.Reference{Type: l1.RefIssue, ID: repo + "#12"}) {
		t.Errorf("References = %v, want the issue the change proposal links to", pr.References)
	}
}

func TestANewEventRedistillsTheDocument(t *testing.T) {
	pool := dbtest.Pool(t)
	src := dbtest.UniqueID()
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

	if !after.Time.Created.Equal(before.Time.Created) {
		t.Errorf("Created moved from %s to %s", before.Time.Created, after.Time.Created)
	}
}

func TestRedistillingToNoneWithdrawsTheOldStance(t *testing.T) {
	pool := dbtest.Scratch(t, "distiller_none")
	src := dbtest.UniqueID()
	ingest(t, pool, fixtureEvents(src))
	d := newDistiller(t, pool, src)
	docID := l1.DocID(src, repo+"#12")
	first, err := d.Distill(t.Context(), docID)
	if err != nil || !first.Written || !first.Asserting {
		t.Fatalf("first Distill = %+v, %v", first, err)
	}
	stored, err := l1.New(pool).Get(t.Context(), docID)
	if err != nil {
		t.Fatal(err)
	}
	graph := l2.New(pool)
	topicID := l2.TopicID("api", docID, 0, "lock order")
	if _, err := graph.OpenTopic(t.Context(), l2.Topic{ID: topicID, Scope: "api", Name: "lock order", OpenedBy: docID, ACL: public}); err != nil {
		t.Fatal(err)
	}
	old, _, err := graph.AppendStance(t.Context(), l2.Stance{
		ID:      l2.StanceID(topicID, docID, "take the lock first", stored.DistilledAt, l2.TierInferred),
		TopicID: topicID, Position: "take the lock first", Evidence: []string{docID},
		StatedAt: stored.Time.LastActivity, Tier: l2.TierInferred, ACL: public,
	}, stored.DistilledAt)
	if err != nil {
		t.Fatal(err)
	}
	client, err := queue.New(pool, queue.Config{Kind: l2.AssertKind()})
	if err != nil {
		t.Fatal(err)
	}

	jobs, err := client.Claim(t.Context())
	if err != nil || len(jobs) != 1 || jobs[0].TargetID != docID {
		t.Fatalf("first assertion jobs = %+v, %v", jobs, err)
	}
	if _, err := client.Complete(t.Context(), jobs[0]); err != nil {
		t.Fatal(err)
	}

	ingest(t, pool, []connector.Event{issueWithoutDecision(src)})
	second, err := d.Distill(t.Context(), docID)
	if err != nil || !second.Written || second.Asserting {
		t.Fatalf("second Distill = %+v, %v", second, err)
	}
	updated, err := l1.New(pool).Get(t.Context(), docID)
	if err != nil || updated.Body.OutcomeKind != l1.OutcomeNone {
		t.Fatalf("updated outcome = %q, %v", updated.Body.OutcomeKind, err)
	}
	jobs, err = client.Claim(t.Context())
	if err != nil || len(jobs) != 1 || jobs[0].TargetID != l2.WithdrawalTarget+old.ID {
		t.Fatalf("repair jobs = %+v, %v", jobs, err)
	}
	registry, err := llm.NewFake(testRepo(src).LLM, loadFixtures(t))
	if err != nil {
		t.Fatal(err)
	}
	worker, err := assertworker.New(pool, registry, testConfig(src))
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Handle(t.Context(), jobs[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Complete(t.Context(), jobs[0]); err != nil {
		t.Fatal(err)
	}
	history, err := graph.StanceHistory(t.Context(), topicID)
	if err != nil || len(history) != 2 || history[1].Supersedes != old.ID || !history[1].Withdrawn {
		t.Fatalf("stance history = %+v, %v", history, err)
	}
	if current, ok := l2.Current(history); ok {
		t.Errorf("current stance = %+v, want none", current)
	}
	third, err := d.Distill(t.Context(), docID)
	if err != nil || third.Written {
		t.Fatalf("repeat Distill = %+v, %v", third, err)
	}
	if err := worker.Handle(t.Context(), jobs[0]); err != nil {
		t.Fatal(err)
	}
	if again, err := graph.StanceHistory(t.Context(), topicID); err != nil || len(again) != 2 {
		t.Fatalf("history after retry = %+v, %v", again, err)
	}
}

func TestATombstonedArtifactLosesItsDocument(t *testing.T) {
	pool := dbtest.Pool(t)
	src := dbtest.UniqueID()
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

	if _, err := d.Distill(t.Context(), id); err != nil {
		t.Errorf("Distill(retracted, again) = %v", err)
	}

	reentry := fixtureEvents(src)[0]
	reentry.NativeID = reentry.Payload.Artifact + "@reentry"
	reentry.Payload.Revision = &connector.Revision{Token: "reentry", EditedAt: at(10)}
	ingest(t, pool, []connector.Event{reentry})
	result, err = d.Distill(t.Context(), id)
	if err != nil || !result.Written {
		t.Fatalf("Distill(restored) = %+v, %v", result, err)
	}
	if _, err := docs.Get(t.Context(), id); err != nil {
		t.Errorf("restored document is missing: %v", err)
	}
}

func TestATombstoneForACommentRedistillsTheDocumentThatQuotedIt(t *testing.T) {
	pool := dbtest.Pool(t)
	src := dbtest.UniqueID()
	ingest(t, pool, fixtureEvents(src))
	commentEvent := connector.EventID(src, retractedComment)

	stop := startDistiller(t, pool, src)
	defer func() {
		if err := stop(); err != nil {
			t.Error(err)
		}
	}()

	docs := l1.New(pool)
	id := l1.DocID(src, repo+"#12")
	waitForIssue := func(what string, ok func(l1.Stored) bool) l1.Stored {
		t.Helper()
		var found l1.Stored
		poll.Within(t, "the issue's document to be "+what, 60*time.Second, func() bool {
			doc, err := docs.Get(t.Context(), id)
			found = doc
			return err == nil && ok(doc)
		})
		return found
	}

	before := waitForIssue("was distilled", func(doc l1.Stored) bool { return slices.Contains(doc.L0Refs, commentEvent) })
	if !strings.Contains(before.RawText, "Seen it twice this week") {
		t.Fatalf("the issue's raw text does not quote the comment before it is deleted:\n%s", before.RawText)
	}

	assertFixtures := llm.NewFixtures()
	budget, ok := llm.Default().Tier(llm.TierAssert)
	if !ok {
		t.Fatal("no assertion model tier")
	}
	const topicName = "Where to take the engine lock"
	const beforePosition = "Move the lock into the queue."
	addAnswer := func(doc l1.Document, answer string) {
		t.Helper()
		if err := assertFixtures.Add(llm.CompletionFixture{Tier: llm.TierAssert,
			Request:  assertworker.RequestFor(doc, budget.MaxTokens),
			Response: llm.Response{JSON: []byte(answer), StopReason: llm.StopEnd, Model: "recorded"}}); err != nil {
			t.Fatal(err)
		}
	}
	addAnswer(before.Document, `{"assertions":[{"topic_name":"`+topicName+`","position":"`+beforePosition+`"}]}`)
	assertRegistry, err := llm.NewFake(testRepo(src).LLM, assertFixtures)
	if err != nil {
		t.Fatal(err)
	}
	asserter, err := assertworker.New(pool, assertRegistry, testConfig(src))
	if err != nil {
		t.Fatal(err)
	}
	asserter = asserter.WithJudgments(jevOver(t, assertJev{}, llm.JudgmentConfig{}))
	if r, err := asserter.Assert(t.Context(), id, "api"); err != nil || r.StancesWritten != 1 {
		t.Fatalf("Assert(before tombstone) = %+v, %v", r, err)
	}

	tombstone := event(src, connector.KindTombstone, retractedComment+":tombstone", at(9), nil, "", "")
	tombstone.Payload.Target = retractedComment
	ingest(t, pool, []connector.Event{tombstone})

	after := waitForIssue("lost the deleted comment", func(doc l1.Stored) bool { return !slices.Contains(doc.L0Refs, commentEvent) })
	if strings.Contains(after.RawText, "Seen it twice this week") {
		t.Errorf("the issue's raw text still quotes the deleted comment:\n%s", after.RawText)
	}
	if after.Text == before.Text || strings.Contains(after.Text, "Two people confirmed") {
		t.Errorf("the issue's text is still the distillation of the deleted comment:\n%s", after.Text)
	}
	if len(after.L0Refs) != len(before.L0Refs)-1 {
		t.Errorf("L0Refs = %v, want %v less the deleted comment", after.L0Refs, before.L0Refs)
	}
	const afterPosition = "Keep the lock in the engine."
	addAnswer(after.Document, `{"assertions":[{"topic_name":"`+topicName+`","position":"`+afterPosition+`"}]}`)
	if r, err := asserter.Assert(t.Context(), id, "api"); err != nil || r.StancesWritten != 1 {
		t.Fatalf("Assert(after tombstone) = %+v, %v", r, err)
	}
	graph := l2.New(pool)
	topics, err := graph.Topics(t.Context(), "api")
	if err != nil || len(topics) != 1 {
		t.Fatalf("Topics() = %+v, %v, want the original topic", topics, err)
	}
	history, err := graph.StanceHistory(t.Context(), topics[0].ID)
	if err != nil || len(history) != 2 || history[1].Supersedes != history[0].ID || history[1].Position != afterPosition {
		t.Errorf("tombstone re-read history = %+v, %v, want replacement of the document's original stance", history, err)
	}
}

type assertJev struct{}

func (assertJev) Judge(_ context.Context, req llm.JudgmentRequest) (llm.JudgmentResponse, error) {
	zero, cost := int64(0), 0.0
	out := llm.JudgmentResponse{ModelVersion: "recorded-v1", Usage: llm.JudgmentUsage{InputTokens: &zero, OutputTokens: &zero, CostUSD: &cost}}
	for _, q := range req.Questions {
		yes := q.ID != "same_answer"
		p := map[string]float64{"yes": 0.96, "no": 0.04}
		if !yes {
			p = map[string]float64{"yes": 0.04, "no": 0.96}
		}
		out.Answers = append(out.Answers, llm.JudgmentAnswer{ID: q.ID, Yes: &yes, Confidence: 0.96, Probabilities: p})
	}
	return out, nil
}

func TestThePumpEnqueuesOneJobPerDocument(t *testing.T) {
	pool := dbtest.Pool(t)
	src := dbtest.UniqueID()

	pump := distiller.NewPump(pool, distiller.PumpOptions{Quiet: 0, Batch: l0.MaxLimit})
	if _, err := pump.Once(t.Context()); err != nil {
		t.Fatalf("Once(catching up) = %v", err)
	}
	client, err := queue.New(pool, queue.Config{Kind: distiller.JobKind()})
	if err != nil {
		t.Fatalf("queue.New() = %v", err)
	}
	before := pendingTargets(t, client)

	ingest(t, pool, fixtureEvents(src))

	var targets []string
	var lastRead distiller.Progress
	poll.Within(t, "the pump to enqueue a job per document", 30*time.Second, func() bool {
		progress, err := pump.Once(t.Context())
		if err != nil {
			t.Fatalf("Once() = %v", err)
		}
		if progress.Events > 0 {
			lastRead = progress
		}
		targets = fromSource(newTargets(pendingTargets(t, client), before), src)
		return len(targets) >= 3
	})

	want := []string{l1.DocID(src, commit), l1.DocID(src, repo+"#12"), l1.DocID(src, repo+"#31")}
	slices.Sort(want)
	slices.Sort(targets)
	if !slices.Equal(targets, want) {
		t.Fatalf("the pump enqueued %v, want one job per document: %v", targets, want)
	}

	saved, err := l0.NewCursors(pool).Load(t.Context(), distiller.Consumer)
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if saved != lastRead.Cursor {
		t.Errorf("the saved cursor is %s and the pump read up to %s", saved, lastRead.Cursor)
	}
}

func TestRunDistillsWhatArrivesAndStopsWhenCancelled(t *testing.T) {
	pool := dbtest.Pool(t)
	src := dbtest.UniqueID()
	ingest(t, pool, fixtureEvents(src))

	stop := startDistiller(t, pool, src)

	docs := l1.New(pool)
	poll.Within(t, "the distiller to write the three documents the fixture repository makes", 60*time.Second, func() bool {
		stored, err := docs.List(t.Context(), l1.ListOptions{Source: src})
		if err != nil {
			t.Fatalf("List() = %v", err)
		}
		return len(stored) == 3
	})

	if err := stop(); err != nil {
		t.Errorf("Run() after cancellation = %v, want nil", err)
	}
}

func TestRunWithNoConfigurationIdlesUntilItIsStopped(t *testing.T) {
	pool := dbtest.Pool(t)
	cfg := config.Default()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- distiller.Run(ctx, &cfg, distiller.Deps{Pool: pool, Listener: listener}) }()

	resp, err := http.Get("http://" + listener.Addr().String() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	select {
	case err := <-done:
		t.Fatalf("Run() returned %v before it was stopped", err)
	default:
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

func fromSource(targets []string, src string) []string {
	var out []string
	for _, target := range targets {
		if strings.HasPrefix(target, l1.DocID(src, "")) {
			out = append(out, target)
		}
	}
	return out
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
	if !got.ACL.Equal(want.ACL) {
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

type interleaving struct {
	llm.Registry
	during func()
}

func (r interleaving) Completer(t llm.Tier) (llm.Completer, error) {
	c, err := r.Registry.Completer(t)
	if err != nil {
		return nil, err
	}
	return interleavingCompleter{Completer: c, during: r.during}, nil
}

type interleavingCompleter struct {
	llm.Completer
	during func()
}

func (c interleavingCompleter) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	c.during()
	return c.Completer.Complete(ctx, req)
}

func TestADistillationBuiltFromAStaleReadIsNotWritten(t *testing.T) {
	pool := dbtest.Pool(t)
	src := dbtest.UniqueID()
	ingest(t, pool, fixtureEvents(src))
	docs := l1.New(pool)
	docID := "l1:" + src + ":" + repo + "#31"

	if _, err := newDistiller(t, pool, src).Distill(t.Context(), docID); err != nil {
		t.Fatalf("the first Distill() = %v", err)
	}
	before, err := docs.Get(t.Context(), docID)
	if err != nil {
		t.Fatalf("Get() = %v", err)
	}

	late := reply(event(src, connector.KindMessage, repo+"#31:comment:99", at(9), who(src, "u2", "samr"),
		"", "One more thing before this merges."), repo+"#31")
	registry, err := llm.NewFake(testRepo(src).LLM, loadFixtures(t))
	if err != nil {
		t.Fatalf("building the fake registry: %v", err)
	}
	var once sync.Once
	d, err := distiller.New(pool, interleaving{Registry: registry, during: func() {
		once.Do(func() {
			ingest(t, pool, []connector.Event{late})
			writeNewerDocument(t, pool, src, docID)
		})
	}}, fixtureJev(t).judgments(), testConfig(src))
	if err != nil {
		t.Fatalf("distiller.New() = %v", err)
	}

	result, err := d.Distill(t.Context(), docID)
	if err != nil {
		t.Fatalf("Distill() over a moving conversation = %v", err)
	}
	if !result.Superseded {
		t.Errorf("Result = %+v, want the distillation to be reported superseded", result)
	}
	if result.Written {
		t.Error("Result.Written is true: a distillation of a conversation that has moved was stored")
	}

	after, err := docs.Get(t.Context(), docID)
	if err != nil {
		t.Fatalf("Get() = %v", err)
	}
	lateID := connector.EventID(late.Source, late.NativeID)
	if !slices.Contains(after.Document.L0Refs, lateID) {
		t.Errorf("l0_refs = %v, want it to still name the reply that landed (%s)",
			after.Document.L0Refs, lateID)
	}
	if slices.Equal(after.Document.L0Refs, before.Document.L0Refs) {
		t.Error("the row went back to the provenance it had before the reply landed")
	}
}

func writeNewerDocument(t *testing.T, pool *pgxpool.Pool, src, docID string) {
	t.Helper()
	source, artifact, err := l1.ParseDocID(docID)
	if err != nil {
		t.Fatalf("ParseDocID(%s) = %v", docID, err)
	}
	events := l0.New(pool)
	roots, err := events.Current(t.Context(), l0.ListOptions{
		Filter: l0.Filter{Source: source, Artifact: artifact}, Limit: 1,
	})
	if err != nil || len(roots) == 0 {
		t.Fatalf("reading the artifact = %v (%d rows)", err, len(roots))
	}
	children, err := events.Current(t.Context(), l0.ListOptions{
		Filter: l0.Filter{Source: source, Thread: artifact}, Limit: l0.MaxLimit,
	})
	if err != nil {
		t.Fatalf("reading the conversation = %v", err)
	}
	doc, err := l1.Build(l1.Input{Root: roots[0], Children: children, Repo: testRepo(src)})
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}
	doc, _, err = doc.WithBody(l1.Body{
		Summary:     "A newer distillation, written by the job the late reply enqueued.",
		Outcome:     "Approved.",
		OutcomeKind: l1.OutcomeResolved,
	})
	if err != nil {
		t.Fatalf("WithBody() = %v", err)
	}
	if _, err := l1.New(pool).Put(t.Context(), doc); err != nil {
		t.Fatalf("Put() = %v", err)
	}
}
