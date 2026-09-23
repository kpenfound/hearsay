package agent_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/agent"
	"github.com/kpenfound/hearsay/internal/principal"
	"github.com/kpenfound/hearsay/internal/service/connectors"
	"github.com/kpenfound/hearsay/internal/service/distiller"
)

const sourceID = "sessions"

// The tokens, by the environment variable the principals name.
var env = map[string]string{
	"SHED_TOKEN":  "shed-secret",
	"SCOUT_TOKEN": "scout-secret",
	"KYLE_TOKEN":  "kyle-secret",
}

func lookup(name string) (string, bool) {
	v, ok := env[name]
	return v, ok
}

var principals = []principal.Principal{
	{ID: "kyle", Kind: principal.KindHuman, TokenEnv: "KYLE_TOKEN"},
	{ID: "robin", Kind: principal.KindHuman},
	{ID: "shed", Kind: principal.KindAgent, Class: principal.ClassWorker, TokenEnv: "SHED_TOKEN"},
	{ID: "scout", Kind: principal.KindAgent, Class: principal.ClassObserver, TokenEnv: "SCOUT_TOKEN"},
	{ID: "quiet", Kind: principal.KindAgent, Class: principal.ClassObserver},
	{ID: "eng", Kind: principal.KindTeam, Members: []string{"kyle"}},
}

func source(containers ...string) connector.SourceConfig {
	if len(containers) == 0 {
		containers = []string{connector.AllowAll}
	}
	return connector.SourceConfig{ID: sourceID, Type: agent.Type, Containers: containers}
}

// server is the connector's handler behind the gate the runtime would give it,
// writing to a recorder.
func server(t *testing.T, src connector.SourceConfig) (*httptest.Server, *connector.Recorder) {
	t.Helper()
	c, err := agent.New(src, principals, lookup)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	rec := &connector.Recorder{}
	gate := connector.NewGate(rec, src.ID, c.Describe(), connector.NewAllowlist(src))
	srv := httptest.NewServer(c.Handler(gate))
	t.Cleanup(srv.Close)
	return srv, rec
}

func post(t *testing.T, url, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(out)
}

// The requests of one session, one per kind, as docs/connector-contract.md
// shows them.
const (
	start = `{"on_behalf_of":"kyle","session":"s-01","kind":"agent_session","phase":"start",
		"started_at":"2026-09-23T17:00:00Z","time":"2026-09-23T17:00:00Z"}`
	turn = `{"on_behalf_of":"kyle","session":"s-01","kind":"agent_turn","turn":"1",
		"started_at":"2026-09-23T17:00:00Z","time":"2026-09-23T17:00:05Z","text":"Reading the retry policy before changing it."}`
	call = `{"on_behalf_of":"kyle","session":"s-01","kind":"tool_call","call":"c-1","tool":"get_bundle",
		"started_at":"2026-09-23T17:00:00Z","time":"2026-09-23T17:00:06Z",
		"input":{"scope":"api"},"output":{"tokens":1830}}`
	end = `{"agent":"shed","on_behalf_of":"kyle","session":"s-01","kind":"agent_session","phase":"end",
		"started_at":"2026-09-23T17:00:00Z","time":"2026-09-23T17:10:00Z"}`
)

// A whole session goes in as four revisions of one artifact, authored by the
// agent the token belongs to and readable by it and the person it acted for.
func TestASessionIsIngested(t *testing.T) {
	srv, rec := server(t, source())
	started := time.Date(2026, 9, 23, 17, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		body     string
		kind     connector.Kind
		nativeID string
		at       time.Time
		text     string
		native   string
	}{
		{"start", start, connector.KindAgentSession, "s-01@start", started, "", `{"session":"s-01","phase":"start"}`},
		{"a turn", turn, connector.KindAgentTurn, "s-01@turn:1", started.Add(5 * time.Second), "Reading the retry policy before changing it.", `{"session":"s-01","turn":"1"}`},
		{"a tool call", call, connector.KindToolCall, "s-01@call:c-1", started.Add(6 * time.Second), "", `{"session":"s-01","call":"c-1","tool":"get_bundle","input":{"scope":"api"},"output":{"tokens":1830}}`},
		{"end", end, connector.KindAgentSession, "s-01@end", started.Add(10 * time.Minute), "", `{"session":"s-01","phase":"end"}`},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, body := post(t, srv.URL, env["SHED_TOKEN"], tt.body)
			if code != http.StatusAccepted {
				t.Fatalf("POST = %d %s, want 202", code, body)
			}
			var accepted agent.Accepted
			if err := json.Unmarshal([]byte(body), &accepted); err != nil {
				t.Fatalf("response %q is not JSON: %v", body, err)
			}
			events := rec.Events()
			if len(events) != i+1 {
				t.Fatalf("recorded %d events, want %d", len(events), i+1)
			}
			ev := events[i]
			if want := connector.EventID(sourceID, tt.nativeID); ev.ID != want || accepted.ID != want {
				t.Errorf("event id = %q, response id = %q, want %q", ev.ID, accepted.ID, want)
			}
			if ev.Kind != tt.kind || ev.NativeID != tt.nativeID || ev.Payload.Artifact != "s-01" {
				t.Errorf("event = %s %s artifact %s, want %s %s artifact s-01", ev.Kind, ev.NativeID, ev.Payload.Artifact, tt.kind, tt.nativeID)
			}
			if !ev.Time.Equal(started) {
				t.Errorf("time = %v, want the session's start %v", ev.Time, started)
			}
			if ev.Payload.Revision == nil || !ev.Payload.Revision.EditedAt.Equal(tt.at) {
				t.Errorf("revision = %+v, want edited_at %v", ev.Payload.Revision, tt.at)
			}
			if ev.Payload.Text != tt.text {
				t.Errorf("text = %q, want %q", ev.Payload.Text, tt.text)
			}
			if string(ev.Payload.Native) != tt.native {
				t.Errorf("native = %s, want %s", ev.Payload.Native, tt.native)
			}
			wantAuthor := connector.Identity{Source: sourceID, Kind: connector.IdentityAgent, NativeID: "shed"}
			if ev.Payload.Author == nil || *ev.Payload.Author != wantAuthor {
				t.Errorf("author = %+v, want %+v", ev.Payload.Author, wantAuthor)
			}
			wantParticipants := []connector.Participant{{Identity: connector.Identity{Source: sourceID, Kind: connector.IdentityUser, NativeID: "kyle"}, Role: connector.RoleAuthor}}
			if !reflect.DeepEqual(ev.Payload.Participants, wantParticipants) {
				t.Errorf("participants = %+v, want %+v", ev.Payload.Participants, wantParticipants)
			}
			if want := (connector.Container{Kind: agent.ContainerStream, NativeID: "shed"}); ev.Payload.Container != want {
				t.Errorf("container = %+v, want %+v", ev.Payload.Container, want)
			}
			wantACL := connector.ACL{
				{Kind: connector.ACLIdentity, Source: sourceID, NativeID: "shed"},
				{Kind: connector.ACLIdentity, Source: sourceID, NativeID: "kyle"},
			}
			if !reflect.DeepEqual(ev.ACL, wantACL) {
				t.Errorf("acl = %+v, want exactly the agent and the person %+v", ev.ACL, wantACL)
			}
			// The distiller makes no document of any of it.
			if target, ok, err := distiller.TargetOf(t.Context(), ev, nil); ok || err != nil {
				t.Errorf("distiller.TargetOf() = %q, %v, %v, want no target", target, ok, err)
			}
		})
	}
}

// Posting an event again is the same event: the same id and the same content,
// which L0 writes once.
func TestAReplayIsTheSameEvent(t *testing.T) {
	for _, body := range []string{start, turn, call, end} {
		srv, rec := server(t, source())
		for range 2 {
			if code, out := post(t, srv.URL, env["SHED_TOKEN"], body); code != http.StatusAccepted {
				t.Fatalf("POST = %d %s, want 202", code, out)
			}
		}
		events := rec.Events()
		if len(events) != 2 || !reflect.DeepEqual(events[0], events[1]) {
			t.Errorf("a replay emitted %+v, want the same event twice", events)
		}
	}
}

// Nothing is emitted for a request the connector refuses, and the refusal says
// whose fault it is.
func TestARequestIsRefused(t *testing.T) {
	tests := []struct {
		name  string
		src   connector.SourceConfig
		token string
		body  string
		want  int
	}{
		{name: "no token", token: "", body: turn, want: http.StatusUnauthorized},
		{name: "a token nobody holds", token: "not-a-token", body: turn, want: http.StatusUnauthorized},
		{name: "a human's token", token: env["KYLE_TOKEN"], body: turn, want: http.StatusUnauthorized},
		{name: "a token for a different agent than the body claims", token: env["SCOUT_TOKEN"], body: end, want: http.StatusForbidden},
		{name: "an unknown human", token: env["SHED_TOKEN"], body: strings.Replace(turn, `"kyle"`, `"nobody"`, 1), want: http.StatusForbidden},
		{name: "acting for an agent", token: env["SHED_TOKEN"], body: strings.Replace(turn, `"kyle"`, `"scout"`, 1), want: http.StatusForbidden},
		{name: "acting for a team", token: env["SHED_TOKEN"], body: strings.Replace(turn, `"kyle"`, `"eng"`, 1), want: http.StatusForbidden},
		{name: "no one to act for", token: env["SHED_TOKEN"], body: strings.Replace(turn, `"on_behalf_of":"kyle",`, ``, 1), want: http.StatusForbidden},
		{name: "an agent the source does not allow", src: source("scout"), token: env["SHED_TOKEN"], body: turn, want: http.StatusForbidden},
		{name: "a field the request does not have", token: env["SHED_TOKEN"], body: strings.Replace(turn, `"turn":"1"`, `"turn":"1","author":"scout"`, 1), want: http.StatusBadRequest},
		{name: "not JSON", token: env["SHED_TOKEN"], body: `turn`, want: http.StatusBadRequest},
		{name: "two events", token: env["SHED_TOKEN"], body: turn + turn, want: http.StatusBadRequest},
		{name: "no session", token: env["SHED_TOKEN"], body: strings.Replace(turn, `"session":"s-01",`, ``, 1), want: http.StatusBadRequest},
		{name: "a session id with an @", token: env["SHED_TOKEN"], body: strings.Replace(turn, `"s-01"`, `"s@01"`, 1), want: http.StatusBadRequest},
		{name: "a kind this source does not emit", token: env["SHED_TOKEN"], body: strings.Replace(turn, `"agent_turn"`, `"message"`, 1), want: http.StatusBadRequest},
		{name: "a turn with no text", token: env["SHED_TOKEN"], body: strings.Replace(turn, `"text":"Reading the retry policy before changing it."`, `"text":""`, 1), want: http.StatusBadRequest},
		{name: "a turn with no id", token: env["SHED_TOKEN"], body: strings.Replace(turn, `"turn":"1",`, ``, 1), want: http.StatusBadRequest},
		{name: "a tool call with no tool", token: env["SHED_TOKEN"], body: strings.Replace(call, `"tool":"get_bundle",`, ``, 1), want: http.StatusBadRequest},
		{name: "a session phase that is neither start nor end", token: env["SHED_TOKEN"], body: strings.Replace(start, `"start"`, `"middle"`, 1), want: http.StatusBadRequest},
		{name: "a session event carrying a tool", token: env["SHED_TOKEN"], body: strings.Replace(start, `"phase"`, `"tool":"x","phase"`, 1), want: http.StatusBadRequest},
		{name: "no start time", token: env["SHED_TOKEN"], body: strings.Replace(turn, `"started_at":"2026-09-23T17:00:00Z",`, ``, 1), want: http.StatusBadRequest},
		{name: "an event before its session started", token: env["SHED_TOKEN"], body: strings.Replace(turn, `17:00:05Z`, `16:59:59Z`, 1), want: http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := tt.src
			if src.ID == "" {
				src = source()
			}
			srv, rec := server(t, src)
			if code, body := post(t, srv.URL, tt.token, tt.body); code != tt.want {
				t.Errorf("POST = %d %s, want %d", code, body, tt.want)
			}
			if n := len(rec.Events()); n != 0 {
				t.Errorf("a refused request emitted %d events", n)
			}
		})
	}
}

// A source that could not accept anything, or could not check who is posting,
// does not start.
func TestNewRefusesASourceItCannotServe(t *testing.T) {
	unset := func(string) (string, bool) { return "", false }
	tests := []struct {
		name       string
		src        connector.SourceConfig
		principals []principal.Principal
		lookup     func(string) (string, bool)
		wantErr    string
	}{
		{name: "an allowed agent", src: source("shed"), principals: principals, lookup: lookup},
		{name: "every agent", src: source(), principals: principals, lookup: lookup},
		{name: "an agent token that is not in the environment", src: source(), principals: principals, lookup: unset, wantErr: "SHED_TOKEN is not set"},
		{name: "no agent that can post", src: source(), principals: principals[:2], lookup: lookup, wantErr: "no agent principal has a token_env"},
		{
			name: "two agents sharing a token", src: source(), lookup: lookup, wantErr: "share an API token",
			principals: append(principals[:4:4], principal.Principal{ID: "copy", Kind: principal.KindAgent, TokenEnv: "SHED_TOKEN"}),
		},
		{name: "a container that is not an agent", src: source("kyle"), principals: principals, lookup: lookup, wantErr: `container "kyle" is not an agent`},
		{
			name: "secrets", principals: principals, lookup: lookup, wantErr: "takes no secrets",
			src: connector.SourceConfig{ID: sourceID, Type: agent.Type, Containers: []string{"*"}, Secrets: map[string]string{"token": "t"}},
		},
		{
			name: "a setting it does not have", principals: principals, lookup: lookup, wantErr: "unknown field",
			src: connector.SourceConfig{ID: sourceID, Type: agent.Type, Containers: []string{"*"}, Settings: json.RawMessage(`{"since":"2026-01-01"}`)},
		},
		{
			name: "no containers", principals: principals, lookup: lookup, wantErr: "containers must be",
			src: connector.SourceConfig{ID: sourceID, Type: agent.Type},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := agent.New(tt.src, tt.principals, tt.lookup)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("New() = %v, want no error", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("New() = %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}

// The source is hosted like any other: mounted under /hooks/<source>, and
// reported by /readyz with the time it last took an event.
func TestTheServiceHostsAndReportsTheSource(t *testing.T) {
	src := source()
	registry := connector.NewRegistry()
	if err := registry.Register(agent.Type, agent.NewFactory(principals, lookup)); err != nil {
		t.Fatal(err)
	}
	var lc net.ListenConfig
	listener, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := "http://" + listener.Addr().String()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- connectors.Run(ctx, &config.Config{Repo: config.Repo{Sources: []connector.SourceConfig{src}}}, connectors.Deps{
			Registry: registry,
			Sink:     &connector.Recorder{},
			Cursors:  connector.NewMemoryCursors(),
			Listener: listener,
		})
	}()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run() = %v", err)
		}
	}()

	// The listener is bound, so these wait for the service rather than fail.
	if code, body := post(t, addr+connector.HookPath(sourceID), env["SHED_TOKEN"], start); code != http.StatusAccepted {
		t.Fatalf("POST %s = %d %s, want 202", connector.HookPath(sourceID), code, body)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, addr+"/readyz", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var ready connectors.Readiness
	if err := json.NewDecoder(resp.Body).Decode(&ready); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || ready.Status != connector.HealthOK {
		t.Errorf("GET /readyz = %d %+v, want 200 and ok", resp.StatusCode, ready)
	}
	if len(ready.Sources) != 1 || ready.Sources[0].Source != sourceID || ready.Sources[0].Type != agent.Type {
		t.Fatalf("readiness lists %+v, want the agent source", ready.Sources)
	}
	if ready.Sources[0].LastEventAt.IsZero() {
		t.Error("readiness has no last event time after an event was taken")
	}
}
