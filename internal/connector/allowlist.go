package connector

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/kpenfound/hearsay/internal/telemetry"
)

// AllowAll is the container entry that widens a source to every container its
// credentials can see. It is spelled out in config, per source, by a person.
const AllowAll = "*"

// Allowlist is the ingest allowlist: which containers of which sources Hearsay
// may write to L0 (docs/design.md#access-control, control point 1). It is built
// from the source configs themselves, so there is no second list to keep in
// step with them, and it is default deny — a source that is not configured, or
// a container that is not listed, is not ingested.
type Allowlist struct {
	// containers maps a source id to its allowed container ids. A source
	// present with an empty set allows nothing.
	containers map[string]map[string]bool
	// folderPrefixes applies only to filesystem vault sources.
	folderPrefixes map[string][]string
}

// NewAllowlist builds the allowlist the configured sources describe.
func NewAllowlist(sources ...SourceConfig) Allowlist {
	a := Allowlist{containers: make(map[string]map[string]bool, len(sources)), folderPrefixes: make(map[string][]string)}
	for _, src := range sources {
		set, ok := a.containers[src.ID]
		if !ok {
			set = make(map[string]bool, len(src.Containers))
			a.containers[src.ID] = set
		}
		for _, c := range src.Containers {
			set[c] = true
			if src.Type == "obsidian" && c != AllowAll {
				a.folderPrefixes[src.ID] = append(a.folderPrefixes[src.ID], c)
			}
		}
	}
	return a
}

// Allows reports whether an event from this container of this source may be
// written. A source that is not configured allows nothing, which is what makes
// a connector running against a source someone removed from config go quiet
// rather than keep writing.
func (a Allowlist) Allows(source, container string) bool {
	set := a.containers[source]
	if len(set) == 0 || container == "" {
		return false
	}
	if set[container] || set[AllowAll] {
		return true
	}
	for _, folder := range a.folderPrefixes[source] {
		if strings.HasPrefix(container, folder+"/") {
			return true
		}
	}
	return false
}

// Gate is the sink a connector writes through, and the only way an event
// reaches L0. It enforces what the runtime is responsible for rather than the
// connector: that an event belongs to this source, that it satisfies the
// contract, that its kind was declared, and that its container is in the ingest
// allowlist.
//
// An event from a container that is not allowlisted is dropped and counted, not
// an error: the connector did nothing wrong, config simply does not cover that
// container. Everything else is a bug in the connector and is returned.
//
// The supervision around this — poll loops, webhook mounting, cursor storage,
// restarts — is [Runtime], which builds one of these per source it hosts. This
// is the part of it that decides what may be written, which the contract fixes.
type Gate struct {
	sink    Sink
	source  string
	kinds   map[Kind]bool
	allow   Allowlist
	dropped atomic.Int64

	// resyncs and wake are set by the runtime for a [Resyncer] it has a
	// [ResyncStore] for; a gate without them records no re-sync.
	resyncs  ResyncStore
	wake     chan struct{}
	pollWake chan struct{}
}

// NewGate returns the gate for one connector: the source it was configured for,
// what it declared it emits, and the ingest allowlist.
func NewGate(sink Sink, source string, desc Descriptor, allow Allowlist) *Gate {
	kinds := make(map[Kind]bool, len(desc.Kinds))
	for _, k := range desc.Kinds {
		kinds[k] = true
	}
	return &Gate{sink: sink, source: source, kinds: kinds, allow: allow}
}

// Emit validates the event, checks it against the allowlist, stamps its id and
// passes it to the sink.
func (g *Gate) Emit(ctx context.Context, ev Event) error {
	if ev.Source != g.source {
		return fmt.Errorf("%w: source %q, connector is configured for %q", ErrForeignSource, ev.Source, g.source)
	}
	if err := ev.Validate(); err != nil {
		return err
	}
	if !g.kinds[ev.Kind] {
		return fmt.Errorf("%w: %q", ErrUndeclaredKind, ev.Kind)
	}
	if !g.allow.Allows(ev.Source, ev.Payload.Container.NativeID) {
		g.dropped.Add(1)
		// Debug, and ids only: a busy container that config excludes would
		// otherwise fill the log, and payload text never goes above debug
		// (ADR-0008).
		telemetry.Logger(ctx).DebugContext(ctx, "event dropped: container is not in the ingest allowlist",
			"source", ev.Source,
			"container", ev.Payload.Container.NativeID,
			"native_id", ev.NativeID,
		)
		return nil
	}

	ev.ID = EventID(ev.Source, ev.NativeID)
	return g.sink.Emit(ctx, ev)
}

// RequestResync implements [ResyncRequester]: it records that a container owes
// a re-sync and wakes the runtime to run it. A container the allowlist does not
// cover owes nothing, because nothing of it was written.
func (g *Gate) RequestResync(ctx context.Context, container string) error {
	if g.resyncs == nil {
		return ErrNoResyncStore
	}
	if !g.allow.Allows(g.source, container) {
		return nil
	}
	if err := g.resyncs.Owe(ctx, g.source, container); err != nil {
		return fmt.Errorf("recording that container %s of source %s owes a re-sync: %w", container, g.source, err)
	}
	select {
	case g.wake <- struct{}{}:
	default:
		// A wake is already pending, and the runtime reads every owed re-sync
		// when it takes it.
	}
	return nil
}

// CurrentArtifact reads only this gate's source. The reader is behind the gate
// because a connector must not bypass the allowlist to emit what it finds.
func (g *Gate) CurrentArtifact(ctx context.Context, source, artifact string) (Event, bool, error) {
	if source != g.source {
		return Event{}, false, ErrForeignSource
	}
	r, ok := g.sink.(ArtifactReader)
	if !ok {
		return Event{}, false, errors.New("sink cannot read current artifacts")
	}
	return r.CurrentArtifact(ctx, source, artifact)
}

// CurrentArtifacts reads this gate's source for change-feed recovery.
func (g *Gate) CurrentArtifacts(ctx context.Context, source string) ([]Event, error) {
	if source != g.source {
		return nil, ErrForeignSource
	}
	r, ok := g.sink.(ArtifactReader)
	if !ok {
		return nil, errors.New("sink cannot read current artifacts")
	}
	return r.CurrentArtifacts(ctx, source)
}

// RequestPoll wakes the polling loop after a verified notification.
func (g *Gate) RequestPoll() {
	if g.pollWake == nil {
		return
	}
	select {
	case g.pollWake <- struct{}{}:
	default:
	}
}

// Dropped is how many events the gate has refused because their container is
// not allowlisted. It is what [Runtime.Health] reports per source, which is
// where an operator asking "why is this source quiet" reads it; a metric of it
// is for whenever ADR-0008's meter provider is wired up.
func (g *Gate) Dropped() int64 { return g.dropped.Load() }
