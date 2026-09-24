package distiller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/llm"
)

const maxMeetingSegments = 64

// MeetingSegmentationRequest asks for source-line boundaries only. The model
// cannot invent references, participants, ACLs, or segment text.
func MeetingSegmentationRequest(root connector.Event, budget int) llm.Request {
	lines := l1.MeetingLines(root.Payload.Text)
	var b strings.Builder
	for i, line := range lines {
		scrubbed, _ := l1.Scrub(line)
		fmt.Fprintf(&b, "%d: %s\n", i+1, scrubbed)
	}
	closed := false
	definition, _ := json.Marshal(schemaNode{Type: "object", Properties: map[string]schemaNode{
		"segments": {Type: "array", MaxItems: maxMeetingSegments, Items: &schemaNode{
			Type: "object", Properties: map[string]schemaNode{
				"topic": {Type: "string", MinLength: 1, MaxLength: 160},
				"start": {Type: "integer"}, "end": {Type: "integer"},
			}, Required: []string{"topic", "start", "end"}, AdditionalProperties: &closed,
		}},
	}, Required: []string{"segments"}, AdditionalProperties: &closed})
	return llm.Request{
		System:    "Identify the distinct topics in this meeting transcript. Return contiguous ranges of numbered source lines in source order. If a topic resumes later, give its later range the same topic label so both ranges become one document. Keep independent decisions and outcomes in separate ranges. Use a short, stable topic label for each range, preserving the label across small transcript edits. Omit greetings, boilerplate and unrelated material. Return no segments if this is not a meeting or has no substantive discussion. Never rewrite or summarize the lines.",
		Messages:  []llm.Message{{Role: llm.RoleUser, Text: b.String()}},
		MaxTokens: budget,
		Schema:    &llm.Schema{Name: "meeting_topics", Description: "Topic boundaries in a transcript", Definition: definition},
	}
}

func (d *Distiller) distillMeeting(ctx context.Context, result Result, root connector.Event) (Result, error) {
	lines := l1.MeetingLines(root.Payload.Text)
	var spans []l1.MeetingSpan
	if len(lines) > 0 {
		req := MeetingSegmentationRequest(root, 0)
		if len(req.Messages[0].Text) > MaxPromptBytes {
			return Result{}, fmt.Errorf("transcript %s exceeds the segmentation prompt limit", result.DocID)
		}
		call, done := context.WithTimeout(ctx, d.timeout)
		response, err := d.tier.Complete(call, req)
		done()
		if err != nil {
			return Result{}, fmt.Errorf("segmenting %s: %w", result.DocID, err)
		}
		var answer struct {
			Segments []l1.MeetingSpan `json:"segments"`
		}
		if err := json.Unmarshal(response.JSON, &answer); err != nil {
			return Result{}, fmt.Errorf("decoding segments of %s: %w", result.DocID, err)
		}
		spans = answer.Segments
	}
	repo, err := d.referenceRepo(ctx)
	if err != nil {
		return Result{}, err
	}
	segments, err := l1.BuildMeetingSegments(root, spans, d.resolver, repo)
	if err != nil {
		return Result{}, fmt.Errorf("building segments of %s: %w", result.DocID, err)
	}
	for i := range segments {
		previous, err := d.docs.Get(ctx, segments[i].ID)
		if err != nil && !errors.Is(err, l1.ErrNotFound) {
			return Result{}, err
		}
		if err == nil && previous.Kind == l1.KindMeetingSegment && previous.RawText == segments[i].RawText {
			if previous.Source.URL == segments[i].Source.URL && slices.Equal(previous.ACL, segments[i].ACL) &&
				slices.Equal(previous.Participants, segments[i].Participants) && slices.Equal(previous.References, segments[i].References) && slices.Equal(previous.Scope, segments[i].Scope) {
				segments[i] = previous.Document
				continue
			}
			segments[i], _, err = segments[i].WithBody(previous.Body)
			if err != nil {
				return Result{}, err
			}
			continue
		}
		body, err := d.distil(ctx, segments[i])
		if err != nil {
			return Result{}, err
		}
		segments[i], _, err = segments[i].WithBody(body)
		if err != nil {
			return Result{}, err
		}
	}
	current, err := d.events.Current(ctx, l0.ListOptions{Filter: l0.Filter{Source: root.Source, Artifact: root.Payload.Artifact}, Limit: 1})
	if err != nil {
		return Result{}, fmt.Errorf("re-reading %s: %w", result.DocID, err)
	}
	if len(current) != 1 || current[0].NativeID != root.NativeID {
		result.Superseded = true
		return result, nil
	}
	ids := make([]string, len(segments))
	for i, segment := range segments {
		ids[i] = segment.ID
	}
	err = pgx.BeginFunc(ctx, d.pool, func(tx pgx.Tx) error {
		removed, err := deleteDerived(ctx, tx, `DELETE FROM l1_docs WHERE source = $1 AND kind = $2 AND left(source_native_id, length($3)) = $3 AND NOT (id = ANY($4))`, root.Source, l1.KindMeetingSegment, l1.MeetingSegmentPrefix(root.Payload.Artifact), ids)
		if err != nil {
			return fmt.Errorf("reconciling segments of %s: %w", result.DocID, err)
		}
		result.Deleted = len(removed) > 0
		if err := l2.EnqueueDeletedEvidence(ctx, tx, removed); err != nil {
			return err
		}
		for _, segment := range segments {
			written, err := l1.New(tx).Put(ctx, segment)
			if err != nil {
				return err
			}
			result.Written = result.Written || written
			if written {
				asserting, err := l2.EnqueueAssertion(ctx, tx, d.repo, segment, root.Payload.Container.NativeID)
				if err != nil {
					return err
				}
				result.Asserting = result.Asserting || asserting
				if !asserting {
					if err := l2.EnqueueNonassertingEvidence(ctx, tx, segment.ID); err != nil {
						return err
					}
				}
			}
		}
		return rebuilt(ctx, tx, ids, removed)
	})
	if err != nil {
		return Result{}, err
	}
	for _, segment := range segments {
		embedded, err := d.embed(ctx, segment.ID)
		if err != nil {
			return Result{}, err
		}
		result.Embedded = result.Embedded || embedded
	}
	result.Skipped = len(segments) == 0 && !result.Deleted
	return result, nil
}
