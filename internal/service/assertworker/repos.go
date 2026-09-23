package assertworker

import (
	"context"
	"errors"
	"fmt"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/l2"
)

// RepoSource reads the repositories of one source, by project: the GitHub
// connector's reader is one (internal/connector/github). Which source types
// have one is the binary's wiring, the way the connector registry is.
type RepoSource interface {
	// ReadFile returns a file's content at HEAD, or an error wrapping
	// fs.ErrNotExist when it is not there.
	ReadFile(ctx context.Context, project, path string) ([]byte, error)
	// TopLevel returns the names of the directories at the root, at HEAD.
	TopLevel(ctx context.Context, project string) ([]string, error)
}

// Repos is the [l2.RepoReader] seeding uses: a [RepoSource] per source id. A
// repository in a source with none is [errors.ErrUnsupported], which
// [l2.Seed] logs and seeds from configuration alone.
type Repos map[string]RepoSource

var _ l2.RepoReader = Repos(nil)

// ReadFile implements [l2.RepoReader].
func (r Repos) ReadFile(ctx context.Context, repo config.SourceRef, path string) ([]byte, error) {
	src, err := r.source(repo)
	if err != nil {
		return nil, err
	}
	return src.ReadFile(ctx, repo.Project, path)
}

// TopLevel implements [l2.RepoReader].
func (r Repos) TopLevel(ctx context.Context, repo config.SourceRef) ([]string, error) {
	src, err := r.source(repo)
	if err != nil {
		return nil, err
	}
	return src.TopLevel(ctx, repo.Project)
}

func (r Repos) source(repo config.SourceRef) (RepoSource, error) {
	src, ok := r[repo.Source]
	if !ok || src == nil {
		return nil, fmt.Errorf("reading %s in source %s: %w", repo.Project, repo.Source, errors.ErrUnsupported)
	}
	return src, nil
}
