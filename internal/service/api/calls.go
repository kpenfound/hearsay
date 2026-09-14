package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/hearsay/internal/bundle"
	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/principal"
	"github.com/kpenfound/hearsay/internal/telemetry"
)

// AuditSource is the source id the audit events this service writes carry. It
// is Hearsay itself rather than a connector (docs/connector-contract.md), so no
// ingest allowlist names it and no connector may emit under it.
const AuditSource = "hearsay"

// Caller is who a request says it is: the person it is for, and the agent
// asking on their behalf, if one is. Every interface reads these from the same
// place — the [PrincipalHeader] and [AgentHeader] headers — so a request means
// the same thing whichever one it came through.
type Caller struct {
	Principal string
	Agent     string
}

// The headers a caller is named by.
const (
	PrincipalHeader = "Hearsay-Principal"
	AgentHeader     = "Hearsay-Agent"
)

// CallerOf reads the caller from a request's headers.
func CallerOf(h http.Header) Caller {
	return Caller{Principal: strings.TrimSpace(h.Get(PrincipalHeader)), Agent: strings.TrimSpace(h.Get(AgentHeader))}
}

// Error is a call that did not succeed, with the HTTP status it is served as and
// a message that is a fixed sentence or names only what the caller sent: it
// never carries an error from Postgres, a model or a source.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

func fail(status int, format string, args ...any) *Error {
	return &Error{Status: status, Message: fmt.Sprintf(format, args...)}
}

// errInternal is what every failure that is not the caller's is served as. What
// went wrong is in the log.
var errInternal = &Error{Status: http.StatusInternalServerError, Message: "the call failed; the server log says why"}

// ErrorBody is how an [Error] is encoded, on every interface.
func ErrorBody(e *Error) []byte {
	body, _ := json.Marshal(struct {
		Error string `json:"error"`
	}{e.Message})
	return body
}

// Tool describes one call, for MCP's tools/list. The same list is what the
// HTTP surface serves, so the two cannot offer different calls.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Calls is the read API, independent of the interface that serves it: a call
// takes a caller, a name and JSON arguments, and returns the bytes every
// interface serves verbatim. That is what makes the same request over MCP and
// HTTP byte-identical — there is one encoding, and it happens here.
type Calls struct {
	docs      *l1.Store
	events    *l0.Store
	graph     *l2.Store
	assembler *bundle.Assembler
	resolver  *principal.Resolver
	embedder  l1.Embedder
	// now and id are the clock and the audit event id, replaceable by a test.
	now func() time.Time
	id  func() string
}

// NewCalls builds the call layer over a database and a configuration. A nil
// embedder is a deployment with no `embed` tier, whose search is full text
// alone.
func NewCalls(q l2.Querier, repo config.Repo, embedder l1.Embedder) (*Calls, error) {
	resolver, err := repo.Resolver()
	if err != nil {
		return nil, fmt.Errorf("building the identity resolver: %w", err)
	}
	return &Calls{
		docs:      l1.New(q),
		events:    l0.New(q),
		graph:     l2.New(q),
		assembler: bundle.New(q),
		resolver:  resolver,
		embedder:  embedder,
		now:       time.Now,
		id:        randomID,
	}, nil
}

// WithBudget sets the bundle budget, in estimated tokens.
func (c *Calls) WithBudget(tokens int) { c.assembler = c.assembler.WithBudget(tokens) }

type call struct {
	tool Tool
	run  func(ctx context.Context, c *Calls, caller Caller, reader l1.Reader, args json.RawMessage) (any, error)
}

// calls is the whole API surface, in the order the design lists it.
var calls = []call{
	{Tool{"get_bundle", "The context bundle for a scope: its entities, current stances, recent activity and open questions, every line carrying an L1 id.",
		schema(`{"scope":{"type":"string","description":"the entity id the bundle is for, such as tracker:github:acme/api#12"}}`, "scope")}, getBundle},
	{Tool{"resolve", "The entity ids a piece of text names, by alias and by path.",
		schema(`{"text":{"type":"string"}}`, "text")}, resolve},
	{Tool{"stance_history", "Every stance on a topic the caller may read, oldest first.",
		schema(`{"topic":{"type":"string","description":"a topic id, from a bundle's topic_id"}}`, "topic")}, stanceHistory},
	{Tool{"get_l1", "One L1 document by id.",
		schema(`{"id":{"type":"string"}}`, "id")}, getL1},
	{Tool{"get_l0", "One L0 event by id.",
		schema(`{"id":{"type":"string"}}`, "id")}, getL0},
	{Tool{"search", "Hybrid retrieval over L1: full text and embeddings, fused by rank. Returns documents, not answers.",
		schema(`{"query":{"type":"string"},"scope":{"type":"string","description":"an entity id to search within"},"limit":{"type":"integer"}}`, "query")}, search},
}

func schema(properties string, required ...string) json.RawMessage {
	req, _ := json.Marshal(required)
	return json.RawMessage(`{"type":"object","properties":` + properties + `,"required":` + string(req) + `,"additionalProperties":false}`)
}

// Tools is every call, for an interface to list.
func Tools() []Tool {
	out := make([]Tool, len(calls))
	for i, c := range calls {
		out[i] = c.tool
	}
	return out
}

// Call runs one call for a caller and returns what every interface serves. A
// failure is always an [*Error].
func (c *Calls) Call(ctx context.Context, caller Caller, name string, args json.RawMessage) ([]byte, error) {
	i := slices.IndexFunc(calls, func(k call) bool { return k.tool.Name == name })
	if i < 0 {
		return nil, fail(http.StatusNotFound, "no such call %q", name)
	}
	reader, err := c.readerFor(caller)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(args)) == 0 {
		args = json.RawMessage(`{}`)
	}
	result, err := calls[i].run(ctx, c, caller, reader, args)
	if err != nil {
		var e *Error
		if errors.As(err, &e) {
			return nil, e
		}
		telemetry.Logger(ctx).ErrorContext(ctx, "call failed", "call", name, "principal", caller.Principal, "agent", caller.Agent, "error", err)
		return nil, errInternal
	}
	if raw, ok := result.([]byte); ok {
		return raw, nil
	}
	body, err := json.Marshal(result)
	if err != nil {
		telemetry.Logger(ctx).ErrorContext(ctx, "encoding a result failed", "call", name, "error", err)
		return nil, errInternal
	}
	return body, nil
}

// readerFor is the effective principal a call runs as, and what it may read.
//
// Nothing configures a grant yet (docs/config.md#agent-classes), so every
// principal is granted every scope and the access lists are what narrow a read;
// an agent is held to its class and to the person it acts for by
// [principal.AgentRead]. A caller the mapping does not hold is refused: a read
// nobody is accountable for is not one Hearsay serves.
func (c *Calls) readerFor(caller Caller) (l1.Reader, error) {
	if caller.Principal == "" {
		return l1.Reader{}, fail(http.StatusUnauthorized, "no principal: name the person the call is for in the %s header", PrincipalHeader)
	}
	human, ok := c.resolver.Principal(caller.Principal)
	if !ok {
		return l1.Reader{}, fail(http.StatusForbidden, "%q is not a configured principal", caller.Principal)
	}
	grant := principal.Grant{Scopes: principal.AllScopes()}
	var eff principal.Effective
	var err error
	if caller.Agent == "" {
		eff, err = principal.HumanRead(human, grant)
	} else {
		agent, found := c.resolver.Principal(caller.Agent)
		if !found {
			return l1.Reader{}, fail(http.StatusForbidden, "%q is not a configured principal", caller.Agent)
		}
		eff, err = principal.AgentRead(agent, human, grant, grant)
	}
	if err != nil {
		return l1.Reader{}, fail(http.StatusForbidden, "%s", err.Error())
	}
	reader, err := l1.ReaderFor(c.resolver, eff)
	if err != nil {
		return l1.Reader{}, fail(http.StatusForbidden, "%s", err.Error())
	}
	return reader, nil
}

// decode reads a call's arguments, refusing a field the call does not take: an
// argument accepted and ignored is a filter nobody knows was dropped.
func decode(args json.RawMessage, into any) error {
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return fail(http.StatusBadRequest, "the arguments are not what this call takes: %s", err.Error())
	}
	if dec.More() {
		return fail(http.StatusBadRequest, "the arguments are more than one JSON object")
	}
	return nil
}

func required(name, value string) error {
	if strings.TrimSpace(value) == "" {
		return fail(http.StatusBadRequest, "%s is required", name)
	}
	return nil
}

func getBundle(ctx context.Context, c *Calls, caller Caller, reader l1.Reader, raw json.RawMessage) (any, error) {
	var args struct {
		Scope string `json:"scope"`
	}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if err := required("scope", args.Scope); err != nil {
		return nil, err
	}
	b, report, err := c.assembler.Assemble(ctx, reader, args.Scope)
	if err != nil {
		return nil, err
	}
	body, err := bundle.Encode(b)
	if err != nil {
		return nil, err
	}
	// A bundle is served only once the record that it was is written
	// (docs/design.md#access-control): a bundle nobody can account for is the
	// thing the audit trail exists to rule out.
	if err := c.audit(ctx, caller, args.Scope, body, report); err != nil {
		return nil, err
	}
	return body, nil
}

// AuditRecord is what an audit event's payload.native holds: who asked, on
// whose behalf, what scope, and what was filtered. The bundle itself is named
// by its digest rather than copied: L0 would otherwise hold every bundle ever
// served, and the digest is enough to tell two apart.
type AuditRecord struct {
	Call      string        `json:"call"`
	Principal string        `json:"principal"`
	Agent     string        `json:"agent,omitempty"`
	Scope     string        `json:"scope"`
	Bundle    string        `json:"bundle"`
	Report    bundle.Report `json:"report"`
}

func (c *Calls) audit(ctx context.Context, caller Caller, scope string, body []byte, report bundle.Report) error {
	sum := sha256.Sum256(body)
	record, err := json.Marshal(AuditRecord{
		Call: "get_bundle", Principal: caller.Principal, Agent: caller.Agent, Scope: scope,
		Bundle: "sha256:" + hex.EncodeToString(sum[:]), Report: report,
	})
	if err != nil {
		return fmt.Errorf("encoding the audit record: %w", err)
	}
	author := connector.Identity{Source: AuditSource, Kind: connector.IdentityUser, NativeID: caller.Principal}
	participants := []connector.Participant(nil)
	if caller.Agent != "" {
		// The agent asked; the person is who it asked for.
		participants = []connector.Participant{{Identity: author, Role: connector.RoleAuthor}}
		author = connector.Identity{Source: AuditSource, Kind: connector.IdentityAgent, NativeID: caller.Agent}
	}
	artifact := "bundle:" + c.id()
	ev := connector.Event{
		Source:   AuditSource,
		NativeID: artifact,
		Kind:     connector.KindAudit,
		Time:     c.now().UTC(),
		Payload: connector.Payload{
			Artifact:     artifact,
			Container:    connector.Container{Kind: connector.ContainerWorkspace, NativeID: "api"},
			Author:       &author,
			Participants: participants,
			Native:       record,
		},
		// Who was served what is itself something to keep from the people who
		// were not: the record is readable by the person it was served for, as
		// Hearsay names them, and by nobody a source's grants reach.
		ACL: connector.ACL{{Kind: connector.ACLIdentity, Source: AuditSource, NativeID: caller.Principal}},
	}
	appended, err := c.events.Append(ctx, ev)
	if err != nil {
		return fmt.Errorf("writing the audit event for a bundle: %w", err)
	}
	telemetry.Logger(ctx).DebugContext(ctx, "bundle served", "audit_event", appended.ID, "scope", scope,
		"principal", caller.Principal, "agent", caller.Agent, "tokens", report.Tokens)
	return nil
}

func randomID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails (Go 1.24).
	return hex.EncodeToString(b[:])
}

// EntityMatch is one entity `resolve` found.
type EntityMatch struct {
	ID      string   `json:"id"`
	Type    string   `json:"type"`
	Name    string   `json:"name,omitempty"`
	Owners  []string `json:"owners,omitempty"`
	Aliases []string `json:"aliases_matched,omitempty"`
	Paths   []string `json:"paths_matched,omitempty"`
}

func resolve(ctx context.Context, c *Calls, _ Caller, reader l1.Reader, raw json.RawMessage) (any, error) {
	var args struct {
		Text string `json:"text"`
	}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if err := required("text", args.Text); err != nil {
		return nil, err
	}
	matches, err := c.graph.Resolve(ctx, args.Text)
	if err != nil {
		return nil, err
	}
	out := struct {
		Entities []EntityMatch `json:"entities"`
	}{Entities: []EntityMatch{}}
	for _, m := range matches {
		// Entities carry no access list; the scopes a reader was granted are
		// what narrows them, as they narrow a bundle.
		if !reader.Effective.Grant.Scopes.Has(m.Entity.ID) {
			continue
		}
		out.Entities = append(out.Entities, EntityMatch{
			ID: m.Entity.ID, Type: string(m.Entity.Type), Name: m.Entity.Name, Owners: m.Entity.Owners,
			Aliases: m.Aliases, Paths: m.Paths,
		})
	}
	return out, nil
}

// StanceRecord is one stance in a history.
type StanceRecord struct {
	ID         string   `json:"id"`
	Position   string   `json:"position"`
	Author     string   `json:"author,omitempty"`
	StatedAt   string   `json:"stated_at"`
	Tier       string   `json:"tier"`
	Supersedes string   `json:"supersedes,omitempty"`
	Evidence   []string `json:"evidence"`
}

func stanceHistory(ctx context.Context, c *Calls, _ Caller, reader l1.Reader, raw json.RawMessage) (any, error) {
	var args struct {
		Topic string `json:"topic"`
	}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if err := required("topic", args.Topic); err != nil {
		return nil, err
	}
	topic, err := c.graph.Topic(ctx, args.Topic)
	if errors.Is(err, l2.ErrNotFound) || err == nil && !reader.Allows(topic.ACL) {
		// One answer for both, so a topic somebody may not read is not
		// distinguishable from one that does not exist. Handles fail closed.
		return nil, fail(http.StatusNotFound, "no topic %q", args.Topic)
	}
	if err != nil {
		return nil, err
	}
	history, err := c.graph.StanceHistory(ctx, topic.ID)
	if err != nil {
		return nil, err
	}
	visible := map[string]bool{}
	out := struct {
		Topic   string         `json:"topic"`
		ID      string         `json:"topic_id"`
		Stances []StanceRecord `json:"stances"`
	}{Topic: topic.Name, ID: topic.ID, Stances: []StanceRecord{}}
	for _, st := range history {
		if !reader.Allows(st.ACL) {
			continue
		}
		visible[st.ID] = true
		out.Stances = append(out.Stances, StanceRecord{
			ID: st.ID, Position: st.Position, Author: st.Author, StatedAt: st.StatedAt.UTC().Format(time.RFC3339),
			Tier: string(st.Tier), Supersedes: st.Supersedes, Evidence: st.Evidence,
		})
	}
	for i := range out.Stances {
		// A stance the reader may not read is not named by one they may.
		if !visible[out.Stances[i].Supersedes] {
			out.Stances[i].Supersedes = ""
		}
	}
	return out, nil
}

func getL1(ctx context.Context, c *Calls, _ Caller, reader l1.Reader, raw json.RawMessage) (any, error) {
	var args struct {
		ID string `json:"id"`
	}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if err := required("id", args.ID); err != nil {
		return nil, err
	}
	doc, err := c.docs.Get(ctx, args.ID)
	if errors.Is(err, l1.ErrNotFound) || err == nil && !reader.MayRead(doc.Document) {
		return nil, fail(http.StatusNotFound, "no document %q", args.ID)
	}
	if err != nil {
		return nil, err
	}
	return doc.Document, nil
}

func getL0(ctx context.Context, c *Calls, _ Caller, reader l1.Reader, raw json.RawMessage) (any, error) {
	var args struct {
		ID string `json:"id"`
	}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if err := required("id", args.ID); err != nil {
		return nil, err
	}
	notFound := fail(http.StatusNotFound, "no event %q", args.ID)
	ev, err := c.events.Get(ctx, args.ID)
	switch {
	case errors.Is(err, l0.ErrNotFound) || errors.Is(err, l0.ErrRetracted):
		return nil, notFound
	case err != nil:
		return nil, err
	case !reader.Allows(ev.ACL):
		return nil, notFound
	}
	if !reader.Effective.Grant.Scopes.All {
		// An event carries no scope; a scoped reader reaches one through a
		// document they may read that was built from it.
		cited, err := c.docs.Cites(ctx, reader, ev.ID)
		if err != nil {
			return nil, err
		}
		if !cited {
			return nil, notFound
		}
	}
	return ev, nil
}

// SearchHit is one document a search found.
type SearchHit struct {
	l1.Document
	Score float64 `json:"score"`
}

func search(ctx context.Context, c *Calls, _ Caller, reader l1.Reader, raw json.RawMessage) (any, error) {
	var args struct {
		Query string `json:"query"`
		Scope string `json:"scope"`
		Limit int    `json:"limit"`
	}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if err := required("query", args.Query); err != nil {
		return nil, err
	}
	hits, err := c.docs.Search(ctx, reader, l1.SearchOptions{Query: args.Query, Scope: args.Scope, Limit: args.Limit, Embedder: c.embedder})
	if err != nil {
		return nil, err
	}
	out := struct {
		Documents []SearchHit `json:"documents"`
	}{Documents: make([]SearchHit, len(hits))}
	for i, h := range hits {
		out.Documents[i] = SearchHit{Document: h.Document, Score: h.Score}
	}
	return out, nil
}
