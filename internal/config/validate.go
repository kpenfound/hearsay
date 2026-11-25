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
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/llm/providers"
	"github.com/kpenfound/hearsay/internal/principal"
)

const maxEntityIDLen = 512

func (l *loader) build() Repo {
	if l.probs.any() {
		return Repo{}
	}

	var r Repo
	r.Sources = l.buildSources()
	r.Principals = l.buildPrincipals(r)
	r.Code = l.buildCode(r)
	r.Scopes = l.buildScopes(r)
	r.Authority = l.buildAuthority(r)
	r.LLM = l.buildLLM()
	r.Judgments = l.buildJudgments()

	if len(l.sources) == 0 {
		l.probs.add("", 0, "", "no sources are configured: Hearsay would ingest nothing")
	}
	if len(l.scopes) == 0 {
		l.probs.add("", 0, "", "no scopes are configured: Hearsay would serve no bundles")
	}
	return r
}

type at struct {
	file  string
	line  int
	noun  string
	label string
}

func locate[T any](d doc[T], noun, id string, i int) at {
	label := fmt.Sprintf("%s %d", noun, i+1)
	if id != "" {
		label = fmt.Sprintf("%s %q", noun, id)
	}
	return at{file: d.file, line: d.line, noun: noun, label: label}
}

func (l *loader) claimID(a at, id, wrong string, defined map[string]string) bool {
	switch {
	case wrong != "":
		l.bad(a, "id", "%s", wrong)
		return false
	case defined[id] != "":
		l.bad(a, "id", "a %s with id %q is already configured at %s", a.noun, id, defined[id])
	default:
		defined[id] = position(a.file, a.line)
	}
	return true
}

func (l *loader) bad(a at, field, format string, args ...any) {
	where := a.label
	if field != "" {
		where += ": " + field
	}
	l.probs.add(a.file, a.line, where, format, args...)
}

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

func (l *loader) buildSources() []connector.SourceConfig {
	out := make([]connector.SourceConfig, 0, len(l.sources))
	defined := make(map[string]string, len(l.sources))

	for i, d := range l.sources {
		src := d.v
		a := locate(d, "source", src.ID, i)

		var wrong string
		switch {
		case src.ID == "":
			wrong = "is required: it is the id every event from this source carries"
		case src.ID == "hearsay":
			wrong = `"hearsay" is reserved for Hearsay's own audit and assertion events`
		case !connector.ValidSourceID(src.ID):
			wrong = fmt.Sprintf("%q is not a source id: 1 to %d bytes of lowercase letters, digits, - and _, starting with a letter or a digit", src.ID, connector.MaxSourceIDLen)
		}
		keep := l.claimID(a, src.ID, wrong, defined)

		switch {
		case src.Type == "":
			l.bad(a, "type", "is required: it selects the connector, such as github, discord, slack, drive, obsidian or agent")
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

		switch ro := src.ReadOnly.(type) {
		case nil:
		case bool:
			cfg.ReadOnly = ro
		case string:
			l.bad(a, "read_only", "%q is not a boolean: want true or false", ro)
		default:
			l.bad(a, "read_only", "%v is not a boolean: want true or false", ro)
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

		if keep {
			out = append(out, cfg)
		}
	}
	l.checkOwnerSources()
	return out
}

func (l *loader) checkOwnerSources() {
	type entry struct {
		a     at
		src   sourceDoc
		named string
	}
	byID := map[string]*entry{}
	entries := make([]*entry, 0, len(l.sources))
	for i, d := range l.sources {
		e := &entry{a: locate(d, "source", d.v.ID, i), src: d.v}
		if raw, present := d.v.Settings["owner_of"]; present && d.v.Type == "work" {
			if name, ok := raw.(string); !ok || name == "" {
				l.bad(e.a, "settings.owner_of", "%v is not a source id: name the work source this one holds documents and decisions for, or leave it out", raw)
			} else {
				e.named = name
			}
		}
		entries = append(entries, e)
		if _, dup := byID[d.v.ID]; !dup {
			byID[d.v.ID] = e
		}
	}
	for _, e := range entries {
		if e.named == "" {
			continue
		}
		target, ok := byID[e.named]
		switch {
		case e.named == e.src.ID:
			l.bad(e.a, "settings.owner_of", "names this source itself: an owner source holds records for another work source")
		case !ok:
			l.bad(e.a, "settings.owner_of", "names source %q, which is not configured", e.named)
		case target.src.Type != "work":
			l.bad(e.a, "settings.owner_of", "names source %q, which is of type %q: an owner source's records are about a work source's projects", e.named, target.src.Type)
		case target.named != "":
			l.bad(e.a, "settings.owner_of", "names source %q, which is itself an owner source: the work source it names must hold the projects", e.named)
		}
	}
}

func (l *loader) buildPrincipals(r Repo) []principal.Principal {
	out := make([]principal.Principal, 0, len(l.principals))
	defined := make(map[string]string, len(l.principals))
	identities := make(map[string]string)
	type team struct {
		a at
		p principalDoc
	}
	var teams []team

	for i, d := range l.principals {
		p := d.v
		a := locate(d, "principal", p.ID, i)

		var wrong string
		switch {
		case p.ID == "":
			wrong = "is required: it is what a stance's author, an owner and an authority policy all name"
		case !principal.ValidID(p.ID):
			wrong = fmt.Sprintf("%q is not a principal id: lowercase letters, digits, - and _, starting with a letter or a digit", p.ID)
		}
		keep := l.claimID(a, p.ID, wrong, defined)

		kind := principal.Kind(p.Kind)
		if p.Kind == "" {
			kind = principal.KindHuman
		}
		class := principal.Class(p.Class)
		if !kind.Valid() {
			l.bad(a, "kind", "%q is not a principal kind: want one of %s", p.Kind, join(principal.Kinds()))
		} else {
			if kind == principal.KindTeam && p.Scopes != nil {
				l.bad(a, "scopes", "a team cannot have scopes: only a human or agent acts")
			}
			if kind == principal.KindAgent && len(p.Scopes) == 0 {
				l.bad(a, "scopes", "is required on agent %q: list its entity ids or %q", p.ID, AnyValue)
			}
			if kind != principal.KindTeam {
				l.names(a, "scopes", p.Scopes, true, validGrantScope)
			}
			if p.TokenEnv != "" && !isEnvVarName(p.TokenEnv) {
				l.bad(a, "token_env", "%q is not the name of an environment variable: config names a secret and the API supplies its value", p.TokenEnv)
			}
			if kind == principal.KindTeam && p.TokenEnv != "" {
				l.bad(a, "token_env", "a team cannot call the API, so it cannot have a token")
			}
			switch {
			case kind == principal.KindAgent && p.Class == "":
				l.bad(a, "class", "is required on an agent: it decides what the agent may read and write. Want one of %s", join(principal.Classes()))
			case kind != principal.KindAgent && p.Class != "":
				l.bad(a, "class", "is an agent's access class, and this principal is a %s", kind)
			case p.Class != "" && !class.Valid():
				l.bad(a, "class", "%q is not an agent class: want one of %s", p.Class, join(principal.Classes()))
			}

			if kind == principal.KindTeam {
				if len(p.Identities) == 0 && len(p.Members) == 0 {
					l.bad(a, "members", "is required on a team with no identities: a team that neither lists its people nor names a group in a source stands for nobody")
				}
				teams = append(teams, team{a, p})
			} else {
				if len(p.Identities) == 0 && !slices.ContainsFunc(r.Sources, func(src connector.SourceConfig) bool { return src.Type == "work" }) {
					l.bad(a, "identities", "is required without a work source: a principal needs a source identity or a work source that matches by principal id")
				}
				if len(p.Members) > 0 {
					l.bad(a, "members", "is a team's membership, and this principal is a %s", kind)
				}
			}
		}

		built := principal.Principal{
			ID:         p.ID,
			Name:       p.Name,
			Kind:       kind,
			Class:      class,
			TokenEnv:   p.TokenEnv,
			Identities: l.buildIdentities(r, a, p, identities),
			Members:    slices.Clone(p.Members),
		}
		if kind == principal.KindHuman && p.Scopes == nil || slices.Equal(p.Scopes, []string{AnyValue}) {
			built.Grant.Scopes = principal.AllScopes()
		} else if kind != principal.KindTeam {
			built.Grant.Scopes = principal.SomeScopes(p.Scopes...)
		}
		if keep {
			out = append(out, built)
		}
	}

	kept := make(map[string]principal.Principal, len(out))
	for _, p := range out {
		kept[p.ID] = p
	}
	for _, t := range teams {
		l.names(t.a, "members", t.p.Members, false, func(id string) string {
			member, ok := kept[id]
			switch {
			case id == t.p.ID:
				return "a team cannot be a member of itself"
			case !ok:
				return fmt.Sprintf("no principal is configured with id %q", id)
			case member.Kind == principal.KindTeam:
				return fmt.Sprintf("%q is a team, and a team may not contain a team: list its people and agents", id)
			}
			return ""
		})
	}
	return out
}

func validGrantScope(id string) string {
	valid := false
	if code, ok := strings.CutPrefix(id, "code:"); ok {
		valid = code != ""
	} else if tracker, ok := strings.CutPrefix(id, "tracker:"); ok {
		source, rest, hasSource := strings.Cut(tracker, ":")
		project, item, hasItem := strings.Cut(rest, "#")
		valid = hasSource && hasItem && connector.ValidSourceID(source) && project != "" && item != "" && !strings.Contains(item, "#")
	}
	if work, ok := strings.CutPrefix(id, "work:"); ok {
		source, artifact, hasSource := strings.Cut(work, ":")
		project, stream, hasStream := strings.Cut(artifact, "#")
		valid = hasSource && connector.ValidSourceID(source) && validWorkGrantID(project) && (!hasStream || validWorkGrantID(stream))
	}
	if !valid || len(id) > maxEntityIDLen || strings.ContainsFunc(id, unicode.IsSpace) {
		return fmt.Sprintf("%q is not an entity id: want code:<id>, tracker:<source>:<project>#<item> or work:<source>:<project>[#<workstream>], at most %d bytes and no whitespace", id, maxEntityIDLen)
	}
	return ""
}

func validWorkGrantID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for i := range len(id) {
		b := id[i]
		switch {
		case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		case b == '-', b == '_', b == '.', b == '/':
		default:
			return false
		}
	}
	return true
}

func (l *loader) buildIdentities(r Repo, a at, p principalDoc, identities map[string]string) []principal.Identity {
	out := make([]principal.Identity, 0, len(p.Identities))
	for j, id := range p.Identities {
		field := fmt.Sprintf("identities[%d]", j)
		if _, ok := r.Source(id.Source); id.Source == "" {
			l.bad(a, field+".source", "is required")
		} else if !ok {
			l.bad(a, field+".source", "no source is configured with id %q", id.Source)
		}
		if id.NativeID == "" && principal.FoldHandle(id.Handle) == "" {
			l.bad(a, field, "needs a native_id or a handle: a native id survives a rename, a handle is what a person can type")
		}
		for _, c := range []struct{ key, written, matched, note string }{
			{"native_id", id.NativeID, id.NativeID, ""},
			{"handle", id.Handle, principal.FoldHandle(id.Handle), ", and handles are matched ignoring case"},
		} {
			if c.matched == "" {
				continue
			}
			claim := id.Source + " " + c.key + " " + c.matched
			if owner, taken := identities[claim]; taken && owner != p.ID {
				l.bad(a, field+"."+c.key, "%q in source %q is already principal %q: one identity is one person%s",
					c.written, id.Source, owner, c.note)
				continue
			}
			identities[claim] = p.ID
		}
		out = append(out, principal.Identity(id))
	}
	return out
}

func (l *loader) buildCode(r Repo) []CodeEntity {
	out := make([]CodeEntity, 0, len(l.code))
	all := make([]CodeEntity, 0, len(l.code))
	defined := make(map[string]string, len(l.code))
	aliases := make(map[string]string)
	located := make(map[string]at, len(l.code))

	for i, d := range l.code {
		e := d.v
		a := locate(d, "code entity", e.ID, i)

		var wrong string
		switch {
		case e.ID == "":
			wrong = "is required: it is what documents and stances reference"
		case !strings.HasPrefix(e.ID, "code:") || len(e.ID) == len("code:"):
			wrong = fmt.Sprintf("%q is not a code entity id: they start with `code:`, as in code:acme/api:engine/server", e.ID)
		case len(e.ID) > maxEntityIDLen:
			wrong = fmt.Sprintf("is %d bytes, and an entity id is at most %d", len(e.ID), maxEntityIDLen)
		case strings.ContainsFunc(e.ID, unicode.IsSpace):
			wrong = fmt.Sprintf("%q contains whitespace", e.ID)
		}
		keep := l.claimID(a, e.ID, wrong, defined)
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
		all = append(all, entity)
		if keep {
			out = append(out, entity)
		}
	}

	l.checkPartOf(all, out, located)
	return out
}

func (l *loader) checkPartOf(all, kept []CodeEntity, located map[string]at) {
	byID := make(map[string]CodeEntity, len(kept))
	for _, e := range kept {
		byID[e.ID] = e
	}

	const (
		open = 1
		done = 2
	)
	colour := make(map[string]int, len(kept))
	var walk func(e CodeEntity, path []string)
	walk = func(e CodeEntity, path []string) {
		colour[e.ID] = open
		for i, parent := range e.PartOf {
			field := fmt.Sprintf("part_of[%d]", i)
			if parent == "" || parent == e.ID {
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
	for _, e := range kept {
		if colour[e.ID] == 0 {
			walk(e, nil)
		}
	}
	for _, e := range all {
		if _, ok := byID[e.ID]; !ok {
			walk(e, nil)
		}
	}
}

func (l *loader) buildScopes(r Repo) []Scope {
	out := make([]Scope, 0, len(l.scopes))
	defined := make(map[string]string, len(l.scopes))

	for i, d := range l.scopes {
		s := d.v
		a := locate(d, "scope", s.ID, i)

		var wrong string
		switch {
		case s.ID == "":
			wrong = "is required: it is what a bundle request asks for"
		case !isName(s.ID):
			wrong = fmt.Sprintf("%q is not a scope id: lowercase letters, digits, - and _, starting with a letter or a digit", s.ID)
		}
		keep := l.claimID(a, s.ID, wrong, defined)

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
		if keep {
			out = append(out, scope)
		}
	}
	return out
}

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

func (l *loader) buildAuthority(r Repo) Authority {
	policies := make([]Policy, 0, len(l.authority))
	defined := make(map[string]string, len(l.authority))
	type policyAt struct {
		scope string
		at    at
	}
	var merged []policyAt
	ambiguous := make(map[string]bool)

	for i, d := range l.authority {
		p := d.v
		a := locate(d, "authority policy", p.Scope, i)

		named := false
		switch {
		case p.Scope == "":
			l.bad(a, "scope", "is required: name a scope, or %q for the policy every other scope inherits", AnyValue)
		case p.Scope != AnyValue && !isName(p.Scope):
			l.bad(a, "scope", "%q is not a scope id", p.Scope)
		case defined[p.Scope] != "":
			l.bad(a, "scope", "an authority policy for %q is already configured at %s: policies are not merged with each other, so two of them would be two answers", p.Scope, defined[p.Scope])
			ambiguous[p.Scope] = true
		default:
			defined[p.Scope] = position(d.file, d.line)
			named = true
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
		if p.ContestedWindow != "" {
			policy.ContestedWindow = l.contestedWindow(a, p.ContestedWindow)
		}
		policies = append(policies, policy)
		if named && (policy.Ranking != nil || policy.RatifiedBy.Artifacts != nil) {
			merged = append(merged, policyAt{scope: p.Scope, at: a})
		}
	}

	auth := newAuthority(policies)
	for _, m := range merged {
		if ambiguous[m.scope] {
			continue
		}
		p := auth.Default()
		if m.scope != AnyValue {
			p = auth.ForScope(m.scope)
		}
		l.checkRatifiersAreRanked(m.at, p)
	}
	return auth
}

func (l *loader) contestedWindow(a at, written string) time.Duration {
	d, err := time.ParseDuration(written)
	switch {
	case err != nil:
		l.bad(a, "contested_window", "%q is not a duration: want something like 336h for 14 days", written)
		return 0
	case d <= 0:
		l.bad(a, "contested_window", "%s is not positive: leave it out to inherit the window, or give one longer than nothing", written)
		return 0
	}
	return d
}

func (l *loader) checkRatifiersAreRanked(a at, p Policy) {
	if len(p.Ranking) == 0 {
		return
	}
	for _, c := range p.RatifiedBy.Artifacts {
		if !c.Valid() || slices.Contains(p.Ranking, c) {
			continue
		}
		l.bad(a, "ratified_by.artifacts", "%q ratifies on its own, and the ranking in force for this scope leaves it out, so a stance it ratifies is outranked by every class the ranking does list. Rank it, or take it out of ratified_by.artifacts", c)
	}
}

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
		if c := ArtifactClass(name); c.Valid() {
			out = append(out, c)
		}
	}
	return out
}

func position(file string, line int) string {
	if line > 0 {
		return fmt.Sprintf("%s:%d", file, line)
	}
	return file
}

func join[T ~string](values []T) string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = string(v)
	}
	return strings.Join(out, ", ")
}

func isName(s string) bool { return connector.ValidSourceID(s) }

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

func (l *loader) buildLLM() llm.Config {
	cfg := llm.Default().Clone()
	defined := map[string]string{}
	for _, d := range l.llm {
		a := at{file: d.file, line: d.line, noun: "llm section", label: "llm"}
		if len(d.v.Tiers) == 0 && d.v.Judgments == nil {
			l.bad(a, "tiers", "no model tiers are configured here: remove the section to take the defaults, or name a tier")
			continue
		}
		for _, name := range slices.Sorted(maps.Keys(d.v.Tiers)) {
			l.buildTier(d, name, d.v.Tiers[name], cfg, defined)
		}
	}
	return cfg
}

func (l *loader) buildTier(d doc[llmDoc], name string, td llmTierDoc, cfg llm.Config, defined map[string]string) {
	line := td.line
	if line == 0 {
		line = d.line
	}
	a := at{file: d.file, line: line, noun: "model tier", label: fmt.Sprintf("model tier %q", name)}

	tier, err := llm.ParseTier(name)
	if err != nil {
		l.bad(a, "", "%s", err)
		return
	}
	if where := defined[name]; where != "" {
		l.bad(a, "", "the %s tier is already configured at %s", name, where)
		return
	}
	defined[name] = position(a.file, a.line)

	tc := cfg.Tiers[tier]
	if td.Provider != "" {
		tc.Provider = td.Provider
	}
	if td.Model != "" {
		tc.Model = td.Model
	}
	if td.MaxTokens != 0 {
		tc.MaxTokens = td.MaxTokens
	}
	if td.Temperature != nil {
		tc.Temperature = td.Temperature
	}
	if td.Dimensions != 0 {
		tc.Dimensions = td.Dimensions
	}
	if td.BaseURL != "" {
		tc.BaseURL = td.BaseURL
	}
	if td.APIKeyEnv != "" {
		tc.APIKeyEnv = td.APIKeyEnv
	}
	if td.MaxRetries != 0 {
		tc.MaxRetries = td.MaxRetries
	}
	tc.Timeout = l.duration(a, "timeout", td.Timeout, tc.Timeout)
	tc.Backoff = l.duration(a, "backoff", td.Backoff, tc.Backoff)
	cfg.Tiers[tier] = tc

	if err := tc.Validate(tier); err != nil {
		l.bad(a, "", "%s", err)
		return
	}
	provider, ok := providers.ByName(tc.Provider)
	if !ok {
		l.bad(a, "provider", "no such provider %q: this build has adapters for %s",
			tc.Provider, strings.Join(providers.Names(), ", "))
		return
	}
	if err := llm.CanServe(provider.Name(), provider.Capabilities(), tier, tc); err != nil {
		l.bad(a, "provider", "%s", err)
	}
}

func (l *loader) duration(a at, field, written string, fallback time.Duration) time.Duration {
	if written == "" {
		return fallback
	}
	d, err := time.ParseDuration(written)
	switch {
	case err != nil:
		l.bad(a, field, "%q is not a duration: want something like 30s, 5m or 1h", written)
		return fallback
	case d < 0:
		l.bad(a, field, "%s is negative", written)
		return fallback
	}
	return d
}
