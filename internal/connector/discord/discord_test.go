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
	"github.com/kpenfound/hearsay/internal/connector/connectortest"
	"github.com/kpenfound/hearsay/internal/connector/discord"
	"github.com/kpenfound/hearsay/internal/poll"
)

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

const (
	replayGuild   = "1551744840499200000"
	replayPublic  = "1551744840499200001"
	replayPrivate = "1551744840499200002"
	replayMessage = "1551744840499200005"
	replayThread  = "thread:1551744840499200004"
	replayFinal   = "1551744840499200010@perm:"
)

type replayResult struct {
	byID        map[string]connector.Event
	deliveries  map[string]int
	handshakes  []int
	reconnected connector.SourceHealth
	final       connector.SourceHealth
}

func replayGateway(t *testing.T, containers ...string) replayResult {
	t.Helper()
	var mu sync.Mutex
	connections := 0
	handshakes := []int{}
	resumeStarted := make(chan struct{}, 1)
	releaseResume := make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseResume) })
	t.Cleanup(release)
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
			if e["t"] == "MESSAGE_CREATE" && e["d"].(map[string]any)["id"] == replayMessage {
				d := e["d"].(map[string]any)
				d["content"] = "<@1551744840499200099> check this, not <@1551744840499200088>"
				d["mentions"] = []map[string]any{{"id": "1551744840499200099", "username": "shed", "bot": true}, {"id": "1551744840499200088", "username": "sam"}, {"id": "1551744840499200077", "username": "implicit"}}
			}
			if err := ws.WriteJSON(e); err != nil {
				return
			}
			if n == 1 && e["t"] == "MESSAGE_CREATE" && e["d"].(map[string]any)["id"] == replayMessage {
				if err := ws.WriteJSON(e); err != nil {
					return
				}
			}
		}
		if n == 1 {
			return
		}
		for {
			var x any
			if err := ws.ReadJSON(&x); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	gatewayURL = "ws" + strings.TrimPrefix(server.URL, "http")
	src := connector.SourceConfig{ID: "chat", Type: discord.Type, Containers: containers, Settings: json.RawMessage(fmt.Sprintf(`{"guild":%q,"gateway_url":%q}`, replayGuild, gatewayURL)), Secrets: map[string]string{"token": "BOT_TOKEN_ENV"}}
	sink := connectortest.NewDeduplicating()
	rt := startRuntime(t, src, func(o *connector.RuntimeOptions) { o.Sink = sink })
	select {
	case <-resumeStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("gateway did not reconnect")
	}
	result := replayResult{reconnected: rt.Health(t.Context()).Sources[0]}
	release()
	sink.Await(t, "the replayed gateway to finish", func() bool {
		for _, ev := range sink.Events() {
			if strings.HasPrefix(ev.NativeID, replayFinal) {
				return true
			}
		}
		return false
	})
	result.final = rt.Health(t.Context()).Sources[0]
	result.byID = map[string]connector.Event{}
	for _, ev := range sink.Events() {
		result.byID[ev.NativeID] = ev
	}
	result.deliveries = map[string]int{}
	for _, ev := range sink.Deliveries() {
		result.deliveries[ev.NativeID]++
	}
	mu.Lock()
	result.handshakes = append([]int(nil), handshakes...)
	mu.Unlock()
	return result
}

func TestGatewayReconnectResumesSession(t *testing.T) {
	r := replayGateway(t, replayPublic, replayPrivate)
	if len(r.handshakes) < 2 || r.handshakes[0] != 2 || r.handshakes[1] != 6 {
		t.Errorf("handshakes = %v, want IDENTIFY then RESUME", r.handshakes)
	}
	if h := r.reconnected; h.Status != connector.HealthDegraded || h.Detail != "reconnecting" {
		t.Errorf("health while reconnecting = %+v", h)
	}
	if r.final.Status != connector.HealthOK {
		t.Errorf("health after resuming = %+v", r.final)
	}
}

func TestGatewayRedeliveredDispatchKeepsEventID(t *testing.T) {
	r := replayGateway(t, replayPublic, replayPrivate)
	originalDeliveries := 0
	for id, n := range r.deliveries {
		if strings.HasPrefix(id, replayMessage+"@perm:") {
			originalDeliveries += n
		}
	}
	if originalDeliveries != 2 || r.deliveries[replayMessage+":tombstone"] != 2 {
		t.Errorf("delivery counts = %v", r.deliveries)
	}
}

func TestGatewayMessageMentionsKeepExplicitHints(t *testing.T) {
	r := replayGateway(t, replayPublic, replayPrivate)
	original, ok := eventPrefix(r.byID, replayMessage+"@perm:")
	if !ok {
		t.Fatal("original message missing")
	}
	m := original.Payload.Mentions
	if len(m) != 2 || m[0].NativeID != "1551744840499200099" || m[0].Kind != connector.IdentityBot || m[1].NativeID != "1551744840499200088" {
		t.Errorf("explicit mention hints = %+v", m)
	}
}

func TestGatewayThreadContainment(t *testing.T) {
	r := replayGateway(t, replayPublic, replayPrivate)
	original, ok := eventPrefix(r.byID, replayMessage+"@perm:")
	if !ok {
		t.Fatal("original message missing")
	}
	if original.Payload.Thread != replayThread || original.Payload.Parent != replayThread || original.Payload.Container.NativeID != replayPublic {
		t.Errorf("thread containment = %+v", original.Payload)
	}
	if reply, ok := eventPrefix(r.byID, "1551744840499200009@perm:"); !ok || reply.Payload.Parent != replayMessage || reply.Payload.Thread != replayThread {
		t.Errorf("reply relationship = %+v", reply)
	}
	thread, ok := eventPrefix(r.byID, replayThread+"@perm:")
	if !ok {
		t.Fatal("thread artifact missing")
	}
	if want := "https://discord.com/channels/" + replayGuild + "/1551744840499200004"; thread.Payload.URL != want {
		t.Errorf("thread URL = %q", thread.Payload.URL)
	}
	if _, ok := eventPrefix(r.byID, "1551744840499200004@perm:"); !ok {
		t.Error("starter message with same Discord id missing")
	}
}

func TestGatewayEditIsNewRevisionAtOriginalTime(t *testing.T) {
	r := replayGateway(t, replayPublic, replayPrivate)
	original, _ := eventPrefix(r.byID, replayMessage+"@perm:")
	edited, ok := eventPrefix(r.byID, replayMessage+"@2026-09-22T01:00:00.000000+00:00+perm:")
	if !ok {
		t.Fatal("edited revision missing")
	}
	if edited.Payload.Revision == nil || !strings.HasPrefix(edited.Payload.Revision.Token, "2026-09-22T01:00:00.000000+00:00+perm:") || !edited.Time.Equal(original.Time) {
		t.Errorf("edited revision = %+v", edited)
	}
}

func TestGatewayDeletionsEmitTombstones(t *testing.T) {
	r := replayGateway(t, replayPublic, replayPrivate)
	if tomb, ok := r.byID[replayMessage+":tombstone"]; !ok || tomb.Payload.Target != replayMessage {
		t.Errorf("message tombstone = %+v", tomb)
	}
	if tomb, ok := r.byID[replayThread+":tombstone"]; !ok || tomb.Payload.Target != replayThread {
		t.Errorf("thread tombstone = %+v", tomb)
	}
	if tomb, ok := r.byID["1551744840499200008:tombstone"]; !ok || tomb.Payload.Target != "1551744840499200008" {
		t.Errorf("bulk tombstone = %+v", tomb)
	}
}

func TestGatewayReactionAndRemovalEvents(t *testing.T) {
	r := replayGateway(t, replayPublic, replayPrivate)
	reaction := replayMessage + ":reaction:1551744840499200007:%F0%9F%91%8D"
	if _, ok := r.byID[reaction]; !ok {
		t.Error("reaction missing")
	}
	if ev, ok := r.byID[reaction+":tombstone"]; !ok || ev.Payload.Target != reaction {
		t.Errorf("reaction tombstone = %+v", ev)
	}
}

func TestGatewayPrivateChannelEventsReadableByThatChannelOnly(t *testing.T) {
	r := replayGateway(t, replayPublic, replayPrivate)
	priv, ok := eventPrefix(r.byID, "1551744840499200008@perm:")
	if !ok {
		t.Fatal("private channel event missing")
	}
	if len(priv.ACL) != 1 || priv.ACL[0].Kind != connector.ACLGroup || priv.ACL[0].NativeID != replayPrivate {
		t.Errorf("private ACL = %+v", priv.ACL)
	}
	if r.final.Dropped != 1 {
		t.Errorf("dropped = %d, want only the unlisted channel's message", r.final.Dropped)
	}
}

func TestGatewayACLFollowsGuildRoleChange(t *testing.T) {
	r := replayGateway(t, replayPublic, replayPrivate)
	changed, ok := eventPrefix(r.byID, replayFinal)
	if !ok || len(changed.ACL) != 1 || changed.ACL[0].Kind != connector.ACLGroup || changed.ACL[0].NativeID != replayPublic {
		t.Errorf("ACL after guild role change = %+v", changed.ACL)
	}
}

func TestGatewayIngestDeniesChannelsNotInContainers(t *testing.T) {
	r := replayGateway(t, replayPublic)
	if _, ok := eventPrefix(r.byID, "1551744840499200006@perm:"); ok {
		t.Error("non-allowlisted message was emitted")
	}
	if _, ok := eventPrefix(r.byID, "1551744840499200008@perm:"); ok {
		t.Error("message of an unlisted private channel was emitted")
	}
	if _, ok := r.byID["1551744840499200008:tombstone"]; ok {
		t.Error("bulk deletion in an unlisted channel was emitted")
	}
	if r.final.Dropped != 3 {
		t.Errorf("dropped = %d, want 3", r.final.Dropped)
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
		for range 5 {
			var hb struct {
				Op int     `json:"op"`
				D  float64 `json:"d"`
			}
			if ws.ReadJSON(&hb) != nil {
				return
			}
			_ = ws.WriteJSON(map[string]any{"op": 11, "d": nil})
			if hb.Op == 1 && hb.D == 42 {
				beat <- hb.D
				break
			}
		}
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
	t.Cleanup(server.Close)
	src := connector.SourceConfig{ID: "chat", Type: discord.Type, Containers: []string{"1551744840499200001"}, Settings: json.RawMessage(fmt.Sprintf(`{"guild":"1551744840499200000","gateway_url":%q}`, "ws"+strings.TrimPrefix(server.URL, "http"))), Secrets: map[string]string{"token": "BOT_TOKEN_ENV"}}
	rt := startRuntime(t, src, func(*connector.RuntimeOptions) {})
	poll.Until(t, "failed health", func() bool { return rt.Health(t.Context()).Sources[0].Status == connector.HealthFailed })
	time.Sleep(30 * time.Millisecond)
	mu.Lock()
	got := connections
	mu.Unlock()
	if got != 1 {
		t.Errorf("permanent rejection made %d connections, want one", got)
	}
}

func eventPrefix(events map[string]connector.Event, prefix string) (connector.Event, bool) {
	for id, ev := range events {
		if strings.HasPrefix(id, prefix) {
			return ev, true
		}
	}
	return connector.Event{}, false
}

func startRuntime(t *testing.T, src connector.SourceConfig, configure func(*connector.RuntimeOptions)) *connector.Runtime {
	t.Helper()
	rt, _ := runRuntime(t, src, configure)
	return rt
}

func runRuntime(t *testing.T, src connector.SourceConfig, configure func(*connector.RuntimeOptions)) (*connector.Runtime, func()) {
	t.Helper()
	return connectortest.Run(t, discord.Type, discord.Factory, src, func(string) (string, bool) { return "bot-token", true }, configure)
}
