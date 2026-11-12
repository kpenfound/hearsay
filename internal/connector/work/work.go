package work

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sync/atomic"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/push"
	"github.com/kpenfound/hearsay/internal/principal"
)

// Type is the connector registry key.
const Type = "work"

// Settings controls posting and trusted attribution. OwnerOf makes the source
// the owner source of the named work source: it then holds only documents and
// decisions about that source's projects and workstreams.
type Settings struct {
	Posters     []string `json:"posters"`
	Attributors []string `json:"attributors"`
	OwnerOf     string   `json:"owner_of,omitempty"`
}

// OwnerOf returns the work source a source configuration names as the one it
// owns records for, or "" for an ordinary work source or a source of another
// type. It reads only that setting, so a caller holding every source's
// configuration (the loader, the paired reads) need not build a connector.
func OwnerOf(src connector.SourceConfig) string {
	if src.Type != Type {
		return ""
	}
	var settings struct {
		OwnerOf string `json:"owner_of"`
	}
	if len(src.Settings) == 0 || json.Unmarshal(src.Settings, &settings) != nil {
		return ""
	}
	return settings.OwnerOf
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
	shared      *Shared
	ownerOf     string
	last        atomic.Int64
}

var (
	_ connector.Pusher = (*Connector)(nil)
	_ connector.Poller = (*Connector)(nil)
)

// Option adjusts how a connector is built.
type Option func(*options)

type options struct {
	shared  *Shared
	configs []connector.SourceConfig
}

// WithShared makes the connector share project locks and peer sinks with the
// connectors built from the same Shared. The two sources of a pair must share
// one; NewFactory gives every connector it builds the same one.
func WithShared(s *Shared) Option { return func(o *options) { o.shared = s } }

// WithSources gives the connector every configured source, so that an owner
// source can check that the work source it names is a work source and is not
// itself an owner source. Configuration validation makes the same check
// first; this is the connector refusing to start without it.
func WithSources(configs []connector.SourceConfig) Option {
	return func(o *options) { o.configs = configs }
}

// NewFactory binds configured principals and all configured source ids. lookup
// reads token_env; nil uses the process environment. Every connector it builds
// shares one [Shared], which is what pairs an owner source with its work source.
func NewFactory(principals []principal.Principal, sources []string, lookup func(string) (string, bool), opts ...Option) connector.Factory {
	shared := NewShared()
	return func(_ context.Context, src connector.SourceConfig) (connector.Connector, error) {
		return New(src, principals, sources, lookup, append([]Option{WithShared(shared)}, opts...)...)
	}
}

// New checks source policy and loads poster credentials before requests arrive.
func New(src connector.SourceConfig, principals []principal.Principal, sources []string, lookup func(string) (string, bool), opts ...Option) (*Connector, error) {
	settings := Settings{Posters: []string{"*"}}
	if err := src.DecodeSettings(&settings); err != nil {
		return nil, err
	}
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	if o.shared == nil {
		o.shared = NewShared()
	}
	if err := checkOwner(src.ID, settings.OwnerOf, sources, o.configs); err != nil {
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
	c := &Connector{source: src.ID, sources: map[string]bool{}, principals: map[string]principal.Principal{}, posters: map[string]bool{}, attributors: map[string]bool{}, shared: o.shared, ownerOf: settings.OwnerOf}
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
	if c.ownerOf != "" {
		c.shared.addOwner(c.ownerOf, c.source)
	}
	return c, nil
}

// checkOwner refuses an owner_of that names the source itself, a source that is
// not configured, one that is not a work source, or another owner source. The
// type and pairing checks need every source's configuration, which only the
// factory built with [WithSources] has.
func checkOwner(self, named string, ids []string, configs []connector.SourceConfig) error {
	if named == "" {
		return nil
	}
	switch {
	case !connector.ValidSourceID(named):
		return fmt.Errorf("owner_of %q is not a source id", named)
	case named == self:
		return errors.New("owner_of names the source itself")
	case len(ids) > 0 && !slices.Contains(ids, named):
		return fmt.Errorf("owner_of names source %q, which is not configured", named)
	}
	for _, cfg := range configs {
		if cfg.ID != named {
			continue
		}
		if cfg.Type != Type {
			return fmt.Errorf("owner_of names source %q, which is not a work source", named)
		}
		if OwnerOf(cfg) != "" {
			return fmt.Errorf("owner_of names source %q, which is itself an owner source", named)
		}
	}
	return nil
}

func (c *Connector) checkAgent(id string) error {
	p, ok := c.principals[id]
	if !ok || p.Kind != principal.KindAgent || p.TokenEnv == "" {
		return fmt.Errorf("%q must be an agent principal with token_env", id)
	}
	return nil
}

// Describe declares the core kinds projects, workstreams, updates and documents
// use, and the one extension kind, work.decision, a decision uses.
func (c *Connector) Describe() connector.Descriptor {
	return connector.Descriptor{Type: Type, Kinds: []connector.Kind{connector.KindIssue, connector.KindMessage, connector.KindDocument, KindDecision, connector.KindTombstone}}
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

// workSource is the source that holds the projects: this one, or the one an
// owner source names. It keys the lock a pair shares.
func (c *Connector) workSource() string {
	if c.ownerOf != "" {
		return c.ownerOf
	}
	return c.source
}

// lock serializes every post and reconciliation about one project across both
// sources of its pair, and returns the release.
func (c *Connector) lock(project string) func() { return c.shared.lock(c.workSource(), project) }
