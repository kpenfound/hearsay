package work

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/push"
	"github.com/kpenfound/hearsay/internal/principal"
)

// Type is the connector registry key.
const Type = "work"

// Settings controls posting and trusted attribution.
type Settings struct {
	Posters     []string `json:"posters"`
	Attributors []string `json:"attributors"`
}

// Connector accepts work artifacts and reconciles their access lists.
type Connector struct {
	source      string
	sources     map[string]bool
	principals  map[string]principal.Principal
	posters     map[string]bool
	attributors map[string]bool
	tokens      push.Tokens
	excluded    push.Tokens
	projects    sync.Map
	last        atomic.Int64
}

var (
	_ connector.Pusher = (*Connector)(nil)
	_ connector.Poller = (*Connector)(nil)
)

// NewFactory binds configured principals and all configured source ids. lookup
// reads token_env; nil uses the process environment.
func NewFactory(principals []principal.Principal, sources []string, lookup func(string) (string, bool)) connector.Factory {
	return func(_ context.Context, src connector.SourceConfig) (connector.Connector, error) {
		return New(src, principals, sources, lookup)
	}
}

// New checks source policy and loads poster credentials before requests arrive.
func New(src connector.SourceConfig, principals []principal.Principal, sources []string, lookup func(string) (string, bool)) (*Connector, error) {
	settings := Settings{Posters: []string{"*"}}
	if err := src.DecodeSettings(&settings); err != nil {
		return nil, err
	}
	if !slices.Equal(src.Containers, []string{"*"}) {
		return nil, errors.New(`containers must be exactly ["*"]`)
	}
	if len(src.Secrets) != 0 {
		return nil, errors.New("work takes no secrets: posters use their principal's token_env")
	}
	if lookup == nil {
		lookup = os.LookupEnv
	}
	c := &Connector{source: src.ID, sources: map[string]bool{}, principals: map[string]principal.Principal{}, posters: map[string]bool{}, attributors: map[string]bool{}}
	for _, id := range sources {
		c.sources[id] = true
	}
	for _, p := range principals {
		c.principals[p.ID] = p
	}
	for _, id := range settings.Posters {
		if id == "*" {
			for _, p := range principals {
				if p.Kind == principal.KindAgent && p.TokenEnv != "" {
					c.posters[p.ID] = true
				}
			}
			continue
		}
		if err := c.checkAgent(id); err != nil {
			return nil, fmt.Errorf("posters: %w", err)
		}
		c.posters[id] = true
	}
	for _, id := range settings.Attributors {
		if err := c.checkAgent(id); err != nil {
			return nil, fmt.Errorf("attributors: %w", err)
		}
		if !c.posters[id] {
			return nil, fmt.Errorf("attributor %q is not allowed by posters", id)
		}
		c.attributors[id] = true
	}
	for _, p := range principals {
		if !c.posters[p.ID] {
			continue
		}
		if err := c.tokens.AddEnv(p.ID, p.TokenEnv, lookup); err != nil {
			return nil, fmt.Errorf("poster %q: %w", p.ID, err)
		}
	}
	if c.tokens.Len() == 0 {
		return nil, errors.New("no poster has a token_env, so nobody could post")
	}
	// Known agents excluded by source policy authenticate only to receive 403.
	// Their absent credentials do not make an otherwise usable source fail.
	for _, p := range principals {
		if c.posters[p.ID] || p.Kind != principal.KindAgent || p.TokenEnv == "" {
			continue
		}
		err := c.excluded.AddEnv(p.ID, p.TokenEnv, lookup)
		var duplicate *push.DuplicateTokenError
		if err != nil && !errors.Is(err, push.ErrEmptyToken) && !errors.As(err, &duplicate) {
			return nil, err
		}
	}
	return c, nil
}

func (c *Connector) checkAgent(id string) error {
	p, ok := c.principals[id]
	if !ok || p.Kind != principal.KindAgent || p.TokenEnv == "" {
		return fmt.Errorf("%q must be an agent principal with token_env", id)
	}
	return nil
}

// Describe declares core kinds, without introducing a work-specific event kind.
func (c *Connector) Describe() connector.Descriptor {
	return connector.Descriptor{Type: Type, Kinds: []connector.Kind{connector.KindIssue, connector.KindMessage, connector.KindTombstone}}
}

// Health reports the last successful emission. Runtime poll health also reports
// reconciliation failures.
func (c *Connector) Health(context.Context) connector.Health {
	h := connector.Health{Status: connector.HealthOK}
	if at := c.last.Load(); at != 0 {
		h.LastEventAt = time.Unix(0, at).UTC()
	}
	return h
}

// Close has no connections or goroutines to release.
func (c *Connector) Close(context.Context) error { return nil }

func (c *Connector) lock(project string) func() {
	value, _ := c.projects.LoadOrStore(project, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}
