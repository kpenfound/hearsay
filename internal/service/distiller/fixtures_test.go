package distiller_test

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
	"github.com/kpenfound/hearsay/internal/service/distiller"
)

var record = flag.Bool("record", false, "rewrite the recorded model answers in testdata/fixtures")

const (
	source = "github-acme"
	repo   = "acme/api"
	commit = repo + "@1f0e3dad99908345f7439f8ffabdffc4"
)

var day = time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)

func at(hours int) time.Time { return day.Add(time.Duration(hours) * time.Hour) }

var (
	container = connector.Container{Kind: connector.ContainerRepository, NativeID: repo, Name: repo}
	public    = connector.ACL{{Kind: connector.ACLPublic}}
)

func who(src, nativeID, handle string) *connector.Identity {
	return &connector.Identity{Source: src, Kind: connector.IdentityUser, NativeID: nativeID, Handle: handle, DisplayName: handle}
}

func event(src string, kind connector.Kind, artifact string, when time.Time, author *connector.Identity, title, text string) connector.Event {
	return connector.Event{
		Source:   src,
		NativeID: artifact,
		Kind:     kind,
		Time:     when,
		Payload: connector.Payload{
			Artifact:  artifact,
			Container: container,
			URL:       "https://github.com/" + repo,
			Title:     title,
			Text:      text,
			Author:    author,
		},
		ACL: public,
	}
}

func revised(ev connector.Event, token string, editedAt time.Time) connector.Event {
	ev.NativeID = ev.Payload.Artifact + "@" + token
	ev.Payload.Revision = &connector.Revision{Token: token, EditedAt: editedAt}
	return ev
}

func reply(ev connector.Event, root string) connector.Event {
	ev.Payload.Parent = root
	ev.Payload.Thread = root
	return ev
}

const (
	issueID  = "l1:" + source + ":" + repo + "#12"
	prID     = "l1:" + source + ":" + repo + "#31"
	commitID = "l1:" + source + ":" + commit
)

func fixtureEvents(src string) []connector.Event {
	issue := revised(event(src, connector.KindIssue, repo+"#12", at(0), who(src, "u1", "kpenfound"),
		"engine: writes race with the job queue",
		"The engine writes before it takes the lock. @samr have you seen this?"), "2026-09-09T12:00:00Z", at(0))
	issueComment := reply(event(src, connector.KindMessage, repo+"#12:comment:1", at(1), who(src, "u2", "samr"),
		"", "Seen it twice this week. We should take the lock first."), repo+"#12")
	issueAnswer := reply(event(src, connector.KindMessage, repo+"#12:comment:2", at(2), who(src, "u1", "kpenfound"),
		"", "Agreed. I will send a change."), repo+"#12")

	pr := revised(event(src, connector.KindPullRequest, repo+"#31", at(3), who(src, "u1", "kpenfound"),
		"engine: take the lock before writing",
		"Fixes #12 by moving the lock above the write. See https://github.com/acme/api/issues/12."), "2026-09-09T18:00:00Z", at(6))
	review := reply(event(src, connector.KindReview, repo+"#31:review:77", at(4), who(src, "u2", "samr"),
		"", "Approved. Worth checking whether the job queue needs the same treatment."), repo+"#31")
	reviewComment := reply(event(src, connector.KindReviewComment, repo+"#31:comment:88", at(5), who(src, "u2", "samr"),
		"engine/server/write.go", "This comment above the lock is now wrong."), repo+"#31")

	commitEvent := event(src, connector.KindCommit, commit, at(7), who(src, "u1", "kpenfound"),
		"engine: take the lock before writing",
		"engine: take the lock before writing\n\nThe write raced with the job queue. Fixes #12.")

	return []connector.Event{issue, issueComment, issueAnswer, pr, review, reviewComment, commitEvent}
}

func laterComment(src string) connector.Event {
	return reply(event(src, connector.KindMessage, repo+"#31:comment:99", at(8), who(src, "u1", "kpenfound"),
		"", "Merged. The job queue is #40."), repo+"#31")
}

const retractedComment = repo + "#12:comment:1"

const (
	pastedSecret    = "tangerine-otter-42"
	secretCommentID = repo + "#12:comment:3"
)

func secretComment(src string) connector.Event {
	return reply(event(src, connector.KindMessage, secretCommentID, at(2).Add(30*time.Minute), who(src, "u2", "samr"),
		"", "For staging, the vault passphrase is "+pastedSecret+"."), repo+"#12")
}

func without(events []connector.Event, artifact string) []connector.Event {
	var kept []connector.Event
	for _, ev := range events {
		if ev.Payload.Artifact != artifact {
			kept = append(kept, ev)
		}
	}
	return kept
}

func testRepo(src string) config.Repo {
	return config.Repo{
		Path:   "testdata",
		Digest: "sha256:test",
		Scopes: []config.Scope{{
			ID:       "api",
			Name:     "API",
			Sources:  []config.ScopeSource{{Source: src, Containers: []string{repo}}},
			Tracker:  config.SourceRef{Source: src, Project: repo},
			Entities: []string{"code:acme/api:engine"},
		}},
		Principals: []principal.Principal{{
			ID:         "kyle",
			Kind:       principal.KindHuman,
			Identities: []principal.Identity{{Source: src, NativeID: "u1", Handle: "kpenfound"}},
		}, {
			ID:         "sam",
			Kind:       principal.KindHuman,
			Identities: []principal.Identity{{Source: src, NativeID: "u2", Handle: "samr"}},
		}},
		Code: []config.CodeEntity{{
			ID:      "code:acme/api:engine",
			Type:    config.TypeModule,
			Name:    "engine",
			Aliases: []string{"the engine"},
		}, {
			ID:      "code:acme/api:queue",
			Type:    config.TypeModule,
			Name:    "queue",
			Aliases: []string{"the job queue"},
		}},
		LLM: llm.Default(),
	}
}

var answers = map[string]map[string]any{
	issueID: {
		"summary":        "The engine writes before taking the lock, which races with the job queue. Two people confirmed it, and the author said they would send a change.",
		"question":       "Should the engine take the lock before writing?",
		"outcome":        "The team agreed the lock has to be taken before the write.",
		"outcome_kind":   "decided",
		"open_questions": []string{"Does the job queue have the same problem?"},
	},
	prID: {
		"summary":        "Moves the lock above the write in the engine, closing the race the issue described. Approved on review.",
		"change":         "Takes the engine's lock before writing rather than after.",
		"outcome":        "Approved.",
		"outcome_kind":   "resolved",
		"open_questions": []string{"Does the job queue need the same treatment?"},
	},
	commitID: {
		"summary":      "Takes the engine's lock before writing, closing the race with the job queue.",
		"change":       "Moves the lock acquisition above the write.",
		"outcome":      "The race described in the issue is closed.",
		"outcome_kind": "resolved",
	},
}

var answersAfterRetraction = map[string]map[string]any{
	issueID: {
		"summary":        "The engine writes before taking the lock, which races with the job queue. The author said they would send a change.",
		"question":       "Should the engine take the lock before writing?",
		"outcome":        "The author proposed taking the lock before the write.",
		"outcome_kind":   "proposed",
		"open_questions": []string{"Does the job queue have the same problem?"},
	},
}

var answersWithoutDecision = map[string]map[string]any{
	issueID: {
		"summary":        "The engine's write order is being investigated again.",
		"question":       "Should the engine take the lock before writing?",
		"outcome":        "",
		"outcome_kind":   "none",
		"open_questions": []string{"Does the write race with the job queue?"},
	},
}

func issueWithoutDecision(src string) connector.Event {
	issue := revised(fixtureEvents(src)[0], "2026-09-09T22:00:00Z", at(10))
	issue.Payload.Text = "The engine's write order is being investigated again."
	return issue
}

var answersWithSecret = map[string]map[string]any{
	issueID: {
		"summary":        "The engine writes before taking the lock, which races with the job queue. Two people confirmed it, the author said they would send a change, and samr shared the staging vault passphrase " + pastedSecret + ".",
		"question":       "Should the engine take the lock before writing?",
		"outcome":        "The team agreed the lock has to be taken before the write.",
		"outcome_kind":   "decided",
		"open_questions": []string{"Does the job queue have the same problem?"},
	},
}

func fixturePath() string { return filepath.Join("testdata", "fixtures", "github.json") }

func loadFixtures(t *testing.T) *llm.Fixtures {
	t.Helper()
	fx, err := llm.LoadFixtures(os.DirFS(filepath.Dir(fixturePath())))
	if err != nil {
		t.Fatalf("reading %s: %v\nrun `go test ./internal/service/distiller -record` to record it", fixturePath(), err)
	}
	if fx.Len() == 0 {
		t.Fatalf("%s records nothing: run `go test ./internal/service/distiller -record`", fixturePath())
	}
	return fx
}

func newFakeRegistry(t *testing.T) llm.Registry {
	t.Helper()
	registry, err := llm.NewFake(testRepo(source).LLM, loadFixtures(t))
	if err != nil {
		t.Fatalf("building the fake registry: %v", err)
	}
	return registry
}

func TestRecordFixtures(t *testing.T) {
	file := llm.FixtureFile{}

	seen := map[string]bool{}
	for _, state := range fixtureStates() {
		for _, doc := range documentsIn(t, state.events) {
			answer, ok := state.answers[doc.ID]
			if !ok {
				answer, ok = answers[doc.ID]
			}
			if !ok {
				t.Fatalf("no answer is written down for %s", doc.ID)
			}
			body, _ := modelAnswer(t, answer)
			req := distiller.RequestFor(doc, distillBudget())
			key := llm.FixtureKey(llm.TierDistill, req)
			if seen[key] {
				continue
			}
			seen[key] = true
			file.Completions = append(file.Completions, llm.CompletionFixture{
				Tier:     llm.TierDistill,
				Request:  req,
				Response: llm.Response{JSON: body, StopReason: llm.StopEnd, Model: "recorded"},
			})
		}
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
		t.Fatalf("reading %s: %v\nrun `go test ./internal/service/distiller -record`", fixturePath(), err)
	}
	if string(onDisk) != string(recorded) {
		t.Fatalf("%s is not what this package would record: run `go test ./internal/service/distiller -record`", fixturePath())
	}

	completer, err := newFakeRegistry(t).Completer(llm.TierDistill)
	if err != nil {
		t.Fatalf("resolving the distill tier: %v", err)
	}
	for _, c := range file.Completions {
		if _, err := completer.Complete(t.Context(), c.Request); err != nil {
			t.Fatalf("the recorded answers do not cover a request this package sends: %v", err)
		}
	}
}

func distillBudget() int {
	tier, _ := testRepo(source).LLM.Tier(llm.TierDistill)
	return tier.MaxTokens
}

type fixtureState struct {
	events  []connector.Event
	answers map[string]map[string]any
}

func fixtureStates() []fixtureState {
	base := fixtureEvents(source)
	return []fixtureState{
		{events: base},
		{events: append(append([]connector.Event{}, base...), laterComment(source))},
		{events: without(base, retractedComment), answers: answersAfterRetraction},
		{events: append(append([]connector.Event{}, base...), issueWithoutDecision(source)), answers: answersWithoutDecision},
		{events: append(append([]connector.Event{}, base...), secretComment(source)), answers: answersWithSecret},
	}
}

func documentsIn(t *testing.T, events []connector.Event) []l1.Document {
	t.Helper()
	if len(events) == 0 {
		return nil
	}
	repo := testRepo(events[0].Source)
	resolver, err := repo.Resolver()
	if err != nil {
		t.Fatalf("building the resolver: %v", err)
	}
	byRoot := map[string][]connector.Event{}
	roots := map[string]connector.Event{}
	var order []string
	for _, ev := range events {
		if _, ok := l1.KindFor(ev); ok {
			if _, seen := roots[ev.Payload.Artifact]; !seen {
				order = append(order, ev.Payload.Artifact)
			}
			roots[ev.Payload.Artifact] = ev
			continue
		}
		root := ev.Payload.Thread
		if root == "" {
			root = ev.Payload.Parent
		}
		byRoot[root] = append(byRoot[root], ev)
	}

	var docs []l1.Document
	for _, artifact := range order {
		doc, err := l1.Build(l1.Input{
			Root:     roots[artifact],
			Children: byRoot[artifact],
			Resolver: resolver,
			Repo:     repo,
		})
		if err != nil {
			t.Fatalf("building the document for %s: %v", artifact, err)
		}
		docs = append(docs, doc)
	}
	return docs
}
