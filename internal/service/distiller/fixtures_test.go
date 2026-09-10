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

// The fixture repository: a small GitHub-shaped source, in the shapes
// docs/connector-contract.md's GitHub table specifies, and the model answers
// recorded for it.
//
// No test here calls a provider (CLAUDE.md). The answers live in
// testdata/fixtures, keyed by the request, and the fake registry replays them;
// a request nothing was recorded for fails loudly rather than reaching the
// network. Change a prompt and the keys change, which is what `-record` is for:
//
//	go test ./internal/service/distiller -record
//
// It rewrites the file from the answers written out below, which are the part a
// person maintains.

var record = flag.Bool("record", false, "rewrite the recorded model answers in testdata/fixtures")

const (
	// source is the configured source instance the recording was made from.
	// The request a document produces does not depend on it — a document
	// attributes its lines to principals, not to sources — which is what lets
	// an integration test take a source id of its own and still hit the
	// recording. There is a test that says so.
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

// The three artifacts the fixture repository holds, and the document each one
// makes.
const (
	issueID  = "l1:" + source + ":" + repo + "#12"
	prID     = "l1:" + source + ":" + repo + "#31"
	commitID = "l1:" + source + ":" + commit
)

// fixtureEvents is the repository as it first arrives: an issue with two
// comments, a change proposal with a review and a comment, and a commit.
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

// laterComment is what arrives after the repository has been distilled once: a
// new event on an artifact that already has a document, which is what makes the
// distiller re-distil.
func laterComment(src string) connector.Event {
	return reply(event(src, connector.KindMessage, repo+"#31:comment:99", at(8), who(src, "u1", "kpenfound"),
		"", "Merged. The job queue is #40."), repo+"#31")
}

// testRepo is the configuration the fixture repository is distilled against.
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

// testConfig is what the distiller is built from.
func testConfig(src string) *config.Config {
	cfg := config.Default()
	cfg.Repo = testRepo(src)
	return &cfg
}

// answers are the model's replies, one per document, written by hand. They are
// what a person maintains: `-record` turns them into the fixture file, keyed by
// the request the distiller would send.
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

// fixturePath is the recorded file, checked in.
func fixturePath() string { return filepath.Join("testdata", "fixtures", "github.json") }

// loadFixtures reads the recorded answers. A file that is not there, or that
// does not cover what a test asks for, is a prompt that changed without being
// re-recorded.
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

// newFakeRegistry is the registry every test here runs on: the real registry,
// with the network replaced by the recorded answers.
func newFakeRegistry(t *testing.T) llm.Registry {
	t.Helper()
	registry, err := llm.NewFake(testRepo(source).LLM, loadFixtures(t))
	if err != nil {
		t.Fatalf("building the fake registry: %v", err)
	}
	return registry
}

// TestRecordFixtures rewrites the recorded answers from the events and the
// replies above. It is a test so that it runs with the package's own imports
// and helpers; without `-record` it checks that what is on disk is what this
// would write, which is what catches a prompt edited without re-recording.
func TestRecordFixtures(t *testing.T) {
	file := llm.FixtureFile{}
	// One recording per distinct request. Two states of the repository ask the
	// same question about an artifact nothing happened to, and a fixture set
	// with the same request in it twice is one the fake refuses to load.
	seen := map[string]bool{}
	for _, state := range fixtureStates() {
		for _, doc := range documentsIn(t, state) {
			answer, ok := answers[doc.ID]
			if !ok {
				t.Fatalf("no answer is written down for %s", doc.ID)
			}
			body, err := json.Marshal(answer)
			if err != nil {
				t.Fatalf("encoding the answer for %s: %v", doc.ID, err)
			}
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

	// Without -record this is the check that the file on disk is the one this
	// package would write. A prompt or a fixture event edited without
	// re-recording fails here, where it says what to do, rather than as a
	// fixture miss in every test that distils.
	onDisk, err := os.ReadFile(fixturePath())
	if err != nil {
		t.Fatalf("reading %s: %v\nrun `go test ./internal/service/distiller -record`", fixturePath(), err)
	}
	if string(onDisk) != string(recorded) {
		t.Fatalf("%s is not what this package would record: run `go test ./internal/service/distiller -record`", fixturePath())
	}

	// And that the fake can answer every one of them. The registry validates a
	// recorded answer against the schema its request asked for, so this also
	// says the answers written down above are answers this build would accept.
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

// distillBudget is the answer budget the distill tier is configured with, which
// the registry fills in before the adapter sees a request — so it is part of
// what a recording is keyed on.
func distillBudget() int {
	tier, _ := testRepo(source).LLM.Tier(llm.TierDistill)
	return tier.MaxTokens
}

// fixtureStates are the states of the fixture repository a test distils: as it
// first arrives, and after a later comment lands on the change proposal.
func fixtureStates() [][]connector.Event {
	base := fixtureEvents(source)
	return [][]connector.Event{base, append(append([]connector.Event{}, base...), laterComment(source))}
}

// documentsIn builds the documents one state of the repository makes, the same
// way the distiller does: the current revision of each artifact, with the
// events that hang off it.
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
