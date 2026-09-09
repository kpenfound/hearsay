package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Errors the contract defines. A runtime distinguishes them; a connector
// returns them only through the functions that document them.
var (
	// ErrUnknownType is returned when no factory is registered for a source's
	// connector type.
	ErrUnknownType = errors.New("unknown connector type")
	// ErrDuplicateType is returned when two factories claim the same type.
	ErrDuplicateType = errors.New("connector type already registered")
	// ErrNoIngestMode is returned for a connector that implements neither
	// [Poller] nor [Pusher] and so could never produce an event.
	ErrNoIngestMode = errors.New("connector implements neither Poller nor Pusher")
	// ErrForeignSource is returned when a connector emits an event for a source
	// other than the one it was configured for.
	ErrForeignSource = errors.New("event is from another source")
	// ErrUndeclaredKind is returned when a connector emits a kind its
	// descriptor does not list.
	ErrUndeclaredKind = errors.New("kind is not declared by the connector")
)

// Sink is where a connector puts events. The runtime supplies it; the
// implementation behind it is the L0 store.
//
// Emit is safe for concurrent use, and safe to call with an event that has been
// emitted before: ingest is idempotent on the event id, so a connector that
// cannot remember what it sent should re-send rather than guess.
type Sink interface {
	Emit(ctx context.Context, ev Event) error
}

// Connector is what a source module implements. It is deliberately small: on
// its own a Connector cannot produce anything, and it must also implement
// [Poller] or [Pusher], which is how it ingests. [Backfiller] is optional and
// adds history.
//
// The lifecycle is: the runtime builds one connector per configured source with
// a [Factory], ingests through Poller or Pusher until the process is shutting
// down, and calls Close. A connector owns no goroutine that outlives Close.
type Connector interface {
	// Describe returns what this connector is and what it emits. It is called
	// after construction and must not block.
	Describe() Descriptor

	// Health reports whether ingest is working, for /healthz and for an
	// operator asking why a source is quiet. It must return promptly and must
	// not perform a network call on the request path.
	Health(ctx context.Context) Health

	// Close releases everything the connector holds. It is called once, and
	// returns when every goroutine the connector started has stopped or ctx is
	// done.
	Close(ctx context.Context) error
}

// Poller is a connector that fetches. The runtime calls Poll on the source's
// refresh cadence and never concurrently with itself, so a poller keeps its
// position in memory and does not have to lock.
//
// Poll returns when it has emitted what one pass found. A poller that returns
// an error is retried on the next tick with backoff.
type Poller interface {
	Connector
	Poll(ctx context.Context, sink Sink) error
}

// Pusher is a connector the source calls: a webhook, or a socket the connector
// itself dials from Handler's lifetime. The runtime mounts the handler under a
// path it owns and hands it a sink.
//
// The handler verifies the source's own signature over the request. The runtime
// cannot: the scheme is the source's. A handler that cannot verify a delivery
// rejects it and emits nothing.
type Pusher interface {
	Connector
	Handler(sink Sink) http.Handler
}

// Backfiller is a connector that can read history. The runtime calls Backfill
// with the cursor the last call returned, and stores the cursor it gets back,
// so that a backfill interrupted by a restart resumes rather than starting
// again. The first call gets the zero [Cursor].
//
// One call does a bounded amount of work — a page, a day, whatever the source's
// API pages by — and returns. Backfill emits the same events live ingest would,
// and re-emitting is free, so the two overlapping is expected rather than
// something a connector must avoid.
type Backfiller interface {
	Connector
	Backfill(ctx context.Context, sink Sink, from Cursor) (BackfillResult, error)
}

// Cursor is a connector's position in a backfill. It is opaque to Hearsay,
// which only stores it and hands it back, so a connector may put a page token,
// a timestamp or a JSON object in it. It must survive a restart, so it may not
// refer to anything the connector holds in memory.
type Cursor string

// BackfillResult is what one Backfill call achieved.
type BackfillResult struct {
	// Next is the cursor to resume from. It is ignored when Done is set.
	Next Cursor
	// Done reports that history is exhausted and the runtime may stop calling
	// Backfill for this source.
	Done bool
	// Events is how many events the call emitted, for progress reporting. It is
	// not load-bearing: the sink is what counts.
	Events int
}

// Descriptor is a connector's self-description.
type Descriptor struct {
	// Type is the connector type the source config selects it by: `github`,
	// `discord`, `drive`. It must equal the type it is registered under.
	Type string
	// Kinds is every kind the connector can emit, including the extension
	// kinds it defines. It is a declaration the runtime enforces: emitting an
	// undeclared kind is an error, so that what a source can produce is
	// readable from config rather than discovered in production.
	Kinds []Kind
}

// Health is a connector's own account of whether it is working.
type Health struct {
	Status HealthStatus
	// Detail says what is wrong, for a human. It carries no credentials, no
	// event text and no personal data: health is served more widely than L0.
	Detail string
	// LastEventAt is when the connector last emitted, which is how an operator
	// tells a quiet source from a stuck one.
	LastEventAt time.Time
}

// HealthStatus is the state of a connector.
type HealthStatus string

// The health statuses.
const (
	// HealthOK means ingest is working.
	HealthOK HealthStatus = "ok"
	// HealthDegraded means ingest is working but behind or partial — rate
	// limited, one container failing. The runtime keeps it running.
	HealthDegraded HealthStatus = "degraded"
	// HealthFailed means the connector is not ingesting and cannot recover by
	// itself: a revoked token, a deleted container.
	HealthFailed HealthStatus = "failed"
)

// SourceConfig is one entry of the config repository's `sources/` directory,
// parsed: what a connector is told about the source it serves. The on-disk
// format is docs/config.md, and `internal/config` produces these values
// directly, so there is no second shape between the file and the connector.
type SourceConfig struct {
	// ID is the source id, unique across the deployment. It appears in the
	// Source field of every event the connector emits and in every event id, so
	// it is chosen once and not renamed.
	ID string
	// Type selects the connector: the type a factory is registered under.
	Type string
	// Containers are the repositories, channels or folders this source may
	// ingest, by native id. It is the ingest allowlist for the source, it is
	// default deny, and the single entry "*" widens it to every container the
	// credentials can see — which is a deliberate choice a human makes, not a
	// default.
	Containers []string
	// Refresh is how often the runtime calls Poll. It is ignored by a connector
	// that only pushes, and the runtime applies its own floor and jitter.
	Refresh time.Duration
	// Settings is the connector's own configuration, as JSON. A connector
	// decodes it with DecodeSettings and fails construction if it cannot.
	Settings json.RawMessage
	// Secrets are the credentials the source needs, resolved by the runtime
	// from the environment. They never live in the config repository, which is
	// checked in: config names a secret, and the runtime supplies its value.
	Secrets map[string]string
}

// DecodeSettings decodes the source's connector-specific settings into v,
// rejecting any field v does not have, so that a typo in config fails at
// startup instead of being ignored quietly. Empty settings leave v untouched.
func (s SourceConfig) DecodeSettings(v any) error {
	if len(s.Settings) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(s.Settings))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("decoding settings for source %q: %w", s.ID, err)
	}
	return nil
}

// Factory builds a connector for one configured source. It validates the
// config, establishes whatever the source needs, and returns an error rather
// than a connector that will fail later: a source that cannot start is a
// startup failure, not a health status.
type Factory func(ctx context.Context, src SourceConfig) (Connector, error)

// Registry maps connector types to their factories. It is how a third-party
// connector is plugged in: the module exports a Factory, and whoever builds the
// runtime registers it. There is no global registry and no init-time
// registration, so what a binary can ingest is readable from its wiring.
type Registry struct {
	mu        sync.RWMutex
	factories map[string]Factory
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{factories: make(map[string]Factory)}
}

// Register adds a factory under a connector type. Registering a type twice is
// an error rather than a replacement: two connectors claiming `github` is a
// wiring bug, and silently keeping one of them hides it.
func (r *Registry) Register(connectorType string, f Factory) error {
	if connectorType == "" {
		return errors.New("registering a connector: type is empty")
	}
	if f == nil {
		return fmt.Errorf("registering connector type %q: factory is nil", connectorType)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.factories[connectorType]; ok {
		return fmt.Errorf("%w: %q", ErrDuplicateType, connectorType)
	}
	r.factories[connectorType] = f
	return nil
}

// New builds the connector a source config asks for, and checks the two things
// about it that must be true before it is run: it ingests somehow, and it
// describes itself as the type it was registered under.
func (r *Registry) New(ctx context.Context, src SourceConfig) (Connector, error) {
	if !ValidSourceID(src.ID) {
		return nil, fmt.Errorf("source %q: id is not a source id (lowercase letters, digits, - and _)", src.ID)
	}
	r.mu.RLock()
	f, ok := r.factories[src.Type]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("source %q: %w: %q", src.ID, ErrUnknownType, src.Type)
	}

	c, err := f(ctx, src)
	if err != nil {
		return nil, fmt.Errorf("source %q: building connector %q: %w", src.ID, src.Type, err)
	}
	// A factory establishes what the source needs, so a connector this rejects
	// may already hold a socket and a goroutine reading it. Nothing else can
	// close it — New is the only thing holding it — so New does.
	_, poller := c.(Poller)
	_, pusher := c.(Pusher)
	if !poller && !pusher {
		return nil, closeRejected(ctx, c, fmt.Errorf("source %q: connector %q: %w", src.ID, src.Type, ErrNoIngestMode))
	}
	if desc := c.Describe(); desc.Type != src.Type {
		return nil, closeRejected(ctx, c, fmt.Errorf("source %q: connector registered as %q describes itself as %q", src.ID, src.Type, desc.Type))
	}
	return c, nil
}

// closeRejected closes a connector New is about to throw away, and returns the
// reason it is being thrown away with any close failure joined to it: the
// rejection is what the caller needs to read first, and a connector that also
// fails to shut down is worth knowing about.
func closeRejected(ctx context.Context, c Connector, reason error) error {
	if err := c.Close(ctx); err != nil {
		return errors.Join(reason, fmt.Errorf("closing the rejected connector: %w", err))
	}
	return reason
}
