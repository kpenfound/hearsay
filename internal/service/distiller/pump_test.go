package distiller_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/service/distiller"
)

// hidden is L0 behind its tombstones, for TargetOf: the events a tombstone
// covers, by artifact.
type hidden map[string]connector.Event

func (h hidden) Retracted(_ context.Context, _, artifact string) (connector.Event, error) {
	ev, ok := h[artifact]
	if !ok {
		return connector.Event{}, fmt.Errorf("%w: %s", l0.ErrNotFound, artifact)
	}
	return ev, nil
}

// hiddenOf is every fixture event as though a tombstone covered it.
func hiddenOf(events []connector.Event) hidden {
	h := hidden{}
	for _, ev := range events {
		h[ev.Payload.Artifact] = ev
	}
	return h
}

// unreadable is an L0 whose read past a tombstone fails.
type unreadable struct{}

var errUnreadable = errors.New("the database went away")

func (unreadable) Retracted(context.Context, string, string) (connector.Event, error) {
	return connector.Event{}, errUnreadable
}

// tombstoneFor is a tombstone shaped the way docs/connector-contract.md's
// GitHub row specifies a deletion: its own artifact, a target, and nothing that
// says which conversation the target was part of.
func tombstoneFor(target string) connector.Event {
	ev := event(source, connector.KindTombstone, target+":tombstone", at(9), nil, "", "")
	ev.Payload.Target = target
	return ev
}

// agentEvent is an agent session event in a channel, which is where a chat
// message would be routed to a window, and hanging off a thread where one is
// given: everything that would give an event a document except its kind.
func agentEvent(kind connector.Kind, thread string) connector.Event {
	ev := event(source, kind, "session-1:turn:1", at(1), &connector.Identity{Source: source, Kind: connector.IdentityAgent, NativeID: "shed"}, "", "a turn")
	ev.Payload.Container = connector.Container{Kind: connector.ContainerChannel, NativeID: "C1"}
	ev.Payload.Thread = thread
	return ev
}

// Which document an event belongs to is the whole of what the pump decides, and
// it is what makes a change proposal with its reviews one document rather than
// five.
func TestTargetOf(t *testing.T) {
	fixture := hiddenOf(fixtureEvents(source))
	chat := event(source, connector.KindMessage, "m1", at(1), who(source, "u1", "kpenfound"), "", "hello")
	chat.Payload.Container = connector.Container{Kind: connector.ContainerChannel, NativeID: "C1"}
	fixture["m1"] = chat
	chatTarget := l1.DocID(source, l1.ChatWindowKey(chat))
	transcript := event(source, connector.KindTranscript, "meeting-1", at(0), nil, "Meeting", "Discussion.")
	fixture["meeting-1"] = transcript
	tests := []struct {
		name   string
		ev     connector.Event
		hidden distiller.Hidden
		want   string
	}{{
		name: "a transcript targets its derived set",
		ev:   transcript,
		want: l1.DocID(source, "meeting-1"),
	}, {
		name:   "a transcript tombstone targets its derived set",
		ev:     tombstoneFor("meeting-1"),
		hidden: fixture,
		want:   l1.DocID(source, "meeting-1"),
	}, {
		name: "a channel message targets its fixed window",
		ev:   chat,
		want: chatTarget,
	}, {
		name: "a chat extension message targets the same window",
		ev: func() connector.Event {
			ev := chat
			ev.Kind = "discord.message"
			ev.Payload.BaseKind = connector.KindMessage
			return ev
		}(),
		want: chatTarget,
	}, {
		name: "a parent-only reply remains in the channel window",
		ev: func() connector.Event {
			ev := chat
			ev.Payload.Artifact = "m2"
			ev.NativeID = "m2"
			ev.Payload.Parent = "m1"
			return ev
		}(),
		want: chatTarget,
	}, {
		name: "a native thread targets its own artifact",
		ev: func() connector.Event {
			ev := chat
			ev.Kind = connector.KindThread
			ev.Payload.Artifact = "th1"
			ev.NativeID = "th1"
			return ev
		}(),
		want: l1.DocID(source, "th1"),
	}, {
		name: "a reply in a native thread targets that thread",
		ev:   func() connector.Event { ev := chat; ev.Payload.Thread = "th1"; return ev }(),
		want: l1.DocID(source, "th1"),
	}, {
		name: "a channel tombstone re-derives the window behind it",
		ev: func() connector.Event {
			ev := tombstoneFor("m1")
			ev.Payload.Container = chat.Payload.Container
			return ev
		}(),
		hidden: fixture,
		want:   chatTarget,
	}, {
		name: "an artifact that makes a document is its own target",
		ev:   event(source, connector.KindIssue, repo+"#12", at(0), who(source, "u1", "kpenfound"), "an issue", "text"),
		want: issueID,
	}, {
		name: "a revision of one is the same target",
		ev:   revised(event(source, connector.KindIssue, repo+"#12", at(0), who(source, "u1", "kpenfound"), "an issue", "text"), "t2", at(1)),
		want: issueID,
	}, {
		name: "a comment belongs to the conversation it is on",
		ev:   reply(event(source, connector.KindMessage, repo+"#12:comment:1", at(1), who(source, "u2", "samr"), "", "text"), repo+"#12"),
		want: issueID,
	}, {
		name: "a review belongs to the change proposal",
		ev:   reply(event(source, connector.KindReview, repo+"#31:review:1", at(1), who(source, "u2", "samr"), "", "text"), repo+"#31"),
		want: prID,
	}, {
		name: "a source with no threads gives a parent, and that is the conversation",
		ev: func() connector.Event {
			ev := event(source, connector.KindMessage, repo+"#31:comment:1", at(1), who(source, "u2", "samr"), "", "text")
			ev.Payload.Parent = repo + "#31"
			return ev
		}(),
		want: prID,
	}, {
		name: "a commit is its own document",
		ev:   event(source, connector.KindCommit, commit, at(0), who(source, "u1", "kpenfound"), "a commit", "text"),
		want: commitID,
	}, {
		name: "a tombstone that names the conversation is taken at its word, without reading L0",
		ev: func() connector.Event {
			ev := tombstoneFor(repo + "#31:comment:1")
			ev.Payload.Parent = repo + "#31"
			ev.Payload.Thread = repo + "#31"
			return ev
		}(),
		hidden: unreadable{},
		want:   prID,
	}, {
		name:   "a tombstone whose only conversation information is its target re-derives the issue the comment was on",
		ev:     tombstoneFor(repo + "#12:comment:1"),
		hidden: fixture,
		want:   issueID,
	}, {
		name:   "a tombstone for a review comment re-derives the change proposal",
		ev:     tombstoneFor(repo + "#31:comment:88"),
		hidden: fixture,
		want:   prID,
	}, {
		name:   "a tombstone for an artifact that makes a document re-derives that document",
		ev:     tombstoneFor(repo + "#31"),
		hidden: fixture,
		want:   prID,
	}, {
		name:   "a tombstone for an artifact L0 holds nothing of has no target",
		ev:     tombstoneFor(repo + "#99:comment:1"),
		hidden: fixture,
		want:   "",
	}, {
		name: "a tombstone for an artifact that belonged to no document has no target",
		ev:   tombstoneFor(repo + "#31:reaction:1"),
		hidden: hidden{repo + "#31:reaction:1": event(source, connector.KindReaction, repo+"#31:reaction:1",
			at(1), who(source, "u2", "samr"), "", "")},
		want: "",
	}, {
		name: "a document routes to its source artifact for section distillation",
		ev:   event(source, connector.KindDocument, "file-1", at(0), who(source, "u1", "kpenfound"), "Design", "# Decision\nText"),
		want: l1.DocID(source, "file-1"),
	}, {
		name: "an extension kind is read through its base kind",
		ev: func() connector.Event {
			ev := event(source, "jira.story", "ENG-7", at(0), who(source, "u1", "kpenfound"), "a story", "text")
			ev.Payload.BaseKind = connector.KindIssue
			return ev
		}(),
		want: l1.DocID(source, "ENG-7"),
	}, {
		name: "a tombstone of an extension kind is read through its base kind",
		ev: func() connector.Event {
			ev := tombstoneFor(repo + "#12:comment:2")
			ev.Kind = "jira.deletion"
			ev.Payload.BaseKind = connector.KindTombstone
			return ev
		}(),
		hidden: fixture,
		want:   issueID,
	}, {
		name: "an agent session event is not distilled",
		ev:   agentEvent(connector.KindAgentSession, ""),
		want: "",
	}, {
		name: "an agent turn is not distilled, even naming a conversation",
		ev:   agentEvent(connector.KindAgentTurn, "session-1"),
		want: "",
	}, {
		name: "a tool call is not distilled, even naming a conversation",
		ev:   agentEvent(connector.KindToolCall, "session-1"),
		want: "",
	}, {
		name: "an extension kind based on an agent turn is not distilled",
		ev: func() connector.Event {
			ev := agentEvent("shed.turn", "session-1")
			ev.Payload.BaseKind = connector.KindAgentTurn
			return ev
		}(),
		want: "",
	}, {
		name:   "a tombstone for an agent turn has no target",
		ev:     tombstoneFor("session-1:turn:1"),
		hidden: hidden{"session-1:turn:1": agentEvent(connector.KindAgentTurn, "session-1")},
		want:   "",
	}, {
		name: "an event that belongs to nothing has no target",
		ev:   event(source, connector.KindReaction, repo+"#31:reaction:1", at(1), who(source, "u2", "samr"), "", ""),
		want: "",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := tt.hidden
			if h == nil {
				// Only a tombstone reads past one: anything else that reads L0
				// here fails.
				h = unreadable{}
			}
			got, ok, err := distiller.TargetOf(t.Context(), tt.ev, h)
			if err != nil {
				t.Fatalf("TargetOf() = %v", err)
			}
			if tt.want == "" {
				if ok {
					t.Fatalf("TargetOf() = %q, want no target", got)
				}
				return
			}
			if !ok {
				t.Fatalf("TargetOf() = no target, want %q", tt.want)
			}
			if got != tt.want {
				t.Errorf("TargetOf() = %q, want %q", got, tt.want)
			}
		})
	}
}

// A read past a tombstone that fails is the pump's failure, not a tombstone that
// belongs to nothing: the batch is read again rather than its cursor moving past
// a deletion nothing re-derived.
func TestTargetOfReportsAFailedReadPastATombstone(t *testing.T) {
	_, ok, err := distiller.TargetOf(t.Context(), tombstoneFor(repo+"#12:comment:1"), unreadable{})
	if !errors.Is(err, errUnreadable) {
		t.Errorf("TargetOf() error = %v, want the read's own error", err)
	}
	if ok {
		t.Error("TargetOf() reported a target alongside an error")
	}
}

// The batch size is held to what one read of the feed returns. The drain loop
// reads a full batch as "there is more", so a batch above the store's cap could
// never be reached and a backfill would wait an interval between batches.
func TestThePumpBatchIsHeldToWhatOneReadReturns(t *testing.T) {
	tests := []struct {
		asked int
		want  int
	}{
		{asked: 0, want: distiller.DefaultPumpBatch},
		{asked: -1, want: distiller.DefaultPumpBatch},
		{asked: 10, want: 10},
		{asked: l0.MaxLimit, want: l0.MaxLimit},
		{asked: l0.MaxLimit + 1, want: l0.MaxLimit},
		{asked: 1_000_000, want: l0.MaxLimit},
	}
	for _, tt := range tests {
		if got := distiller.NewPump(nil, distiller.PumpOptions{Batch: tt.asked}).Batch(); got != tt.want {
			t.Errorf("a pump asked for a batch of %d uses %d, want %d", tt.asked, got, tt.want)
		}
	}
}

// A distill job's target is a document id, so the id the pump writes has to be
// one the handler can read back.
func TestEveryTargetParsesBackToItsArtifact(t *testing.T) {
	events := fixtureEvents(source)
	for _, ev := range events {
		target, ok, err := distiller.TargetOf(t.Context(), ev, hiddenOf(events))
		if err != nil || !ok {
			t.Errorf("TargetOf(%s) = %v, no target, and every fixture event belongs to a document", ev.NativeID, err)
			continue
		}
		src, artifact, err := l1.ParseDocID(target)
		if err != nil {
			t.Errorf("ParseDocID(%q) = %v", target, err)
			continue
		}
		if src != ev.Source {
			t.Errorf("TargetOf(%s) names source %q, want %q", ev.NativeID, src, ev.Source)
		}
		if artifact == "" {
			t.Errorf("TargetOf(%s) names no artifact", ev.NativeID)
		}
	}
}
