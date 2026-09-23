package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/kpenfound/hearsay/internal/connector"
)

// Reader reads what seeding the entity map needs from a source's repositories:
// the names of the directories at the root of the default branch, and a file
// somebody named in configuration (a CODEOWNERS file). It is not a connector and
// emits nothing; the assertion worker builds one per GitHub source a `code/`
// entry names (docs/config.md#code).
//
// Hearsay holds no code, so this is the whole of what it reads from a
// repository's tree: the root listing, which carries names and types but no
// content, and the one file it is asked for. It keeps nothing it read.
type Reader struct {
	repos []string
	api   *client
}

// NewReader builds the reader for a source. It needs the source's settings and
// its token; the webhook secret is the connector's, and is not required here.
func NewReader(src connector.SourceConfig) (*Reader, error) {
	sc, err := parseSource(src)
	if err != nil {
		return nil, err
	}
	return &Reader{repos: sc.repos, api: sc.api}, nil
}

// entry is one item of a directory listing from the contents API. A listing
// carries no file content, only what each entry is.
type entry struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// file is the contents API's answer for a file.
type file struct {
	Type     string `json:"type"`
	Encoding string `json:"encoding"`
	Content  string `json:"content"`
}

// TopLevel returns the names of the directories at the root of a repository's
// default branch. The repository's metadata is read first, so a repository the
// token cannot see fails as the connector's walks do; an empty repository,
// which GitHub answers with a 404 for the listing, has none.
func (r *Reader) TopLevel(ctx context.Context, repo string) ([]string, error) {
	if !slices.Contains(r.repos, repo) {
		return nil, fmt.Errorf("repository %s is not one the source names", repo)
	}
	if _, err := r.api.repository(ctx, repo); err != nil {
		return nil, err
	}
	var entries []entry
	_, err := r.api.get(ctx, repoPath(repo)+"/contents", &entries)
	if serr := (*statusError)(nil); errors.As(err, &serr) && serr.status == http.StatusNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing the root of %s: %w", repo, err)
	}
	var dirs []string
	for _, e := range entries {
		if e.Type == "dir" {
			dirs = append(dirs, e.Name)
		}
	}
	slices.Sort(dirs)
	return dirs, nil
}

// ReadFile returns a file's content on a repository's default branch. A file
// that is not there, in a repository the token can read, is an error wrapping
// [fs.ErrNotExist]; anything but a regular file — a directory, a symlink, a
// submodule — is refused.
func (r *Reader) ReadFile(ctx context.Context, repo, path string) ([]byte, error) {
	if !slices.Contains(r.repos, repo) {
		return nil, fmt.Errorf("repository %s is not one the source names", repo)
	}
	rel, err := contentsPath(path)
	if err != nil {
		return nil, err
	}
	var raw json.RawMessage
	_, err = r.api.get(ctx, repoPath(repo)+"/contents/"+rel, &raw)
	if serr := (*statusError)(nil); errors.As(err, &serr) && serr.status == http.StatusNotFound {
		// GitHub answers 404 for a repository the token cannot see as well as
		// for a file that is not there, and only the second is a missing file.
		if _, err := r.api.repository(ctx, repo); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%s in %s: %w", path, repo, fs.ErrNotExist)
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s in %s: %w", path, repo, err)
	}
	if bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) {
		return nil, fmt.Errorf("%s in %s is a directory, not a file", path, repo)
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("decoding %s in %s: %w", path, repo, err)
	}
	if f.Type != "file" {
		return nil, fmt.Errorf("%s in %s is a %s, not a file", path, repo, f.Type)
	}
	if f.Encoding != "base64" {
		// GitHub answers "none" for a file over a megabyte, which no CODEOWNERS
		// file worth reading is.
		return nil, fmt.Errorf("%s in %s came back with encoding %q, not base64: the contents API serves files of up to 1 MB", path, repo, f.Encoding)
	}
	content, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(f.Content, "\n", ""))
	if err != nil {
		return nil, fmt.Errorf("decoding %s in %s: %w", path, repo, err)
	}
	return content, nil
}

// contentsPath is a repository-relative path as the contents API takes it, one
// escaped segment at a time. A path that could climb out of the repository or
// names nothing is refused rather than sent.
func contentsPath(path string) (string, error) {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("%q is not a path inside the repository", path)
		}
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/"), nil
}
