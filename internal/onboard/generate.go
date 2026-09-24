package onboard

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/llm"
)

// TokenBytes is how much randomness an API token carries. It is written as
// hex, so a token is twice as many characters.
const TokenBytes = 32

// Result is what [Generate] wrote.
type Result struct {
	// Config is the single-file configuration, `hearsay.yaml`.
	Config []byte
	// Env is the env file: the generated API tokens, and every other
	// variable the configuration names, empty.
	Env []byte
	// Principals are the ids of the principals written, the operator first.
	Principals []string
	// Tokens are the variables the env file holds a generated token in.
	Tokens []string
	// Empty are the variables the env file leaves for the person to fill in.
	Empty []string
	// Notes say what the sources were asked and was left out, and why.
	Notes []string
}

// Generate writes the configuration and the env file for the input. random is
// where the API tokens come from, crypto/rand's Reader outside a test. It
// reads the sources through dir and nothing else, and it checks nothing the
// loader checks: the caller validates what it wrote with [config.Load].
func Generate(ctx context.Context, in Input, dir Directory, random io.Reader) (Result, error) {
	if err := in.Check(); err != nil {
		return Result{}, err
	}
	all, notes, err := people(ctx, in, dir)
	if err != nil {
		return Result{}, err
	}
	sources := in.sources()

	taken := map[string]bool{}
	for _, s := range sources {
		for _, p := range s.secrets {
			taken[p.value] = true
		}
	}
	taken[EnvAnthropicAPIKey] = true
	tokens := make([]pair, 0, len(all))
	envs := make(map[string]string, len(all))
	for _, p := range all {
		name := tokenEnv(p.id, taken)
		raw := make([]byte, TokenBytes)
		if _, err := io.ReadFull(random, raw); err != nil {
			return Result{}, fmt.Errorf("generating an API token: %w", err)
		}
		envs[p.id] = name
		tokens = append(tokens, pair{name, hex.EncodeToString(raw)})
	}

	res := Result{Notes: notes}
	for _, p := range all {
		res.Principals = append(res.Principals, p.id)
	}
	for _, t := range tokens {
		res.Tokens = append(res.Tokens, t.key)
	}
	res.Config, err = renderConfig(in, sources, all, envs)
	if err != nil {
		return Result{}, err
	}
	res.Env, res.Empty = renderEnv(sources, tokens)
	return res, nil
}

// tokenEnv names the variable a principal's API token is in, and claims it:
// HEARSAY_<ID>_TOKEN, numbered when that is already a name the file uses.
func tokenEnv(id string, taken map[string]bool) string {
	base := "HEARSAY_" + strings.ToUpper(strings.ReplaceAll(id, "-", "_"))
	name := base + "_TOKEN"
	for n := 2; taken[name]; n++ {
		name = base + "_" + strconv.Itoa(n) + "_TOKEN"
	}
	taken[name] = true
	return name
}

var notScopeID = regexp.MustCompile(`[^a-z0-9_-]+`)

// scopeIDs names a scope for each repository: its name, or owner-name where
// two repositories share a name, in the shape a scope id takes.
func scopeIDs(repos []string) []string {
	clean := func(s string) string {
		s = strings.Trim(notScopeID.ReplaceAllString(strings.ToLower(s), "-"), "-_")
		if len(s) > 64 {
			s = strings.TrimRight(s[:64], "-_")
		}
		return s
	}
	names := make([]string, len(repos))
	count := map[string]int{}
	for i, r := range repos {
		_, name, _ := strings.Cut(r, "/")
		names[i] = clean(name)
		count[names[i]]++
	}
	out := make([]string, len(repos))
	used := map[string]bool{}
	for i, r := range repos {
		id := names[i]
		if count[id] > 1 || id == "" {
			id = clean(strings.ReplaceAll(r, "/", "-"))
		}
		if id == "" {
			id = "repo"
		}
		base := id
		for n := 2; used[id]; n++ {
			id = fmt.Sprintf("%s-%d", base, n)
		}
		used[id] = true
		out[i] = id
	}
	return out
}

// TeamScope is the scope a configuration without GitHub gets: there is no
// repository to name one after, and a configuration needs one.
const TeamScope = "team"

func renderConfig(in Input, sources []source, all []*person, envs map[string]string) ([]byte, error) {
	var top []*yaml.Node

	var srcs []*yaml.Node
	for _, s := range sources {
		entry := []*yaml.Node{key("id"), str(s.id), key("type"), str(s.typ), key("containers"), flow(s.containers...)}
		if len(s.settings) > 0 {
			var settings []*yaml.Node
			for _, p := range s.settings {
				settings = append(settings, key(p.key), str(p.value))
			}
			entry = append(entry, key("settings"), mapping(settings...))
		}
		var secrets []*yaml.Node
		for _, p := range s.secrets {
			secrets = append(secrets, key(p.key), str(p.value))
		}
		entry = append(entry, key("secrets"), mapping(secrets...))
		srcs = append(srcs, mapping(entry...))
	}
	top = append(top, comment(key("sources"),
		"What Hearsay ingests. `containers` is the allowlist: nothing else in a source is read.",
		"Each secret is the name of an environment variable; the env file beside this one holds the values.",
		"docs/config.md has each source's optional settings, and what its credential needs to be allowed to do."),
		seq(srcs...))

	var scopes, code []*yaml.Node
	if in.HasGitHub() {
		ids := scopeIDs(in.GitHub.Repos)
		for i, repo := range in.GitHub.Repos {
			entity := "code:" + repo
			scopeSources := []*yaml.Node{mapping(key("source"), str(SourceGitHub), key("containers"), flow(repo))}
			for _, s := range sources[1:] {
				scopeSources = append(scopeSources, str(s.id))
			}
			scopes = append(scopes, mapping(
				key("id"), str(ids[i]),
				key("name"), str(repo),
				key("sources"), seq(scopeSources...),
				key("tracker"), mapping(key("source"), str(SourceGitHub), key("project"), str(repo)),
				key("entities"), flow(entity),
			))
			_, name, _ := strings.Cut(repo, "/")
			code = append(code, mapping(
				key("id"), str(entity),
				key("type"), str(string(config.TypeProject)),
				key("name"), str(name),
				key("repo"), mapping(key("source"), str(SourceGitHub), key("project"), str(repo)),
			))
		}
		top = append(top, comment(key("scopes"),
			"One scope per repository: its tracker is the repository's issues, and it is about the repository's code.",
			"Every scope takes all of Discord and Drive; narrow a scope to the channels and folders about it with",
			"`- source: discord` and `containers: [...]`."),
			seq(scopes...))
	} else {
		var scopeSources []*yaml.Node
		for _, s := range sources {
			scopeSources = append(scopeSources, str(s.id))
		}
		scopes = append(scopes, mapping(key("id"), str(TeamScope), key("name"), str("Team"), key("sources"), seq(scopeSources...)))
		top = append(top, comment(key("scopes"),
			"One scope over every source. With no repository there is nothing to split it by; add scopes as the team's work divides."),
			seq(scopes...))
	}

	var principals []*yaml.Node
	for _, p := range all {
		entry := []*yaml.Node{key("id"), str(p.id)}
		if p.name != "" {
			entry = append(entry, key("name"), str(p.name))
		}
		entry = append(entry, key("token_env"), str(envs[p.id]))
		var idents []*yaml.Node
		for _, id := range p.identities() {
			fields := []*yaml.Node{key("source"), str(id.Source)}
			if id.NativeID != "" {
				fields = append(fields, key("native_id"), str(id.NativeID))
			}
			if id.Handle != "" {
				fields = append(fields, key("handle"), str(id.Handle))
			}
			idents = append(idents, mapping(fields...))
		}
		entry = append(entry, key("identities"), seq(idents...))
		principals = append(principals, mapping(entry...))
	}
	principalsNote := []string{
		"The people on the team. Each calls the API with the token in its token_env, and with no `scopes:` reads every scope.",
		"A Discord or Drive identity is written only where the sources confirmed it; add the rest by hand, per source.",
	}
	if len(all) == 1 {
		principalsNote = []string{
			"You, as you described yourself. Nothing was read from GitHub, so nobody else is here yet:",
			"add each person with an identity per source they appear in, and a token_env.",
		}
	}
	top = append(top, comment(key("principals"), principalsNote...), seq(principals...))

	if len(code) > 0 {
		top = append(top, comment(key("code"),
			"Each repository as a project. The assertion worker adds its top-level directories as modules at startup."),
			seq(code...))
	}

	def := config.DefaultPolicy()
	ranking := make([]string, 0, len(def.Ranking))
	for _, c := range def.Ranking {
		ranking = append(ranking, string(c))
	}
	artifacts := make([]string, 0, len(def.RatifiedBy.Artifacts))
	for _, c := range def.RatifiedBy.Artifacts {
		artifacts = append(artifacts, string(c))
	}
	top = append(top, comment(key("authority"),
		"The built-in default, written out so it can be read here: with no authority at all, this is what applies.",
		"`principals: [\"*\"]` lets anyone who can write to a scope ratify by hand; name people to narrow it."),
		seq(mapping(
			key("scope"), str(config.AnyValue),
			key("ranking"), flow(ranking...),
			key("ratified_by"), mapping(
				key("principals"), flow(def.RatifiedBy.Principals...),
				key("sources"), flow(def.RatifiedBy.Sources...),
				key("artifacts"), flow(artifacts...),
			),
			key("contested_window"), str(hours(def.ContestedWindow)),
		)))

	defaults := llm.Default()
	var tiers []*yaml.Node
	for _, t := range llm.CompletionTiers {
		tc, _ := defaults.Tier(t)
		tiers = append(tiers, key(string(t)), mapping(
			key("provider"), str(tc.Provider),
			key("model"), str(tc.Model),
			key("max_tokens"), &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: strconv.Itoa(tc.MaxTokens)},
		))
	}
	top = append(top, comment(key("llm"),
		"The shipped defaults for the two completion tiers, written out; "+EnvAnthropicAPIKey+" is their credential.",
		"There is no embed tier: this build ships no embedding provider, and search runs on full text until one is named."),
		mapping(key("tiers"), mapping(tiers...)))

	// Each section is encoded on its own and the sections joined with a blank
	// line, which the encoder does not put between mapping keys.
	var buf bytes.Buffer
	buf.WriteString("# Hearsay configuration, written by `hearsay init`. The format is docs/config.md.\n" +
		"# Check a change with `hearsay config validate <this file>`. No secret is in here: every\n" +
		"# `secrets:` value and `token_env:` names an environment variable.\n")
	for i := 0; i+1 < len(top); i += 2 {
		buf.WriteString("\n")
		enc := yaml.NewEncoder(&buf)
		enc.SetIndent(2)
		if err := enc.Encode(mapping(top[i], top[i+1])); err != nil {
			return nil, fmt.Errorf("encoding the configuration's %s: %w", top[i].Value, err)
		}
		if err := enc.Close(); err != nil {
			return nil, fmt.Errorf("encoding the configuration's %s: %w", top[i].Value, err)
		}
	}
	return buf.Bytes(), nil
}

// hours writes a duration the way docs/config.md does, in whole hours.
func hours(d time.Duration) string {
	if d%time.Hour == 0 {
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	}
	return d.String()
}

func key(k string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Value: k} }

// str is a string scalar, which the encoder quotes where YAML would read it as
// anything else: a Discord id stays a string, and `*` is not an alias.
func str(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v} }

func mapping(kv ...*yaml.Node) *yaml.Node { return &yaml.Node{Kind: yaml.MappingNode, Content: kv} }

func seq(items ...*yaml.Node) *yaml.Node { return &yaml.Node{Kind: yaml.SequenceNode, Content: items} }

func flow(items ...string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
	for _, v := range slices.Clone(items) {
		n.Content = append(n.Content, str(v))
	}
	return n
}

func comment(n *yaml.Node, lines ...string) *yaml.Node {
	n.HeadComment = strings.Join(lines, "\n")
	return n
}
