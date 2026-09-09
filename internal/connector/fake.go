package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
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
}

// Emit implements [Sink].
func (r *Recorder) Emit(_ context.Context, ev Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Err != nil {
		return r.Err
	}
	r.events = append(r.events, ev)
	return nil
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
