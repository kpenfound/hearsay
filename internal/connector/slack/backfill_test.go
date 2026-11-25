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

func publicRoot() connector.Event {
	return connector.Event{Source: "slack-acme", NativeID: public + "/1758700000.000100@v1+perm:public", Kind: connector.KindMessage, Time: time.Unix(1758700000, 100000).UTC(), ACL: connector.ACL{{Kind: connector.ACLPublic}}, Payload: connector.Payload{Artifact: public + "/1758700000.000100", Container: connector.Container{Kind: connector.ContainerChannel, NativeID: public}, Text: "root", Author: &connector.Identity{Source: "slack-acme", Kind: connector.IdentityUser, NativeID: "U0KYLE"}, Revision: &connector.Revision{Token: "v1+perm:public"}}}
}

type backfillRun struct {
	sink  *deduplicating
	calls []string
}

func runBackfillWithRestarts(t *testing.T) backfillRun {
	t.Helper()
	fake := newFakeSlack(t, func(int, *websocket.Conn) {})
	run := backfillRun{sink: &deduplicating{seen: map[string]bool{}}}
	fake.history = func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/")
		cursor := r.FormValue("cursor")
		run.calls = append(run.calls, path+":"+cursor)
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
	gate := connector.NewGate(run.sink, fake.resolved().ID, (&slack.Connector{}).Describe(), connector.NewAllowlist(fake.resolved()))
	var from connector.Cursor
	for pass := 0; ; pass++ {
		if pass == 8 {
			t.Fatal("backfill did not finish")
		}
		c, err := slack.New(fake.resolved())
		if err != nil {
			t.Fatal(err)
		}
		result, err := c.Backfill(t.Context(), gate, from)
		if err != nil {
			t.Fatal(err)
		}
		if result.Done {
			return run
		}
		if result.Next == from {
			t.Fatal("cursor did not advance")
		}
		from = result.Next
	}
}

func TestBackfillStoresRootAndReplyAcrossRestarts(t *testing.T) {
	if got := len(runBackfillWithRestarts(t).sink.recorder.Events()); got != 2 {
		t.Errorf("stored events = %d, want root and reply", got)
	}
}

func TestBackfillReplaysRootRevisionWithTheSameID(t *testing.T) {
	deliveries := runBackfillWithRestarts(t).sink.delivered()
	if len(deliveries) != 3 || deliveries[0] != deliveries[1] {
		t.Errorf("deliveries = %v, want identical root revision replay", deliveries)
	}
}

func TestBackfillReplyBelongsToRootThread(t *testing.T) {
	events := runBackfillWithRestarts(t).sink.recorder.Events()
	if events[1].Payload.Thread != public+"/1758700000.000100" {
		t.Errorf("reply thread = %q", events[1].Payload.Thread)
	}
}

func TestBackfillReadsEachHistoryAndReplyPage(t *testing.T) {
	if calls := runBackfillWithRestarts(t).calls; len(calls) != 4 {
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

func resyncedPublicRoot(t *testing.T) (*slack.Connector, *connector.Recorder) {
	t.Helper()
	fake := newFakeSlack(t, func(int, *websocket.Conn) {})
	c, err := slack.New(fake.resolved())
	if err != nil {
		t.Fatal(err)
	}
	rec := &connector.Recorder{}
	if err := rec.Emit(t.Context(), publicRoot()); err != nil {
		t.Fatal(err)
	}
	first, err := c.Resync(t.Context(), rec, public, "")
	if err != nil || !first.Done {
		t.Fatalf("resync = %+v, %v", first, err)
	}
	return c, rec
}

func TestResyncRevisesPriorEventsUnderCurrentPermissions(t *testing.T) {
	_, rec := resyncedPublicRoot(t)
	events := rec.Events()
	if len(events) != 2 || events[1].ACL[0].Kind != connector.ACLGroup || events[1].Payload.Revision.Token != "v1+perm:private" {
		t.Fatalf("events = %+v", events)
	}
}

func TestResyncReplayWritesNothingNew(t *testing.T) {
	c, rec := resyncedPublicRoot(t)
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

type archiveRun struct {
	c            *slack.Connector
	sink         *owedSink
	firstAcked   bool
	firstStreamE error
}

func runArchiveTransition(t *testing.T) archiveRun {
	t.Helper()
	archived := `{"id":"C0PUBLIC","is_channel":true,"is_archived":true,"is_member":true}`
	firstAcked := make(chan bool, 1)
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
			firstAcked <- ws.ReadJSON(&ack) == nil
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
	run := archiveRun{c: c, sink: sink}
	run.firstStreamE = c.Stream(t.Context(), sink)
	run.firstAcked = <-firstAcked
	if err := c.Stream(t.Context(), sink); err == nil {
		t.Fatal("fixture should close stream")
	}
	return run
}

func TestArchiveTransitionIsNotAckedBeforeResyncIsRequested(t *testing.T) {
	run := runArchiveTransition(t)
	if run.firstStreamE == nil {
		t.Error("stream should fail when the durable request fails")
	}
	if run.firstAcked {
		t.Error("acknowledged before the durable request")
	}
}

func TestArchiveTransitionRetriesFailedResyncRequest(t *testing.T) {
	if run := runArchiveTransition(t); run.sink.requests != 2 {
		t.Errorf("requests = %d, want retry", run.sink.requests)
	}
}

func TestArchivedChannelTombstoneIsRestricted(t *testing.T) {
	events := runArchiveTransition(t).sink.Events()
	if len(events) != 1 || events[0].ACL[0].Kind != connector.ACLGroup || events[0].Payload.Revision.Token != "perm:private" {
		t.Errorf("restricted tombstone = %+v", events)
	}
}

func TestArchivedChannelIsNotPublic(t *testing.T) {
	run := runArchiveTransition(t)
	publicNow, err := run.c.Public(t.Context(), public)
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
	ev := publicRoot()
	if err := rec.Emit(t.Context(), ev); err != nil {
		t.Fatal(err)
	}
	sink := &failOnceSink{Recorder: rec, fail: true}
	if _, err := c.Resync(t.Context(), sink, public, ""); err == nil {
		t.Fatal("want failed re-sync")
	}
	result, err := c.Resync(t.Context(), sink, public, "")
	if err != nil || !result.Done || len(rec.Events()) != 2 {
		t.Fatalf("retry = %+v, %v, events %d", result, err, len(rec.Events()))
	}
}
