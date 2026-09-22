package discord_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/discord"
)

const (
	testGuild   = "1551744840499200000"
	testChannel = "1551744840499200001"
	testThread  = "1551744840499200004"
)

type restFixture struct {
	mu             sync.Mutex
	private        bool
	failed         bool
	pages          []string
	thread         bool
	gatewayMessage bool
	archived       string
	gatewayUpdate  bool
	gatewaySilent  bool
	holdPage       <-chan struct{}
	pageEntered    chan struct{}
	messages       []map[string]any
}

func (f *restFixture) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/gateway" {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		_ = ws.WriteJSON(map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 5000}})
		var identify any
		if ws.ReadJSON(&identify) != nil {
			return
		}
		f.mu.Lock()
		silent := f.gatewaySilent
		f.mu.Unlock()
		if !silent {
			_ = ws.WriteJSON(map[string]any{"op": 0, "t": "READY", "s": 1, "d": map[string]any{"session_id": "test"}})
		}
		f.mu.Lock()
		sendMessage := f.gatewayMessage
		sendUpdate := f.gatewayUpdate
		var m map[string]any
		if sendMessage {
			m = f.messages[0]
		}
		if sendUpdate {
			_ = ws.WriteJSON(map[string]any{"op": 0, "t": "GUILD_CREATE", "s": 2, "d": map[string]any{"id": testGuild, "roles": []any{map[string]any{"id": testGuild, "permissions": "1024"}}, "channels": []any{map[string]any{"id": testChannel, "guild_id": testGuild, "name": "general", "type": 0}}}})
			_ = ws.WriteJSON(map[string]any{"op": 0, "t": "CHANNEL_UPDATE", "s": 3, "d": map[string]any{"id": testChannel, "guild_id": testGuild, "name": "general", "type": 0, "permission_overwrites": []any{map[string]any{"id": testGuild, "type": 0, "allow": "0", "deny": "1024"}}}})
		}
		f.mu.Unlock()
		if sendMessage {
			_ = ws.WriteJSON(map[string]any{"op": 0, "t": "GUILD_CREATE", "s": 2, "d": map[string]any{"id": testGuild, "roles": []any{map[string]any{"id": testGuild, "permissions": "1024"}}, "channels": []any{map[string]any{"id": testChannel, "guild_id": testGuild, "name": "general", "type": 0, "permission_overwrites": []any{}}}}})
			_ = ws.WriteJSON(map[string]any{"op": 0, "t": "MESSAGE_CREATE", "s": 3, "d": m})
		}
		for {
			var x any
			if ws.ReadJSON(&x) != nil {
				return
			}
		}
	}
	if r.URL.Path == "/channels/"+testChannel+"/messages" && r.URL.Query().Get("before") != "" {
		f.mu.Lock()
		hold, entered := f.holdPage, f.pageEntered
		f.mu.Unlock()
		if hold != nil {
			select {
			case entered <- struct{}{}:
			default:
			}
			select {
			case <-hold:
			case <-r.Context().Done():
				return
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	var value any
	switch path {
	case "/guilds/" + testGuild + "/roles":
		value = []any{map[string]any{"id": testGuild, "permissions": "1024"}}
	case "/channels/" + testChannel:
		ow := []any{}
		if f.private {
			ow = []any{map[string]any{"id": testGuild, "type": 0, "allow": "0", "deny": "1024"}}
		}
		value = map[string]any{"id": testChannel, "guild_id": testGuild, "name": "general", "type": 0, "permission_overwrites": ow}
	case "/channels/" + testThread:
		kind := 12
		if f.archived == "public" {
			kind = 11
		}
		value = map[string]any{"id": testThread, "guild_id": testGuild, "parent_id": testChannel, "name": "thread", "type": kind, "owner_id": "1551744840499200007"}
	case "/guilds/" + testGuild + "/threads/active":
		threads := []any{}
		if f.thread {
			threads = []any{map[string]any{"id": testThread, "parent_id": testChannel, "type": 12}}
		}
		value = map[string]any{"threads": threads}
	case "/channels/" + testChannel + "/threads/archived/public", "/channels/" + testChannel + "/threads/archived/private":
		threads := []any{}
		if strings.HasSuffix(path, "/"+f.archived) {
			threads = []any{map[string]any{"id": testThread, "thread_metadata": map[string]any{"archive_timestamp": "2026-09-22T01:00:00Z"}}}
		}
		value = map[string]any{"threads": threads, "has_more": false}
	case "/channels/" + testThread + "/messages":
		value = []any{f.message(testThread, "1551744840499200012")}
	case "/channels/" + testChannel + "/messages":
		before := r.URL.Query().Get("before")
		f.pages = append(f.pages, before)
		if f.failed && before != "" {
			f.failed = false
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"retry_after":0.001}`))
			return
		}
		var msgs []map[string]any
		for _, m := range f.messages {
			if before == "" || m["id"].(string) < before {
				msgs = append(msgs, m)
			}
			if len(msgs) == 100 {
				break
			}
		}
		value = msgs
	default:
		http.Error(w, "unexpected "+path, http.StatusNotFound)
		return
	}
	_ = json.NewEncoder(w).Encode(value)
}

func (*restFixture) message(channel, id string) map[string]any {
	return map[string]any{"id": id, "guild_id": testGuild, "channel_id": channel, "content": "decision", "timestamp": "2026-09-22T00:00:00Z", "author": map[string]any{"id": "1551744840499200007", "username": "alice"}}
}

func discordSource(api string) connector.SourceConfig {
	return connector.SourceConfig{ID: "chat", Type: discord.Type, Containers: []string{testChannel}, Settings: json.RawMessage(fmt.Sprintf(`{"guild":%q,"api_url":%q,"gateway_url":%q}`, testGuild, api, "ws"+strings.TrimPrefix(api, "http")+"/gateway")), Secrets: map[string]string{"token": "bot-token"}}
}

func walkDiscord(t *testing.T, c *discord.Connector, sink connector.Sink, from connector.Cursor) {
	t.Helper()
	for i := 0; i < 30; i++ {
		res, err := c.Backfill(t.Context(), sink, from)
		if err != nil {
			t.Fatal(err)
		}
		if res.Done {
			return
		}
		if res.Next == from {
			t.Fatalf("no cursor progress at %s", from)
		}
		from = res.Next
	}
	t.Fatal("backfill did not finish")
}

func TestRESTBackfillResumesAfterFailedPage(t *testing.T) {
	f := &restFixture{failed: true}
	for i := 0; i < 101; i++ {
		f.messages = append(f.messages, f.message(testChannel, fmt.Sprint(1551744840499200200-i)))
	}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	defer server.Close()
	src := discordSource(server.URL)
	c, err := discord.New(src)
	if err != nil {
		t.Fatal(err)
	}
	sink := connector.NewGate(&connector.Recorder{}, src.ID, c.Describe(), connector.NewAllowlist(src))
	first, err := c.Backfill(t.Context(), sink, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Done || first.Events != 100 {
		t.Fatalf("first page = %+v", first)
	}
	// A new connector has no gateway cache. The stored cursor still names the
	// next page; a 429 does not advance it.
	c, err = discord.New(src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Backfill(t.Context(), sink, first.Next); err == nil {
		t.Fatal("429 succeeded")
	}
	res, err := c.Backfill(t.Context(), sink, first.Next)
	if err != nil || res.Events != 1 {
		t.Fatalf("retry = %+v, %v", res, err)
	}
	f.mu.Lock()
	pages := append([]string(nil), f.pages...)
	f.mu.Unlock()
	if len(pages) != 3 || pages[0] != "" || pages[1] == "" || pages[1] != pages[2] {
		t.Errorf("page positions = %v", pages)
	}
	walkDiscord(t, c, sink, res.Next)
}

func TestPrivateThreadBackfillKeepsParentContainer(t *testing.T) {
	f := &restFixture{thread: true, messages: []map[string]any{}}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	defer server.Close()
	src := discordSource(server.URL)
	c, err := discord.New(src)
	if err != nil {
		t.Fatal(err)
	}
	rec := &connector.Recorder{}
	walkDiscord(t, c, connector.NewGate(rec, src.ID, c.Describe(), connector.NewAllowlist(src)), "")
	var found bool
	for _, ev := range rec.Events() {
		if ev.Payload.Artifact == "1551744840499200012" {
			found = true
			if ev.Payload.Container.NativeID != testChannel || ev.Payload.Thread != "thread:"+testThread || len(ev.ACL) != 1 || ev.ACL[0].Kind != connector.ACLGroup || ev.ACL[0].NativeID != testThread {
				t.Errorf("private thread event = %+v", ev)
			}
		}
	}
	if !found {
		t.Fatal("thread message missing")
	}
}

func TestArchivedThreadBackfill(t *testing.T) {
	for _, tt := range []struct {
		name string
		acl  connector.ACLKind
	}{
		{"public", connector.ACLPublic},
		{"private", connector.ACLGroup},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &restFixture{archived: tt.name}
			server := httptest.NewServer(http.HandlerFunc(f.serve))
			defer server.Close()
			src := discordSource(server.URL)
			c, err := discord.New(src)
			if err != nil {
				t.Fatal(err)
			}
			rec := &connector.Recorder{}
			walkDiscord(t, c, connector.NewGate(rec, src.ID, c.Describe(), connector.NewAllowlist(src)), "")
			var found bool
			for _, ev := range rec.Events() {
				if ev.Payload.Artifact == "1551744840499200012" {
					found = true
					if ev.Payload.Container.NativeID != testChannel || len(ev.ACL) != 1 || ev.ACL[0].Kind != tt.acl {
						t.Errorf("archived message = %+v", ev)
					}
				}
			}
			if !found {
				t.Fatal("archived thread message missing")
			}
		})
	}
}

func TestPermissionResyncUsesDurableRuntimeStore(t *testing.T) {
	f := &restFixture{gatewaySilent: true, messages: []map[string]any{}}
	f.messages = []map[string]any{f.message(testChannel, "1551744840499200011")}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	defer server.Close()
	src := discordSource(server.URL)
	c, err := discord.New(src)
	if err != nil {
		t.Fatal(err)
	}
	rec := &connector.Recorder{}
	gate := connector.NewGate(rec, src.ID, c.Describe(), connector.NewAllowlist(src))
	walkDiscord(t, c, gate, "")
	original := rec.Events()[0]
	if len(original.ACL) != 1 || original.ACL[0].Kind != connector.ACLPublic {
		t.Fatalf("initial ACL = %+v", original.ACL)
	}
	f.mu.Lock()
	f.private = true
	f.mu.Unlock()
	store := connector.NewMemoryResyncs()
	registry := connector.NewRegistry()
	if err := registry.Register(discord.Type, discord.Factory); err != nil {
		t.Fatal(err)
	}
	runtime, err := connector.NewRuntime(t.Context(), connector.RuntimeOptions{Sources: []connector.SourceConfig{src}, Registry: registry, Sink: rec, Resyncs: store, Exposure: rec, Lookup: func(string) (string, bool) { return "bot-token", true }, Cadence: connector.Cadence{MinRefresh: time.Millisecond, Refresh: time.Millisecond, MaxBackoff: 5 * time.Millisecond, Shutdown: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("runtime did not stop")
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		record := store.Get(src.ID, testChannel)
		var changed connector.Event
		for _, ev := range rec.Events() {
			if ev.Payload.Artifact == original.Payload.Artifact && ev.NativeID != original.NativeID {
				changed = ev
			}
		}
		if record.Generation > 0 && !record.Owed && changed.NativeID != "" {
			if !strings.Contains(changed.NativeID, "@perm:") || len(changed.ACL) != 1 || changed.ACL[0].Kind != connector.ACLGroup || !changed.Time.Equal(original.Time) || changed.Payload.Text != original.Payload.Text {
				t.Errorf("re-emitted event = %+v", changed)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("startup check never settled re-sync: %+v", record)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestGatewayChannelUpdateRequestsResync(t *testing.T) {
	f := &restFixture{private: true, gatewayUpdate: true, messages: []map[string]any{}}
	f.messages = []map[string]any{f.message(testChannel, "1551744840499200011")}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	defer server.Close()
	src := discordSource(server.URL)
	registry := connector.NewRegistry()
	if err := registry.Register(discord.Type, discord.Factory); err != nil {
		t.Fatal(err)
	}
	store := connector.NewMemoryResyncs()
	runtime, err := connector.NewRuntime(t.Context(), connector.RuntimeOptions{Sources: []connector.SourceConfig{src}, Registry: registry, Sink: &connector.Recorder{}, Resyncs: store, Lookup: func(string) (string, bool) { return "bot-token", true }, Cadence: connector.Cadence{MinRefresh: time.Millisecond, Refresh: time.Millisecond, MaxBackoff: 5 * time.Millisecond, Shutdown: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("runtime did not stop")
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for store.Get(src.ID, testChannel).Generation < 2 {
		if time.Now().After(deadline) {
			t.Fatal("gateway update did not record re-sync")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestFreshGatewaySessionWalksOutageGap(t *testing.T) {
	f := &restFixture{}
	f.messages = []map[string]any{f.message(testChannel, "1551744840499200011")}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	defer server.Close()
	src := discordSource(server.URL)
	registry := connector.NewRegistry()
	if err := registry.Register(discord.Type, discord.Factory); err != nil {
		t.Fatal(err)
	}
	cursors := connector.NewMemoryCursors()
	cursors.Set(src.ID, connector.BackfillState{Done: true})
	store := connector.NewMemoryResyncs()
	rec := &connector.Recorder{}
	runtime, err := connector.NewRuntime(t.Context(), connector.RuntimeOptions{Sources: []connector.SourceConfig{src}, Registry: registry, Sink: rec, Cursors: cursors, Resyncs: store, Lookup: func(string) (string, bool) { return "bot-token", true }, Cadence: connector.Cadence{MinRefresh: time.Millisecond, Refresh: time.Millisecond, MaxBackoff: 5 * time.Millisecond, Shutdown: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("runtime did not stop")
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		record := store.Get(src.ID, testChannel)
		if record.Generation > 0 && !record.Owed && len(rec.Events()) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("restart gap not ingested: %+v", record)
		}
		time.Sleep(time.Millisecond)
	}
	if got := cursors.Saves(src.ID); len(got) != 0 {
		t.Errorf("finished initial backfill was restarted: %+v", got)
	}
}

func TestBackfillAndGatewayUseSameMessageIdentity(t *testing.T) {
	f := &restFixture{gatewayMessage: true}
	f.messages = []map[string]any{f.message(testChannel, "1551744840499200011")}
	f.messages[0]["content"] = "ask <@1551744840499200099>"
	f.messages[0]["mentions"] = []map[string]any{{"id": "1551744840499200099", "username": "shed", "bot": true}}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	defer server.Close()
	src := discordSource(server.URL)
	c, err := discord.New(src)
	if err != nil {
		t.Fatal(err)
	}
	rec := &connector.Recorder{}
	gate := connector.NewGate(rec, src.ID, c.Describe(), connector.NewAllowlist(src))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- c.Stream(ctx, gate) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("gateway did not stop")
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for len(rec.Events()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("gateway message never arrived")
		}
		time.Sleep(time.Millisecond)
	}
	first := rec.Events()[0]
	walkDiscord(t, c, gate, "")
	events := rec.Events()
	if len(events) != 2 || !reflect.DeepEqual(first, events[1]) {
		t.Errorf("gateway and REST events differ: %+v / %+v", first, events)
	}
}
