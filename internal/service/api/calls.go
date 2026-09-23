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

	"github.com/jackc/pgx/v5"

	"github.com/kpenfound/hearsay/internal/bundle"
	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/principal"
	"github.com/kpenfound/hearsay/internal/queue"
	"github.com/kpenfound/hearsay/internal/telemetry"
)

// AuditSource is the source id the audit and assertion events this service
// writes carry. It is Hearsay itself rather than a connector
// (docs/connector-contract.md), so no ingest allowlist names it and no
// connector may emit under it.
const AuditSource = connector.SelfSource

// Caller is the authenticated person a request is for and, if present, the
// authenticated agent acting with that person's delegated credential.
type Caller struct {
	Principal string
	Agent     string
}

// The headers a caller is named by. Names alone grant no access.
const (
	PrincipalHeader = "Hearsay-Principal"
	AgentHeader     = "Hearsay-Agent"
)

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

// Calls is the API, independent of the interface that serves it: a call
// takes a caller, a name and JSON arguments, and returns the bytes every
// interface serves verbatim. That is what makes the same request over MCP and
// HTTP byte-identical — there is one encoding, and it happens here.
type Calls struct {
	db        DB
	docs      *l1.Store
	events    *l0.Store
	graph     *l2.Store
	authority config.Authority
	assembler *bundle.Assembler
	cache     *bundleCache
	resolver  *principal.Resolver
	auth      *authenticator
	embedder  l1.Embedder
	// now and id are the clock and the audit event id, replaceable by a test.
	// The clock is also an assertion event's time.
	now func() time.Time
	id  func() string
}

// DB is what the call layer runs on: a pool, in whose transactions `assert`
// writes its event and the job that appends its stance together.
type DB interface {
	l2.Querier
	Begin(ctx context.Context) (pgx.Tx, error)
}

// NewCalls builds the call layer over a database and a configuration. A nil
// embedder is a deployment with no `embed` tier, whose search is full text
// alone.
func NewCalls(q DB, repo config.Repo, embedder l1.Embedder) (*Calls, error) {
	auth, err := newAuthenticator(repo.Principals)
	if err != nil {
		return nil, fmt.Errorf("configuring API authentication: %w", err)
	}
	resolver, err := repo.Resolver()
	if err != nil {
		return nil, fmt.Errorf("building the identity resolver: %w", err)
	}
	return &Calls{
		db:        q,
		docs:      l1.New(q),
		events:    l0.New(q),
		graph:     l2.New(q),
		assembler: bundle.New(q).WithDirectiveSources(repo, resolver).WithAuthority(repo.Authority),
		cache:     newBundleCache(),
		authority: repo.Authority,
		resolver:  resolver,
		auth:      auth,
		embedder:  embedder,
		now:       time.Now,
		id:        randomID,
	}, nil
}

// WithBudget sets the bundle budget, in estimated tokens.
func (c *Calls) WithBudget(tokens int) {
	c.assembler = c.assembler.WithBudget(tokens)
	c.cache = newBundleCache()
}

type call struct {
	tool Tool
	run  func(ctx context.Context, c *Calls, caller Caller, reader l1.Reader, args json.RawMessage) (any, error)
}

// calls is the whole API surface, in the order the design lists it.
var calls = []call{
	{Tool{"get_bundle", "The context bundle for a scope, with an optional directive from a triggering L0 message id; its other sections contain entities, current stances, recent activity and open questions.",
		schema(`{"scope":{"type":"string","description":"the entity id the bundle is for, such as tracker:github:acme/api#12"},"directive":{"type":"string","description":"L0 event id of the triggering message"}}`, "scope")}, getBundle},
	{Tool{"resolve", "The entity ids a piece of text names, by alias and by path.",
		schema(`{"text":{"type":"string"}}`, "text")}, resolve},
	{Tool{"stance_history", "Every stance on a topic the caller may read, oldest first, with the topic's current stance and its tier: ratified, inferred or contested.",
		schema(`{"topic":{"type":"string","description":"a topic id, from a bundle's topic_id"}}`, "topic")}, stanceHistory},
	{Tool{"get_l1", "One L1 document by id.",
		schema(`{"id":{"type":"string"}}`, "id")}, getL1},
	{Tool{"get_l0", "One L0 event by id.",
		schema(`{"id":{"type":"string"}}`, "id")}, getL0},
	{Tool{"search", "Hybrid retrieval over L1: full text and embeddings, fused by rank. Returns documents, not answers.",
		schema(`{"query":{"type":"string"},"scope":{"type":"string","description":"an entity id to search within"},"limit":{"type":"integer"}}`, "query")}, search},
	{Tool{"assert", "Propose a position on an existing topic, citing the L1 documents it rests on. Only an agent of class worker or above may; the stance is appended by the assertion worker, ranks as class agent and does not ratify on its own under the default authority policy. Returns the id of the L0 assertion event; repeating a request returns the same id and writes nothing more.",
		schema(`{"topic":{"type":"string","description":"an existing topic id, from a bundle's topic_id"},"position":{"type":"string","description":"the position, at most 2000 bytes"},"evidence":{"type":"array","items":{"type":"string"},"minItems":1,"maxItems":32,"description":"L1 document ids the position rests on; the caller must be able to read every one"}}`, "topic", "position", "evidence")}, assertStance},
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
		Scope     string `json:"scope"`
		Directive string `json:"directive"`
	}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if err := required("scope", args.Scope); err != nil {
		return nil, err
	}
	var revision int64
	if err := c.db.QueryRow(ctx, `SELECT count(*) FROM bundle_changes`).Scan(&revision); err != nil {
		return nil, fmt.Errorf("reading bundle watermark: %w", err)
	}
	key := bundleKey{args.Scope, caller.Principal, caller.Agent, args.Directive, revision}
	entry, hit := c.cache.get(key)
	if !hit {
		b, report, err := c.assembler.AssembleForEvent(ctx, reader, args.Scope, args.Directive)
		if err != nil {
			return nil, err
		}
		body, err := bundle.Encode(b)
		if err != nil {
			return nil, err
		}
		entry = bundleValue{key: key, body: body, report: report}
	}
	// A bundle is served only once the record that it was is written
	// (docs/design.md#access-control): a bundle nobody can account for is the
	// thing the audit trail exists to rule out.
	if err := c.audit(ctx, caller, args.Scope, entry.body, entry.report); err != nil {
		return nil, err
	}
	if !hit {
		c.cache.put(entry)
	}
	return entry.body, nil
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
		// were not. The entry names the principal in Hearsay's own source, which
		// no configured identity is in and so no reader's audience holds: the
		// record is readable by nobody through the API yet, the person it was
		// served for included. Failing closed until audit reads are built.
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

// resolve reads configured names and learned names whose evidence the reader may inspect.
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
	matches, err := c.graph.ResolveFor(ctx, reader, args.Text)
	if err != nil {
		return nil, err
	}
	out := struct {
		Entities []EntityMatch `json:"entities"`
	}{Entities: []EntityMatch{}}
	for _, m := range matches {
		out.Entities = append(out.Entities, EntityMatch{
			ID: m.Entity.ID, Type: string(m.Entity.Type), Name: m.Entity.Name, Owners: m.Entity.Owners,
			Aliases: m.Aliases, Paths: m.Paths,
		})
	}
	return out, nil
}

// StanceRecord is one stance in a history.
type StanceRecord struct {
	ID        string  `json:"id"`
	Position  string  `json:"position"`
	Judgement *string `json:"judgement"`
	Author    string  `json:"author,omitempty"`
	StatedAt  string  `json:"stated_at"`
	// RecordedTier is the tier the stance was written with: a record of that
	// moment, not the tier the topic stands at, which is [History.Tier].
	RecordedTier string   `json:"recorded_tier"`
	Supersedes   string   `json:"supersedes,omitempty"`
	Evidence     []string `json:"evidence"`
}

// History is a topic's `stance_history`: every stance on it the caller may
// read, oldest first, and where the topic stands — its current stance and the
// tier computed under the policy in force for its scope, the same the bundle
// and L3 serve. Current and Tier are empty where the caller may not read the
// current stance, as the bundle leaves such a topic out.
type History struct {
	Topic   string         `json:"topic"`
	ID      string         `json:"topic_id"`
	Current string         `json:"current,omitempty"`
	Tier    string         `json:"tier,omitempty"`
	Stances []StanceRecord `json:"stances"`
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
	// Who may read the topic and each stance is what their documents allow
	// now (l2.Access), not what they allowed when the worker read them, so a
	// superseded or retired stance goes with its evidence too.
	topic, err := c.graph.Topic(ctx, args.Topic)
	if errors.Is(err, l2.ErrNotFound) {
		return nil, fail(http.StatusNotFound, "no topic %q", args.Topic)
	}
	if err != nil {
		return nil, err
	}
	assessed, err := c.graph.Assess(ctx, c.authority, reader, []l2.Topic{topic})
	if err != nil {
		return nil, err
	}
	a := assessed[0]
	if !a.Access.Topic(reader, topic) {
		// One answer for both, so a topic somebody may not read is not
		// distinguishable from one that does not exist. Handles fail closed.
		return nil, fail(http.StatusNotFound, "no topic %q", args.Topic)
	}
	visible := map[string]bool{}
	out := History{Topic: topic.Name, ID: topic.ID, Stances: []StanceRecord{}}
	if a.Stands && a.Access.Stance(reader, a.Standing.Current) {
		out.Current, out.Tier = a.Standing.Current.ID, string(a.Standing.Tier)
	}
	for _, st := range a.History {
		if !a.Access.Stance(reader, st) {
			continue
		}
		visible[st.ID] = true
		out.Stances = append(out.Stances, StanceRecord{
			ID: st.ID, Position: st.Position, Author: st.Author, StatedAt: st.StatedAt.UTC().Format(time.RFC3339),
			RecordedTier: string(st.Tier), Supersedes: st.Supersedes, Evidence: st.Evidence,
			Judgement: stanceJudgement(st.Judgement),
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

func stanceJudgement(j l2.Judgement) *string {
	if j == l2.JudgementUnknown {
		return nil
	}
	v := string(j)
	return &v
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
	// An event carries no scope, so its access list is the whole filter. That
	// is enough while every caller is granted every scope (readerFor); a
	// scoped grant would reach an event through a document it may read.
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

// Asserted is what `assert` returns: the id of the L0 `assertion` event that
// carries the request. The stance follows once the assertion worker appends
// it, and `stance_history` then serves it.
type Asserted struct {
	ID string `json:"id"`
}

// errUnreadable is the one answer for a topic or a piece of evidence that does
// not exist or that the caller may not read, whichever it is and whichever id:
// handles fail closed, and a refusal that named the id would tell the caller
// which private document exists.
var errUnreadable = &Error{Status: http.StatusNotFound, Message: "the topic and the evidence are not all ones you may read"}

// assertStance is `assert(topic, position, evidence)`
// (docs/design.md#read-and-assert-api): an agent's stance, written as an L0
// `assertion` event under source hearsay, with the assert job that appends it
// to the topic enqueued in the same transaction under the topic's scope. No
// model is called. The request is its own idempotency key: the event's native
// id is derived from the agent, the person, the topic, the position and the
// evidence, so a retry is the same event and the same stance.
func assertStance(ctx context.Context, c *Calls, caller Caller, reader l1.Reader, raw json.RawMessage) (any, error) {
	if err := c.mayAssert(caller); err != nil {
		return nil, err
	}
	var args struct {
		Topic    string   `json:"topic"`
		Position string   `json:"position"`
		Evidence []string `json:"evidence"`
	}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if err := required("topic", args.Topic); err != nil {
		return nil, err
	}
	// Cleaned the way the assertion worker cleans a model's positions.
	position, _ := l1.Scrub(strings.TrimSpace(args.Position))
	position = strings.TrimSpace(position)
	if err := required("position", position); err != nil {
		return nil, err
	}
	if len(position) > l2.MaxPosition {
		return nil, fail(http.StatusBadRequest, "position is %d bytes, and a position is at most %d", len(position), l2.MaxPosition)
	}
	evidence := l2.SortedEvidence(args.Evidence)
	if len(evidence) == 0 {
		return nil, fail(http.StatusBadRequest, "evidence is required: cite at least one L1 document id")
	}
	if len(evidence) > l2.MaxAssertionEvidence {
		return nil, fail(http.StatusBadRequest, "evidence cites %d documents, and an assertion cites at most %d", len(evidence), l2.MaxAssertionEvidence)
	}
	for _, id := range evidence {
		if strings.TrimSpace(id) == "" {
			return nil, fail(http.StatusBadRequest, "an evidence id is empty")
		}
	}

	topic, err := c.graph.Topic(ctx, args.Topic)
	if errors.Is(err, l2.ErrNotFound) {
		return nil, errUnreadable
	}
	if err != nil {
		return nil, err
	}
	// The same test every read of the stance will make (l2.Access): the topic
	// by its opening document, and the stance by every document it cites, as
	// L1 holds them now. A document that is not there fails it.
	proposed := l2.Stance{Evidence: evidence}
	access, err := c.graph.Access(ctx, []l2.Topic{topic}, []l2.Stance{proposed})
	if err != nil {
		return nil, err
	}
	if !access.Topic(reader, topic) || !access.Stance(reader, proposed) {
		return nil, errUnreadable
	}

	as := l2.Assertion{Topic: topic.ID, Position: position, Evidence: evidence, Agent: caller.Agent, Principal: caller.Principal}
	native, err := json.Marshal(as)
	if err != nil {
		return nil, fmt.Errorf("encoding an assertion: %w", err)
	}
	agent := connector.Identity{Source: AuditSource, Kind: connector.IdentityAgent, NativeID: caller.Agent}
	person := connector.Identity{Source: AuditSource, Kind: connector.IdentityUser, NativeID: caller.Principal}
	nativeID := as.NativeID()
	ev := connector.Event{
		Source:   AuditSource,
		NativeID: nativeID,
		Kind:     connector.KindAssertion,
		Time:     c.now().UTC(),
		Payload: connector.Payload{
			Artifact:  nativeID,
			Container: connector.Container{Kind: connector.ContainerWorkspace, NativeID: "api"},
			Text:      position,
			Author:    &agent,
			// The agent asserted; the person is who it acted for.
			Participants: []connector.Participant{{Identity: person, Role: connector.RoleAuthor}},
			Native:       native,
		},
		// Who may read the stance is decided on every read from all of its
		// evidence as it is then, which an access list written once cannot
		// say: it would neither be the intersection of several documents' lists
		// nor follow a re-sync (ADR-0013). So the event fails closed, as an
		// audit event does, and the stance is the way to read the position.
		ACL: connector.ACL{{Kind: connector.ACLIdentity, Source: AuditSource, NativeID: caller.Principal}},
	}
	id := connector.EventID(AuditSource, nativeID)
	err = pgx.BeginFunc(ctx, c.db, func(tx pgx.Tx) error {
		// A retry is the same event at a later time, which L0 refuses as a
		// rewrite and keeps the first of. The job is asked for again: it
		// collapses into a pending one, and writes nothing where the stance is
		// already there.
		if _, err := l0.New(tx).Append(ctx, ev); err != nil && !errors.Is(err, l0.ErrRewrite) {
			return fmt.Errorf("writing an assertion event: %w", err)
		}
		_, err := queue.Enqueue(ctx, tx, queue.Request{Kind: l2.AssertKind(), TargetID: id, SerialKey: topic.Scope})
		return err
	})
	if err != nil {
		return nil, err
	}
	telemetry.Logger(ctx).DebugContext(ctx, "assertion written", "l0_id", id, "topic", topic.ID,
		"principal", caller.Principal, "agent", caller.Agent, "evidence", len(evidence))
	return Asserted{ID: id}, nil
}

// mayAssert refuses a caller that is not an agent whose class may assert:
// people take positions in their own tools, and an observer writes nothing
// (docs/design.md#access-control). Nothing configures a grant's rights yet
// (readerFor), so the agent's class is the whole of what it may write.
func (c *Calls) mayAssert(caller Caller) error {
	if caller.Agent == "" {
		return fail(http.StatusForbidden, "assert is for agents: name the agent acting for you in the %s header", AgentHeader)
	}
	agent, ok := c.resolver.Principal(caller.Agent)
	if !ok || !agent.Class.Rights().Write.Has(principal.WriteAssert) {
		return fail(http.StatusForbidden, "agent %q is of class %q, which may not assert", caller.Agent, agent.Class)
	}
	return nil
}
