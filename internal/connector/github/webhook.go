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
	"slices"
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
	Ref     string `json:"ref"`
	Commits []struct {
		ID string `json:"id"`
	} `json:"commits"`
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
		events, err := c.deliver(ctx, sink, event, body)
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

// deliver turns a verified delivery into the events it means. An event the
// connector does not ingest is nothing, not an error: GitHub sends what the
// webhook was configured for, and a person may have ticked more.
func (c *Connector) deliver(ctx context.Context, sink connector.Sink, event string, body []byte) ([]connector.Event, error) {
	if event == "ping" {
		return nil, nil
	}
	var d delivery
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", errMalformed, event, err)
	}
	if d.Repository == nil || d.Repository.FullName == "" {
		// An organisation's event that is about no repository: nothing here is.
		return nil, nil
	}
	v := view{source: c.source, repo: c.canonical(d.Repository.FullName), private: d.Repository.Private}
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

	case "repository":
		if d.Action == "privatized" && slices.Contains(c.repos, v.repo) {
			c.startResync(ctx, sink, v.repo)
		}
		return nil, nil
	}
	return nil, nil
}

// pushed is the commits a push added to the default branch. A push payload
// names its author by git name and email and not by account, so each commit is
// read from the REST API: that is the same object a backfill reads, and so the
// same event. A push that deletes the branch names no commits, and so is
// nothing.
func (c *Connector) pushed(ctx context.Context, v view, d delivery) ([]connector.Event, error) {
	if d.Repository.DefaultBranch == "" || d.Ref != "refs/heads/"+d.Repository.DefaultBranch {
		return nil, nil
	}
	events := make([]connector.Event, 0, len(d.Commits))
	for _, pc := range d.Commits {
		if pc.ID == "" {
			return nil, fmt.Errorf("%w: push names a commit with no id", errMalformed)
		}
		var cm commit
		if _, err := c.api.get(ctx, repoPath(v.repo)+"/commits/"+pc.ID, &cm); err != nil {
			return nil, fmt.Errorf("reading pushed commit: %w", err)
		}
		ev, err := v.commitEvent(cm)
		if err != nil {
			return nil, err
		}
		events = append(events, ev)
	}
	return events, nil
}

// startResync re-emits every artifact in a repository that has just gone
// private, under the composed `perm:private` token and the new ACL, in the
// background: a delivery is answered in seconds and a repository's history is
// not walked in seconds. One re-sync per repository runs at a time, and Close
// stops and waits for them.
func (c *Connector) startResync(ctx context.Context, sink connector.Sink, repo string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.resyncing[repo] {
		return
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
}

// resync walks one repository the way a backfill does, retrying a page that
// fails until it works or the connector is closed.
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
		n, next, err := c.page(ctx, sink, *pos)
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
