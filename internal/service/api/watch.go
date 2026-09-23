package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/principal"
	"github.com/kpenfound/hearsay/internal/queue"
	"github.com/kpenfound/hearsay/internal/service/distiller"
)

// How long `watch` waits.
const (
	// DefaultWatchWait is how long a watch that finds nothing waits for a
	// notification before it returns an empty list: long enough that a
	// consumer is not polling, short enough for the proxies between it and
	// Hearsay to leave the request alone.
	DefaultWatchWait = 25 * time.Second
	// DefaultWatchPoll is how often a waiting watch reads the feed again. It
	// holds no database connection in between.
	DefaultWatchPoll = time.Second
	// MaxNotifications is the most one watch returns. The cursor it returns is
	// where the next one picks up.
	MaxNotifications = 100
)

// WithWatch sets how long a watch waits for a notification before it returns
// an empty list, and how often it reads the feed while it waits. Zero is the
// default for either.
func (c *Calls) WithWatch(wait, poll time.Duration) {
	if wait <= 0 {
		wait = DefaultWatchWait
	}
	if poll <= 0 {
		poll = DefaultWatchPoll
	}
	c.watchWait, c.watchPoll = wait, poll
}

// stopWatches ends every watch that is waiting, each with what it has, so a
// server shutting down is not held for a wait's length. It is idempotent.
func (c *Calls) stopWatches() { c.stopOnce.Do(func() { close(c.stopping) }) }

// Notification is one event `watch` delivers: the handles to it and to the
// document it is in, and no content. The consumer follows the handles with
// get_l0 and get_l1.
type Notification struct {
	Event    string         `json:"event"`
	Kind     connector.Kind `json:"kind"`
	Source   string         `json:"source"`
	Time     time.Time      `json:"time"`
	Document string         `json:"document"`
}

// Watched is what `watch` returns: the notifications after the cursor it was
// given, oldest first, and the cursor to pass next.
type Watched struct {
	Notifications []Notification `json:"notifications"`
	Cursor        string         `json:"cursor"`
}

// WatchFilter narrows a watch to some event kinds and sources. An empty list
// is every kind or every source. A kind matches an event of that kind, or an
// extension kind that declares it as its base kind.
type WatchFilter struct {
	Kinds   []string `json:"kinds"`
	Sources []string `json:"sources"`
}

func (f WatchFilter) matches(ev connector.Event) bool {
	if len(f.Kinds) > 0 && !slices.Contains(f.Kinds, string(ev.Kind)) &&
		(ev.Payload.BaseKind == "" || !slices.Contains(f.Kinds, string(ev.Payload.BaseKind))) {
		return false
	}
	return len(f.Sources) == 0 || slices.Contains(f.Sources, ev.Source)
}

// watchCursorVersion prefixes the position a watch cursor carries, so that the
// form can change without a consumer's stored cursor being misread.
const watchCursorVersion = "w1:"

// encodeWatchCursor is a feed position as the opaque cursor `watch` serves. It
// is never empty, the beginning of the feed included: an empty cursor is a
// consumer starting from now.
func encodeWatchCursor(c l0.Cursor) string {
	return base64.RawURLEncoding.EncodeToString([]byte(watchCursorVersion + c.String()))
}

func decodeWatchCursor(s string) (l0.Cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return l0.Cursor{}, err
	}
	position, ok := strings.CutPrefix(string(raw), watchCursorVersion)
	if !ok {
		return l0.Cursor{}, errors.New("not a watch cursor")
	}
	return l0.ParseCursor(position)
}

// watch is `watch(scope, filter, after?)` (docs/design.md#read-and-assert-api):
// the L0 events after a cursor that are on a scope, in change-feed order, as
// notifications. It is a long poll with no state kept between calls: when
// there is nothing after the cursor it waits, reading the feed every
// [DefaultWatchPoll], and returns an empty list once [DefaultWatchWait] has
// passed, the request is cancelled or the server is stopping.
//
// An event is on the scope when a document built from it is about the scope
// or an entity `part_of` it, in the caller's reach, and the caller may read the
// document and the event. That is known once the event is distilled, so a
// watch delivers an event then, and holds its cursor at the first event whose
// distillation has not landed — one the distiller has not read off the feed
// yet, or whose document's job is still to run — so that it cannot pass one it
// would have delivered. An event that is never distilled (agent session
// events, Hearsay's own audit and assertion events) or that a finished
// distillation left out of every document — a tombstone, a revision a later
// one replaced — is passed without a notification.
//
// What the caller may not see is passed the same way as what is not on the
// scope: the cursor moves over it, and the response carries no position of any
// event, so there is no gap to see. The cursor is opaque.
func watch(ctx context.Context, c *Calls, caller Caller, reader l1.Reader, raw json.RawMessage) (any, error) {
	if err := c.mayWatch(caller); err != nil {
		return nil, err
	}
	var args struct {
		Scope  string      `json:"scope"`
		Filter WatchFilter `json:"filter"`
		After  string      `json:"after"`
	}
	if err := decode(raw, &args); err != nil {
		return nil, err
	}
	if err := required("scope", args.Scope); err != nil {
		return nil, err
	}
	var from l0.Cursor
	var err error
	if args.After == "" {
		from, err = c.events.Head(ctx)
	} else if from, err = decodeWatchCursor(args.After); err != nil {
		return nil, fail(http.StatusBadRequest, "after is not a cursor a watch returned")
	}
	if err != nil {
		return nil, err
	}
	// What the scope covers is read once: a watch is tens of seconds.
	covered, err := c.reach.Descendants(ctx, []string{args.Scope})
	if err != nil {
		return nil, fmt.Errorf("reading what %s covers: %w", args.Scope, err)
	}
	// Only what is in reach: a scope out of it is watched as one that does not
	// exist, as its bundle is.
	on := map[string]bool{}
	for _, id := range covered {
		if reader.InReach([]string{id}) {
			on[id] = true
		}
	}

	wait := time.NewTimer(c.watchWait)
	defer wait.Stop()
	poll := time.NewTicker(c.watchPoll)
	defer poll.Stop()
	for {
		notes, next, err := c.scan(ctx, reader, on, args.Filter, from)
		if err != nil {
			if ctx.Err() != nil {
				return nil, errCancelled
			}
			return nil, err
		}
		from = next
		if len(notes) > 0 {
			return Watched{Notifications: notes, Cursor: encodeWatchCursor(from)}, nil
		}
		select {
		case <-ctx.Done():
			return nil, errCancelled
		case <-c.stopping:
			return Watched{Notifications: []Notification{}, Cursor: encodeWatchCursor(from)}, nil
		case <-wait.C:
			return Watched{Notifications: []Notification{}, Cursor: encodeWatchCursor(from)}, nil
		case <-poll.C:
		}
	}
}

// errCancelled is a watch whose request went away while it waited. Nobody is
// left to read it.
var errCancelled = &Error{Status: http.StatusServiceUnavailable, Message: "the watch was cancelled"}

// scan reads the feed after a cursor and returns the notifications it holds
// for this reader, and how far it got: to the end of the feed, to the first
// event still waiting on its distillation, or to the last of
// [MaxNotifications]. Every read is a statement on the pool; nothing holds a
// connection between them.
func (c *Calls) scan(ctx context.Context, reader l1.Reader, on map[string]bool, filter WatchFilter, from l0.Cursor) ([]Notification, l0.Cursor, error) {
	notes := []Notification{}
	for {
		changes, err := c.events.Changes(ctx, from, l0.Filter{}, l0.MaxLimit)
		if err != nil {
			return nil, from, err
		}
		if len(changes) == 0 {
			return notes, from, nil
		}
		// The events that could be delivered, and the document each belongs
		// to. The rest are passed as they are read.
		candidates := map[string]string{}
		ids, targets := []string{}, []string{}
		for _, change := range changes {
			ev := change.Event
			if !c.mayNotify(reader, filter, ev) {
				continue
			}
			target, ok, err := distiller.TargetOf(ctx, ev, c.events)
			if err != nil {
				return nil, from, err
			}
			if ok {
				candidates[ev.ID] = target
				ids, targets = append(ids, ev.ID), append(targets, target)
			}
		}
		// Read in this order, each in its own snapshot: where the distiller
		// has read the feed to, then which documents' jobs are unfinished,
		// then which documents hold each event. The pump enqueues a job in the
		// transaction that moves its cursor, and a job is finished only after
		// the documents it wrote are committed, so an event the distiller has
		// passed is either in a document by the third read or has a job the
		// second one saw.
		distilled, err := l0.NewCursors(c.db).Load(ctx, distiller.Consumer)
		if err != nil {
			return nil, from, err
		}
		unfinished, err := queue.Unfinished(ctx, c.db, distiller.JobKind(), targets)
		if err != nil {
			return nil, from, err
		}
		holders, err := c.docs.Holders(ctx, ids)
		if err != nil {
			return nil, from, err
		}
		for _, change := range changes {
			ev := change.Event
			target, candidate := candidates[ev.ID]
			held := holders[ev.ID]
			if candidate && len(held) == 0 && (distilled.Compare(change.Cursor) < 0 || unfinished[target]) {
				// Not distilled yet: wait here rather than pass it.
				return notes, from, nil
			}
			from = change.Cursor
			if !candidate {
				continue
			}
			if doc, ok := onScope(reader, on, held); ok {
				notes = append(notes, Notification{Event: ev.ID, Kind: ev.Kind, Source: ev.Source, Time: ev.Time, Document: doc})
				if len(notes) == MaxNotifications {
					return notes, from, nil
				}
			}
		}
		if len(notes) > 0 || len(changes) < l0.MaxLimit {
			return notes, from, nil
		}
	}
}

// mayNotify reports whether an event could be a notification for this reader
// whatever it is distilled into: it is not Hearsay's own, the filter takes it,
// and its access list lets the reader read it. An audit or assertion event
// belongs to no document, so the distiller never passes one on either; the
// source is refused here so that no document naming one can change that.
func (c *Calls) mayNotify(reader l1.Reader, filter WatchFilter, ev connector.Event) bool {
	switch {
	case ev.Source == AuditSource:
		return false
	case !filter.matches(ev):
		return false
	}
	return reader.Allows(ev.ACL)
}

// onScope is the first document, by id, of those holding an event that is
// about an entity the watch covers and that the reader may read.
func onScope(reader l1.Reader, on map[string]bool, held []l1.Holder) (string, bool) {
	for _, h := range held {
		if reader.Allows(h.ACL) && slices.ContainsFunc(h.Scope, func(id string) bool { return on[id] }) {
			return h.ID, true
		}
	}
	return "", false
}

// mayWatch refuses an agent whose class may not subscribe
// (docs/design.md#access-control). A person calling directly may watch.
func (c *Calls) mayWatch(caller Caller) error {
	if caller.Agent == "" {
		return nil
	}
	agent, ok := c.resolver.Principal(caller.Agent)
	if !ok || !agent.Class.Rights().Write.Has(principal.WriteSubscribe) {
		return fail(http.StatusForbidden, "agent %q is of class %q, which may not watch", caller.Agent, agent.Class)
	}
	return nil
}
