package l1

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/principal"
)

// WikiSectionPrefix groups all derived sections of one source artifact. The
// length makes prefixes unambiguous even if an artifact id contains a colon.
func WikiSectionPrefix(artifact string) string {
	return fmt.Sprintf("wiki-section:%d:%s:", len(artifact), artifact)
}

// WikiSection is one contiguous slice of the source markdown. Heading is empty
// for material before the first heading (or for a page with no headings).
type WikiSection struct {
	Key     string
	Heading string
	Text    string
}

var markdownHeading = regexp.MustCompile(`^ {0,3}#{1,6}[ \t]+(.+?)[ \t]*#*[ \t]*$`)

// SplitWikiSections partitions markdown at ATX headings of any level. A
// heading starts a section and the next heading ends it, regardless of depth.
// Text before the first heading is a separate intro section; a page without
// headings is one intro section. Fenced code is kept verbatim.
func SplitWikiSections(artifact, markdown string) []WikiSection {
	var sections []WikiSection
	seen := map[string]int{}
	start, heading, key := 0, "", "intro"
	inFence := byte(0)
	fenceSize := 0
	frontmatter := strings.HasPrefix(markdown, "---\n") || strings.HasPrefix(markdown, "---\r\n")
	for offset := 0; offset < len(markdown); {
		end := strings.IndexByte(markdown[offset:], '\n')
		if end < 0 {
			end = len(markdown)
		} else {
			end += offset + 1
		}
		line := strings.TrimRight(markdown[offset:end], "\r\n")
		if frontmatter {
			if offset > 0 && (line == "---" || line == "...") {
				frontmatter = false
			}
			offset = end
			continue
		}
		trimmed := strings.TrimLeft(line, " ")
		if len(line)-len(trimmed) <= 3 && len(trimmed) >= 3 && (strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")) {
			mark := trimmed[0]
			n := 0
			for n < len(trimmed) && trimmed[n] == mark {
				n++
			}
			if inFence == 0 {
				inFence, fenceSize = mark, n
			} else if mark == inFence && n >= fenceSize && strings.TrimSpace(trimmed[n:]) == "" {
				inFence = 0
			}
		} else if inFence == 0 {
			if match := markdownHeading.FindStringSubmatch(line); match != nil {
				if text := strings.TrimSpace(markdown[start:offset]); text != "" {
					sections = append(sections, WikiSection{Key: WikiSectionPrefix(artifact) + key, Heading: heading, Text: text})
				}
				heading = strings.TrimSpace(match[1])
				identity := strings.ToLower(heading)
				seen[identity]++
				sum := sha256.Sum256([]byte(identity))
				key = hex.EncodeToString(sum[:12]) + fmt.Sprintf(":%d", seen[identity])
				start = offset
			}
		}
		offset = end
	}
	if text := strings.TrimSpace(markdown[start:]); text != "" {
		sections = append(sections, WikiSection{Key: WikiSectionPrefix(artifact) + key, Heading: heading, Text: text})
	}
	return sections
}

// BuildWikiSections builds each section from the same current L0 revision.
// References and model input see only that section's markdown, while source
// metadata, ACL, participants and provenance come from the source artifact.
func BuildWikiSections(root connector.Event, resolver *principal.Resolver, repo config.Repo) ([]Document, error) {
	if err := root.Validate(); err != nil {
		return nil, fmt.Errorf("the root event: %w", err)
	}
	if root.Kind != connector.KindDocument && root.Payload.BaseKind != connector.KindDocument {
		return nil, fmt.Errorf("%w: %s is not a document", ErrNotDistilled, root.Kind)
	}
	sections := SplitWikiSections(root.Payload.Artifact, root.Payload.Text)
	if len(sections) == 0 {
		sections = []WikiSection{{Key: WikiSectionPrefix(root.Payload.Artifact) + "intro", Text: root.Payload.Title}}
	}
	result := make([]Document, 0, len(sections))
	for _, section := range sections {
		view := root
		view.Payload.Text = section.Text
		view.Payload.Title = section.Heading
		view.Payload.Links = nil // source-wide links must not leak into another section
		view.Payload.Mentions = nil
		refs := References([]connector.Event{view}, resolver, repo.Code)
		raw, _ := Scrub(section.Text)
		doc := Document{
			ID: DocID(root.Source, section.Key), Kind: KindWikiSection, ArtifactClass: config.ArtifactSpec,
			Source: Source{System: root.Source, NativeID: section.Key, URL: root.Payload.URL},
			L0Refs: []string{connector.EventID(root.Source, root.NativeID)},
			Time:   timesOf(root, nil), Participants: participantsOf(root, nil, resolver),
			References: refs, ACL: append(connector.ACL(nil), root.ACL...),
			RawText: trimLines(raw),
		}
		doc.Scope = scopeOf(root, repo, refs)
		if doc.RawText != "" {
			result = append(result, doc)
		}
	}
	return result, nil
}
