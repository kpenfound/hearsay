package onboard_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/onboard"
)

// zeros stands in for crypto/rand.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func load(t *testing.T, body []byte) config.Repo {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hearsay.yaml")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	repo, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load = %v\n%s", err, body)
	}
	return repo
}

// Names the generated file derives — a scope per repository, a token
// variable per principal — never collide, and always load.
func TestGeneratedNames(t *testing.T) {
	tests := []struct {
		name       string
		in         onboard.Input
		wantScopes []string
		wantTokens []string
	}{
		{
			name:       "a scope is the repository's name",
			in:         onboard.Input{Operator: onboard.Operator{ID: "kyle", GitHub: "kpenfound"}, GitHub: onboard.GitHub{Repos: []string{"acme/api", "acme/Web.App"}}},
			wantScopes: []string{"api", "web-app"},
			wantTokens: []string{"HEARSAY_KYLE_TOKEN"},
		},
		{
			name:       "or owner-name, where two repositories share a name",
			in:         onboard.Input{Operator: onboard.Operator{ID: "kyle", GitHub: "kpenfound"}, GitHub: onboard.GitHub{Repos: []string{"acme/api", "other/api", "acme/infra"}}},
			wantScopes: []string{"acme-api", "other-api", "infra"},
			wantTokens: []string{"HEARSAY_KYLE_TOKEN"},
		},
		{
			name:       "a name that is punctuation alone",
			in:         onboard.Input{Operator: onboard.Operator{ID: "kyle", GitHub: "kpenfound"}, GitHub: onboard.GitHub{Repos: []string{"acme/..x", "acme/-"}}},
			wantScopes: []string{"x", "acme"},
			wantTokens: []string{"HEARSAY_KYLE_TOKEN"},
		},
		{
			// The operator's token variable would be the GitHub source's.
			name:       "a token variable a source already names",
			in:         onboard.Input{Operator: onboard.Operator{ID: "github", GitHub: "kpenfound"}, GitHub: onboard.GitHub{Repos: []string{"acme/api"}}},
			wantScopes: []string{"api"},
			wantTokens: []string{"HEARSAY_GITHUB_2_TOKEN"},
		},
		{
			name:       "an id with a dash",
			in:         onboard.Input{Operator: onboard.Operator{ID: "kyle-p", Email: "kyle@acme.example"}, Drive: onboard.Drive{Folders: []string{"f1"}}},
			wantScopes: []string{onboard.TeamScope},
			wantTokens: []string{"HEARSAY_KYLE_P_TOKEN"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := onboard.Generate(t.Context(), tt.in, onboard.Directory{}, zeros{})
			if err != nil {
				t.Fatalf("Generate = %v", err)
			}
			repo := load(t, res.Config)
			var scopes []string
			for _, s := range repo.Scopes {
				scopes = append(scopes, s.ID)
			}
			if !slices.Equal(scopes, tt.wantScopes) {
				t.Errorf("scopes = %q, want %q", scopes, tt.wantScopes)
			}
			if !slices.Equal(res.Tokens, tt.wantTokens) {
				t.Errorf("tokens = %q, want %q", res.Tokens, tt.wantTokens)
			}
			for _, tok := range res.Tokens {
				if slices.Contains(res.Empty, tok) || !strings.Contains(string(res.Env), "\n"+tok+"=") {
					t.Errorf("token variable %s is not one of its own in the env file:\n%s", tok, res.Env)
				}
			}
		})
	}
}
