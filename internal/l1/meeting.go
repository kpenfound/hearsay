package l1

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/principal"
)

// MeetingSegmentPrefix groups every derived topic of one transcript. The
// length prevents a file named "a:b" from sharing a prefix with "a".
func MeetingSegmentPrefix(artifact string) string {
	return fmt.Sprintf("meeting-segment:%d:%s:", len(artifact), artifact)
}

// MeetingSpan names a contiguous, one-based range of transcript lines. Topic
// is an identity label, not model-written document content.
type MeetingSpan struct {
	Topic string `json:"topic"`
	Start int    `json:"start"`
	End   int    `json:"end"`
}

// MeetingLines keeps nonempty source lines in their original order. Numbering
// these lines lets a model choose boundaries without rewriting transcript text.
func MeetingLines(text string) []string {
	var lines []string
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// BuildMeetingSegments builds envelopes from validated topic boundaries. Each
// reference is extracted from the source words of this topic alone.
func BuildMeetingSegments(root connector.Event, spans []MeetingSpan, resolver *principal.Resolver, repo config.Repo) ([]Document, error) {
	if err := root.Validate(); err != nil {
		return nil, fmt.Errorf("the transcript event: %w", err)
	}
	if root.Kind != connector.KindTranscript && root.Payload.BaseKind != connector.KindTranscript {
		return nil, fmt.Errorf("%w: %s is not a transcript", ErrNotDistilled, root.Kind)
	}
	lines := MeetingLines(root.Payload.Text)
	previous := 0
	order := []string{}
	byTopic := map[string][]string{}
	for _, span := range spans {
		topic := strings.ToLower(strings.Join(strings.Fields(span.Topic), " "))
		if topic == "" || len(topic) > 160 || span.Start <= previous || span.End < span.Start || span.End > len(lines) {
			return nil, fmt.Errorf("%w: invalid meeting span %+v", ErrInvalidDocument, span)
		}
		previous = span.End
		if _, ok := byTopic[topic]; !ok {
			order = append(order, topic)
		}
		byTopic[topic] = append(byTopic[topic], lines[span.Start-1:span.End]...)
	}
	docs := make([]Document, 0, len(order))
	for _, topic := range order {
		sum := sha256.Sum256([]byte(topic))
		key := MeetingSegmentPrefix(root.Payload.Artifact) + hex.EncodeToString(sum[:12])
		raw := strings.Join(byTopic[topic], "\n")
		view := root
		view.Payload.Title = ""
		view.Payload.Text = raw
		view.Payload.Links = nil
		view.Payload.Mentions = nil
		refs := References([]connector.Event{view}, resolver, repo.Code)
		scrubbed, _ := Scrub(raw)
		if scrubbed = trimLines(scrubbed); scrubbed == "" {
			continue
		}
		doc := Document{
			ID: DocID(root.Source, key), Kind: KindMeetingSegment,
			Source: Source{System: root.Source, NativeID: key, URL: root.Payload.URL},
			L0Refs: []string{connector.EventID(root.Source, root.NativeID)},
			Time:   timesOf(root, nil), Participants: participantsOf(root, nil, resolver),
			References: refs, ACL: append(connector.ACL(nil), root.ACL...), RawText: scrubbed,
		}
		doc.Scope = scopeOf(view, repo, refs)
		docs = append(docs, doc)
	}
	return docs, nil
}
