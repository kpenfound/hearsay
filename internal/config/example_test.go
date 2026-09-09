package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/principal"
)

// docsPath is the schema documentation, which carries the example this package
// is tested against.
const docsPath = "../../docs/config.md"

// TestDocsExampleLoadsBothWays loads the complete example in docs/config.md —
// both the single file and the directory — and checks that they are the same
// configuration.
//
// The example is read out of the documentation rather than copied into testdata
// on purpose. An example that has drifted from the loader is worse than no
// example, and the only way to be sure it has not is to run the one people
// read.
func TestDocsExampleLoadsBothWays(t *testing.T) {
	examples := docExamples(t)
	single := writeFiles(t, filesUnder(t, examples, "single"))
	dir := writeFiles(t, filesUnder(t, examples, "dir"))

	fromFile, err := config.Load(filepath.Join(single, "hearsay.yaml"))
	if err != nil {
		t.Fatalf("Load(the single-file example) = %v, want no error", err)
	}
	fromDir, err := config.Load(dir)
	if err != nil {
		t.Fatalf("Load(the directory example) = %v, want no error", err)
	}

	// Pointing at the directory holding the single file finds it, which is what
	// `--config ./config` does for a team that has not split it up.
	fromDirWithFile, err := config.Load(single)
	if err != nil {
		t.Fatalf("Load(the directory holding the single file) = %v, want no error", err)
	}
	if fromDirWithFile.Digest != fromFile.Digest {
		t.Errorf("digest via the directory = %s, via the file = %s, want the same file to give the same digest",
			fromDirWithFile.Digest, fromFile.Digest)
	}

	// Path and digest identify the bytes, not the meaning, and the two forms
	// are different bytes. Everything else has to match.
	fileRepo, dirRepo := normalize(fromFile), normalize(fromDir)
	if !reflect.DeepEqual(fileRepo, dirRepo) {
		t.Errorf("the two forms of the example load differently\n single file: %+v\n  directory: %+v", fileRepo, dirRepo)
	}

	if fromFile.Digest == fromDir.Digest {
		t.Errorf("both forms have digest %s, want different bytes to digest differently", fromFile.Digest)
	}
	if !strings.HasPrefix(fromFile.Digest, "sha256:") {
		t.Errorf("digest = %q, want it to name the algorithm", fromFile.Digest)
	}
}

// TestDocsExampleIsTheConfigurationItDescribes checks the parts of the example
// a reader is most likely to rely on, so that changing the example without
// meaning to changes a test.
func TestDocsExampleIsTheConfigurationItDescribes(t *testing.T) {
	repo := loadExample(t)

	scope, ok := repo.Scope("api")
	if !ok {
		t.Fatalf("Scope(api) not found; scopes are %v", scopeIDs(repo))
	}
	// The scope takes one Discord channel of the two the source ingests: a
	// scope filters, and it cannot widen what the source allows.
	if got, want := scope.Covers("discord", "824100000000000001"), true; got != want {
		t.Errorf("scope covers #eng = %v, want %v", got, want)
	}
	if got, want := scope.Covers("discord", "824100000000000002"), false; got != want {
		t.Errorf("scope covers #eng-infra = %v, want %v", got, want)
	}
	// Writing a source id on its own takes every container it ingests.
	if got, want := scope.Covers("github", "acme/infra"), true; got != want {
		t.Errorf("scope covers acme/infra = %v, want %v", got, want)
	}

	if got, ok := scope.TrackerItemID("1234"); !ok || got != "tracker:github:acme/api#1234" {
		t.Errorf("TrackerItemID(1234) = %q, %v, want tracker:github:acme/api#1234, true", got, ok)
	}

	// The scope's policy sets a ranking and the artifacts that ratify, and
	// inherits the principals from the `*` policy.
	policy := repo.Authority.ForScope("api")
	// The example's whole point as an override: this team decides in meetings,
	// so the scope inverts the two positions the default fixes.
	if !policy.Outranks(config.ArtifactMeeting, config.ArtifactMergedPR) {
		t.Error("a meeting does not outrank a merged PR in scope api, and the example says it does")
	}
	if !repo.Authority.Default().Outranks(config.ArtifactMergedPR, config.ArtifactMeeting) {
		t.Error("overriding one scope's ranking changed the default the other scopes inherit")
	}
	if !policy.RatifiedByArtifact(config.ArtifactSpec, "drive") {
		t.Error("a spec does not ratify in scope api, and the example says it does")
	}
	if !policy.RatifiedByPrincipal("kyle") || policy.RatifiedByPrincipal("shed") {
		t.Errorf("ratifiers in scope api = %v, want the inherited [kyle robin]", policy.RatifiedBy.Principals)
	}
	// The scope's ranking lists every class, so nothing in it is unranked: a
	// class that ratifies and does not rank is a contradiction the loader
	// refuses, and an example that reads as a model to copy has to be clear of
	// it.
	for _, c := range config.ArtifactClasses() {
		if _, ok := policy.Rank(c); !ok {
			t.Errorf("the example's ranking for scope api leaves out %s", c)
		}
	}

	agent, ok := repo.Principal("shed")
	if !ok || agent.Kind != principal.KindAgent || agent.Class != principal.ClassWorker {
		t.Errorf("principal shed = %+v, %v, want an agent of class worker", agent, ok)
	}
	human, ok := repo.Principal("kyle")
	if !ok || human.Kind != principal.KindHuman {
		t.Errorf("principal kyle = %+v, %v, want a human", human, ok)
	}
	// The example's team is a GitHub team, so the source holds the membership
	// and the mapping only names it.
	team, ok := repo.Principal("api-team")
	if !ok || team.Kind != principal.KindTeam || len(team.Members) != 0 || len(team.Identities) != 1 {
		t.Errorf("principal api-team = %+v, %v, want a team that claims a group", team, ok)
	}

	// Principals come out as the identity model, so the resolver the example
	// describes is the one it builds.
	resolver, err := repo.Resolver()
	if err != nil {
		t.Fatalf("Resolver: %v", err)
	}
	for _, tt := range []struct {
		hint connector.Identity
		want string
	}{
		{connector.Identity{Source: "github", Kind: connector.IdentityUser, NativeID: "MDQ6VXNlcjE="}, "kyle"},
		{connector.Identity{Source: "discord", Kind: connector.IdentityUser, Handle: "Robin"}, "robin"},
		{connector.Identity{Source: "drive", Kind: connector.IdentityUser, Email: "kyle@acme.example"}, "kyle"},
		{connector.Identity{Source: "github", Kind: connector.IdentityBot, Handle: "shed-agent[bot]"}, "shed"},
		{connector.Identity{Source: "github", Kind: connector.IdentityUser, NativeID: "MDQ6VGVhbTE="}, "api-team"},
	} {
		if got := resolver.Resolve(tt.hint); got.Status != principal.Resolved || got.Principal.ID != tt.want {
			t.Errorf("the example resolves %+v to %+v, want %s", tt.hint, got, tt.want)
		}
	}
	// An owner may be a team as well as a person.
	engine, ok := repo.CodeEntity("code:acme/api:engine/server")
	if !ok || !slices.Equal(engine.Owners, []string{"kyle", "api-team"}) {
		t.Errorf("the engine's owners = %v", engine.Owners)
	}

	// Sources come out as what the connector runtime consumes, allowlist and
	// all, with no second shape in between.
	allow := repo.Allowlist()
	if !allow.Allows("github", "acme/api") || allow.Allows("github", "acme/other") {
		t.Error("the ingest allowlist does not match the configured containers")
	}
	src, ok := repo.Source("drive")
	if !ok {
		t.Fatal("Source(drive) not found")
	}
	if src.Refresh.String() != "15m0s" {
		t.Errorf("drive refresh = %v, want 15m", src.Refresh)
	}
	if got, want := string(src.Settings), `{"recursive":true}`; got != want {
		t.Errorf("drive settings = %s, want %s", got, want)
	}
	if got, want := src.Secrets["credentials"], "HEARSAY_DRIVE_CREDENTIALS"; got != want {
		t.Errorf("drive credentials secret = %q, want %q", got, want)
	}
}

// loadExample loads the single-file example from the documentation.
func loadExample(t *testing.T) config.Repo {
	t.Helper()
	dir := writeFiles(t, filesUnder(t, docExamples(t), "single"))
	repo, err := config.Load(filepath.Join(dir, "hearsay.yaml"))
	if err != nil {
		t.Fatalf("Load(the example) = %v, want no error", err)
	}
	return repo
}

// docExamples extracts the example files from the schema documentation. Each is
// a fenced YAML block preceded by `<!-- example: <path> -->`.
func docExamples(t *testing.T) map[string]string {
	t.Helper()
	body, err := os.ReadFile(docsPath)
	if err != nil {
		t.Fatalf("reading %s: %v", docsPath, err)
	}

	const marker = "<!-- example: "
	files := map[string]string{}
	lines := strings.Split(string(body), "\n")
	for i := 0; i < len(lines); i++ {
		name, ok := strings.CutPrefix(lines[i], marker)
		if !ok {
			continue
		}
		name, _, ok = strings.Cut(name, " -->")
		if !ok {
			t.Fatalf("%s:%d: example marker is not closed", docsPath, i+1)
		}
		if i+1 >= len(lines) || !strings.HasPrefix(lines[i+1], "```yaml") {
			t.Fatalf("%s:%d: example %q is not followed by a yaml block", docsPath, i+1, name)
		}
		end := i + 2
		for end < len(lines) && !strings.HasPrefix(lines[end], "```") {
			end++
		}
		if end >= len(lines) {
			t.Fatalf("%s:%d: example %q has no closing fence", docsPath, i+1, name)
		}
		files[name] = strings.Join(lines[i+2:end], "\n") + "\n"
		i = end
	}
	if len(files) == 0 {
		t.Fatalf("%s holds no examples: look for %q", docsPath, marker)
	}
	return files
}

// filesUnder returns the examples under one prefix, with the prefix removed.
func filesUnder(t *testing.T, files map[string]string, prefix string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for name, body := range files {
		if rest, ok := strings.CutPrefix(name, prefix+"/"); ok {
			out[rest] = body
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s holds no examples under %s/", docsPath, prefix)
	}
	return out
}

// writeFiles writes the files into a temporary directory and returns it.
func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("creating %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatalf("writing %s: %v", full, err)
		}
	}
	return root
}

// normalize drops what identifies the bytes rather than the configuration, and
// sorts the objects, because which file an object was written in decides the
// order it is read in and nothing else.
func normalize(r config.Repo) config.Repo {
	r.Path = ""
	r.Digest = ""
	slices.SortFunc(r.Sources, func(a, b connector.SourceConfig) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(r.Scopes, func(a, b config.Scope) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(r.Principals, func(a, b principal.Principal) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(r.Code, func(a, b config.CodeEntity) int { return strings.Compare(a.ID, b.ID) })
	return r
}

func scopeIDs(r config.Repo) []string {
	ids := make([]string, 0, len(r.Scopes))
	for _, s := range r.Scopes {
		ids = append(ids, s.ID)
	}
	return ids
}
