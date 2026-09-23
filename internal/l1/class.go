package l1

import (
	"encoding/json"
	"fmt"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
)

// ClassFor assigns authority provenance from the source artifact, independently
// of the model's judgement about its outcome.
func ClassFor(kind Kind, root connector.Event) (config.ArtifactClass, error) {
	switch kind {
	case KindPR:
		var state struct {
			MergedAt *string `json:"merged_at"`
			State    string  `json:"state"`
		}
		if len(root.Payload.Native) != 0 {
			if err := json.Unmarshal(root.Payload.Native, &state); err != nil {
				return "", fmt.Errorf("reading pull request state: %w", err)
			}
		}
		if state.MergedAt != nil && *state.MergedAt != "" || state.State == "merged" {
			return config.ArtifactMergedPR, nil
		}
		return config.ArtifactPullRequest, nil
	case KindMeetingSegment:
		return config.ArtifactMeeting, nil
	case KindWikiSection:
		return config.ArtifactSpec, nil
	case KindIssue:
		return config.ArtifactIssue, nil
	case KindCommit:
		return config.ArtifactCommit, nil
	case KindChatThread, KindChatBurst:
		switch root.Payload.Container.Kind {
		case connector.ContainerDM, "direct_message":
			return config.ArtifactDM, nil
		default:
			return config.ArtifactChatThread, nil
		}
	default:
		return "", fmt.Errorf("%w: no artifact class for kind %q", ErrInvalidDocument, kind)
	}
}
