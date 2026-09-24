package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
)

// maxReplyPages bounds how far [Replier.Reply] reads an issue's comments
// looking for a reply it already posted: a hundred comments a page, written
// after the command.
const maxReplyPages = 10

// ErrReplyGone is a reply that is no longer there to revise: somebody deleted
// it.
var ErrReplyGone = errors.New("the reply comment is gone")

// Replier writes Hearsay's answer to a `/hearsay` command: one comment on the
// issue or pull request the command was written on, and edits to that comment
// and no other (ADR-0022). It is not the connector, which never writes to
// GitHub; the assertion worker builds one per GitHub source, from the same
// configuration, and it needs the token and not the webhook secret. The token
// has to be allowed to write issue and pull request comments.
//
// Every reply ends with [ReplyMarker] for the command it answers, so the
// connector reads its webhook echo as a [KindReply] and not as team content
// or a command, and a reply posted by a run that died before recording it is
// found rather than posted twice.
type Replier struct {
	repos []string
	api   *client
}

// NewReplier builds the replier for a source. A read-only source has none:
// Hearsay posts nothing to it.
func NewReplier(src connector.SourceConfig) (*Replier, error) {
	if src.ReadOnly {
		return nil, fmt.Errorf("source %q is read_only: Hearsay writes nothing to it", src.ID)
	}
	sc, err := parseSource(src)
	if err != nil {
		return nil, err
	}
	return &Replier{repos: sc.repos, api: sc.api}, nil
}

type postedComment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
}

// Reply answers command comment command, written on issue or pull request
// issue of repo at since, with body, and returns the reply's comment id. A
// reply to that command already on the issue is returned and nothing is
// posted.
func (r *Replier) Reply(ctx context.Context, repo string, issue int, command int64, since time.Time, body string) (int64, error) {
	if !slices.Contains(r.repos, repo) {
		return 0, fmt.Errorf("repository %s is not one the source names", repo)
	}
	path := repoPath(repo) + "/issues/" + strconv.Itoa(issue) + "/comments"
	marker := ReplyMarker(command)
	// A minute's slack either side of the command's own clock: the reply was
	// written after it.
	q := url.Values{"since": {since.Add(-time.Minute).UTC().Format(time.RFC3339)}, "per_page": {"100"}}
	for page := 1; page <= maxReplyPages; page++ {
		q.Set("page", strconv.Itoa(page))
		var comments []postedComment
		more, err := r.api.get(ctx, path+"?"+q.Encode(), &comments)
		if err != nil {
			return 0, fmt.Errorf("looking for the reply to comment %d on %s#%d: %w", command, repo, issue, err)
		}
		for _, c := range comments {
			if strings.Contains(c.Body, marker) {
				return c.ID, nil
			}
		}
		if !more {
			break
		}
	}
	var posted postedComment
	if err := r.api.send(ctx, http.MethodPost, path, map[string]string{"body": withMarker(body, command)}, &posted); err != nil {
		return 0, fmt.Errorf("replying to comment %d on %s#%d: %w", command, repo, issue, err)
	}
	if posted.ID <= 0 {
		return 0, fmt.Errorf("replying to comment %d on %s#%d: GitHub returned no comment id", command, repo, issue)
	}
	return posted.ID, nil
}

// Revise replaces the text of reply, Hearsay's reply to command comment
// command in repo, with body. A reply somebody deleted is [ErrReplyGone].
func (r *Replier) Revise(ctx context.Context, repo string, command, reply int64, body string) error {
	if !slices.Contains(r.repos, repo) {
		return fmt.Errorf("repository %s is not one the source names", repo)
	}
	path := repoPath(repo) + "/issues/comments/" + strconv.FormatInt(reply, 10)
	var posted postedComment
	err := r.api.send(ctx, http.MethodPatch, path, map[string]string{"body": withMarker(body, command)}, &posted)
	if serr := (*statusError)(nil); errors.As(err, &serr) && (serr.status == http.StatusNotFound || serr.status == http.StatusGone) {
		return fmt.Errorf("revising reply %d in %s: %w", reply, repo, ErrReplyGone)
	}
	if err != nil {
		return fmt.Errorf("revising reply %d in %s: %w", reply, repo, err)
	}
	return nil
}

func withMarker(body string, command int64) string {
	return strings.TrimRight(body, "\n") + "\n\n" + ReplyMarker(command)
}
