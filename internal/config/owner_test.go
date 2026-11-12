package config_test

import (
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/config"
)

// An owner source is a second work source whose owner_of names the work source
// it holds documents and decisions for. Validation refuses the pairings that
// would make no sense, whichever file declares which source first.
func TestOwnerSourceSetting(t *testing.T) {
	work := "id: projects\ntype: work\ncontainers: ['*']\n"
	owner := "id: owner\ntype: work\ncontainers: ['*']\nsettings:\n  owner_of: projects\n"
	tests := []struct {
		name  string
		files map[string]string
		want  string // "" is accepted; otherwise a substring of the one problem
	}{
		{name: "a pair", files: map[string]string{"sources/projects.yaml": work, "sources/owner.yaml": owner}},
		{name: "declared in the other order", files: map[string]string{"sources/a-owner.yaml": owner, "sources/z-projects.yaml": work}},
		{name: "names itself", files: map[string]string{
			"sources/owner.yaml": "id: owner\ntype: work\ncontainers: ['*']\nsettings:\n  owner_of: owner\n"},
			want: "names this source itself"},
		{name: "names a source that is not a work source", files: map[string]string{
			"sources/owner.yaml": owner, "sources/projects.yaml": "id: projects\ntype: github\ncontainers: [acme/api]\n"},
			want: `which is of type "github"`},
		{name: "names another owner source", files: map[string]string{
			"sources/projects.yaml": work, "sources/owner.yaml": owner,
			"sources/second.yaml": "id: second\ntype: work\ncontainers: ['*']\nsettings:\n  owner_of: owner\n"},
			want: "which is itself an owner source"},
		{name: "names a source that is not configured", files: map[string]string{
			"sources/owner.yaml": owner},
			want: `names source "projects", which is not configured`},
		{name: "is not a source id", files: map[string]string{
			"sources/projects.yaml": work,
			"sources/owner.yaml":    "id: owner\ntype: work\ncontainers: ['*']\nsettings:\n  owner_of: 3\n"},
			want: "is not a source id"},
		{name: "is empty", files: map[string]string{
			"sources/projects.yaml": work,
			"sources/owner.yaml":    "id: owner\ntype: work\ncontainers: ['*']\nsettings:\n  owner_of: ''\n"},
			want: "is not a source id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := map[string]string{"scopes/work.yaml": "id: work\nsources: [" + firstSource(tt.files) + "]\n"}
			for name, body := range tt.files {
				files[name] = body
			}
			_, err := config.Load(writeFiles(t, files))
			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("Load = %v, want no error", err)
			case tt.want != "" && err == nil:
				t.Fatalf("Load = nil, want an error containing %q", tt.want)
			case tt.want != "" && !strings.Contains(err.Error(), tt.want):
				t.Fatalf("Load = %v, want an error containing %q", err, tt.want)
			case tt.want != "" && strings.Count(err.Error(), "settings.owner_of") != 1:
				t.Fatalf("Load = %v, want the one mistake reported once", err)
			}
		})
	}
}

func firstSource(files map[string]string) string {
	for _, body := range files {
		if _, rest, ok := strings.Cut(body, "id: "); ok {
			id, _, _ := strings.Cut(rest, "\n")
			return id
		}
	}
	return ""
}
