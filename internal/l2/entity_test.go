package l2_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/telemetry"
)

// fakeRepos is a repository reader over fixed content.
type fakeRepos struct {
	files map[string]string   // "<project>:<path>"
	top   map[string][]string // project
	err   error
	// unsupported are the sources the reader cannot read at all.
	unsupported []string
	reads       []string
}

func (f *fakeRepos) ReadFile(_ context.Context, repo config.SourceRef, path string) ([]byte, error) {
	f.reads = append(f.reads, repo.Project+":"+path)
	if f.err != nil {
		return nil, f.err
	}
	content, ok := f.files[repo.Project+":"+path]
	if !ok {
		return nil, fmt.Errorf("%s: %w", path, fs.ErrNotExist)
	}
	return []byte(content), nil
}

func (f *fakeRepos) TopLevel(_ context.Context, repo config.SourceRef) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	if slices.Contains(f.unsupported, repo.Source) {
		return nil, fmt.Errorf("source %s: %w", repo.Source, errors.ErrUnsupported)
	}
	return f.top[repo.Project], nil
}

func seedRepo() config.Repo {
	api := config.SourceRef{Source: "github-acme", Project: "acme/api"}
	return config.Repo{
		Principals: ownersPrincipals(),
		Code: []config.CodeEntity{
			{ID: "code:acme/api:engine", Type: config.TypeModule, Name: "engine", Aliases: []string{"the engine"},
				PathPatterns: []string{"engine/**"}, PartOf: []string{"code:acme/api"}, Repo: api, CodeOwners: ".github/CODEOWNERS"},
			{ID: "code:acme/api:engine/server", Type: config.TypeService, Name: "Engine server",
				PathPatterns: []string{"engine/server/**"}, PartOf: []string{"code:acme/api:engine"}, Repo: api},
			{ID: "code:acme/api:engine/client", Type: config.TypeModule, Name: "Engine client",
				PathPatterns: []string{"engine/client/**"}, PartOf: []string{"code:acme/api:engine"}, Repo: api,
				Owners: []string{"robin"}},
			{ID: "code:acme/api:queue", Type: config.TypeModule, Name: "queue",
				PathPatterns: []string{"internal/queue/**"}, Repo: api},
		},
	}
}

// entitySummary is what a seeding case compares.
type entitySummary struct {
	id     string
	typ    l2.EntityType
	origin l2.Origin
	owners []string
	partOf []string
	paths  []string
}

func summarize(entities []l2.Entity) []entitySummary {
	out := []entitySummary{}
	for _, e := range entities {
		out = append(out, entitySummary{e.ID, e.Type, e.Origin, e.Owners, e.PartOf, e.PathPatterns})
	}
	return out
}

func sameSummaries(a, b []entitySummary) bool {
	return slices.EqualFunc(a, b, func(x, y entitySummary) bool {
		return x.id == y.id && x.typ == y.typ && x.origin == y.origin && slices.Equal(x.owners, y.owners) &&
			slices.Equal(x.partOf, y.partOf) && slices.Equal(x.paths, y.paths)
	})
}

func TestSeedFromConfigurationAlone(t *testing.T) {
	got, err := l2.Seed(t.Context(), seedRepo(), nil)
	if err != nil {
		t.Fatalf("Seed() = %v", err)
	}
	want := []entitySummary{
		{"code:acme/api:engine", l2.TypeModule, l2.OriginConfig, nil, []string{"code:acme/api"}, []string{"engine/**"}},
		{"code:acme/api:engine/client", l2.TypeModule, l2.OriginConfig, []string{"robin"}, []string{"code:acme/api:engine"}, []string{"engine/client/**"}},
		{"code:acme/api:engine/server", l2.TypeService, l2.OriginConfig, nil, []string{"code:acme/api:engine"}, []string{"engine/server/**"}},
		{"code:acme/api:queue", l2.TypeModule, l2.OriginConfig, nil, nil, []string{"internal/queue/**"}},
	}
	if s := summarize(got); !sameSummaries(s, want) {
		t.Errorf("Seed() =\n%+v\nwant\n%+v", s, want)
	}
	if !slices.Equal(got[0].Aliases, []string{"the engine"}) || got[0].Name != "engine" {
		t.Errorf("Seed() lost the name or the aliases: %+v", got[0])
	}
}

func TestSeedFromARepository(t *testing.T) {
	reader := &fakeRepos{
		files: map[string]string{"acme/api:.github/CODEOWNERS": codeowners},
		top:   map[string][]string{"acme/api": {"engine", "cmd", ".github", "docs/"}},
	}
	got, err := l2.Seed(t.Context(), seedRepo(), reader)
	if err != nil {
		t.Fatalf("Seed() = %v", err)
	}
	want := []entitySummary{
		// The project and the directories nobody configured, from the layout;
		// not the dot-directory, and not `engine`, which is configured.
		{"code:acme/api", l2.TypeProject, l2.OriginRepoStructure, nil, nil, nil},
		{"code:acme/api:cmd", l2.TypeModule, l2.OriginRepoStructure, nil, []string{"code:acme/api"}, []string{"cmd/**"}},
		{"code:acme/api:docs", l2.TypeModule, l2.OriginRepoStructure, nil, []string{"code:acme/api"}, []string{"docs/**"}},
		// Owners from the CODEOWNERS file the entity names, for it and its
		// descendants, resolved to principals; a login nobody mapped is left
		// out, and configured owners stand.
		{"code:acme/api:engine", l2.TypeModule, l2.OriginConfig, []string{"kyle"}, []string{"code:acme/api"}, []string{"engine/**"}},
		{"code:acme/api:engine/client", l2.TypeModule, l2.OriginConfig, []string{"robin"}, []string{"code:acme/api:engine"}, []string{"engine/client/**"}},
		{"code:acme/api:engine/server", l2.TypeService, l2.OriginConfig, []string{"sam"}, []string{"code:acme/api:engine"}, []string{"engine/server/**"}},
		// Not under the entity that names the file, so not seeded from it; and
		// under no other entity's patterns, so part of the repository's project.
		{"code:acme/api:queue", l2.TypeModule, l2.OriginConfig, nil, []string{"code:acme/api"}, []string{"internal/queue/**"}},
	}
	if s := summarize(got); !sameSummaries(s, want) {
		t.Errorf("Seed() =\n%+v\nwant\n%+v", s, want)
	}
	if !slices.Equal(reader.reads, []string{"acme/api:.github/CODEOWNERS"}) {
		t.Errorf("Seed() read %q, want the one CODEOWNERS file configuration names", reader.reads)
	}

	again, err := l2.Seed(t.Context(), seedRepo(), reader)
	if err != nil || !sameSummaries(summarize(again), summarize(got)) {
		t.Errorf("Seed() a second time = %+v, %v, want the same entities", summarize(again), err)
	}
}

func TestSeedFailsWhenTheRepositoryCannotBeRead(t *testing.T) {
	boom := errors.New("rate limited")
	if _, err := l2.Seed(t.Context(), seedRepo(), &fakeRepos{err: boom}); !errors.Is(err, boom) {
		t.Errorf("Seed() = %v, want the reader's error", err)
	}
	unreadable := &fakeRepos{top: map[string][]string{"acme/api": {"engine"}}}
	denied := errors.New("403 Forbidden")
	if _, err := l2.Seed(t.Context(), seedRepo(), &readErr{unreadable, denied}); !errors.Is(err, denied) {
		t.Errorf("Seed() with the CODEOWNERS file unreadable = %v, want the reader's error", err)
	}
}

// readErr is a reader whose files cannot be read, for a reason other than
// their not being there.
type readErr struct {
	*fakeRepos
	err error
}

func (r *readErr) ReadFile(context.Context, config.SourceRef, string) ([]byte, error) {
	return nil, r.err
}

// seedLog is a context whose log lines land in the returned buffer.
func seedLog(t *testing.T) (context.Context, *bytes.Buffer) {
	var out bytes.Buffer
	return telemetry.WithLogger(t.Context(), slog.New(slog.NewJSONHandler(&out, nil))), &out
}

func TestSeedSkipsAMissingCodeOwnersFile(t *testing.T) {
	ctx, log := seedLog(t)
	reader := &fakeRepos{top: map[string][]string{"acme/api": {"engine", "cmd"}}}
	got, err := l2.Seed(ctx, seedRepo(), reader)
	if err != nil {
		t.Fatalf("Seed() with the CODEOWNERS file missing = %v, want it skipped", err)
	}
	// The layout is still imported; the owners are configuration's alone.
	want := []entitySummary{
		{"code:acme/api", l2.TypeProject, l2.OriginRepoStructure, nil, nil, nil},
		{"code:acme/api:cmd", l2.TypeModule, l2.OriginRepoStructure, nil, []string{"code:acme/api"}, []string{"cmd/**"}},
		{"code:acme/api:engine", l2.TypeModule, l2.OriginConfig, nil, []string{"code:acme/api"}, []string{"engine/**"}},
		{"code:acme/api:engine/client", l2.TypeModule, l2.OriginConfig, []string{"robin"}, []string{"code:acme/api:engine"}, []string{"engine/client/**"}},
		{"code:acme/api:engine/server", l2.TypeService, l2.OriginConfig, nil, []string{"code:acme/api:engine"}, []string{"engine/server/**"}},
		{"code:acme/api:queue", l2.TypeModule, l2.OriginConfig, nil, []string{"code:acme/api"}, []string{"internal/queue/**"}},
	}
	if s := summarize(got); !sameSummaries(s, want) {
		t.Errorf("Seed() =\n%+v\nwant\n%+v", s, want)
	}
	for _, line := range []string{`"msg":"CODEOWNERS file not found: no owners imported from it"`, `"entity":"code:acme/api:engine"`, `"path":".github/CODEOWNERS"`} {
		if !strings.Contains(log.String(), line) {
			t.Errorf("log = %s, want a line with %s", log, line)
		}
	}
}

func TestSeedSkipsARepositoryNoReaderSupports(t *testing.T) {
	ctx, log := seedLog(t)
	reader := &fakeRepos{unsupported: []string{"github-acme"}}
	got, err := l2.Seed(ctx, seedRepo(), reader)
	if err != nil {
		t.Fatalf("Seed() = %v, want the repository seeded from configuration", err)
	}
	alone, err := l2.Seed(t.Context(), seedRepo(), nil)
	if err != nil || !sameSummaries(summarize(got), summarize(alone)) {
		t.Errorf("Seed() = %+v, want what configuration alone seeds, %+v (%v)", summarize(got), summarize(alone), err)
	}
	if len(reader.reads) != 0 {
		t.Errorf("Seed() read %q from a repository it cannot read", reader.reads)
	}
	if want := `"msg":"no repository reader for this source: its layout and CODEOWNERS files are not imported"`; !strings.Contains(log.String(), want) {
		t.Errorf("log = %s, want %s", log, want)
	}
}

// Issue #119: seeding ranks the hierarchy's sources. `code/`'s part_of stands
// where it sets any; everywhere else an entity is part of the nearest entity
// its path patterns lie under, configured or seeded from the layout; and the
// derived edge that would close a cycle with configuration is dropped.
func TestSeedDerivesTheHierarchyFromPathPatterns(t *testing.T) {
	api := config.SourceRef{Source: "github-acme", Project: "acme/api"}
	repo := config.Repo{Code: []config.CodeEntity{
		{ID: "code:acme/api:engine", Type: config.TypeModule, PathPatterns: []string{"engine/**"}, Repo: api},
		{ID: "code:acme/api:engine/server/write", Type: config.TypeModule,
			PathPatterns: []string{"engine/server/write/**"}, Repo: api},
		{ID: "code:acme/api:engine/server", Type: config.TypeService, PathPatterns: []string{"engine/server/**"}, Repo: api},
		// Configured: replaces, not joins, the engine the patterns imply.
		{ID: "code:acme/api:engine/client", Type: config.TypeModule,
			PathPatterns: []string{"engine/client/**"}, PartOf: []string{"code:acme/api:queue"}, Repo: api},
		{ID: "code:acme/api:queue", Type: config.TypeModule, PathPatterns: []string{"internal/queue/**"}, Repo: api},
		// Under a directory nobody configured.
		{ID: "code:acme/api:lib/x", Type: config.TypeModule, PathPatterns: []string{"lib/x/**"}, Repo: api},
		// A person put tools under tools/gen; the patterns say the opposite.
		{ID: "code:acme/api:tools", Type: config.TypeModule,
			PathPatterns: []string{"tools/**"}, PartOf: []string{"code:acme/api:tools/gen"}, Repo: api},
		{ID: "code:acme/api:tools/gen", Type: config.TypeModule, PathPatterns: []string{"tools/gen/**"}, Repo: api},
	}}
	parents := func(entities []l2.Entity) map[string][]string {
		out := map[string][]string{}
		for _, e := range entities {
			out[e.ID] = e.PartOf
		}
		return out
	}

	ctx, log := seedLog(t)
	got, err := l2.Seed(ctx, repo, &fakeRepos{top: map[string][]string{"acme/api": {"engine", "lib", "tools", "internal"}}})
	if err != nil {
		t.Fatalf("Seed() = %v", err)
	}
	want := map[string][]string{
		"code:acme/api":                     nil,
		"code:acme/api:engine":              {"code:acme/api"},
		"code:acme/api:engine/client":       {"code:acme/api:queue"},
		"code:acme/api:engine/server":       {"code:acme/api:engine"},
		"code:acme/api:engine/server/write": {"code:acme/api:engine/server"},
		"code:acme/api:internal":            {"code:acme/api"},
		"code:acme/api:lib":                 {"code:acme/api"},
		"code:acme/api:lib/x":               {"code:acme/api:lib"},
		"code:acme/api:queue":               {"code:acme/api:internal"},
		"code:acme/api:tools":               {"code:acme/api:tools/gen"},
		"code:acme/api:tools/gen":           nil,
	}
	if p := parents(got); fmt.Sprint(p) != fmt.Sprint(want) {
		t.Errorf("Seed() part_of =\n%v\nwant\n%v", p, want)
	}
	if d := droppedIn(t, log.String()); fmt.Sprint(d) != fmt.Sprint([]dropped{{"code:acme/api:tools/gen", "code:acme/api:tools", "repo_structure"}}) {
		t.Errorf("logged dropped edges %v, want tools/gen's edge to tools", d)
	}

	// Nesting reads patterns, not the repository: without a reader there is no
	// project or layout, and the configured entities nest all the same.
	alone, err := l2.Seed(t.Context(), repo, nil)
	if err != nil {
		t.Fatalf("Seed() = %v", err)
	}
	wantAlone := map[string][]string{
		"code:acme/api:engine":              nil,
		"code:acme/api:engine/client":       {"code:acme/api:queue"},
		"code:acme/api:engine/server":       {"code:acme/api:engine"},
		"code:acme/api:engine/server/write": {"code:acme/api:engine/server"},
		"code:acme/api:lib/x":               nil,
		"code:acme/api:queue":               nil,
		"code:acme/api:tools":               {"code:acme/api:tools/gen"},
		"code:acme/api:tools/gen":           nil,
	}
	if p := parents(alone); fmt.Sprint(p) != fmt.Sprint(wantAlone) {
		t.Errorf("Seed() without a reader part_of =\n%v\nwant\n%v", p, wantAlone)
	}
}
