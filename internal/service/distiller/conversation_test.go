package distiller_test

import (
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/conversation"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/service/distiller"
)

func segmentOf() l1.Document {
	return l1.Document{
		ID: "l1:s:conversation-segment:1:c:abc", Kind: l1.KindConversationSegment,
		RawText: "Owner: Which cache?\nKern: Use Redis, I decided.\nOwner (interrupted): Fine, use Redis, that is",
	}
}

func TestJevJudgesTheOwnersWordsAndSeesKernAsContext(t *testing.T) {
	req := distiller.OutcomeRequest(segmentOf(), llm.JudgmentConfig{})
	owners, context, found := strings.Cut(req.State, "Context from Kern:")
	if !found {
		t.Fatalf("the state has no context section: %q", req.State)
	}
	if strings.Contains(owners, "Kern") || strings.Contains(owners, "Redis, I decided") {
		t.Errorf("Kern's words are among the owner's: %q", owners)
	}
	for _, want := range []string{"1. Owner: Which cache?", "3. Owner (interrupted): Fine, use Redis, that is"} {
		if !strings.Contains(owners, want) {
			t.Errorf("the owner's section lacks %q: %q", want, owners)
		}
	}
	if !strings.Contains(context, "2. Kern: Use Redis, I decided.") || strings.Contains(context, "Owner") {
		t.Errorf("the context section = %q, want Kern's line alone", context)
	}
}

func TestEveryOutcomeQuestionAboutAConversationNamesWhoseWordsCount(t *testing.T) {
	req := distiller.OutcomeRequest(segmentOf(), llm.JudgmentConfig{})
	for _, q := range req.Questions {
		if !strings.Contains(q.Question, "only what the Owner's own lines say") {
			t.Errorf("question %s does not tell Jev whose words count: %q", q.ID, q.Question)
		}
	}
}

func TestAMeetingSegmentIsNotJudgedAsAConversation(t *testing.T) {
	meeting := distiller.OutcomeRequest(l1.Document{Kind: l1.KindMeetingSegment, RawText: "Kern: said so"}, llm.JudgmentConfig{})
	if strings.Contains(meeting.State, "Owner's words") || strings.Contains(meeting.Questions[0].Question, "Owner") {
		t.Errorf("a meeting segment is judged as a conversation: %+v", meeting)
	}
}

func TestAKernOnlySegmentHasNoOwnerWords(t *testing.T) {
	doc := l1.Document{Kind: l1.KindConversationSegment, RawText: "Kern: I decided on Redis."}
	req := distiller.OutcomeRequest(doc, llm.JudgmentConfig{})
	if !strings.HasPrefix(req.State, "Owner's words:\n(none)") {
		t.Errorf("state = %q", req.State)
	}
}

func TestTheConversationPromptNumbersItsLines(t *testing.T) {
	req := distiller.RequestFor(segmentOf(), 0)
	if want := "1. Owner: Which cache?\n2. Kern: Use Redis, I decided.\n3. Owner (interrupted)"; !strings.HasPrefix(req.Messages[0].Text, want) {
		t.Errorf("prompt = %q", req.Messages[0].Text)
	}
}

func TestTheConversationRequestAsksForOutcomeLinesAndKeepsKernAsContext(t *testing.T) {
	req := distiller.RequestFor(segmentOf(), 0)
	if !strings.Contains(string(req.Schema.Definition), "outcome_lines") {
		t.Errorf("schema = %s", req.Schema.Definition)
	}
	if !strings.Contains(req.System, "Kern's lines are context, never the source of an outcome") {
		t.Errorf("system prompt = %q", req.System)
	}
}

func TestL1ReadsTheKindTheConversationSourceWrites(t *testing.T) {
	if l1.KindConversationMessage != conversation.Kind {
		t.Fatalf("l1 reads %q, the source writes %q", l1.KindConversationMessage, conversation.Kind)
	}
}

func TestAConversationMessageTakesTheConversationPathAndNoChatPath(t *testing.T) {
	ev := connector.Event{Kind: conversation.Kind, Payload: connector.Payload{BaseKind: connector.KindMessage, Container: connector.Container{Kind: conversation.ContainerStream}}}
	if _, ok := l1.KindFor(ev); ok || l1.ChatRoot(ev) {
		t.Error("the chat paths take a conversation message")
	}
	if !l1.IsConversationMessage(ev) {
		t.Error("a conversation message is not recognised")
	}
}
