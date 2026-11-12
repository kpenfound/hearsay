//go:build integration

package work_test

import (
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/work"
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/l0"
)

// A work source and its owner source over the real L0 store: documents and
// decisions are written as the contract says, their structured fields come
// back from the stored event, the project-wide id rule holds across the pair,
// and a project's access reaches both sources' records before it answers.
func TestRecordsInL0(t *testing.T) {
	url := os.Getenv("HEARSAY_DATABASE_URL")
	if url == "" {
		t.Skip("HEARSAY_DATABASE_URL is not set")
	}
	pool, err := db.Connect(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	workSrc, ownerSrc := "projects-"+suffix, "owner-"+suffix
	workCfg := connector.SourceConfig{ID: workSrc, Type: work.Type, Containers: []string{"*"}, Settings: json.RawMessage(`{"attributors":["factory"]}`)}
	ownerCfg := connector.SourceConfig{ID: ownerSrc, Type: work.Type, Containers: []string{"*"}, Settings: json.RawMessage(`{"attributors":["factory"],"owner_of":"` + workSrc + `"}`)}
	configs := []connector.SourceConfig{workCfg, ownerCfg}
	factory := work.NewFactory(principals(), []string{workSrc, ownerSrc, "github", "github-other"}, lookup, work.WithSources(configs))
	handlerOf := func(cfg connector.SourceConfig) http.Handler {
		c, err := factory(t.Context(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		wc := c.(*work.Connector)
		return wc.Handler(connector.NewGate(l0.New(pool), cfg.ID, wc.Describe(), connector.NewAllowlist(configs...)))
	}
	workH, ownerH := handlerOf(workCfg), handlerOf(ownerCfg)
	store := l0.New(pool)
	expect := func(h http.Handler, body string, want int) string {
		t.Helper()
		return send2(t, h, body, want)
	}
	private := with(t, fixture(t, "project"), map[string]any{"repos": nil})
	expect(workH, private, 202)
	expect(workH, fixture(t, "workstream"), 202)

	// A document on a workstream: posted, reposted identically, revised, deleted.
	first := expect(workH, fixture(t, "document"), 202)
	if again := expect(workH, fixture(t, "document"), 202); again != first {
		t.Errorf("identical repost = %s, want %s", again, first)
	}
	revised := with(t, fixture(t, "document"), map[string]any{"body": "# Retry policy\n\nCap at 6.", "updated_at": "2026-10-14T19:00:00Z"})
	expect(workH, revised, 202)
	doc, ok, err := store.CurrentArtifact(t.Context(), workSrc, "payments-rewrite#retry-policy:document:spec-1")
	if err != nil || !ok || !strings.Contains(doc.Payload.Text, "Cap at 6") {
		t.Fatalf("current document = %v %v %q, want the revision", ok, err, doc.Payload.Text)
	}

	// A decision with every field set on the owner source; its fields come back.
	expect(ownerH, minimal(t, "d-1", map[string]any{"workstream": "retry-policy", "decider": "owner"}), 202)
	expect(ownerH, fixture(t, "decision"), 202)
	ev, ok, err := store.CurrentArtifact(t.Context(), ownerSrc, "payments-rewrite#retry-policy:decision:d-2")
	if err != nil || !ok {
		t.Fatalf("decision not held: %v %v", ok, err)
	}
	h, ok := work.Decode(ev)
	if !ok || ev.Kind != work.KindDecision || ev.Payload.BaseKind != connector.KindDocument {
		t.Fatalf("decision stored as %s (base %s), want work.decision over document", ev.Kind, ev.Payload.BaseKind)
	}
	n := h.Native()
	if n.Outcome != "Cap retries at 5 with jittered backoff." || n.Decider != "owner" || n.Supersedes != "d-1" || n.SpecDocument != "spec-1" ||
		n.PullRequest != "https://github.com/acme/api/pull/42" || n.Question != "q-17" || len(n.RejectedOptions) != 2 || n.Context == "" || n.Rationale == "" {
		t.Errorf("native = %+v, want every posted field", n)
	}
	if n.SupersedesAt == nil || n.SupersedesAt.Source != ownerSrc || n.SpecDocumentAt == nil || n.SpecDocumentAt.Source != workSrc {
		t.Errorf("resolved targets = %+v %+v, want the owner and work sources", n.SupersedesAt, n.SpecDocumentAt)
	}

	// The id rule across the pair: d-1 is held on the owner source.
	expect(workH, minimal(t, "d-1", map[string]any{"workstream": "other-stream"}), 409)
	expect(workH, minimal(t, "d-1", nil), 409)
	expect(ownerH, doc2(t, "spec-1"), 409) // the work source holds spec-1 under retry-policy
	expect(ownerH, minimal(t, "d-1", map[string]any{"supersedes": "d-1"}), 400)
	expect(ownerH, minimal(t, "d-3", map[string]any{"supersedes": "missing"}), 409)
	// A revision of d-1 that would close the loop d-2 -> d-1 -> d-2.
	expect(ownerH, minimal(t, "d-1", map[string]any{"workstream": "retry-policy", "decider": "owner", "supersedes": "d-2", "updated_at": "2026-10-14T20:00:00Z"}), 409)
	// Deleting frees the id.
	expect(workH, `{"kind":"decision","project":"payments-rewrite","decision":"x","deleted":true}`, 204)
	expect(ownerH, `{"kind":"decision","project":"payments-rewrite","workstream":"retry-policy","decision":"d-2","deleted":true}`, 202)
	expect(ownerH, `{"kind":"decision","project":"payments-rewrite","workstream":"retry-policy","decision":"d-1","deleted":true}`, 202)
	expect(workH, minimal(t, "d-1", nil), 202)

	// Access follows the project to both sources' records.
	expect(ownerH, minimal(t, "o-1", map[string]any{"decider": "owner"}), 202)
	withRepos := with(t, fixture(t, "project"), map[string]any{"updated_at": "2026-10-15T10:00:00Z"})
	expect(workH, withRepos, 202)
	root, _, _ := store.CurrentArtifact(t.Context(), workSrc, "payments-rewrite")
	for _, ref := range []struct{ source, artifact string }{
		{workSrc, "payments-rewrite#retry-policy:document:spec-1"},
		{workSrc, "payments-rewrite:decision:d-1"},
		{ownerSrc, "payments-rewrite:decision:o-1"},
	} {
		got, ok, err := store.CurrentArtifact(t.Context(), ref.source, ref.artifact)
		if err != nil || !ok || !got.ACL.Equal(root.ACL) {
			t.Errorf("%s %s: ACL %v (held %v, %v), want the project's %v", ref.source, ref.artifact, got.ACL, ok, err, root.ACL)
		}
	}
}

func send2(t *testing.T, h http.Handler, body string, want int) string {
	t.Helper()
	w := request(t.Context(), h, http.MethodPost, "factory-token", body)
	if w.Code != want {
		t.Fatalf("POST %s = %d %s, want %d", body, w.Code, w.Body.String(), want)
	}
	return w.Body.String()
}

func doc2(t *testing.T, id string) string {
	t.Helper()
	return with(t, fixture(t, "document"), map[string]any{"document": id, "workstream": nil})
}
