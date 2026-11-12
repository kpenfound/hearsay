package work

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/push"
)

// workReader reads the work source that holds the projects. On the work source
// that is its own sink; on an owner source it is the paired source's.
func (c *Connector) workReader(own connector.ArtifactReader) (connector.ArtifactReader, error) {
	if c.ownerOf == "" {
		return own, nil
	}
	return c.peerReader(c.ownerOf)
}

func (c *Connector) peerReader(source string) (connector.ArtifactReader, error) {
	sink, ok := c.shared.sink(source)
	if !ok {
		return nil, fmt.Errorf("source %s is not running in this process", source)
	}
	reader, ok := sink.(connector.ArtifactReader)
	if !ok {
		return nil, fmt.Errorf("source %s cannot be read", source)
	}
	return reader, nil
}

// pairSources are the sources whose records one project-wide id spans: the
// work source and every owner source naming it, each with its reader.
func (c *Connector) pairSources(own connector.ArtifactReader) ([]SourceReader, error) {
	ws := c.workSource()
	out := make([]SourceReader, 0, 2)
	for _, source := range append([]string{ws}, c.shared.ownersOf(ws)...) {
		if source == c.source {
			out = append(out, SourceReader{Source: source, Reader: own})
			continue
		}
		reader, err := c.peerReader(source)
		if err != nil {
			return nil, err
		}
		out = append(out, SourceReader{Source: source, Reader: reader})
	}
	return out, nil
}

// record builds the event for a document or decision. The caller holds the
// project's pair lock, which is what makes the id and supersession checks here
// stand until the event is written.
func (c *Connector) record(ctx context.Context, own connector.ArtifactReader, poster, artifact string, r Request) (connector.Event, error) {
	works, err := c.workReader(own)
	if err != nil {
		return connector.Event{}, err
	}
	ws := c.workSource()
	project, ok, err := works.CurrentArtifact(ctx, ws, r.Project)
	if err != nil {
		return connector.Event{}, fmt.Errorf("read project %s: %w", r.Project, err)
	}
	if !ok {
		return connector.Event{}, push.Refuse(push.ErrConflict, "project is not held")
	}
	if r.Workstream != "" {
		if _, ok, err := works.CurrentArtifact(ctx, ws, r.Project+"#"+r.Workstream); err != nil {
			return connector.Event{}, fmt.Errorf("read workstream: %w", err)
		} else if !ok {
			return connector.Event{}, push.Refuse(push.ErrConflict, "workstream is not held")
		}
	}
	sources, err := c.pairSources(own)
	if err != nil {
		return connector.Event{}, err
	}
	records, err := Collect(ctx, r.Project, sources...)
	if err != nil {
		return connector.Event{}, err
	}
	id := r.Document
	if r.Kind == KindDecisionRecord {
		id = r.Decision
	}
	held, found, err := Resolve(records, r.Kind, id)
	if err != nil {
		return connector.Event{}, err
	}
	if found && (held.Source != c.source || held.Artifact != artifact) {
		return connector.Event{}, push.Refuse(push.ErrConflict, fmt.Sprintf("%s id %q is already held in project %s at %s; delete it there before posting it here", r.Kind, id, r.Project, held.Where()))
	}

	native := Native{Project: r.Project, Workstream: r.Workstream, Poster: poster, WorkSource: ws}
	author := c.identity(poster)
	if r.fields["author"] {
		author = c.identity(r.Author)
	}
	ev := connector.Event{Source: c.source, Kind: connector.KindDocument, Time: r.CreatedAt.UTC(), ACL: project.ACL, Payload: connector.Payload{
		Artifact: artifact, Container: connector.Container{Kind: "project", NativeID: r.Project}, Author: &author,
	}}
	if r.fields["author"] {
		ev.Payload.Participants = []connector.Participant{{Identity: c.identity(poster), Role: connector.RoleAgent}}
	}
	if r.Kind == KindDocumentRecord {
		native.Document = r.Document
		ev.Payload.Title, ev.Payload.Text, ev.Payload.URL = r.Name, r.Body, r.URL
	} else {
		ev.Kind, ev.Payload.BaseKind = KindDecision, connector.KindDocument
		native.Decision, native.Context, native.Outcome, native.Rationale = r.Decision, r.Context, r.Outcome, r.Rationale
		native.RejectedOptions, native.Decider = r.RejectedOptions, r.Decider
		native.Supersedes, native.SpecDocument, native.PullRequest, native.Question = r.Supersedes, r.SpecDocument, r.PullRequest, r.Question
		if r.PullRequest != "" {
			ev.Payload.Links = []string{r.PullRequest}
		}
		if r.Supersedes != "" {
			target, ok, err := Resolve(records, KindDecisionRecord, r.Supersedes)
			if err != nil {
				return connector.Event{}, err
			}
			if !ok {
				return connector.Event{}, push.Refuse(push.ErrConflict, fmt.Sprintf("superseded decision %q is not held in project %s", r.Supersedes, r.Project))
			}
			native.SupersedesAt = &Location{Source: target.Source, Artifact: target.Artifact}
			cycle, err := ClosesCycle(records, r.Decision, r.Supersedes)
			if err != nil {
				return connector.Event{}, err
			}
			if cycle {
				return connector.Event{}, push.Refuse(push.ErrConflict, fmt.Sprintf("decision %q superseding %q would close a cycle of supersessions", r.Decision, r.Supersedes))
			}
		}
		if r.SpecDocument != "" {
			// The link resolves like any id, when its document is held. A
			// document posted later is not a reason to refuse the decision.
			if doc, ok, err := Resolve(records, KindDocumentRecord, r.SpecDocument); err != nil {
				return connector.Event{}, err
			} else if ok {
				native.SpecDocumentAt = &Location{Source: doc.Source, Artifact: doc.Artifact}
			}
		}
		ev.Payload.Title = decisionTitle(r.Outcome)
		ev.Payload.Text = decisionText(r)
	}
	raw, err := json.Marshal(native)
	if err != nil {
		return connector.Event{}, fmt.Errorf("encode native ids: %w", err)
	}
	ev.Payload.Native = raw
	edited := r.UpdatedAt
	if edited.IsZero() {
		edited = r.CreatedAt
	}
	return push.Revise(ev, edited.UTC())
}

const maxTitleRunes = 120

// decisionTitle is the outcome's first line, cut to a title's length.
func decisionTitle(outcome string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(outcome), "\n")
	line = strings.TrimSpace(line)
	if utf8.RuneCountInString(line) > maxTitleRunes {
		line = string([]rune(line)[:maxTitleRunes-1]) + "…"
	}
	return line
}

// decisionText renders the structured fields as prose, for search and for a
// reader of the document. The fields themselves stay in the payload's native
// object; nothing reads them back from this text.
func decisionText(r Request) string {
	var b strings.Builder
	section := func(label, value string) {
		if strings.TrimSpace(value) != "" {
			fmt.Fprintf(&b, "%s: %s\n\n", label, strings.TrimSpace(value))
		}
	}
	section("Outcome", r.Outcome)
	section("Decider", r.Decider)
	section("Context", r.Context)
	section("Rationale", r.Rationale)
	if len(r.RejectedOptions) > 0 {
		b.WriteString("Rejected options:\n")
		for _, option := range r.RejectedOptions {
			fmt.Fprintf(&b, "- %s\n", strings.TrimSpace(option))
		}
		b.WriteString("\n")
	}
	section("Supersedes", r.Supersedes)
	section("Spec document", r.SpecDocument)
	section("Question", r.Question)
	section("Pull request", r.PullRequest)
	return strings.TrimSpace(b.String())
}
