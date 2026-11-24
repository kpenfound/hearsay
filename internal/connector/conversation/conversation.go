// Package conversation is the conversation source (`type: conversation`): a
// person's conversation with their agent, which the agent posts to the
// connectors service one message at a time. It is a [connector.Pusher], mounted
// at `/hooks/<source id>` like any other, and the memory of it is owner-only.
//
// # Configuration
//
//	# sources/kern.yaml
//	id: kern
//	type: conversation
//	containers: [kyle]            # the owner's id, or `*`
//	settings:
//	  owner: kyle                 # required; one configured human principal
//	  agents: [kern]              # required; agent principals that may post
//
// The source names exactly one owner and the agents allowed to post. The owner
// is a configured human; every agent is a configured agent with a `token_env`
// that is set, and no two share a token. The owner and each allowed agent must
// list an identity in this source whose `native_id` is their principal id, as
// they do for the agent session source: it is what resolves the events' author
// to a principal and what lets the owner read them. The source takes no secrets
// and no environment variable of its own.
//
// # Who is posting
//
// An agent authenticates with its own API bearer token, the one its principal
// names in `token_env`. The token is the whole of the agent's identity: it is
// the author of the answers it posts. The body names the owner too, and that
// must be the configured one. A known agent that is not in `settings.agents`,
// and a body naming another owner, are refused with 403.
//
// # What it writes
//
// Each message is one event and its own artifact, `<conversation>/<exchange>:<role>`,
// so the owner's words and the answer to them never share an artifact and
// deleting one exchange touches no other's text. The kind is the extension kind
// [Kind], `conversation.message`, whose `base_kind` is `message`; its container
// is the owner's stream, `{kind: stream, native_id: <owner>}`, never a `channel`,
// so the chat-thread and chat-burst paths have nothing to take from it. The
// conversation id is `payload.thread`, `time` is when the words were said, and
// an interrupted exchange's owner words carry `native.interrupted`.
//
// The access list is one `identity` entry, the owner. The posting agent is not
// on it: it reads the messages only as an agent acting for the owner.
//
// A message posted again with the content the artifact's current revision holds
// writes nothing and is answered with that revision's id. Different content is
// a new revision of the same artifact that becomes the current one, whatever the
// earlier revisions said, and is never a conflict. Every write goes through
// [Connector.store], which is where a refusal to take a message back belongs.
//
// The request, its responses and a worked example are the "Conversation"
// section of docs/connector-contract.md; the source's configuration is
// docs/config.md.
package conversation

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/push"
	"github.com/kpenfound/hearsay/internal/principal"
)

// Type is the registry key for the conversation connector.
const Type = "conversation"

// Kind is the kind of every event the source stores: an extension kind that
// behaves like a chat message.
const Kind connector.Kind = "conversation.message"

// ContainerStream is the container kind of the owner's conversation stream.
const ContainerStream connector.ContainerKind = "stream"

// Settings is the source's `settings`.
type Settings struct {
	// Owner is the configured human principal whose conversation this is, and
	// the only reader of its events.
	Owner string `json:"owner"`
	// Agents are the agent principals allowed to post.
	Agents []string `json:"agents"`
}

// OwnerOf reads who a conversation source belongs to and which agents may post
// to it, from its configuration and with none of the checks [New] makes. It is
// for a caller that must tell whose messages a source holds, as the API's
// `delete` does; a source whose settings cannot be read has no owner.
func OwnerOf(src connector.SourceConfig) (owner string, agents []string, ok bool) {
	if src.Type != Type {
		return "", nil, false
	}
	var settings Settings
	if err := src.DecodeSettings(&settings); err != nil || settings.Owner == "" {
		return "", nil, false
	}
	return settings.Owner, settings.Agents, true
}

// Connector accepts the messages of one person's conversation.
type Connector struct {
	source string
	owner  string
	// agents are the agents allowed to post.
	agents map[string]bool
	// tokens authenticates every agent that has a token, so that an agent the
	// source does not allow is told so rather than refused as unknown.
	tokens push.Tokens
	last   atomic.Int64
}

var _ connector.Pusher = (*Connector)(nil)

// NewFactory returns the factory the binary registers under [Type]. The
// principals are the configuration's `principals/`, and lookup reads the
// environment their `token_env` names; nil reads the process environment.
func NewFactory(principals []principal.Principal, lookup func(string) (string, bool)) connector.Factory {
	return func(_ context.Context, src connector.SourceConfig) (connector.Connector, error) {
		return New(src, principals, lookup)
	}
}

// New validates a source: its owner, the agents allowed to post and their
// tokens, and that the owner's events are inside the ingest allowlist.
func New(src connector.SourceConfig, principals []principal.Principal, lookup func(string) (string, bool)) (*Connector, error) {
	var settings Settings
	if err := src.DecodeSettings(&settings); err != nil {
		return nil, err
	}
	if len(src.Secrets) != 0 {
		return nil, errors.New("a conversation source takes no secrets: an agent authenticates with the token its principal names in token_env")
	}
	byID := make(map[string]principal.Principal, len(principals))
	for _, p := range principals {
		byID[p.ID] = p
	}
	if settings.Owner == "" {
		return nil, errors.New("settings.owner is required: it is the one configured human whose conversation this is")
	}
	if owner, ok := byID[settings.Owner]; !ok || owner.Kind != principal.KindHuman {
		return nil, fmt.Errorf("settings.owner %q is not a configured human principal", settings.Owner)
	}
	if len(settings.Agents) == 0 {
		return nil, errors.New("settings.agents is required: name the agent principals allowed to post")
	}
	c := &Connector{source: src.ID, owner: settings.Owner, agents: make(map[string]bool, len(settings.Agents))}
	for _, id := range settings.Agents {
		if p, ok := byID[id]; !ok || p.Kind != principal.KindAgent {
			return nil, fmt.Errorf("settings.agents names %q, which is not a configured agent principal", id)
		}
		if c.agents[id] {
			return nil, fmt.Errorf("settings.agents names %q twice", id)
		}
		c.agents[id] = true
	}

	// The allowed agents' tokens are required. Another agent's is kept when it
	// has one, only to answer it 403, and is left out when it cannot be used.
	for _, p := range principals {
		if p.Kind != principal.KindAgent || p.TokenEnv == "" {
			continue
		}
		err := c.tokens.AddEnv(p.ID, p.TokenEnv, lookup)
		if err == nil || !c.agents[p.ID] {
			continue
		}
		var duplicate *push.DuplicateTokenError
		switch {
		case errors.Is(err, push.ErrEmptyToken):
			return nil, fmt.Errorf("agent %q: %s is not set or is empty", p.ID, p.TokenEnv)
		case errors.As(err, &duplicate):
			return nil, fmt.Errorf("agents %q and %q share an API token", duplicate.Other, duplicate.ID)
		default:
			return nil, err
		}
	}
	for id := range c.agents {
		if byID[id].TokenEnv == "" {
			return nil, fmt.Errorf("agent %q has no token_env, so it could not post to this source", id)
		}
	}

	if len(src.Containers) == 0 {
		return nil, errors.New(`containers must be the owner's id or "*"`)
	}
	for _, id := range src.Containers {
		if id != connector.AllowAll && id != c.owner {
			return nil, fmt.Errorf("container %q is not the owner %q: the only container is the owner's stream", id, c.owner)
		}
	}
	if !connector.NewAllowlist(src).Allows(src.ID, c.owner) {
		return nil, fmt.Errorf("containers does not allow the owner %q, so every message would be dropped", c.owner)
	}

	// Without an identity mapping the owner could not read their own events and
	// the author of a message would resolve to nobody.
	for _, id := range append([]string{c.owner}, settings.Agents...) {
		if !mapped(byID[id], src.ID) {
			return nil, fmt.Errorf("principal %q has no identity in source %q: list {source: %s, native_id: %s} under its identities", id, src.ID, src.ID, id)
		}
	}
	return c, nil
}

// mapped reports whether p lists the identity this source gives it: its own
// principal id as the native id.
func mapped(p principal.Principal, source string) bool {
	return slices.ContainsFunc(p.Identities, func(i principal.Identity) bool {
		return i.Source == source && i.NativeID == p.ID
	})
}

// Describe declares the one kind the source emits.
func (c *Connector) Describe() connector.Descriptor {
	return connector.Descriptor{Type: Type, Kinds: []connector.Kind{Kind}}
}

// Health reports ok: a push source has nothing of its own to fail, and a quiet
// one is told from a stuck one by the time of its last event.
func (c *Connector) Health(context.Context) connector.Health {
	h := connector.Health{Status: connector.HealthOK}
	if at := c.last.Load(); at != 0 {
		h.LastEventAt = time.Unix(0, at).UTC()
	}
	return h
}

// Close releases nothing: the connector holds no connection and no goroutine.
func (c *Connector) Close(context.Context) error { return nil }
