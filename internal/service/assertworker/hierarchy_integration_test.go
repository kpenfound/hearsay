//go:build integration

package assertworker_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/bundle"
	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/github"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/principal"
	"github.com/kpenfound/hearsay/internal/service/assertworker"
)

// hierarchySource is the GitHub source the connector's sub-issue recordings
// are delivered as.
const hierarchySource = "github-acme"

// hierarchyConfig maps acme/api to a tracker, or, unmapped, only covers it.
func hierarchyConfig(mapped bool) config.Repo {
	scope := config.Scope{ID: "api", Sources: []config.ScopeSource{{Source: hierarchySource, Containers: []string{"acme/api"}}}}
	if mapped {
		scope.Tracker = config.SourceRef{Source: hierarchySource, Project: "acme/api"}
	}
	return config.Repo{Scopes: []config.Scope{scope}}
}

// hooks is the GitHub connector's webhook handler writing into L0 through the
// gate production puts in front of it.
type hooks struct {
	t *testing.T
	h http.Handler
}

func newHooks(t *testing.T, pool *pgxpool.Pool) *hooks {
	t.Helper()
	src := connector.SourceConfig{
		ID: hierarchySource, Type: github.Type, Containers: []string{"acme/api"},
		Settings: []byte(`{"api_url":"http://127.0.0.1:1"}`),
		Secrets:  map[string]string{github.SecretToken: "token", github.SecretWebhook: "secret"},
	}
	c, err := github.New(src)
	if err != nil {
		t.Fatalf("github.New = %v", err)
	}
	t.Cleanup(func() { _ = c.Close(t.Context()) })
	gate := connector.NewGate(l0.New(pool), src.ID, c.Describe(), connector.NewAllowlist(src))
	return &hooks{t: t, h: c.Handler(gate)}
}

// deliver posts one of the connector's recorded deliveries.
func (h *hooks) deliver(event, name string) {
	h.t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "connector", "github", "testdata", "hooks", name+".json"))
	if err != nil {
		h.t.Fatal(err)
	}
	mac := hmac.New(sha256.New, []byte("secret"))
	mac.Write(body)
	req := httptest.NewRequestWithContext(h.t.Context(), http.MethodPost, "/hooks/"+hierarchySource, bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	rr := httptest.NewRecorder()
	h.h.ServeHTTP(rr, req)
	if rr.Code != http.StatusAccepted {
		h.t.Fatalf("%s: status %d, want 202", name, rr.Code)
	}
}

func item(n string) string { return "tracker:" + hierarchySource + ":acme/api#" + n }

// follow reads the whole change feed into the tracker hierarchy.
func follow(t *testing.T, pool *pgxpool.Pool, repo config.Repo) {
	t.Helper()
	f := assertworker.NewFollower(pool, repo, time.Millisecond, l0.MaxLimit)
	for {
		n, err := f.Once(t.Context())
		if err != nil {
			t.Fatalf("Once() = %v", err)
		}
		if n == 0 {
			return
		}
	}
}

// stanceOn opens a topic about a tracker item with one stance, evidenced by
// the item's own document.
func stanceOn(t *testing.T, pool *pgxpool.Pool, n, position string) {
	t.Helper()
	at := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	artifact := "acme/api#" + n
	doc := l1.Document{
		ID: l1.DocID(hierarchySource, artifact), Kind: l1.KindIssue, ArtifactClass: config.ArtifactIssue,
		Source: l1.Source{System: hierarchySource, NativeID: artifact},
		L0Refs: []string{connector.EventID(hierarchySource, artifact)}, Time: l1.Times{Created: at, Updated: at, LastActivity: at},
		Scope: []string{item(n)}, ACL: connector.ACL{{Kind: connector.ACLPublic}}, Text: artifact, RawText: artifact,
		Body: l1.Body{Summary: artifact, OutcomeKind: l1.OutcomeDecided},
	}
	if _, err := l1.New(pool).Put(t.Context(), doc); err != nil {
		t.Fatalf("Put(%s) = %v", doc.ID, err)
	}
	graph := l2.New(pool)
	topic := l2.Topic{ID: l2.TopicID("api", doc.ID, 0, position), Scope: "api", Name: position,
		About: []string{item(n)}, ACL: doc.ACL, OpenedBy: doc.ID}
	if _, err := graph.OpenTopic(t.Context(), topic); err != nil {
		t.Fatal(err)
	}
	if _, _, err := graph.AppendStance(t.Context(), l2.Stance{
		ID: l2.StanceID(topic.ID, doc.ID, position, at, l2.TierInferred), TopicID: topic.ID, Position: position,
		StatedAt: at, Evidence: []string{doc.ID}, Tier: l2.TierInferred, ACL: doc.ACL,
	}, at); err != nil {
		t.Fatal(err)
	}
}

// inherited is the positions a bundle for a tracker item serves as inherited.
func inherited(t *testing.T, pool *pgxpool.Pool, entity string) []string {
	t.Helper()
	reader := l1.Reader{Effective: principal.Effective{Human: "kyle", Grant: principal.Grant{Scopes: principal.AllScopes()}}}
	b, _, err := bundle.New(pool).Assemble(t.Context(), reader, entity)
	if err != nil {
		t.Fatalf("Assemble(%s) = %v", entity, err)
	}
	out := []string{}
	for _, s := range b.Stances {
		if s.Inherited {
			out = append(out, s.Current)
		}
	}
	slices.Sort(out)
	return out
}

// Issue #120, from GitHub's recorded deliveries to a bundle: a sub-issue
// inherits its parent's stances, and its grandparent's; moving it to another
// parent changes what it inherits, and taking its parent out of the
// grandparent takes the grandparent's away.
func TestASubIssueInheritsItsParentsStances(t *testing.T) {
	pool := scratchPool(t)
	repo := hierarchyConfig(true)
	gh := newHooks(t, pool)
	stanceOn(t, pool, "10", "health checks ship before the launch")
	stanceOn(t, pool, "12", "readiness is its own endpoint")

	steps := []struct {
		name, event, file string
		want              []string
	}{
		{
			name: "#11 is a sub-issue of #10", event: "issues", file: "issues.edited.sub_issue",
			want: []string{"health checks ship before the launch"},
		},
		{
			name: "#11 moves to #12", event: "sub_issues", file: "sub_issues.parent_issue_added.reparent",
			want: []string{"readiness is its own endpoint"},
		},
		{
			name: "the removal from #10 arrives after the move", event: "sub_issues", file: "sub_issues.parent_issue_removed.reparent",
			want: []string{"readiness is its own endpoint"},
		},
		{
			name: "#12 becomes a sub-issue of #10", event: "sub_issues", file: "sub_issues.parent_issue_added",
			want: []string{"health checks ship before the launch", "readiness is its own endpoint"},
		},
		{
			name: "#12 is taken out of #10", event: "sub_issues", file: "sub_issues.parent_issue_removed",
			want: []string{"readiness is its own endpoint"},
		},
	}
	f := assertworker.NewFollower(pool, repo, time.Millisecond, l0.MaxLimit)
	for _, step := range steps {
		var held pgx.Tx
		if step.file == "issues.edited.sub_issue" {
			// Hold the feed head while the first delivery commits. This
			// reproduces the zero-read window that parallel integration tests
			// can create, so a one-shot follower would miss the placement.
			tx, err := pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			var xid string
			if err := tx.QueryRow(t.Context(), `SELECT pg_current_xact_id()::text`).Scan(&xid); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
			held = tx
		}
		gh.deliver(step.event, step.file)
		if held != nil {
			go func() {
				time.Sleep(100 * time.Millisecond)
				_ = held.Rollback(context.Background())
			}()
		}
		// An open transaction elsewhere in the cluster can make the feed
		// temporarily empty, even after this delivery commits. Wait for the
		// placement rather than treating one empty read as caught up.
		waitFor(t, step.name, func() bool {
			if _, err := f.Once(t.Context()); err != nil {
				t.Fatalf("Once() = %v", err)
			}
			return slices.Equal(inherited(t, pool, item("11")), step.want)
		})
		if got := inherited(t, pool, item("11")); !slices.Equal(got, step.want) {
			t.Fatalf("%s: #11 inherits %q, want %q", step.name, got, step.want)
		}
	}
	e, err := l2.New(pool).Entity(t.Context(), item("12"))
	if err != nil || len(e.PartOf) != 0 {
		t.Errorf("#12 = %+v, %v; want it part of nothing", e, err)
	}
}

// A repository no scope's tracker maps is not a tracker: following it creates
// no entity and no edge, and neither does seeding from it.
func TestNoTrackerItemForARepositoryNoScopeMaps(t *testing.T) {
	pool := scratchPool(t)
	repo := hierarchyConfig(false)
	gh := newHooks(t, pool)
	gh.deliver("issues", "issues.edited.sub_issue")
	gh.deliver("sub_issues", "sub_issues.parent_issue_added")
	follow(t, pool, repo)
	if _, err := assertworker.SeedEntities(t.Context(), pool, repo, nil); err != nil {
		t.Fatalf("SeedEntities() = %v", err)
	}
	entities, err := l2.New(pool).Entities(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(entities) != 0 {
		t.Errorf("entities = %+v, want none", entities)
	}
}

// Startup seeds the tracker hierarchy L0 holds, with no feed read at all, and a
// restart whose configuration no longer maps the tracker takes it away.
func TestStartupSeedsTheTrackerHierarchyFromL0(t *testing.T) {
	pool := scratchPool(t)
	gh := newHooks(t, pool)
	gh.deliver("issues", "issues.edited.sub_issue")
	gh.deliver("sub_issues", "sub_issues.parent_issue_added")
	gh.deliver("sub_issues", "sub_issues.parent_issue_removed")

	parents := func() map[string][]string {
		t.Helper()
		entities, err := l2.New(pool).Entities(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		out := map[string][]string{}
		for _, e := range entities {
			out[e.ID] = e.PartOf
		}
		return out
	}
	if _, err := assertworker.SeedEntities(t.Context(), pool, hierarchyConfig(true), nil); err != nil {
		t.Fatalf("SeedEntities() = %v", err)
	}
	// #12's current revision is part of nothing: it was taken out of #10.
	want := map[string][]string{item("10"): {}, item("11"): {item("10")}}
	if got := parents(); !equalParents(got, want) {
		t.Errorf("seeded %v, want %v", got, want)
	}

	if _, err := assertworker.SeedEntities(t.Context(), pool, hierarchyConfig(false), nil); err != nil {
		t.Fatalf("SeedEntities() = %v", err)
	}
	want = map[string][]string{item("10"): {}, item("11"): {}}
	if got := parents(); !equalParents(got, want) {
		t.Errorf("seeded with the tracker unmapped %v, want %v", got, want)
	}
}

func equalParents(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for id, p := range a {
		q, ok := b[id]
		if !ok || !slices.Equal(p, q) {
			return false
		}
	}
	return true
}
