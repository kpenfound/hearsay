package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/telemetry"
)

const maxDelivery = 25 << 20

type delivery struct {
	Action      string      `json:"action"`
	Repository  *repository `json:"repository"`
	Issue       *issue      `json:"issue"`
	Comment     *comment    `json:"comment"`
	PullRequest *pull       `json:"pull_request"`
	Review      *review     `json:"review"`
	ParentIssue *issue      `json:"parent_issue"`
	SubIssue    *issue      `json:"sub_issue"`
	Ref         string      `json:"ref"`
	Before      string      `json:"before"`
	After       string      `json:"after"`
}

var readTimeout = 8 * time.Second

type comparison struct {
	TotalCommits int      `json:"total_commits"`
	Commits      []commit `json:"commits"`
}

var errMalformed = errors.New("malformed delivery")

// Handler implements [connector.Pusher]: GitHub's webhook deliveries, each
// verified against the source's webhook secret before anything in it is read.
func (c *Connector) Handler(sink connector.Sink) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		log := telemetry.Logger(ctx)
		if r.Method != http.MethodPost {
			http.Error(w, "webhook deliveries are POSTed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxDelivery))
		if err != nil {
			http.Error(w, "reading the delivery", http.StatusRequestEntityTooLarge)
			return
		}
		deliveryID := r.Header.Get("X-GitHub-Delivery")
		if !c.verify(r.Header.Get("X-Hub-Signature-256"), body) {
			log.WarnContext(ctx, "webhook delivery refused: signature does not verify", "delivery", deliveryID)
			http.Error(w, "signature does not verify", http.StatusUnauthorized)
			return
		}

		event := r.Header.Get("X-GitHub-Event")
		events, owed, err := c.deliver(ctx, sink, event, body)
		switch {
		case errors.Is(err, errMalformed):
			log.WarnContext(ctx, "webhook delivery refused", "delivery", deliveryID, "event", event, "error", err)
			http.Error(w, "the delivery cannot be read", http.StatusBadRequest)
			return
		case err != nil:
			log.ErrorContext(ctx, "webhook delivery failed", "delivery", deliveryID, "event", event, "error", err)
			http.Error(w, "the delivery could not be ingested", http.StatusInternalServerError)
			return
		}
		if len(events) == 0 && !owed {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		for _, ev := range events {
			if err := c.emit(ctx, sink, ev); err != nil {
				log.ErrorContext(ctx, "webhook delivery failed", "delivery", deliveryID, "event", event, "native_id", ev.NativeID, "error", err)
				http.Error(w, "the delivery could not be ingested", http.StatusInternalServerError)
				return
			}
		}
		w.WriteHeader(http.StatusAccepted)
	})
}

func (c *Connector) verify(header string, body []byte) bool {
	hexSig, ok := strings.CutPrefix(header, "sha256=")
	if !ok {
		return false
	}
	sig, err := hex.DecodeString(hexSig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, c.secret)
	mac.Write(body)
	return hmac.Equal(sig, mac.Sum(nil))
}

func (c *Connector) deliver(ctx context.Context, sink connector.Sink, event string, body []byte) ([]connector.Event, bool, error) {
	if event == "ping" {
		return nil, false, nil
	}
	var d delivery
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, false, fmt.Errorf("%w: %s: %w", errMalformed, event, err)
	}
	if d.Repository == nil || d.Repository.FullName == "" {
		return nil, false, nil
	}
	v := c.view(d.Repository.FullName, d.Repository.Private)
	if event == "repository" {
		if d.Action != "privatized" || !slices.Contains(c.repos, v.repo) {
			return nil, false, nil
		}
		owes, ok := sink.(connector.ResyncRequester)
		if !ok {
			return nil, false, fmt.Errorf("repository %s went private and the sink cannot record a re-sync", v.repo)
		}
		if err := owes.RequestResync(ctx, v.repo); err != nil {
			return nil, false, err
		}
		return nil, true, nil
	}
	events, err := c.events(ctx, event, v, d)
	return events, false, err
}

func (c *Connector) events(ctx context.Context, event string, v view, d delivery) ([]connector.Event, error) {
	missing := func(what string) error {
		return fmt.Errorf("%w: %s.%s has no %s", errMalformed, event, d.Action, what)
	}

	one := func(ev connector.Event, err error) ([]connector.Event, error) {
		if err != nil {
			return nil, fmt.Errorf("%w: %w", errMalformed, err)
		}
		return []connector.Event{ev}, nil
	}

	switch event {
	case "issues":
		if d.Issue == nil {
			return nil, missing("issue")
		}
		if d.Action == "deleted" {
			return one(v.tombstone(issueArtifact(v.repo, d.Issue.Number), d.Issue.UpdatedAt))
		}
		return one(v.issueEvent(*d.Issue))

	case "issue_comment":
		if d.Comment == nil {
			return nil, missing("comment")
		}
		if d.Action == "deleted" {
			artifact, _, err := v.commentOn(*d.Comment)
			if err != nil {
				return nil, fmt.Errorf("%w: %w", errMalformed, err)
			}
			return one(v.tombstone(artifact, d.Comment.UpdatedAt))
		}
		return one(v.commentEvent(*d.Comment))

	case "pull_request":
		if d.PullRequest == nil {
			return nil, missing("pull_request")
		}
		ctx, cancel := context.WithTimeout(ctx, readTimeout)
		defer cancel()
		t, err := c.touched(ctx, v.repo, d.PullRequest.Number)
		if err != nil {
			return nil, err
		}
		return one(v.pullEvent(*d.PullRequest, t))

	case "pull_request_review":
		if d.Review == nil || d.PullRequest == nil {
			return nil, missing("review or pull_request")
		}
		switch d.Action {
		case "submitted", "edited", "dismissed":
		default:
			return nil, nil
		}
		if !d.Review.submitted() {
			return nil, nil
		}
		return one(v.reviewEvent(d.PullRequest.Number, *d.Review))

	case "pull_request_review_comment":
		if d.Comment == nil {
			return nil, missing("comment")
		}
		if d.Action == "deleted" {
			artifact, _, err := v.reviewCommentOn(*d.Comment)
			if err != nil {
				return nil, fmt.Errorf("%w: %w", errMalformed, err)
			}
			return one(v.tombstone(artifact, d.Comment.UpdatedAt))
		}
		return one(v.reviewCommentEvent(*d.Comment))

	case "sub_issues":
		return subIssue(v, d, missing)

	case "push":
		return c.pushed(ctx, v, d)
	}
	return nil, nil
}

func subIssue(v view, d delivery, missing func(string) error) ([]connector.Event, error) {
	if d.SubIssue == nil || d.ParentIssue == nil {
		return nil, missing("sub_issue or parent_issue")
	}
	repo, ok := repositoryAt(d.SubIssue.RepositoryURL)
	if !ok {
		return nil, missing("sub_issue.repository_url")
	}
	if !strings.EqualFold(repo, v.repo) {
		return nil, nil
	}
	child := *d.SubIssue
	switch d.Action {
	case "parent_issue_added", "sub_issue_added":
		child.ParentIssueURL = d.ParentIssue.URL
	case "parent_issue_removed", "sub_issue_removed":
		if child.ParentIssueURL != "" && sameIssue(child.ParentIssueURL, d.ParentIssue.URL) {
			child.ParentIssueURL = ""
		}
	default:
		return nil, nil
	}
	ev, err := v.issueEvent(child)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errMalformed, err)
	}
	return []connector.Event{ev}, nil
}

func sameIssue(a, b string) bool {
	ra, na, errA := issueAt(a)
	rb, nb, errB := issueAt(b)
	return errA == nil && errB == nil && na == nb && strings.EqualFold(ra, rb)
}

func (c *Connector) pushed(ctx context.Context, v view, d delivery) ([]connector.Event, error) {
	if d.Repository.DefaultBranch == "" || d.Ref != "refs/heads/"+d.Repository.DefaultBranch {
		return nil, nil
	}
	if !isCommitID(d.Before) || !isCommitID(d.After) {
		return nil, fmt.Errorf("%w: push has no before and after commit ids", errMalformed)
	}
	if isNullCommit(d.After) {
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	var (
		commits []commit
		total   int
	)
	if isNullCommit(d.Before) {
		q := url.Values{"sha": {d.After}, "per_page": {strconv.Itoa(perPage)}}
		more, err := c.api.get(ctx, repoPath(v.repo)+"/commits?"+q.Encode(), &commits)
		if err != nil {
			return nil, fmt.Errorf("reading the history of a pushed branch: %w", err)
		}
		total = len(commits)
		if more {
			total++
		}
	} else {
		var cmp comparison
		if _, err := c.api.get(ctx, repoPath(v.repo)+"/compare/"+d.Before+"..."+d.After, &cmp); err != nil {
			return nil, fmt.Errorf("reading pushed commits: %w", err)
		}
		commits, total = cmp.Commits, cmp.TotalCommits
	}
	if total > len(commits) {
		log := telemetry.Logger(ctx)
		log.WarnContext(ctx, "push has more commits than one read returns: the rest are not ingested",
			"repository", v.repo, "read", len(commits), "total", total)
	}

	events := make([]connector.Event, 0, len(commits))
	for _, cm := range commits {
		ev, err := v.commitEvent(cm)
		if err != nil {
			return nil, err
		}
		events = append(events, ev)
	}
	return events, nil
}

func isCommitID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := range len(s) {
		if !strings.ContainsRune("0123456789abcdef", rune(s[i])) {
			return false
		}
	}
	return true
}

func isNullCommit(s string) bool { return strings.Trim(s, "0") == "" }
