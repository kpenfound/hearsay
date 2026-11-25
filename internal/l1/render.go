package l1

import (
	"strings"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/principal"
)

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

func header(ev connector.Event, kind connector.Kind, resolver *principal.Resolver) string {
	return string(kind) + " by " + attribution(ev.Payload.Author, resolver) + " at " + ev.Time.UTC().Format(time.RFC3339)
}

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
