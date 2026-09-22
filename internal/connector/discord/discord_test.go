package discord_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/discord"
)

type uniqueSink struct {
	mu       sync.Mutex
	recorder connector.Recorder
	seen     map[string]bool
}

func (s *uniqueSink) Emit(ctx context.Context, ev connector.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen[ev.ID] {
		return nil
	}
	if err := s.recorder.Emit(ctx, ev); err != nil {
		return err
	}
	s.seen[ev.ID] = true
	return nil
}
func (s *uniqueSink) events() []connector.Event { return s.recorder.Events() }

func fixture(t *testing.T, name string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var events []map[string]any
	if err := json.Unmarshal(raw, &events); err != nil {
		t.Fatal(err)
	}
	return events
}

func TestGatewayReplayThroughRuntimeGate(t *testing.T) {
	const guild = "1551744840499200000"
	const public = "1551744840499200001"
	const private = "1551744840499200002"
	const message = "1551744840499200005"
	tests := []struct {
		name       string
		containers []string
		dropped    int64
		private    bool
	}{
		{name: "public and private channels", containers: []string{public, private}, dropped: 1, private: true},
		{name: "public channel only", containers: []string{public}, dropped: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			connections := 0
			handshakes := []int{}
			serverDone := make(chan struct{}, 1)
			var gatewayURL string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer ws.Close()
				mu.Lock()
				connections++
				n := connections
				mu.Unlock()
				if err := ws.WriteJSON(map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 5000}}); err != nil {
					return
				}
				var hello struct {
					Op int            `json:"op"`
					D  map[string]any `json:"d"`
				}
				if err := ws.ReadJSON(&hello); err != nil {
					return
				}
				mu.Lock()
				handshakes = append(handshakes, hello.Op)
				mu.Unlock()
				if n == 2 {
					if hello.Op != 6 || hello.D["session_id"] != "sess" || hello.D["seq"] != float64(9) {
						t.Errorf("RESUME = %+v", hello)
					}
				}
				name := "first"
				if n > 1 {
					name = "replay"
				}
				for _, e := range fixture(t, name) {
					if e["t"] == "READY" {
						e["d"].(map[string]any)["resume_gateway_url"] = gatewayURL
					}
					if err := ws.WriteJSON(e); err != nil {
						return
					}
					if n == 1 && e["t"] == "MESSAGE_CREATE" && e["d"].(map[string]any)["id"] == message {
						if err := ws.WriteJSON(e); err != nil {
							return
						}
					}
				}
				if n == 1 {
					return
				}
				serverDone <- struct{}{}
				for {
					var x any
					if err := ws.ReadJSON(&x); err != nil {
						return
					}
				}
			}))
			defer server.Close()
			gatewayURL = "ws" + strings.TrimPrefix(server.URL, "http")
			src := connector.SourceConfig{ID: "chat", Type: discord.Type, Containers: tt.containers, Settings: json.RawMessage(fmt.Sprintf(`{"guild":%q,"gateway_url":%q}`, guild, gatewayURL)), Secrets: map[string]string{"token": "BOT_TOKEN_ENV"}}
			reg := connector.NewRegistry()
			if err := reg.Register(discord.Type, discord.Factory); err != nil {
				t.Fatal(err)
			}
			sink := &uniqueSink{seen: map[string]bool{}}
			rt, err := connector.NewRuntime(t.Context(), connector.RuntimeOptions{Sources: []connector.SourceConfig{src}, Registry: reg, Sink: sink, Lookup: func(name string) (string, bool) { return "test-bot-token", true }, Cadence: connector.Cadence{MinRefresh: time.Millisecond, Refresh: time.Millisecond, MaxBackoff: 5 * time.Millisecond, Shutdown: time.Second}})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- rt.Run(ctx) }()
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
			select {
			case <-serverDone:
			case <-time.After(5 * time.Second):
				t.Fatal("gateway replay did not finish")
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				h := rt.Health(t.Context()).Sources[0]
				foundDelete := false
				for _, ev := range sink.events() {
					if ev.NativeID == message+":tombstone" {
						foundDelete = true
					}
				}
				if h.Status == connector.HealthOK && h.Dropped == tt.dropped && foundDelete {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("health = %+v", h)
				}
				time.Sleep(time.Millisecond)
			}
			mu.Lock()
			gotHandshakes := append([]int(nil), handshakes...)
			mu.Unlock()
			if len(gotHandshakes) < 2 || gotHandshakes[0] != 2 || gotHandshakes[1] != 6 {
				t.Errorf("handshakes = %v, want IDENTIFY then RESUME", gotHandshakes)
			}
			events := sink.events()
			byID := map[string]connector.Event{}
			for _, ev := range events {
				if _, ok := byID[ev.NativeID]; ok {
					t.Errorf("duplicate native id %q", ev.NativeID)
				}
				byID[ev.NativeID] = ev
			}
			original, ok := byID[message]
			if !ok {
				t.Fatal("original message missing")
			}
			if original.Payload.Thread != "1551744840499200004" || original.Payload.Container.NativeID != public {
				t.Errorf("thread containment = %+v", original.Payload)
			}
			edited, ok := byID[message+"@2026-09-22T01:00:00Z"]
			if !ok {
				t.Fatal("edited revision missing")
			}
			if edited.Payload.Revision == nil || edited.Payload.Revision.Token != "2026-09-22T01:00:00Z" || !edited.Time.Equal(original.Time) {
				t.Errorf("edited revision = %+v", edited)
			}
			if tomb, ok := byID[message+":tombstone"]; !ok || tomb.Payload.Target != message {
				t.Errorf("message tombstone = %+v", tomb)
			}
			if _, ok := byID["1551744840499200006"]; ok {
				t.Error("non-allowlisted message was emitted")
			}
			priv, ok := byID["1551744840499200008"]
			if ok != tt.private {
				t.Errorf("private event present = %v", ok)
			}
			if ok && (len(priv.ACL) != 1 || priv.ACL[0].Kind != connector.ACLGroup || priv.ACL[0].NativeID != private) {
				t.Errorf("private ACL = %+v", priv.ACL)
			}
			reaction := "1551744840499200005:reaction:1551744840499200007:%F0%9F%91%8D"
			if _, ok := byID[reaction]; !ok {
				t.Error("reaction missing")
			}
			if ev, ok := byID[reaction+":tombstone"]; !ok || ev.Payload.Target != reaction {
				t.Errorf("reaction tombstone = %+v", ev)
			}
		})
	}
}
