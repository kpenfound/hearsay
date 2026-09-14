package assertworker_test

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/principal"
	"github.com/kpenfound/hearsay/internal/service/assertworker"
)

// The fixture: an issue that proposes one thing, and a later pull request that
// fixes it another way and is merged. The model's answers are recorded in
// testdata/fixtures, keyed by the request, and replayed through the fake in
// internal/llm (CLAUDE.md). Change the prompt and the keys change:
//
//	go test ./internal/service/assertworker -record

var record = flag.Bool("record", false, "rewrite the recorded model answers in testdata/fixtures")

const (
	repo      = "acme/api"
	topicName = "Where the engine takes its lock relative to the write"
	// issuePosition is what the issue proposes, and prPosition what the merged
	// pull request did instead.
	issuePosition = "Move the lock into the job queue."
	prPosition    = "The engine takes its own lock, before it writes."
)

var day = time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)

func at(hours int) time.Time { return day.Add(time.Duration(hours) * time.Hour) }

func event(src string, kind connector.Kind, artifact string, when time.Time, handle, nativeID, title, text string) connector.Event {
	return connector.Event{
		Source:   src,
		NativeID: artifact,
		Kind:     kind,
		Time:     when,
		Payload: connector.Payload{
			Artifact:  artifact,
			Container: connector.Container{Kind: connector.ContainerRepository, NativeID: repo, Name: repo},
			URL:       "https://github.com/" + repo,
			Title:     title,
			Text:      text,
			Author:    &connector.Identity{Source: src, Kind: connector.IdentityUser, NativeID: nativeID, Handle: handle},
		},
		ACL: connector.ACL{{Kind: connector.ACLPublic}},
	}
}

// fixture is the two artifacts as a connector would emit them: each root with
// the events that hang off it.
type fixture struct {
	issue, pr       connector.Event
	prChildren      []connector.Event
	issueID, prID   string
	issueTrackerID  string
	prTrackerItemID string
}

func newFixture(src string) fixture {
	issue := event(src, connector.KindIssue, repo+"#12", at(0), "kpenfound", "u1",
		"engine: writes race with the job queue",
		"The engine writes before it takes the lock. I propose we move the lock into the job queue instead.")
	pr := event(src, connector.KindPullRequest, repo+"#31", at(3), "kpenfound", "u1",
		"engine: take the lock before writing",
		"Fixes #12. Rather than moving the lock into the job queue, the engine now takes it before the write. See https://github.com/acme/api/issues/12.")
	review := event(src, connector.KindReview, repo+"#31:review:77", at(4), "samr", "u2", "", "Approved, and merged.")
	review.Payload.Parent, review.Payload.Thread = repo+"#31", repo+"#31"
	return fixture{
		issue: issue, pr: pr, prChildren: []connector.Event{review},
		issueID: l1.DocID(src, repo+"#12"), prID: l1.DocID(src, repo+"#31"),
		issueTrackerID:  "tracker:" + src + ":" + repo + "#12",
		prTrackerItemID: "tracker:" + src + ":" + repo + "#31",
	}
}

// bodies are what the distiller concluded about each artifact.
var (
	issueBody = l1.Body{
		Summary:     "The engine writes before taking its lock, which races with the job queue. The author proposes moving the lock into the job queue.",
		Question:    "Where should the lock be taken?",
		Outcome:     "Proposed: take the lock inside the job queue.",
		OutcomeKind: l1.OutcomeProposed,
	}
	prBody = l1.Body{
		Summary:     "Fixes the race the issue described by having the engine take its lock before it writes, rather than moving the lock into the job queue. Approved and merged.",
		Change:      "The engine takes its lock before the write.",
		Outcome:     "Merged.",
		OutcomeKind: l1.OutcomeResolved,
	}
)

// testRepo is the configuration the fixture is asserted under. The scope is
// named after the source, so every test has a scope — and so a topic list — of
// its own in a database the tests share; nothing a request holds depends on it.
func testRepo(src string) config.Repo {
	return config.Repo{
		Path:   "testdata",
		Digest: "sha256:test",
		Scopes: []config.Scope{{
			ID:      src,
			Name:    "API",
			Sources: []config.ScopeSource{{Source: src, Containers: []string{repo}}},
			Tracker: config.SourceRef{Source: src, Project: repo},
		}},
		Principals: []principal.Principal{
			{ID: "kyle", Kind: principal.KindHuman, Identities: []principal.Identity{{Source: src, NativeID: "u1", Handle: "kpenfound"}}},
			{ID: "sam", Kind: principal.KindHuman, Identities: []principal.Identity{{Source: src, NativeID: "u2", Handle: "samr"}}},
		},
		LLM: llm.Default(),
	}
}

// documents are the two L1 documents the fixture distils to.
func documents(t *testing.T, src string) (issue, pr l1.Document) {
	t.Helper()
	f := newFixture(src)
	return buildDoc(t, src, f.issue, nil, issueBody), buildDoc(t, src, f.pr, f.prChildren, prBody)
}

// commitDocument is a third document, whose recorded answer is one a model
// should not give: an email address in a position, a topic with a blank name
// and a position that is blank.
func commitDocument(t *testing.T, src string) l1.Document {
	t.Helper()
	commit := event(src, connector.KindCommit, repo+"@1f0e3dad99908345f7439f8ffabdffc4", at(7), "kpenfound", "u1",
		"engine: note the lock order", "engine: note the lock order\n\nWrites down why the lock comes first.")
	return buildDoc(t, src, commit, nil, commitBody)
}

var commitBody = l1.Body{
	Summary:     "Writes down that the engine takes its lock before it writes.",
	Change:      "Adds a comment on the lock order.",
	Outcome:     "The lock order is documented.",
	OutcomeKind: l1.OutcomeResolved,
}

// commitEmail is the address the commit's recorded answer puts in a position.
const commitEmail = "robin@example.com"

func buildDoc(t *testing.T, src string, root connector.Event, children []connector.Event, body l1.Body) l1.Document {
	t.Helper()
	cfg := testRepo(src)
	resolver, err := cfg.Resolver()
	if err != nil {
		t.Fatalf("building the resolver: %v", err)
	}
	doc, err := l1.Build(l1.Input{Root: root, Children: children, Resolver: resolver, Repo: cfg})
	if err != nil {
		t.Fatalf("building %s: %v", root.NativeID, err)
	}
	doc, _, err = doc.WithBody(body)
	if err != nil {
		t.Fatalf("adding the body of %s: %v", root.NativeID, err)
	}
	return doc
}

// exchange is one recorded call: what the worker asks, and what the model says.
type exchange struct {
	name       string
	doc        func(t *testing.T, src string) l1.Document
	candidates []assertworker.Candidate
	answer     map[string]any
}

func issueDoc(t *testing.T, src string) l1.Document { issue, _ := documents(t, src); return issue }
func prDoc(t *testing.T, src string) l1.Document    { _, pr := documents(t, src); return pr }

func exchanges() []exchange {
	return []exchange{
		{
			name:   "the issue, with nothing to continue, opens a topic",
			doc:    issueDoc,
			answer: map[string]any{"assertions": []map[string]any{{"topic": "new", "topic_name": topicName, "position": issuePosition}}},
		},
		{
			name:       "the pull request continues it with a different position",
			doc:        prDoc,
			candidates: []assertworker.Candidate{{Name: topicName, Current: issuePosition}},
			answer:     map[string]any{"assertions": []map[string]any{{"topic": "T1", "topic_name": topicName, "position": prPosition}}},
		},
		{
			name:       "the issue read again finds the topic it opened and takes the same position",
			doc:        issueDoc,
			candidates: []assertworker.Candidate{{Name: topicName, Current: issuePosition}},
			answer:     map[string]any{"assertions": []map[string]any{{"topic": "T1", "topic_name": topicName, "position": issuePosition}}},
		},
		{
			name: "the commit's answer carries what must not be stored",
			doc:  commitDocument,
			answer: map[string]any{"assertions": []map[string]any{
				{"topic": "new", "topic_name": "Who to ask about the lock order", "position": "Ask " + commitEmail + " before changing the lock order."},
				{"topic": "new", "topic_name": "   ", "position": "A position on a topic with no name."},
				{"topic": "new", "topic_name": "A topic with no position", "position": "   "},
			}},
		},
	}
}

func fixturePath() string { return filepath.Join("testdata", "fixtures", "github.json") }

func assertBudget() int {
	tier, _ := llm.Default().Tier(llm.TierAssert)
	return tier.MaxTokens
}

func loadFixtures(t *testing.T) *llm.Fixtures {
	t.Helper()
	fx, err := llm.LoadFixtures(os.DirFS(filepath.Dir(fixturePath())))
	if err != nil {
		t.Fatalf("reading %s: %v\nrun `go test ./internal/service/assertworker -record`", fixturePath(), err)
	}
	return fx
}

// TestRecordFixtures rewrites the recorded answers with -record, and without it
// checks that what is on disk is what this package would send.
func TestRecordFixtures(t *testing.T) {
	file := llm.FixtureFile{}
	for _, ex := range exchanges() {
		body, err := json.Marshal(ex.answer)
		if err != nil {
			t.Fatalf("%s: encoding the answer: %v", ex.name, err)
		}
		file.Completions = append(file.Completions, llm.CompletionFixture{
			Tier:     llm.TierAssert,
			Request:  assertworker.RequestFor(ex.doc(t, "github-acme"), ex.candidates, assertBudget()),
			Response: llm.Response{JSON: body, StopReason: llm.StopEnd, Model: "recorded"},
		})
	}
	recorded, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		t.Fatalf("encoding the fixtures: %v", err)
	}
	recorded = append(recorded, '\n')

	if *record {
		if err := os.MkdirAll(filepath.Dir(fixturePath()), 0o755); err != nil {
			t.Fatalf("making testdata/fixtures: %v", err)
		}
		if err := os.WriteFile(fixturePath(), recorded, 0o644); err != nil {
			t.Fatalf("writing %s: %v", fixturePath(), err)
		}
		t.Logf("recorded %d answers in %s", len(file.Completions), fixturePath())
		return
	}
	onDisk, err := os.ReadFile(fixturePath())
	if err != nil {
		t.Fatalf("reading %s: %v\nrun `go test ./internal/service/assertworker -record`", fixturePath(), err)
	}
	if string(onDisk) != string(recorded) {
		t.Fatalf("%s is not what this package would record: run `go test ./internal/service/assertworker -record`", fixturePath())
	}

	// The fake loads them, which validates every answer against the schema its
	// request asked for — so the answers above are ones this build accepts.
	registry, err := llm.NewFake(llm.Default(), loadFixtures(t))
	if err != nil {
		t.Fatalf("building the fake registry: %v", err)
	}
	completer, err := registry.Completer(llm.TierAssert)
	if err != nil {
		t.Fatalf("resolving the assert tier: %v", err)
	}
	for _, c := range file.Completions {
		if _, err := completer.Complete(t.Context(), c.Request); err != nil {
			t.Fatalf("the recorded answers do not cover a request this package sends: %v", err)
		}
	}
}

// The request is a function of what the team said: two sources holding the same
// conversation ask the model the same thing, which is what lets a test take a
// source of its own and still replay the recording.
func TestTheRequestDoesNotDependOnTheSource(t *testing.T) {
	a, _ := documents(t, "github-acme")
	b, _ := documents(t, "github-other")
	candidates := []assertworker.Candidate{{Name: topicName, Current: issuePosition}}
	if llm.FixtureKey(llm.TierAssert, assertworker.RequestFor(a, candidates, 0)) !=
		llm.FixtureKey(llm.TierAssert, assertworker.RequestFor(b, candidates, 0)) {
		t.Error("the same conversation in two sources makes two different requests")
	}
}
