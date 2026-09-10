package l1

import (
	"strings"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/principal"
)

// The two renderings a document carries, and why they are different strings.
//
// RawText is the artifact in the team's own words: the title, the body, and
// every comment and review in the order they were written, each attributed and
// timestamped. It is what the full-text index searches, it is what the distill
// tier is given to read, and it is never embedded — a long conversation
// averages out into a vector that means nothing.
//
// Text is the distillation: the title, the summary, what was concluded and what
// is still open. It is what is embedded, because it is one document about one
// thing, and it is what a bundle quotes a line of.
//
// Both are deterministic functions of what they render, which is what makes
// re-distilling an artifact produce the row that is already there.

// renderRaw is the conversation as one document.
func renderRaw(root connector.Event, children []connector.Event, resolver *principal.Resolver) string {
	var b strings.Builder
	if title := strings.TrimSpace(root.Payload.Title); title != "" {
		b.WriteString("# " + title + "\n\n")
	}
	b.WriteString(header(root, root.Kind, resolver))
	if text := strings.TrimSpace(root.Payload.Text); text != "" {
		b.WriteString("\n\n" + text)
	}
	for _, child := range children {
		b.WriteString("\n\n## " + header(child, child.Kind, resolver))
		if title := strings.TrimSpace(child.Payload.Title); title != "" && title != root.Payload.Title {
			b.WriteString("\n\n" + title)
		}
		if text := strings.TrimSpace(child.Payload.Text); text != "" {
			b.WriteString("\n\n" + text)
		}
	}
	return b.String()
}

// header attributes one event: what it is, who wrote it and when.
func header(ev connector.Event, kind connector.Kind, resolver *principal.Resolver) string {
	return string(kind) + " by " + attribution(ev.Payload.Author, resolver) + " at " + ev.Time.UTC().Format(time.RFC3339)
}

// attribution is who an event is by, for a person and for a model reading the
// document: the principal id where the identity mapping has one, and what the
// source called them where it does not.
//
// Preferring the principal id is what makes a document stable across a rename
// at the source, and it is why fixing the mapping and re-distilling recovers
// authorship — the document is rebuilt with the name Hearsay knows.
func attribution(author *connector.Identity, resolver *principal.Resolver) string {
	if author == nil {
		return "an unnamed author"
	}
	if id := resolvePerson(resolver, *author); id != "" {
		return id
	}
	switch {
	case author.Handle != "":
		return "@" + author.Handle
	case author.DisplayName != "":
		return author.DisplayName
	default:
		return author.Source + ":" + author.NativeID
	}
}

// renderText is the distillation, which is what is embedded.
//
// It holds the model's words and nothing else: no title, no id, no attribution.
// Those are columns of their own, and a vector over one document about one
// thing is what a similarity search is for — putting an artifact id in it moves
// every document a little closer to every other document from the same
// repository.
func renderText(d Document) string {
	var b strings.Builder
	if d.Body.Summary != "" {
		b.WriteString(d.Body.Summary + "\n\n")
	}
	if d.Body.Question != "" {
		b.WriteString("Question: " + d.Body.Question + "\n\n")
	}
	if d.Body.Change != "" {
		b.WriteString("Change: " + d.Body.Change + "\n\n")
	}
	if d.Body.Outcome != "" {
		b.WriteString("Outcome (" + string(d.Body.OutcomeKind) + "): " + d.Body.Outcome + "\n\n")
	}
	if len(d.Body.OpenQuestions) > 0 {
		b.WriteString("Open questions:\n")
		for _, q := range d.Body.OpenQuestions {
			b.WriteString("- " + q + "\n")
		}
	}
	return b.String()
}
