package l1_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
)

// testRepo is the configuration a document is built against: one scope over the
// repository, with a tracker and the code entities it is about.
var testRepo = config.Repo{
	Path: "testdata",
	Scopes: []config.Scope{{
		ID:       "api",
		Name:     "API",
		Sources:  []config.ScopeSource{{Source: source, Containers: []string{repo}}},
		Tracker:  config.SourceRef{Source: source, Project: repo},
		Entities: []string{"code:acme/api:engine"},
	}, {
		ID:       "elsewhere",
		Sources:  []config.ScopeSource{{Source: source, Containers: []string{"acme/other"}}},
		Entities: []string{"code:acme/other:thing"},
	}},
	Code: testCode,
}

// pullRequest is the fixture conversation: a change proposal, a review, a
// review comment and a comment, in the shapes the GitHub table of
// docs/connector-contract.md specifies.
func pullRequest() (connector.Event, []connector.Event) {
	pr := revised(event(connector.KindPullRequest, repo+"#31", at(0), who("u1", "kpenfound"),
		"engine: take the lock before writing",
		"Fixes the race in #12. @samr please look."), "2026-09-09T14:00:00Z", at(2))

	review := reply(event(connector.KindReview, repo+"#31:review:77", at(3), who("u2", "samr"),
		"", "Approving. The job queue path is the one I was worried about."), repo+"#31")
	comment := reply(revised(event(connector.KindMessage, repo+"#31:comment:88", at(4), who("u1", "kpenfound"),
		"", "Merged."), "2026-09-09T17:00:00Z", at(5)), repo+"#31")
	bot := reply(event(connector.KindMessage, repo+"#31:comment:89", at(6), agent("b1", "shed-bot"),
		"", "Deployed to staging."), repo+"#31")
	return pr, []connector.Event{comment, bot, review}
}

func TestBuild(t *testing.T) {
	resolver := testPrincipals(t)
	root, children := pullRequest()

	doc, err := l1.Build(l1.Input{Root: root, Children: children, Resolver: resolver, Repo: testRepo})
	if err != nil {
		t.Fatalf("Build() = %v, want no error", err)
	}

	if want := "l1:" + source + ":" + repo + "#31"; doc.ID != want {
		t.Errorf("ID = %q, want %q", doc.ID, want)
	}
	if doc.Kind != l1.KindPR {
		t.Errorf("Kind = %q, want %q", doc.Kind, l1.KindPR)
	}
	if doc.Source.System != source || doc.Source.NativeID != repo+"#31" {
		t.Errorf("Source = %+v, want the artifact and the source it came from", doc.Source)
	}

	// Provenance: the current revision of the artifact and of each reply, in
	// the order the document reads.
	wantRefs := []string{
		connector.EventID(source, repo+"#31@2026-09-09T14:00:00Z"),
		connector.EventID(source, repo+"#31:review:77"),
		connector.EventID(source, repo+"#31:comment:88@2026-09-09T17:00:00Z"),
		connector.EventID(source, repo+"#31:comment:89"),
	}
	if !slices.Equal(doc.L0Refs, wantRefs) {
		t.Errorf("L0Refs = %v\nwant %v", doc.L0Refs, wantRefs)
	}

	// The artifact happened when it happened, was edited later, and the
	// conversation went on after that.
	if !doc.Time.Created.Equal(at(0)) {
		t.Errorf("Time.Created = %s, want %s", doc.Time.Created, at(0))
	}
	if !doc.Time.Updated.Equal(at(2)) {
		t.Errorf("Time.Updated = %s, want the revision's edit time %s", doc.Time.Updated, at(2))
	}
	if !doc.Time.LastActivity.Equal(at(6)) {
		t.Errorf("Time.LastActivity = %s, want the newest thing in the document %s", doc.Time.LastActivity, at(6))
	}

	// Whoever opened it authored it; whoever reviewed it is a reviewer; an
	// agent that commented is an agent.
	wantParticipants := []l1.Participant{
		{PrincipalID: "kyle", Role: connector.RoleAuthor},
		{PrincipalID: "sam", Role: connector.RoleReviewer},
		{PrincipalID: "shed", Role: connector.RoleAgent},
	}
	if !slices.Equal(doc.Participants, wantParticipants) {
		t.Errorf("Participants = %v\nwant %v", doc.Participants, wantParticipants)
	}

	// The scope is the covering scope's entities, the entities the text names,
	// and the tracker item the artifact is. The scope that covers another
	// repository contributes nothing.
	wantScope := []string{"code:acme/api:engine", "code:acme/api:queue", "tracker:" + source + ":" + repo + "#31"}
	if !slices.Equal(doc.Scope, wantScope) {
		t.Errorf("Scope = %v\nwant %v", doc.Scope, wantScope)
	}

	wantRefsOut := []l1.Reference{
		{Type: l1.RefPerson, ID: "sam"},
		{Type: l1.RefSystem, ID: "code:acme/api:engine"},
		{Type: l1.RefSystem, ID: "code:acme/api:queue"},
		{Type: l1.RefTrackerItem, ID: repo + "#12"},
	}
	assertRefs(t, doc.References, wantRefsOut)

	// The conversation reads in order, with everything attributed to the
	// principal rather than to the login.
	for _, want := range []string{
		"# engine: take the lock before writing",
		"pull_request by kyle at 2026-09-09T12:00:00Z",
		"## review by sam at 2026-09-09T15:00:00Z",
		"## message by kyle at 2026-09-09T16:00:00Z",
		"## message by shed at 2026-09-09T18:00:00Z",
	} {
		if !strings.Contains(doc.RawText, want) {
			t.Errorf("RawText does not contain %q:\n%s", want, doc.RawText)
		}
	}
	if at := strings.Index(doc.RawText, "## review"); at > strings.Index(doc.RawText, "## message by kyle") {
		t.Errorf("the conversation is out of order:\n%s", doc.RawText)
	}

	// The body has not been distilled yet, so there is no text to embed.
	if doc.Text != "" {
		t.Errorf("Text = %q before a body was set, want empty", doc.Text)
	}
	if err := doc.Validate(); err == nil {
		t.Error("Validate() on a document with no body = nil, want an error: every document has an outcome kind")
	}
}

// The order children are handed over in is the caller's business and not the
// document's: a store returns them in one order, a test in another, and the
// document has to be the same either way or re-distilling would rewrite the row.
func TestBuildIsIndependentOfTheOrderChildrenArriveIn(t *testing.T) {
	resolver := testPrincipals(t)
	root, children := pullRequest()

	forwards, err := l1.Build(l1.Input{Root: root, Children: children, Resolver: resolver, Repo: testRepo})
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}
	slices.Reverse(children)
	backwards, err := l1.Build(l1.Input{Root: root, Children: children, Resolver: resolver, Repo: testRepo})
	if err != nil {
		t.Fatalf("Build(reversed) = %v", err)
	}
	if forwards.RawText != backwards.RawText {
		t.Errorf("the document depends on the order its children arrived in:\n%s\n---\n%s", forwards.RawText, backwards.RawText)
	}
	if !slices.Equal(forwards.L0Refs, backwards.L0Refs) {
		t.Errorf("L0Refs = %v and %v for the same events in two orders", forwards.L0Refs, backwards.L0Refs)
	}
}

// A document quotes every event in it, so it is readable by whoever may read
// all of them. A reply somebody restricted narrows the document rather than
// being published under the artifact's own access list.
func TestBuildFoldsTheAccessListOfEveryEventItQuotes(t *testing.T) {
	resolver := testPrincipals(t)
	root, children := pullRequest()
	// The repository went private between the change proposal and the review,
	// so the review carries the collaborator group as well as public.
	children[2].ACL = connector.ACL{{Kind: connector.ACLPublic}, {Kind: connector.ACLGroup, Source: source, NativeID: repo}}
	root.ACL = connector.ACL{{Kind: connector.ACLGroup, Source: source, NativeID: repo, Label: "collaborators"}}

	doc, err := l1.Build(l1.Input{Root: root, Children: children[2:3], Resolver: resolver, Repo: testRepo})
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}
	if len(doc.ACL) != 1 || doc.ACL[0].Kind != connector.ACLGroup || doc.ACL[0].NativeID != repo {
		t.Errorf("ACL = %v, want only the grant both events carry", doc.ACL)
	}
	// The label is a display name, so the two spellings of one grant are one
	// grant and the artifact's own label survives.
	if doc.ACL[0].Label != "collaborators" {
		t.Errorf("ACL[0].Label = %q, want the artifact's own", doc.ACL[0].Label)
	}
}

// A reply that shares no grant with the artifact cannot narrow the document to
// nothing — a document nobody may read is unreadable for ever, which is not the
// same thing as a private one — so it is left out of the document instead.
func TestBuildLeavesOutAReplyThatSharesNoGrant(t *testing.T) {
	resolver := testPrincipals(t)
	root, children := pullRequest()
	secret := children[0]
	secret.ACL = connector.ACL{{Kind: connector.ACLIdentity, Source: source, NativeID: "u9"}}

	doc, err := l1.Build(l1.Input{Root: root, Children: []connector.Event{secret}, Resolver: resolver, Repo: testRepo})
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}
	if len(doc.ACL) != 1 || doc.ACL[0].Kind != connector.ACLPublic {
		t.Errorf("ACL = %v, want the artifact's own", doc.ACL)
	}
	if len(doc.L0Refs) != 1 {
		t.Errorf("L0Refs = %v, want only the artifact: the reply nobody in the document may read is not in it", doc.L0Refs)
	}
	if strings.Contains(doc.RawText, "Merged.") {
		t.Errorf("RawText quotes a reply that is not in the document:\n%s", doc.RawText)
	}
}

// An artifact that makes no document of its own is not an error to be retried;
// it is a job with nothing to do.
func TestBuildRefusesAnArtifactThatIsPartOfAnotherDocument(t *testing.T) {
	_, children := pullRequest()
	_, err := l1.Build(l1.Input{Root: children[0], Repo: testRepo})
	if !errors.Is(err, l1.ErrNotDistilled) {
		t.Fatalf("Build(a comment) = %v, want l1.ErrNotDistilled", err)
	}
}

// A connector's own kind is read through the core kind it says it behaves like,
// which is what docs/connector-contract.md promises: a source nobody wrote code
// for here still distils.
func TestBuildReadsAnExtensionKindThroughItsBaseKind(t *testing.T) {
	root := event("jira.story", "ENG-7", at(0), who("u1", "kpenfound"), "the story", "do the thing")
	root.Payload.BaseKind = connector.KindIssue

	doc, err := l1.Build(l1.Input{Root: root, Repo: testRepo})
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}
	if doc.Kind != l1.KindIssue {
		t.Errorf("Kind = %q, want %q: an extension kind is read through its base kind", doc.Kind, l1.KindIssue)
	}
}

// A child that hangs off something else is a caller mistake, and a document
// that quoted it would attribute one conversation's words to another.
func TestBuildRefusesAChildOfAnotherConversation(t *testing.T) {
	root, children := pullRequest()
	stray := reply(children[0], repo+"#99")
	if _, err := l1.Build(l1.Input{Root: root, Children: []connector.Event{stray}, Repo: testRepo}); err == nil {
		t.Fatal("Build() with a child of another conversation = nil, want an error")
	}
}

// The scrub runs before anything is stored, on the conversation and on what the
// model wrote about it.
func TestBuildScrubsTheConversation(t *testing.T) {
	root, _ := pullRequest()
	root.Payload.Text = "the key is AKIAIOSFODNN7EXAMPLE, mail kyle@example.com"

	doc, err := l1.Build(l1.Input{Root: root, Repo: testRepo})
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}
	for _, secret := range []string{"AKIAIOSFODNN7EXAMPLE", "kyle@example.com"} {
		if strings.Contains(doc.RawText, secret) {
			t.Errorf("RawText holds %q:\n%s", secret, doc.RawText)
		}
	}

	body := l1.Body{Summary: "the key is AKIAIOSFODNN7EXAMPLE", OutcomeKind: l1.OutcomeNone}
	withBody, redacted, err := doc.WithBody(body)
	if err != nil {
		t.Fatalf("WithBody() = %v", err)
	}
	if strings.Contains(withBody.Text, "AKIAIOSFODNN7EXAMPLE") || strings.Contains(withBody.Body.Summary, "AKIAIOSFODNN7EXAMPLE") {
		t.Errorf("the body was not scrubbed: %q", withBody.Text)
	}
	if !slices.Contains(redacted, "aws-key") {
		t.Errorf("WithBody() reported %v, want the shape it took out", redacted)
	}
}

// The distillation is what gets embedded, so it holds what the model wrote and
// nothing that would move every document from one repository closer together.
func TestWithBodyRendersTheDistillation(t *testing.T) {
	root, _ := pullRequest()
	doc, err := l1.Build(l1.Input{Root: root, Repo: testRepo})
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}
	doc, _, err = doc.WithBody(l1.Body{
		Summary:       "The engine takes the lock before writing.",
		Change:        "Moves the lock acquisition above the write.",
		Outcome:       "Merged.",
		OutcomeKind:   l1.OutcomeResolved,
		OpenQuestions: []string{"Does the queue need the same treatment?", "  "},
	})
	if err != nil {
		t.Fatalf("WithBody() = %v", err)
	}
	for _, want := range []string{
		"The engine takes the lock before writing.",
		"Change: Moves the lock acquisition above the write.",
		"Outcome (resolved): Merged.",
		"- Does the queue need the same treatment?",
	} {
		if !strings.Contains(doc.Text, want) {
			t.Errorf("Text does not contain %q:\n%s", want, doc.Text)
		}
	}
	if strings.Contains(doc.Text, repo+"#31") {
		t.Errorf("Text carries the artifact id, which is a column of its own:\n%s", doc.Text)
	}
	if len(doc.Body.OpenQuestions) != 1 {
		t.Errorf("OpenQuestions = %v, want the blank one dropped", doc.Body.OpenQuestions)
	}
	if err := doc.Validate(); err != nil {
		t.Errorf("Validate() = %v, want no error", err)
	}
}

// Every document has an outcome kind, and it is one of the design's five.
func TestWithBodyRefusesAnOutcomeKindThatIsNotOneOfTheFive(t *testing.T) {
	root, _ := pullRequest()
	doc, err := l1.Build(l1.Input{Root: root, Repo: testRepo})
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}
	for _, kind := range []l1.OutcomeKind{"", "merged", "RESOLVED"} {
		if _, _, err := doc.WithBody(l1.Body{Summary: "something", OutcomeKind: kind}); !errors.Is(err, l1.ErrInvalidDocument) {
			t.Errorf("WithBody(%q) = %v, want l1.ErrInvalidDocument", kind, err)
		}
	}
	for _, kind := range l1.OutcomeKinds {
		if _, _, err := doc.WithBody(l1.Body{Summary: "something", OutcomeKind: kind}); err != nil {
			t.Errorf("WithBody(%q) = %v, want no error", kind, err)
		}
	}
}

// A document with nothing in the body is not a document: text is what gets
// embedded, and a row with none of it would be found by nothing.
func TestWithBodyRefusesAnEmptyBody(t *testing.T) {
	root, _ := pullRequest()
	doc, err := l1.Build(l1.Input{Root: root, Repo: testRepo})
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}
	if _, _, err := doc.WithBody(l1.Body{OutcomeKind: l1.OutcomeNone}); err == nil {
		t.Fatal("WithBody(empty) = nil, want an error")
	}
}
