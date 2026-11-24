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
	"github.com/kpenfound/hearsay/internal/queue"
)

// ConversationSegmentationRequest asks for source-line boundaries only, over
// the conversation's numbered, speaker-labelled lines. The model cannot invent
// references, participants, ACLs or segment text.
func ConversationSegmentationRequest(lines []l1.ConversationLine, budget int) llm.Request {
	var b strings.Builder
	for i, line := range lines {
		scrubbed, _ := l1.Scrub(line.Text())
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
		System:    "Identify the distinct topics in this conversation between a person (Owner) and their assistant (Kern). Each numbered line is one message. Return contiguous ranges of numbered lines in source order. Topics may be interleaved: if a topic resumes later, give its later range the same topic label so both ranges become one document. Keep independent decisions and outcomes in separate ranges. Use a short, stable topic label for each range, preserving the label across small edits. Omit greetings, pleasantries and unrelated asides. Return no segments if there is no substantive discussion. Never rewrite or summarize the lines.",
		Messages:  []llm.Message{{Role: llm.RoleUser, Text: b.String()}},
		MaxTokens: budget,
		Schema:    &llm.Schema{Name: "conversation_topics", Description: "Topic boundaries in a conversation", Definition: definition},
	}
}

// The outcome of a conversation segment is the owner's, or it is nothing.
//
// This is the one place that rule lives. A conversation's lines are the owner's
// words and the assistant's answers, and only the owner's own words can become
// the owner's stance. Both ways a document enters the assertion pipeline come
// through here: the outcome sentence the stance takes its position from, and
// the judgments that classify it.
//
//   - [ownerOutcome] keeps the model's outcome sentence only when it cites
//     Owner lines and nothing else. A sentence resting on a Kern line, or on
//     none, is dropped, and a document with no outcome sentence is not
//     decided, proposed or resolved ([decideOutcome]).
//   - [ownerView] is what Jev judges: the owner's lines as the evidence, and
//     the assistant's lines apart, as context.
//
// The assertion worker reads a stance's position from [l1.Document.StanceText],
// which for a conversation segment is that sentence alone.

// ownerRule is the sentence every outcome question of a conversation segment
// begins with.
const ownerRule = "Judge only what the Owner's own lines say, under \"Owner's words\". The lines under \"Context from Kern\" are context only and never count: something only Kern decided, proposed or concluded is not an outcome. "

// ownerView is a conversation segment as Jev reads it for its outcome.
func ownerView(doc l1.Document) string {
	owner := l1.OwnerLines(doc.RawText)
	var said, aside []string
	for i, line := range strings.Split(doc.RawText, "\n") {
		numbered := fmt.Sprintf("%d. %s", i+1, line)
		if owner[i] {
			said = append(said, numbered)
		} else {
			aside = append(aside, numbered)
		}
	}
	view := "Owner's words:\n" + strings.Join(said, "\n")
	if len(said) == 0 {
		view = "Owner's words:\n(none)"
	}
	if len(aside) > 0 {
		view += "\n\nContext from Kern:\n" + strings.Join(aside, "\n")
	}
	return view
}

// ownerOutcome is the body a conversation segment's answer makes, with the
// outcome sentence kept only where it rests on the owner's lines: at least one
// cited line, and every cited line an Owner line of this segment.
func ownerOutcome(doc l1.Document, a answer) l1.Body {
	body := a.body()
	owner := l1.OwnerLines(doc.RawText)
	if len(a.OutcomeLines) == 0 {
		body.Outcome = ""
	}
	for _, n := range a.OutcomeLines {
		if n < 1 || n > len(owner) || !owner[n-1] {
			body.Outcome = ""
		}
	}
	return body
}

// isConversation reports whether a thread with no artifact of its own is a
// conversation a conversation source holds: it has current messages of that
// kind, or a distillation was published for it, which is what a conversation
// whose every message was deleted still has.
func (d *Distiller) isConversation(ctx context.Context, source, thread string) (bool, error) {
	events, err := d.events.Current(ctx, l0.ListOptions{Filter: l0.Filter{Source: source, Thread: thread}, Limit: 1})
	if err != nil {
		return false, fmt.Errorf("reading the conversation %s: %w", thread, err)
	}
	if len(events) > 0 {
		return l1.IsConversationMessage(events[0]), nil
	}
	_, published, err := d.docs.ConversationDistillationOf(ctx, source, thread)
	return published, err
}

// conversationLines reads a conversation's current messages as the lines the
// documents quote, and the event ids they are: the revisions this attempt read.
func conversationLines(ctx context.Context, events *l0.Store, source, thread string) ([]l1.ConversationLine, []string, error) {
	current, err := events.Current(ctx, l0.ListOptions{Filter: l0.Filter{Source: source, Thread: thread}, Limit: l0.MaxLimit})
	if err != nil {
		return nil, nil, fmt.Errorf("reading the conversation %s: %w", thread, err)
	}
	if len(current) >= l0.MaxLimit {
		// A document that claims to be the whole conversation and is not would
		// stay wrong on every rebuild, and a record naming only some messages
		// would leave the rest pending for good. A failed job is visible.
		return nil, nil, fmt.Errorf("conversation %s has at least %d messages, which is more than one read returns", thread, l0.MaxLimit)
	}
	lines := l1.ConversationLines(current)
	ids := make([]string, len(lines))
	for i, line := range lines {
		ids[i] = line.Event.ID
	}
	slices.Sort(ids)
	return lines, ids, nil
}

// distillConversation distils one conversation the way distillMeeting distils a
// transcript: a segmentation call over its numbered lines, one document per
// topic, and one transaction that reconciles the set. It also publishes the
// completion record, in that transaction, of exactly the message revisions this
// attempt read.
//
// An attempt that fails before the transaction records nothing. One whose
// inputs changed while it ran writes nothing either — not a record of what it
// read, which would name revisions no document was built from, and not
// documents built from messages that have since moved — and asks for another
// attempt, so the newer message is distilled afterwards.
func (d *Distiller) distillConversation(ctx context.Context, result Result, source, thread string) (Result, error) {
	lines, inputs, err := conversationLines(ctx, d.events, source, thread)
	if err != nil {
		return Result{}, err
	}
	var spans []l1.MeetingSpan
	if len(lines) > 0 {
		req := ConversationSegmentationRequest(lines, 0)
		if len(req.Messages[0].Text) > MaxPromptBytes {
			return Result{}, fmt.Errorf("conversation %s exceeds the segmentation prompt limit", thread)
		}
		call, done := context.WithTimeout(ctx, d.timeout)
		response, err := d.tier.Complete(call, req)
		done()
		if err != nil {
			return Result{}, fmt.Errorf("segmenting conversation %s: %w", thread, err)
		}
		var answer struct {
			Segments []l1.MeetingSpan `json:"segments"`
		}
		if err := json.Unmarshal(response.JSON, &answer); err != nil {
			return Result{}, fmt.Errorf("decoding segments of conversation %s: %w", thread, err)
		}
		spans = answer.Segments
	}
	repo, err := d.referenceRepo(ctx)
	if err != nil {
		return Result{}, err
	}
	segments, err := l1.BuildConversationSegments(source, thread, lines, spans, d.resolver, repo)
	if err != nil {
		return Result{}, fmt.Errorf("building segments of conversation %s: %w", thread, err)
	}
	runs := make(map[int]*outcomeRun)
	for i := range segments {
		previous, err := d.docs.Get(ctx, segments[i].ID)
		if err != nil && !errors.Is(err, l1.ErrNotFound) {
			return Result{}, err
		}
		if err == nil && previous.Kind == l1.KindConversationSegment && previous.RawText == segments[i].RawText {
			// The model already read these very lines. Its body stands, and the
			// store writes nothing if the envelope is the one already there.
			segments[i], _, err = segments[i].WithBody(previous.Body)
			if err != nil {
				return Result{}, err
			}
			continue
		}
		run, err := d.modelDocument(ctx, segments[i])
		if err != nil {
			return Result{}, err
		}
		runs[i], segments[i] = run, run.doc
	}

	ids := make([]string, len(segments))
	documents := make([]string, len(segments))
	for i, segment := range segments {
		ids[i], documents[i] = segment.ID, segment.ID
	}
	// The first line's container and touches pick the scope key, as a
	// transcript's root does.
	container := ""
	var touches []connector.Touch
	if len(lines) > 0 {
		container, touches = lines[0].Event.Payload.Container.NativeID, lines[0].Event.Payload.Touches
	}
	err = pgx.BeginFunc(ctx, d.pool, func(tx pgx.Tx) error {
		// The inputs are read again where they are written. A message that was
		// rewritten, or arrived, since this attempt read them is not in what the
		// documents quote or the record names, so none of it is published.
		_, now, err := conversationLines(ctx, l0.New(tx), source, thread)
		if err != nil {
			return err
		}
		if !slices.Equal(now, inputs) {
			result.Superseded = true
			_, err := queue.Enqueue(ctx, tx, queue.Request{Kind: JobKind(), TargetID: l1.DocID(source, thread)})
			return err
		}
		batch := make([]*outcomeRun, 0, len(runs))
		for _, run := range runs {
			batch = append(batch, run)
		}
		if err := stale(batch...); err != nil {
			return err
		}
		if err := commitOutcomes(ctx, tx, batch...); err != nil {
			return err
		}

		removed, err := deleteDerived(ctx, tx, `DELETE FROM l1_docs WHERE source = $1 AND kind = $2 AND left(source_native_id, length($3)) = $3 AND NOT (id = ANY($4))`, source, l1.KindConversationSegment, l1.ConversationSegmentPrefix(thread), ids)
		if err != nil {
			return fmt.Errorf("reconciling segments of conversation %s: %w", thread, err)
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
				asserting, err := l2.EnqueueAssertion(ctx, tx, d.repo, segment, container, touches)
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
		result.Recorded, err = l1.New(tx).PutConversationDistillation(ctx, l1.ConversationDistillation{
			Source: source, Thread: thread, Inputs: inputs, Documents: documents,
		})
		if err != nil {
			return err
		}
		return rebuilt(ctx, tx, ids, removed)
	})
	if err != nil {
		return Result{}, err
	}
	if result.Superseded {
		return result, nil
	}
	for _, segment := range segments {
		embedded, err := d.embed(ctx, segment.ID)
		if err != nil {
			return Result{}, err
		}
		result.Embedded = result.Embedded || embedded
	}
	result.Skipped = len(segments) == 0 && !result.Deleted && !result.Recorded
	return result, nil
}
