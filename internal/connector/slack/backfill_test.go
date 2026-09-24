package slack_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/slack"
)

func TestBackfillPagesRepliesAndRestarts(t *testing.T) {
	fake := newFakeSlack(t, func(int, *websocket.Conn) {})
	calls := []string{}
	fake.history = func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/")
		cursor := r.FormValue("cursor")
		calls = append(calls, path+":"+cursor)
		switch path + ":" + cursor {
		case "conversations.history:":
			fmt.Fprint(w, `{"ok":true,"messages":[{"ts":"1758700000.000100","user":"U0KYLE","text":"root","reply_count":1}],"response_metadata":{"next_cursor":"h2"}}`)
		case "conversations.replies:":
			fmt.Fprint(w, `{"ok":true,"messages":[{"ts":"1758700000.000100","user":"U0KYLE","text":"root"},{"ts":"1758700060.000200","user":"U0SAM","text":"reply"}],"response_metadata":{"next_cursor":"r2"}}`)
		case "conversations.replies:r2":
			fmt.Fprint(w, `{"ok":true,"messages":[],"response_metadata":{"next_cursor":""}}`)
		case "conversations.history:h2":
			fmt.Fprint(w, `{"ok":true,"messages":[],"response_metadata":{"next_cursor":""}}`)
		default:
			t.Errorf("unexpected page %s:%s", path, cursor)
		}
	}
	rec := &deduplicating{seen: map[string]bool{}}
	gate := connector.NewGate(rec, fake.resolved().ID, (&slack.Connector{}).Describe(), connector.NewAllowlist(fake.resolved()))
	var from connector.Cursor
	for pass := 0; pass < 8; pass++ {
		c, err := slack.New(fake.resolved())
		if err != nil {
			t.Fatal(err)
		}
		result, err := c.Backfill(t.Context(), gate, from)
		if err != nil {
			t.Fatal(err)
		}
		if result.Done {
			break
		}
		if result.Next == from {
			t.Fatal("cursor did not advance")
		}
		from = result.Next
		if pass == 7 {
			t.Fatal("backfill did not finish")
		}
	}
	events := rec.recorder.Events()
	if len(events) != 2 {
		t.Fatalf("stored events = %d, want root and reply", len(events))
	}
	if deliveries := rec.delivered(); len(deliveries) != 3 || deliveries[0] != deliveries[1] {
		t.Errorf("deliveries = %v, want identical root revision replay", deliveries)
	}
	if events[1].Payload.Thread != public+"/1758700000.000100" {
		t.Errorf("reply thread = %q", events[1].Payload.Thread)
	}
	if len(calls) != 4 {
		t.Errorf("calls = %v", calls)
	}
}

func TestBackfillRateLimitLeavesCursor(t *testing.T) {
	fake := newFakeSlack(t, func(int, *websocket.Conn) {})
	hits := 0
	fake.history = func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			w.Header().Set("Retry-After", "0.001")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, `{"ok":true,"messages":[]}`)
	}
	c, err := slack.New(fake.resolved())
	if err != nil {
		t.Fatal(err)
	}
	rec := &connector.Recorder{}
	if _, err = c.Backfill(t.Context(), rec, ""); err == nil {
		t.Fatal("want rate limit error")
	}
	result, err := c.Backfill(t.Context(), rec, "")
	if err != nil || !result.Done || hits != 2 {
		t.Fatalf("retry = %+v, %v, hits %d", result, err, hits)
	}
}

func TestResyncRevisesPriorEventsAndRetries(t *testing.T) {
	fake := newFakeSlack(t, func(int, *websocket.Conn) {})
	c, err := slack.New(fake.resolved())
	if err != nil {
		t.Fatal(err)
	}
	rec := &connector.Recorder{}
	// A public artifact already in L0 before the visibility transition.
	ev := connector.Event{Source: "slack-acme", NativeID: public + "/1758700000.000100@v1+perm:public", Kind: connector.KindMessage, Time: time.Unix(1758700000, 100000).UTC(), ACL: connector.ACL{{Kind: connector.ACLPublic}}, Payload: connector.Payload{Artifact: public + "/1758700000.000100", Container: connector.Container{Kind: connector.ContainerChannel, NativeID: public}, Text: "root", Author: &connector.Identity{Source: "slack-acme", Kind: connector.IdentityUser, NativeID: "U0KYLE"}, Revision: &connector.Revision{Token: "v1+perm:public"}}}
	if err := rec.Emit(t.Context(), ev); err != nil {
		t.Fatal(err)
	}
	first, err := c.Resync(t.Context(), rec, public, "")
	if err != nil || !first.Done {
		t.Fatalf("resync = %+v, %v", first, err)
	}
	events := rec.Events()
	if len(events) != 2 || events[1].ACL[0].Kind != connector.ACLGroup || events[1].Payload.Revision.Token != "v1+perm:private" {
		t.Fatalf("events = %+v", events)
	}
	again, err := c.Resync(t.Context(), rec, public, "")
	if err != nil || !again.Done || len(rec.Events()) != 2 {
		t.Errorf("replay = %+v, %v, events %d", again, err, len(rec.Events()))
	}
}

type owedSink struct {
	connector.Recorder
	requests int
	fail     bool
}

func (s *owedSink) RequestResync(_ context.Context, channel string) error {
	s.requests++
	if channel != public {
		return fmt.Errorf("wrong channel %s", channel)
	}
	if s.fail {
		s.fail = false
		return errors.New("resync store unavailable")
	}
	return nil
}

func TestArchiveTransitionRequestsResyncBeforeAckAndRetries(t *testing.T) {
	archived := `{"id":"C0PUBLIC","is_channel":true,"is_archived":true,"is_member":true}`
	var fake *fakeSlack
	fake = newFakeSlack(t, func(n int, ws *websocket.Conn) {
		if n == 1 {
			_ = ws.WriteJSON(map[string]any{"type": "hello"})
			fake.mu.Lock()
			fake.channels[public] = archived
			fake.mu.Unlock()
			_ = ws.WriteJSON(map[string]any{"type": "events_api", "envelope_id": "one", "payload": map[string]any{"type": "event_callback", "team_id": "T0001", "event": map[string]any{"type": "channel_archive", "channel": public}}})
			var ack map[string]any
			_ = ws.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			if err := ws.ReadJSON(&ack); err == nil {
				t.Errorf("acknowledged before durable request: %v", ack)
			}
			return
		}
		_ = ws.WriteJSON(map[string]any{"type": "hello"})
		_ = ws.WriteJSON(map[string]any{"type": "events_api", "envelope_id": "two", "payload": map[string]any{"type": "event_callback", "team_id": "T0001", "event": map[string]any{"type": "channel_archive", "channel": public}}})
		var ack map[string]any
		_ = ws.SetReadDeadline(time.Now().Add(time.Second))
		if err := ws.ReadJSON(&ack); err != nil {
			t.Errorf("no ack after request: %v", err)
		}
		_ = ws.WriteJSON(map[string]any{"type": "events_api", "envelope_id": "three", "payload": map[string]any{"type": "event_callback", "team_id": "T0001", "event": map[string]any{"type": "message", "channel": public, "channel_type": "channel", "subtype": "message_deleted", "deleted_ts": "1758700000.000100"}}})
		if err := ws.ReadJSON(&ack); err != nil {
			t.Errorf("no ack for restricted tombstone: %v", err)
		}
	})
	c, err := slack.New(fake.resolved())
	if err != nil {
		t.Fatal(err)
	}
	sink := &owedSink{fail: true}
	if err := c.Stream(t.Context(), sink); err == nil {
		t.Fatal("stream should fail when durable request fails")
	}
	if err := c.Stream(t.Context(), sink); err == nil {
		t.Fatal("fixture should close stream")
	}
	if sink.requests != 2 {
		t.Errorf("requests = %d, want retry", sink.requests)
	}
	if events := sink.Events(); len(events) != 1 || events[0].ACL[0].Kind != connector.ACLGroup || events[0].Payload.Revision.Token != "perm:private" {
		t.Errorf("restricted tombstone = %+v", events)
	}
	publicNow, err := c.Public(t.Context(), public)
	if err != nil || publicNow {
		t.Errorf("archived public = %v, %v", publicNow, err)
	}
}

type failOnceSink struct {
	*connector.Recorder
	fail bool
}

func (s *failOnceSink) Emit(ctx context.Context, ev connector.Event) error {
	if s.fail {
		s.fail = false
		return errors.New("L0 unavailable")
	}
	return s.Recorder.Emit(ctx, ev)
}
func (s *failOnceSink) CurrentArtifacts(ctx context.Context, source string) ([]connector.Event, error) {
	return s.Recorder.CurrentArtifacts(ctx, source)
}

func TestPrivateResyncRetry(t *testing.T) {
	fake := newFakeSlack(t, func(int, *websocket.Conn) {})
	fake.channels[public] = `{"id":"C0PUBLIC","is_channel":true,"is_private":true,"is_member":true}`
	c, err := slack.New(fake.resolved())
	if err != nil {
		t.Fatal(err)
	}
	isPublic, err := c.Public(t.Context(), public)
	if err != nil || isPublic {
		t.Fatalf("private public = %v, %v", isPublic, err)
	}
	rec := &connector.Recorder{}
	ev := connector.Event{Source: "slack-acme", NativeID: public + "/1758700000.000100@v1+perm:public", Kind: connector.KindMessage, Time: time.Unix(1758700000, 100000).UTC(), ACL: connector.ACL{{Kind: connector.ACLPublic}}, Payload: connector.Payload{Artifact: public + "/1758700000.000100", Container: connector.Container{Kind: connector.ContainerChannel, NativeID: public}, Text: "root", Author: &connector.Identity{Source: "slack-acme", Kind: connector.IdentityUser, NativeID: "U0KYLE"}, Revision: &connector.Revision{Token: "v1+perm:public"}}}
	if err := rec.Emit(t.Context(), ev); err != nil {
		t.Fatal(err)
	}
	sink := &failOnceSink{Recorder: rec, fail: true}
	if _, err := c.Resync(t.Context(), sink, public, ""); err == nil {
		t.Fatal("want failed re-sync")
	}
	if len(rec.Events()) != 1 {
		t.Fatal("failed attempt wrote event")
	}
	result, err := c.Resync(t.Context(), sink, public, "")
	if err != nil || !result.Done || len(rec.Events()) != 2 {
		t.Fatalf("retry = %+v, %v, events %d", result, err, len(rec.Events()))
	}
}
