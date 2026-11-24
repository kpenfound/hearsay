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

// segmentOf is a conversation segment of three lines: the owner's question,
// Kern's answer and the owner's decision.
func segmentOf(t *testing.T) l1.Document {
	t.Helper()
	doc := l1.Document{
		ID: "l1:s:conversation-segment:1:c:abc", Kind: l1.KindConversationSegment,
		RawText: "Owner: Which cache?\nKern: Use Redis, I decided.\nOwner (interrupted): Fine, use Redis, that is",
	}
	return doc
}

// The outcome sentence of a conversation segment survives only when it cites
// Owner lines and nothing else.
func TestAnOutcomeStandsOnlyOnTheOwnersLines(t *testing.T) {
	doc := segmentOf(t)
	for _, tc := range []struct {
		name  string
		cited []int
		keep  bool
	}{
		{"an owner line", []int{1}, true},
		{"an interrupted owner line", []int{3}, true},
		{"several owner lines", []int{1, 3}, true},
		{"a Kern line", []int{2}, false},
		{"an owner line and a Kern line", []int{1, 2}, false},
		{"no line", nil, false},
		{"a line the segment does not have", []int{4}, false},
		{"line zero", []int{0}, false},
		{"a negative line", []int{-1}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := distiller.OwnerOutcome(doc, "A summary of both.", "The cache is Redis.", tc.cited)
			if kept := body.Outcome != ""; kept != tc.keep {
				t.Errorf("outcome %q with lines %v: kept = %v, want %v", body.Outcome, tc.cited, kept, tc.keep)
			}
			if body.Summary != "A summary of both." {
				t.Errorf("the summary describes the whole topic and is never cut: %q", body.Summary)
			}
		})
	}
}

// What Jev judges is the owner's lines as the evidence, with Kern's apart as
// context, and the question says so.
func TestJevJudgesTheOwnersWordsAndSeesKernAsContext(t *testing.T) {
	doc := segmentOf(t)
	req := distiller.OutcomeRequest(doc, llm.JudgmentConfig{})
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
	for _, q := range req.Questions {
		if !strings.Contains(q.Question, "only what the Owner's own lines say") {
			t.Errorf("question %s does not tell Jev whose words count: %q", q.ID, q.Question)
		}
	}

	// Another kind's judgments are asked as they always were.
	meeting := distiller.OutcomeRequest(l1.Document{Kind: l1.KindMeetingSegment, RawText: "Kern: said so"}, llm.JudgmentConfig{})
	if strings.Contains(meeting.State, "Owner's words") || strings.Contains(meeting.Questions[0].Question, "Owner") {
		t.Errorf("a meeting segment is judged as a conversation: %+v", meeting)
	}
}

// A segment with no owner line has nothing for Jev to judge.
func TestAKernOnlySegmentHasNoOwnerWords(t *testing.T) {
	doc := l1.Document{Kind: l1.KindConversationSegment, RawText: "Kern: I decided on Redis."}
	req := distiller.OutcomeRequest(doc, llm.JudgmentConfig{})
	if !strings.HasPrefix(req.State, "Owner's words:\n(none)") {
		t.Errorf("state = %q", req.State)
	}
	if body := distiller.OwnerOutcome(doc, "s", "Redis.", []int{1}); body.Outcome != "" {
		t.Errorf("outcome = %q, want none", body.Outcome)
	}
}

// The conversation prompt numbers the lines the outcome cites, and the schema
// asks for them.
func TestTheConversationPromptNumbersItsLines(t *testing.T) {
	req := distiller.RequestFor(segmentOf(t), 0)
	if want := "1. Owner: Which cache?\n2. Kern: Use Redis, I decided.\n3. Owner (interrupted)"; !strings.HasPrefix(req.Messages[0].Text, want) {
		t.Errorf("prompt = %q", req.Messages[0].Text)
	}
	if !strings.Contains(string(req.Schema.Definition), "outcome_lines") {
		t.Errorf("schema = %s", req.Schema.Definition)
	}
	if !strings.Contains(req.System, "Kern's lines are context, never the source of an outcome") {
		t.Errorf("system prompt = %q", req.System)
	}
}

// The kind L1 reads is the kind the conversation source writes: a connector's
// output is spelled twice, so a change to one fails here.
func TestL1ReadsTheKindTheConversationSourceWrites(t *testing.T) {
	if l1.KindConversationMessage != conversation.Kind {
		t.Fatalf("l1 reads %q, the source writes %q", l1.KindConversationMessage, conversation.Kind)
	}
	ev := connector.Event{Kind: conversation.Kind, Payload: connector.Payload{BaseKind: connector.KindMessage, Container: connector.Container{Kind: conversation.ContainerStream}}}
	if _, ok := l1.KindFor(ev); ok || l1.ChatRoot(ev) {
		t.Error("the chat paths take a conversation message")
	}
	if !l1.IsConversationMessage(ev) {
		t.Error("a conversation message is not recognised")
	}
}
