package l1

import (
	"fmt"
	"strings"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/work"
	"github.com/kpenfound/hearsay/internal/principal"
)

// IsDecision reports whether an L0 event is a decision a `work` source
// recorded. Its text is rendered from its fields, and nothing reads the fields
// back from it.
func IsDecision(ev connector.Event) bool { return ev.Kind == work.KindDecision }

// BuildDecision makes the document of a decision. Unlike every other kind it
// is complete without a model: the outcome, the decider, what it supersedes and
// the rest are fields of the record, and the document says what they say.
// Everything is scrubbed like any text L1 stores, and a re-run over the same
// revision builds the document that is already there.
//
// The class is `decision`, from the source the event was posted to and not from
// anything the record claims: who decided and who is named as author change
// nothing about it.
func BuildDecision(root connector.Event, resolver *principal.Resolver, repo config.Repo) (Document, error) {
	if err := root.Validate(); err != nil {
		return Document{}, fmt.Errorf("the root event: %w", err)
	}
	held, ok := work.Decode(root)
	if !ok || held.Kind != work.KindDecisionRecord {
		return Document{}, fmt.Errorf("%w: %s is not a decision", ErrNotDistilled, root.Kind)
	}
	ws, ok := WorkSourceOf(repo, root.Source)
	if !ok {
		return Document{}, fmt.Errorf("%w: source %q is not a work source", ErrNotDistilled, root.Source)
	}
	class, err := ClassFor(KindDecision, root)
	if err != nil {
		return Document{}, err
	}
	n := held.Native()
	scrub := func(s string) string {
		out, _ := Scrub(s)
		return trimLines(out)
	}
	decision := &Decision{
		ID: held.ID, WorkSource: ws, Project: held.Project, Workstream: held.Workstream,
		Decider: n.Decider, Context: scrub(n.Context), Rationale: scrub(n.Rationale),
		Supersedes: n.Supersedes, SpecDocument: n.SpecDocument, PullRequest: n.PullRequest, Question: n.Question,
	}
	for _, option := range n.RejectedOptions {
		decision.RejectedOptions = append(decision.RejectedOptions, scrub(option))
	}
	outcome := scrub(n.Outcome)

	refs := References([]connector.Event{root}, resolver, repo.Code)
	doc := Document{
		ID:            DocID(root.Source, root.Payload.Artifact),
		Kind:          KindDecision,
		ArtifactClass: class,
		Source:        Source{System: root.Source, NativeID: root.Payload.Artifact, URL: root.Payload.URL},
		L0Refs:        []string{connector.EventID(root.Source, root.NativeID)},
		Time:          timesOf(root, nil),
		Participants:  participantsOf(root, nil, resolver),
		References:    refs,
		ACL:           append(connector.ACL(nil), root.ACL...),
		Body: Body{
			Summary:     line(outcome),
			Outcome:     outcome,
			OutcomeKind: OutcomeDecided,
			Decision:    decision,
		},
	}
	doc.Scope = scopeOf(root, repo, refs)
	doc.RawText = trimLines(renderDecision(outcome, decision))
	doc.Text = doc.RawText
	if outcome == "" {
		return Document{}, fmt.Errorf("%w: %s has no outcome left after the scrub", ErrNotDistilled, doc.ID)
	}
	return doc, nil
}

// line is the first non-empty line of a text, trimmed.
func line(text string) string {
	for _, l := range strings.Split(text, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

// renderDecision is the decision as the prose search finds it by and a reader
// of the document reads: the same fields, in a fixed order.
func renderDecision(outcome string, d *Decision) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Outcome: %s\n\nDecider: %s\n\n", outcome, d.Decider)
	if d.Context != "" {
		fmt.Fprintf(&b, "Context: %s\n\n", d.Context)
	}
	if d.Rationale != "" {
		fmt.Fprintf(&b, "Rationale: %s\n\n", d.Rationale)
	}
	if len(d.RejectedOptions) > 0 {
		b.WriteString("Rejected options:\n")
		for _, option := range d.RejectedOptions {
			fmt.Fprintf(&b, "- %s\n", option)
		}
		b.WriteString("\n")
	}
	if d.Supersedes != "" {
		fmt.Fprintf(&b, "Supersedes: %s\n\n", d.Supersedes)
	}
	if d.SpecDocument != "" {
		fmt.Fprintf(&b, "Spec document: %s\n\n", d.SpecDocument)
	}
	if d.Question != "" {
		fmt.Fprintf(&b, "Question: %s\n\n", d.Question)
	}
	if d.PullRequest != "" {
		fmt.Fprintf(&b, "Pull request: %s\n\n", d.PullRequest)
	}
	return b.String()
}
