package l1_test

import (
	"encoding/json"
	"testing"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
)

func TestClassFor(t *testing.T) {
	cases := []struct {
		name      string
		kind      l1.Kind
		native    string
		container connector.ContainerKind
		want      config.ArtifactClass
	}{
		{"merged pull request", l1.KindPR, `{"merged_at":"2026-09-09T12:00:00Z","state":"closed"}`, connector.ContainerRepository, config.ArtifactMergedPR},
		{"open pull request", l1.KindPR, `{"state":"open"}`, connector.ContainerRepository, config.ArtifactPullRequest},
		{"closed unmerged pull request", l1.KindPR, `{"state":"closed"}`, connector.ContainerRepository, config.ArtifactPullRequest},
		{"meeting", l1.KindMeetingSegment, "", connector.ContainerFolder, config.ArtifactMeeting},
		{"spec", l1.KindWikiSection, "", connector.ContainerFolder, config.ArtifactSpec},
		{"issue", l1.KindIssue, "", connector.ContainerRepository, config.ArtifactIssue},
		{"commit", l1.KindCommit, "", connector.ContainerRepository, config.ArtifactCommit},
		{"channel thread", l1.KindChatThread, "", connector.ContainerChannel, config.ArtifactChatThread},
		{"direct thread", l1.KindChatThread, "", "dm", config.ArtifactDM},
		{"channel burst", l1.KindChatBurst, "", connector.ContainerChannel, config.ArtifactChatThread},
		{"direct burst", l1.KindChatBurst, "", "direct_message", config.ArtifactDM},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, err := l1.ClassFor(tt.kind, connector.Event{Payload: connector.Payload{
				Native: json.RawMessage(tt.native), Container: connector.Container{Kind: tt.container},
			}})
			if err != nil || got != tt.want || !got.Valid() {
				t.Errorf("ClassFor(%s) = %q, %v; want %q", tt.kind, got, err, tt.want)
			}
		})
	}
	for _, kind := range l1.Kinds() {
		got, err := l1.ClassFor(kind, connector.Event{})
		if err != nil || !got.Valid() {
			t.Errorf("kind %s has no valid class: %q, %v", kind, got, err)
		}
	}
	if _, err := l1.ClassFor("future_kind", connector.Event{}); err == nil {
		t.Fatal("unmapped kind accepted")
	}
}
