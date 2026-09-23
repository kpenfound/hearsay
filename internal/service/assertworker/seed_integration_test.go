//go:build integration

package assertworker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/github"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/principal"
	"github.com/kpenfound/hearsay/internal/service/assertworker"
	"github.com/kpenfound/hearsay/internal/telemetry"
)

// seedSource is the GitHub source the seeding fixtures belong to.
const seedSource = "github-acme"

// recordedGitHub replays GitHub's answers recorded in testdata/github, by REST
// path, and answers anything else with a 404. It keeps every path asked for.
type recordedGitHub struct {
	srv *httptest.Server

	mu       sync.Mutex
	answers  map[string][]byte
	requests []string
}

func newRecordedGitHub(t *testing.T) *recordedGitHub {
	t.Helper()
	gh := &recordedGitHub{answers: map[string][]byte{}}
	for path, file := range map[string]string{
		"/repos/acme/api":                             "repo.json",
		"/repos/acme/api/contents":                    "contents-root.json",
		"/repos/acme/api/contents/.github/CODEOWNERS": "contents-codeowners.json",
	} {
		raw, err := os.ReadFile(filepath.Join("testdata", "github", file))
		if err != nil {
			t.Fatal(err)
		}
		gh.answers[path] = raw
	}
	gh.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gh.mu.Lock()
		defer gh.mu.Unlock()
		gh.requests = append(gh.requests, r.Method+" "+r.URL.RequestURI())
		raw, ok := gh.answers[r.URL.Path]
		if !ok || r.Header.Get("Authorization") != "Bearer seed-token" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(raw)
	}))
	t.Cleanup(gh.srv.Close)
	return gh
}

func (gh *recordedGitHub) drop(path string) {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	delete(gh.answers, path)
}

func (gh *recordedGitHub) seen() []string {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	return slices.Clone(gh.requests)
}

// readers is what the binary hands the worker for the source: the GitHub
// reader, built from the source's configuration with its token.
func (gh *recordedGitHub) readers(t *testing.T) assertworker.Repos {
	t.Helper()
	settings, err := json.Marshal(map[string]string{"api_url": gh.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := github.NewReader(connector.SourceConfig{
		ID: seedSource, Type: github.Type, Containers: []string{"acme/api"},
		Settings: settings, Secrets: map[string]string{github.SecretToken: "seed-token"},
	})
	if err != nil {
		t.Fatalf("NewReader = %v", err)
	}
	return assertworker.Repos{seedSource: reader}
}

// seedConfig is a `code/` naming acme/api and its CODEOWNERS file, and the
// people that file's logins map to.
func seedConfig() config.Repo {
	api := config.SourceRef{Source: seedSource, Project: "acme/api"}
	return config.Repo{
		Principals: []principal.Principal{
			{ID: "kyle", Kind: principal.KindHuman, Identities: []principal.Identity{{Source: seedSource, NativeID: "u1", Handle: "kpenfound"}}},
			{ID: "sam", Kind: principal.KindHuman, Identities: []principal.Identity{{Source: seedSource, NativeID: "u2", Handle: "samr"}}},
		},
		Code: []config.CodeEntity{
			{ID: "code:acme/api", Type: config.TypeProject, Name: "acme/api", Repo: api, CodeOwners: ".github/CODEOWNERS"},
			{ID: "code:acme/api:engine", Type: config.TypeModule, Name: "engine",
				PathPatterns: []string{"engine/**"}, PartOf: []string{"code:acme/api"}, Repo: api},
		},
	}
}

// seeded is an entity as the assertions compare it.
type seeded struct {
	ID     string
	Origin l2.Origin
	Owners []string
}

func seededEntities(t *testing.T, store *l2.Store) []seeded {
	t.Helper()
	entities, err := store.Entities(t.Context())
	if err != nil {
		t.Fatalf("Entities() = %v", err)
	}
	var out []seeded
	for _, e := range entities {
		out = append(out, seeded{e.ID, e.Origin, e.Owners})
	}
	return out
}

func logged(t *testing.T) (context.Context, *bytes.Buffer) {
	var out bytes.Buffer
	return telemetry.WithLogger(t.Context(), slog.New(slog.NewJSONHandler(&out, nil))), &out
}

const noReaderWarning = "no repository reader"

// The acceptance criterion: startup seeds the top-level directories of a
// configured GitHub repository as modules and imports owners from its
// CODEOWNERS file, from recorded GitHub answers, reading the repository's
// metadata, its root listing and that one file and nothing else.
func TestStartupSeedsFromGitHub(t *testing.T) {
	pool := scratchPool(t)
	gh := newRecordedGitHub(t)
	ctx, log := logged(t)

	n, err := assertworker.SeedEntities(ctx, pool, seedConfig(), gh.readers(t))
	if err != nil {
		t.Fatalf("SeedEntities() = %v", err)
	}
	want := []seeded{
		// The CODEOWNERS file seeds the entity naming it and its descendants:
		// `*` is kyle's, /engine/ sam's, and /docs/'s owner maps to nobody. The
		// project has no path of its own to own.
		{"code:acme/api", l2.OriginConfig, nil},
		{"code:acme/api:cmd", l2.OriginRepoStructure, []string{"kyle"}},
		{"code:acme/api:docs", l2.OriginRepoStructure, nil},
		{"code:acme/api:engine", l2.OriginConfig, []string{"sam"}},
	}
	got := seededEntities(t, l2.New(pool))
	if n != len(want) || !slices.EqualFunc(got, want, func(a, b seeded) bool {
		return a.ID == b.ID && a.Origin == b.Origin && slices.Equal(a.Owners, b.Owners)
	}) {
		t.Errorf("SeedEntities() = %d, stored %+v, want %+v", n, got, want)
	}

	wantRequests := []string{
		"GET /repos/acme/api",
		"GET /repos/acme/api/contents",
		"GET /repos/acme/api/contents/.github/CODEOWNERS",
	}
	if got := gh.seen(); !slices.Equal(got, wantRequests) {
		t.Errorf("requests = %q, want %q: metadata, the root listing and the configured file only", got, wantRequests)
	}
	if strings.Contains(log.String(), noReaderWarning) {
		t.Errorf("log = %s, want no %q warning with a reader for the source", log, noReaderWarning)
	}
}

func TestStartupSeedingFromGitHubFailures(t *testing.T) {
	pool := scratchPool(t)

	t.Run("without a reader the worker says so", func(t *testing.T) {
		ctx, log := logged(t)
		if _, err := assertworker.SeedEntities(ctx, pool, seedConfig(), nil); err != nil {
			t.Fatalf("SeedEntities() = %v", err)
		}
		if !strings.Contains(log.String(), noReaderWarning) {
			t.Errorf("log = %s, want the %q warning", log, noReaderWarning)
		}
	})

	t.Run("a missing CODEOWNERS file is logged and skipped", func(t *testing.T) {
		gh := newRecordedGitHub(t)
		gh.drop("/repos/acme/api/contents/.github/CODEOWNERS")
		ctx, log := logged(t)
		if _, err := assertworker.SeedEntities(ctx, pool, seedConfig(), gh.readers(t)); err != nil {
			t.Fatalf("SeedEntities() = %v, want the file skipped", err)
		}
		want := []seeded{
			{"code:acme/api", l2.OriginConfig, nil},
			{"code:acme/api:cmd", l2.OriginRepoStructure, nil},
			{"code:acme/api:docs", l2.OriginRepoStructure, nil},
			{"code:acme/api:engine", l2.OriginConfig, nil},
		}
		got := seededEntities(t, l2.New(pool))
		if !slices.EqualFunc(got, want, func(a, b seeded) bool {
			return a.ID == b.ID && a.Origin == b.Origin && slices.Equal(a.Owners, b.Owners)
		}) {
			t.Errorf("stored %+v, want the layout and no owners, %+v", got, want)
		}
		if want := "CODEOWNERS file not found"; !strings.Contains(log.String(), want) {
			t.Errorf("log = %s, want %q", log, want)
		}
	})

	t.Run("an unreadable repository fails startup with the connector's error", func(t *testing.T) {
		gh := newRecordedGitHub(t)
		gh.drop("/repos/acme/api")
		_, err := assertworker.SeedEntities(t.Context(), pool, seedConfig(), gh.readers(t))
		want := "reading repository acme/api: GET /repos/acme/api: GitHub answered 404 Not Found"
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("SeedEntities() = %v, want an error containing %q", err, want)
		}
	})
}

// Issue #119: startup stores the hierarchy the sources merge to — a configured
// entity with no part_of is part of the directory the layout seeded above it.
func TestStartupStoresTheDerivedHierarchy(t *testing.T) {
	pool := scratchPool(t)
	gh := newRecordedGitHub(t)
	cfg := seedConfig()
	cfg.Code = append(cfg.Code, config.CodeEntity{
		ID: "code:acme/api:cmd/hearsay", Type: config.TypeModule, Name: "hearsay",
		PathPatterns: []string{"cmd/hearsay/**"}, Repo: cfg.Code[0].Repo,
	})

	if _, err := assertworker.SeedEntities(t.Context(), pool, cfg, gh.readers(t)); err != nil {
		t.Fatalf("SeedEntities() = %v", err)
	}
	entities, err := l2.New(pool).Entities(t.Context())
	if err != nil {
		t.Fatalf("Entities() = %v", err)
	}
	got := map[string][]string{}
	for _, e := range entities {
		got[e.ID] = e.PartOf
	}
	want := map[string][]string{
		"code:acme/api":             {},
		"code:acme/api:cmd":         {"code:acme/api"},
		"code:acme/api:cmd/hearsay": {"code:acme/api:cmd"},
		"code:acme/api:docs":        {"code:acme/api"},
		"code:acme/api:engine":      {"code:acme/api"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("stored part_of %v, want %v", got, want)
	}
}
