//go:build integration

package github_test

import (
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/l0"
)

// The acceptance criteria against the real store: a backfill of a named
// repository lands its issues, pull requests, reviews and commits in L0 with
// the contract's native ids; a second backfill writes nothing new; webhook
// events for the same objects are deduplicated against the backfilled ones;
// and a repository going private lands one new revision per artifact.
func TestBackfillAndWebhooksLandInL0Once(t *testing.T) {
	url := os.Getenv("HEARSAY_DATABASE_URL")
	if url == "" {
		t.Skip("HEARSAY_DATABASE_URL is not set")
	}
	pool, err := db.Connect(t.Context(), url)
	if err != nil {
		t.Fatalf("connecting to postgres: %v", err)
	}
	t.Cleanup(pool.Close)
	store := l0.New(pool)

	// A source nothing else in the shared database uses.
	id := "gh" + strconv.FormatInt(time.Now().UnixNano(), 36)
	stored := func() []string {
		t.Helper()
		events, err := store.List(t.Context(), l0.ListOptions{Filter: l0.Filter{Source: id}, Limit: l0.MaxLimit})
		if err != nil {
			t.Fatalf("listing L0: %v", err)
		}
		return nativeIDs(events)
	}

	gh := newFakeGitHub(t)
	src := newSource(t, gh, id, nil)
	first := newConnector(t, src)
	backfillAll(t, first, gateFor(src, first, store))

	got := stored()
	want := slices.Clone(wantBackfill)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("L0 holds %v, want %v", got, want)
	}

	second := newConnector(t, src)
	backfillAll(t, second, gateFor(src, second, store))
	if n := len(stored()); n != len(wantBackfill) {
		t.Errorf("after a second backfill L0 holds %d events, want %d", n, len(wantBackfill))
	}

	h := second.Handler(gateFor(src, second, store))
	for _, d := range liveHooks {
		if code := deliver(t, h, d.event, hook(t, d.file)); code != http.StatusAccepted {
			t.Errorf("%s: status = %d, want 202", d.file, code)
		}
	}
	if n := len(stored()); n != len(wantBackfill) {
		t.Errorf("after the webhooks L0 holds %d events, want %d", n, len(wantBackfill))
	}

	gh.private.Store(true)
	if code := deliver(t, h, "repository", hook(t, "repository.privatized")); code != http.StatusAccepted {
		t.Fatalf("privatized: status = %d, want 202", code)
	}
	waitFor(t, "the re-sync to land", func() bool { return len(stored()) == 2*len(wantBackfill) })
	if err := second.Close(t.Context()); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if n := len(stored()); n != 2*len(wantBackfill) {
		t.Errorf("after the re-sync L0 holds %d events, want %d", n, 2*len(wantBackfill))
	}
}

// Issue #73 against the real store: a review edited after it was ingested is
// read again by a backfill over a cleared cursor, which lands a second revision
// instead of failing with ErrRewrite, and the edit's webhook deduplicates
// against it.
func TestABackfillOverAnEditedReviewLandsANewRevision(t *testing.T) {
	url := os.Getenv("HEARSAY_DATABASE_URL")
	if url == "" {
		t.Skip("HEARSAY_DATABASE_URL is not set")
	}
	pool, err := db.Connect(t.Context(), url)
	if err != nil {
		t.Fatalf("connecting to postgres: %v", err)
	}
	t.Cleanup(pool.Close)
	store := l0.New(pool)
	id := "gh" + strconv.FormatInt(time.Now().UnixNano(), 36)

	gh := newFakeGitHub(t)
	src := newSource(t, gh, id, nil)
	first := newConnector(t, src)
	backfillAll(t, first, gateFor(src, first, store))

	const edited = "Looks good, once the handler is named."
	gh.edit("/repos/acme/api/pulls/2/reviews", 77, "body", edited)
	second := newConnector(t, src)
	backfillAll(t, second, gateFor(src, second, store))

	body := strings.Replace(string(hook(t, "pull_request_review.submitted")), `"action": "submitted"`, `"action": "edited"`, 1)
	body = strings.Replace(body, `"body": "Looks good."`, `"body": "`+edited+`"`, 1)
	if code := deliver(t, second.Handler(gateFor(src, second, store)), "pull_request_review", []byte(body)); code != http.StatusAccepted {
		t.Fatalf("edited: status = %d, want 202", code)
	}

	const artifact = "acme/api#2:review:77"
	events, err := store.List(t.Context(), l0.ListOptions{Filter: l0.Filter{Source: id, Artifact: artifact}, Limit: l0.MaxLimit})
	if err != nil {
		t.Fatalf("listing L0: %v", err)
	}
	want := []string{artifact + "@" + reviewToken, artifact + "@56cc310aec1b377c"}
	if got := nativeIDs(events); !slices.Equal(got, want) {
		t.Fatalf("L0 holds %v for the review, want %v", got, want)
	}
	current, err := store.Current(t.Context(), l0.ListOptions{Filter: l0.Filter{Source: id, Artifact: artifact}, Limit: l0.MaxLimit})
	if err != nil {
		t.Fatalf("reading the current revision: %v", err)
	}
	if len(current) != 1 || current[0].Payload.Text != edited {
		t.Errorf("current revision = %+v, want the edited one", current)
	}
}
