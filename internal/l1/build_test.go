package l1_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
)

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

func TestBuildChatWindowAcceptsMessageExtension(t *testing.T) {
	ev := event(connector.KindMessage, "m1", at(0), who("u1", "kpenfound"), "", "A channel message")
	ev.Kind = "discord.message"
	ev.Payload.BaseKind = connector.KindMessage
	ev.Payload.Container = connector.Container{Kind: connector.ContainerChannel, NativeID: "C1"}
	key := l1.ChatWindowKey(ev)
	doc, err := l1.BuildChatWindow(key, []connector.Event{ev}, testPrincipals(t), testRepo)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Kind != l1.KindChatThread || doc.Source.NativeID != key || len(doc.L0Refs) != 1 {
		t.Errorf("chat document = %+v", doc)
	}
}

func buildPullRequest(t *testing.T) l1.Document {
	t.Helper()
	root, children := pullRequest()
	doc, err := l1.Build(l1.Input{Root: root, Children: children, Resolver: testPrincipals(t), Repo: testRepo})
	if err != nil {
		t.Fatalf("Build() = %v, want no error", err)
	}
	return doc
}

func TestBuildNamesTheDocumentAfterItsArtifact(t *testing.T) {
	doc := buildPullRequest(t)
	if want := "l1:" + source + ":" + repo + "#31"; doc.ID != want {
		t.Errorf("ID = %q, want %q", doc.ID, want)
	}
	if doc.Kind != l1.KindPR {
		t.Errorf("Kind = %q, want %q", doc.Kind, l1.KindPR)
	}
	if doc.Source.System != source || doc.Source.NativeID != repo+"#31" {
		t.Errorf("Source = %+v, want the artifact and the source it came from", doc.Source)
	}
}

func TestBuildQuotesEveryRevisionInTheOrderTheDocumentReads(t *testing.T) {
	doc := buildPullRequest(t)
	wantRefs := []string{
		connector.EventID(source, repo+"#31@2026-09-09T14:00:00Z"),
		connector.EventID(source, repo+"#31:review:77"),
		connector.EventID(source, repo+"#31:comment:88@2026-09-09T17:00:00Z"),
		connector.EventID(source, repo+"#31:comment:89"),
	}
	if !slices.Equal(doc.L0Refs, wantRefs) {
		t.Errorf("L0Refs = %v\nwant %v", doc.L0Refs, wantRefs)
	}
}

func TestBuildTimesTheDocumentByItsArtifactAndItsNewestEvent(t *testing.T) {
	doc := buildPullRequest(t)
	if !doc.Time.Created.Equal(at(0)) {
		t.Errorf("Time.Created = %s, want %s", doc.Time.Created, at(0))
	}
	if !doc.Time.Updated.Equal(at(2)) {
		t.Errorf("Time.Updated = %s, want the revision's edit time %s", doc.Time.Updated, at(2))
	}
	if !doc.Time.LastActivity.Equal(at(6)) {
		t.Errorf("Time.LastActivity = %s, want the newest thing in the document %s", doc.Time.LastActivity, at(6))
	}
}

func TestBuildNamesEachParticipantByTheirStrongestRole(t *testing.T) {
	doc := buildPullRequest(t)
	wantParticipants := []l1.Participant{
		{PrincipalID: "kyle", Role: connector.RoleAuthor},
		{PrincipalID: "sam", Role: connector.RoleReviewer},
		{PrincipalID: "shed", Role: connector.RoleAgent},
	}
	if !slices.Equal(doc.Participants, wantParticipants) {
		t.Errorf("Participants = %v\nwant %v", doc.Participants, wantParticipants)
	}
}

func TestBuildScopesTheDocumentToTheCodeAndTheTrackerItem(t *testing.T) {
	doc := buildPullRequest(t)
	wantScope := []string{"code:acme/api:engine", "code:acme/api:queue", "tracker:" + source + ":" + repo + "#31"}
	if !slices.Equal(doc.Scope, wantScope) {
		t.Errorf("Scope = %v\nwant %v", doc.Scope, wantScope)
	}
}

func TestBuildReferencesThePeopleSystemsAndItemsTheTextNames(t *testing.T) {
	doc := buildPullRequest(t)
	assertRefs(t, doc.References, []l1.Reference{
		{Type: l1.RefPerson, ID: "sam"},
		{Type: l1.RefSystem, ID: "code:acme/api:engine"},
		{Type: l1.RefSystem, ID: "code:acme/api:queue"},
		{Type: l1.RefTrackerItem, ID: repo + "#12"},
	})
}

func TestBuildRendersTheConversationAttributedAndInOrder(t *testing.T) {
	doc := buildPullRequest(t)
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
}

func TestBuildLeavesTheDistillationEmptyUntilABodyIsSet(t *testing.T) {
	doc := buildPullRequest(t)
	if doc.Text != "" {
		t.Errorf("Text = %q before a body was set, want empty", doc.Text)
	}
	if err := doc.Validate(); err == nil {
		t.Error("Validate() on a document with no body = nil, want an error: every document has an outcome kind")
	}
}

func TestBuildLinksAChangeToTheCodeItTouches(t *testing.T) {
	root, children := pullRequest()
	root.Payload.Paths = []string{"engine/server/lock.go", "engine/server/write_test.go"}
	cfg := testRepo
	cfg.Code = pathCode

	doc, err := l1.Build(l1.Input{Root: root, Children: children, Resolver: testPrincipals(t), Repo: cfg})
	if err != nil {
		t.Fatalf("Build() = %v, want no error", err)
	}
	var systems []string
	for _, ref := range doc.References {
		if ref.Type == l1.RefSystem {
			systems = append(systems, ref.ID)
		}
	}
	wantSystems := []string{"code:acme/api:engine/server", "code:acme/api:engine", "code:acme/api:golang", "code:acme/api:queue"}
	if !slices.Equal(systems, wantSystems) {
		t.Errorf("system references = %v\nwant %v", systems, wantSystems)
	}
	for _, id := range wantSystems {
		if !slices.Contains(doc.Scope, id) {
			t.Errorf("Scope = %v, want it to hold %s", doc.Scope, id)
		}
	}
	if slices.Contains(doc.Scope, "code:acme/api:engine/client") {
		t.Errorf("Scope = %v holds the sibling the change did not touch", doc.Scope)
	}
}

func TestBuildWithARevisionThatCarriesNoEditTime(t *testing.T) {
	root, _ := pullRequest()
	root.Payload.Revision.EditedAt = time.Time{}

	doc, err := l1.Build(l1.Input{Root: root, Repo: testRepo})
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}
	if !doc.Time.Updated.Equal(doc.Time.Created) {
		t.Errorf("Time.Updated = %s, want the artifact's own time %s", doc.Time.Updated, doc.Time.Created)
	}
	if !doc.Time.LastActivity.Equal(doc.Time.Created) {
		t.Errorf("Time.LastActivity = %s, want the artifact's own time %s", doc.Time.LastActivity, doc.Time.Created)
	}
}

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

func TestBuildFoldsTheAccessListOfEveryEventItQuotes(t *testing.T) {
	resolver := testPrincipals(t)
	root, children := pullRequest()
	children[2].ACL = connector.ACL{{Kind: connector.ACLPublic}, {Kind: connector.ACLGroup, Source: source, NativeID: repo}}
	root.ACL = connector.ACL{{Kind: connector.ACLGroup, Source: source, NativeID: repo, Label: "collaborators"}}

	doc, err := l1.Build(l1.Input{Root: root, Children: children[2:3], Resolver: resolver, Repo: testRepo})
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}
	if len(doc.ACL) != 1 || doc.ACL[0].Kind != connector.ACLGroup || doc.ACL[0].NativeID != repo {
		t.Errorf("ACL = %v, want only the grant both events carry", doc.ACL)
	}
	if doc.ACL[0].Label != "collaborators" {
		t.Errorf("ACL[0].Label = %q, want the artifact's own", doc.ACL[0].Label)
	}
}

func TestBuildLeavesOutAReplyReadableByADifferentGroup(t *testing.T) {
	root, children := pullRequest()
	root.ACL = connector.ACL{{Kind: connector.ACLGroup, Source: source, NativeID: repo}}
	elsewhere := children[0]
	elsewhere.ACL = connector.ACL{{Kind: connector.ACLGroup, Source: source, NativeID: "acme/other"}}

	doc, err := l1.Build(l1.Input{Root: root, Children: []connector.Event{elsewhere}, Resolver: testPrincipals(t), Repo: testRepo})
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}
	if len(doc.L0Refs) != 1 {
		t.Errorf("L0Refs = %v, want only the artifact: the reply is readable by a different group", doc.L0Refs)
	}
	if len(doc.ACL) != 1 || doc.ACL[0].NativeID != repo {
		t.Errorf("ACL = %v, want the artifact's own group", doc.ACL)
	}
}

func TestBuildLeavesOutAReplyWhoseIdentityIsNamedLikeTheGroup(t *testing.T) {
	root, children := pullRequest()
	root.ACL = connector.ACL{{Kind: connector.ACLGroup, Source: source, NativeID: repo}}
	person := children[0]
	person.ACL = connector.ACL{{Kind: connector.ACLIdentity, Source: source, NativeID: repo}}

	doc, err := l1.Build(l1.Input{Root: root, Children: []connector.Event{person}, Resolver: testPrincipals(t), Repo: testRepo})
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}
	if len(doc.L0Refs) != 1 {
		t.Errorf("L0Refs = %v, want only the artifact: a group and an identity are not one grant", doc.L0Refs)
	}
}

func TestBuildQuotesAReplyThatCarriesTheSameGrantUnderAnotherLabel(t *testing.T) {
	root, children := pullRequest()
	root.ACL = connector.ACL{{Kind: connector.ACLGroup, Source: source, NativeID: repo}}
	same := children[0]
	same.ACL = connector.ACL{{Kind: connector.ACLGroup, Source: source, NativeID: repo, Label: "the api team"}}

	doc, err := l1.Build(l1.Input{Root: root, Children: []connector.Event{same}, Resolver: testPrincipals(t), Repo: testRepo})
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}
	if len(doc.L0Refs) != 2 {
		t.Errorf("L0Refs = %v, want the reply as well: it carries the same grant", doc.L0Refs)
	}
}

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

func TestBuildLeavesOutCommandsAndReplies(t *testing.T) {
	resolver := testPrincipals(t)
	root, children := pullRequest()
	command := reply(event(connector.KindCommand, repo+"#31:comment:90", at(7), who("u2", "samr"),
		"", "/hearsay ratify"), repo+"#31")
	answer := reply(event("github.reply", repo+"#31:comment:91", at(8), agent("b1", "shed-bot"),
		"", "Ratified 2 stances."), repo+"#31")
	answer.Payload.BaseKind = connector.KindCommand

	want, err := l1.Build(l1.Input{Root: root, Children: children, Resolver: resolver, Repo: testRepo})
	if err != nil {
		t.Fatalf("Build() = %v", err)
	}
	got, err := l1.Build(l1.Input{Root: root, Children: append(slices.Clone(children), command, answer), Resolver: resolver, Repo: testRepo})
	if err != nil {
		t.Fatalf("Build() with a command and a reply = %v", err)
	}
	if !slices.Equal(got.L0Refs, want.L0Refs) || got.RawText != want.RawText || len(got.Participants) != len(want.Participants) || !got.Time.LastActivity.Equal(want.Time.LastActivity) {
		t.Errorf("Build() with a command and a reply = %+v, want the document without them %+v", got, want)
	}
	for _, text := range []string{"/hearsay", "Ratified"} {
		if strings.Contains(got.RawText, text) {
			t.Errorf("RawText quotes %q:\n%s", text, got.RawText)
		}
	}
}

func TestBuildRefusesAnArtifactThatIsPartOfAnotherDocument(t *testing.T) {
	_, children := pullRequest()
	_, err := l1.Build(l1.Input{Root: children[0], Repo: testRepo})
	if !errors.Is(err, l1.ErrNotDistilled) {
		t.Fatalf("Build(a comment) = %v, want l1.ErrNotDistilled", err)
	}
}

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

func TestBuildRefusesAChildOfAnotherConversation(t *testing.T) {
	root, children := pullRequest()
	stray := reply(children[0], repo+"#99")
	if _, err := l1.Build(l1.Input{Root: root, Children: []connector.Event{stray}, Repo: testRepo}); err == nil {
		t.Fatal("Build() with a child of another conversation = nil, want an error")
	}
}

func TestBuildScrubsCredentialsAndAddressesFromTheConversation(t *testing.T) {
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
}

func TestBuildScrubsTheCredentialsOfALinkFromTheTextAndTheReference(t *testing.T) {
	credentialed, _ := pullRequest()
	credentialed.Payload.Text = "status is at https://deploy:hunter2@internal.example.com/status?token=abcd1234"
	doc, err := l1.Build(l1.Input{Root: credentialed, Repo: testRepo})
	if err != nil {
		t.Fatalf("Build(a link with credentials) = %v", err)
	}
	for _, secret := range []string{"hunter2", "abcd1234"} {
		for _, ref := range doc.References {
			if strings.Contains(ref.ID, secret) {
				t.Errorf("the %s reference %q holds %q", ref.Type, ref.ID, secret)
			}
		}
		if strings.Contains(doc.RawText, secret) {
			t.Errorf("RawText holds %q:\n%s", secret, doc.RawText)
		}
	}
	want := l1.Reference{Type: l1.RefURL, ID: "https://internal.example.com/status?token=[redacted:secret]"}
	if !slices.Contains(doc.References, want) {
		t.Errorf("References = %v, want it to carry %v", doc.References, want)
	}
}

func TestWithBodyScrubsTheModelsWords(t *testing.T) {
	root, _ := pullRequest()
	doc, err := l1.Build(l1.Input{Root: root, Repo: testRepo})
	if err != nil {
		t.Fatalf("Build() = %v", err)
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
	if len(doc.Body.OpenQuestions) != 1 {
		t.Errorf("OpenQuestions = %v, want the blank one dropped", doc.Body.OpenQuestions)
	}
	if err := doc.Validate(); err != nil {
		t.Errorf("Validate() = %v, want no error", err)
	}
}

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
