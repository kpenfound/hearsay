package slack_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/slack"
)

const (
	appToken = "xapp-1-A0HEARSAY-fixture"
	botToken = "xoxb-fixture"
	public   = "C0PUBLIC"
)

// fakeSlack is the Web API and the Socket Mode endpoint, serving recorded
// frames. session runs once per WebSocket connection, numbered from 1.
type fakeSlack struct {
	t        *testing.T
	server   *httptest.Server
	mu       sync.Mutex
	channels map[string]string
	infoCode int
	openErr  string
	opens    int
	session  func(n int, ws *websocket.Conn)
	history  func(http.ResponseWriter, *http.Request)
}

func newFakeSlack(t *testing.T, session func(n int, ws *websocket.Conn)) *fakeSlack {
	t.Helper()
	f := &fakeSlack{t: t, session: session, channels: map[string]string{
		public: `{"id":"C0PUBLIC","name":"eng","is_channel":true,"is_group":false,"is_im":false,"is_mpim":false,"is_private":false,"is_archived":false,"is_ext_shared":false,"is_pending_ext_shared":false,"is_shared":false,"is_org_shared":false,"is_member":true,"context_team_id":"T0001"}`,
	}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/apps.connections.open", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+appToken {
			t.Errorf("apps.connections.open authorization = %q", r.Header.Get("Authorization"))
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.openErr != "" {
			fmt.Fprintf(w, `{"ok":false,"error":%q}`, f.openErr)
			return
		}
		f.opens++
		fmt.Fprintf(w, `{"ok":true,"url":"ws%s/link?n=%d"}`, strings.TrimPrefix(f.server.URL, "http"), f.opens)
	})
	mux.HandleFunc("POST /api/conversations.info", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+botToken {
			t.Errorf("conversations.info authorization = %q", r.Header.Get("Authorization"))
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.infoCode != 0 {
			w.WriteHeader(f.infoCode)
			return
		}
		ch, ok := f.channels[r.FormValue("channel")]
		if !ok {
			fmt.Fprint(w, `{"ok":false,"error":"channel_not_found"}`)
			return
		}
		fmt.Fprintf(w, `{"ok":true,"channel":%s}`, ch)
	})
	mux.HandleFunc("GET /link", func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		n, _ := strconv.Atoi(r.URL.Query().Get("n"))
		f.session(n, ws)
	})
	for _, method := range []string{"conversations.history", "conversations.replies"} {
		mux.HandleFunc("POST /api/"+method, func(w http.ResponseWriter, r *http.Request) {
			if f.history == nil {
				t.Errorf("unexpected %s", r.URL.Path)
				return
			}
			f.history(w, r)
		})
	}
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeSlack) source(readOnly bool) connector.SourceConfig {
	return connector.SourceConfig{
		ID: "slack-acme", Type: slack.Type, Containers: []string{public}, ReadOnly: readOnly,
		Settings: json.RawMessage(fmt.Sprintf(`{"team":"T0001","api_url":%q}`, f.server.URL+"/api")),
		Secrets:  map[string]string{slack.SecretAppToken: "SLACK_APP_TOKEN", slack.SecretBotToken: "SLACK_BOT_TOKEN"},
	}
}

func (f *fakeSlack) resolved() connector.SourceConfig {
	src := f.source(false)
	src.Secrets = map[string]string{slack.SecretAppToken: appToken, slack.SecretBotToken: botToken}
	return src
}

func lookup(name string) (string, bool) {
	switch name {
	case "SLACK_APP_TOKEN":
		return appToken, true
	case "SLACK_BOT_TOKEN":
		return botToken, true
	}
	return "", false
}

func frames(t *testing.T, name string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// deliver sends recorded frames, and reads the acknowledgement Slack expects
// for every event envelope before sending the next one.
func deliver(t *testing.T, ws *websocket.Conn, fs []map[string]any) bool {
	t.Helper()
	for _, frame := range fs {
		if err := ws.WriteJSON(frame); err != nil {
			return false
		}
		if frame["type"] != "events_api" {
			continue
		}
		var ack map[string]any
		_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
		if err := ws.ReadJSON(&ack); err != nil {
			t.Errorf("no acknowledgement for %v: %v", frame["envelope_id"], err)
			return false
		}
		if len(ack) != 1 || ack["envelope_id"] != frame["envelope_id"] {
			t.Errorf("acknowledgement = %v, want envelope_id %v", ack, frame["envelope_id"])
		}
	}
	return true
}

// deduplicating is L0's idempotence: an event id it holds is not written twice.
type deduplicating struct {
	mu         sync.Mutex
	recorder   connector.Recorder
	seen       map[string]bool
	deliveries []string
}

func (s *deduplicating) Emit(ctx context.Context, ev connector.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deliveries = append(s.deliveries, ev.ID)
	if s.seen[ev.ID] {
		return nil
	}
	s.seen[ev.ID] = true
	return s.recorder.Emit(ctx, ev)
}

func (s *deduplicating) delivered() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.deliveries)
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// want is what one emitted event must say. Messages carry a hashed content
// token, so their native id is checked as `<artifact>@<token>`.
type want struct {
	kind                 connector.Kind
	artifact             string
	author               string
	authorKind           connector.IdentityKind
	text, parent, target string
	thread               string
	url                  string
	mentions             []string
	editedAt             time.Time
	emoji                string
	revision             bool
}

func TestStreamThroughRuntimeGate(t *testing.T) {
	root := public + "/1758700000.000100"
	reply := public + "/1758700060.000200"
	reaction := root + ":reaction:U0SAM:%2B1"
	wants := []want{
		{kind: connector.KindMessage, artifact: root, author: "U0KYLE", authorKind: connector.IdentityUser, text: "Should we ship the retry change? <@U0SAM>", mentions: []string{"U0SAM"}, url: "https://slack.com/archives/C0PUBLIC/p1758700000000100", revision: true},
		{kind: connector.KindMessage, artifact: reply, author: "U0SAM", authorKind: connector.IdentityUser, text: "Yes, once the tests pass.", parent: root, thread: root, url: "https://slack.com/archives/C0PUBLIC/p1758700060000200?thread_ts=1758700000.000100&cid=C0PUBLIC", revision: true},
		{kind: connector.KindMessage, artifact: reply, author: "U0SAM", authorKind: connector.IdentityUser, text: "Yes, once the integration tests pass.", parent: root, thread: root, url: "https://slack.com/archives/C0PUBLIC/p1758700060000200?thread_ts=1758700000.000100&cid=C0PUBLIC", editedAt: time.Unix(1758700120, 0).UTC(), revision: true},
		{kind: connector.KindMessage, artifact: public + "/1758700180.000300", author: "B0DEPLOY", authorKind: connector.IdentityBot, text: "Deployed v1.2.3", url: "https://slack.com/archives/C0PUBLIC/p1758700180000300", revision: true},
		{kind: connector.KindReaction, artifact: reaction, author: "U0SAM", authorKind: connector.IdentityUser, parent: root, url: "https://slack.com/archives/C0PUBLIC/p1758700000000100", emoji: "+1"},
		{kind: connector.KindTombstone, artifact: reaction + ":tombstone", target: reaction, url: "https://slack.com/archives/C0PUBLIC/p1758700000000100"},
		{kind: connector.KindTombstone, artifact: public + "/1758700180.000300:tombstone", target: public + "/1758700180.000300", url: "https://slack.com/archives/C0PUBLIC/p1758700180000300"},
		{kind: connector.KindMessage, artifact: public + "/1758700240.000400", author: "U0KYLE", authorKind: connector.IdentityUser, text: "Scratch that.", url: "https://slack.com/archives/C0PUBLIC/p1758700240000400", revision: true},
		{kind: connector.KindTombstone, artifact: public + "/1758700240.000400:tombstone", target: public + "/1758700240.000400", url: "https://slack.com/archives/C0PUBLIC/p1758700240000400"},
	}
	times := map[string]time.Time{
		root:  time.Unix(1758700000, 100000).UTC(),
		reply: time.Unix(1758700060, 200000).UTC(),
	}
	for _, readOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("read_only %v", readOnly), func(t *testing.T) {
			refreshed := make(chan struct{})
			holdThird := make(chan struct{})
			third := make(chan struct{})
			release := sync.OnceFunc(func() { close(holdThird) })
			defer release()
			fake := newFakeSlack(t, func(n int, ws *websocket.Conn) {
				switch n {
				case 1:
					deliver(t, ws, frames(t, "first"))
					// Slack closes the socket some seconds after its disconnect frame.
					_, _, _ = ws.ReadMessage()
				case 2:
					close(refreshed)
					// A connection that breaks without warning, after Slack
					// redelivers the reply it believes was never acknowledged.
					deliver(t, ws, frames(t, "second"))
				case 3:
					close(third)
					<-holdThird
					deliver(t, ws, frames(t, "second")[:1])
					_, _, _ = ws.ReadMessage()
				}
			})
			reg := connector.NewRegistry()
			if err := reg.Register(slack.Type, slack.Factory); err != nil {
				t.Fatal(err)
			}
			sink := &deduplicating{seen: map[string]bool{}}
			rt, err := connector.NewRuntime(t.Context(), connector.RuntimeOptions{
				Sources: []connector.SourceConfig{fake.source(readOnly)}, Registry: reg, Sink: sink, Lookup: lookup,
				Cadence: connector.Cadence{MinRefresh: time.Millisecond, Refresh: time.Millisecond, MaxBackoff: 5 * time.Millisecond, Shutdown: time.Second},
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- rt.Run(ctx) }()
			defer func() {
				cancel()
				release()
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
			case <-refreshed:
			case <-time.After(5 * time.Second):
				t.Fatal("the connector did not open a new connection when Slack asked")
			}
			select {
			case <-third:
			case <-time.After(5 * time.Second):
				t.Fatal("the runtime did not reconnect a broken stream")
			}
			// Slack's refresh is not a failure; the broken connection is.
			h := rt.Health(t.Context()).Sources[0]
			if h.Status != connector.HealthDegraded || h.Detail != "reconnecting" || h.StreamFailures != 1 {
				t.Errorf("health while reconnecting = %+v, want degraded, reconnecting, one stream failure", h)
			}
			release()
			waitFor(t, "a connected stream", func() bool { return rt.Health(t.Context()).Sources[0].Status == connector.HealthOK })
			h = rt.Health(t.Context()).Sources[0]
			if h.Detail != "connected" || h.Dropped != 1 || h.LastEventAt.IsZero() {
				t.Errorf("connected health = %+v, want connected, one allowlist drop, a last event", h)
			}

			got := sink.recorder.Events()
			if len(got) != len(wants) {
				for _, ev := range got {
					t.Logf("emitted %s", ev.NativeID)
				}
				t.Fatalf("emitted %d events, want %d", len(got), len(wants))
			}
			for i, w := range wants {
				ev := got[i]
				check(t, ev, w)
				if at, ok := times[w.artifact]; ok && !ev.Time.Equal(at) {
					t.Errorf("%s time = %v, want %v", ev.NativeID, ev.Time, at)
				}
			}
			// The thread root reported again for its reply, and the reply Slack
			// redelivered, are the observations already held.
			deliveries := sink.delivered()
			if len(deliveries) != len(wants)+2 || deliveries[2] != deliveries[0] || deliveries[len(deliveries)-1] != deliveries[1] {
				t.Errorf("deliveries = %v, want the root's and the reply's repeats to reuse their ids", deliveries)
			}
			if got[1].ID == got[2].ID {
				t.Error("an edited reply kept the id of the reply it revises")
			}
		})
	}
}

func check(t *testing.T, ev connector.Event, w want) {
	t.Helper()
	p := ev.Payload
	if ev.Kind != w.kind || p.Artifact != w.artifact || ev.Source != "slack-acme" {
		t.Errorf("event %s = %s %s, want %s %s", ev.NativeID, ev.Kind, p.Artifact, w.kind, w.artifact)
		return
	}
	if err := ev.Validate(); err != nil {
		t.Errorf("%s: %v", ev.NativeID, err)
	}
	if len(ev.ACL) != 1 || ev.ACL[0].Kind != connector.ACLPublic {
		t.Errorf("%s ACL = %+v, want public", ev.NativeID, ev.ACL)
	}
	if p.Container != (connector.Container{Kind: connector.ContainerChannel, NativeID: public}) {
		t.Errorf("%s container = %+v", ev.NativeID, p.Container)
	}
	if w.revision {
		if p.Revision == nil || !strings.HasSuffix(p.Revision.Token, "+perm:public") || ev.NativeID != w.artifact+"@"+p.Revision.Token || !p.Revision.EditedAt.Equal(w.editedAt) {
			t.Errorf("%s revision = %+v, want a content token with perm:public, edited at %v", ev.NativeID, p.Revision, w.editedAt)
		}
	} else if ev.NativeID != w.artifact || p.Revision != nil {
		t.Errorf("%s revision = %+v, want the artifact as its own native id", ev.NativeID, p.Revision)
	}
	author := ""
	if p.Author != nil {
		author = p.Author.NativeID
		if p.Author.Kind != w.authorKind || p.Author.Source != "slack-acme" {
			t.Errorf("%s author = %+v, want kind %s", ev.NativeID, p.Author, w.authorKind)
		}
	}
	if author != w.author || p.Text != w.text || p.Parent != w.parent || p.Thread != w.thread || p.Target != w.target || p.URL != w.url {
		t.Errorf("%s = author %q text %q parent %q thread %q target %q url %q; want %q %q %q %q %q %q",
			ev.NativeID, author, p.Text, p.Parent, p.Thread, p.Target, p.URL, w.author, w.text, w.parent, w.thread, w.target, w.url)
	}
	var mentions []string
	for _, m := range p.Mentions {
		mentions = append(mentions, m.NativeID)
	}
	if !slices.Equal(mentions, w.mentions) {
		t.Errorf("%s mentions = %v, want %v", ev.NativeID, mentions, w.mentions)
	}
	emoji := ""
	if len(p.Native) > 0 {
		var n struct{ Emoji string }
		_ = json.Unmarshal(p.Native, &n)
		emoji = n.Emoji
	}
	if emoji != w.emoji {
		t.Errorf("%s emoji = %q, want %q", ev.NativeID, emoji, w.emoji)
	}
}

// TestStreamRefusesUnsupportedChannels covers the channel classes only Slack
// can tell apart, which stop the source, and the failures a retry can fix,
// which do not.
func TestStreamRefusesUnsupportedChannels(t *testing.T) {
	tests := []struct {
		name      string
		channel   string // conversations.info's channel; "" is channel_not_found
		infoCode  int
		openErr   string
		permanent bool
		detail    string
	}{
		{name: "legacy private group", channel: `{"id":"C0PUBLIC","is_group":true,"is_member":true}`, permanent: true, detail: "is a private channel"},
		{name: "direct message", channel: `{"id":"C0PUBLIC","is_im":true}`, permanent: true, detail: "is a direct message"},
		{name: "group direct message", channel: `{"id":"C0PUBLIC","is_mpim":true,"is_private":true}`, permanent: true, detail: "is a group direct message"},
		{name: "slack connect", channel: `{"id":"C0PUBLIC","is_channel":true,"is_ext_shared":true,"is_shared":true,"is_member":true}`, permanent: true, detail: "Slack Connect"},
		{name: "slack connect invitation", channel: `{"id":"C0PUBLIC","is_channel":true,"is_pending_ext_shared":true,"is_member":true}`, permanent: true, detail: "Slack Connect"},
		{name: "channel the token cannot see", permanent: true, detail: "channel_not_found"},
		{name: "revoked app token", channel: `{"id":"C0PUBLIC","is_channel":true,"is_member":true}`, openErr: "invalid_auth", permanent: true, detail: "app-level token: invalid_auth"},
		{name: "slack unavailable", infoCode: http.StatusServiceUnavailable, detail: "reconnecting"},
		{name: "rate limited", channel: `{"id":"C0PUBLIC","is_channel":true,"is_member":true}`, openErr: "ratelimited", detail: "reconnecting"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeSlack(t, func(int, *websocket.Conn) { t.Error("dialled a source that should not connect") })
			fake.infoCode, fake.openErr = tt.infoCode, tt.openErr
			delete(fake.channels, public)
			if tt.channel != "" {
				fake.channels[public] = tt.channel
			}
			c, err := slack.New(fake.resolved())
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close(t.Context())
			err = c.Stream(t.Context(), &connector.Recorder{})
			if err == nil || errors.Is(err, connector.ErrStreamPermanent) != tt.permanent {
				t.Fatalf("Stream() = %v, want permanent %v", err, tt.permanent)
			}
			h := c.Health(t.Context())
			wantStatus := connector.HealthDegraded
			if tt.permanent {
				wantStatus = connector.HealthFailed
			}
			if h.Status != wantStatus || !strings.Contains(h.Detail, tt.detail) {
				t.Errorf("health = %+v, want %s containing %q", h, wantStatus, tt.detail)
			}
		})
	}
}

// TestStreamReportsAChannelWithoutTheApp: Slack sends nothing from a channel
// the app was not invited to, which health says, and a link Slack disabled
// stops the stream.
func TestStreamReportsAChannelWithoutTheApp(t *testing.T) {
	checked := make(chan struct{})
	fake := newFakeSlack(t, func(_ int, ws *websocket.Conn) {
		deliver(t, ws, frames(t, "second")[:1])
		<-checked
		_ = ws.WriteJSON(map[string]string{"type": "disconnect", "reason": "link_disabled"})
		_, _, _ = ws.ReadMessage()
	})
	fake.channels[public] = `{"id":"C0PUBLIC","is_channel":true,"is_member":false}`
	c, err := slack.New(fake.resolved())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(t.Context())
	done := make(chan error, 1)
	go func() { done <- c.Stream(t.Context(), &connector.Recorder{}) }()
	waitFor(t, "hello", func() bool { return c.Health(t.Context()).Detail != "connecting" })
	if h := c.Health(t.Context()); h.Status != connector.HealthDegraded || !strings.Contains(h.Detail, "not a member of C0PUBLIC") {
		t.Errorf("health = %+v, want degraded naming the channel without the app", h)
	}
	close(checked)
	select {
	case err := <-done:
		if !errors.Is(err, connector.ErrStreamPermanent) {
			t.Errorf("Stream() = %v, want permanent", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not stop")
	}
	if h := c.Health(t.Context()); h.Status != connector.HealthFailed || !strings.Contains(h.Detail, "Socket Mode is turned off") {
		t.Errorf("health = %+v, want failed", h)
	}
}

type failing struct{}

func (failing) Emit(context.Context, connector.Event) error { return errors.New("store unavailable") }

// TestStreamLeavesAFailedEventUnacknowledged: Slack delivers again an
// envelope it was not told about, so one whose event did not reach L0 is not
// acknowledged.
func TestStreamLeavesAFailedEventUnacknowledged(t *testing.T) {
	acked := make(chan bool, 1)
	fake := newFakeSlack(t, func(_ int, ws *websocket.Conn) {
		fs := frames(t, "first")
		if err := ws.WriteJSON(fs[0]); err != nil {
			return
		}
		if err := ws.WriteJSON(fs[1]); err != nil {
			return
		}
		_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _, err := ws.ReadMessage()
		acked <- err == nil
	})
	c, err := slack.New(fake.resolved())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(t.Context())
	if err := c.Stream(t.Context(), failing{}); err == nil || !strings.Contains(err.Error(), "store unavailable") {
		t.Errorf("Stream() = %v, want the sink's error", err)
	}
	if <-acked {
		t.Error("an envelope whose event failed was acknowledged")
	}
	if h := c.Health(t.Context()); h.Status != connector.HealthDegraded || h.Detail != "reconnecting" {
		t.Errorf("health = %+v, want reconnecting", h)
	}
}

func TestCloseStopsStream(t *testing.T) {
	fake := newFakeSlack(t, func(_ int, ws *websocket.Conn) {
		deliver(t, ws, frames(t, "second")[:1])
		_, _, _ = ws.ReadMessage()
	})
	c, err := slack.New(fake.resolved())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.Stream(t.Context(), &connector.Recorder{}) }()
	waitFor(t, "hello", func() bool { return c.Health(t.Context()).Status == connector.HealthOK })
	if err := c.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not stop the stream")
	}
	if err := c.Stream(t.Context(), &connector.Recorder{}); !errors.Is(err, connector.ErrClosed) {
		t.Errorf("Stream() after Close = %v, want ErrClosed", err)
	}
}

func TestNewValidatesSource(t *testing.T) {
	valid := func() connector.SourceConfig {
		return connector.SourceConfig{
			ID: "slack", Type: slack.Type, Containers: []string{"C0PUBLIC", "C0SECOND"},
			Settings: json.RawMessage(`{"team":"T0001"}`),
			Secrets:  map[string]string{slack.SecretAppToken: appToken, slack.SecretBotToken: botToken},
		}
	}
	tests := []struct {
		name   string
		change func(*connector.SourceConfig)
		want   string
	}{
		{name: "public channels", change: func(*connector.SourceConfig) {}},
		{name: "read only", change: func(s *connector.SourceConfig) { s.ReadOnly = true }},
		{name: "local fixture api", change: func(s *connector.SourceConfig) {
			s.Settings = json.RawMessage(`{"team":"T0001","api_url":"http://127.0.0.1:9/api"}`)
		}},
		{name: "direct message", change: func(s *connector.SourceConfig) { s.Containers = []string{"D0DIRECT"} }, want: "is a direct message"},
		{name: "private channel", change: func(s *connector.SourceConfig) { s.Containers = []string{"C0PUBLIC", "G0PRIVATE"} }, want: "private channel or group direct message"},
		{name: "every channel", change: func(s *connector.SourceConfig) { s.Containers = []string{connector.AllowAll} }, want: "not *"},
		{name: "channel name", change: func(s *connector.SourceConfig) { s.Containers = []string{"general"} }, want: "must be a public channel id"},
		{name: "no channels", change: func(s *connector.SourceConfig) { s.Containers = nil }, want: "at least one public channel"},
		{name: "no team", change: func(s *connector.SourceConfig) { s.Settings = nil }, want: "team must be a workspace id"},
		{name: "unknown setting", change: func(s *connector.SourceConfig) {
			s.Settings = json.RawMessage(`{"team":"T0001","channels":["C0PUBLIC"]}`)
		}, want: "unknown field"},
		{name: "plain http api", change: func(s *connector.SourceConfig) {
			s.Settings = json.RawMessage(`{"team":"T0001","api_url":"http://slack.example/api"}`)
		}, want: "https outside localhost"},
		{name: "no app token", change: func(s *connector.SourceConfig) { delete(s.Secrets, slack.SecretAppToken) }, want: "app_token is required"},
		{name: "bot token as app token", change: func(s *connector.SourceConfig) { s.Secrets[slack.SecretAppToken] = botToken }, want: "app-level token"},
		{name: "no bot token", change: func(s *connector.SourceConfig) { delete(s.Secrets, slack.SecretBotToken) }, want: "bot_token is required"},
		{name: "user token", change: func(s *connector.SourceConfig) { s.Secrets[slack.SecretBotToken] = "xoxp-user" }, want: "bot token"},
		{name: "unknown secret", change: func(s *connector.SourceConfig) { s.Secrets["signing_secret"] = "x" }, want: `does not read secret "signing_secret"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := valid()
			tt.change(&src)
			c, err := slack.New(src)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("New() = %v", err)
				}
				desc := c.Describe()
				if desc.Type != slack.Type || slices.Contains(desc.Kinds, connector.KindCommand) {
					t.Errorf("Describe() = %+v, want slack with no command kind", desc)
				}
				if h := c.Health(t.Context()); h.Status != connector.HealthDegraded || h.Detail != "connecting" {
					t.Errorf("Health() before a connection = %+v", h)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("New() = %v, want an error containing %q", err, tt.want)
			}
		})
	}
}
