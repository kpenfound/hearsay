package l1_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
)

// TestBuildChatReplies: on a two-level chat source a reply's thread is the
// message it answers, and that message heads a conversation of its own.
func TestBuildChatReplies(t *testing.T) {
	channel := func(ev connector.Event) connector.Event {
		ev.Payload.Container = connector.Container{Kind: connector.ContainerChannel, NativeID: "C1"}
		return ev
	}
	root := channel(event(connector.KindMessage, "C1/1.000001", at(0), who("u1", "kpenfound"), "", "Should we ship the retry change?"))
	answer := channel(event(connector.KindMessage, "C1/2.000002", at(1), who("u2", "samr"), "", "Yes, once the tests pass."))
	answer.Payload.Parent, answer.Payload.Thread = root.Payload.Artifact, root.Payload.Artifact
	inline := channel(event(connector.KindMessage, "C1/3.000003", at(2), who("u2", "samr"), "", "A reply that stays in the channel."))
	inline.Payload.Parent = root.Payload.Artifact
	reaction := channel(event(connector.KindReaction, "C1/1.000001:reaction:u2:%2B1", at(0), who("u2", "samr"), "", ""))
	reaction.Payload.Parent = root.Payload.Artifact
	command := channel(event(connector.KindCommand, "C1/4.000004", at(3), who("u1", "kpenfound"), "", "/hearsay pin"))
	command.Payload.Parent, command.Payload.Thread = root.Payload.Artifact, root.Payload.Artifact
	threaded := root
	threaded.Payload.Thread = "thread:C9"
	elsewhere := root
	elsewhere.Payload.Container = connector.Container{Kind: connector.ContainerRepository, NativeID: repo}

	tests := []struct {
		name   string
		root   connector.Event
		events []connector.Event
		want   []connector.Event // the replies kept; nil is no document
	}{
		{"a message and its replies", root, []connector.Event{inline, answer, reaction}, []connector.Event{answer}},
		{"a message nobody answered", root, nil, nil},
		{"answers that stay in the channel", root, []connector.Event{inline, reaction}, nil},
		{"only a command", root, []connector.Event{command}, nil},
		{"a message inside a thread", threaded, []connector.Event{answer}, nil},
		{"a comment on an issue", elsewhere, []connector.Event{answer}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc, replies, err := l1.BuildChatReplies(tt.root, tt.events, testPrincipals(t), testRepo)
			if tt.want == nil {
				if !errors.Is(err, l1.ErrNotDistilled) {
					t.Errorf("BuildChatReplies() = %+v, %v; want ErrNotDistilled", doc, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			wantRefs := []string{connector.EventID(source, root.NativeID)}
			for _, ev := range tt.want {
				wantRefs = append(wantRefs, connector.EventID(source, ev.NativeID))
			}
			if doc.Kind != l1.KindChatThread || doc.ID != l1.DocID(source, root.Payload.Artifact) || doc.ArtifactClass != "chat_thread" || !slices.Equal(doc.L0Refs, wantRefs) {
				t.Errorf("document = %s %s %s %v, want a chat_thread keyed by the root over %v", doc.Kind, doc.ID, doc.ArtifactClass, doc.L0Refs, wantRefs)
			}
			if len(replies) != len(tt.want) || replies[0].NativeID != tt.want[0].NativeID {
				t.Errorf("replies = %v, want %v", replies, tt.want)
			}
		})
	}
}
