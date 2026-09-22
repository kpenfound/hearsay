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
	mu         sync.Mutex
	recorder   connector.Recorder
	seen       map[string]bool
	deliveries []connector.Event
}

func (s *uniqueSink) Emit(ctx context.Context, ev connector.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deliveries = append(s.deliveries, ev)
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
func (s *uniqueSink) allDeliveries() []connector.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]connector.Event(nil), s.deliveries...)
}

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
		{name: "public channel only", containers: []string{public}, dropped: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			connections := 0
			handshakes := []int{}
			serverDone := make(chan struct{}, 1)
			resumeStarted := make(chan struct{}, 1)
			releaseResume := make(chan struct{})
			release := sync.OnceFunc(func() { close(releaseResume) })
			defer release()
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
					if hello.Op != 6 || hello.D["session_id"] != "sess" || hello.D["seq"] != float64(11) {
						t.Errorf("RESUME = %+v", hello)
					}
					resumeStarted <- struct{}{}
					<-releaseResume
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
			case <-resumeStarted:
			case <-time.After(5 * time.Second):
				t.Fatal("gateway did not reconnect")
			}
			if h := rt.Health(t.Context()).Sources[0]; h.Status != connector.HealthDegraded || h.Detail != "reconnecting" {
				t.Errorf("reconnecting health = %+v", h)
			}
			release()
			select {
			case <-serverDone:
			case <-time.After(5 * time.Second):
				t.Fatal("gateway replay did not finish")
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				h := rt.Health(t.Context()).Sources[0]
				foundDelete := false
				foundBulk := false
				foundChangedVisibility := false
				for _, ev := range sink.events() {
					if ev.NativeID == "thread:1551744840499200004:tombstone" {
						foundDelete = true
					}
					if ev.NativeID == "1551744840499200008:tombstone" {
						foundBulk = true
					}
					if ev.NativeID == "1551744840499200010" {
						foundChangedVisibility = true
					}
				}
				if h.Status == connector.HealthOK && h.Dropped == tt.dropped && foundDelete && foundBulk == tt.private && foundChangedVisibility {
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
			counts := map[string]int{}
			for _, ev := range sink.allDeliveries() {
				counts[ev.NativeID]++
			}
			if counts[message] != 2 || counts[message+":tombstone"] != 2 {
				t.Errorf("replayed delivery counts = %v", counts)
			}
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
			if original.Payload.Thread != "thread:1551744840499200004" || original.Payload.Parent != "thread:1551744840499200004" || original.Payload.Container.NativeID != public {
				t.Errorf("thread containment = %+v", original.Payload)
			}
			if reply, ok := byID["1551744840499200009"]; !ok || reply.Payload.Parent != message || reply.Payload.Thread != "thread:1551744840499200004" {
				t.Errorf("reply relationship = %+v", reply)
			}
			edited, ok := byID[message+"@2026-09-22T01:00:00.000000+00:00"]
			if !ok {
				t.Fatal("edited revision missing")
			}
			if edited.Payload.Revision == nil || edited.Payload.Revision.Token != "2026-09-22T01:00:00.000000+00:00" || !edited.Time.Equal(original.Time) {
				t.Errorf("edited revision = %+v", edited)
			}
			if tomb, ok := byID[message+":tombstone"]; !ok || tomb.Payload.Target != message {
				t.Errorf("message tombstone = %+v", tomb)
			}
			if tomb, ok := byID["thread:1551744840499200004:tombstone"]; !ok || tomb.Payload.Target != "thread:1551744840499200004" {
				t.Errorf("thread tombstone = %+v", tomb)
			}
			if _, ok := byID["thread:1551744840499200004"]; !ok {
				t.Error("thread artifact missing")
			} else if got := byID["thread:1551744840499200004"].Payload.URL; got != "https://discord.com/channels/1551744840499200000/1551744840499200004" {
				t.Errorf("thread URL = %q", got)
			}
			if _, ok := byID["1551744840499200004"]; !ok {
				t.Error("starter message with same Discord id missing")
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
			if changed, ok := byID["1551744840499200010"]; !ok || len(changed.ACL) != 1 || changed.ACL[0].Kind != connector.ACLGroup || changed.ACL[0].NativeID != public {
				t.Errorf("ACL after guild role change = %+v", changed.ACL)
			}
			if tomb, present := byID["1551744840499200008:tombstone"]; present != tt.private || (present && tomb.Payload.Target != "1551744840499200008") {
				t.Errorf("bulk tombstone = %+v, present %v", tomb, present)
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

func TestHeartbeatAndClose(t *testing.T) {
	const guild = "1551744840499200000"
	beat := make(chan float64, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		_ = ws.WriteJSON(map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 25}})
		var hello struct {
			Op int `json:"op"`
		}
		if ws.ReadJSON(&hello) != nil {
			return
		}
		if hello.Op != 2 {
			t.Errorf("first opcode = %d", hello.Op)
		}
		_ = ws.WriteJSON(map[string]any{"op": 0, "t": "READY", "s": 42, "d": map[string]any{"session_id": "sess"}})
		var hb struct {
			Op int     `json:"op"`
			D  float64 `json:"d"`
		}
		if ws.ReadJSON(&hb) != nil {
			return
		}
		if hb.Op == 1 {
			beat <- hb.D
		}
		_ = ws.WriteJSON(map[string]any{"op": 11, "d": nil})
		for {
			var x any
			if ws.ReadJSON(&x) != nil {
				return
			}
		}
	}))
	defer server.Close()
	src := connector.SourceConfig{ID: "chat", Type: discord.Type, Containers: []string{"1551744840499200001"}, Settings: json.RawMessage(fmt.Sprintf(`{"guild":%q,"gateway_url":%q}`, guild, "ws"+strings.TrimPrefix(server.URL, "http"))), Secrets: map[string]string{"token": "test-bot-token"}}
	c, err := discord.New(src)
	if err != nil {
		t.Fatal(err)
	}
	rec := &connector.Recorder{}
	gate := connector.NewGate(rec, src.ID, c.Describe(), connector.NewAllowlist(src))
	done := make(chan error, 1)
	go func() { done <- c.Stream(t.Context(), gate) }()
	select {
	case seq := <-beat:
		if seq != 42 {
			t.Errorf("heartbeat seq = %v, want 42", seq)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("heartbeat not sent")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stream outlived Close")
	}
}

func TestRejectedGatewayReportsFailedWithoutSecrets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		_ = ws.WriteJSON(map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 5000}})
		var hello any
		if ws.ReadJSON(&hello) != nil {
			return
		}
		_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(4014, "bad intents"), time.Now().Add(time.Second))
	}))
	defer server.Close()
	src := connector.SourceConfig{ID: "chat", Type: discord.Type, Containers: []string{"1551744840499200001"}, Settings: json.RawMessage(fmt.Sprintf(`{"guild":"1551744840499200000","gateway_url":%q}`, "ws"+strings.TrimPrefix(server.URL, "http"))), Secrets: map[string]string{"token": "secret-value"}}
	c, err := discord.New(src)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(t.Context())
	gate := connector.NewGate(&connector.Recorder{}, src.ID, c.Describe(), connector.NewAllowlist(src))
	if err := c.Stream(t.Context(), gate); err == nil {
		t.Fatal("rejected gateway returned no error")
	}
	h := c.Health(t.Context())
	if h.Status != connector.HealthFailed || strings.Contains(h.Detail, "secret-value") {
		t.Errorf("failed health = %+v", h)
	}
}

func TestInvalidSequenceStartsNewSession(t *testing.T) {
	var mu sync.Mutex
	handshakes := []int{}
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		mu.Lock()
		count++
		n := count
		mu.Unlock()
		_ = ws.WriteJSON(map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 5000}})
		var hello struct {
			Op int `json:"op"`
		}
		if ws.ReadJSON(&hello) != nil {
			return
		}
		mu.Lock()
		handshakes = append(handshakes, hello.Op)
		mu.Unlock()
		if n == 1 {
			_ = ws.WriteJSON(map[string]any{"op": 0, "t": "READY", "s": 4, "d": map[string]any{"session_id": "sess"}})
			_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(4007, "invalid seq"), time.Now().Add(time.Second))
			return
		}
		_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(4014, "disallowed"), time.Now().Add(time.Second))
	}))
	defer server.Close()
	src := connector.SourceConfig{ID: "chat", Type: discord.Type, Containers: []string{"1551744840499200001"}, Settings: json.RawMessage(fmt.Sprintf(`{"guild":"1551744840499200000","gateway_url":%q}`, "ws"+strings.TrimPrefix(server.URL, "http"))), Secrets: map[string]string{"token": "test"}}
	c, err := discord.New(src)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(t.Context())
	gate := connector.NewGate(&connector.Recorder{}, src.ID, c.Describe(), connector.NewAllowlist(src))
	if err := c.Stream(t.Context(), gate); err == nil {
		t.Fatal("invalid sequence did not end stream")
	}
	if err := c.Stream(t.Context(), gate); err == nil {
		t.Fatal("rejected second session did not end stream")
	}
	mu.Lock()
	got := append([]int(nil), handshakes...)
	mu.Unlock()
	if len(got) != 2 || got[0] != 2 || got[1] != 2 {
		t.Errorf("handshakes = %v, want IDENTIFY twice", got)
	}
}

func TestPermanentRejectionStopsRuntimeRetry(t *testing.T) {
	var mu sync.Mutex
	connections := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		mu.Lock()
		connections++
		mu.Unlock()
		_ = ws.WriteJSON(map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 5000}})
		var hello any
		if ws.ReadJSON(&hello) != nil {
			return
		}
		_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(4014, "disallowed"), time.Now().Add(time.Second))
	}))
	defer server.Close()
	src := connector.SourceConfig{ID: "chat", Type: discord.Type, Containers: []string{"1551744840499200001"}, Settings: json.RawMessage(fmt.Sprintf(`{"guild":"1551744840499200000","gateway_url":%q}`, "ws"+strings.TrimPrefix(server.URL, "http"))), Secrets: map[string]string{"token": "BOT_TOKEN_ENV"}}
	reg := connector.NewRegistry()
	if err := reg.Register(discord.Type, discord.Factory); err != nil {
		t.Fatal(err)
	}
	rt, err := connector.NewRuntime(t.Context(), connector.RuntimeOptions{Sources: []connector.SourceConfig{src}, Registry: reg, Sink: &connector.Recorder{}, Lookup: func(string) (string, bool) { return "test", true }, Cadence: connector.Cadence{MinRefresh: time.Millisecond, Refresh: time.Millisecond, MaxBackoff: 5 * time.Millisecond, Shutdown: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- rt.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("runtime did not stop")
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for rt.Health(t.Context()).Sources[0].Status != connector.HealthFailed {
		if time.Now().After(deadline) {
			t.Fatal("failed health not reported")
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(30 * time.Millisecond)
	mu.Lock()
	got := connections
	mu.Unlock()
	if got != 1 {
		t.Errorf("permanent rejection made %d connections, want one", got)
	}
}
