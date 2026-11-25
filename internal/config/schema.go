package config

import (
	"fmt"

	"go.yaml.in/yaml/v3"
)

type sourceDoc struct {
	ID         string            `yaml:"id"`
	Type       string            `yaml:"type"`
	Containers []string          `yaml:"containers"`
	Refresh    string            `yaml:"refresh"`
	ReadOnly   any               `yaml:"read_only"`
	Settings   map[string]any    `yaml:"settings"`
	Secrets    map[string]string `yaml:"secrets"`
}

type scopeDoc struct {
	ID       string           `yaml:"id"`
	Name     string           `yaml:"name"`
	Sources  []scopeSourceDoc `yaml:"sources"`
	Tracker  *sourceRefDoc    `yaml:"tracker"`
	Entities []string         `yaml:"entities"`
}

type scopeSourceDoc struct {
	Source     string   `yaml:"source"`
	Containers []string `yaml:"containers"`
}

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

type sourceRefDoc struct {
	Source  string `yaml:"source"`
	Project string `yaml:"project"`
}

type principalDoc struct {
	ID         string        `yaml:"id"`
	Name       string        `yaml:"name"`
	Kind       string        `yaml:"kind"`
	Class      string        `yaml:"class"`
	TokenEnv   string        `yaml:"token_env"`
	Scopes     []string      `yaml:"scopes"`
	Identities []identityDoc `yaml:"identities"`
	Members    []string      `yaml:"members"`
}

type identityDoc struct {
	Source   string `yaml:"source"`
	NativeID string `yaml:"native_id"`
	Handle   string `yaml:"handle"`
}

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

type authorityDoc struct {
	Scope           string        `yaml:"scope"`
	Ranking         []string      `yaml:"ranking"`
	RatifiedBy      *ratifiersDoc `yaml:"ratified_by"`
	ContestedWindow string        `yaml:"contested_window"`
}

type ratifiersDoc struct {
	Principals []string `yaml:"principals"`
	Sources    []string `yaml:"sources"`
	Artifacts  []string `yaml:"artifacts"`
}

type llmDoc struct {
	Judgments *judgmentsDoc         `yaml:"judgments"`
	Tiers     map[string]llmTierDoc `yaml:"tiers"`
}

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

	line int
}

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

type judgmentsDoc struct {
	Model                  string   `yaml:"model"`
	BaseURL                string   `yaml:"base_url"`
	APIKeyEnv              string   `yaml:"api_key_env"`
	Threshold              *float64 `yaml:"threshold"`
	Timeout                string   `yaml:"timeout"`
	TotalTimeout           string   `yaml:"total_timeout"`
	MaxRetries             int      `yaml:"max_retries"`
	Backoff                string   `yaml:"backoff"`
	MaxTotalTokens         int      `yaml:"max_total_tokens"`
	MaxStateQuestionTokens int      `yaml:"max_state_question_tokens"`
}
