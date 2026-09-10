package l1

import (
	"cmp"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/principal"
)

// MaxReferences bounds how many references one document carries. References are
// a join key rather than an index of everything a conversation mentioned, and a
// document naming hundreds of things joins to everything, which is the same as
// joining to nothing. The cap is applied after sorting, so what survives is
// stable rather than whichever comment happened to be read first.
const MaxReferences = 256

// RefType is what sort of thing a reference points at. The set is closed: a
// consumer joins on it, and a type nothing produces is a join that never
// matches.
type RefType string

// The reference types.
const (
	// RefIssue is a tracker item the source called an issue.
	RefIssue RefType = "issue"
	// RefPR is a change proposal.
	RefPR RefType = "pr"
	// RefCommit is a commit.
	RefCommit RefType = "commit"
	// RefTrackerItem is an issue or a pull request where the text does not say
	// which — `#31` and `acme/api#31` with no link, on a source whose issues
	// and change proposals share one numbering space. It is the design's
	// `tracker_item` entity type, and it collapses into [RefIssue] or [RefPR]
	// wherever the same document also spells the id out.
	RefTrackerItem RefType = "tracker_item"
	// RefPerson is a principal, from an @-mention or a mention the source
	// resolved itself. The id is the principal id, never a handle.
	RefPerson RefType = "person"
	// RefSystem is a code entity from `code/` configuration, matched on its
	// name and its aliases. The id is the entity id.
	RefSystem RefType = "system"
	// RefURL is a link that is none of the above, normalised.
	RefURL RefType = "url"
)

// Reference is one typed thing a document points at.
type Reference struct {
	Type RefType `json:"type"`
	ID   string  `json:"id"`
}

func (r Reference) validate() error {
	switch r.Type {
	case RefIssue, RefPR, RefCommit, RefTrackerItem, RefPerson, RefSystem, RefURL:
	default:
		return fmt.Errorf("type %q is not a reference type", r.Type)
	}
	if r.ID == "" {
		return fmt.Errorf("the %s reference has no id", r.Type)
	}
	return nil
}

// The shapes reference extraction reads out of a source's own text. They are
// deliberately conservative: a pattern that fires on ordinary prose costs every
// document a wrong join key, and a reference that is missed costs one.
var (
	// linkRe is a bare or markdown link. The trailing punctuation a sentence
	// puts after a URL is trimmed off the match rather than matched.
	linkRe = regexp.MustCompile(`https?://[^\s<>()\[\]{}"'` + "`" + `]+`)
	// mentionRe is an @-mention: a handle, or `@org/team` for a source that
	// names groups that way. The handle rule is the widest the sources this
	// build reads use — letters, digits and `-`, up to 39 of them.
	mentionRe = regexp.MustCompile(`@([A-Za-z0-9][A-Za-z0-9-]{0,38})(/[A-Za-z0-9][A-Za-z0-9._-]{0,38})?`)
	// itemRe is a short cross-reference: `#31`, or `owner/repo#31`. The number
	// is required to be a number, so `#1 priority` is a reference and
	// `#hashtag` is not.
	itemRe = regexp.MustCompile(`(?:([A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*))?#([0-9]{1,12})`)
	// pathRe is the shape of a hosted issue, pull request or commit URL, on any
	// host: `/owner/repo/(issues|pull|commit)/<id>`. Matching the shape rather
	// than the host is what makes an enterprise install of the same forge
	// produce the same ids as the public one — reference ids are spelled the
	// way that source's artifact ids are, and two forges are two sources.
	pathRe = regexp.MustCompile(`^/([A-Za-z0-9][A-Za-z0-9._-]*)/([A-Za-z0-9][A-Za-z0-9._-]*)/(issues|pull|commit|commits)/([A-Za-z0-9._-]+)`)
)

// References extracts everything a document points at, from the events it is
// built from, with no model in the loop: links, @-mentions resolved through the
// principal resolver, short cross-references read against the artifact's own
// container, and the code entities `code/` configuration names.
//
// It is deterministic and total — the same events, resolver and configuration
// always produce the same list, sorted and deduplicated — which is what lets L2
// join on it and what makes re-distilling an artifact cost nothing here.
//
// An @-mention that does not resolve to a principal produces no reference: no
// placeholder principal id is minted, and the sighting is recorded by the
// resolver for a person to map (internal/principal).
func References(events []connector.Event, resolver *principal.Resolver, code []config.CodeEntity) []Reference {
	seen := map[Reference]bool{}
	add := func(t RefType, id string) {
		if id != "" {
			seen[Reference{Type: t, ID: id}] = true
		}
	}
	aliases := systemAliases(code)

	for _, ev := range events {
		container := ev.Payload.Container.NativeID
		for _, link := range ev.Payload.Links {
			t, id := classifyLink(link)
			add(t, id)
		}
		for _, hint := range ev.Payload.Mentions {
			add(RefPerson, resolvePerson(resolver, hint))
		}
		for _, text := range []string{ev.Payload.Title, ev.Payload.Text} {
			if text == "" {
				continue
			}
			for _, link := range linkRe.FindAllString(text, -1) {
				t, id := classifyLink(trimLink(link))
				add(t, id)
			}
			for _, m := range mentionRe.FindAllStringSubmatch(text, -1) {
				add(RefPerson, resolveMention(resolver, ev.Source, m[1], m[2]))
			}
			for _, m := range itemRe.FindAllStringSubmatch(text, -1) {
				repo := cmp.Or(m[1], container)
				if repo != "" {
					add(RefTrackerItem, repo+"#"+m[2])
				}
			}
			for _, id := range systemsIn(text, aliases) {
				add(RefSystem, id)
			}
		}
	}

	return collapse(seen)
}

// collapse turns the set into the sorted list a document carries, dropping a
// `tracker_item` whose id the same document also spells out as an issue or a
// pull request: `#31` and a link to `/pull/31` in one conversation are one
// reference, and keeping both would make the vaguer one look like a second
// thing.
func collapse(seen map[Reference]bool) []Reference {
	spelled := map[string]bool{}
	for ref := range seen {
		if ref.Type == RefIssue || ref.Type == RefPR {
			spelled[ref.ID] = true
		}
	}
	refs := make([]Reference, 0, len(seen))
	for ref := range seen {
		if ref.Type == RefTrackerItem && spelled[ref.ID] {
			continue
		}
		refs = append(refs, ref)
	}
	slices.SortFunc(refs, func(a, b Reference) int {
		if c := cmp.Compare(a.Type, b.Type); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	if len(refs) > MaxReferences {
		refs = refs[:MaxReferences]
	}
	return refs
}

// classifyLink types one URL. A link to an issue, a pull request or a commit
// becomes that thing, spelled the way the source spells its artifact ids
// (docs/connector-contract.md); anything else is the link itself, normalised so
// that two spellings of one URL are one reference.
func classifyLink(link string) (RefType, string) {
	u, err := url.Parse(link)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return RefURL, ""
	}
	if m := pathRe.FindStringSubmatch(u.Path); m != nil {
		repo := m[1] + "/" + m[2]
		switch m[3] {
		case "issues":
			return RefIssue, repo + "#" + m[4]
		case "pull":
			return RefPR, repo + "#" + m[4]
		default:
			return RefCommit, repo + "@" + m[4]
		}
	}
	// The fragment is a position in a page, not a different page, and the
	// scheme and host are compared without case because they are not
	// case-sensitive. Everything else is left exactly as the source wrote it:
	// a query string can be the whole of what a link points at.
	u.Fragment = ""
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	return RefURL, strings.TrimSuffix(u.String(), "/")
}

// trimLink drops the punctuation a sentence puts after a URL, and the closing
// bracket of a markdown link the pattern stopped at.
func trimLink(link string) string { return strings.TrimRight(link, ".,;:!?") }

// resolvePerson is the principal an identity hint names, and the empty string
// for one that does not resolve.
func resolvePerson(resolver *principal.Resolver, hint connector.Identity) string {
	if resolver == nil {
		return ""
	}
	if res := resolver.Resolve(hint); res.Status == principal.Resolved {
		return res.Principal.ID
	}
	return ""
}

// resolveMention is the principal an @-mention names. `@org/team` is a group —
// a team is a principal that never acts, and a document referring to one is
// referring to the team — so it goes through the group lookup, with the
// source's own `@` stripped the way internal/principal asks.
func resolveMention(resolver *principal.Resolver, source, handle, team string) string {
	if resolver == nil {
		return ""
	}
	if team != "" {
		if res := resolver.ResolveGroup(source, handle+team); res.Status == principal.Resolved {
			return res.Principal.ID
		}
		return ""
	}
	return resolvePerson(resolver, connector.Identity{Source: source, Kind: connector.IdentityUser, Handle: handle})
}

// alias is one name a code entity answers to, folded for matching.
type alias struct {
	folded string
	entity string
}

// systemAliases is every name and alias of every configured code entity,
// longest first so that "engine server" wins over "engine" where both are
// configured and both match at the same place.
func systemAliases(code []config.CodeEntity) []alias {
	aliases := make([]alias, 0, len(code)*2)
	for _, e := range code {
		for _, name := range append([]string{e.Name}, e.Aliases...) {
			folded := strings.ToLower(strings.TrimSpace(name))
			if folded == "" {
				continue
			}
			aliases = append(aliases, alias{folded: folded, entity: e.ID})
		}
	}
	slices.SortFunc(aliases, func(a, b alias) int {
		if c := cmp.Compare(len(b.folded), len(a.folded)); c != 0 {
			return c
		}
		if c := cmp.Compare(a.folded, b.folded); c != 0 {
			return c
		}
		return cmp.Compare(a.entity, b.entity)
	})
	return aliases
}

// systemsIn is every code entity a text names. Matching is case-insensitive and
// bounded by word edges, so "the engine" matches "The Engine" and does not
// match "engineering".
func systemsIn(text string, aliases []alias) []string {
	if len(aliases) == 0 {
		return nil
	}
	folded := strings.ToLower(text)
	var found []string
	for _, a := range aliases {
		if containsWord(folded, a.folded) {
			found = append(found, a.entity)
		}
	}
	return found
}

// containsWord reports whether needle appears in haystack at word edges. Both
// are already folded.
func containsWord(haystack, needle string) bool {
	for at := 0; at+len(needle) <= len(haystack); {
		i := strings.Index(haystack[at:], needle)
		if i < 0 {
			return false
		}
		start := at + i
		end := start + len(needle)
		if isEdge(haystack, start, -1) && isEdge(haystack, end, +1) {
			return true
		}
		at = start + 1
	}
	return false
}

// isEdge reports whether the byte on one side of a position is not part of a
// word. A match at the start or the end of the text is an edge.
func isEdge(s string, at, dir int) bool {
	if dir < 0 {
		if at == 0 {
			return true
		}
		return !isWordByte(s[at-1])
	}
	if at >= len(s) {
		return true
	}
	return !isWordByte(s[at])
}

// isWordByte reports whether a byte continues a word. Bytes above ASCII are
// word bytes, so an alias is not matched inside a word of another script.
func isWordByte(c byte) bool {
	return c >= 0x80 || c == '_' || unicode.IsLetter(rune(c)) || unicode.IsDigit(rune(c))
}
