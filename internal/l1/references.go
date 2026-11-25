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
	// name and its aliases, or on its path patterns against the paths a change
	// touches. The id is the entity id.
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

var (
	linkRe    = regexp.MustCompile(`(?i:https?)://[^\s<>()\[\]{}"'` + "`" + `]+`)
	mentionRe = regexp.MustCompile(`@([A-Za-z0-9][A-Za-z0-9-]{0,38})(/[A-Za-z0-9][A-Za-z0-9._-]{0,38})?`)
	itemRe    = regexp.MustCompile(`(?:([A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*))?#([0-9]{1,12})`)
	pathRe    = regexp.MustCompile(`^/([A-Za-z0-9][A-Za-z0-9._-]*)/([A-Za-z0-9][A-Za-z0-9._-]*)/(issues|pull|commit|commits)/([A-Za-z0-9._-]+)`)
)

// References extracts everything a document points at, from the events it is
// built from, with no model in the loop: links, @-mentions resolved through the
// principal resolver, short cross-references read against the artifact's own
// container, the code entities `code/` configuration names, and the code
// entities whose path patterns match a path an event says it touches. Entries
// in payload.touches also name the repository's project entity and match paths
// in that repository, independently of the event's own source and container.
//
// It is deterministic and total — the same events, resolver and configuration
// always produce the same list, sorted and deduplicated — which is what lets L2
// join on it and what makes re-distilling an artifact cost nothing here. The
// list is sorted by type, then id, except that the code entities a change
// touches come first among the `system` references, most specific first (see
// [touchedSystems]).
//
// The paths themselves are matched and dropped: a document records which
// entities a change touched, never the files.
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
	touched := map[string]int{}
	addTouched := func(matches map[string]int) {
		for id, depth := range matches {
			add(RefSystem, id)
			if held, ok := touched[id]; !ok || depth > held {
				touched[id] = depth
			}
		}
	}

	for _, ev := range events {
		container := ev.Payload.Container.NativeID
		addTouched(touchedSystems(ev, code))
		for _, touch := range ev.Payload.Touches {
			addTouched(pathSystems(touch.Source, touch.Project, touch.Paths, code))
			for _, e := range code {
				if e.Repo.Source == touch.Source && strings.EqualFold(e.Repo.Project, touch.Project) && e.ID == "code:"+e.Repo.Project {
					add(RefSystem, e.ID)
				}
			}
		}
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
			spans := linkRe.FindAllStringIndex(text, -1)
			for _, span := range spans {
				t, id := classifyLink(trimLink(text[span[0]:span[1]]))
				add(t, id)
			}
			prose := maskSpans(text, spans)
			for _, m := range mentionRe.FindAllStringSubmatchIndex(prose, -1) {
				if !isEdge(prose, m[0], -1) {
					continue
				}
				add(RefPerson, resolveMention(resolver, ev.Source, group(prose, m, 1), group(prose, m, 2)))
			}
			for _, m := range itemRe.FindAllStringSubmatch(prose, -1) {
				repo := cmp.Or(m[1], container)
				if repo != "" {
					add(RefTrackerItem, repo+"#"+m[2])
				}
			}
			for _, id := range systemsIn(prose, aliases) {
				add(RefSystem, id)
			}
		}
	}

	return collapse(seen, touched)
}

func touchedSystems(ev connector.Event, code []config.CodeEntity) map[string]int {
	return pathSystems(ev.Source, ev.Payload.Container.NativeID, ev.Payload.Paths, code)
}

func pathSystems(source, project string, paths []string, code []config.CodeEntity) map[string]int {
	if len(paths) == 0 {
		return nil
	}
	out := map[string]int{}
	for _, e := range code {
		if e.Repo.Source != source || !strings.EqualFold(e.Repo.Project, project) {
			continue
		}
		for _, pattern := range e.PathPatterns {
			if !slices.ContainsFunc(paths, func(path string) bool { return config.MatchPath(pattern, path) }) {
				continue
			}
			depth := 0
			if prefix := config.StaticPrefix(pattern); prefix != "" {
				depth = strings.Count(prefix, "/") + 1
			}
			if held, ok := out[e.ID]; !ok || depth > held {
				out[e.ID] = depth
			}
		}
	}
	return out
}

func collapse(seen map[Reference]bool, touched map[string]int) []Reference {
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
		if a.Type == RefSystem {
			if c := cmp.Compare(specificity(touched, b.ID), specificity(touched, a.ID)); c != 0 {
				return c
			}
		}
		return cmp.Compare(a.ID, b.ID)
	})
	if len(refs) > MaxReferences {
		refs = refs[:MaxReferences]
	}
	return refs
}

func specificity(touched map[string]int, id string) int {
	if depth, ok := touched[id]; ok {
		return depth
	}
	return -1
}

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
	u.Fragment = ""
	u.User = nil
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	id, _ := Scrub(strings.TrimSuffix(u.String(), "/"))
	return RefURL, id
}

func trimLink(link string) string { return strings.TrimRight(link, ".,;:!?") }

func maskSpans(text string, spans [][]int) string {
	if len(spans) == 0 {
		return text
	}
	b := []byte(text)
	for _, span := range spans {
		for i := span[0]; i < span[1]; i++ {
			b[i] = ' '
		}
	}
	return string(b)
}

func group(s string, m []int, n int) string {
	if 2*n+1 >= len(m) || m[2*n] < 0 {
		return ""
	}
	return s[m[2*n]:m[2*n+1]]
}

func resolvePerson(resolver *principal.Resolver, hint connector.Identity) string {
	if resolver == nil {
		return ""
	}
	if res := resolver.Resolve(hint); res.Status == principal.Resolved {
		return res.Principal.ID
	}
	return ""
}

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

type alias struct {
	folded string
	entity string
}

func systemAliases(code []config.CodeEntity) []alias {
	aliases := make([]alias, 0, len(code)*2)
	owners := map[string]map[string]bool{}
	for _, e := range code {
		for _, name := range append([]string{e.Name}, e.Aliases...) {
			key := strings.ToLower(strings.TrimSpace(name))
			if key == "" {
				continue
			}
			if owners[key] == nil {
				owners[key] = map[string]bool{}
			}
			owners[key][e.ID] = true
		}
	}
	for _, e := range code {
		for _, name := range append([]string{e.Name}, e.Aliases...) {
			folded := strings.ToLower(strings.TrimSpace(name))
			if folded == "" || len(owners[folded]) != 1 {
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

func isWordByte(c byte) bool {
	return c >= 0x80 || c == '_' || unicode.IsLetter(rune(c)) || unicode.IsDigit(rune(c))
}
