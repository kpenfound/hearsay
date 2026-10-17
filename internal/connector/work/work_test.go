package work_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/work"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/principal"
)

const sourceID = "projects"

func source() connector.SourceConfig {
	return connector.SourceConfig{ID: sourceID, Type: work.Type, Containers: []string{"*"}, Settings: json.RawMessage(`{"attributors":["factory"]}`)}
}
func principals() []principal.Principal {
	return []principal.Principal{
		{ID: "factory", Kind: principal.KindAgent, Class: principal.ClassObserver, TokenEnv: "FACTORY_TOKEN"},
		{ID: "shed", Kind: principal.KindAgent, Class: principal.ClassWorker, TokenEnv: "SHED_TOKEN"},
		{ID: "kyle", Kind: principal.KindHuman},
		{ID: "team", Kind: principal.KindTeam},
	}
}
func lookup(env string) (string, bool) {
	switch env {
	case "FACTORY_TOKEN":
		return "factory-token", true
	case "SHED_TOKEN":
		return "shed-token", true
	}
	return "", false
}
func build(t *testing.T, src connector.SourceConfig) *work.Connector {
	t.Helper()
	c, err := work.New(src, principals(), []string{sourceID, "github", "github-other"}, lookup)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// memoryStore models L0's deduplication and edited_at/ingest ordering, unlike a
// recorder that simply treats the last emission as current. Every call checks
// cancellation. Hooks interrupt an emission without needing a database.
type memoryStore struct {
	mu             sync.Mutex
	events         []connector.Event
	before         func(context.Context, connector.Event) error
	readErr        error
	listErr        error
	afterInventory func([]connector.Event)
}

func (s *memoryStore) Emit(ctx context.Context, ev connector.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.before != nil {
		if err := s.before(ctx, ev); err != nil {
			return err
		}
	}
	if err := ev.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, old := range s.events {
		if old.ID == ev.ID {
			if !reflect.DeepEqual(old, ev) {
				return l0.ErrRewrite
			}
			return nil
		}
	}
	s.events = append(s.events, ev)
	return nil
}
func (s *memoryStore) current(source string) []connector.Event {
	current := map[string]connector.Event{}
	tombstones := map[string]int{}
	for i, ev := range s.events {
		if ev.Source == source && ev.Kind == connector.KindTombstone {
			tombstones[ev.Payload.Target] = i + 1
		}
	}
	for i, ev := range s.events {
		if ev.Source != source || ev.Kind == connector.KindTombstone || i+1 <= tombstones[ev.Payload.Artifact] {
			continue
		}
		old, ok := current[ev.Payload.Artifact]
		if !ok || !ev.Payload.Revision.EditedAt.Before(old.Payload.Revision.EditedAt) {
			current[ev.Payload.Artifact] = ev
		}
	}
	out := make([]connector.Event, 0, len(current))
	for _, ev := range current {
		out = append(out, ev)
	}
	slices.SortFunc(out, func(a, b connector.Event) int { return strings.Compare(a.Payload.Artifact, b.Payload.Artifact) })
	return out
}
func (s *memoryStore) CurrentArtifact(ctx context.Context, source, artifact string) (connector.Event, bool, error) {
	if err := ctx.Err(); err != nil {
		return connector.Event{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readErr != nil {
		return connector.Event{}, false, s.readErr
	}
	for _, ev := range s.current(source) {
		if ev.Payload.Artifact == artifact {
			return ev, true, nil
		}
	}
	return connector.Event{}, false, nil
}
func (s *memoryStore) CurrentArtifacts(ctx context.Context, source string) ([]connector.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	out := s.current(source)
	if s.afterInventory != nil {
		s.afterInventory(out)
	}
	return out, nil
}
func (s *memoryStore) history() []connector.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.events)
}
func handler(c *work.Connector, s connector.Sink) http.Handler {
	return c.Handler(connector.NewGate(s, sourceID, c.Describe(), connector.NewAllowlist(source())))
}
func request(ctx context.Context, h http.Handler, method, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequestWithContext(ctx, method, "/hooks/projects", strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func send(t *testing.T, h http.Handler, body string, want int) string {
	t.Helper()
	w := request(t.Context(), h, http.MethodPost, "factory-token", body)
	if w.Code != want {
		t.Fatalf("POST = %d %s, want %d", w.Code, w.Body.String(), want)
	}
	return w.Body.String()
}
func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func with(t *testing.T, body string, fields map[string]any) string {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal([]byte(body), &value); err != nil {
		t.Fatal(err)
	}
	for k, v := range fields {
		if v == nil {
			delete(value, k)
		} else {
			value[k] = v
		}
	}
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func held(t *testing.T, s *memoryStore, artifact string) connector.Event {
	t.Helper()
	ev, ok, err := s.CurrentArtifact(t.Context(), sourceID, artifact)
	if err != nil || !ok {
		t.Fatalf("CurrentArtifact(%s) = %v %v", artifact, ok, err)
	}
	return ev
}

func TestStartup(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*connector.SourceConfig, *[]principal.Principal, map[string]string)
		want   string
	}{
		{"missing containers", func(s *connector.SourceConfig, _ *[]principal.Principal, _ map[string]string) { s.Containers = nil }, "exactly"},
		{"named containers", func(s *connector.SourceConfig, _ *[]principal.Principal, _ map[string]string) {
			s.Containers = []string{"p"}
		}, "exactly"},
		{"wildcard and another", func(s *connector.SourceConfig, _ *[]principal.Principal, _ map[string]string) {
			s.Containers = []string{"*", "p"}
		}, "exactly"},
		{"secrets", func(s *connector.SourceConfig, _ *[]principal.Principal, _ map[string]string) {
			s.Secrets = map[string]string{"token": "secret"}
		}, "no secrets"},
		{"unknown settings", func(s *connector.SourceConfig, _ *[]principal.Principal, _ map[string]string) {
			s.Settings = []byte(`{"poster":["factory"]}`)
		}, "unknown field"},
		{"wrong settings type", func(s *connector.SourceConfig, _ *[]principal.Principal, _ map[string]string) {
			s.Settings = []byte(`{"posters":true}`)
		}, "unmarshal"},
		{"unknown poster", func(s *connector.SourceConfig, _ *[]principal.Principal, _ map[string]string) {
			s.Settings = []byte(`{"posters":["missing"]}`)
		}, "agent principal"},
		{"human poster", func(s *connector.SourceConfig, _ *[]principal.Principal, _ map[string]string) {
			s.Settings = []byte(`{"posters":["kyle"]}`)
		}, "agent principal"},
		{"team poster", func(s *connector.SourceConfig, _ *[]principal.Principal, _ map[string]string) {
			s.Settings = []byte(`{"posters":["team"]}`)
		}, "agent principal"},
		{"poster without token_env", func(s *connector.SourceConfig, p *[]principal.Principal, _ map[string]string) {
			s.Settings = []byte(`{"posters":["factory"]}`)
			(*p)[0].TokenEnv = ""
		}, "token_env"},
		{"unknown attributor", func(s *connector.SourceConfig, _ *[]principal.Principal, _ map[string]string) {
			s.Settings = []byte(`{"attributors":["missing"]}`)
		}, "agent principal"},
		{"human attributor", func(s *connector.SourceConfig, _ *[]principal.Principal, _ map[string]string) {
			s.Settings = []byte(`{"attributors":["kyle"]}`)
		}, "agent principal"},
		{"team attributor", func(s *connector.SourceConfig, _ *[]principal.Principal, _ map[string]string) {
			s.Settings = []byte(`{"attributors":["team"]}`)
		}, "agent principal"},
		{"attributor without token_env", func(s *connector.SourceConfig, p *[]principal.Principal, _ map[string]string) { (*p)[0].TokenEnv = "" }, "token_env"},
		{"attributor excluded", func(s *connector.SourceConfig, _ *[]principal.Principal, _ map[string]string) {
			s.Settings = []byte(`{"posters":["shed"],"attributors":["factory"]}`)
		}, "not allowed"},
		{"no posters", func(s *connector.SourceConfig, _ *[]principal.Principal, _ map[string]string) {
			s.Settings = []byte(`{"posters":[]}`)
		}, "no poster"},
		{"no token-bearing agents", func(s *connector.SourceConfig, p *[]principal.Principal, _ map[string]string) {
			s.Settings = nil
			(*p)[0].TokenEnv = ""
			(*p)[1].TokenEnv = ""
		}, "no poster"},
		{"unset token", func(_ *connector.SourceConfig, _ *[]principal.Principal, e map[string]string) {
			delete(e, "FACTORY_TOKEN")
		}, "not set or is empty"},
		{"empty token", func(_ *connector.SourceConfig, _ *[]principal.Principal, e map[string]string) {
			e["FACTORY_TOKEN"] = ""
		}, "not set or is empty"},
		{"another allowed poster unset", func(_ *connector.SourceConfig, _ *[]principal.Principal, e map[string]string) {
			delete(e, "SHED_TOKEN")
		}, "not set or is empty"},
		{"duplicate token", func(_ *connector.SourceConfig, _ *[]principal.Principal, e map[string]string) {
			e["SHED_TOKEN"] = e["FACTORY_TOKEN"]
		}, "share an API token"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src, p, env := source(), principals(), map[string]string{"FACTORY_TOKEN": "one", "SHED_TOKEN": "two"}
			tt.mutate(&src, &p, env)
			_, err := work.New(src, p, []string{"github"}, func(k string) (string, bool) { v, ok := env[k]; return v, ok })
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("New = %v, want %s", err, tt.want)
			}
		})
	}
	for _, class := range []principal.Class{principal.ClassObserver, principal.ClassWorker, principal.ClassOrchestrator, principal.ClassSteward, "unused"} {
		t.Run("class "+string(class), func(t *testing.T) {
			p := principals()
			p[0].Class = class
			c, err := work.New(source(), p, []string{"github"}, lookup)
			if err != nil {
				t.Fatal(err)
			}
			send(t, handler(c, &memoryStore{}), fixture(t, "project"), 202)
		})
	}
	t.Run("defaults", func(t *testing.T) {
		src := source()
		src.Settings = nil
		c := build(t, src)
		s := &memoryStore{}
		h := handler(c, s)
		body := with(t, fixture(t, "project"), map[string]any{"author": nil})
		send(t, h, body, 202)
		w := request(t.Context(), h, http.MethodPost, "shed-token", with(t, body, map[string]any{"project": "second"}))
		if w.Code != 202 {
			t.Fatal(w.Code, w.Body.String())
		}
		send(t, h, fixture(t, "project"), 403)
	})
	t.Run("excluded unset token is harmless", func(t *testing.T) {
		src := source()
		src.Settings = []byte(`{"posters":["factory"]}`)
		_, err := work.New(src, principals(), []string{"github"}, func(k string) (string, bool) {
			if k == "FACTORY_TOKEN" {
				return "one", true
			}
			return "", false
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}

func TestRequestRules(t *testing.T) {
	project, stream, update := fixture(t, "project"), fixture(t, "workstream"), fixture(t, "update")
	tests := []struct{ name, body, want string }{
		{"unknown kind", with(t, project, map[string]any{"kind": "task"}), "kind must"},
		{"missing project", with(t, project, map[string]any{"project": nil}), "project must"},
		{"missing stream id", with(t, stream, map[string]any{"workstream": nil}), "valid workstream"},
		{"missing update id", with(t, update, map[string]any{"update": nil}), "valid update"},
		{"empty optional stream", with(t, update, map[string]any{"workstream": ""}), "workstream must"},
		{"project with stream id", with(t, project, map[string]any{"workstream": "w"}), "no workstream"},
		{"project with update id", with(t, project, map[string]any{"update": "u"}), "no workstream"},
		{"stream with update id", with(t, stream, map[string]any{"update": "u"}), "no update"},
		{"project missing name", with(t, project, map[string]any{"name": nil}), "name is required"},
		{"stream missing name", with(t, stream, map[string]any{"name": ""}), "name is required"},
		{"update missing text", with(t, update, map[string]any{"text": nil}), "text is required"},
		{"update state", with(t, update, map[string]any{"state": "started"}), "not allowed on an update"},
		{"update status alone", with(t, update, map[string]any{"status": "Building"}), "not allowed on an update"},
		{"update numeric state", with(t, update, map[string]any{"state": 123}), "not allowed on an update"},
		{"update object state", with(t, update, map[string]any{"state": map[string]any{}}), "not allowed on an update"},
		{"update open state", with(t, update, map[string]any{"state": "open"}), "not allowed on an update"},
		{"update boolean status", with(t, update, map[string]any{"status": false}), "not allowed on an update"},
		{"update array status", with(t, update, map[string]any{"status": []any{}}), "not allowed on an update"},
		{"update x status", with(t, update, map[string]any{"status": "x"}), "not allowed on an update"},
		{"update both", with(t, update, map[string]any{"state": "started", "status": "Building"}), "not allowed on an update"},

		{"update null state", with(t, update, map[string]any{"state": json.RawMessage("null")}), "not allowed on an update"},
		{"update null status", with(t, update, map[string]any{"status": json.RawMessage("null")}), "not allowed on an update"},
		{"update case variant state", with(t, update, map[string]any{"STATE": "started"}), "not allowed on an update"},
		{"update empty status", with(t, update, map[string]any{"status": ""}), "not allowed on an update"},
		{"invalid state", with(t, project, map[string]any{"state": "open"}), "core state"},
		{"empty state", with(t, stream, map[string]any{"state": ""}), "core state"},
		{"project status alone", with(t, project, map[string]any{"state": nil}), "status requires state"},
		{"stream status alone", with(t, stream, map[string]any{"state": nil}), "status requires state"},
		{"empty status still needs state", with(t, project, map[string]any{"state": nil, "status": ""}), "status requires state"},
		{"long status", with(t, project, map[string]any{"status": strings.Repeat("s", 65)}), "at most 64 bytes"},
		{"status byte length", with(t, stream, map[string]any{"status": strings.Repeat("é", 33)}), "at most 64 bytes"},
		{"project text", with(t, project, map[string]any{"text": ""}), "text belongs only"},
		{"stream text", with(t, stream, map[string]any{"text": "a"}), "text belongs only"},
		{"update name", with(t, update, map[string]any{"name": ""}), "not allowed"},
		{"update description", with(t, update, map[string]any{"description": "a"}), "not allowed"},
		{"project touches", with(t, project, map[string]any{"touches": []any{}}), "only on a workstream"},
		{"stream repos", with(t, stream, map[string]any{"repos": []any{}}), "only on a project"},
		{"update repos", with(t, update, map[string]any{"repos": []any{}}), "only on a project"},
		{"update touches", with(t, update, map[string]any{"touches": []any{}}), "only on a workstream"},
		{"unknown repo source", with(t, project, map[string]any{"repos": []work.Repo{{Source: "absent", Project: "acme/api"}}}), "configured source"},
		{"empty repo source", with(t, project, map[string]any{"repos": []work.Repo{{Project: "acme/api"}}}), "configured source"},
		{"empty repo project", with(t, project, map[string]any{"repos": []work.Repo{{Source: "github"}}}), "a project"},
		{"unknown touch source", with(t, stream, map[string]any{"touches": []connector.Touch{{Source: "absent", Project: "acme/api"}}}), "configured source"},

		{"empty touch project", with(t, stream, map[string]any{"touches": []connector.Touch{{Source: "github"}}}), "a project"},
		{"no created_at", with(t, project, map[string]any{"created_at": nil}), "created_at is required"},
		{"zero created_at", with(t, update, map[string]any{"created_at": "0001-01-01T00:00:00Z"}), "created_at is required"},
		{"created_at format", with(t, project, map[string]any{"created_at": "yesterday"}), "parsing time"},
		{"updated_at format", with(t, stream, map[string]any{"updated_at": "yesterday"}), "parsing time"},
		{"updated_at before created_at", with(t, project, map[string]any{"updated_at": "2026-01-01T00:00:00Z"}), "not earlier"},
		{"zero updated_at", with(t, project, map[string]any{"updated_at": "0001-01-01T00:00:00Z"}), "not earlier"},
		{"unknown field", with(t, project, map[string]any{"typo": true}), "unknown field"},
		{"nested unknown repo field", with(t, project, map[string]any{"repos": []any{map[string]any{"source": "github", "project": "acme/api", "paths": []string{"a"}}}}), "unknown field"},
		{"nested unknown touch field", with(t, stream, map[string]any{"touches": []any{map[string]any{"source": "github", "project": "acme/api", "typo": true}}}), "unknown field"},
		{"wrong field type", with(t, project, map[string]any{"name": 42}), "unmarshal"},
		{"null field", with(t, project, map[string]any{"name": json.RawMessage("null")}), "must not be null"},
		{"trailing object", project + project, "one event"},
		{"trailing malformed", project + "{", "one event"},
		{"array", "[]", "not an event"},
		{"null", "null", "one event"},
		{"scalar", "true", "not an event"},
		{"malformed", "{", "not an event"},
		{"empty body", "", "not an event"},
	}
	for _, field := range []string{"project", "workstream", "update"} {
		base := project
		if field == "workstream" {
			base = stream
		}
		if field == "update" {
			base = update
		}
		for _, id := range []string{"", strings.Repeat("x", 129), "with space", "p#x", "p:x", "p@x", "é", "p\\x"} {
			tests = append(tests, struct{ name, body, want string }{field + " invalid " + id, with(t, base, map[string]any{field: id}), ""})
		}
	}
	repos := make([]work.Repo, 17)
	touches := make([]connector.Touch, 17)
	for i := range repos {
		repos[i] = work.Repo{Source: "github", Project: fmt.Sprint(i)}
		touches[i] = connector.Touch{Source: "github", Project: fmt.Sprint(i)}
	}
	tests = append(tests, struct{ name, body, want string }{"17 repos", with(t, project, map[string]any{"repos": repos}), "at most 16"}, struct{ name, body, want string }{"17 touches", with(t, stream, map[string]any{"touches": touches}), "at most 16"})
	paths := make([]string, 301)
	for i := range paths {
		paths[i] = fmt.Sprint(i)
	}
	tests = append(tests, struct{ name, body, want string }{"301 paths total", with(t, stream, map[string]any{"touches": []connector.Touch{{Source: "github", Project: "acme/api", Paths: paths[:150]}, {Source: "github", Project: "acme/api", Paths: paths[150:]}}}), "at most 300"})
	// Deletions have a deliberately stricter shape than the tracker. Presence,
	// rather than truthiness, is what prevents empty extra fields slipping in.
	for _, field := range []string{"name", "description", "text", "state", "status", "repos", "touches", "url", "author", "created_at", "updated_at"} {
		var value any = ""
		switch field {
		case "state":
			value = "started"
		case "repos", "touches":
			value = []any{}
		case "created_at", "updated_at":
			value = "2026-10-14T17:00:00Z"
		}
		tests = append(tests, struct{ name, body, want string }{"deletion with " + field, with(t, fixture(t, "delete"), map[string]any{field: value}), "only ids"})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := build(t, source())
			s := &memoryStore{}
			h := handler(c, s)
			send(t, h, project, 202)
			send(t, h, stream, 202)
			n := len(s.history())
			out := send(t, h, tt.body, 400)
			if !strings.Contains(out, tt.want) {
				t.Errorf("body = %s, want %s", out, tt.want)
			}
			if len(s.history()) != n {
				t.Fatal("invalid request emitted an event")
			}
		})
	}
}

func TestResponses(t *testing.T) {
	tests := []struct {
		name, method, token, body string
		setup                     func(*memoryStore)
		status                    int
		want                      string
	}{
		{name: "accepted", token: "factory-token", status: 202},
		{name: "missing token", status: 401},
		{name: "unknown token", token: "unknown", status: 401},
		{name: "poster excluded", token: "shed-token", status: 403, want: "not allowed by posters"},
		{name: "non-attributor", token: "shed-token", status: 403, want: "not allowed to attribute"},
		{name: "unknown author", token: "factory-token", body: with(t, fixture(t, "project"), map[string]any{"author": "unknown"}), status: 403, want: "configured human or agent"},
		{name: "team author", token: "factory-token", body: with(t, fixture(t, "project"), map[string]any{"author": "team"}), status: 403, want: "configured human or agent"},
		{name: "empty author", token: "factory-token", body: with(t, fixture(t, "project"), map[string]any{"author": ""}), status: 403, want: "configured human or agent"},
		{name: "get", method: "GET", token: "factory-token", status: 405},
		{name: "put", method: "PUT", status: 405},
		{name: "oversized", token: "factory-token", body: strings.Repeat("x", (4<<20)+1), status: 413},
		{name: "invalid", token: "factory-token", body: "{}", status: 400},
		{name: "rewrite", token: "factory-token", setup: func(s *memoryStore) { s.before = func(context.Context, connector.Event) error { return l0.ErrRewrite } }, status: 409},
		{name: "stream project missing", token: "factory-token", body: fixture(t, "workstream"), status: 409, want: "project is not held"},
		{name: "project update project missing", token: "factory-token", body: with(t, fixture(t, "update"), map[string]any{"workstream": nil}), status: 409, want: "project is not held"},
		{name: "stream update project missing", token: "factory-token", body: fixture(t, "update"), status: 409, want: "project is not held"},
		{name: "update stream missing", token: "factory-token", body: fixture(t, "update"), status: 409, want: "workstream is not held"},
		{name: "missing project deletion", token: "factory-token", body: fixture(t, "delete"), status: 204},
		{name: "missing stream deletion", token: "factory-token", body: `{"kind":"workstream","project":"p","workstream":"w","deleted":true}`, status: 204},
		{name: "missing update deletion", token: "factory-token", body: `{"kind":"update","project":"p","update":"u","deleted":true}`, status: 204},
		{name: "emit failure", token: "factory-token", setup: func(s *memoryStore) {
			s.before = func(context.Context, connector.Event) error { return errors.New("store unavailable") }
		}, status: 500},
		{name: "read failure", token: "factory-token", setup: func(s *memoryStore) { s.readErr = errors.New("store unavailable") }, status: 500},
		{name: "inventory failure", token: "factory-token", setup: func(s *memoryStore) { s.listErr = errors.New("store unavailable") }, status: 500},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := source()
			if tt.name == "poster excluded" {
				src.Settings = []byte(`{"posters":["factory"],"attributors":["factory"]}`)
			}
			c := build(t, src)
			s := &memoryStore{}
			h := handler(c, s)
			if tt.name == "update stream missing" {
				send(t, h, fixture(t, "project"), 202)
			}
			if tt.setup != nil {
				tt.setup(s)
			}
			method := tt.method
			if method == "" {
				method = http.MethodPost
			}
			body := tt.body
			if body == "" {
				body = fixture(t, "project")
			}
			w := request(t.Context(), h, method, tt.token, body)
			if w.Code != tt.status || !strings.Contains(w.Body.String(), tt.want) {
				t.Fatalf("response = %d %s, want %d %s", w.Code, w.Body.String(), tt.status, tt.want)
			}
			if tt.status == 401 && w.Header().Get("WWW-Authenticate") == "" {
				t.Error("missing bearer challenge")
			}
			if tt.status == 204 && w.Body.Len() != 0 {
				t.Error("204 has a body")
			}
		})
	}
}

func TestMappingAndReposts(t *testing.T) {
	c := build(t, source())
	s := &memoryStore{}
	h := handler(c, s)
	created := "2026-10-14T17:00:00Z"
	cases := []struct {
		name, body, artifact                    string
		kind                                    connector.Kind
		title, text, parent, partOf, at, edited string
		author, poster                          string
		touches                                 []connector.Touch
		state                                   connector.CoreState
		status                                  string
	}{
		{name: "project", body: fixture(t, "project"), artifact: "payments-rewrite", kind: connector.KindIssue, title: "Payments rewrite", text: "Move settlement off the legacy queue.", at: created, edited: "2026-10-14T18:00:00Z", author: "kyle", poster: "factory", touches: []connector.Touch{{Source: "github", Project: "acme/api"}}, state: "started", status: "Building"},
		{name: "workstream", body: fixture(t, "workstream"), artifact: "payments-rewrite#retry-policy", kind: connector.KindIssue, title: "Retry policy", text: "Bounded retries for settlement calls.", partOf: "payments-rewrite", at: "2026-10-14T17:05:00Z", edited: "2026-10-14T18:10:00Z", author: "shed", poster: "factory", touches: []connector.Touch{{Source: "github", Project: "acme/api", Paths: []string{"engine/server/retry.go"}}}, state: "blocked", status: "Waiting on review"},
		{name: "update on workstream", body: fixture(t, "update"), artifact: "payments-rewrite#retry-policy:update:u-17", kind: connector.KindMessage, text: "Decided: cap retries at 5 with jittered backoff; the queue handles the rest.", parent: "payments-rewrite#retry-policy", at: "2026-10-14T18:09:00Z", edited: "2026-10-14T18:09:00Z", author: "kyle", poster: "factory"},
		{name: "update on project", body: with(t, fixture(t, "update"), map[string]any{"workstream": nil, "author": nil}), artifact: "payments-rewrite:update:u-17", kind: connector.KindMessage, text: "Decided: cap retries at 5 with jittered backoff; the queue handles the rest.", parent: "payments-rewrite", at: "2026-10-14T18:09:00Z", edited: "2026-10-14T18:09:00Z", author: "factory", poster: "factory"},
	}
	refs, key, err := connector.CanonicalContainers([]connector.ContainerRef{{Source: "github", NativeID: "acme/api"}})
	if err != nil {
		t.Fatal(err)
	}
	wantACL := connector.ACL{{Kind: connector.ACLContainers, NativeID: key, Containers: refs}}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			response := send(t, h, tt.body, 202)
			ev := held(t, s, tt.artifact)
			if ev.Kind != tt.kind || ev.Source != sourceID || ev.Payload.Artifact != tt.artifact || ev.NativeID != tt.artifact+"@"+ev.Payload.Revision.Token || ev.ID != connector.EventID(sourceID, ev.NativeID) {
				t.Errorf("event identity = %+v", ev)
			}
			if len(ev.Payload.Revision.Token) != 16 {
				t.Error("revision token is not 16 hex digits")
			}
			if ev.Payload.Container != (connector.Container{Kind: "project", NativeID: "payments-rewrite"}) || ev.Payload.Title != tt.title || ev.Payload.Text != tt.text || ev.Payload.Parent != tt.parent || ev.Payload.Thread != tt.parent || ev.Payload.PartOf != tt.partOf {
				t.Errorf("payload = %+v", ev.Payload)
			}
			if ev.Time.Format(time.RFC3339) != tt.at || ev.Payload.Revision.EditedAt.Format(time.RFC3339) != tt.edited {
				t.Errorf("times = %v %v", ev.Time, ev.Payload.Revision.EditedAt)
			}
			if !reflect.DeepEqual(ev.Payload.Touches, tt.touches) || !ev.ACL.Equal(wantACL) {
				t.Errorf("touches or acl = %+v %+v", ev.Payload.Touches, ev.ACL)
			}
			if tt.state == "" {
				if ev.Payload.State != nil {
					t.Fatal("update has state")
				}
			} else if ev.Payload.State == nil || ev.Payload.State.Core != tt.state || ev.Payload.State.Native == nil || *ev.Payload.State.Native != tt.status {
				t.Errorf("state = %+v", ev.Payload.State)
			}
			authorKind := connector.IdentityAgent
			if tt.author == "kyle" {
				authorKind = connector.IdentityUser
			}
			if *ev.Payload.Author != (connector.Identity{Source: sourceID, Kind: authorKind, NativeID: tt.author}) {
				t.Errorf("author = %+v", ev.Payload.Author)
			}
			if tt.author != tt.poster {
				if !reflect.DeepEqual(ev.Payload.Participants, []connector.Participant{{Identity: connector.Identity{Source: sourceID, Kind: connector.IdentityAgent, NativeID: tt.poster}, Role: connector.RoleAgent}}) {
					t.Error("attribution did not preserve poster")
				}
			} else if len(ev.Payload.Participants) != 0 {
				t.Error("unattributed event has participants")
			}
			var native work.Native
			if err := json.Unmarshal(ev.Payload.Native, &native); err != nil {
				t.Fatal(err)
			}
			if native.Project != "payments-rewrite" || native.Poster != tt.poster || native.Status != tt.status {
				t.Errorf("native = %+v", native)
			}

			wantStream, wantUpdate := "", ""
			if strings.Contains(tt.artifact, "#") {
				wantStream = "retry-policy"
			}
			if tt.kind == connector.KindMessage {
				wantUpdate = "u-17"
			}
			if native.Workstream != wantStream || native.Update != wantUpdate {
				t.Errorf("native hierarchy ids = %+v", native)
			}
			if tt.name == "project" && ev.Payload.URL != "https://factory.example/p/payments-rewrite" {
				t.Error("url missing")
			}
			n := len(s.history())
			again := send(t, h, tt.body, 202)
			if response != again || len(s.history()) != n {
				t.Fatal("identical repost changed id or emitted events")
			}
		})
	}
	t.Run("equivalent JSON and timezone", func(t *testing.T) {
		original := fixture(t, "project")
		first := send(t, h, original, 202)
		n := len(s.history())
		again := send(t, h, with(t, original, map[string]any{"created_at": "2026-10-14T19:00:00+02:00", "updated_at": "2026-10-14T20:00:00+02:00"}), 202)
		if first != again || len(s.history()) != n {
			t.Fatal("JSON order or timezone changed revision")
		}
	})
	for _, artifact := range []string{"payments-rewrite#retry-policy:update:u-17", "payments-rewrite:update:u-17", "payments-rewrite#retry-policy", "payments-rewrite"} {
		t.Run("deletion "+artifact, func(t *testing.T) {
			old := held(t, s, artifact)
			var deletion map[string]any
			if err := json.Unmarshal(old.Payload.Native, &deletion); err != nil {
				t.Fatal(err)
			}
			delete(deletion, "poster")
			delete(deletion, "status")
			deletion["deleted"] = true
			switch {
			case strings.Contains(artifact, ":update:"):
				deletion["kind"] = "update"
			case strings.Contains(artifact, "#"):
				deletion["kind"] = "workstream"
			default:
				deletion["kind"] = "project"
			}
			raw, _ := json.Marshal(deletion)
			send(t, h, string(raw), 202)
			ev := s.history()[len(s.history())-1]
			if ev.Kind != connector.KindTombstone || ev.Payload.Target != artifact || !ev.Time.Equal(old.Time) || ev.Payload.Container != old.Payload.Container || !ev.ACL.Equal(old.ACL) {
				t.Errorf("tombstone = %+v", ev)
			}
			if ev.Payload.State != nil || ev.Payload.Author != nil || ev.Payload.Text != "" || ev.Payload.Revision != nil || !strings.HasPrefix(ev.Payload.Artifact, artifact+":tombstone:") || ev.NativeID != ev.Payload.Artifact || ev.ID != connector.EventID(sourceID, ev.NativeID) {
				t.Error("tombstone carried content or wrong identity")
			}
			n := len(s.history())
			send(t, h, string(raw), 204)
			if len(s.history()) != n {
				t.Fatal("missing deletion emitted event")
			}
		})
	}
}

func TestPrivateAccessAndDeletionLeavesChildren(t *testing.T) {
	for _, author := range []string{"", "kyle", "shed", "factory"} {
		t.Run("author "+author, func(t *testing.T) {
			c := build(t, source())
			s := &memoryStore{}
			h := handler(c, s)
			fields := map[string]any{"repos": nil, "author": nil}
			if author != "" {
				fields["author"] = author
			}
			send(t, h, with(t, fixture(t, "project"), fields), 202)
			want := connector.ACL{{Kind: connector.ACLIdentity, Source: sourceID, NativeID: "factory"}}
			if author != "" && author != "factory" {
				want = append(want, connector.ACLEntry{Kind: connector.ACLIdentity, Source: sourceID, NativeID: author})
			}
			send(t, h, fixture(t, "workstream"), 202)
			send(t, h, fixture(t, "update"), 202)
			for _, artifact := range []string{"payments-rewrite", "payments-rewrite#retry-policy", "payments-rewrite#retry-policy:update:u-17"} {
				if !held(t, s, artifact).ACL.Equal(want) {
					t.Errorf("wrong ACL on %s", artifact)
				}
			}
			send(t, h, fixture(t, "delete"), 202)
			if _, ok, err := s.CurrentArtifact(t.Context(), sourceID, "payments-rewrite"); ok || err != nil {
				t.Fatal("project deletion was not applied")
			}
			held(t, s, "payments-rewrite#retry-policy")
			held(t, s, "payments-rewrite#retry-policy:update:u-17")
		})
	}
}

func TestValidBoundaryRequests(t *testing.T) {
	p, w, u := fixture(t, "project"), fixture(t, "workstream"), fixture(t, "update")
	paths := make([]string, 300)
	for i := range paths {
		paths[i] = fmt.Sprint(i)
	}
	repos := make([]work.Repo, 16)
	for i := range repos {
		repos[i] = work.Repo{Source: "github", Project: fmt.Sprint(i)}
	}
	tests := []struct{ name, body string }{
		{"128 byte project", with(t, p, map[string]any{"project": strings.Repeat("x", 128)})},
		{"id alphabet", with(t, p, map[string]any{"project": "aZ09-_./"})},
		{"16 repos", with(t, p, map[string]any{"repos": repos})},
		{"300 paths", with(t, w, map[string]any{"touches": []connector.Touch{{Source: "github", Project: "acme/api", Paths: paths}}})},
		{"16 touches", with(t, w, map[string]any{"touches": slices.Repeat([]connector.Touch{{Source: "github", Project: "acme/api"}}, 16)})},
		{"no description", with(t, p, map[string]any{"description": nil})},
		{"no state or status", with(t, w, map[string]any{"state": nil, "status": nil})},
		{"empty status with state", with(t, p, map[string]any{"status": ""})},
		{"64 byte status", with(t, p, map[string]any{"status": strings.Repeat("s", 64)})},
		{"128 byte stream", with(t, w, map[string]any{"workstream": strings.Repeat("s", 128)})},
		{"exactly 4 MiB", u + strings.Repeat(" ", (4<<20)-len(u))},
		{"128 byte update", with(t, u, map[string]any{"update": strings.Repeat("u", 128)})},
	}
	for _, state := range []string{"proposed", "started", "blocked", "completed", "abandoned"} {
		tests = append(tests, struct{ name, body string }{"core state " + state, with(t, p, map[string]any{"state": state})})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := build(t, source())
			s := &memoryStore{}
			h := handler(c, s)
			send(t, h, p, 202)
			send(t, h, w, 202)
			send(t, h, tt.body, 202)
		})
	}
}

func seedChildren(t *testing.T, h http.Handler) {
	t.Helper()
	send(t, h, fixture(t, "project"), 202)
	send(t, h, fixture(t, "workstream"), 202)
	send(t, h, fixture(t, "update"), 202)
	send(t, h, with(t, fixture(t, "update"), map[string]any{"workstream": nil, "update": "project-update"}), 202)
}
func restrictedProject(t *testing.T) string {
	t.Helper()
	return with(t, fixture(t, "project"), map[string]any{"repos": []work.Repo{{Source: "github", Project: "acme/private"}}, "updated_at": "2026-10-15T17:00:00Z"})
}
func assertMirrored(t *testing.T, s *memoryStore) {
	t.Helper()
	root := held(t, s, "payments-rewrite")
	all, err := s.CurrentArtifacts(t.Context(), sourceID)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, ev := range all {
		if ev.Payload.Artifact != root.Payload.Artifact {
			count++
			if !ev.ACL.Equal(root.ACL) {
				t.Errorf("child %s carries old ACL", ev.Payload.Artifact)
			}
		}
	}
	if count != 3 {
		t.Fatalf("have %d children, want 3", count)
	}
}

func TestPropagation(t *testing.T) {
	c := build(t, source())
	s := &memoryStore{}
	h := handler(c, s)
	seedChildren(t, h)
	before := s.history()
	n := len(before)
	response := send(t, h, restrictedProject(t), 202)
	if len(s.history()) != n+4 {
		t.Fatalf("propagation emitted %d events, want 4", len(s.history())-n)
	}
	assertMirrored(t, s)
	for _, old := range before[1:] {
		current := held(t, s, old.Payload.Artifact)
		if old.ID == current.ID || !old.Time.Equal(current.Time) {
			t.Error("ACL change did not revise id or preserved time incorrectly")
		}
		wantPayload := old.Payload
		wantPayload.Revision = nil
		gotPayload := current.Payload
		gotPayload.Revision = nil
		if !reflect.DeepEqual(wantPayload, gotPayload) {
			t.Error("ACL propagation changed payload content")
		}
		if !old.Payload.Revision.EditedAt.Equal(current.Payload.Revision.EditedAt) {
			t.Error("first ACL revision unnecessarily changed source edited_at")
		}
	}
	n = len(s.history())
	if again := send(t, h, restrictedProject(t), 202); again != response || len(s.history()) != n {
		t.Fatal("unchanged repost emitted anything or changed id")
	}
	if err := c.Poll(t.Context(), connector.NewGate(s, sourceID, c.Describe(), connector.NewAllowlist(source()))); err != nil {
		t.Fatal(err)
	}
	if len(s.history()) != n {
		t.Fatal("reconciliation emitted unchanged children")
	}
	// A second transition back to a previously held ACL must survive real L0
	// deduplication, including unchanged child timestamps.
	reverted := with(t, fixture(t, "project"), map[string]any{"updated_at": "2026-10-16T17:00:00Z"})
	send(t, h, reverted, 202)
	assertMirrored(t, s)
	for _, repo := range []string{"acme/private", "acme/api"} {
		send(t, h, with(t, fixture(t, "project"), map[string]any{"repos": []work.Repo{{Source: "github", Project: repo}}, "updated_at": "2026-10-17T17:00:00Z"}), 202)
		assertMirrored(t, s)
	}
}

func TestInterruptedPropagationRecovery(t *testing.T) {
	for _, interruption := range []string{"emit fails partway", "stop after project written"} {
		for _, recovery := range []string{"identical repost", "fresh runtime start"} {
			t.Run(interruption+" then "+recovery, func(t *testing.T) {
				c := build(t, source())
				s := &memoryStore{}
				h := handler(c, s)
				seedChildren(t, h)
				restricted := restrictedProject(t)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				childEmits := 0
				s.before = func(_ context.Context, ev connector.Event) error {
					if ev.Payload.Artifact == "payments-rewrite" {
						if interruption == "stop after project written" {
							// Commit the project before simulating process loss; the handler's
							// next read sees cancellation and cannot acknowledge the post.
							s.mu.Lock()
							s.events = append(s.events, ev)
							s.mu.Unlock()
							cancel()
							return context.Canceled
						}
						return nil
					}
					childEmits++
					if childEmits == 2 {
						return errors.New("interrupted propagation")
					}
					return nil
				}
				w := request(ctx, h, http.MethodPost, "factory-token", restricted)
				if w.Code != 500 {
					t.Fatalf("interrupted post answered %d %s", w.Code, w.Body.String())
				}
				root := held(t, s, "payments-rewrite")
				if len(root.ACL) != 1 || root.ACL[0].Containers[0].NativeID != "acme/private" {
					t.Fatal("interruption did not leave new project persisted")
				}
				s.before = nil
				if recovery == "identical repost" {
					send(t, h, restricted, 202)
				} else {
					// Exercise the actual runtime startup lifecycle, with no recovery post.
					registry := connector.NewRegistry()
					if err := registry.Register(work.Type, work.NewFactory(principals(), []string{sourceID, "github"}, lookup)); err != nil {
						t.Fatal(err)
					}
					runtime, err := connector.NewRuntime(t.Context(), connector.RuntimeOptions{Sources: []connector.SourceConfig{source()}, Registry: registry, Sink: s})
					if err != nil {
						t.Fatal(err)
					}
					runctx, stop := context.WithCancel(t.Context())
					completed := make(chan struct{}, 1)
					s.afterInventory = func(events []connector.Event) {
						for _, ev := range events {
							if !ev.ACL.Equal(root.ACL) {
								return
							}
						}
						if len(events) == 4 {
							select {
							case completed <- struct{}{}:
							default:
							}
						}
					}
					exited := make(chan error, 1)
					go func() { exited <- runtime.Run(runctx) }()
					select {
					case <-completed:
					case <-time.After(5 * time.Second):
						stop()
						t.Fatal("startup did not reconcile")
					}
					// The verified inventory contains the repaired children;
					// stop and join the runtime before inspecting the store.
					stop()
					if err := <-exited; err != nil {
						t.Fatal(err)
					}
					s.afterInventory = nil
				}
				assertMirrored(t, s)
				n := len(s.history())
				send(t, h, restricted, 202)
				if len(s.history()) != n {
					t.Fatal("recovered state still owed propagation")
				}
			})
		}
	}
}

func TestConcurrentChildAndProjectPosts(t *testing.T) {
	for _, first := range []string{"child", "project"} {
		t.Run(first+" enters first", func(t *testing.T) {
			c := build(t, source())
			s := &memoryStore{}
			h := handler(c, s)
			send(t, h, fixture(t, "project"), 202)
			child := with(t, fixture(t, "update"), map[string]any{"workstream": nil})
			entered := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			s.before = func(_ context.Context, ev connector.Event) error {
				isProject := ev.Payload.Artifact == "payments-rewrite"
				if (first == "child" && !isProject) || (first == "project" && isProject) {
					once.Do(func() { close(entered); <-release })
				}
				return nil
			}
			childResponse := make(chan *httptest.ResponseRecorder, 1)
			projectResponse := make(chan *httptest.ResponseRecorder, 1)
			postChild := func() { childResponse <- request(t.Context(), h, http.MethodPost, "factory-token", child) }
			postProject := func() {
				projectResponse <- request(t.Context(), h, http.MethodPost, "factory-token", restrictedProject(t))
			}
			if first == "child" {
				go postChild()
			} else {
				go postProject()
			}
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("first post did not reach emission")
			}
			if first == "child" {
				go postProject()
			} else {
				go postChild()
			}
			// The second request must not finish while the first holds the project's
			// lock. Without serialization, a child can finish an old-ACL emission
			// after the project's inventory walk has already missed it.
			select {
			case w := <-childResponse:
				close(release)
				t.Fatalf("child passed project lock: %d", w.Code)
			case w := <-projectResponse:
				close(release)
				t.Fatalf("project passed child lock: %d", w.Code)
			case <-time.After(30 * time.Millisecond):
			}
			close(release)
			for _, ch := range []chan *httptest.ResponseRecorder{childResponse, projectResponse} {
				select {
				case w := <-ch:
					if w.Code != 202 {
						t.Fatalf("concurrent response %d %s", w.Code, w.Body.String())
					}
				case <-time.After(5 * time.Second):
					t.Fatal("concurrent post stuck")
				}
			}
			root := held(t, s, "payments-rewrite")
			update := held(t, s, "payments-rewrite:update:u-17")
			if !root.ACL.Equal(update.ACL) || root.ACL[0].Containers[0].NativeID != "acme/private" {
				t.Error("concurrent child retained permissive list")
			}
		})
	}
}

func TestContractFixtures(t *testing.T) {
	raw, err := os.ReadFile("../../../docs/connector-contract.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"project", "workstream", "update", "delete", "event"} {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(string(raw), strings.TrimSpace(fixture(t, name))) {
				t.Errorf("contract is missing %s fixture", name)
			}
		})
	}
	c := build(t, source())
	s := &memoryStore{}
	send(t, handler(c, s), fixture(t, "project"), 202)
	var want connector.Event
	if err := json.Unmarshal([]byte(fixture(t, "event")), &want); err != nil {
		t.Fatal(err)
	}
	got := s.history()[0]
	// Native is raw JSON; compare the encoded objects to ignore indentation.
	gotRaw, _ := json.Marshal(got)
	wantRaw, _ := json.Marshal(want)
	var a, b any
	_ = json.Unmarshal(gotRaw, &a)
	_ = json.Unmarshal(wantRaw, &b)
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("written event drifted from contract\ngot %s\nwant %s", gotRaw, wantRaw)
	}
}

func TestDeletingAParentDoesNotDeleteChildren(t *testing.T) {
	for _, kind := range []string{"project", "workstream"} {
		t.Run(kind, func(t *testing.T) {
			c := build(t, source())
			s := &memoryStore{}
			h := handler(c, s)
			seedChildren(t, h)
			deletion := map[string]any{"kind": kind, "project": "payments-rewrite", "deleted": true}
			if kind == "workstream" {
				deletion["workstream"] = "retry-policy"
			}
			raw, _ := json.Marshal(deletion)
			send(t, h, string(raw), 202)
			held(t, s, "payments-rewrite#retry-policy:update:u-17")
			held(t, s, "payments-rewrite:update:project-update")
			n := len(s.history())
			if err := c.Poll(t.Context(), s); err != nil {
				t.Fatal(err)
			}
			if len(s.history()) != n {
				t.Fatal("startup reconciliation touched orphaned children")
			}
		})
	}
}

func TestPropagationSkipsDeletedChildren(t *testing.T) {
	c := build(t, source())
	s := &memoryStore{}
	h := handler(c, s)
	seedChildren(t, h)
	send(t, h, `{"kind":"update","project":"payments-rewrite","workstream":"retry-policy","update":"u-17","deleted":true}`, 202)
	n := len(s.history())
	send(t, h, restrictedProject(t), 202)
	if len(s.history()) != n+3 {
		t.Fatal("propagation re-emitted a deleted child")
	}
	if _, ok, err := s.CurrentArtifact(t.Context(), sourceID, "payments-rewrite#retry-policy:update:u-17"); ok || err != nil {
		t.Fatal("deleted child came back")
	}
}
