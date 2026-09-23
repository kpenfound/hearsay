package github_test

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/connector/github"
)

// wantCodeOwners is testdata/api/contents-codeowners.json's content, decoded.
const wantCodeOwners = `# Owners of acme/api. The last matching rule wins.
*                 @kpenfound
/engine/          @samr
/engine/server/   @samr @acme/api-team
/docs/            @nobody-mapped
`

func newReader(t *testing.T, gh *fakeGitHub) *github.Reader {
	t.Helper()
	src := newSource(t, gh, sourceID, nil)
	delete(src.Secrets, github.SecretWebhook)
	r, err := github.NewReader(src)
	if err != nil {
		t.Fatalf("NewReader = %v", err)
	}
	return r
}

// Seeding reads the repository's metadata, its root listing — names and types,
// no content — and the one file configuration names, and nothing else.
func TestReaderReadsTheLayoutAndTheFileAskedFor(t *testing.T) {
	gh := newFakeGitHub(t)
	r := newReader(t, gh)

	dirs, err := r.TopLevel(t.Context(), "acme/api")
	if err != nil {
		t.Fatalf("TopLevel = %v", err)
	}
	// Directories only: not the files at the root, and not the submodule.
	if want := []string{".github", "cmd", "docs", "engine"}; !slices.Equal(dirs, want) {
		t.Errorf("TopLevel = %q, want %q", dirs, want)
	}
	content, err := r.ReadFile(t.Context(), "acme/api", ".github/CODEOWNERS")
	if err != nil {
		t.Fatalf("ReadFile = %v", err)
	}
	if string(content) != wantCodeOwners {
		t.Errorf("ReadFile = %q, want %q", content, wantCodeOwners)
	}

	want := []string{"/repos/acme/api", "/repos/acme/api/contents", "/repos/acme/api/contents/.github/CODEOWNERS"}
	if got := gh.seen(); !slices.Equal(got, want) {
		t.Errorf("requests = %q, want %q: the metadata, the root listing and the one file", got, want)
	}
}

func TestReaderFailures(t *testing.T) {
	tests := []struct {
		name string
		// arrange changes the fake before the call.
		arrange func(gh *fakeGitHub)
		call    func(ctx context.Context, r *github.Reader) error
		// wantErr is part of the error; notExist is whether it is a missing file.
		wantErr  string
		notExist bool
		// wantRequests is every request made, in order.
		wantRequests []string
	}{
		{
			name:         "a file that is not there is fs.ErrNotExist",
			arrange:      func(gh *fakeGitHub) { gh.setContents("/repos/acme/api/contents/.github/CODEOWNERS", nil) },
			call:         readCodeOwners,
			wantErr:      ".github/CODEOWNERS in acme/api",
			notExist:     true,
			wantRequests: []string{"/repos/acme/api/contents/.github/CODEOWNERS", "/repos/acme/api"},
		},
		{
			// The connector's own error for a repository it cannot read, the
			// one a backfill or a visibility check fails with.
			name:         "a repository the token cannot read fails the layout",
			arrange:      func(gh *fakeGitHub) { gh.fail("/repos/acme/api", http.StatusNotFound) },
			call:         topLevel,
			wantErr:      "reading repository acme/api: GET /repos/acme/api: GitHub answered 404 Not Found",
			wantRequests: []string{"/repos/acme/api"},
		},
		{
			name: "and is not mistaken for a missing file",
			arrange: func(gh *fakeGitHub) {
				gh.fail("/repos/acme/api", http.StatusNotFound)
				gh.fail("/repos/acme/api/contents/.github/CODEOWNERS", http.StatusNotFound)
			},
			call:         readCodeOwners,
			wantErr:      "reading repository acme/api: GET /repos/acme/api: GitHub answered 404 Not Found",
			wantRequests: []string{"/repos/acme/api/contents/.github/CODEOWNERS", "/repos/acme/api"},
		},
		{
			name:         "a file GitHub will not serve is an error",
			arrange:      func(gh *fakeGitHub) { gh.fail("/repos/acme/api/contents/.github/CODEOWNERS", http.StatusForbidden) },
			call:         readCodeOwners,
			wantErr:      "reading .github/CODEOWNERS in acme/api: GET /repos/acme/api/contents/.github/CODEOWNERS: GitHub answered 403",
			wantRequests: []string{"/repos/acme/api/contents/.github/CODEOWNERS"},
		},
		{
			name:         "a repository the source does not name is refused without a call",
			call:         func(ctx context.Context, r *github.Reader) error { _, err := r.TopLevel(ctx, "acme/other"); return err },
			wantErr:      "repository acme/other is not one the source names",
			wantRequests: []string{},
		},
		{
			name: "so is reading a file from one",
			call: func(ctx context.Context, r *github.Reader) error {
				_, err := r.ReadFile(ctx, "acme/other", ".github/CODEOWNERS")
				return err
			},
			wantErr:      "repository acme/other is not one the source names",
			wantRequests: []string{},
		},
		{
			name: "a path out of the repository is refused without a call",
			call: func(ctx context.Context, r *github.Reader) error {
				_, err := r.ReadFile(ctx, "acme/api", "../x")
				return err
			},
			wantErr:      `"../x" is not a path inside the repository`,
			wantRequests: []string{},
		},
		{
			name:    "a directory is not read as a file",
			arrange: func(gh *fakeGitHub) { gh.setContents("/repos/acme/api/contents/docs", []byte(`[]`)) },
			call: func(ctx context.Context, r *github.Reader) error {
				_, err := r.ReadFile(ctx, "acme/api", "docs")
				return err
			},
			wantErr:      "docs in acme/api is a directory",
			wantRequests: []string{"/repos/acme/api/contents/docs"},
		},
		{
			name: "a file too large for the contents API is an error",
			arrange: func(gh *fakeGitHub) {
				gh.setContents("/repos/acme/api/contents/.github/CODEOWNERS", []byte(`{"type":"file","encoding":"none","content":""}`))
			},
			call:         readCodeOwners,
			wantErr:      `encoding "none"`,
			wantRequests: []string{"/repos/acme/api/contents/.github/CODEOWNERS"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gh := newFakeGitHub(t)
			r := newReader(t, gh)
			if tt.arrange != nil {
				tt.arrange(gh)
			}
			err := tt.call(t.Context(), r)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tt.wantErr)
			}
			if got := errors.Is(err, fs.ErrNotExist); got != tt.notExist {
				t.Errorf("errors.Is(%v, fs.ErrNotExist) = %t, want %t", err, got, tt.notExist)
			}
			if got := gh.seen(); !slices.Equal(got, tt.wantRequests) {
				t.Errorf("requests = %q, want %q", got, tt.wantRequests)
			}
		})
	}
}

func TestReaderOfAnEmptyRepositoryListsNothing(t *testing.T) {
	gh := newFakeGitHub(t)
	r := newReader(t, gh)
	// GitHub answers the root listing of a repository with no commits with a
	// 404, and the repository itself with its metadata.
	gh.setContents("/repos/acme/api/contents", nil)
	dirs, err := r.TopLevel(t.Context(), "acme/api")
	if err != nil || len(dirs) != 0 {
		t.Errorf("TopLevel = %q, %v, want nothing and no error", dirs, err)
	}
}

func TestNewReader(t *testing.T) {
	gh := newFakeGitHub(t)
	tests := []struct {
		name    string
		secrets map[string]string
		wantErr string
	}{
		{"the token is enough", map[string]string{github.SecretToken: testToken}, ""},
		{"the webhook secret is allowed", map[string]string{github.SecretToken: testToken, github.SecretWebhook: hookSecret}, ""},
		{"the token is required", map[string]string{github.SecretWebhook: hookSecret}, `secret "token" is required`},
		{"a secret the connector does not read is refused", map[string]string{github.SecretToken: testToken, "extra": "x"}, `secret "extra" is not one`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := newSource(t, gh, sourceID, nil)
			src.Secrets = tt.secrets
			_, err := github.NewReader(src)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("NewReader = %v, want no error", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("NewReader = %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}

func readCodeOwners(ctx context.Context, r *github.Reader) error {
	_, err := r.ReadFile(ctx, "acme/api", ".github/CODEOWNERS")
	return err
}

func topLevel(ctx context.Context, r *github.Reader) error {
	_, err := r.TopLevel(ctx, "acme/api")
	return err
}
