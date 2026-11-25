//go:build integration

package work_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/work"
	"github.com/kpenfound/hearsay/internal/db/dbtest"
	"github.com/kpenfound/hearsay/internal/l0"
)

func TestRecordsInL0(t *testing.T) {
	pool := dbtest.Pool(t)
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
	private := with(t, fixture(t, "project"), map[string]any{"repos": nil})
	send(t, workH, private, 202)
	send(t, workH, fixture(t, "workstream"), 202)

	first := send(t, workH, fixture(t, "document"), 202)
	if again := send(t, workH, fixture(t, "document"), 202); again != first {
		t.Errorf("identical repost = %s, want %s", again, first)
	}
	revised := with(t, fixture(t, "document"), map[string]any{"body": "# Retry policy\n\nCap at 6.", "updated_at": "2026-10-14T19:00:00Z"})
	send(t, workH, revised, 202)
	document, ok, err := store.CurrentArtifact(t.Context(), workSrc, "payments-rewrite#retry-policy:document:spec-1")
	if err != nil || !ok || !strings.Contains(document.Payload.Text, "Cap at 6") {
		t.Fatalf("current document = %v %v %q, want the revision", ok, err, document.Payload.Text)
	}

	send(t, ownerH, minimal(t, "d-1", map[string]any{"workstream": "retry-policy", "decider": "owner"}), 202)
	send(t, ownerH, fixture(t, "decision"), 202)
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

	send(t, workH, minimal(t, "d-1", map[string]any{"workstream": "other-stream"}), 409)
	send(t, workH, minimal(t, "d-1", nil), 409)
	send(t, ownerH, doc(t, map[string]any{"document": "spec-1", "workstream": nil}), 409)
	send(t, ownerH, minimal(t, "d-1", map[string]any{"supersedes": "d-1"}), 400)
	send(t, ownerH, minimal(t, "d-3", map[string]any{"supersedes": "missing"}), 409)
	send(t, ownerH, minimal(t, "d-1", map[string]any{"workstream": "retry-policy", "decider": "owner", "supersedes": "d-2", "updated_at": "2026-10-14T20:00:00Z"}), 409)
	send(t, workH, `{"kind":"decision","project":"payments-rewrite","decision":"x","deleted":true}`, 204)
	send(t, ownerH, `{"kind":"decision","project":"payments-rewrite","workstream":"retry-policy","decision":"d-2","deleted":true}`, 202)
	send(t, ownerH, `{"kind":"decision","project":"payments-rewrite","workstream":"retry-policy","decision":"d-1","deleted":true}`, 202)
	send(t, workH, minimal(t, "d-1", nil), 202)

	send(t, ownerH, minimal(t, "o-1", map[string]any{"decider": "owner"}), 202)
	withRepos := with(t, fixture(t, "project"), map[string]any{"updated_at": "2026-10-15T10:00:00Z"})
	send(t, workH, withRepos, 202)
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
