package github

import (
	"context"
	"encoding/json"
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

// perPage is the page size of every list call: GitHub's largest.
const perPage = "100"

// The steps a backfill walks each repository through, in order. Issues come
// before their comments and pull requests before their reviews and review
// comments, so a walk that stops early has the things comments hang off.
const (
	stepIssues         = "issues"
	stepPulls          = "pulls"
	stepIssueComments  = "issue_comments"
	stepReviewComments = "review_comments"
	stepCommits        = "commits"
)

var steps = []string{stepIssues, stepPulls, stepIssueComments, stepReviewComments, stepCommits}

// position is a backfill cursor: the repository and step being walked, and the
// next page of it as a path relative to the API, which is GitHub's own page
// token. An empty Next is the step's first page.
type position struct {
	Repo string `json:"repo"`
	Step string `json:"step"`
	Next string `json:"next,omitempty"`
}

// Backfill implements [connector.Backfiller]: one page of one step of one
// repository per call.
func (c *Connector) Backfill(ctx context.Context, sink connector.Sink, from connector.Cursor) (connector.BackfillResult, error) {
	pos, err := c.resume(from)
	if err != nil {
		return connector.BackfillResult{}, err
	}
	if pos == nil {
		return connector.BackfillResult{Done: true}, nil
	}
	n, next, err := c.page(ctx, sink, *pos)
	if err != nil {
		return connector.BackfillResult{}, err
	}
	if next == nil {
		return connector.BackfillResult{Done: true, Events: n}, nil
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return connector.BackfillResult{}, fmt.Errorf("encoding the backfill cursor: %w", err)
	}
	if len(raw) > connector.MaxCursorLen {
		return connector.BackfillResult{}, fmt.Errorf("backfill cursor for %s is %d bytes, over the %d a cursor may be", next.Repo, len(raw), connector.MaxCursorLen)
	}
	return connector.BackfillResult{Next: connector.Cursor(raw), Events: n}, nil
}

// resume decodes a cursor into where to walk next, or nil when there is nothing
// left. A cursor naming a repository config no longer lists resumes at the first
// configured repository after it, so removing one from config neither restarts
// the walk nor strands it.
func (c *Connector) resume(from connector.Cursor) (*position, error) {
	if from == "" {
		return &position{Repo: c.repos[0], Step: steps[0]}, nil
	}
	var pos position
	if err := json.Unmarshal([]byte(from), &pos); err != nil {
		return nil, fmt.Errorf("backfill cursor is not one the github connector wrote: %w", err)
	}
	if !slices.Contains(steps, pos.Step) {
		return nil, fmt.Errorf("backfill cursor names step %q, which the github connector does not have", pos.Step)
	}
	if slices.Contains(c.repos, pos.Repo) {
		return &pos, nil
	}
	for _, r := range c.repos {
		if r > pos.Repo {
			return &position{Repo: r, Step: steps[0]}, nil
		}
	}
	return nil, nil
}

// advance is the position after a step that has no more pages: the next step,
// the next repository's first, or nil at the end of history.
func (c *Connector) advance(pos position) *position {
	if i := slices.Index(steps, pos.Step); i+1 < len(steps) {
		return &position{Repo: pos.Repo, Step: steps[i+1]}
	}
	if i := slices.Index(c.repos, pos.Repo); i >= 0 && i+1 < len(c.repos) {
		return &position{Repo: c.repos[i+1], Step: steps[0]}
	}
	return nil
}

// page walks one page at pos and emits what is on it, returning how many
// events it emitted and the position after it. The repository is read first,
// on every page, because its visibility is what the events' ACL and revision
// tokens are made of and it is read at the time of the read.
func (c *Connector) page(ctx context.Context, sink connector.Sink, pos position) (int, *position, error) {
	var repo repository
	if _, err := c.api.get(ctx, repoPath(pos.Repo), &repo); err != nil {
		return 0, nil, fmt.Errorf("reading repository %s: %w", pos.Repo, err)
	}
	v := view{source: c.source, repo: pos.Repo, private: repo.Private}

	rel := pos.Next
	var (
		n    int
		next string
		err  error
	)
	switch pos.Step {
	case stepIssues:
		if rel == "" {
			rel = c.list(pos.Repo, "/issues", url.Values{"state": {"all"}}, true)
		}
		n, next, err = c.issues(ctx, sink, v, rel)
	case stepPulls:
		if rel == "" {
			// The pulls list takes no `since`; the page is filtered instead.
			rel = c.list(pos.Repo, "/pulls", url.Values{"state": {"all"}}, false)
		}
		n, next, err = c.pulls(ctx, sink, v, rel)
	case stepIssueComments:
		if rel == "" {
			rel = c.list(pos.Repo, "/issues/comments", url.Values{}, true)
		}
		n, next, err = c.comments(ctx, sink, v, rel, v.commentEvent)
	case stepReviewComments:
		if rel == "" {
			rel = c.list(pos.Repo, "/pulls/comments", url.Values{}, true)
		}
		n, next, err = c.comments(ctx, sink, v, rel, v.reviewCommentEvent)
	case stepCommits:
		if rel == "" {
			q := url.Values{"sha": {repo.DefaultBranch}, "per_page": {perPage}}
			if !c.since.IsZero() {
				q.Set("since", stamp(c.since))
			}
			rel = repoPath(pos.Repo) + "/commits?" + q.Encode()
		}
		n, next, err = c.commits(ctx, sink, v, rel)
	}
	if err != nil {
		return n, nil, fmt.Errorf("backfilling %s of %s: %w", pos.Step, pos.Repo, err)
	}
	if next != "" {
		return n, &position{Repo: pos.Repo, Step: pos.Step, Next: next}, nil
	}
	return n, c.advance(pos), nil
}

// list is the first page of a list endpoint, oldest change first, so that what
// changes during a walk moves to a page not yet read rather than one already
// read. takesSince is whether the endpoint filters by the start date itself.
func (c *Connector) list(repo, endpoint string, q url.Values, takesSince bool) string {
	q.Set("sort", "updated")
	q.Set("direction", "asc")
	q.Set("per_page", perPage)
	if takesSince && !c.since.IsZero() {
		q.Set("since", stamp(c.since))
	}
	return repoPath(repo) + endpoint + "?" + q.Encode()
}

// before reports whether something last updated at t is older than the
// configured start date. The list endpoints that take `since` apply it
// themselves; the pulls list does not, so every step checks.
func (c *Connector) before(t time.Time) bool { return !c.since.IsZero() && t.Before(c.since) }

func (c *Connector) issues(ctx context.Context, sink connector.Sink, v view, rel string) (int, string, error) {
	var page []issue
	next, err := c.api.get(ctx, rel, &page)
	if err != nil {
		return 0, "", err
	}
	n := 0
	for _, is := range page {
		if isPull(is.PullRequest) || c.before(is.UpdatedAt) {
			continue
		}
		ev, err := v.issueEvent(is)
		if err != nil {
			return n, "", err
		}
		if err := c.emit(ctx, sink, ev); err != nil {
			return n, "", err
		}
		n++
	}
	return n, next, nil
}

func isPull(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}

// pulls emits a page of pull requests and, for each, every submitted review:
// GitHub lists reviews per pull request and not per repository.
func (c *Connector) pulls(ctx context.Context, sink connector.Sink, v view, rel string) (int, string, error) {
	var page []pull
	next, err := c.api.get(ctx, rel, &page)
	if err != nil {
		return 0, "", err
	}
	n := 0
	for _, p := range page {
		if c.before(p.UpdatedAt) {
			continue
		}
		ev, err := v.pullEvent(p)
		if err != nil {
			return n, "", err
		}
		if err := c.emit(ctx, sink, ev); err != nil {
			return n, "", err
		}
		n++
		reviews, err := c.reviews(ctx, sink, v, p.Number)
		n += reviews
		if err != nil {
			return n, "", err
		}
	}
	return n, next, nil
}

func (c *Connector) reviews(ctx context.Context, sink connector.Sink, v view, number int) (int, error) {
	rel := repoPath(v.repo) + "/pulls/" + strconv.Itoa(number) + "/reviews?per_page=" + perPage
	n := 0
	for rel != "" {
		var page []review
		next, err := c.api.get(ctx, rel, &page)
		if err != nil {
			return n, err
		}
		for _, r := range page {
			if !r.submitted() {
				continue
			}
			ev, err := v.reviewEvent(number, r)
			if err != nil {
				return n, err
			}
			if err := c.emit(ctx, sink, ev); err != nil {
				return n, err
			}
			n++
		}
		rel = next
	}
	return n, nil
}

func (c *Connector) comments(ctx context.Context, sink connector.Sink, v view, rel string, build func(comment) (connector.Event, error)) (int, string, error) {
	var page []comment
	next, err := c.api.get(ctx, rel, &page)
	if err != nil {
		return 0, "", err
	}
	n := 0
	for _, cm := range page {
		if c.before(cm.UpdatedAt) {
			continue
		}
		ev, err := build(cm)
		if err != nil {
			return n, "", err
		}
		if err := c.emit(ctx, sink, ev); err != nil {
			return n, "", err
		}
		n++
	}
	return n, next, nil
}

func (c *Connector) commits(ctx context.Context, sink connector.Sink, v view, rel string) (int, string, error) {
	var page []commit
	next, err := c.api.get(ctx, rel, &page)
	if serr := (*statusError)(nil); errors.As(err, &serr) && serr.status == http.StatusConflict {
		// GitHub's answer for a repository with no commits yet.
		return 0, "", nil
	}
	if err != nil {
		return 0, "", err
	}
	n := 0
	for _, cm := range page {
		ev, err := v.commitEvent(cm)
		if err != nil {
			return n, "", err
		}
		if err := c.emit(ctx, sink, ev); err != nil {
			return n, "", err
		}
		n++
	}
	return n, next, nil
}

// repoPath is a repository's REST path.
func repoPath(fullName string) string {
	owner, name, _ := strings.Cut(fullName, "/")
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
}
