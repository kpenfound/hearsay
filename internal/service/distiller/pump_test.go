package distiller_test

import (
	"testing"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/service/distiller"
)

// Which document an event belongs to is the whole of what the pump decides, and
// it is what makes a change proposal with its reviews one document rather than
// five.
func TestTargetOf(t *testing.T) {
	tests := []struct {
		name string
		ev   connector.Event
		want string
	}{{
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
		name: "a tombstone re-derives the conversation the retracted artifact was in",
		ev: func() connector.Event {
			ev := event(source, connector.KindTombstone, repo+"#31:comment:1:tombstone", at(2), nil, "", "")
			ev.Payload.Target = repo + "#31:comment:1"
			ev.Payload.Parent = repo + "#31"
			ev.Payload.Thread = repo + "#31"
			return ev
		}(),
		want: prID,
	}, {
		name: "a tombstone that says nothing else re-derives the artifact's own document",
		ev: func() connector.Event {
			ev := event(source, connector.KindTombstone, repo+"#31:tombstone", at(2), nil, "", "")
			ev.Payload.Target = repo + "#31"
			return ev
		}(),
		want: prID,
	}, {
		name: "an extension kind is read through its base kind",
		ev: func() connector.Event {
			ev := event(source, "jira.story", "ENG-7", at(0), who(source, "u1", "kpenfound"), "a story", "text")
			ev.Payload.BaseKind = connector.KindIssue
			return ev
		}(),
		want: l1.DocID(source, "ENG-7"),
	}, {
		name: "an event that belongs to nothing has no target",
		ev:   event(source, connector.KindReaction, repo+"#31:reaction:1", at(1), who(source, "u2", "samr"), "", ""),
		want: "",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := distiller.TargetOf(tt.ev)
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
	for _, ev := range fixtureEvents(source) {
		target, ok := distiller.TargetOf(ev)
		if !ok {
			t.Errorf("TargetOf(%s) has no target, and every fixture event belongs to a document", ev.NativeID)
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
