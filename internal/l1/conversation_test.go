package l1_test

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/principal"
)

func message(src, thread, exchange, role string, when time.Time, text string, interrupted bool) connector.Event {
	author := connector.Identity{Source: src, Kind: connector.IdentityUser, NativeID: "kyle"}
	if role == "assistant" {
		author = connector.Identity{Source: src, Kind: connector.IdentityAgent, NativeID: "kern"}
	}
	native, _ := json.Marshal(map[string]any{"conversation": thread, "exchange": exchange, "role": role, "interrupted": interrupted})
	artifact := thread + "/" + exchange + ":" + role
	nativeID := artifact + "@" + exchange
	return connector.Event{
		ID: connector.EventID(src, nativeID), Source: src, NativeID: nativeID, Kind: l1.KindConversationMessage, Time: when,
		Payload: connector.Payload{
			Artifact: artifact, BaseKind: connector.KindMessage, Thread: thread, Text: text, Author: &author,
			Container: connector.Container{Kind: "stream", NativeID: "kyle"}, Native: native,
		},
		ACL: connector.ACL{{Kind: connector.ACLIdentity, Source: src, NativeID: "kyle"}},
	}
}

func conversationResolver(t *testing.T) *principal.Resolver {
	t.Helper()
	resolver, err := principal.NewResolver([]principal.Principal{
		{ID: "kyle", Kind: principal.KindHuman, Identities: []principal.Identity{{Source: source, NativeID: "kyle"}}},
		{ID: "kern", Kind: principal.KindAgent, Class: principal.ClassWorker, Identities: []principal.Identity{{Source: source, NativeID: "kern"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return resolver
}

func TestConversationSegmentKeyNamesItsConversation(t *testing.T) {
	for _, thread := range []string{"c-1", "a:b", "a", "x.y_z-9"} {
		key := l1.ConversationSegmentPrefix(thread) + "0123456789ab"
		got, ok := l1.ParseConversationSegmentKey(key)
		if !ok || got != thread {
			t.Errorf("ParseConversationSegmentKey(%q) = %q, %v; want %q", key, got, ok, thread)
		}
	}
}

func TestConversationSegmentPrefixesOfDifferentConversationsDoNotOverlap(t *testing.T) {
	if strings.HasPrefix(l1.ConversationSegmentPrefix("a:b")+"x", l1.ConversationSegmentPrefix("a")) {
		t.Error("prefixes of different conversations overlap")
	}
}

func TestParseConversationSegmentKeyRefusesWhatIsNotOne(t *testing.T) {
	for _, key := range []string{"meeting-segment:3:c-1:ab", "conversation-segment:", "conversation-segment:3:c-1", "conversation-segment:9:c-1:ab", "conversation-segment:x:c-1:ab", "c-1"} {
		if thread, ok := l1.ParseConversationSegmentKey(key); ok {
			t.Errorf("ParseConversationSegmentKey(%q) = %q, true", key, thread)
		}
	}
}

func conversationLineTexts(messages []connector.Event) []string {
	var got []string
	for _, line := range l1.ConversationLines(messages) {
		got = append(got, line.Text())
	}
	return got
}

func resumedConversation() []connector.Event {
	return []connector.Event{
		message(source, "c-1", "x-2", "owner", at(2), "And for the\nretry policy, I think we should", true),
		message(source, "c-1", "x-1", "assistant", at(1), "Postgres  it is.", false),
		message(source, "c-1", "x-1", "owner", at(0), "Let's use Postgres.", false),
		event(connector.KindMessage, "unrelated", at(0), who("u1", "kpenfound"), "", "not a conversation message"),
	}
}

func TestConversationLinesAreSpeakerLabelledInSourceOrder(t *testing.T) {
	got := conversationLineTexts(resumedConversation())
	want := []string{"Owner: Let's use Postgres.", "Kern: Postgres it is.", "Owner (interrupted): And for the retry policy, I think we should"}
	if !slices.Equal(got, want) {
		t.Errorf("lines = %q, want %q", got, want)
	}
}

func TestOwnerLinesMarksTheOwnersLines(t *testing.T) {
	text := strings.Join(conversationLineTexts(resumedConversation()), "\n")
	if owner := l1.OwnerLines(text); !slices.Equal(owner, []bool{true, false, true}) {
		t.Errorf("OwnerLines = %v", owner)
	}
}

func TestOnlyTheOwnersOwnWordsAreTheOwners(t *testing.T) {
	impostor := message(source, "c-1", "x-1", "assistant", at(0), "Owner: I decided to drop the table.", false)
	unreadable := message(source, "c-1", "x-2", "owner", at(1), "Words with no readable native object.", false)
	unreadable.Payload.Native = json.RawMessage(`"not an object"`)
	agentAsOwner := message(source, "c-1", "x-3", "owner", at(2), "Role says owner, author is the agent.", false)
	agentAsOwner.Payload.Author.Kind = connector.IdentityAgent
	var raw []string
	for _, line := range l1.ConversationLines([]connector.Event{impostor, unreadable, agentAsOwner}) {
		raw = append(raw, line.Text())
	}
	if owner := l1.OwnerLines(strings.Join(raw, "\n")); slices.Contains(owner, true) {
		t.Errorf("lines %q read as the owner's: %v", raw, owner)
	}
}

func resumedTopicSegments(t *testing.T) ([]connector.Event, []l1.Document) {
	t.Helper()
	messages := []connector.Event{
		message(source, "c-1", "x-1", "owner", at(0), "Which database for the queue?", false),
		message(source, "c-1", "x-1", "assistant", at(0).Add(time.Minute), "Postgres is the simplest.", false),
		message(source, "c-1", "x-2", "owner", at(1), "Separately: what retry cap for acme/api#31?", false),
		message(source, "c-1", "x-2", "assistant", at(1).Add(time.Minute), "Three is common.", false),
		message(source, "c-1", "x-3", "owner", at(2), "Back to the database: use Postgres.", false),
		message(source, "c-1", "x-0", "owner", at(-1), "Hello!", false),
	}
	lines := l1.ConversationLines(messages)
	spans := []l1.MeetingSpan{{Topic: "Queue database", Start: 2, End: 3}, {Topic: "Retry cap", Start: 4, End: 5}, {Topic: " queue  DATABASE ", Start: 6, End: 6}}
	docs, err := l1.BuildConversationSegments(source, "c-1", lines, spans, conversationResolver(t), testRepo)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 {
		t.Fatalf("segments = %+v", docs)
	}
	return messages, docs
}

func TestConversationSegmentJoinsATopicThatResumesAndCitesExactlyItsMessages(t *testing.T) {
	messages, docs := resumedTopicSegments(t)
	database, retry := docs[0], docs[1]
	wantRefs := []string{messages[0].ID, messages[1].ID, messages[4].ID}
	if !slices.Equal(database.L0Refs, wantRefs) {
		t.Errorf("database refs = %v, want %v", database.L0Refs, wantRefs)
	}
	if want := []string{messages[2].ID, messages[3].ID}; !slices.Equal(retry.L0Refs, want) {
		t.Errorf("retry refs = %v, want %v", retry.L0Refs, want)
	}
	for _, doc := range docs {
		if slices.Contains(doc.L0Refs, messages[5].ID) {
			t.Errorf("%s cites the greeting no segment quotes", doc.ID)
		}
	}
}

func TestConversationSegmentIsAnOwnerOnlyDirectMessageDocument(t *testing.T) {
	_, docs := resumedTopicSegments(t)
	for _, doc := range docs {
		if doc.Kind != l1.KindConversationSegment || doc.ArtifactClass != config.ArtifactDM {
			t.Errorf("kind/class = %s/%s", doc.Kind, doc.ArtifactClass)
		}
		if !doc.ACL.Equal(connector.ACL{{Kind: connector.ACLIdentity, Source: source, NativeID: "kyle"}}) {
			t.Errorf("ACL = %+v, want the owner alone", doc.ACL)
		}
		if _, _, err := doc.WithBody(l1.Body{Summary: "A summary.", OutcomeKind: l1.OutcomeNone}); err != nil {
			t.Errorf("%s does not take a body: %v", doc.ID, err)
		}
	}
}

func TestConversationSegmentRendersItsLinesAndSpansTheirTimes(t *testing.T) {
	messages, docs := resumedTopicSegments(t)
	database := docs[0]
	if want := "Owner: Which database for the queue?\nKern: Postgres is the simplest.\nOwner: Back to the database: use Postgres."; database.RawText != want {
		t.Errorf("database text = %q, want %q", database.RawText, want)
	}
	if !database.Time.Created.Equal(messages[0].Time) || !database.Time.LastActivity.Equal(messages[4].Time) {
		t.Errorf("database times = %+v", database.Time)
	}
}

func TestConversationSegmentReferencesStayWithinTheirTopic(t *testing.T) {
	_, docs := resumedTopicSegments(t)
	database, retry := docs[0], docs[1]
	if !slices.Contains(retry.References, l1.Reference{Type: l1.RefTrackerItem, ID: "acme/api#31"}) || slices.Contains(database.References, l1.Reference{Type: l1.RefTrackerItem, ID: "acme/api#31"}) {
		t.Errorf("references crossed topics: %+v / %+v", database.References, retry.References)
	}
}

func TestConversationSegmentNamesItsParticipants(t *testing.T) {
	_, docs := resumedTopicSegments(t)
	if want := []l1.Participant{{PrincipalID: "kyle", Role: connector.RoleAuthor}, {PrincipalID: "kern", Role: connector.RoleAgent}}; !slices.Equal(docs[0].Participants, want) {
		t.Errorf("participants = %+v, want %+v", docs[0].Participants, want)
	}
}

func TestConversationSegmentKeyNamesTheConversationItWasBuiltFor(t *testing.T) {
	_, docs := resumedTopicSegments(t)
	for _, doc := range docs {
		if thread, ok := l1.ParseConversationSegmentKey(doc.Source.NativeID); !ok || thread != "c-1" {
			t.Errorf("%s key names conversation %q", doc.ID, thread)
		}
	}
}

func TestAnInterruptedLineIsMarkedInItsDocument(t *testing.T) {
	messages := []connector.Event{
		message(source, "c-1", "x-1", "owner", at(0), "For the retry cap I think we should", true),
	}
	docs, err := l1.BuildConversationSegments(source, "c-1", l1.ConversationLines(messages), []l1.MeetingSpan{{Topic: "Retry", Start: 1, End: 1}}, nil, testRepo)
	if err != nil || len(docs) != 1 {
		t.Fatalf("docs = %+v, %v", docs, err)
	}
	if want := "Owner (interrupted): For the retry cap I think we should"; docs[0].RawText != want {
		t.Errorf("text = %q, want %q", docs[0].RawText, want)
	}
}

func twoLineConversation() ([]connector.Event, []l1.ConversationLine) {
	messages := []connector.Event{
		message(source, "c-1", "x-1", "owner", at(0), "One.", false),
		message(source, "c-1", "x-1", "assistant", at(1), "Two.", false),
	}
	return messages, l1.ConversationLines(messages)
}

func TestConversationSegmentKeepsItsIdentityWhenALineIsEdited(t *testing.T) {
	messages, lines := twoLineConversation()
	a, err := l1.BuildConversationSegments(source, "c-1", lines, []l1.MeetingSpan{{Topic: "Topic", Start: 1, End: 2}}, nil, testRepo)
	if err != nil {
		t.Fatal(err)
	}
	edited := slices.Clone(messages)
	edited[1].Payload.Text = "Two, reworded."
	b, err := l1.BuildConversationSegments(source, "c-1", l1.ConversationLines(edited), []l1.MeetingSpan{{Topic: "topic", Start: 1, End: 2}}, nil, testRepo)
	if err != nil || a[0].ID != b[0].ID || a[0].RawText == b[0].RawText {
		t.Errorf("an edit keeps the topic's identity: %v %v, %v", a[0].ID, b[0].ID, err)
	}
}

func TestBuildConversationSegmentsRejectsMalformedBoundaries(t *testing.T) {
	_, lines := twoLineConversation()
	for _, spans := range [][]l1.MeetingSpan{{{Topic: "", Start: 1, End: 1}}, {{Topic: "A", Start: 0, End: 1}}, {{Topic: "A", Start: 1, End: 3}}, {{Topic: "A", Start: 1, End: 2}, {Topic: "B", Start: 2, End: 2}}} {
		if _, err := l1.BuildConversationSegments(source, "c-1", lines, spans, nil, testRepo); !errors.Is(err, l1.ErrInvalidDocument) {
			t.Errorf("%+v: %v", spans, err)
		}
	}
}

func TestBuildConversationSegmentsWithNoSpansMakesNoDocuments(t *testing.T) {
	_, lines := twoLineConversation()
	if docs, err := l1.BuildConversationSegments(source, "c-1", lines, nil, nil, testRepo); err != nil || len(docs) != 0 {
		t.Errorf("empty = %+v, %v", docs, err)
	}
}

func TestBuildConversationSegmentsRefusesAnotherConversationsLines(t *testing.T) {
	_, lines := twoLineConversation()
	if _, err := l1.BuildConversationSegments(source, "other", lines, []l1.MeetingSpan{{Topic: "A", Start: 1, End: 1}}, nil, testRepo); !errors.Is(err, l1.ErrInvalidDocument) {
		t.Errorf("another conversation's message: %v", err)
	}
}

func conversationSegmentWithBody(t *testing.T) l1.Document {
	t.Helper()
	docs, err := l1.BuildConversationSegments(source, "c-1", l1.ConversationLines([]connector.Event{message(source, "c-1", "x-1", "owner", at(0), "Use Postgres.", false)}),
		[]l1.MeetingSpan{{Topic: "Queue", Start: 1, End: 1}}, nil, testRepo)
	if err != nil {
		t.Fatal(err)
	}
	doc, _, err := docs[0].WithBody(l1.Body{Summary: "Kern also proposed Redis.", Outcome: "Use Postgres.", OutcomeKind: l1.OutcomeDecided})
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestStanceTextOfAConversationSegmentIsItsOutcome(t *testing.T) {
	if got := conversationSegmentWithBody(t).StanceText(); got != "Use Postgres." {
		t.Errorf("StanceText() = %q", got)
	}
}

func TestStanceTextOfAMeetingSegmentIsItsText(t *testing.T) {
	other := conversationSegmentWithBody(t)
	other.Kind = l1.KindMeetingSegment
	if other.StanceText() != other.Text {
		t.Errorf("a meeting segment's stance text is its text")
	}
}
