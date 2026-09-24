package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// FakeType is the connector type [Fake] registers and describes itself as.
const FakeType = "fake"

// ErrClosed is returned by a [Fake] asked to ingest after it has been closed.
// It is what makes a runtime that keeps polling a closed connector fail a test
// rather than quietly work.
var ErrClosed = errors.New("connector is closed")

// Fake is a connector that emits events a test scripted, and the connector
// every other package's tests use. It implements [Poller], [Pusher] and
// [Backfiller], so a runtime can be exercised through any of the three ingest
// paths without a network.
//
// Every exported field is set before the connector is run and not touched
// afterwards: the fake's lock is its own, so writing a field while something is
// polling is a data race the race detector will report. A test that wants to
// add events to a running fake calls [Fake.Enqueue], which takes the lock.
type Fake struct {
	// Type is what Describe reports. It defaults to FakeType.
	Type string
	// Kinds is what Describe declares. It defaults to every core kind, so a
	// scripted event is never rejected for a kind the fake forgot to declare.
	Kinds []Kind
	// Queue is what the next Poll emits. Poll drains it. Use Enqueue to add to
	// it once the fake is running.
	Queue []Event
	// Pages is what Backfill returns, one page per call, oldest first.
	Pages [][]Event
	// PollErr, if set, is what Poll returns instead of emitting.
	PollErr error
	// BackfillErr, if set, is what Backfill returns instead of emitting.
	BackfillErr error
	// Status is what Health reports. It defaults to HealthOK.
	Status HealthStatus

	source      SourceConfig
	container   Container
	mu          sync.Mutex
	closed      bool
	lastEventAt time.Time
}

// NewFake returns a fake connector for a source. Events built with
// [Fake.NewEvent] land in the source's first named container, so a fake wired
// to the allowlist the same config produces is ingested rather than dropped.
func NewFake(src SourceConfig) *Fake {
	container := Container{Kind: ContainerWorkspace, NativeID: src.ID, Name: src.ID}
	for _, c := range src.Containers {
		if c != AllowAll {
			container = Container{Kind: ContainerChannel, NativeID: c, Name: c}
			break
		}
	}
	return &Fake{source: src, container: container}
}

// FakeFactory is a [Factory] for [Fake], for a test that needs a registry with
// a connector in it that works.
func FakeFactory(_ context.Context, src SourceConfig) (Connector, error) {
	return NewFake(src), nil
}

// NewEvent returns a valid event from the fake's source: the shortest way for
// another package's test to get an event that passes [Event.Validate] and the
// gate. Fields a test cares about are set on the result.
func (f *Fake) NewEvent(kind Kind, artifact, text string) Event {
	return Event{
		Source:   f.source.ID,
		NativeID: artifact,
		Kind:     kind,
		Time:     time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC),
		Payload: Payload{
			Artifact:  artifact,
			Container: f.container,
			Text:      text,
			Author: &Identity{
				Source:      f.source.ID,
				Kind:        IdentityUser,
				NativeID:    "u1",
				Handle:      "someone",
				DisplayName: "Someone",
			},
		},
		ACL: ACL{{Kind: ACLPublic}},
	}
}

// Describe implements [Connector].
func (f *Fake) Describe() Descriptor {
	desc := Descriptor{Type: f.Type, Kinds: f.Kinds}
	if desc.Type == "" {
		desc.Type = FakeType
	}
	if desc.Kinds == nil {
		desc.Kinds = CoreKinds()
	}
	return desc
}

// Health implements [Connector].
func (f *Fake) Health(context.Context) Health {
	f.mu.Lock()
	defer f.mu.Unlock()
	status := f.Status
	if status == "" {
		status = HealthOK
	}
	return Health{Status: status, LastEventAt: f.lastEventAt}
}

// Close implements [Connector]. A fake that has been closed refuses to ingest.
func (f *Fake) Close(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

// Enqueue adds events for the next Poll to emit, under the fake's lock, so a
// test may feed a fake that something is already polling.
func (f *Fake) Enqueue(events ...Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Queue = append(f.Queue, events...)
}

// Poll implements [Poller]: it emits and drains Queue.
func (f *Fake) Poll(ctx context.Context, sink Sink) error {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return ErrClosed
	}
	if err := f.PollErr; err != nil {
		f.mu.Unlock()
		return err
	}
	queued := f.Queue
	f.Queue = nil
	f.mu.Unlock()

	for _, ev := range queued {
		if err := f.emit(ctx, sink, ev); err != nil {
			return err
		}
	}
	return nil
}

// Backfill implements [Backfiller]: one page of Pages per call, with the page
// number as the cursor.
func (f *Fake) Backfill(ctx context.Context, sink Sink, from Cursor) (BackfillResult, error) {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return BackfillResult{}, ErrClosed
	}
	if err := f.BackfillErr; err != nil {
		f.mu.Unlock()
		return BackfillResult{}, err
	}
	pages := f.Pages
	f.mu.Unlock()

	page := 0
	if from != "" {
		n, err := strconv.Atoi(string(from))
		if err != nil {
			return BackfillResult{}, fmt.Errorf("fake backfill cursor %q: %w", from, err)
		}
		if n < 0 {
			return BackfillResult{}, fmt.Errorf("fake backfill cursor %q: negative page", from)
		}
		page = n
	}
	if page >= len(pages) {
		return BackfillResult{Done: true}, nil
	}
	for _, ev := range pages[page] {
		if err := f.emit(ctx, sink, ev); err != nil {
			return BackfillResult{}, err
		}
	}
	return BackfillResult{
		Next:   Cursor(strconv.Itoa(page + 1)),
		Done:   page+1 >= len(pages),
		Events: len(pages[page]),
	}, nil
}

// Handler implements [Pusher]. It emits the event, or array of events, posted
// to it, which is as close as a fake gets to a webhook.
//
// A post with the query parameter `command` is a chat command instead: the
// body is a [Command], handed to the sink's [CommandSink], and the answer is
// the [CommandResult] as JSON — or 204 and no body where the source takes no
// commands, which is a source that answers nothing.
func (f *Fake) Handler(sink Sink) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "post an event", http.StatusMethodNotAllowed)
			return
		}
		// A webhook body is small; a fake does not need to be generous.
		const maxBody = 1 << 20
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
		if err != nil {
			http.Error(w, "reading body", http.StatusBadRequest)
			return
		}
		if r.URL.Query().Has("command") {
			f.command(w, r, sink, body)
			return
		}

		var events []Event
		if bytes.HasPrefix(bytes.TrimLeft(body, " \t\r\n"), []byte("[")) {
			err = json.Unmarshal(body, &events)
		} else {
			var one Event
			if err = json.Unmarshal(body, &one); err == nil {
				events = []Event{one}
			}
		}
		if err != nil {
			http.Error(w, "body is neither an event nor an array of events", http.StatusBadRequest)
			return
		}
		for _, ev := range events {
			if err := f.emit(r.Context(), sink, ev); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		w.WriteHeader(http.StatusAccepted)
	})
}

// command hands a posted command to the runtime and answers with its result.
func (f *Fake) command(w http.ResponseWriter, r *http.Request, sink Sink, body []byte) {
	var cmd Command
	if err := json.Unmarshal(body, &cmd); err != nil {
		http.Error(w, "body is not a command", http.StatusBadRequest)
		return
	}
	commands, ok := sink.(CommandSink)
	if !ok {
		http.Error(w, "the sink takes no commands", http.StatusNotImplemented)
		return
	}
	res, err := commands.Command(r.Context(), cmd)
	switch {
	case errors.Is(err, ErrReadOnly):
		w.WriteHeader(http.StatusNoContent)
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(res)
}

func (f *Fake) emit(ctx context.Context, sink Sink, ev Event) error {
	if err := sink.Emit(ctx, ev); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if ev.Time.After(f.lastEventAt) {
		f.lastEventAt = ev.Time
	}
	return nil
}

// Recorder is a [Sink] that keeps what it was given, for a test that asserts on
// what a connector emitted.
type Recorder struct {
	// Err, if set, is returned by every Emit, so a test can exercise what a
	// connector does when the store is failing.
	Err error

	mu     sync.Mutex
	events []Event
	at     []time.Time
}

// Emit implements [Sink]. An event emitted before is recorded again, where L0
// would write nothing: a test asserting on what a connector sent wants both.
func (r *Recorder) Emit(_ context.Context, ev Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Err != nil {
		return r.Err
	}
	r.events = append(r.events, ev)
	r.at = append(r.at, time.Now())
	return nil
}

// CurrentArtifact implements ArtifactReader for connector tests.
func (r *Recorder) CurrentArtifact(_ context.Context, source, artifact string) (Event, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var current Event
	found := false
	for _, ev := range r.events {
		if ev.Source != source {
			continue
		}
		if ev.Kind == KindTombstone && ev.Payload.Target == artifact {
			found = false
			continue
		}
		if ev.Payload.Artifact == artifact && ev.Kind != KindTombstone {
			current, found = ev, true
		}
	}
	return current, found, nil
}

// LastRetraction implements RetractionReader for connector tests.
func (r *Recorder) LastRetraction(_ context.Context, source, artifact string) (Event, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.events) - 1; i >= 0; i-- {
		ev := r.events[i]
		if ev.Source == source && ev.Kind == KindTombstone && ev.Payload.Target == artifact {
			return ev, true, nil
		}
	}
	return Event{}, false, nil
}

// CurrentArtifacts implements ArtifactReader for connector tests.
func (r *Recorder) CurrentArtifacts(_ context.Context, source string) ([]Event, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	current := map[string]Event{}
	for _, ev := range r.events {
		if ev.Source != source {
			continue
		}
		if ev.Kind == KindTombstone {
			delete(current, ev.Payload.Target)
		} else {
			current[ev.Payload.Artifact] = ev
		}
	}
	out := make([]Event, 0, len(current))
	for _, ev := range current {
		out = append(out, ev)
	}
	slices.SortFunc(out, func(a, b Event) int { return strings.Compare(a.Payload.Artifact, b.Payload.Artifact) })
	return out, nil
}

// Exposed implements [ExposureReader] over what was emitted, the way L0 reads
// it for a connector that emits revisions in order: an artifact's current
// revision is the one that arrived last, a retracted artifact is not served,
// and a tombstone is not an artifact anyone reads.
func (r *Recorder) Exposed(_ context.Context, source string) ([]Exposure, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	current := map[string]int{}
	retracted := map[string]bool{}
	for i, ev := range r.events {
		if ev.Source != source {
			continue
		}
		if ev.Kind == KindTombstone {
			retracted[ev.Payload.Target] = true
			continue
		}
		current[ev.Payload.Artifact] = i
	}
	last := map[string]time.Time{}
	for artifact, i := range current {
		ev := r.events[i]
		if retracted[artifact] || !ev.ACL.public() {
			continue
		}
		if c := ev.Payload.Container.NativeID; r.at[i].After(last[c]) {
			last[c] = r.at[i]
		}
	}
	out := []Exposure{}
	for c, at := range last {
		out = append(out, Exposure{Container: c, LastPublic: at})
	}
	slices.SortFunc(out, func(a, b Exposure) int { return strings.Compare(a.Container, b.Container) })
	return out, nil
}

// Events returns a copy of what has been emitted, oldest first.
func (r *Recorder) Events() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...)
}

// IDs returns the ids of what has been emitted, which is usually what a test
// wants to compare.
func (r *Recorder) IDs() []string {
	ids := []string{}
	for _, ev := range r.Events() {
		ids = append(ids, ev.ID)
	}
	return ids
}
