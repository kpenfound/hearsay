package config

import (
	"fmt"

	"go.yaml.in/yaml/v3"
)

// The types in this file are the on-disk schema: what a configuration file is
// allowed to say, one struct per kind of object, decoded with unknown fields
// rejected. They are deliberately separate from the types in repo.go, which are
// what the rest of Hearsay consumes: a duration is text here and a
// [time.Duration] there, and every check that turns one into the other is in
// validate.go, where it can report where in the file the problem is.

// sourceDoc is one entry of `sources/`.
type sourceDoc struct {
	ID         string            `yaml:"id"`
	Type       string            `yaml:"type"`
	Containers []string          `yaml:"containers"`
	Refresh    string            `yaml:"refresh"`
	Settings   map[string]any    `yaml:"settings"`
	Secrets    map[string]string `yaml:"secrets"`
}

// scopeDoc is one entry of `scopes/`.
type scopeDoc struct {
	ID       string           `yaml:"id"`
	Name     string           `yaml:"name"`
	Sources  []scopeSourceDoc `yaml:"sources"`
	Tracker  *sourceRefDoc    `yaml:"tracker"`
	Entities []string         `yaml:"entities"`
}

// scopeSourceDoc is one source a scope draws on. It is written either as the
// source id on its own, which takes every container that source ingests, or as
// a mapping naming the containers.
type scopeSourceDoc struct {
	Source     string   `yaml:"source"`
	Containers []string `yaml:"containers"`
}

// UnmarshalYAML accepts both forms.
//
// The decoder's own strictness does not reach a type that unmarshals itself, so
// the mapping's keys are checked here; without that, `containars:` in a scope
// would be dropped in silence, which is the one thing this format promises not
// to do. The error carries its own line number because yaml does not add one to
// what an unmarshaler returns.
func (s *scopeSourceDoc) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		return n.Decode(&s.Source)
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: want a source id or a mapping with source and containers, found %s", n.Line, nodeKind(n))
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		switch key := n.Content[i]; key.Value {
		case "source", "containers":
		default:
			return fmt.Errorf("line %d: no such field %q in a scope source", key.Line, key.Value)
		}
	}
	type plain scopeSourceDoc
	var p plain
	if err := n.Decode(&p); err != nil {
		return err
	}
	*s = scopeSourceDoc(p)
	return nil
}

// sourceRefDoc points at a project inside a source: a scope's tracker, a code
// entity's repository.
type sourceRefDoc struct {
	Source  string `yaml:"source"`
	Project string `yaml:"project"`
}

// principalDoc is one entry of `principals/`.
type principalDoc struct {
	ID         string        `yaml:"id"`
	Name       string        `yaml:"name"`
	Kind       string        `yaml:"kind"`
	Class      string        `yaml:"class"`
	Identities []identityDoc `yaml:"identities"`
	Members    []string      `yaml:"members"`
}

// identityDoc is one principal in one source.
type identityDoc struct {
	Source   string `yaml:"source"`
	NativeID string `yaml:"native_id"`
	Handle   string `yaml:"handle"`
}

// codeDoc is one entry of `code/`.
type codeDoc struct {
	ID           string        `yaml:"id"`
	Type         string        `yaml:"type"`
	Name         string        `yaml:"name"`
	Aliases      []string      `yaml:"aliases"`
	PathPatterns []string      `yaml:"path_patterns"`
	PartOf       []string      `yaml:"part_of"`
	Owners       []string      `yaml:"owners"`
	Repo         *sourceRefDoc `yaml:"repo"`
	CodeOwners   string        `yaml:"codeowners"`
}

// authorityDoc is one entry of `authority/`: the policy for one scope, or for
// every scope that has none of its own.
type authorityDoc struct {
	Scope      string        `yaml:"scope"`
	Ranking    []string      `yaml:"ranking"`
	RatifiedBy *ratifiersDoc `yaml:"ratified_by"`
}

// ratifiersDoc is what may ratify a stance. A list that is absent inherits from
// the default policy; a list that is present but empty inherits nothing, which
// is how a scope says that nobody may ratify by hand.
type ratifiersDoc struct {
	Principals []string `yaml:"principals"`
	Sources    []string `yaml:"sources"`
	Artifacts  []string `yaml:"artifacts"`
}

// llmDoc is `llm/`, or the `llm:` key of the single file: which provider and
// which model backs each of the three model tiers (ADR-0005). A tier the file
// does not name keeps the shipped default, so this section is what a team
// changes rather than what it has to write.
type llmDoc struct {
	Tiers map[string]llmTierDoc `yaml:"tiers"`
}

// llmTierDoc is one tier's entry. It is the on-disk form of an
// [llm.TierConfig]: durations are text here, and a field the file leaves out
// takes the default rather than the zero value.
type llmTierDoc struct {
	Provider    string   `yaml:"provider"`
	Model       string   `yaml:"model"`
	MaxTokens   int      `yaml:"max_tokens"`
	Temperature *float64 `yaml:"temperature"`
	Dimensions  int      `yaml:"dimensions"`
	BaseURL     string   `yaml:"base_url"`
	APIKeyEnv   string   `yaml:"api_key_env"`
	Timeout     string   `yaml:"timeout"`
	MaxRetries  int      `yaml:"max_retries"`
	Backoff     string   `yaml:"backoff"`

	// line is where this tier is written, so that a problem with it is
	// reported against the tier rather than against the section.
	line int
}

// UnmarshalYAML decodes one tier and remembers its line.
//
// The decoder's own strictness does not reach a type that unmarshals itself, so
// the keys are checked here: without that, `max_tokens:` misspelled inside a
// tier would be dropped in silence and the tier would quietly run on the
// default budget.
func (t *llmTierDoc) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: want a mapping of provider, model and the tier's parameters, found %s", n.Line, nodeKind(n))
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		switch key := n.Content[i]; key.Value {
		case "provider", "model", "max_tokens", "temperature", "dimensions",
			"base_url", "api_key_env", "timeout", "max_retries", "backoff":
		default:
			return fmt.Errorf("line %d: no such field %q in a model tier", key.Line, key.Value)
		}
	}
	type plain llmTierDoc
	var p plain
	if err := n.Decode(&p); err != nil {
		return err
	}
	*t = llmTierDoc(p)
	t.line = n.Line
	return nil
}
