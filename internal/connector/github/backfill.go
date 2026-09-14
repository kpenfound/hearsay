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

// perPage is the page size of every list call: GitHub's largest. It is a
// variable so that a test can walk pages of a few items.
var perPage = 100

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

// position is a backfill cursor: the repository and step being walked, and
// where in the step the next page starts. What "where" is depends on the step;
// see [Connector.page]. It holds no URL, so a cursor carries no host and the
// token only ever goes to the configured API.
type position struct {
	Repo string `json:"repo"`
	Step string `json:"step"`
	// Since is the updated_at the next page starts from, on the steps walked
	// by it. Empty is the start date.
	Since string `json:"since,omitempty"`
	// Page is the page number the next call reads, within Since or within a
	// fixed ordering. Zero is the first.
	Page int `json:"page,omitempty"`
	// Head is the commit a walk of the default branch is pinned to, once its
	// first page is read.
	Head string `json:"head,omitempty"`
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
	n, next, err := c.page(ctx, sink, *pos, c.since)
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
	if pos.Page < 0 {
		return nil, fmt.Errorf("backfill cursor names page %d", pos.Page)
	}
	if _, err := parseStamp(pos.Since); err != nil {
		return nil, fmt.Errorf("backfill cursor: %w", err)
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

// parseStamp reads a position's Since; empty is the zero time.
func parseStamp(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("since %q is not an RFC 3339 timestamp", s)
	}
	return t, nil
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
// events it emitted and the position after it. floor is the start date: the
// configured one for a backfill, and none for a re-sync, which has to reach
// everything a webhook may have emitted whatever its date.
//
// The repository is read first, on every page, because its visibility is what
// the events' ACL and revision tokens are made of.
//
// No step pages by offset into an ordering that can change while it walks: an
// item moving out of a page already read would shift its neighbours back, and
// the one at the boundary would never be read — nor, if it never changes, sent
// by a webhook. Instead:
//
//   - issues and both comment lists are walked by updated_at, ascending, and
//     each page starts one second before the last updated_at of the page before
//     it. What changes moves ahead of the walk; what does not stays where it
//     was. The overlap re-emits the tail of a page, which is free, and makes the
//     walk independent of whether GitHub's `since` is inclusive.
//   - pull requests, whose list takes no `since`, are walked in creation order,
//     which nothing changes: a pull request cannot be deleted.
//   - commits are walked from the commit the default branch pointed at when the
//     walk began, whose history cannot change.
func (c *Connector) page(ctx context.Context, sink connector.Sink, pos position, floor time.Time) (int, *position, error) {
	var repo repository
	if _, err := c.api.get(ctx, repoPath(pos.Repo), &repo); err != nil {
		return 0, nil, fmt.Errorf("reading repository %s: %w", pos.Repo, err)
	}
	v := view{source: c.source, repo: pos.Repo, private: repo.Private}

	var (
		n    int
		next *position
		err  error
	)
	switch pos.Step {
	case stepIssues:
		n, next, err = walkUpdated(ctx, c, sink, pos, floor, "/issues", url.Values{"state": {"all"}},
			func(is issue) time.Time { return is.UpdatedAt },
			func(is issue) (*connector.Event, error) {
				if isPull(is.PullRequest) {
					// Read from the pulls list, which has what an issue lacks.
					return nil, nil
				}
				ev, err := v.issueEvent(is)
				return &ev, err
			})
	case stepPulls:
		n, next, err = c.pulls(ctx, sink, v, pos, floor)
	case stepIssueComments:
		n, next, err = walkUpdated(ctx, c, sink, pos, floor, "/issues/comments", url.Values{}, commentUpdated, eventOf(v.commentEvent))
	case stepReviewComments:
		n, next, err = walkUpdated(ctx, c, sink, pos, floor, "/pulls/comments", url.Values{}, commentUpdated, eventOf(v.reviewCommentEvent))
	case stepCommits:
		n, next, err = c.commits(ctx, sink, v, pos, floor, repo.DefaultBranch)
	}
	if err != nil {
		return n, nil, fmt.Errorf("backfilling %s of %s: %w", pos.Step, pos.Repo, err)
	}
	if next == nil {
		return n, c.advance(pos), nil
	}
	return n, next, nil
}

func commentUpdated(cm comment) time.Time { return cm.UpdatedAt }

func eventOf[T any](build func(T) (connector.Event, error)) func(T) (*connector.Event, error) {
	return func(x T) (*connector.Event, error) {
		ev, err := build(x)
		return &ev, err
	}
}

// before reports whether something last updated at t is older than floor. Only
// the pulls list needs it: every other list is bounded by `since` at GitHub.
func before(floor, t time.Time) bool { return !floor.IsZero() && t.Before(floor) }

// walkUpdated reads one page of a list GitHub sorts by updated_at and filters
// with `since`, emits what build makes of each item (nil is nothing), and
// returns the position of the next page, or nil when the list is exhausted.
//
// The next page starts one second before the last item's updated_at. Where
// that is not past where this page started — a page's worth of items updated in
// the same second — the next page is the next page number at the same since,
// so the walk always moves.
func walkUpdated[T any](ctx context.Context, c *Connector, sink connector.Sink, pos position, floor time.Time,
	endpoint string, q url.Values, updated func(T) time.Time, build func(T) (*connector.Event, error),
) (int, *position, error) {
	since, err := parseStamp(pos.Since)
	if err != nil {
		return 0, nil, err
	}
	if since.IsZero() {
		since = floor
	}
	page := max(pos.Page, 1)
	q.Set("sort", "updated")
	q.Set("direction", "asc")
	q.Set("per_page", strconv.Itoa(perPage))
	if !since.IsZero() {
		query := since
		if pos.Since == "" {
			// GitHub documents since as "updated after", so the start date
			// itself is asked for a second early. Timestamps are whole seconds,
			// so nothing earlier than the start date comes back.
			query = since.Add(-time.Second)
		}
		q.Set("since", stamp(query))
	}
	if page > 1 {
		q.Set("page", strconv.Itoa(page))
	}

	var items []T
	more, err := c.api.get(ctx, repoPath(pos.Repo)+endpoint+"?"+q.Encode(), &items)
	if err != nil {
		return 0, nil, err
	}
	n := 0
	for _, it := range items {
		ev, err := build(it)
		if err != nil {
			return n, nil, err
		}
		if ev == nil {
			continue
		}
		if err := c.emit(ctx, sink, *ev); err != nil {
			return n, nil, err
		}
		n++
	}
	if !more || len(items) == 0 {
		return n, nil, nil
	}
	next := position{Repo: pos.Repo, Step: pos.Step}
	if bound := updated(items[len(items)-1]).Add(-time.Second); bound.After(since) {
		next.Since = stamp(bound)
	} else {
		next.Since, next.Page = pos.Since, page+1
	}
	return n, &next, nil
}

func isPull(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}

// pulls emits a page of pull requests, in creation order, and every submitted
// review of each: GitHub lists reviews per pull request and not per
// repository. The list takes no `since`, so the page is filtered instead.
func (c *Connector) pulls(ctx context.Context, sink connector.Sink, v view, pos position, floor time.Time) (int, *position, error) {
	page := max(pos.Page, 1)
	q := url.Values{"state": {"all"}, "sort": {"created"}, "direction": {"asc"}, "per_page": {strconv.Itoa(perPage)}}
	if page > 1 {
		q.Set("page", strconv.Itoa(page))
	}
	var items []pull
	more, err := c.api.get(ctx, repoPath(v.repo)+"/pulls?"+q.Encode(), &items)
	if err != nil {
		return 0, nil, err
	}
	n := 0
	for _, p := range items {
		if before(floor, p.UpdatedAt) {
			continue
		}
		ev, err := v.pullEvent(p)
		if err != nil {
			return n, nil, err
		}
		if err := c.emit(ctx, sink, ev); err != nil {
			return n, nil, err
		}
		n++
		reviews, err := c.reviews(ctx, sink, v, p.Number)
		n += reviews
		if err != nil {
			return n, nil, err
		}
	}
	if !more || len(items) == 0 {
		return n, nil, nil
	}
	return n, &position{Repo: pos.Repo, Step: pos.Step, Page: page + 1}, nil
}

// reviews emits every submitted review of one pull request, all its pages in
// this call.
func (c *Connector) reviews(ctx context.Context, sink connector.Sink, v view, number int) (int, error) {
	n := 0
	for page, more := 1, true; more; page++ {
		q := url.Values{"per_page": {strconv.Itoa(perPage)}}
		if page > 1 {
			q.Set("page", strconv.Itoa(page))
		}
		var items []review
		var err error
		more, err = c.api.get(ctx, repoPath(v.repo)+"/pulls/"+strconv.Itoa(number)+"/reviews?"+q.Encode(), &items)
		if err != nil {
			return n, err
		}
		for _, r := range items {
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
		if len(items) == 0 {
			break
		}
	}
	return n, nil
}

// commits emits a page of the default branch's history. The first page reads
// the commit the branch points at and pins the walk to it, so a push or a force
// push while the walk runs cannot move a commit between pages; what the branch
// gains in the meantime is the push webhook's.
func (c *Connector) commits(ctx context.Context, sink connector.Sink, v view, pos position, floor time.Time, branch string) (int, *position, error) {
	head := pos.Head
	if head == "" {
		var b struct {
			Commit struct {
				SHA string `json:"sha"`
			} `json:"commit"`
		}
		_, err := c.api.get(ctx, repoPath(v.repo)+"/branches/"+url.PathEscape(branch), &b)
		if serr := (*statusError)(nil); errors.As(err, &serr) && serr.status == http.StatusNotFound {
			// The default branch of a repository with no commits yet.
			return 0, nil, nil
		}
		if err != nil {
			return 0, nil, fmt.Errorf("reading branch %s: %w", branch, err)
		}
		if b.Commit.SHA == "" {
			return 0, nil, fmt.Errorf("branch %s names no commit", branch)
		}
		head = b.Commit.SHA
	}

	page := max(pos.Page, 1)
	q := url.Values{"sha": {head}, "per_page": {strconv.Itoa(perPage)}}
	if page > 1 {
		q.Set("page", strconv.Itoa(page))
	}
	if !floor.IsZero() {
		// "After" the start date, as for the lists: a second early.
		q.Set("since", stamp(floor.Add(-time.Second)))
	}
	var items []commit
	more, err := c.api.get(ctx, repoPath(v.repo)+"/commits?"+q.Encode(), &items)
	if err != nil {
		return 0, nil, err
	}
	n := 0
	for _, cm := range items {
		ev, err := v.commitEvent(cm)
		if err != nil {
			return n, nil, err
		}
		if err := c.emit(ctx, sink, ev); err != nil {
			return n, nil, err
		}
		n++
	}
	if !more || len(items) == 0 {
		return n, nil, nil
	}
	return n, &position{Repo: pos.Repo, Step: pos.Step, Head: head, Page: page + 1}, nil
}

// repoPath is a repository's REST path.
func repoPath(fullName string) string {
	owner, name, _ := strings.Cut(fullName, "/")
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
}
