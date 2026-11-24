package conversation_test

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/kpenfound/hearsay/internal/connector/conversation"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/principal"
	"github.com/kpenfound/hearsay/internal/service/connectors"
)

const sourceID = "kern-chat"

// The tokens, by the environment variable the principals name.
var env = map[string]string{
	"KERN_TOKEN":  "kern-secret",
	"SCOUT_TOKEN": "scout-secret",
	"KYLE_TOKEN":  "kyle-secret",
}

func lookup(name string) (string, bool) {
	v, ok := env[name]
	return v, ok
}

// identity is the mapping a principal needs in the conversation source for its
// events to resolve and be read.
func identity(id string) []principal.Identity {
	return []principal.Identity{{Source: sourceID, NativeID: id}}
}

var principals = []principal.Principal{
	{ID: "kyle", Kind: principal.KindHuman, TokenEnv: "KYLE_TOKEN", Identities: identity("kyle")},
	{ID: "robin", Kind: principal.KindHuman, Identities: identity("robin")},
	{ID: "kern", Kind: principal.KindAgent, Class: principal.ClassWorker, TokenEnv: "KERN_TOKEN", Identities: identity("kern")},
	{ID: "scout", Kind: principal.KindAgent, Class: principal.ClassObserver, TokenEnv: "SCOUT_TOKEN", Identities: identity("scout")},
	{ID: "quiet", Kind: principal.KindAgent, Class: principal.ClassObserver},
}

func source() connector.SourceConfig {
	return connector.SourceConfig{
		ID: sourceID, Type: conversation.Type, Containers: []string{"kyle"},
		Settings: json.RawMessage(`{"owner":"kyle","agents":["kern"]}`),
	}
}

// server is the connector's handler behind the gate the runtime would give it,
// writing to a recorder.
func server(t *testing.T, src connector.SourceConfig) (*httptest.Server, *connector.Recorder) {
	t.Helper()
	c, err := conversation.New(src, principals, lookup)
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

// The two messages of one exchange, as docs/connector-contract.md shows them.
const (
	ownerWords = `{"owner":"kyle","conversation":"c-1","exchange":"x-1","role":"owner",
		"time":"2026-11-02T09:00:00Z","text":"Let's use Postgres for the queue."}`
	answer = `{"owner":"kyle","conversation":"c-1","exchange":"x-1","role":"assistant",
		"time":"2026-11-02T09:00:07Z","text":"Postgres it is. I'll note the decision."}`
)

func accepted(t *testing.T, srv *httptest.Server, token, body string) conversation.Accepted {
	t.Helper()
	code, out := post(t, srv.URL, token, body)
	if code != http.StatusAccepted {
		t.Fatalf("POST = %d %s, want 202", code, out)
	}
	var got conversation.Accepted
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("response %q is not JSON: %v", out, err)
	}
	return got
}

// An exchange is two events, authored by the owner and by the agent, in the
// owner's stream, readable by the owner alone.
func TestAnExchangeIsTwoOwnerOnlyEvents(t *testing.T) {
	srv, rec := server(t, source())
	owner := accepted(t, srv, env["KERN_TOKEN"], ownerWords)
	reply := accepted(t, srv, env["KERN_TOKEN"], answer)

	events := rec.Events()
	if len(events) != 2 {
		t.Fatalf("recorded %d events, want 2", len(events))
	}
	tests := []struct {
		name     string
		ev       connector.Event
		response conversation.Accepted
		artifact string
		text     string
		author   connector.Identity
		at       time.Time
	}{
		{"the owner's words", events[0], owner, "c-1/x-1:owner", "Let's use Postgres for the queue.",
			connector.Identity{Source: sourceID, Kind: connector.IdentityUser, NativeID: "kyle"}, time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)},
		{"the answer", events[1], reply, "c-1/x-1:assistant", "Postgres it is. I'll note the decision.",
			connector.Identity{Source: sourceID, Kind: connector.IdentityAgent, NativeID: "kern"}, time.Date(2026, 11, 2, 9, 0, 7, 0, time.UTC)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := tt.ev
			if tt.response.Status != conversation.Stored || tt.response.EventID != ev.ID {
				t.Errorf("response = %+v, want stored with the event id %q", tt.response, ev.ID)
			}
			if ev.Kind != conversation.Kind || ev.Payload.BaseKind != connector.KindMessage {
				t.Errorf("kind = %s base %s, want %s based on message", ev.Kind, ev.Payload.BaseKind, conversation.Kind)
			}
			if ev.Payload.Artifact != tt.artifact || !strings.HasPrefix(ev.NativeID, tt.artifact+"@") || ev.ID != connector.EventID(sourceID, ev.NativeID) {
				t.Errorf("artifact %q native id %q id %q, want a revision of %q", ev.Payload.Artifact, ev.NativeID, ev.ID, tt.artifact)
			}
			if ev.Payload.Thread != "c-1" {
				t.Errorf("thread = %q, want the conversation id c-1", ev.Payload.Thread)
			}
			if !ev.Time.Equal(tt.at) {
				t.Errorf("time = %v, want when the words were said, %v", ev.Time, tt.at)
			}
			if ev.Payload.Text != tt.text {
				t.Errorf("text = %q, want %q", ev.Payload.Text, tt.text)
			}
			if ev.Payload.Author == nil || *ev.Payload.Author != tt.author {
				t.Errorf("author = %+v, want %+v", ev.Payload.Author, tt.author)
			}
			if want := (connector.Container{Kind: conversation.ContainerStream, NativeID: "kyle"}); ev.Payload.Container != want {
				t.Errorf("container = %+v, want %+v", ev.Payload.Container, want)
			}
			wantACL := connector.ACL{{Kind: connector.ACLIdentity, Source: sourceID, NativeID: "kyle"}}
			if !reflect.DeepEqual(ev.ACL, wantACL) {
				t.Errorf("acl = %+v, want the owner alone, %+v", ev.ACL, wantACL)
			}
			// The chat paths take a message from a channel, which this is not.
			if kind, ok := l1.KindFor(ev); ok || l1.ChatRoot(ev) {
				t.Errorf("l1 reads the event as a %q document root (chat root %v), want neither", kind, l1.ChatRoot(ev))
			}
		})
	}
	if events[0].ID == events[1].ID {
		t.Error("the owner's words and the answer share an event id, want two artifacts")
	}
}

// A message posted again with its current content writes nothing, and with
// changed text is a new current revision, never a conflict.
func TestARepostIsUnchangedOrANewRevision(t *testing.T) {
	srv, rec := server(t, source())
	first := accepted(t, srv, env["KERN_TOKEN"], ownerWords)

	again := accepted(t, srv, env["KERN_TOKEN"], ownerWords)
	if again.EventID != first.EventID || again.Status != conversation.Unchanged {
		t.Errorf("an identical repost = %+v, want the same id and unchanged", again)
	}
	if n := len(rec.Events()); n != 1 {
		t.Fatalf("an identical repost emitted, %d events, want 1", n)
	}

	rewritten := strings.Replace(ownerWords, "Postgres for the queue", "Postgres for the job queue", 1)
	second := accepted(t, srv, env["KERN_TOKEN"], rewritten)
	if second.EventID == first.EventID || second.Status != conversation.Stored {
		t.Errorf("changed text = %+v, want a new event, stored", second)
	}
	current, ok, err := rec.CurrentArtifact(t.Context(), sourceID, "c-1/x-1:owner")
	if err != nil || !ok || current.ID != second.EventID || !strings.Contains(current.Payload.Text, "job queue") {
		t.Fatalf("the current revision = %s %v %v, want the rewrite %s", current.ID, ok, err, second.EventID)
	}
	if current.Payload.Artifact != "c-1/x-1:owner" {
		t.Errorf("the rewrite is artifact %q, want the same artifact", current.Payload.Artifact)
	}

	// The rewrite posted again is unchanged, and the words coming back are a
	// third revision rather than the first one over again.
	if got := accepted(t, srv, env["KERN_TOKEN"], rewritten); got.EventID != second.EventID || got.Status != conversation.Unchanged {
		t.Errorf("the rewrite again = %+v, want the same id and unchanged", got)
	}
	back := accepted(t, srv, env["KERN_TOKEN"], ownerWords)
	if back.Status != conversation.Stored || back.EventID == first.EventID || back.EventID == second.EventID {
		t.Errorf("the first words again = %+v, want a new revision", back)
	}
	if current, _, _ := rec.CurrentArtifact(t.Context(), sourceID, "c-1/x-1:owner"); current.ID != back.EventID {
		t.Errorf("the current revision is %s, want the last post %s", current.ID, back.EventID)
	}
}

// Words that were never completed are posted alone, marked, and the mark is
// kept on the stored event.
func TestAnInterruptedMessageKeepsItsMark(t *testing.T) {
	srv, rec := server(t, source())
	body := strings.Replace(ownerWords, `"text"`, `"interrupted":true,"text"`, 1)
	if got := accepted(t, srv, env["KERN_TOKEN"], body); got.Status != conversation.Stored {
		t.Fatalf("response = %+v, want stored", got)
	}
	ev := rec.Events()[0]
	var native conversation.Native
	if err := json.Unmarshal(ev.Payload.Native, &native); err != nil {
		t.Fatal(err)
	}
	want := conversation.Native{Conversation: "c-1", Exchange: "x-1", Role: "owner", Interrupted: true}
	if native != want {
		t.Errorf("native = %+v, want %+v", native, want)
	}
	if !strings.Contains(string(ev.Payload.Native), `"interrupted":true`) {
		t.Errorf("native = %s, want the interrupted mark", ev.Payload.Native)
	}

	// The mark is content: the same words without it are a new revision.
	if got := accepted(t, srv, env["KERN_TOKEN"], ownerWords); got.Status != conversation.Stored || got.EventID == ev.ID {
		t.Errorf("the words unmarked = %+v, want a new revision", got)
	}
	// A completed exchange's words carry no mark at all.
	if strings.Contains(string(rec.Events()[1].Payload.Native), "interrupted") {
		t.Errorf("native = %s, want no mark", rec.Events()[1].Payload.Native)
	}
}

// Nothing is emitted for a request the connector refuses, and every refusal
// says why in the contract's JSON.
func TestARequestIsRefused(t *testing.T) {
	remove := func(field string) string {
		var m map[string]any
		if err := json.Unmarshal([]byte(ownerWords), &m); err != nil {
			t.Fatal(err)
		}
		delete(m, field)
		out, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	tests := []struct {
		name  string
		token string
		body  string
		want  int
		code  string
	}{
		{name: "no token", body: ownerWords, want: http.StatusUnauthorized, code: "unauthorized"},
		{name: "a token nobody holds", token: "not-a-token", body: ownerWords, want: http.StatusUnauthorized, code: "unauthorized"},
		{name: "a human's token", token: env["KYLE_TOKEN"], body: ownerWords, want: http.StatusUnauthorized, code: "unauthorized"},
		{name: "an agent the source does not allow", token: env["SCOUT_TOKEN"], body: ownerWords, want: http.StatusForbidden, code: "forbidden"},
		{name: "an agent the source does not allow with a malformed body", token: env["SCOUT_TOKEN"], body: `{`, want: http.StatusForbidden, code: "forbidden"},
		{name: "a different owner", token: env["KERN_TOKEN"], body: strings.Replace(ownerWords, `"kyle"`, `"robin"`, 1), want: http.StatusForbidden, code: "forbidden"},
		{name: "an owner who is not a principal", token: env["KERN_TOKEN"], body: strings.Replace(ownerWords, `"kyle"`, `"nobody"`, 1), want: http.StatusForbidden, code: "forbidden"},
		{name: "no owner", token: env["KERN_TOKEN"], body: remove("owner"), want: http.StatusBadRequest, code: "invalid_request"},
		{name: "a field the request does not have", token: env["KERN_TOKEN"], body: strings.Replace(ownerWords, `"role"`, `"author":"kern","role"`, 1), want: http.StatusBadRequest, code: "invalid_request"},
		{name: "not JSON", token: env["KERN_TOKEN"], body: `words`, want: http.StatusBadRequest, code: "invalid_request"},
		{name: "truncated JSON", token: env["KERN_TOKEN"], body: ownerWords[:40], want: http.StatusBadRequest, code: "invalid_request"},
		{name: "an array", token: env["KERN_TOKEN"], body: `[` + ownerWords + `]`, want: http.StatusBadRequest, code: "invalid_request"},
		{name: "two messages", token: env["KERN_TOKEN"], body: ownerWords + ownerWords, want: http.StatusBadRequest, code: "invalid_request"},
		{name: "no conversation", token: env["KERN_TOKEN"], body: remove("conversation"), want: http.StatusBadRequest, code: "invalid_request"},
		{name: "no exchange", token: env["KERN_TOKEN"], body: remove("exchange"), want: http.StatusBadRequest, code: "invalid_request"},
		{name: "no role", token: env["KERN_TOKEN"], body: remove("role"), want: http.StatusBadRequest, code: "invalid_request"},
		{name: "no time", token: env["KERN_TOKEN"], body: remove("time"), want: http.StatusBadRequest, code: "invalid_request"},
		{name: "no text", token: env["KERN_TOKEN"], body: remove("text"), want: http.StatusBadRequest, code: "invalid_request"},
		{name: "blank text", token: env["KERN_TOKEN"], body: strings.Replace(ownerWords, `"Let's use Postgres for the queue."`, `"  "`, 1), want: http.StatusBadRequest, code: "invalid_request"},
		{name: "a NUL in the text", token: env["KERN_TOKEN"], body: strings.Replace(ownerWords, `Let's`, `Let\u0000's`, 1), want: http.StatusBadRequest, code: "invalid_request"},
		{name: "a role that is neither", token: env["KERN_TOKEN"], body: strings.Replace(ownerWords, `"role":"owner"`, `"role":"system"`, 1), want: http.StatusBadRequest, code: "invalid_request"},
		{name: "a time that is not RFC 3339", token: env["KERN_TOKEN"], body: strings.Replace(ownerWords, `2026-11-02T09:00:00Z`, `yesterday`, 1), want: http.StatusBadRequest, code: "invalid_request"},
		{name: "a conversation id with a slash", token: env["KERN_TOKEN"], body: strings.Replace(ownerWords, `"c-1"`, `"c/1"`, 1), want: http.StatusBadRequest, code: "invalid_request"},
		{name: "an exchange id with an @", token: env["KERN_TOKEN"], body: strings.Replace(ownerWords, `"x-1"`, `"x@1"`, 1), want: http.StatusBadRequest, code: "invalid_request"},
		{name: "an exchange id that is too long", token: env["KERN_TOKEN"], body: strings.Replace(ownerWords, `"x-1"`, `"`+strings.Repeat("x", 129)+`"`, 1), want: http.StatusBadRequest, code: "invalid_request"},
		{name: "interrupted on the answer", token: env["KERN_TOKEN"], body: strings.Replace(answer, `"text"`, `"interrupted":true,"text"`, 1), want: http.StatusBadRequest, code: "invalid_request"},
		{name: "interrupted that is not a bool", token: env["KERN_TOKEN"], body: strings.Replace(ownerWords, `"text"`, `"interrupted":"yes","text"`, 1), want: http.StatusBadRequest, code: "invalid_request"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, rec := server(t, source())
			code, body := post(t, srv.URL, tt.token, tt.body)
			if code != tt.want {
				t.Errorf("POST = %d %s, want %d", code, body, tt.want)
			}
			var refusal conversation.Refusal
			if err := json.Unmarshal([]byte(body), &refusal); err != nil || refusal.Code != tt.code || refusal.Error == "" {
				t.Errorf("body = %q, want JSON with an error and code %q", body, tt.code)
			}
			if n := len(rec.Events()); n != 0 {
				t.Errorf("a refused request emitted %d events", n)
			}
		})
	}

	t.Run("a GET", func(t *testing.T) {
		srv, _ := server(t, source())
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("GET = %d, want 405", resp.StatusCode)
		}
	})
}

// A message the store refuses to take back is answered 410 and a failure of the
// store is answered 500 without its reason; neither writes.
func TestTheStoreDecidesWhatAMessageIsAnsweredWith(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
		body string
	}{
		{"a deleted message", conversation.ErrDeleted, http.StatusGone, `"code":"deleted","status":"deleted"`},
		{"a failing store", errors.New("connection refused at 10.0.0.9"), http.StatusInternalServerError, `"code":"internal"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, rec := server(t, source())
			rec.Err = tt.err
			code, body := post(t, srv.URL, env["KERN_TOKEN"], ownerWords)
			if code != tt.want || !strings.Contains(body, tt.body) || strings.Contains(body, "10.0.0.9") {
				t.Errorf("POST = %d %s, want %d containing %s and no cause", code, body, tt.want, tt.body)
			}
			if n := len(rec.Events()); n != 0 {
				t.Errorf("emitted %d events", n)
			}
		})
	}
}

// A source that could not accept anything, or whose events nobody could read,
// does not start.
func TestNewRefusesASourceItCannotServe(t *testing.T) {
	unset := func(string) (string, bool) { return "", false }
	with := func(settings, containers string) connector.SourceConfig {
		src := connector.SourceConfig{ID: sourceID, Type: conversation.Type, Containers: []string{"kyle"}, Settings: json.RawMessage(settings)}
		if containers != "" {
			src.Containers = strings.Split(containers, ",")
		}
		return src
	}
	noIdentity := func(id string) []principal.Principal {
		out := make([]principal.Principal, len(principals))
		copy(out, principals)
		for i := range out {
			if out[i].ID == id {
				out[i].Identities = nil
			}
		}
		return out
	}
	tests := []struct {
		name       string
		src        connector.SourceConfig
		principals []principal.Principal
		lookup     func(string) (string, bool)
		wantErr    string
	}{
		{name: "the documented source", src: source(), principals: principals, lookup: lookup},
		{name: "every container", src: with(`{"owner":"kyle","agents":["kern"]}`, "*"), principals: principals, lookup: lookup},
		{name: "two agents", src: with(`{"owner":"kyle","agents":["kern","scout"]}`, ""), principals: principals, lookup: lookup},
		{name: "no owner", src: with(`{"agents":["kern"]}`, ""), principals: principals, lookup: lookup, wantErr: "settings.owner is required"},
		{name: "no settings", src: connector.SourceConfig{ID: sourceID, Type: conversation.Type, Containers: []string{"kyle"}}, principals: principals, lookup: lookup, wantErr: "settings.owner is required"},
		{name: "an owner who is an agent", src: with(`{"owner":"kern","agents":["kern"]}`, "kern"), principals: principals, lookup: lookup, wantErr: "not a configured human"},
		{name: "an owner nobody configured", src: with(`{"owner":"nobody","agents":["kern"]}`, "nobody"), principals: principals, lookup: lookup, wantErr: "not a configured human"},
		{name: "no agents", src: with(`{"owner":"kyle"}`, ""), principals: principals, lookup: lookup, wantErr: "settings.agents is required"},
		{name: "an agent who is a human", src: with(`{"owner":"kyle","agents":["robin"]}`, ""), principals: principals, lookup: lookup, wantErr: "not a configured agent"},
		{name: "an agent twice", src: with(`{"owner":"kyle","agents":["kern","kern"]}`, ""), principals: principals, lookup: lookup, wantErr: "twice"},
		{name: "an agent with no token_env", src: with(`{"owner":"kyle","agents":["quiet"]}`, ""), principals: append(principals[:4:4], principal.Principal{ID: "quiet", Kind: principal.KindAgent, Identities: identity("quiet")}), lookup: lookup, wantErr: "no token_env"},
		{name: "an agent token that is not in the environment", src: source(), principals: principals, lookup: unset, wantErr: "KERN_TOKEN is not set"},
		{
			name: "two allowed agents sharing a token", src: with(`{"owner":"kyle","agents":["kern","copy"]}`, ""), lookup: lookup, wantErr: "share an API token",
			principals: append(principals[:4:4], principal.Principal{ID: "copy", Kind: principal.KindAgent, TokenEnv: "KERN_TOKEN", Identities: identity("copy")}),
		},
		{name: "a setting it does not have", src: with(`{"owner":"kyle","agents":["kern"],"since":"2026"}`, ""), principals: principals, lookup: lookup, wantErr: "unknown field"},
		{name: "no containers", src: connector.SourceConfig{ID: sourceID, Type: conversation.Type, Settings: json.RawMessage(`{"owner":"kyle","agents":["kern"]}`)}, principals: principals, lookup: lookup, wantErr: "containers must be"},
		{name: "a container that is not the owner", src: with(`{"owner":"kyle","agents":["kern"]}`, "robin"), principals: principals, lookup: lookup, wantErr: `container "robin" is not the owner`},
		{
			name: "secrets", principals: principals, lookup: lookup, wantErr: "takes no secrets",
			src: connector.SourceConfig{ID: sourceID, Type: conversation.Type, Containers: []string{"kyle"}, Settings: json.RawMessage(`{"owner":"kyle","agents":["kern"]}`), Secrets: map[string]string{"token": "t"}},
		},
		{name: "an owner with no identity in the source", src: source(), principals: noIdentity("kyle"), lookup: lookup, wantErr: `principal "kyle" has no identity in source "kern-chat"`},
		{name: "an agent with no identity in the source", src: source(), principals: noIdentity("kern"), lookup: lookup, wantErr: `principal "kern" has no identity`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := conversation.New(tt.src, tt.principals, tt.lookup)
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
// reported by /readyz with the time it last took a message.
func TestTheServiceHostsAndReportsTheSource(t *testing.T) {
	src := source()
	registry := connector.NewRegistry()
	if err := registry.Register(conversation.Type, conversation.NewFactory(principals, lookup)); err != nil {
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
	if code, body := post(t, addr+connector.HookPath(sourceID), env["KERN_TOKEN"], ownerWords); code != http.StatusAccepted {
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
	if len(ready.Sources) != 1 || ready.Sources[0].Source != sourceID || ready.Sources[0].Type != conversation.Type {
		t.Fatalf("readiness lists %+v, want the conversation source", ready.Sources)
	}
	if ready.Sources[0].LastEventAt.IsZero() {
		t.Error("readiness has no last event time after a message was taken")
	}
}
