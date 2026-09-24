//go:build integration

package api_test

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/discord"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/queue"
	"github.com/kpenfound/hearsay/internal/service/api"
)

const (
	discordGuild   = "824100000000000000"
	discordApp     = "824100000000000009"
	discordChannel = "824100000000000001"
	otherChannel   = "824100000000000002"
)

// discordWorld is a Discord source whose application Hearsay answers, over
// its own scope: a distilled thread to pin, topics to merge (one only kyle
// may read, one under another scope key), and a fake of Discord's REST API
// that records the edits of deferred answers.
//
// kyle, sam and viv are people; kyle and sam may ratify by hand and viv may
// not. shed is an agent. Discord user 99 maps to nobody.
type discordWorld struct {
	src, scope          string
	thread, bare        string
	lock, holder, vault string
	elsewhere           string
	pool                *pgxpool.Pool
	server              *httptest.Server
	interactions        *api.Interactions
	key                 ed25519.PrivateKey
	edits               chan string
	seq                 int
}

var discordUsers = map[string]string{"kyle": "11", "sam": "12", "viv": "13", "shed": "14", "nobody": "99"}

func newDiscordWorld(t *testing.T) *discordWorld {
	t.Helper()
	pool := newPool(t)
	ctx := t.Context()
	w := &discordWorld{src: newSource(), edits: make(chan string, 4)}
	w.scope = "d" + w.src
	w.thread, w.bare = strconv.FormatInt(time.Now().UnixNano(), 10), strconv.FormatInt(time.Now().UnixNano()+1, 10)
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	w.key = priv

	rest := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/messages/@original") {
			var edit struct{ Content string }
			_ = json.Unmarshal(body, &edit)
			w.edits <- edit.Content
		}
	}))
	t.Cleanup(rest.Close)

	root := t.TempDir()
	files := map[string]string{
		"sources/discord.yaml": fmt.Sprintf("id: %s\ntype: discord\ncontainers: [\"%s\"]\nsettings:\n  guild: \"%s\"\n  application_id: \"%s\"\n  public_key: \"%s\"\n  api_url: %s\nsecrets:\n  token: HEARSAY_TEST_DISCORD_TOKEN\n",
			w.src, discordChannel, discordGuild, discordApp, hex.EncodeToString(pub), rest.URL),
		"scopes/s.yaml": fmt.Sprintf("id: %s\nsources: [%s]\n", w.scope, w.src),
		"principals/p.yaml": fmt.Sprintf("- id: kyle\n  identities: [{source: %[1]s, native_id: \"11\"}]\n"+
			"- id: sam\n  identities: [{source: %[1]s, native_id: \"12\"}]\n"+
			"- id: viv\n  identities: [{source: %[1]s, native_id: \"13\"}]\n"+
			"- id: shed\n  kind: agent\n  class: worker\n  scopes: [code:%[1]s]\n  identities: [{source: %[1]s, native_id: \"14\"}]\n", w.src),
		"authority/a.yaml": "scope: \"*\"\nratified_by:\n  principals: [kyle, sam]\n",
	}
	for name, body := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	repo, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	src, _ := repo.Source(w.src)
	src.Secrets = map[string]string{discord.SecretToken: "test-bot-token"}
	app, err := discord.NewApp(src)
	if err != nil || app == nil {
		t.Fatalf("NewApp() = %v, %v", app, err)
	}

	public := connector.ACL{{Kind: connector.ACLPublic}}
	kyleOnly := connector.ACL{{Kind: connector.ACLIdentity, Source: w.src, NativeID: "11"}}
	when := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	events, docs, graph := l0.New(pool), l1.New(pool), l2.New(pool)
	entity := "code:" + w.src
	if err := graph.PutEntity(ctx, l2.Entity{ID: entity, Type: l2.TypeProject, Name: w.src, Origin: l2.OriginConfig}); err != nil {
		t.Fatal(err)
	}
	// The thread, as the connector and the distiller leave it.
	artifact := discord.ThreadArtifact(w.thread)
	ev := connector.Event{Source: w.src, NativeID: artifact, Kind: connector.KindThread, Time: when, ACL: public,
		Payload: connector.Payload{Artifact: artifact, Title: "where the lock lives",
			Container: connector.Container{Kind: connector.ContainerChannel, NativeID: discordChannel},
			Author:    &connector.Identity{Source: w.src, Kind: connector.IdentityUser, NativeID: "11"}}}
	if _, err := events.Append(ctx, ev); err != nil {
		t.Fatal(err)
	}
	put := func(id string, kind l1.Kind, class config.ArtifactClass, native, l0ref string, acl connector.ACL) string {
		t.Helper()
		doc := l1.Document{ID: id, Kind: kind, ArtifactClass: class, Source: l1.Source{System: w.src, NativeID: native},
			L0Refs: []string{l0ref}, Time: l1.Times{Created: when, Updated: when, LastActivity: when}, Scope: []string{entity},
			ACL: acl, Text: native, RawText: native, Body: l1.Body{Summary: native, OutcomeKind: l1.OutcomeDecided}}
		if _, err := docs.Put(ctx, doc); err != nil {
			t.Fatal(err)
		}
		return id
	}
	put(l1.DocID(w.src, artifact), l1.KindChatThread, config.ArtifactChatThread, artifact, connector.EventID(w.src, artifact), public)

	open := func(scope, name string, acl connector.ACL) string {
		t.Helper()
		native := strings.ReplaceAll(name, " ", "-")
		doc := put(l1.DocID(w.src, native), l1.KindIssue, config.ArtifactIssue, native, connector.EventID(w.src, native), acl)
		topic := l2.Topic{ID: l2.TopicID(scope, doc, 0, name), Scope: scope, Name: name, ACL: acl, OpenedBy: doc}
		if _, err := graph.OpenTopic(ctx, topic); err != nil {
			t.Fatal(err)
		}
		st := l2.Stance{ID: l2.StanceID(topic.ID, doc, name, when, l2.TierInferred), TopicID: topic.ID, Position: name,
			StatedAt: when, Evidence: []string{doc}, Tier: l2.TierInferred, ACL: acl}
		if _, _, err := graph.AppendStance(ctx, st, when); err != nil {
			t.Fatal(err)
		}
		return topic.ID
	}
	w.lock = open(w.scope, "the lock", public)
	w.holder = open(w.scope, "who holds the lock", public)
	w.vault = open(w.scope, "the vault key", kyleOnly)
	w.elsewhere = open("source:"+w.src, "the lock elsewhere", public)

	w.interactions, err = api.NewInteractions(pool, repo, app)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.interactions.Stop)
	mux := http.NewServeMux()
	mux.Handle("POST "+api.DiscordPath("{source}"), w.interactions)
	w.server = httptest.NewServer(mux)
	t.Cleanup(w.server.Close)
	w.pool = pool
	return w
}

// interaction is a request as Discord would send it, from one of
// discordUsers, in a channel of the given type.
type interaction struct {
	kind       discord.InteractionType
	user       string
	channel    string
	threadType bool
	sub        string
	options    []map[string]any
}

func (w *discordWorld) body(in interaction) (string, []byte) {
	w.seq++
	id := strconv.FormatUint(uint64(time.Now().UnixMilli()-1420070400000)<<22+uint64(w.seq), 10)
	channel := map[string]any{"id": in.channel, "type": 0}
	if in.threadType {
		channel = map[string]any{"id": in.channel, "type": 11, "parent_id": discordChannel}
		if in.channel == w.bare+"x" {
			channel["parent_id"] = otherChannel
		}
	}
	data := map[string]any{"name": discord.CommandName}
	if in.sub != "" {
		data["options"] = []any{map[string]any{"name": in.sub, "type": 1, "options": in.options}}
	}
	body, _ := json.Marshal(map[string]any{
		"id": id, "application_id": discordApp, "type": in.kind, "token": "tok-" + id, "guild_id": discordGuild,
		"channel_id": in.channel, "channel": channel,
		"member": map[string]any{"user": map[string]any{"id": discordUsers[in.user], "username": in.user}},
		"data":   data,
	})
	return id, body
}

// send posts a signed body and returns the status, the response and how long
// it took.
func (w *discordWorld) send(t *testing.T, body []byte, key ed25519.PrivateKey) (int, discord.Response, time.Duration) {
	t.Helper()
	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, w.server.URL+api.DiscordPath(w.src), bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Signature-Timestamp", stamp)
	req.Header.Set("X-Signature-Ed25519", hex.EncodeToString(ed25519.Sign(key, append([]byte(stamp), body...))))
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	took := time.Since(start)
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Type int             `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("response %s: %v", raw, err)
		}
	}
	return resp.StatusCode, discord.Response{Type: out.Type, Data: out.Data}, took
}

// answer sends a command and returns its ephemeral answer, failing on
// anything but one given within Discord's deadline.
func (w *discordWorld) answer(t *testing.T, in interaction) (string, string) {
	t.Helper()
	in.kind = discord.InteractionCommand
	id, body := w.body(in)
	status, resp, took := w.send(t, body, w.key)
	if status != http.StatusOK || resp.Type != 4 {
		t.Fatalf("command answered %d, type %d", status, resp.Type)
	}
	if took >= 3*time.Second {
		t.Errorf("command answered after %v, past Discord's three seconds", took)
	}
	var data struct {
		Content string `json:"content"`
		Flags   int    `json:"flags"`
	}
	_ = json.Unmarshal(resp.Data.(json.RawMessage), &data)
	if data.Flags != 64 {
		t.Errorf("answer %q has flags %d, want ephemeral (64)", data.Content, data.Flags)
	}
	return connector.EventID(w.src, "interaction:"+id), data.Content
}

// choices sends an autocomplete for merge and returns the topic ids offered.
func (w *discordWorld) choices(t *testing.T, user string, options ...map[string]any) []string {
	t.Helper()
	_, body := w.body(interaction{kind: discord.InteractionAutocomplete, user: user, channel: discordChannel, sub: discord.CommandMerge, options: options})
	status, resp, took := w.send(t, body, w.key)
	if status != http.StatusOK || resp.Type != 8 {
		t.Fatalf("autocomplete answered %d, type %d", status, resp.Type)
	}
	if took >= 3*time.Second {
		t.Errorf("autocomplete answered after %v", took)
	}
	var data struct{ Choices []discord.Choice }
	_ = json.Unmarshal(resp.Data.(json.RawMessage), &data)
	ids := []string{}
	for _, c := range data.Choices {
		ids = append(ids, c.Value)
	}
	slices.Sort(ids)
	return ids
}

func focused(name, value string) map[string]any {
	return map[string]any{"name": name, "type": 3, "value": value, "focused": true}
}

func option(name, value string) map[string]any {
	return map[string]any{"name": name, "type": 3, "value": value}
}

func sorted(ids ...string) []string {
	slices.Sort(ids)
	return ids
}

// Only a request Discord signed with the application's key is answered, and
// Discord's endpoint check is.
func TestDiscordInteractionsAreVerified(t *testing.T) {
	w := newDiscordWorld(t)
	_, other, _ := ed25519.GenerateKey(nil)
	_, body := w.body(interaction{kind: discord.InteractionCommand, user: "kyle", channel: w.thread, threadType: true, sub: discord.CommandPin})
	if status, _, _ := w.send(t, body, other); status != http.StatusUnauthorized {
		t.Errorf("a request signed by another key = %d, want 401", status)
	}
	if status, resp, _ := w.send(t, []byte(`{"type":1}`), w.key); status != http.StatusOK || resp.Type != 1 {
		t.Errorf("ping = %d, type %d; want a pong", status, resp.Type)
	}
}

// `/hearsay pin` pins the thread it is run in, as the person the Discord user
// maps to, when the scope lets them; every other case says why nothing was
// pinned and pins nothing.
func TestDiscordPin(t *testing.T) {
	w := newDiscordWorld(t)
	graph := l2.New(w.pool)
	pins := func() int {
		t.Helper()
		got, err := graph.Pins(t.Context(), "code:"+w.src)
		if err != nil {
			t.Fatal(err)
		}
		return len(got)
	}
	gestured := func(event string) bool {
		t.Helper()
		_, err := graph.GestureByEvent(t.Context(), event)
		return err == nil
	}
	tests := []struct {
		name string
		in   interaction
		want string
	}{
		{"outside a thread", interaction{user: "kyle", channel: discordChannel, sub: discord.CommandPin}, "inside a thread"},
		{"in a channel Hearsay does not read", interaction{user: "kyle", channel: w.bare + "x", threadType: true, sub: discord.CommandPin}, "does not read this channel"},
		{"in a thread not distilled yet", interaction{user: "kyle", channel: w.bare, threadType: true, sub: discord.CommandPin}, "has not distilled this thread"},
		{"by a Discord user mapped to nobody", interaction{user: "nobody", channel: w.thread, threadType: true, sub: discord.CommandPin}, "not mapped to a Hearsay principal"},
		{"by an agent's account", interaction{user: "shed", channel: w.thread, threadType: true, sub: discord.CommandPin}, "Only a person"},
		{"by a person the scope does not let ratify", interaction{user: "viv", channel: w.thread, threadType: true, sub: discord.CommandPin}, `"viv" may not ratify by hand in scope "` + w.scope + `"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event, got := w.answer(t, tt.in)
			if !strings.Contains(got, tt.want) {
				t.Errorf("answer %q, want it to say %q", got, tt.want)
			}
			if gestured(event) || pins() != 0 {
				t.Errorf("a refused pin recorded a gesture or a pin")
			}
		})
	}

	event, got := w.answer(t, interaction{user: "kyle", channel: w.thread, threadType: true, sub: discord.CommandPin})
	g, err := graph.GestureByEvent(t.Context(), event)
	if err != nil {
		t.Fatalf("kyle's pin (%q) recorded no gesture: %v", got, err)
	}
	want := fmt.Sprintf("Pinned this thread in scope %s, as gesture %d.", w.scope, g.ID)
	if !strings.HasPrefix(got, want) {
		t.Errorf("answer %q, want %q", got, want)
	}
	if g.Action != l2.GesturePin || g.Principal != "kyle" || !slices.Equal(g.Documents, []string{l1.DocID(w.src, discord.ThreadArtifact(w.thread))}) {
		t.Errorf("gesture %+v, want kyle pinning the thread", g)
	}
	if pins() != 1 {
		t.Errorf("%d pins after kyle's pin, want 1", pins())
	}
	// The command is in L0, as control traffic.
	ev, err := l0.New(w.pool).Get(t.Context(), event)
	if err != nil || ev.Kind != connector.KindCommand {
		t.Errorf("the command's event = %+v, %v; want a command", ev, err)
	}
}

// `/hearsay merge` offers, and accepts, only topics the person may read, and
// merges them as that person when the scope lets them.
func TestDiscordMerge(t *testing.T) {
	w := newDiscordWorld(t)
	graph := l2.New(w.pool)
	ops := func() int {
		t.Helper()
		got, err := graph.Operations(t.Context(), l2.OperationFilter{Scope: w.scope})
		if err != nil {
			t.Fatal(err)
		}
		return len(got)
	}

	t.Run("autocomplete", func(t *testing.T) {
		tests := []struct {
			name    string
			user    string
			options []map[string]any
			want    []string
		}{
			{"sam sees the topics sam may read", "sam", []map[string]any{focused("from", "")}, sorted(w.lock, w.holder, w.elsewhere)},
			{"kyle also sees the one only kyle may read", "kyle", []map[string]any{focused("from", "")}, sorted(w.lock, w.holder, w.vault, w.elsewhere)},
			{"what is typed narrows the list", "sam", []map[string]any{focused("from", "HOLDS")}, []string{w.holder}},
			{"into stays in from's scope and leaves from out", "sam", []map[string]any{option("from", w.lock), focused("into", "lock")}, []string{w.holder}},
			{"into after an unreadable from is every readable topic", "sam", []map[string]any{option("from", w.vault), focused("into", "")}, sorted(w.lock, w.holder, w.elsewhere)},
			{"a Discord user mapped to nobody is offered nothing", "nobody", []map[string]any{focused("from", "")}, []string{}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				if got := w.choices(t, tt.user, tt.options...); !slices.Equal(got, tt.want) {
					t.Errorf("choices %v, want %v", got, tt.want)
				}
			})
		}
	})

	merge := func(from, into string) []map[string]any {
		return []map[string]any{option("from", from), option("into", into)}
	}
	tests := []struct {
		name    string
		user    string
		options []map[string]any
		want    string
	}{
		{"a topic sam may not read", "sam", merge(w.vault, w.lock), fmt.Sprintf("no topic %q that you can read", w.vault)},
		{"a topic that does not exist", "sam", merge("topic:nothing", w.lock), `no topic "topic:nothing"`},
		{"a topic into itself", "sam", merge(w.lock, w.lock), "cannot be merged into itself"},
		{"across scopes", "sam", merge(w.elsewhere, w.lock), "could not merge these topics"},
		{"by a person the scope does not let ratify", "viv", merge(w.holder, w.lock), `"viv" may not ratify by hand`},
		{"by a Discord user mapped to nobody", "nobody", merge(w.holder, w.lock), "not mapped to a Hearsay principal"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, got := w.answer(t, interaction{user: tt.user, channel: discordChannel, sub: discord.CommandMerge, options: tt.options})
			if !strings.Contains(got, tt.want) {
				t.Errorf("answer %q, want it to say %q", got, tt.want)
			}
			if ops() != 0 {
				t.Error("a refused merge recorded an operation")
			}
		})
	}

	_, got := w.answer(t, interaction{user: "sam", channel: discordChannel, sub: discord.CommandMerge, options: merge(w.holder, w.lock)})
	recorded, err := graph.Operations(t.Context(), l2.OperationFilter{Scope: w.scope})
	if err != nil || len(recorded) != 1 {
		t.Fatalf("sam's merge (%q) recorded %v, %v", got, recorded, err)
	}
	op := recorded[0]
	if op.Kind != l2.OperationMerge || op.Principal != "sam" || !slices.Contains(op.Topics, w.holder) || !slices.Contains(op.Topics, w.lock) {
		t.Errorf("operation %+v, want sam merging the holder into the lock", op)
	}
	want := fmt.Sprintf(`Merged "who holds the lock" into "the lock" in scope %s, as operation %d.`, w.scope, op.ID)
	if !strings.HasPrefix(got, want) {
		t.Errorf("answer %q, want %q", got, want)
	}
	if got := w.choices(t, "sam", focused("from", "")); !slices.Equal(got, sorted(w.lock, w.elsewhere)) {
		t.Errorf("after the merge sam is offered %v, want the lock and the other scope's topic", got)
	}
}

// A command whose work outlasts the deadline is answered at once with an
// ephemeral deferral, and its answer is edited in once the work is done.
func TestDiscordCommandIsDeferredPastTheDeadline(t *testing.T) {
	w := newDiscordWorld(t)
	w.interactions.AnswerWithin(200 * time.Millisecond)
	// Hold the scope's key, as an assert job running on it would.
	client, err := queue.New(w.pool, queue.Config{Kind: l2.AssertKind()})
	if err != nil {
		t.Fatal(err)
	}
	job, err := client.Hold(t.Context(), w.scope, "test-hold:"+w.src)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			if _, err := client.Complete(t.Context(), job); err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(release)

	_, body := w.body(interaction{kind: discord.InteractionCommand, user: "kyle", channel: w.thread, threadType: true, sub: discord.CommandPin})
	status, resp, took := w.send(t, body, w.key)
	if status != http.StatusOK || resp.Type != 5 {
		t.Fatalf("a held command answered %d, type %d; want a deferral", status, resp.Type)
	}
	var data struct{ Flags int }
	_ = json.Unmarshal(resp.Data.(json.RawMessage), &data)
	if data.Flags != 64 || took >= 3*time.Second {
		t.Errorf("deferral flags %d after %v, want ephemeral within three seconds", data.Flags, took)
	}
	select {
	case got := <-w.edits:
		t.Fatalf("the answer %q was edited in while the scope was held", got)
	case <-time.After(300 * time.Millisecond):
	}
	release()
	select {
	case got := <-w.edits:
		if !strings.HasPrefix(got, "Pinned this thread in scope "+w.scope) {
			t.Errorf("edited answer %q, want the pin", got)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the deferred answer was never edited in")
	}
}
