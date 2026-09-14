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

// The waits between attempts of a re-sync that is failing.
const resyncMaxRetry = 15 * time.Minute

// resyncRetry is the first of them, doubling up to resyncMaxRetry. It is a
// variable so that a test can shorten it.
var resyncRetry = time.Minute

// delivery is a webhook payload, as far as the connector reads one. Which of
// the objects is set depends on the event.
type delivery struct {
	Action      string      `json:"action"`
	Repository  *repository `json:"repository"`
	Issue       *issue      `json:"issue"`
	Comment     *comment    `json:"comment"`
	PullRequest *pull       `json:"pull_request"`
	Review      *review     `json:"review"`
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
		events, resyncing, err := c.deliver(ctx, sink, event, body)
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
		if len(events) == 0 && !resyncing {
			// Verified, and nothing to do: an event not ingested, a push to
			// another branch, a re-sync already running.
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
// whether it started a re-sync. An event the connector does not ingest is
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
	v := view{source: c.source, repo: c.canonical(d.Repository.FullName), private: d.Repository.Private}
	if event == "repository" {
		// Only a repository config names is re-synced: the gate would drop what
		// a walk of any other emits, and the token has no business reading it.
		started := d.Action == "privatized" && slices.Contains(c.repos, v.repo) && c.startResync(ctx, sink, v.repo)
		return nil, started, nil
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
		// A submitted review does not change (docs/connector-contract.md), so an
		// edit or a dismissal is not a new observation of it.
		if d.Action != "submitted" || !d.Review.submitted() {
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

	case "push":
		return c.pushed(ctx, v, d)
	}
	return nil, nil
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

// startResync re-emits every artifact in a repository that has just gone
// private, under the composed `perm:private` token and the new ACL, in the
// background: a delivery is answered in seconds and a repository's history is
// not walked in seconds. One re-sync per repository runs at a time, and Close
// stops and waits for them. It reports whether it started one.
func (c *Connector) startResync(ctx context.Context, sink connector.Sink, repo string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.resyncing[repo] {
		return false
	}
	c.resyncing[repo] = true

	// The delivery's context ends with its response; the re-sync keeps its
	// values (the logger) and is cancelled by Close instead.
	rctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	c.wg.Add(2)
	go func() {
		defer c.wg.Done()
		select {
		case <-c.stop:
			cancel()
		case <-rctx.Done():
		}
	}()
	go func() {
		defer c.wg.Done()
		defer cancel()
		c.resync(telemetry.With(rctx, "repository", repo), sink, repo)
	}()
	return true
}

// resync walks one repository the way a backfill does, retrying a page that
// fails until it works or the connector is closed. It ignores the configured
// start date: a push webhook emits a commit whatever its date, and anything a
// webhook may have emitted with the public ACL has to be re-emitted.
func (c *Connector) resync(ctx context.Context, sink connector.Sink, repo string) {
	log := telemetry.Logger(ctx)
	failing := false
	defer func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		delete(c.resyncing, repo)
		if failing {
			c.resyncFails--
		}
	}()
	setFailing := func(f bool) {
		if f == failing {
			return
		}
		failing = f
		c.mu.Lock()
		defer c.mu.Unlock()
		if f {
			c.resyncFails++
		} else {
			c.resyncFails--
		}
	}

	log.InfoContext(ctx, "repository went private: re-syncing its artifacts")
	pos := &position{Repo: repo, Step: steps[0]}
	wait := resyncRetry
	emitted := 0
	for pos != nil && pos.Repo == repo {
		n, next, err := c.page(ctx, sink, *pos, time.Time{})
		emitted += n
		if err != nil {
			if ctx.Err() != nil {
				log.InfoContext(ctx, "re-sync stopped")
				return
			}
			setFailing(true)
			log.WarnContext(ctx, "re-sync failed, retrying", "error", err, "retry_in", wait.String())
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				log.InfoContext(ctx, "re-sync stopped")
				return
			case <-timer.C:
			}
			wait = min(wait*2, resyncMaxRetry)
			continue
		}
		setFailing(false)
		wait = resyncRetry
		pos = next
	}
	log.InfoContext(ctx, "re-sync complete", "events", emitted)
}
