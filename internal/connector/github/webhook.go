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

// maxDelivery is GitHub's own cap on a webhook payload.
const maxDelivery = 25 << 20

// delivery is a webhook payload, as far as the connector reads one. Which of
// the objects is set depends on the event.
type delivery struct {
	Action      string      `json:"action"`
	Repository  *repository `json:"repository"`
	Issue       *issue      `json:"issue"`
	Comment     *comment    `json:"comment"`
	PullRequest *pull       `json:"pull_request"`
	Review      *review     `json:"review"`
	// sub_issues
	ParentIssue *issue `json:"parent_issue"`
	SubIssue    *issue `json:"sub_issue"`
	// push
	Ref    string `json:"ref"`
	Before string `json:"before"`
	After  string `json:"after"`
}

// pushReadTimeout bounds the one REST read a push delivery makes. GitHub gives
// up on a delivery that is not answered in ten seconds, closes the connection
// and does not redeliver, so a read that cannot finish inside that is answered
// as a failure while GitHub is still listening. A variable so that a test can
// shorten it.
var pushReadTimeout = 8 * time.Second

// comparison is GitHub's compare response, as far as a push reads it.
type comparison struct {
	TotalCommits int      `json:"total_commits"`
	Commits      []commit `json:"commits"`
}

// errMalformed is a delivery that verified but that the connector cannot read:
// the sender's fault, answered 400.
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
			// Verified, and nothing to do: an event not ingested, a push to
			// another branch.
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

// verify checks GitHub's HMAC-SHA256 signature over the body, in constant time.
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

// deliver turns a verified delivery into the events it means, and reports
// whether it recorded a re-sync. An event the connector does not ingest is
// nothing, not an error: GitHub sends what the webhook was configured for, and
// a person may have ticked more.
func (c *Connector) deliver(ctx context.Context, sink connector.Sink, event string, body []byte) ([]connector.Event, bool, error) {
	if event == "ping" {
		return nil, false, nil
	}
	var d delivery
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, false, fmt.Errorf("%w: %s: %w", errMalformed, event, err)
	}
	if d.Repository == nil || d.Repository.FullName == "" {
		// An organisation's event that is about no repository: nothing here is.
		return nil, false, nil
	}
	v := c.view(d.Repository.FullName, d.Repository.Private)
	if event == "repository" {
		// Only a repository config names is re-synced: the gate would drop what
		// a walk of any other emits, and the token has no business reading it.
		if d.Action != "privatized" || !slices.Contains(c.repos, v.repo) {
			return nil, false, nil
		}
		// Recorded before the delivery is answered, and walked by the runtime:
		// a walk held in this process would be lost to a restart. A record that
		// cannot be written is a failed delivery, which the runtime's startup
		// check still catches.
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

// events is what a delivery about one repository's objects emits.
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
		return one(v.pullEvent(*d.PullRequest))

	case "pull_request_review":
		if d.Review == nil || d.PullRequest == nil {
			return nil, missing("review or pull_request")
		}
		// An edit or a dismissal changes a review's content token, so it is a new
		// revision (ADR-0012); a pending review is not an artifact yet.
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

// subIssue is a sub_issues delivery: the sub-issue again, part of the parent it
// was added to or of none. GitHub sends one to each end of the relationship —
// `parent_issue_*` to the sub-issue's repository and `sub_issue_*` to the
// parent's — and they build the same event, so one that is about a sub-issue
// in another repository is left to that repository's delivery.
//
// A removal keeps a parent the sub-issue already names that is not the one
// removed: moving an issue to another parent removes it from the old one, and
// that delivery may arrive after the one that added the new.
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

// sameIssue reports whether two API URLs name one issue. GitHub's names are
// case-insensitive.
func sameIssue(a, b string) bool {
	ra, na, errA := issueAt(a)
	rb, nb, errB := issueAt(b)
	return errA == nil && errB == nil && na == nb && strings.EqualFold(ra, rb)
}

// pushed is the commits a push added to the default branch. A push payload
// names its authors by git name and email and not by account, so the commits
// are read from the REST API — the same objects a backfill reads, and so the
// same events — in one call, inside [pushReadTimeout]:
//
//   - a push that moved the branch compares where it was with where it is,
//     which returns up to 250 commits;
//   - the push that created the branch reads the first page of its history.
//
// A push that deletes the branch is nothing. Commits beyond what the one call
// returns are logged, by count, and not emitted.
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

	ctx, cancel := context.WithTimeout(ctx, pushReadTimeout)
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

// isCommitID reports whether s is a commit id: 40 hex digits, or 64 in a
// SHA-256 repository.
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

// isNullCommit reports whether a commit id is all zeros: GitHub's `before` of a
// push that created a branch, and `after` of one that deleted it.
func isNullCommit(s string) bool { return strings.Trim(s, "0") == "" }
