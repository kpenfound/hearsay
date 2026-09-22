package discord_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/discord"
)

const (
	testGuild   = "1551744840499200000"
	testChannel = "1551744840499200001"
	testThread  = "1551744840499200004"
)

type restFixture struct {
	mu       sync.Mutex
	private  bool
	failed   bool
	pages    []string
	thread   bool
	messages []map[string]any
}

func (f *restFixture) serve(w http.ResponseWriter, r *http.Request) {
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
		value = map[string]any{"id": testThread, "guild_id": testGuild, "parent_id": testChannel, "name": "secret thread", "type": 12, "owner_id": "1551744840499200007"}
	case "/guilds/" + testGuild + "/threads/active":
		threads := []any{}
		if f.thread {
			threads = []any{map[string]any{"id": testThread, "parent_id": testChannel, "type": 12}}
		}
		value = map[string]any{"threads": threads}
	case "/channels/" + testChannel + "/threads/archived/public", "/channels/" + testChannel + "/threads/archived/private":
		value = map[string]any{"threads": []any{}, "has_more": false}
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
	return connector.SourceConfig{ID: "chat", Type: discord.Type, Containers: []string{testChannel}, Settings: json.RawMessage(fmt.Sprintf(`{"guild":%q,"api_url":%q}`, testGuild, api)), Secrets: map[string]string{"token": "bot-token"}}
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

func TestPermissionResyncUsesDurableRuntimeStore(t *testing.T) {
	f := &restFixture{messages: []map[string]any{}}
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
	public, err := c.Public(t.Context(), testChannel)
	if err != nil || public {
		t.Fatalf("startup Public = %v, %v", public, err)
	}
	store := connector.NewMemoryResyncs()
	if err := store.Owe(t.Context(), src.ID, testChannel); err != nil {
		t.Fatal(err)
	}
	var cursor connector.Cursor
	for i := 0; i < 30; i++ {
		res, err := c.Resync(t.Context(), gate, testChannel, cursor)
		if err != nil {
			t.Fatal(err)
		}
		record := store.Get(src.ID, testChannel)
		if res.Done {
			if _, err := store.Finish(t.Context(), src.ID, record); err != nil {
				t.Fatal(err)
			}
			break
		}
		cursor = res.Next
		record.Cursor = cursor
		if err := store.Save(t.Context(), src.ID, record); err != nil {
			t.Fatal(err)
		}
		if i == 29 {
			t.Fatal("resync did not finish")
		}
	}
	if store.Get(src.ID, testChannel).Owed {
		t.Fatal("resync still owed")
	}
	var changed connector.Event
	for _, ev := range rec.Events() {
		if ev.Payload.Artifact == original.Payload.Artifact && ev.NativeID != original.NativeID {
			changed = ev
		}
	}
	if changed.NativeID == "" || !strings.Contains(changed.NativeID, "@perm:") || len(changed.ACL) != 1 || changed.ACL[0].Kind != connector.ACLGroup || !changed.Time.Equal(original.Time) || changed.Payload.Text != original.Payload.Text {
		t.Errorf("re-emitted event = %+v", changed)
	}
}

func TestBackfillAndGatewayUseSameMessageIdentity(t *testing.T) {
	f := &restFixture{messages: []map[string]any{}}
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
	first := rec.Events()[0]
	// A second REST observation has the exact id and payload the gateway's
	// MESSAGE_CREATE builder uses, so the L0 identity collapses on the gate.
	walkDiscord(t, c, gate, "")
	second := rec.Events()[1]
	if first.ID != second.ID || first.NativeID != second.NativeID || first.Payload.Text != second.Payload.Text || !first.Time.Equal(second.Time) {
		t.Errorf("overlap = %+v / %+v", first, second)
	}
}
