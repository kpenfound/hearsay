package l1_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
)

func meetingRoot() connector.Event {
	root := event(connector.KindTranscript, "file-1", at(0), nil, "Planning meeting", "Intro.\nAPI retry policy uses engine and acme/api#31.\nDecision: cap retries.\nQueue dashboard is proposed in https://github.com/acme/api/pull/42.\nDecision is pending.")
	root.Payload.Participants = []connector.Participant{{Identity: *who("u1", "kpenfound"), Role: connector.RoleAttendee}, {Identity: *who("u2", "samr"), Role: connector.RoleAttendee}}
	root.ACL = connector.ACL{{Kind: connector.ACLGroup, Source: source, NativeID: "team"}}
	return root
}

var meetingSpans = []l1.MeetingSpan{{Topic: "Retry policy", Start: 2, End: 3}, {Topic: "Queue dashboard", Start: 4, End: 5}}

func buildMeeting(t *testing.T, root connector.Event) []l1.Document {
	t.Helper()
	docs, err := l1.BuildMeetingSegments(root, meetingSpans, testPrincipals(t), testRepo)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 || docs[0].ID == docs[1].ID {
		t.Fatalf("segments = %+v", docs)
	}
	return docs
}

func TestBuildMeetingSegmentsInheritTheTranscriptsProvenanceAndAccess(t *testing.T) {
	root := meetingRoot()
	for _, d := range buildMeeting(t, root) {
		if d.Kind != l1.KindMeetingSegment || !slices.Equal(d.L0Refs, []string{connector.EventID(source, root.NativeID)}) || !d.ACL.Equal(root.ACL) || len(d.Participants) != 2 || d.Participants[0].Role != connector.RoleAttendee {
			t.Errorf("envelope = %+v", d)
		}
	}
}

func TestBuildMeetingSegmentsKeepTopicsIndependent(t *testing.T) {
	docs := buildMeeting(t, meetingRoot())
	if !slices.Contains(docs[0].References, l1.Reference{Type: l1.RefTrackerItem, ID: "acme/api#31"}) || slices.Contains(docs[1].References, l1.Reference{Type: l1.RefTrackerItem, ID: "acme/api#31"}) {
		t.Errorf("tracker refs crossed topics: %+v", docs)
	}
	if !slices.Contains(docs[1].References, l1.Reference{Type: l1.RefPR, ID: "acme/api#42"}) || slices.Contains(docs[0].References, l1.Reference{Type: l1.RefPR, ID: "acme/api#42"}) {
		t.Errorf("PR refs crossed topics: %+v", docs)
	}
}

func TestBuildMeetingSegmentsKeepTheirIdentityWhenTheTextIsEdited(t *testing.T) {
	root := meetingRoot()
	docs := buildMeeting(t, root)
	changed := root
	changed.Payload.Text = strings.Replace(root.Payload.Text, "cap retries", "limit retries", 1)
	again := buildMeeting(t, changed)
	if docs[0].ID != again[0].ID || docs[1].ID != again[1].ID || docs[0].RawText == again[0].RawText {
		t.Errorf("edit identity = %+v", again)
	}
}

func TestReturningToATopicKeepsOneSegment(t *testing.T) {
	root := event(connector.KindTranscript, "meeting-1", at(0), nil, "Meeting", "Retry: proposal.\nDashboard: open.\nRetry: decision.")
	spans := []l1.MeetingSpan{{Topic: "Retry", Start: 1, End: 1}, {Topic: "Dashboard", Start: 2, End: 2}, {Topic: " retry ", Start: 3, End: 3}}
	docs, err := l1.BuildMeetingSegments(root, spans, nil, testRepo)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 || docs[0].RawText != "Retry: proposal.\nRetry: decision." || docs[1].RawText != "Dashboard: open." {
		t.Errorf("return to topic = %+v", docs)
	}
}

func TestBuildMeetingSegmentsRejectsMalformedBoundaries(t *testing.T) {
	root := event(connector.KindTranscript, "file-1", at(0), nil, "Meeting", "One.\nTwo.")
	for _, spans := range [][]l1.MeetingSpan{{{Topic: "", Start: 1, End: 1}}, {{Topic: "A", Start: 0, End: 1}}, {{Topic: "A", Start: 1, End: 3}}, {{Topic: "A", Start: 1, End: 2}, {Topic: "B", Start: 2, End: 2}}} {
		if _, err := l1.BuildMeetingSegments(root, spans, nil, testRepo); !errors.Is(err, l1.ErrInvalidDocument) {
			t.Errorf("%+v: %v", spans, err)
		}
	}
}

func TestBuildMeetingSegmentsWithNoSpansMakesNoDocuments(t *testing.T) {
	root := event(connector.KindTranscript, "file-1", at(0), nil, "Meeting", "One.\nTwo.")
	if docs, err := l1.BuildMeetingSegments(root, nil, nil, testRepo); err != nil || len(docs) != 0 {
		t.Errorf("empty = %+v, %v", docs, err)
	}
}
