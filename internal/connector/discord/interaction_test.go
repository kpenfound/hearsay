package discord_test

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/discord"
)

const (
	appGuild = "824100000000000000"
	testApp  = "824100000000000009"
)

func testKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func appSource(settings map[string]any) connector.SourceConfig {
	raw, _ := json.Marshal(settings)
	return connector.SourceConfig{ID: "team-chat", Type: discord.Type, Containers: []string{"824100000000000001"},
		Settings: raw, Secrets: map[string]string{discord.SecretToken: "bot-secret"}}
}

// The application settings are set together or not at all, and the
// connector refuses the same half-configured source the API would. A
// read-only source has no application, whatever its settings name, so the
// API registers no command and answers no interaction for it; its settings
// are still checked.
func TestApplicationSettings(t *testing.T) {
	pub, _ := testKey(t)
	key := hex.EncodeToString(pub)
	tests := []struct {
		name     string
		settings map[string]any
		readOnly bool
		app      bool
		wantErr  string
	}{
		{"no application", map[string]any{"guild": appGuild}, false, false, ""},
		{"both", map[string]any{"guild": appGuild, "application_id": testApp, "public_key": key}, false, true, ""},
		{"id alone", map[string]any{"guild": appGuild, "application_id": testApp}, false, false, "public_key"},
		{"key alone", map[string]any{"guild": appGuild, "public_key": key}, false, false, "application_id"},
		{"short key", map[string]any{"guild": appGuild, "application_id": testApp, "public_key": key[:10]}, false, false, "public_key"},
		{"id not a snowflake", map[string]any{"guild": appGuild, "application_id": "app", "public_key": key}, false, false, "application_id"},
		{"both, read-only", map[string]any{"guild": appGuild, "application_id": testApp, "public_key": key}, true, false, ""},
		{"no application, read-only", map[string]any{"guild": appGuild}, true, false, ""},
		{"key alone, read-only", map[string]any{"guild": appGuild, "public_key": key}, true, false, "application_id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := appSource(tt.settings)
			src.ReadOnly = tt.readOnly
			if got := discord.HasApp(src); got != (tt.app || (tt.wantErr != "" && !tt.readOnly)) {
				t.Errorf("HasApp() = %v", got)
			}
			app, err := discord.NewApp(src)
			_, connErr := discord.New(src)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("NewApp() = %v, want an error naming %s", err, tt.wantErr)
				}
				if connErr == nil || !strings.Contains(connErr.Error(), tt.wantErr) {
					t.Errorf("New() = %v, want an error naming %s", connErr, tt.wantErr)
				}
				return
			}
			if err != nil || connErr != nil {
				t.Fatalf("NewApp() = %v, New() = %v", err, connErr)
			}
			if (app != nil) != tt.app {
				t.Errorf("NewApp() = %v, want an application: %v", app, tt.app)
			}
		})
	}
}

func TestVerify(t *testing.T) {
	pub, priv := testKey(t)
	_, other := testKey(t)
	app, err := discord.NewApp(appSource(map[string]any{"guild": appGuild, "application_id": testApp, "public_key": hex.EncodeToString(pub)}))
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"type":1}`)
	sign := func(key ed25519.PrivateKey, stamp string, msg []byte) http.Header {
		h := http.Header{}
		h.Set("X-Signature-Timestamp", stamp)
		h.Set("X-Signature-Ed25519", hex.EncodeToString(ed25519.Sign(key, append([]byte(stamp), msg...))))
		return h
	}
	tests := []struct {
		name   string
		header http.Header
		body   []byte
		want   bool
	}{
		{"signed by the application", sign(priv, "1700000000", body), body, true},
		{"another body", sign(priv, "1700000000", body), []byte(`{"type":2}`), false},
		{"another timestamp", func() http.Header {
			h := sign(priv, "1700000000", body)
			h.Set("X-Signature-Timestamp", "1700000001")
			return h
		}(), body, false},
		{"another key", sign(other, "1700000000", body), body, false},
		{"no signature", http.Header{}, body, false},
		{"not hex", http.Header{"X-Signature-Ed25519": {"zz"}, "X-Signature-Timestamp": {"1"}}, body, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := app.Verify(tt.header, tt.body); got != tt.want {
				t.Errorf("Verify() = %v, want %v", got, tt.want)
			}
		})
	}
}

// fakeREST records the calls Hearsay makes to Discord's REST API.
type fakeREST struct {
	mu     sync.Mutex
	calls  []restCall
	status []int
}

type restCall struct {
	method, path, auth string
	body               []byte
}

func (f *fakeREST) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.calls = append(f.calls, restCall{r.Method, r.URL.EscapedPath(), r.Header.Get("Authorization"), body})
	status := http.StatusOK
	if len(f.status) > 0 {
		status, f.status = f.status[0], f.status[1:]
	}
	f.mu.Unlock()
	w.WriteHeader(status)
}

func restApp(t *testing.T, rest *fakeREST) *discord.App {
	t.Helper()
	srv := httptest.NewServer(rest)
	t.Cleanup(srv.Close)
	pub, _ := testKey(t)
	app, err := discord.NewApp(appSource(map[string]any{"guild": appGuild, "application_id": testApp,
		"public_key": hex.EncodeToString(pub), "api_url": srv.URL + "/api/v10"}))
	if err != nil {
		t.Fatal(err)
	}
	return app
}

// Registration replaces the guild's commands with /hearsay pin and
// /hearsay merge, whose topics are autocompleted, with the bot token.
func TestRegister(t *testing.T) {
	rest := &fakeREST{}
	app := restApp(t, rest)
	if err := app.Register(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(rest.calls) != 1 {
		t.Fatalf("Register made %d calls, want 1", len(rest.calls))
	}
	call := rest.calls[0]
	if call.method != http.MethodPut || call.path != "/api/v10/applications/"+testApp+"/guilds/"+appGuild+"/commands" || call.auth != "Bot bot-secret" {
		t.Errorf("Register called %s %s with %q", call.method, call.path, call.auth)
	}
	var got []discord.Command
	if err := json.Unmarshal(call.body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != discord.CommandName || len(got[0].Options) != 2 {
		t.Fatalf("registered %+v, want one /hearsay with two subcommands", got)
	}
	pin, merge := got[0].Options[0], got[0].Options[1]
	if pin.Name != discord.CommandPin || pin.Type != 1 || len(pin.Options) != 0 {
		t.Errorf("pin = %+v, want a subcommand with no options", pin)
	}
	if merge.Name != discord.CommandMerge || merge.Type != 1 || len(merge.Options) != 2 {
		t.Fatalf("merge = %+v, want a subcommand with two options", merge)
	}
	for i, name := range []string{discord.OptionFrom, discord.OptionInto} {
		o := merge.Options[i]
		if o.Name != name || o.Type != 3 || !o.Required || !o.Autocomplete {
			t.Errorf("merge option %d = %+v, want required, autocompleted text %q", i, o, name)
		}
	}

	rest.status = []int{http.StatusForbidden}
	if err := app.Register(t.Context()); err == nil || strings.Contains(err.Error(), "bot-secret") {
		t.Errorf("Register() against a 403 = %v, want an error without the token", err)
	}
}

// An edit uses the interaction's token and not the bot's, retries while
// Discord has not seen the deferral, and never puts the token in an error.
func TestEdit(t *testing.T) {
	rest := &fakeREST{status: []int{http.StatusNotFound}}
	app := restApp(t, rest)
	if err := app.Edit(t.Context(), "interaction-token", "Pinned."); err != nil {
		t.Fatal(err)
	}
	if len(rest.calls) != 2 {
		t.Fatalf("Edit made %d calls, want a retry after the 404", len(rest.calls))
	}
	call := rest.calls[1]
	if call.method != http.MethodPatch || call.path != "/api/v10/webhooks/"+testApp+"/interaction-token/messages/@original" || call.auth != "" {
		t.Errorf("Edit called %s %s with %q", call.method, call.path, call.auth)
	}
	var body map[string]any
	if err := json.Unmarshal(call.body, &body); err != nil {
		t.Fatal(err)
	}
	if body["content"] != "Pinned." {
		t.Errorf("Edit sent %s", call.body)
	}
	rest.status = []int{http.StatusBadRequest}
	if err := app.Edit(t.Context(), "interaction-token", "x"); err == nil || strings.Contains(err.Error(), "interaction-token") {
		t.Errorf("Edit() against a 400 = %v, want an error without the token", err)
	}
}

func TestParseInteraction(t *testing.T) {
	tests := []struct {
		name string
		body string
		want discord.Interaction
	}{
		{
			name: "pin in a thread",
			body: `{"id":"900000000000000001","type":2,"token":"tok","guild_id":"` + appGuild + `","channel_id":"700",
				"channel":{"id":"700","type":11,"parent_id":"824100000000000001"},
				"member":{"nick":"Kyle P","user":{"id":"42","username":"kyle","global_name":"Kyle"}},
				"data":{"name":"hearsay","options":[{"name":"pin","type":1}]}}`,
			want: discord.Interaction{ID: "900000000000000001", Type: discord.InteractionCommand, Token: "tok", Guild: appGuild,
				Channel: discord.InteractionChannel{ID: "700", Type: 11, ParentID: "824100000000000001"},
				User:    discord.InteractionUser{ID: "42", Username: "kyle", Global: "Kyle", Nick: "Kyle P"},
				Command: "hearsay", Subcommand: "pin", Options: map[string]string{}},
		},
		{
			name: "merge autocomplete",
			body: `{"id":"900000000000000002","type":4,"guild_id":"` + appGuild + `","channel_id":"701",
				"member":{"user":{"id":"42","username":"kyle"}},
				"data":{"name":"hearsay","options":[{"name":"merge","type":1,"options":[
					{"name":"from","type":3,"value":"topic:a"},{"name":"into","type":3,"value":"lo","focused":true}]}]}}`,
			want: discord.Interaction{ID: "900000000000000002", Type: discord.InteractionAutocomplete, Guild: appGuild,
				Channel: discord.InteractionChannel{ID: "701"}, User: discord.InteractionUser{ID: "42", Username: "kyle"},
				Command: "hearsay", Subcommand: "merge", Options: map[string]string{"from": "topic:a", "into": "lo"}, Focused: "into"},
		},
		{
			name: "a user outside a guild",
			body: `{"id":"900000000000000003","type":2,"channel_id":"9","user":{"id":"42","username":"kyle"},"data":{"name":"hearsay"}}`,
			want: discord.Interaction{ID: "900000000000000003", Type: discord.InteractionCommand, Channel: discord.InteractionChannel{ID: "9"},
				User: discord.InteractionUser{ID: "42", Username: "kyle"}, Command: "hearsay", Options: map[string]string{}},
		},
		{
			name: "ping",
			body: `{"type":1}`,
			want: discord.Interaction{Type: discord.InteractionPing, Options: map[string]string{}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := discord.ParseInteraction([]byte(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(tt.want)
			if string(gotJSON) != string(wantJSON) || got.User.Nick != tt.want.User.Nick {
				t.Errorf("ParseInteraction() = %+v, want %+v", got, tt.want)
			}
		})
	}
	if _, err := discord.ParseInteraction([]byte(`{"type":2}`)); err == nil {
		t.Error("ParseInteraction accepted a command with no id")
	}
}

// A command is a valid L0 `command` event, under the channel a thread
// belongs to, readable through the channel it was run in.
func TestCommandEvent(t *testing.T) {
	pub, _ := testKey(t)
	app, err := discord.NewApp(appSource(map[string]any{"guild": appGuild, "application_id": testApp, "public_key": hex.EncodeToString(pub)}))
	if err != nil {
		t.Fatal(err)
	}
	in := discord.Interaction{ID: "900000000000000001", Type: discord.InteractionCommand, Guild: appGuild,
		Channel: discord.InteractionChannel{ID: "700", Type: 11, ParentID: "824100000000000001"},
		User:    discord.InteractionUser{ID: "42", Username: "kyle"}, Command: "hearsay", Subcommand: "merge",
		Options: map[string]string{"from": "topic:a", "into": "topic:b"}}
	ev := app.CommandEvent(in)
	if err := ev.Validate(); err != nil {
		t.Fatal(err)
	}
	if ev.Kind != connector.KindCommand || ev.Source != "team-chat" || ev.NativeID != "interaction:"+in.ID {
		t.Errorf("event %s %s %s", ev.Kind, ev.Source, ev.NativeID)
	}
	if ev.Payload.Container.NativeID != "824100000000000001" || ev.Payload.Thread != "" || ev.Payload.Parent != "" {
		t.Errorf("container %q, thread %q, parent %q: want the parent channel and no conversation", ev.Payload.Container.NativeID, ev.Payload.Thread, ev.Payload.Parent)
	}
	if ev.Payload.Text != "/hearsay merge from:topic:a into:topic:b" || ev.Payload.Author.NativeID != "42" {
		t.Errorf("text %q by %+v", ev.Payload.Text, ev.Payload.Author)
	}
	if len(ev.ACL) != 1 || ev.ACL[0].Kind != connector.ACLGroup || ev.ACL[0].NativeID != "700" {
		t.Errorf("ACL %+v, want the channel it was run in", ev.ACL)
	}
}

func TestResponses(t *testing.T) {
	encode := func(r discord.Response) string {
		b, _ := json.Marshal(r)
		return string(b)
	}
	tests := []struct {
		name string
		got  discord.Response
		want string
	}{
		{"pong", discord.Pong(), `{"type":1}`},
		{"answer", discord.Answer("Pinned."), `{"type":4,"data":{"content":"Pinned.","flags":64,"allowed_mentions":{"parse":[]}}}`},
		{"defer", discord.Defer(), `{"type":5,"data":{"flags":64}}`},
		{"no choices", discord.Choices(nil), `{"type":8,"data":{"choices":[]}}`},
		{"a value too long to cut", discord.Choices([]discord.Choice{{Name: "a", Value: strings.Repeat("x", 101)}, {Name: "b", Value: "topic:b"}}),
			`{"type":8,"data":{"choices":[{"name":"b","value":"topic:b"}]}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := encode(tt.got); got != tt.want {
				t.Errorf("got %s, want %s", got, tt.want)
			}
		})
	}
	many := make([]discord.Choice, 30)
	for i := range many {
		many[i] = discord.Choice{Name: strings.Repeat("n", 120), Value: "topic"}
	}
	var got struct {
		Data struct{ Choices []discord.Choice } `json:"data"`
	}
	_ = json.Unmarshal([]byte(encode(discord.Choices(many))), &got)
	if len(got.Data.Choices) != discord.MaxChoices || len([]rune(got.Data.Choices[0].Name)) != 100 {
		t.Errorf("%d choices, the first named %d characters: want %d, cut to 100", len(got.Data.Choices), len([]rune(got.Data.Choices[0].Name)), discord.MaxChoices)
	}
}
