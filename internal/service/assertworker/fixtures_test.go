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
	"github.com/kpenfound/hearsay/internal/service/distiller"
)

var record = flag.Bool("record", false, "rewrite the recorded model answers in testdata/fixtures")

const (
	repo      = "acme/api"
	topicName = "Where the engine takes its lock relative to the write"

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

	pr.Payload.Native = json.RawMessage(`{"state":"closed","merged_at":"` + at(4).Format(time.RFC3339) + `"}`)
	review := event(src, connector.KindReview, repo+"#31:review:77", at(4), "samr", "u2", "", "Approved, and merged.")
	review.Payload.Parent, review.Payload.Thread = repo+"#31", repo+"#31"
	return fixture{
		issue: issue, pr: pr, prChildren: []connector.Event{review},
		issueID: l1.DocID(src, repo+"#12"), prID: l1.DocID(src, repo+"#31"),
		issueTrackerID:  "tracker:" + src + ":" + repo + "#12",
		prTrackerItemID: "tracker:" + src + ":" + repo + "#31",
	}
}

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

func documents(t *testing.T, src string) (issue, pr l1.Document) {
	t.Helper()
	f := newFixture(src)
	return buildDoc(t, src, f.issue, nil, issueBody), buildDoc(t, src, f.pr, f.prChildren, prBody)
}

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

type exchange struct {
	name   string
	doc    func(t *testing.T, src string) l1.Document
	answer map[string]any
}

const restatedPosition = "The job queue should hold the lock."

func issueComment(src string) connector.Event {
	comment := event(src, connector.KindMessage, repo+"#12:comment:90", at(5), "samr", "u2", "",
		"Noting for the record that this was fixed differently in #31.")
	comment.Payload.Parent, comment.Payload.Thread = repo+"#12", repo+"#12"
	return comment
}

var commentedIssueBody = l1.Body{
	Summary:     "The engine writes before taking its lock, which races with the job queue. The author proposes moving the lock into the job queue; a later comment notes #31 fixed it differently.",
	Question:    "Where should the lock be taken?",
	Outcome:     "Proposed: take the lock inside the job queue.",
	OutcomeKind: l1.OutcomeProposed,
}

func commentedIssueDoc(t *testing.T, src string) l1.Document {
	t.Helper()
	f := newFixture(src)
	return buildDoc(t, src, f.issue, []connector.Event{issueComment(src)}, commentedIssueBody)
}

func issueDoc(t *testing.T, src string) l1.Document { issue, _ := documents(t, src); return issue }
func prDoc(t *testing.T, src string) l1.Document    { _, pr := documents(t, src); return pr }

func exchanges() []exchange {
	one := func(name, position string) map[string]any {
		return map[string]any{"assertions": []map[string]any{{"topic_name": name, "position": position}}}
	}
	return []exchange{
		{name: "the issue proposes moving the lock", doc: issueDoc, answer: one(topicName, issuePosition)},
		{name: "the pull request takes the lock before the write", doc: prDoc, answer: one(topicName, prPosition)},
		{name: "the issue, commented on after the merge, restates its proposal in other words", doc: commentedIssueDoc, answer: one(topicName, restatedPosition)},
		{
			name: "the commit's answer carries what must not be stored",
			doc:  commitDocument,
			answer: map[string]any{"assertions": []map[string]any{
				{"topic_name": "Who to ask about the lock order", "position": "Ask " + commitEmail + " before changing the lock order."},
				{"topic_name": "   ", "position": "A position on a topic with no name."},
				{"topic_name": "A topic with no position", "position": "   "},
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

func TestRecordFixtures(t *testing.T) {
	file := llm.FixtureFile{}
	for _, ex := range exchanges() {
		body, err := json.Marshal(ex.answer)
		if err != nil {
			t.Fatalf("%s: encoding the answer: %v", ex.name, err)
		}
		file.Completions = append(file.Completions, llm.CompletionFixture{
			Tier:     llm.TierAssert,
			Request:  assertworker.RequestFor(ex.doc(t, "github-acme"), assertBudget()),
			Response: llm.Response{JSON: body, StopReason: llm.StopEnd, Model: "recorded"},
		})
	}

	root := commandIssue("github-acme")
	doc := buildDoc(t, root.Source, root, nil, issueBody)
	budget, _ := llm.Default().Tier(llm.TierDistill)
	body, err := json.Marshal(map[string]any{
		"summary": issueBody.Summary, "question": issueBody.Question,
		"outcome": issueBody.Outcome,
	})
	if err != nil {
		t.Fatal(err)
	}
	file.Completions = append(file.Completions, llm.CompletionFixture{
		Tier: llm.TierDistill, Request: distiller.RequestFor(doc, budget.MaxTokens),
		Response: llm.Response{JSON: body, StopReason: llm.StopEnd, Model: "recorded"},
	})
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

	registry, err := llm.NewFake(llm.Default(), loadFixtures(t))
	if err != nil {
		t.Fatalf("building the fake registry: %v", err)
	}
	for _, c := range file.Completions {
		completer, err := registry.Completer(c.Tier)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := completer.Complete(t.Context(), c.Request); err != nil {
			t.Fatalf("the recorded answers do not cover a request this package sends: %v", err)
		}
	}
}

func TestTheAnswerIsPositionsAndTheirQuestions(t *testing.T) {
	request := assertworker.RequestFor(prDoc(t, "github-acme"), assertBudget())
	for _, tc := range []struct {
		name  string
		json  string
		valid bool
	}{
		{"a position", `{"assertions":[{"topic_name":"lock","position":"before write"}]}`, true},
		{"no positions", `{"assertions":[]}`, true},
		{"a topic choice", `{"assertions":[{"topic":"T1","topic_name":"lock","position":"before write"}]}`, false},
		{"a judgement", `{"assertions":[{"topic_name":"lock","position":"before write","judgement":"changes"}]}`, false},
		{"no question", `{"assertions":[{"position":"before write"}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := request.Schema.ValidateJSON([]byte(tc.json))
			if (err == nil) != tc.valid {
				t.Errorf("schema validation = %v, want valid %v", err, tc.valid)
			}
		})
	}
}

func commandIssue(src string) connector.Event {
	ev := newFixture(src).issue
	ev.NativeID, ev.Payload.Artifact = repo+"#77", repo+"#77"
	return ev
}
