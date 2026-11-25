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

const watchCursorVersion = "w1:"

func encodeWatchCursor(m l0.Mark) string {
	return base64.RawURLEncoding.EncodeToString([]byte(watchCursorVersion + m.String()))
}

func decodeWatchCursor(s string) (l0.Mark, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return l0.Mark{}, err
	}
	position, ok := strings.CutPrefix(string(raw), watchCursorVersion)
	if !ok {
		return l0.Mark{}, errors.New("not a watch cursor")
	}
	return l0.ParseMark(position)
}

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

	var from l0.Mark
	var err error
	if args.After == "" {
		from, err = c.events.Now(ctx)
	} else if from, err = decodeWatchCursor(args.After); err != nil {
		return nil, fail(http.StatusBadRequest, "after is not a cursor a watch returned")
	}
	if err != nil {
		return nil, err
	}
	hidden, err := c.graph.HiddenWorkEntities(ctx, reader)
	if err != nil {
		return nil, err
	}

	covered, err := c.graph.Descendants(ctx, []string{args.Scope})
	if err != nil {
		return nil, fmt.Errorf("reading what %s covers: %w", args.Scope, err)
	}

	on := map[string]bool{}
	for _, id := range covered {
		if !hidden.Hidden(args.Scope) && !hidden.Hidden(id) && reader.InReach([]string{id}) {
			on[id] = true
		}
	}

	deadline := time.Now().Add(c.watchWait)
	wait := time.NewTimer(c.watchWait)
	defer wait.Stop()
	poll := time.NewTicker(c.watchPoll)
	defer poll.Stop()
	for {
		notes, next, err := c.scan(ctx, reader, on, args.Filter, from, deadline)
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

var errCancelled = &Error{Status: http.StatusServiceUnavailable, Message: "the watch was cancelled"}

func (c *Calls) scan(ctx context.Context, reader l1.Reader, on map[string]bool, filter WatchFilter, from l0.Mark, deadline time.Time) ([]Notification, l0.Mark, error) {
	notes := []Notification{}
	for {
		changes, err := c.events.Changes(ctx, from.From, l0.Filter{}, l0.MaxLimit)
		if err != nil {
			return nil, from, err
		}
		if len(changes) == 0 {
			return notes, from, nil
		}

		candidates := map[string]string{}
		ids, targets := []string{}, []string{}
		for _, change := range changes {
			ev := change.Event
			if from.Committed(change.Cursor) || !c.mayNotify(reader, filter, ev) {
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
				return notes, from, nil
			}
			from = from.Past(change.Cursor)
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
		if len(changes) < l0.MaxLimit || !time.Now().Before(deadline) {
			return notes, from, nil
		}
	}
}

func (c *Calls) mayNotify(reader l1.Reader, filter WatchFilter, ev connector.Event) bool {
	switch {
	case ev.Source == AuditSource:
		return false
	case !filter.matches(ev):
		return false
	}
	return reader.Allows(ev.ACL)
}

func onScope(reader l1.Reader, on map[string]bool, held []l1.Holder) (string, bool) {
	for _, h := range held {
		if reader.Allows(h.ACL) && slices.ContainsFunc(h.Scope, func(id string) bool { return on[id] }) {
			return h.ID, true
		}
	}
	return "", false
}

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
