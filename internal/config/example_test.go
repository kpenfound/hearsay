package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/config/configtest"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/principal"
)

const docsPath = "../../docs/config.md"

func TestDocsExampleLoadsBothWays(t *testing.T) {
	examples := docExamples(t)
	single := configtest.Write(t, filesUnder(t, examples, "single"))
	dir := configtest.Write(t, filesUnder(t, examples, "dir"))

	fromFile, err := config.Load(filepath.Join(single, "hearsay.yaml"))
	if err != nil {
		t.Fatalf("Load(the single-file example) = %v, want no error", err)
	}
	fromDir, err := config.Load(dir)
	if err != nil {
		t.Fatalf("Load(the directory example) = %v, want no error", err)
	}

	fromDirWithFile, err := config.Load(single)
	if err != nil {
		t.Fatalf("Load(the directory holding the single file) = %v, want no error", err)
	}
	if fromDirWithFile.Digest != fromFile.Digest {
		t.Errorf("digest via the directory = %s, via the file = %s, want the same file to give the same digest",
			fromDirWithFile.Digest, fromFile.Digest)
	}

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

func TestDocsClaudeSbxExampleLoads(t *testing.T) {
	root := configtest.Write(t, filesUnder(t, docExamples(t), "claude-sbx"))
	repo, err := config.Load(filepath.Join(root, "hearsay.yaml"))
	if err != nil {
		t.Fatalf("Load(the claude-sbx example) = %v, want no error", err)
	}
	for _, tier := range llm.CompletionTiers {
		tc, ok := repo.LLM.Tier(tier)
		if !ok {
			t.Fatalf("the %s tier is not configured", tier)
		}
		if tc.Provider != llm.ProviderClaudeSbx || tc.Model == "" {
			t.Errorf("the %s tier = %+v, want claude-sbx and the model the example names", tier, tc)
		}
		if tc.Temperature != nil || tc.BaseURL != "" || tc.APIKeyEnv != "" {
			t.Errorf("the %s tier = %+v, want none of the settings claude-sbx refuses", tier, tc)
		}
	}
	distill, _ := repo.LLM.Tier(llm.TierDistill)
	if distill.MaxTokens != 4096 {
		t.Errorf("distill max_tokens = %d, want the 4096 the example writes", distill.MaxTokens)
	}
}

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

func normalize(r config.Repo) config.Repo {
	r.Path = ""
	r.Digest = ""
	slices.SortFunc(r.Sources, func(a, b connector.SourceConfig) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(r.Scopes, func(a, b config.Scope) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(r.Principals, func(a, b principal.Principal) int { return strings.Compare(a.ID, b.ID) })
	slices.SortFunc(r.Code, func(a, b config.CodeEntity) int { return strings.Compare(a.ID, b.ID) })
	return r
}
