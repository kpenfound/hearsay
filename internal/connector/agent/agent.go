// Package agent is the agent session source: an agent posts its own session
// events — the session starting and ending, its turns and its tool calls — to
// the connectors service, which writes them to L0 as `agent_session`,
// `agent_turn` and `tool_call` events. It is a [connector.Pusher], mounted at
// `/hooks/<source id>` like any other.
//
// # Who is posting
//
// An agent authenticates with its own API bearer token, the one its entry in
// `principals/` names in `token_env` (ADR-0014): `Authorization: Bearer
// <token>`. The token is the whole of the agent's identity. The event's author
// is the agent the token belongs to, never one the request names; a body whose
// `agent` names anyone else is refused. The body also names the person the
// session acts for, in `on_behalf_of`, and that must be a configured human
// principal. The agent's token is enough to name them: this records what an
// agent did, and reading on that person's behalf still takes their own token
// at the API.
//
// The tokens are read from the environment when the connector is built, so an
// agent whose `token_env` is set but empty is a startup failure, as it is for
// the API. An agent with no `token_env` cannot post, and a configuration with
// no agent that can is a source that could accept nothing, which is a startup
// failure too.
//
// # What it writes
//
// The artifact is the session: every event of one session carries the same
// `payload.artifact`, the session id the agent supplies, and each event is one
// revision of it, keyed by what it is — `<session>@start`, `<session>@end`,
// `<session>@turn:<turn>`, `<session>@call:<call>`. So an artifact's history in
// L0 is the session in order, and the same event posted twice is the same
// event id and writes nothing. The same key posted again with different
// content is refused with 409: an event key names one thing that happened.
//
// `time` is when the session started, on every event, which is what the
// contract asks of every revision of an artifact; when this event happened is
// `payload.revision.edited_at`, and it is what orders the session's events.
//
// The container is the agent's session stream, `{kind: stream, native_id:
// <agent id>}`, so the source's `containers` is `*` or the agent ids that may
// post. An agent the allowlist does not cover is refused with 403 rather than
// silently dropped, because the agent is the one asking.
//
// The access list is the agent and the person it acted for, and nobody else:
// two `identity` entries in this source, whose native ids are the principal
// ids. A principal reads them by listing that identity in `principals/`
// (`{source: <this source>, native_id: <principal id>}`), which is also what
// resolves the events' author.
//
// These kinds are provenance rather than team knowledge, and the distiller
// does not distil them.
//
// The request, one event per POST, is documented with an example per kind in
// docs/connector-contract.md; the source's configuration is docs/config.md.
package agent

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/principal"
)

// Type is the registry key for the agent session connector.
const Type = "agent"

// ContainerStream is the container kind of an agent's session stream.
const ContainerStream connector.ContainerKind = "stream"

// Connector accepts the session events of the agents configuration names.
type Connector struct {
	source string
	allow  connector.Allowlist
	// agents are the agents that may post, with the digest of their token.
	agents []credential
	// humans are the principal ids an agent may act for.
	humans map[string]bool
	last   atomic.Int64
}

type credential struct {
	id     string
	digest [sha256.Size]byte
}

var _ connector.Pusher = (*Connector)(nil)

// NewFactory returns the factory the binary registers under [Type]. The
// principals are the configuration's `principals/`, and lookup reads the
// environment their `token_env` names; nil is [os.LookupEnv].
func NewFactory(principals []principal.Principal, lookup func(string) (string, bool)) connector.Factory {
	return func(_ context.Context, src connector.SourceConfig) (connector.Connector, error) {
		return New(src, principals, lookup)
	}
}

// New validates a source and resolves the tokens of the agents that may post
// to it.
func New(src connector.SourceConfig, principals []principal.Principal, lookup func(string) (string, bool)) (*Connector, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	var settings struct{}
	if err := src.DecodeSettings(&settings); err != nil {
		return nil, err
	}
	if len(src.Secrets) != 0 {
		return nil, errors.New("agent takes no secrets: an agent authenticates with the token its principal names in token_env")
	}
	if len(src.Containers) == 0 {
		return nil, errors.New(`containers must be "*" or the ids of the agents that may post`)
	}
	c := &Connector{source: src.ID, allow: connector.NewAllowlist(src), humans: make(map[string]bool)}
	kinds := make(map[string]principal.Kind, len(principals))
	seen := make(map[[sha256.Size]byte]string)
	for _, p := range principals {
		kinds[p.ID] = p.Kind
		switch p.Kind {
		case principal.KindHuman:
			c.humans[p.ID] = true
		case principal.KindAgent:
			if p.TokenEnv == "" {
				continue
			}
			value, ok := lookup(p.TokenEnv)
			if !ok || value == "" {
				return nil, fmt.Errorf("agent %q: %s is not set or is empty", p.ID, p.TokenEnv)
			}
			digest := sha256.Sum256([]byte(value))
			if other, dup := seen[digest]; dup {
				return nil, fmt.Errorf("agents %q and %q share an API token", other, p.ID)
			}
			seen[digest] = p.ID
			c.agents = append(c.agents, credential{id: p.ID, digest: digest})
		}
	}
	for _, id := range src.Containers {
		if id != connector.AllowAll && kinds[id] != principal.KindAgent {
			return nil, fmt.Errorf("container %q is not an agent principal: containers are agent ids, or *", id)
		}
	}
	if len(c.agents) == 0 {
		return nil, errors.New("no agent principal has a token_env, so no agent could post to this source")
	}
	return c, nil
}

// Describe declares the three session kinds.
func (c *Connector) Describe() connector.Descriptor {
	return connector.Descriptor{Type: Type, Kinds: []connector.Kind{connector.KindAgentSession, connector.KindAgentTurn, connector.KindToolCall}}
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

// agentFor is the agent a presented token belongs to. Every configured digest
// is compared, in constant time, so how long this takes says nothing about
// which agent, if any, came close.
func (c *Connector) agentFor(token string) (string, bool) {
	got := sha256.Sum256([]byte(token))
	found := ""
	for _, a := range c.agents {
		if subtle.ConstantTimeCompare(got[:], a.digest[:]) == 1 {
			found = a.id
		}
	}
	return found, found != ""
}

// Request is one event, as an agent posts it. Exactly one of Phase, Turn and
// Call is set, according to Kind.
type Request struct {
	// Agent, where set, must be the agent the token authenticates. It is a
	// check, never a source of identity.
	Agent string `json:"agent,omitempty"`
	// OnBehalfOf is the human principal the session acts for.
	OnBehalfOf string `json:"on_behalf_of"`
	// Session is the agent's own id for the session: the artifact.
	Session string `json:"session"`
	// Kind is agent_session, agent_turn or tool_call.
	Kind connector.Kind `json:"kind"`
	// StartedAt is when the session started, the same on every event of it.
	StartedAt time.Time `json:"started_at"`
	// Time is when this event happened.
	Time time.Time `json:"time"`
	// Phase is `start` or `end`, on an agent_session.
	Phase string `json:"phase,omitempty"`
	// Turn is the turn's id within the session, on an agent_turn.
	Turn string `json:"turn,omitempty"`
	// Call is the call's id within the session, and Tool the tool's name, on a
	// tool_call.
	Call string `json:"call,omitempty"`
	Tool string `json:"tool,omitempty"`
	// Text is what the turn said. It is required on an agent_turn, and may
	// annotate either of the others.
	Text string `json:"text,omitempty"`
	// Input and Output are a tool call's arguments and result, as JSON.
	Input  json.RawMessage `json:"input,omitempty"`
	Output json.RawMessage `json:"output,omitempty"`
}

// Native is what an event's payload.native holds: the parts of the request
// the standard payload fields have no place for.
type Native struct {
	Session string          `json:"session"`
	Phase   string          `json:"phase,omitempty"`
	Turn    string          `json:"turn,omitempty"`
	Call    string          `json:"call,omitempty"`
	Tool    string          `json:"tool,omitempty"`
	Input   json.RawMessage `json:"input,omitempty"`
	Output  json.RawMessage `json:"output,omitempty"`
}

// The session phases.
const (
	PhaseStart = "start"
	PhaseEnd   = "end"
)

// maxKey is the longest session, turn or call id.
const maxKey = 128

// validKey reports whether s may be a session, turn or call id: 1 to [maxKey]
// bytes of letters, digits, `-`, `_`, `.` and `:`. The set leaves out `@`,
// which separates an artifact from its revision in a native id, and anything
// an event id would have to escape.
func validKey(s string) bool {
	if s == "" || len(s) > maxKey {
		return false
	}
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-' || c == '_' || c == '.' || c == ':':
		default:
			return false
		}
	}
	return true
}

// event turns an authenticated agent's request into the event it means. The
// request is the caller's, so what is wrong with it is an [errBadRequest].
func (c *Connector) event(agent string, req Request) (connector.Event, error) {
	if req.Session == "" || !validKey(req.Session) {
		return connector.Event{}, badRequest("session must be 1 to %d bytes of letters, digits, -, _, . and :", maxKey)
	}
	if req.StartedAt.IsZero() || req.Time.IsZero() {
		return connector.Event{}, badRequest("started_at and time are required")
	}
	if req.Time.Before(req.StartedAt) {
		return connector.Event{}, badRequest("time is earlier than started_at")
	}
	native := Native{Session: req.Session}
	var revision string
	switch req.Kind {
	case connector.KindAgentSession:
		if req.Phase != PhaseStart && req.Phase != PhaseEnd {
			return connector.Event{}, badRequest("an agent_session has a phase of start or end")
		}
		if req.Turn != "" || req.Call != "" || req.Tool != "" || req.Input != nil || req.Output != nil {
			return connector.Event{}, badRequest("an agent_session has a phase and nothing of a turn or a tool call")
		}
		native.Phase, revision = req.Phase, req.Phase
	case connector.KindAgentTurn:
		if !validKey(req.Turn) {
			return connector.Event{}, badRequest("an agent_turn has a turn id of 1 to %d bytes of letters, digits, -, _, . and :", maxKey)
		}
		if req.Text == "" {
			return connector.Event{}, badRequest("an agent_turn has text")
		}
		if req.Phase != "" || req.Call != "" || req.Tool != "" || req.Input != nil || req.Output != nil {
			return connector.Event{}, badRequest("an agent_turn has a turn and text and nothing of a session phase or a tool call")
		}
		native.Turn, revision = req.Turn, "turn:"+req.Turn
	case connector.KindToolCall:
		if !validKey(req.Call) {
			return connector.Event{}, badRequest("a tool_call has a call id of 1 to %d bytes of letters, digits, -, _, . and :", maxKey)
		}
		if req.Tool == "" {
			return connector.Event{}, badRequest("a tool_call names its tool")
		}
		if req.Phase != "" || req.Turn != "" {
			return connector.Event{}, badRequest("a tool_call has a call and a tool and nothing of a session phase or a turn")
		}
		native.Call, native.Tool, native.Input, native.Output = req.Call, req.Tool, req.Input, req.Output
		revision = "call:" + req.Call
	default:
		return connector.Event{}, badRequest("kind must be agent_session, agent_turn or tool_call")
	}
	raw, err := json.Marshal(native)
	if err != nil {
		return connector.Event{}, badRequest("input and output must be JSON")
	}

	edited := req.Time.UTC()
	author := connector.Identity{Source: c.source, Kind: connector.IdentityAgent, NativeID: agent}
	person := connector.Identity{Source: c.source, Kind: connector.IdentityUser, NativeID: req.OnBehalfOf}
	nativeID := req.Session + "@" + revision
	return connector.Event{
		ID:       connector.EventID(c.source, nativeID),
		Source:   c.source,
		NativeID: nativeID,
		Kind:     req.Kind,
		Time:     req.StartedAt.UTC(),
		Payload: connector.Payload{
			Artifact:  req.Session,
			Container: connector.Container{Kind: ContainerStream, NativeID: agent},
			Text:      req.Text,
			Author:    &author,
			// The agent acted; the person is who it acted for, as the API's own
			// audit and assertion events say it.
			Participants: []connector.Participant{{Identity: person, Role: connector.RoleAuthor}},
			Revision:     &connector.Revision{Token: revision, EditedAt: edited},
			Native:       raw,
		},
		ACL: connector.ACL{
			{Kind: connector.ACLIdentity, Source: c.source, NativeID: agent},
			{Kind: connector.ACLIdentity, Source: c.source, NativeID: req.OnBehalfOf},
		},
	}, nil
}

// errBadRequest is a request the connector cannot turn into an event: the
// agent's fault, answered 400.
var errBadRequest = errors.New("bad request")

func badRequest(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errBadRequest, fmt.Sprintf(format, args...))
}

// mayActFor reports whether id is a configured human.
func (c *Connector) mayActFor(id string) bool { return c.humans[id] }
