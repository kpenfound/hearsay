//go:build integration

package distiller_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/service/assertworker"
	"github.com/kpenfound/hearsay/internal/service/distiller"
)

// assertJobs is the serial key of every assert job for one document. It reads
// the table in one statement rather than listing the queue state by state,
// because a worker in another package's test may move the job between states
// while it is being looked for.
func assertJobs(t *testing.T, pool *pgxpool.Pool, docID string) []string {
	t.Helper()
	rows, err := pool.Query(t.Context(),
		`SELECT serial_key FROM queue_job WHERE kind = $1 AND target_id = $2 ORDER BY id`, l2.AssertKindName, docID)
	if err != nil {
		t.Fatalf("reading assert jobs: %v", err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatalf("reading assert jobs: %v", err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading assert jobs: %v", err)
	}
	return keys
}

// Equal-time channel messages arrive in sequence order, but L1 chooses its
// root by artifact ID. Both enqueue paths must use that root's touches.
func TestChatWindowTouchesScopeIsTheSameAfterRecovery(t *testing.T) {
	for _, tt := range []struct {
		name  string
		order []string
	}{
		{"reverse artifact order", []string{"b", "a"}},
		{"artifact order", []string{"a", "b"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pool := newScratchPool(t)
			src := newSource(t)
			cfg := testConfig(src)
			cfg.Repo.Sources = []connector.SourceConfig{{ID: src, Type: "slack"}, {ID: "github", Type: "github"}}
			cfg.Repo.Scopes = []config.Scope{
				{ID: "scope-a", Sources: []config.ScopeSource{{Source: "github", Containers: []string{"acme/a"}}}},
				{ID: "scope-b", Sources: []config.ScopeSource{{Source: "github", Containers: []string{"acme/b"}}}},
			}
			dir := t.TempDir()
			for name, body := range map[string]string{
				"sources/github.yaml":    "id: github\ntype: github\ncontainers: [acme/a, acme/b]\n",
				"scopes/scope-a.yaml":    "id: scope-a\nsources: [github]\n",
				"scopes/scope-b.yaml":    "id: scope-b\nsources: [github]\n",
				"authority/default.yaml": "scope: '*'\nratified_by:\n  principals: []\n  sources: []\n  artifacts: []\n",
				"authority/scope-a.yaml": "scope: scope-a\nratified_by:\n  sources: ['*']\n  artifacts: [chat_thread]\n",
			} {
				full := filepath.Join(dir, name)
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			loaded, err := config.Load(dir)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Repo.Authority = loaded.Authority
			messages := map[string]connector.Event{
				"a": chatMessage(src, "a", "Should we take the lock first?", day),
				"b": chatMessage(src, "b", "Agreed. Take the lock before writing.", day),
			}
			for _, id := range tt.order {
				ev := messages[id]
				ev.Payload.Touches = []connector.Touch{{Source: "github", Project: "acme/" + id}}
				ingest(t, pool, []connector.Event{ev})
			}
			current, err := l0.New(pool).Current(t.Context(), l0.ListOptions{Filter: l0.Filter{Source: src}, Limit: 10})
			if err != nil || len(current) != 2 {
				t.Fatalf("current messages = %+v, %v", current, err)
			}
			if current[0].Payload.Artifact != tt.order[0] || current[1].Payload.Artifact != tt.order[1] {
				t.Fatalf("current messages must retain arrival order: %+v", current)
			}
			registry, err := llm.NewFake(cfg.Repo.LLM, loadFixtures(t))
			if err != nil {
				t.Fatal(err)
			}
			d, err := distiller.New(pool, registry, cfg)
			if err != nil {
				t.Fatal(err)
			}
			id := l1.DocID(src, l1.ChatWindowKey(messages["a"]))
			result, err := d.Distill(t.Context(), id)
			if err != nil || !result.Written || !result.Asserting {
				t.Fatalf("Distill = %+v, %v", result, err)
			}
			stored, err := l1.New(pool).Get(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if len(stored.L0Refs) != 2 || stored.L0Refs[0] != connector.EventID(src, "a") {
				t.Fatalf("document root = %v, want message a first", stored.L0Refs)
			}
			if wrong := l2.RecordedTier(cfg.Repo.Authority.ForScope("scope-b"), stored.Document); wrong != l2.TierInferred {
				t.Fatalf("other message's authority must differ: %s", wrong)
			}
			for _, phase := range []string{"normal distillation", "startup recovery"} {
				if phase == "startup recovery" {
					if _, err := pool.Exec(t.Context(), `DELETE FROM queue_job WHERE kind = 'assert' AND target_id = $1`, id); err != nil {
						t.Fatal(err)
					}
					if _, err := assertworker.Sweep(t.Context(), pool, cfg.Repo); err != nil {
						t.Fatal(err)
					}
				}
				keys := assertJobs(t, pool, id)
				if len(keys) != 1 || keys[0] != "scope-a" {
					t.Fatalf("%s keys = %v, want scope-a", phase, keys)
				}
				if tier := l2.RecordedTier(cfg.Repo.Authority.ForScope(keys[0]), stored.Document); tier != l2.TierRatified {
					t.Fatalf("%s authority tier = %s, want ratified", phase, tier)
				}
			}
		})
	}
}

// A document whose outcome enters the assertion pipeline is written with its
// assert job, under the scope that covers the artifact; a re-distillation that
// changes nothing asks for none.
func TestADocumentThatAssertsIsWrittenWithItsAssertJob(t *testing.T) {
	pool := newPool(t)
	src := newSource(t)
	ingest(t, pool, fixtureEvents(src))
	d := newDistiller(t, pool, src)
	id := l1.DocID(src, repo+"#12")

	result, err := d.Distill(t.Context(), id)
	if err != nil {
		t.Fatalf("Distill(%s) = %v", id, err)
	}
	if !result.Written || !result.Asserting {
		t.Fatalf("Distill(%s) = %+v, want the document written and an assert job asked for", id, result)
	}
	keys := assertJobs(t, pool, id)
	if len(keys) != 1 || keys[0] != "api" {
		t.Fatalf("assert jobs for %s = %q, want one under the covering scope", id, keys)
	}

	again, err := d.Distill(t.Context(), id)
	if err != nil {
		t.Fatalf("Distill(%s, again) = %v", id, err)
	}
	if again.Written || again.Asserting {
		t.Errorf("Distill(%s, again) = %+v, want nothing written and no job", id, again)
	}
	if keys := assertJobs(t, pool, id); len(keys) != 1 {
		t.Errorf("assert jobs after re-distilling = %q, want still one", keys)
	}
}

// Recovery must select the same authority scope as the transaction that first
// wrote a work document, even though its own project container is not covered.
func TestWorkTouchesScopeIsTheSameAfterRecovery(t *testing.T) {
	pool := newScratchPool(t)
	src := newSource(t)
	events := fixtureEvents(src)[:3]
	for i := range events {
		events[i].Payload.Container = connector.Container{Kind: "project", NativeID: "work-project"}
		if i == 0 {
			events[i].Payload.Touches = []connector.Touch{{Source: "github", Project: repo}}
		}
	}
	ingest(t, pool, events)
	cfg := testConfig(src)
	cfg.Repo.Sources = []connector.SourceConfig{{ID: src, Type: "work"}, {ID: "github", Type: "github"}}
	cfg.Repo.Scopes[0].Sources = []config.ScopeSource{{Source: "github", Containers: []string{repo}}}
	cfg.Repo.Scopes[0].Tracker = config.SourceRef{}
	// Distinct policies make an accidental source fallback observable.
	dir := t.TempDir()
	for name, body := range map[string]string{
		"sources/github.yaml":    "id: github\ntype: github\ncontainers: [acme/api]\n",
		"principals/kyle.yaml":   "id: kyle\nidentities: [{source: github, native_id: u1}]\n",
		"scopes/api.yaml":        "id: api\nsources: [github]\n",
		"authority/default.yaml": "scope: '*'\nratified_by:\n  principals: []\n  sources: []\n  artifacts: []\n",
		"authority/api.yaml":     "scope: api\nratified_by:\n  principals: [kyle]\n  sources: ['*']\n  artifacts: [issue]\n",
	} {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	loaded, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Repo.Authority = loaded.Authority
	registry, err := llm.NewFake(cfg.Repo.LLM, loadFixtures(t))
	if err != nil {
		t.Fatal(err)
	}
	d, err := distiller.New(pool, registry, cfg)
	if err != nil {
		t.Fatal(err)
	}
	id := l1.DocID(src, repo+"#12")
	result, err := d.Distill(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Asserting {
		t.Fatalf("Distill = %+v", result)
	}
	keys := assertJobs(t, pool, id)
	if len(keys) != 1 || keys[0] != "api" {
		t.Fatalf("distillation keys = %v", keys)
	}
	stored, err := l1.New(pool).Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	normal := l2.RecordedTier(cfg.Repo.Authority.ForScope(keys[0]), stored.Document)
	if normal != l2.TierRatified {
		t.Fatalf("normal authority tier = %s", normal)
	}
	if fallback := l2.RecordedTier(cfg.Repo.Authority.ForScope("source:"+src), stored.Document); fallback != l2.TierInferred {
		t.Fatalf("fallback policy must differ: %s", fallback)
	}
	if _, err := pool.Exec(t.Context(), `DELETE FROM queue_job WHERE kind = 'assert' AND target_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := assertworker.Sweep(t.Context(), pool, cfg.Repo); err != nil {
		t.Fatal(err)
	}
	keys = assertJobs(t, pool, id)
	if len(keys) != 1 || keys[0] != "api" {
		t.Fatalf("recovery keys = %v", keys)
	}
	if recovered := l2.RecordedTier(cfg.Repo.Authority.ForScope(keys[0]), stored.Document); recovered != normal {
		t.Fatalf("recovered tier = %s, normal = %s", recovered, normal)
	}
	// Pins select their key from the same root's persisted touches as well.
	gesture, written, err := l2.RecordGesture(t.Context(), pool, cfg.Repo, l2.GestureRequest{
		Event: connector.EventID(src, "touches-pin"), Principal: "kyle", Action: l2.GesturePin, Documents: []string{id},
	})
	if err != nil || !written || gesture.Scope != "api" {
		t.Fatalf("pin = %+v, %v, %v", gesture, written, err)
	}

}
