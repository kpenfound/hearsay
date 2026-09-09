package config

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/kpenfound/hearsay/internal/connector"
)

// maxEntityIDLen bounds an entity id. Entity ids travel in bundles and in L2
// rows the way event ids travel in L0, so they get a stated bound rather than
// whatever someone pastes.
const maxEntityIDLen = 512

// build turns the decoded documents into a [Repo] and records a problem for
// everything wrong with them. It never stops early: a configuration with four
// mistakes should take one round trip to fix, not four.
//
// The order is the dependency order. Sources are validated first because
// everything else references them, then principals, then the code entities that
// name principals as owners, then the scopes that name both, and last the
// authority policies, which reference all of it. An object that failed
// validation is still put in the Repo, so that a reference to it by id still
// resolves and one mistake is reported once. The exception is an object whose
// id is what is wrong: there is then genuinely nothing of that name, and
// everything that named it says so.
func (l *loader) build() Repo {
	// Anything recorded before this point is a file that did not parse or a
	// key that is not a field. What such a file was meant to say is unknown, so
	// every reference into it would be reported as missing and the whole
	// configuration would look empty. Report what could not be read, and check
	// the rest once it can be.
	if l.probs.any() {
		return Repo{}
	}

	var r Repo
	r.Sources = l.buildSources()
	r.Principals = l.buildPrincipals(r)
	r.Code = l.buildCode(r)
	r.Scopes = l.buildScopes(r)
	r.Authority = l.buildAuthority(r)

	if len(r.Sources) == 0 {
		l.probs.add("", 0, "", "no sources are configured: Hearsay would ingest nothing")
	}
	if len(r.Scopes) == 0 {
		l.probs.add("", 0, "", "no scopes are configured: Hearsay would serve no bundles")
	}
	return r
}

// at locates one object of the configuration, for error messages.
type at struct {
	file  string
	line  int
	label string
}

// locate names an object by its id, falling back to its position in the file
// for an object whose id is the thing that is wrong.
func locate[T any](d doc[T], noun, id string, i int) at {
	label := fmt.Sprintf("%s %d", noun, i+1)
	if id != "" {
		label = fmt.Sprintf("%s %q", noun, id)
	}
	return at{file: d.file, line: d.line, label: label}
}

// bad records a problem with one field of one object. An empty field means the
// object as a whole.
func (l *loader) bad(a at, field, format string, args ...any) {
	where := a.label
	if field != "" {
		where += ": " + field
	}
	l.probs.add(a.file, a.line, where, format, args...)
}

// names checks a list of names: no blanks, no duplicates, and [AnyValue] only
// where it is allowed and only on its own, because `[*, x]` reads as though x
// were narrowing something it is not. check reports what is wrong with one
// name, or empty if it is fine; it is not called for [AnyValue].
func (l *loader) names(a at, field string, items []string, allowAny bool, check func(string) string) {
	seen := make(map[string]bool, len(items))
	for i, name := range items {
		f := fmt.Sprintf("%s[%d]", field, i)
		switch {
		case name == "":
			l.bad(a, f, "is empty")
		case seen[name]:
			l.bad(a, f, "%q is listed twice", name)
		case name == AnyValue && !allowAny:
			l.bad(a, f, "%q is not allowed here: list them explicitly", AnyValue)
		case name == AnyValue && len(items) > 1:
			l.bad(a, f, "%q covers everything, so it must be the only entry", AnyValue)
		case name != AnyValue && check != nil:
			if msg := check(name); msg != "" {
				l.bad(a, f, "%s", msg)
			}
		}
		seen[name] = true
	}
}

// buildSources validates `sources/` and returns what a connector consumes.
func (l *loader) buildSources() []connector.SourceConfig {
	out := make([]connector.SourceConfig, 0, len(l.sources))
	defined := make(map[string]string, len(l.sources))

	for i, d := range l.sources {
		src := d.v
		a := locate(d, "source", src.ID, i)

		switch {
		case src.ID == "":
			l.bad(a, "id", "is required: it is the id every event from this source carries")
		case !connector.ValidSourceID(src.ID):
			l.bad(a, "id", "%q is not a source id: 1 to %d bytes of lowercase letters, digits, - and _, starting with a letter or a digit", src.ID, connector.MaxSourceIDLen)
		case defined[src.ID] != "":
			l.bad(a, "id", "a source with id %q is already configured at %s", src.ID, defined[src.ID])
		default:
			defined[src.ID] = position(d.file, d.line)
		}

		switch {
		case src.Type == "":
			l.bad(a, "type", "is required: it selects the connector, such as github, discord or drive")
		case !isName(src.Type):
			l.bad(a, "type", "%q is not a connector type: lowercase letters, digits, - and _", src.Type)
		}

		if len(src.Containers) == 0 {
			l.bad(a, "containers", "is required: ingest is default deny, so a source with no containers ingests nothing. List the repositories, channels or folders, or %q for all of them", connector.AllowAll)
		}
		l.names(a, "containers", src.Containers, true, nil)

		cfg := connector.SourceConfig{
			ID:         src.ID,
			Type:       src.Type,
			Containers: slices.Clone(src.Containers),
			Secrets:    maps.Clone(src.Secrets),
		}

		if src.Refresh != "" {
			refresh, err := time.ParseDuration(src.Refresh)
			switch {
			case err != nil:
				l.bad(a, "refresh", "%q is not a duration: want something like 30s, 5m or 1h", src.Refresh)
			case refresh < 0:
				l.bad(a, "refresh", "%s is negative", src.Refresh)
			default:
				cfg.Refresh = refresh
			}
		}

		if len(src.Settings) > 0 {
			settings, err := json.Marshal(src.Settings)
			if err != nil {
				l.bad(a, "settings", "cannot be read as JSON, which is how a connector receives it: %v", err)
			} else {
				cfg.Settings = settings
			}
		}

		for _, name := range slices.Sorted(maps.Keys(src.Secrets)) {
			if !isEnvVarName(src.Secrets[name]) {
				l.bad(a, "secrets."+name, "%q is not the name of an environment variable: config names a secret and the runtime supplies its value, so this never holds the value itself", src.Secrets[name])
			}
		}

		out = append(out, cfg)
	}
	return out
}

// buildPrincipals validates `principals/`.
func (l *loader) buildPrincipals(r Repo) []Principal {
	out := make([]Principal, 0, len(l.principals))
	defined := make(map[string]string, len(l.principals))
	// identities maps a source-native identity to the principal that claimed
	// it, so that two principals cannot both be the same person in one source.
	identities := make(map[string]string)

	for i, d := range l.principals {
		p := d.v
		a := locate(d, "principal", p.ID, i)

		switch {
		case p.ID == "":
			l.bad(a, "id", "is required: it is what a stance's author, an owner and an authority policy all name")
		case !isName(p.ID):
			l.bad(a, "id", "%q is not a principal id: lowercase letters, digits, - and _, starting with a letter or a digit", p.ID)
		case defined[p.ID] != "":
			l.bad(a, "id", "a principal with id %q is already configured at %s", p.ID, defined[p.ID])
		default:
			defined[p.ID] = position(d.file, d.line)
		}

		// Most principals are people, so kind defaults to human; an agent says
		// so, because what it may do differs.
		kind := PrincipalKind(p.Kind)
		if p.Kind == "" {
			kind = PrincipalHuman
		}
		if kind != PrincipalHuman && kind != PrincipalAgent {
			l.bad(a, "kind", "%q is not a principal kind: want %s or %s", p.Kind, PrincipalHuman, PrincipalAgent)
		}

		class := AgentClass(p.Class)
		switch {
		case kind == PrincipalAgent && p.Class == "":
			l.bad(a, "class", "is required on an agent: it decides what the agent may read and write. Want one of %s", join(agentClasses))
		case kind != PrincipalAgent && p.Class != "":
			l.bad(a, "class", "is an agent's access class, and this principal is a %s", kind)
		case p.Class != "" && !slices.Contains(agentClasses, class):
			l.bad(a, "class", "%q is not an agent class: want one of %s", p.Class, join(agentClasses))
		}

		if len(p.Identities) == 0 {
			l.bad(a, "identities", "is required: a principal with no source identity is never matched to anything anyone said")
		}
		out = append(out, Principal{
			ID:         p.ID,
			Name:       p.Name,
			Kind:       kind,
			Class:      class,
			Identities: l.buildIdentities(r, a, p, identities),
		})
	}
	return out
}

// buildIdentities validates one principal's identities and records them so that
// a second principal claiming the same one is caught.
func (l *loader) buildIdentities(r Repo, a at, p principalDoc, identities map[string]string) []Identity {
	out := make([]Identity, 0, len(p.Identities))
	for j, id := range p.Identities {
		field := fmt.Sprintf("identities[%d]", j)
		if _, ok := r.Source(id.Source); id.Source == "" {
			l.bad(a, field+".source", "is required")
		} else if !ok {
			l.bad(a, field+".source", "no source is configured with id %q", id.Source)
		}
		if id.NativeID == "" && id.Handle == "" {
			l.bad(a, field, "needs a native_id or a handle: a native id survives a rename, a handle is what a person can type")
		}
		for _, claimed := range []struct{ key, value string }{{"native_id", id.NativeID}, {"handle", id.Handle}} {
			key, value := claimed.key, claimed.value
			if value == "" {
				continue
			}
			claim := id.Source + " " + key + " " + value
			if owner, taken := identities[claim]; taken && owner != p.ID {
				l.bad(a, field+"."+key, "%q in source %q is already principal %q: one identity is one person", value, id.Source, owner)
				continue
			}
			identities[claim] = p.ID
		}
		// The schema type and the parsed type carry the same three fields, so
		// the compiler checks this conversion: adding a field to one without
		// the other fails the build rather than dropping it silently.
		out = append(out, Identity(id))
	}
	return out
}

// buildCode validates `code/`.
func (l *loader) buildCode(r Repo) []CodeEntity {
	out := make([]CodeEntity, 0, len(l.code))
	defined := make(map[string]string, len(l.code))
	aliases := make(map[string]string)
	located := make(map[string]at, len(l.code))

	for i, d := range l.code {
		e := d.v
		a := locate(d, "code entity", e.ID, i)

		switch {
		case e.ID == "":
			l.bad(a, "id", "is required: it is what documents and stances reference")
		case !strings.HasPrefix(e.ID, "code:") || len(e.ID) == len("code:"):
			l.bad(a, "id", "%q is not a code entity id: they start with `code:`, as in code:acme/api:engine/server", e.ID)
		case len(e.ID) > maxEntityIDLen:
			l.bad(a, "id", "is %d bytes, and an entity id is at most %d", len(e.ID), maxEntityIDLen)
		case strings.ContainsFunc(e.ID, unicode.IsSpace):
			l.bad(a, "id", "%q contains whitespace", e.ID)
		case defined[e.ID] != "":
			l.bad(a, "id", "a code entity with id %q is already configured at %s", e.ID, defined[e.ID])
		default:
			defined[e.ID] = position(d.file, d.line)
		}
		located[e.ID] = a

		entityType := CodeEntityType(e.Type)
		switch {
		case e.Type == "":
			l.bad(a, "type", "is required: want one of %s", join(codeEntityTypes))
		case !slices.Contains(codeEntityTypes, entityType):
			l.bad(a, "type", "%q is not a code entity type: want one of %s", e.Type, join(codeEntityTypes))
		}

		l.names(a, "owners", e.Owners, false, func(id string) string {
			if _, ok := r.Principal(id); !ok {
				return fmt.Sprintf("no principal is configured with id %q", id)
			}
			return ""
		})
		l.names(a, "path_patterns", e.PathPatterns, false, nil)
		l.names(a, "part_of", e.PartOf, false, func(id string) string {
			if id == e.ID {
				return "an entity cannot be part of itself"
			}
			return ""
		})

		for j, alias := range e.Aliases {
			field := fmt.Sprintf("aliases[%d]", j)
			folded := foldAlias(alias)
			switch {
			case folded == "":
				l.bad(a, field, "is empty")
			case aliases[folded] == e.ID:
				l.bad(a, field, "%q is listed twice; aliases are matched ignoring case and surrounding space", alias)
			case aliases[folded] != "":
				l.bad(a, field, "%q is already an alias of %q: an alias that means two things resolves to neither", alias, aliases[folded])
			default:
				aliases[folded] = e.ID
			}
		}

		entity := CodeEntity{
			ID:           e.ID,
			Type:         entityType,
			Name:         e.Name,
			Aliases:      slices.Clone(e.Aliases),
			PathPatterns: slices.Clone(e.PathPatterns),
			PartOf:       slices.Clone(e.PartOf),
			Owners:       slices.Clone(e.Owners),
			CodeOwners:   e.CodeOwners,
		}
		if e.Repo != nil {
			entity.Repo = SourceRef{Source: e.Repo.Source, Project: e.Repo.Project}
			l.checkSourceRef(r, a, "repo", entity.Repo)
		}
		if len(e.PathPatterns) > 0 && e.Repo == nil {
			l.bad(a, "path_patterns", "need a repo to resolve against")
		}
		switch {
		case e.CodeOwners != "" && e.Repo == nil:
			l.bad(a, "codeowners", "needs a repo: it is a path within one")
		case e.CodeOwners != "" && !isRepoRelativePath(e.CodeOwners):
			l.bad(a, "codeowners", "%q is not a path inside the repository", e.CodeOwners)
		}
		out = append(out, entity)
	}

	l.checkPartOf(out, located)
	return out
}

// checkPartOf checks the entity hierarchy: every parent is configured, and the
// edges do not form a cycle. A cycle would make "the stances an entity inherits
// from its ancestors" a walk with no end (docs/design.md#l3-derived-views).
func (l *loader) checkPartOf(entities []CodeEntity, located map[string]at) {
	byID := make(map[string]CodeEntity, len(entities))
	for _, e := range entities {
		byID[e.ID] = e
	}

	// Depth-first, colouring: 1 is on the current path, 2 is finished and known
	// to reach no cycle.
	const (
		open = 1
		done = 2
	)
	colour := make(map[string]int, len(entities))
	var walk func(e CodeEntity, path []string)
	walk = func(e CodeEntity, path []string) {
		colour[e.ID] = open
		for i, parent := range e.PartOf {
			field := fmt.Sprintf("part_of[%d]", i)
			if parent == "" || parent == e.ID {
				// Both are already reported where the list is checked, and an
				// entity that is its own parent is a cycle of one, which would
				// otherwise be said twice.
				continue
			}
			up, ok := byID[parent]
			if !ok {
				l.bad(located[e.ID], field, "no code entity is configured with id %q", parent)
				continue
			}
			switch colour[parent] {
			case open:
				cycle := append(slices.Clone(path), e.ID, parent)
				l.bad(located[e.ID], field, "part_of is a cycle: %s", strings.Join(cycle, " -> "))
			case done:
			default:
				walk(up, append(slices.Clone(path), e.ID))
			}
		}
		colour[e.ID] = done
	}
	for _, e := range entities {
		if colour[e.ID] == 0 {
			walk(e, nil)
		}
	}
}

// buildScopes validates `scopes/`.
func (l *loader) buildScopes(r Repo) []Scope {
	out := make([]Scope, 0, len(l.scopes))
	defined := make(map[string]string, len(l.scopes))

	for i, d := range l.scopes {
		s := d.v
		a := locate(d, "scope", s.ID, i)

		switch {
		case s.ID == "":
			l.bad(a, "id", "is required: it is what a bundle request asks for")
		case !isName(s.ID):
			l.bad(a, "id", "%q is not a scope id: lowercase letters, digits, - and _, starting with a letter or a digit", s.ID)
		case defined[s.ID] != "":
			l.bad(a, "id", "a scope with id %q is already configured at %s", s.ID, defined[s.ID])
		default:
			defined[s.ID] = position(d.file, d.line)
		}

		scope := Scope{ID: s.ID, Name: s.Name, Entities: slices.Clone(s.Entities)}
		if len(s.Sources) == 0 {
			l.bad(a, "sources", "is required: a scope is a named bundle of sources")
		}
		listed := make(map[string]bool, len(s.Sources))
		for j, ss := range s.Sources {
			field := fmt.Sprintf("sources[%d]", j)
			src, known := r.Source(ss.Source)
			switch {
			case ss.Source == "":
				l.bad(a, field+".source", "is required")
			case listed[ss.Source]:
				l.bad(a, field+".source", "source %q is listed twice: put its containers in one entry", ss.Source)
			case !known:
				l.bad(a, field+".source", "no source is configured with id %q", ss.Source)
			}
			listed[ss.Source] = true
			containers := ss.Containers
			if len(containers) == 0 {
				// Naming no container takes whatever the source ingests, which
				// is what writing the source id on its own means.
				containers = []string{connector.AllowAll}
			}
			l.names(a, field+".containers", containers, true, func(c string) string {
				if !known || slices.Contains(src.Containers, connector.AllowAll) || slices.Contains(src.Containers, c) {
					return ""
				}
				return fmt.Sprintf("source %q does not ingest %q, so the scope would never see it", ss.Source, c)
			})
			scope.Sources = append(scope.Sources, ScopeSource{Source: ss.Source, Containers: containers})
		}

		l.names(a, "entities", s.Entities, false, func(id string) string {
			if _, ok := r.CodeEntity(id); !ok {
				return fmt.Sprintf("no code entity is configured with id %q", id)
			}
			return ""
		})

		if s.Tracker != nil {
			scope.Tracker = SourceRef{Source: s.Tracker.Source, Project: s.Tracker.Project}
			l.checkSourceRef(r, a, "tracker", scope.Tracker)
			if scope.Tracker.Source != "" && scope.Tracker.Project != "" && !scope.Covers(scope.Tracker.Source, scope.Tracker.Project) {
				l.bad(a, "tracker", "this scope does not cover %q in source %q, so it would have no tracker items to map",
					scope.Tracker.Project, scope.Tracker.Source)
			}
		}
		out = append(out, scope)
	}
	return out
}

// checkSourceRef checks a pointer into a source: the source is configured, and
// the project is one of the containers it ingests.
func (l *loader) checkSourceRef(r Repo, a at, field string, ref SourceRef) {
	src, known := r.Source(ref.Source)
	switch {
	case ref.Source == "":
		l.bad(a, field+".source", "is required")
	case !known:
		l.bad(a, field+".source", "no source is configured with id %q", ref.Source)
	}
	switch {
	case ref.Project == "":
		l.bad(a, field+".project", "is required: it is the repository or project inside the source, by native id")
	case known && !slices.Contains(src.Containers, connector.AllowAll) && !slices.Contains(src.Containers, ref.Project):
		l.bad(a, field+".project", "source %q does not ingest %q", ref.Source, ref.Project)
	}
}

// buildAuthority validates `authority/` and merges it onto [DefaultPolicy].
func (l *loader) buildAuthority(r Repo) Authority {
	policies := make([]Policy, 0, len(l.authority))
	defined := make(map[string]string, len(l.authority))

	for i, d := range l.authority {
		p := d.v
		a := locate(d, "authority policy", p.Scope, i)

		switch {
		case p.Scope == "":
			l.bad(a, "scope", "is required: name a scope, or %q for the policy every other scope inherits", AnyValue)
		case p.Scope != AnyValue && !isName(p.Scope):
			l.bad(a, "scope", "%q is not a scope id", p.Scope)
		case defined[p.Scope] != "":
			l.bad(a, "scope", "an authority policy for %q is already configured at %s: policies are not merged with each other, so two of them would be two answers", p.Scope, defined[p.Scope])
		default:
			defined[p.Scope] = position(d.file, d.line)
			if p.Scope != AnyValue {
				if _, ok := r.Scope(p.Scope); !ok {
					l.bad(a, "scope", "no scope is configured with id %q", p.Scope)
				}
			}
		}

		policy := Policy{Scope: p.Scope}
		if p.Ranking != nil {
			if len(p.Ranking) == 0 {
				l.bad(a, "ranking", "is empty: leave it out to inherit the default ranking, or list the artifact classes in authority order")
			}
			policy.Ranking = l.artifactClasses(a, "ranking", p.Ranking)
		}
		if p.RatifiedBy != nil {
			policy.RatifiedBy.Artifacts = l.artifactClasses(a, "ratified_by.artifacts", p.RatifiedBy.Artifacts)
			policy.RatifiedBy.Principals = slices.Clone(p.RatifiedBy.Principals)
			policy.RatifiedBy.Sources = slices.Clone(p.RatifiedBy.Sources)
			l.names(a, "ratified_by.principals", p.RatifiedBy.Principals, true, func(id string) string {
				if _, ok := r.Principal(id); !ok {
					return fmt.Sprintf("no principal is configured with id %q", id)
				}
				return ""
			})
			l.names(a, "ratified_by.sources", p.RatifiedBy.Sources, true, func(id string) string {
				if _, ok := r.Source(id); !ok {
					return fmt.Sprintf("no source is configured with id %q", id)
				}
				return ""
			})
		}
		policies = append(policies, policy)
	}
	return newAuthority(policies)
}

// artifactClasses converts a list of artifact class names, reporting the ones
// that are not classes. A nil list stays nil, because that is what tells the
// merge to inherit rather than to replace.
func (l *loader) artifactClasses(a at, field string, names []string) []ArtifactClass {
	if names == nil {
		return nil
	}
	out := make([]ArtifactClass, 0, len(names))
	l.names(a, field, names, false, func(name string) string {
		if !ArtifactClass(name).Valid() {
			return fmt.Sprintf("%q is not an artifact class: want one of %s", name, join(ArtifactClasses()))
		}
		return ""
	})
	for _, name := range names {
		out = append(out, ArtifactClass(name))
	}
	return out
}

// position renders where an object was written, for a message about a second
// one that collides with it.
func position(file string, line int) string {
	if line > 0 {
		return fmt.Sprintf("%s:%d", file, line)
	}
	return file
}

// join lists the values of a closed vocabulary for an error message.
func join[T ~string](values []T) string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = string(v)
	}
	return strings.Join(out, ", ")
}

// isName reports whether s is a scope id, a principal id or a connector type:
// the same shape as a source id (docs/connector-contract.md), so that one rule
// covers every name a person types into configuration and there is only one of
// them to keep in step.
func isName(s string) bool { return connector.ValidSourceID(s) }

// isEnvVarName reports whether s names an environment variable, which is what a
// secret in configuration holds. Rejecting anything else is what stops a token
// from being pasted into a repository that is checked in.
func isEnvVarName(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z':
		case (c >= '0' && c <= '9') || c == '_':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// isRepoRelativePath reports whether s is a path inside a repository: relative,
// and with nothing that climbs out of it.
func isRepoRelativePath(s string) bool {
	if s == "" || strings.HasPrefix(s, "/") || strings.ContainsFunc(s, unicode.IsSpace) {
		return false
	}
	for _, part := range strings.Split(s, "/") {
		if part == ".." {
			return false
		}
	}
	return true
}
