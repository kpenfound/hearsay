package l2

import (
	"strings"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/principal"
)

// OwnerRule is one line of a CODEOWNERS file: a pattern and the owners it
// names, as the file spells them — `@login`, `@org/team`, or an email address.
type OwnerRule struct {
	Pattern string
	Owners  []string
}

// ParseCodeOwners reads a CODEOWNERS file into its rules, in file order.
// Comments and blank lines are skipped, and so is a line with a pattern and no
// owners, which is the file's way of saying a path has none.
//
// It never fails: a line it cannot read is a line that owns nothing, which is
// what GitHub makes of it too.
func ParseCodeOwners(content []byte) []OwnerRule {
	var rules []OwnerRule
	for _, line := range strings.Split(string(content), "\n") {
		if at := strings.Index(line, "#"); at >= 0 && (at == 0 || line[at-1] != '\\') {
			line = line[:at]
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		rules = append(rules, OwnerRule{Pattern: fields[0], Owners: fields[1:]})
	}
	return rules
}

// OwnersOf is the owners the last rule covering a directory names, which is
// CODEOWNERS' own precedence: later lines win. The directory is repository
// relative, with no leading slash.
//
// The pattern syntax is CODEOWNERS' subset of gitignore: a pattern with no slash
// but a trailing one matches that name at any depth, a leading slash anchors it
// at the root, and a pattern naming a directory owns everything under it. A
// rule for files (`*.go`) does not own a directory.
func OwnersOf(rules []OwnerRule, dir string) []string {
	var owners []string
	for _, rule := range rules {
		if ownsDir(rule.Pattern, dir) {
			owners = rule.Owners
		}
	}
	return owners
}

func ownsDir(pattern, dir string) bool {
	if pattern == "*" || pattern == "/*" || pattern == "**" {
		return true
	}
	anchored := strings.HasPrefix(pattern, "/")
	pattern = strings.TrimSuffix(strings.TrimPrefix(pattern, "/"), "/")
	if pattern == "" {
		return false
	}
	if !anchored && !strings.Contains(pattern, "/") {
		pattern = "**/" + pattern
	}
	// A directory pattern owns the directory and everything under it, so a dir
	// is owned when it is the pattern or inside what the pattern names.
	return MatchPath(pattern, dir) || MatchPath(pattern+"/**", dir)
}

// ResolveOwners turns CODEOWNERS spellings into principal ids through the
// identity mapping, for the source the repository belongs to. `@login` is a
// person, `@org/team` a team. An owner that does not resolve — an email address,
// a login nobody mapped — is left out: no placeholder principal is minted, and
// the resolver keeps the sighting for a person to map (internal/principal).
func ResolveOwners(resolver *principal.Resolver, source string, spellings []string) []string {
	if resolver == nil {
		return nil
	}
	seen := map[string]bool{}
	var ids []string
	for _, spelling := range spellings {
		handle, ok := strings.CutPrefix(spelling, "@")
		if !ok {
			continue
		}
		var res principal.Resolution
		if strings.Contains(handle, "/") {
			res = resolver.ResolveGroup(source, handle)
		} else {
			res = resolver.Resolve(connector.Identity{Source: source, Kind: connector.IdentityUser, Handle: handle})
		}
		if res.Status == principal.Resolved && !seen[res.Principal.ID] {
			seen[res.Principal.ID] = true
			ids = append(ids, res.Principal.ID)
		}
	}
	return ids
}
