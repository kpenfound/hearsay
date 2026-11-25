//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/principal"
	"github.com/kpenfound/hearsay/internal/service/api"
	"github.com/kpenfound/hearsay/internal/service/assertworker"
	"github.com/kpenfound/hearsay/internal/service/distiller"
)

type jevWorld struct {
	pool   *pgxpool.Pool
	cfg    *config.Config
	jev    *ruleJev
	events []connector.Event
	docs   []l1.Document
	probes []string
	calls  *api.Calls
}

type ruleJev struct {
	fail  atomic.Bool
	mu    sync.Mutex
	asked []llm.JudgmentRequest
}

func (j *ruleJev) Judge(_ context.Context, req llm.JudgmentRequest) (llm.JudgmentResponse, error) {
	j.mu.Lock()
	j.asked = append(j.asked, req)
	j.mu.Unlock()
	if j.fail.Load() {
		return llm.JudgmentResponse{}, &llm.Error{Status: 503, Retryable: true, Msg: "fixture outage"}
	}
	zero, cost := int64(0), float64(0)
	out := llm.JudgmentResponse{ModelVersion: "recorded-v1", Usage: llm.JudgmentUsage{InputTokens: &zero, OutputTokens: &zero, CostUSD: &cost}}
	for _, q := range req.Questions {
		p := 0.05
		switch {
		case q.ID == "work_finished":
			if strings.Contains(req.State, "Take the lock before writing") {
				p = 0.95
			}
		case q.ID == "unagreed_proposal":
			p = 0.95
		case strings.HasPrefix(q.ID, "T"):
			p = 0.95
		}
		yes := p >= 0.5
		out.Answers = append(out.Answers, llm.JudgmentAnswer{ID: q.ID, Yes: &yes, Confidence: max(p, 1-p), Probabilities: map[string]float64{"yes": p, "no": 1 - p}})
	}
	return out, nil
}

func (j *ruleJev) calls() int { j.mu.Lock(); defer j.mu.Unlock(); return len(j.asked) }

func newJevWorld(t *testing.T) *jevWorld {
	t.Helper()
	_, pool := deleteDatabase(t, "jev_")
	path := writeConfig(t, map[string]string{
		"sources/s.yaml":    "id: github\ntype: github\ncontainers: [acme/api]\nread_only: false\n",
		"scopes/s.yaml":     "id: team\nsources: [github]\n",
		"principals/p.yaml": "- id: pat\n  token_env: JEV_PAT_TOKEN\n  scopes: ['*']\n  identities: [{source: github, native_id: u1}]\n",
		"llm/j.yaml":        "judgments:\n  model: recorded-v1\n  max_retries: -1\n",
	})
	cfg := config.Default()
	if err := loadConfig(t.Context(), &cfg, path); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JEV_PAT_TOKEN", "fixture-person")
	t.Setenv(llm.DefaultJudgmentAPIKeyEnv, "fixture-key")
	w := &jevWorld{pool: pool, cfg: &cfg, jev: &ruleJev{}}
	at := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	texts := []string{"Propose taking the lock inside the queue.", "Take the lock before writing. See https://github.com/acme/api/issues/1."}
	bodies := []l1.Body{
		{Summary: "The team proposes holding the lock in the queue.", Question: "Where should the lock be taken?", Outcome: "Take the lock inside the queue.", OutcomeKind: l1.OutcomeProposed},
		{Summary: "The team took the lock before writing.", Outcome: "The lock now precedes the write.", OutcomeKind: l1.OutcomeResolved},
	}
	positions := []string{"Take the lock inside the queue.", "Take the lock before writing."}
	resolver, err := cfg.Repo.Resolver()
	if err != nil {
		t.Fatal(err)
	}
	fx := llm.NewFixtures()
	distill, _ := cfg.Repo.LLM.Tier(llm.TierDistill)
	assert, _ := cfg.Repo.LLM.Tier(llm.TierAssert)
	for i, text := range texts {
		artifact := "acme/api#" + string(rune('1'+i))
		ev := connector.Event{Source: "github", NativeID: artifact, Kind: connector.KindIssue, Time: at.Add(time.Duration(i) * time.Hour), ACL: connector.ACL{{Kind: connector.ACLPublic}}, Payload: connector.Payload{Artifact: artifact, Title: "Where to take the lock", Text: text, URL: "https://github.com/acme/api/issues/" + string(rune('1'+i)), Container: connector.Container{Kind: connector.ContainerRepository, NativeID: "acme/api"}, Author: &connector.Identity{Source: "github", Kind: connector.IdentityUser, NativeID: "u1"}}}
		w.events = append(w.events, ev)
		doc, err := l1.Build(l1.Input{Root: ev, Resolver: resolver, Repo: cfg.Repo})
		if err != nil {
			t.Fatal(err)
		}
		answer, _ := json.Marshal(map[string]string{"summary": bodies[i].Summary, "question": bodies[i].Question, "outcome": bodies[i].Outcome})
		if err = fx.Add(llm.CompletionFixture{Tier: llm.TierDistill, Request: distiller.RequestFor(doc, distill.MaxTokens), Response: llm.Response{JSON: answer, StopReason: llm.StopEnd}}); err != nil {
			t.Fatal(err)
		}
		if doc, _, err = doc.WithBody(bodies[i]); err != nil {
			t.Fatal(err)
		}
		w.docs = append(w.docs, doc)
		asserted, _ := json.Marshal(map[string]any{"assertions": []map[string]string{{"topic_name": "Where to take the lock", "position": positions[i]}}})
		if err = fx.Add(llm.CompletionFixture{Tier: llm.TierAssert, Request: assertworker.RequestFor(doc, assert.MaxTokens), Response: llm.Response{JSON: asserted, StopReason: llm.StopEnd}}); err != nil {
			t.Fatal(err)
		}
	}
	registry, err := llm.NewFake(cfg.Repo.LLM, fx)
	if err != nil {
		t.Fatal(err)
	}
	judgments, err := judgmentCapability(&cfg, func(llm.JudgmentConfig, string) llm.JudgmentProvider { return w.jev })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 2)
	for _, name := range []string{distiller.Name, assertworker.Name} {
		var lc net.ListenConfig
		listener, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		w.probes = append(w.probes, "http://"+listener.Addr().String()+"/readyz")
		go func() {
			if name == distiller.Name {
				done <- distiller.Run(ctx, &cfg, distiller.Deps{Pool: pool, LLM: registry, Judgments: judgments, Listener: listener, Pump: distiller.PumpOptions{Interval: 5 * time.Millisecond}, Concurrency: 1})
			} else {
				done <- assertworker.Run(ctx, &cfg, assertworker.Deps{Pool: pool, LLM: registry, Judgments: judgments, Listener: listener, Connectors: connectorRegistry(cfg.Repo.Principals, cfg.Repo.Sources)})
			}
		}()
	}
	t.Cleanup(func() {
		cancel()
		for range 2 {
			if err := <-done; err != nil {
				t.Error(err)
			}
		}
	})
	if w.calls, err = api.NewCalls(pool, cfg.Repo, nil); err != nil {
		t.Fatal(err)
	}
	return w
}

func await(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for !condition() {
		select {
		case <-deadline.C:
			t.Fatal("the running pipeline did not reach the expected state")
		case <-tick.C:
		}
	}
}

func (w *jevWorld) ingest(t *testing.T, i int) {
	t.Helper()
	if _, err := l0.New(w.pool).Append(t.Context(), w.events[i]); err != nil {
		t.Fatal(err)
	}
}

func (w *jevWorld) awaitAsserted(t *testing.T, i int) {
	t.Helper()
	await(t, func() bool {
		var n int
		err := w.pool.QueryRow(t.Context(), `SELECT count(*) FROM l2_asserted WHERE doc_id=$1`, w.docs[i].ID).Scan(&n)
		return err == nil && n > 0
	})
}

func (w *jevWorld) readiness(t *testing.T) []llm.JudgmentStatus {
	t.Helper()
	client := http.Client{Timeout: time.Second}
	var out []llm.JudgmentStatus
	for _, url := range w.probes {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var body struct {
			Status    string
			Judgments llm.JudgmentStatus
		}
		err = json.NewDecoder(resp.Body).Decode(&body)
		_ = resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK || body.Status != "ok" {
			t.Fatalf("readyz %s: %d %+v %v", url, resp.StatusCode, body, err)
		}
		out = append(out, body.Judgments)
	}
	return out
}

func TestJevDecidesTheRunningPipeline(t *testing.T) {
	w := newJevWorld(t)
	w.ingest(t, 0)
	w.awaitAsserted(t, 0)
	w.ingest(t, 1)
	w.awaitAsserted(t, 1)

	for i, want := range []l1.OutcomeKind{l1.OutcomeProposed, l1.OutcomeResolved} {
		doc, err := l1.New(w.pool).Get(t.Context(), w.docs[i].ID)
		if err != nil {
			t.Fatal(err)
		}
		if doc.Body.OutcomeKind != want || doc.Body.Outcome != w.docs[i].Body.Outcome {
			t.Fatalf("document %d: %+v, want %s", i, doc.Body, want)
		}
	}
	var topics int
	if err := w.pool.QueryRow(t.Context(), `SELECT count(*) FROM l2_topics`).Scan(&topics); err != nil || topics != 1 {
		t.Fatalf("topics=%d %v: the second issue did not continue the first's topic", topics, err)
	}
	var judgement, position string
	if err := w.pool.QueryRow(t.Context(), `SELECT judgement, position FROM l2_stances WHERE evidence @> ARRAY[$1]::text[]`, w.docs[1].ID).Scan(&judgement, &position); err != nil {
		t.Fatal(err)
	}
	if judgement != "changes" || position != "Take the lock before writing." {
		t.Fatalf("stance=%s %q", judgement, position)
	}
	var unaccepted int
	if err := w.pool.QueryRow(t.Context(), `SELECT count(*) FROM judgment_journal WHERE state='committed' AND (mode<>'accepted' OR outcome<>'answer')`).Scan(&unaccepted); err != nil || unaccepted != 0 {
		t.Fatalf("unaccepted journal records=%d %v", unaccepted, err)
	}
	for _, st := range w.readiness(t) {
		if !st.Healthy || st.Observations == 0 {
			t.Fatalf("judgments=%+v", st)
		}
	}

	before := w.jev.calls()
	var first []byte
	for range 2 {
		got, err := w.calls.Call(t.Context(), api.Caller{Principal: "pat"}, "get_bundle", json.RawMessage(`{"scope":"team"}`))
		if err != nil {
			t.Fatal(err)
		}
		if first != nil && !bytes.Equal(first, got) {
			t.Fatalf("get_bundle changed: %s / %s", first, got)
		}
		first = got
	}
	if w.jev.calls() != before {
		t.Fatal("the read path called Jev")
	}

	os, err := l1.New(w.pool).ObservationsFor(t.Context(), l1.Reader{Effective: principal.Effective{Human: "pat", Grant: principal.Grant{Scopes: principal.Scopes{All: true}}}, Audience: connector.ACL{{Kind: connector.ACLPublic}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range os {
		if o.Cohort != "accepted" || o.JudgmentRequests == nil || o.JudgmentAttempts == nil || o.GenerativeCalls == nil || o.GenerativeAttempts == nil || *o.GenerativeCalls != *o.GenerativeAttempts {
			t.Fatalf("stage accounting: %+v", o)
		}
		if o.Stage == "distill" && (*o.JudgmentRequests != 1 || *o.JudgmentAttempts != 1) {
			t.Fatalf("a distillation asks one batch: %+v", o)
		}
	}
}

func TestAJevOutageFailsTheJobUntilItRecovers(t *testing.T) {
	w := newJevWorld(t)
	w.jev.fail.Store(true)
	w.ingest(t, 0)
	await(t, func() bool {
		for _, st := range w.readiness(t) {
			if !st.Healthy && st.LastFailure == llm.JudgmentUnavailable && st.Failures > 0 {
				return true
			}
		}
		return false
	})
	var written int
	if err := w.pool.QueryRow(t.Context(), `SELECT count(*) FROM l1_docs WHERE id=$1`, w.docs[0].ID).Scan(&written); err != nil || written != 0 {
		t.Fatalf("a document was written without Jev's judgment: %d %v", written, err)
	}
	w.jev.fail.Store(false)
	if _, err := w.pool.Exec(t.Context(), `UPDATE queue_job SET run_after=now() WHERE kind=$1 AND state='pending'`, distiller.JobKind().Name); err != nil {
		t.Fatal(err)
	}
	w.awaitAsserted(t, 0)
}
