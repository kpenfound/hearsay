package l2

import (
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/kpenfound/hearsay/internal/config"
)

// Match is one entity a text names, and how it named it.
type Match struct {
	Entity Entity
	// Aliases are the names and aliases of the entity the text contains, as
	// the entity spells them. It is the bundle's `aliases_matched`.
	Aliases []string
	// Paths are the words of the text that one of the entity's path patterns
	// matches, as the text spells them.
	Paths []string
}

// Resolve is design.md's resolve(text): the entities a piece of text names, by
// alias and by path pattern, sorted by entity id.
//
// An alias is matched ignoring case at word edges, so "the engine" matches
// "The Engine" and "engine" does not match "engineering". An alias that more
// than one entity answers to resolves to none of them (docs/config.md#code),
// except that a name configuration gave wins over one seeded from a
// repository's layout: a person wrote the first down.
//
// A path is a word of the text with a `/` or a `.` in it — `engine/server`,
// `write.go` — matched against every entity's path patterns. Words without
// either are prose, and a directory called `docs` should not resolve every
// sentence that mentions documentation.
func Resolve(entities []Entity, text string) []Match {
	if strings.TrimSpace(text) == "" {
		return []Match{}
	}
	prose, paths := splitWords(text)
	folded := strings.ToLower(prose)
	foldedPaths := map[string]bool{}
	for _, p := range paths {
		foldedPaths[strings.ToLower(p)] = true
	}
	matches := map[string]*Match{}
	at := func(i int) *Match {
		m, ok := matches[entities[i].ID]
		if !ok {
			m = &Match{Entity: entities[i]}
			matches[entities[i].ID] = m
		}
		return m
	}

	for alias, owners := range aliasOwners(entities) {
		// An alias shaped like a path — `acme/api#12` — is a whole word or
		// nothing, so `acme/api` does not match inside it; every other alias
		// is read from the prose with those words blanked out, so `engine` does
		// not match inside `engine/server/write.go` or a link.
		if isPathLike(alias) && !foldedPaths[alias] || !isPathLike(alias) && !containsWord(folded, alias) {
			continue
		}
		for _, i := range owners {
			m := at(i)
			m.Aliases = append(m.Aliases, spelling(entities[i], alias))
		}
	}

	for _, word := range paths {
		if strings.Contains(word, "://") {
			continue
		}
		for i, e := range entities {
			for _, pattern := range e.PathPatterns {
				if config.MatchPath(pattern, word) {
					m := at(i)
					m.Paths = append(m.Paths, word)
					break
				}
			}
		}
	}

	out := make([]Match, 0, len(matches))
	for _, id := range sortedKeys(matches) {
		m := matches[id]
		m.Aliases = sortedUnique(m.Aliases)
		m.Paths = sortedUnique(m.Paths)
		out = append(out, *m)
	}
	return out
}

// aliasOwners maps every folded name and alias to the entities that answer to
// it, with the ambiguous ones already resolved or dropped.
func aliasOwners(entities []Entity) map[string][]int {
	all := map[string][]int{}
	for i, e := range entities {
		for _, name := range append([]string{e.Name}, e.Aliases...) {
			f := fold(name)
			if f == "" || slices.Contains(all[f], i) {
				continue
			}
			all[f] = append(all[f], i)
		}
	}
	out := map[string][]int{}
	for alias, owners := range all {
		if len(owners) == 1 {
			out[alias] = owners
			continue
		}
		var configured []int
		for _, i := range owners {
			if entities[i].Origin == OriginConfig {
				configured = append(configured, i)
			}
		}
		if len(configured) == 1 {
			out[alias] = configured
		}
	}
	return out
}

// spelling is the entity's own spelling of a folded alias.
func spelling(e Entity, folded string) string {
	for _, name := range append([]string{e.Name}, e.Aliases...) {
		if fold(name) == folded {
			return strings.TrimSpace(name)
		}
	}
	return folded
}

func fold(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// wordRe is one whitespace-separated word.
var wordRe = regexp.MustCompile(`\S+`)

// splitWords separates a text into its prose and its path-like words: the text
// with every word that holds a `/` or a `.` blanked out, byte for byte so that
// word edges either side stay where they were, and those words, trimmed of the
// punctuation around them. Links are among the words; the caller decides what
// they are.
func splitWords(text string) (string, []string) {
	b := []byte(text)
	var words []string
	for _, span := range wordRe.FindAllStringIndex(text, -1) {
		word := strings.Trim(text[span[0]:span[1]], "`'\"()[]{}<>,;:!?")
		word = strings.TrimRight(word, ".")
		if !isPathLike(word) {
			continue
		}
		words = append(words, word)
		for i := span[0]; i < span[1]; i++ {
			b[i] = ' '
		}
	}
	return string(b), words
}

// isPathLike reports whether a word is shaped like a path, a link or a
// qualified reference rather than prose.
func isPathLike(word string) bool { return strings.ContainsAny(word, "/.") }

// containsWord reports whether needle appears in haystack at word edges. Both
// are already folded.
func containsWord(haystack, needle string) bool {
	for from := 0; from+len(needle) <= len(haystack); {
		i := strings.Index(haystack[from:], needle)
		if i < 0 {
			return false
		}
		start := from + i
		end := start + len(needle)
		if (start == 0 || !isWordByte(haystack[start-1])) && (end == len(haystack) || !isWordByte(haystack[end])) {
			return true
		}
		from = start + 1
	}
	return false
}

// isWordByte reports whether a byte continues a word. Bytes above ASCII are word
// bytes, so an alias is not matched inside a word of another script.
func isWordByte(c byte) bool {
	return c >= 0x80 || c == '_' || unicode.IsLetter(rune(c)) || unicode.IsDigit(rune(c))
}
