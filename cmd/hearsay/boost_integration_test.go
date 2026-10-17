//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/l2"

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

// These are running services: ingestion is pumped into distill jobs, L1 writes
// enqueue assert jobs, and the real queue workers consume them. Only providers
// are fake. No test calls Distill, Assert, Handle or writes an L1/L2 fixture.
type boostWorld struct {
	provider  *timedJudgmentProvider
	pool      *pgxpool.Pool
	cfg       *config.Config
	judgments llm.Judgments
	fake      *llm.FakeJudgmentProvider
	recorded  map[string]json.RawMessage
	events    []connector.Event
	docs      []l1.Document
	probes    []string
	calls     *api.Calls
}

func boostRecorded(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	b, err := os.ReadFile("testdata/boost/recorded.json")
	if err != nil {
		t.Fatal(err)
	}
	var r map[string]json.RawMessage
	if err = json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func newBoostWorld(t *testing.T, mode string, multi bool) *boostWorld {
	t.Helper()
	_, pool := deleteDatabase(t, "boost_")
	path := writeConfig(t, map[string]string{
		"sources/s.yaml":    "id: github\ntype: github\ncontainers: [acme/api]\nread_only: false\n",
		"scopes/s.yaml":     "id: team\nsources: [github]\n",
		"principals/p.yaml": "- id: pat\n  token_env: BOOST_PAT_TOKEN\n  scopes: ['*']\n  identities: [{source: github, native_id: u1}]\n- id: bot\n  kind: agent\n  class: worker\n  identities: [{source: github, native_id: bot}]\n  token_env: BOOST_BOT_TOKEN\n  scopes: ['*']\n",
		"authority/a.yaml":  "scope: '*'\nratified_by:\n  principals: [pat]\n",
	})
	cfg := config.Default()
	if err := loadConfig(t.Context(), &cfg, path); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BOOST_PAT_TOKEN", "fixture-person")
	t.Setenv("BOOST_BOT_TOKEN", "fixture-agent")
	t.Setenv(llm.DefaultJudgmentAPIKeyEnv, "")
	cfg.Repo.Boost = llm.JudgmentConfig{Enabled: mode != "off", Model: "recorded-v1", MaxRetries: -1}
	if mode != "off" && mode != "missing" {
		t.Setenv(llm.DefaultJudgmentAPIKeyEnv, "fixture-key")
	}
	if mode == "qualified" || mode == "refused" || mode == "budget" {
		cfg.Repo.Boost.Policies = map[llm.JudgmentTask]llm.AcceptancePolicy{}
		for _, task := range []llm.JudgmentTask{llm.TaskOutcomeClassification, llm.TaskStanceComparison, llm.TaskTopicMatching, llm.TaskTopicShortlist} {
			cfg.Repo.Boost.Policies[task] = llm.AcceptancePolicy{Name: "synthetic-test-only", ModelVersion: "recorded-v1", Threshold: .9}
		}
	}
	w := &boostWorld{pool: pool, cfg: &cfg, recorded: boostRecorded(t)}
	at := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	for i, text := range []string{"Propose taking the lock inside the queue.", "Take the lock before writing. See https://github.com/acme/api/issues/1."} {
		artifact := fmt.Sprintf("acme/api#%d", i+1)
		w.events = append(w.events, connector.Event{Source: "github", NativeID: artifact, Kind: connector.KindIssue, Time: at.Add(time.Duration(i) * time.Hour), ACL: connector.ACL{{Kind: connector.ACLPublic}}, Payload: connector.Payload{Artifact: artifact, Title: "Where to take the lock", Text: text, URL: fmt.Sprintf("https://github.com/acme/api/issues/%d", i+1), Container: connector.Container{Kind: connector.ContainerRepository, NativeID: "acme/api"}, Author: &connector.Identity{Source: "github", Kind: connector.IdentityUser, NativeID: "u1"}}})
	}
	if multi {
		side := w.events[0]
		side.NativeID = "acme/api#3"
		side.Payload.Artifact = side.NativeID
		side.Payload.Text = "Additional lock proposals."
		side.Payload.URL = "https://github.com/acme/api/issues/3"
		w.events = append(w.events, side)
		w.events[1].Payload.Text += " See https://github.com/acme/api/issues/3."
	}
	fx := llm.NewFixtures()
	resolver, err := cfg.Repo.Resolver()
	if err != nil {
		t.Fatal(err)
	}
	for i, ev := range w.events {
		doc, err := l1.Build(l1.Input{Root: ev, Resolver: resolver, Repo: cfg.Repo})
		if err != nil {
			t.Fatal(err)
		}
		name := []string{"first", "second", "side"}[i]
		tier, _ := cfg.Repo.LLM.Tier(llm.TierDistill)
		if err = fx.Add(llm.CompletionFixture{Tier: llm.TierDistill, Request: distiller.RequestFor(doc, tier.MaxTokens), Response: llm.Response{JSON: w.recorded[name], StopReason: llm.StopEnd}}); err != nil {
			t.Fatal(err)
		}
		var body l1.Body
		if err = json.Unmarshal(w.recorded[name], &body); err != nil {
			t.Fatal(err)
		}
		doc, _, err = doc.WithBody(body)
		if err != nil {
			t.Fatal(err)
		}
		w.docs = append(w.docs, doc)
		candidates := []assertworker.Candidate(nil)
		answer := "assert_first"
		if multi {
			answer = "assert_seed_multi"
			if i == 2 {
				answer = "assert_seed_side"
			}
		}
		if i == 1 {
			candidates = []assertworker.Candidate{{Name: "Where to take the lock", Current: "Take the lock inside the queue."}}
			answer = "assert_second"
			if multi {
				answer = "assert_multi"
				candidates = nil
				names := []string{}
				for n := range 6 {
					names = append(names, fmt.Sprintf("Lock question %d", n))
				}
				sort.Slice(names, func(i, j int) bool {
					a, b := indexName(names[i]), indexName(names[j])
					if (a < 4) != (b < 4) {
						return a < 4
					}
					doc := w.docs[0].ID
					if a >= 4 {
						doc = l1.DocID("github", "acme/api#3")
						a -= 4
						b -= 4
					}
					return l2.TopicID("team", doc, a, names[i]) < l2.TopicID("team", doc, b, names[j])
				})
				for _, name := range names[:5] {
					candidates = append(candidates, assertworker.Candidate{Name: name, Current: "Take the lock inside the queue."})
				}
			}
		}
		tier, _ = cfg.Repo.LLM.Tier(llm.TierAssert)
		add := func(d l1.Document) {
			t.Helper()
			if err := fx.Add(llm.CompletionFixture{Tier: llm.TierAssert, Request: assertworker.RequestFor(d, candidates, tier.MaxTokens), Response: llm.Response{JSON: w.recorded[answer], StopReason: llm.StopEnd}}); err != nil {
				t.Fatal(err)
			}
		}
		add(doc)
		if i == 1 {
			if err = fx.Add(llm.CompletionFixture{Tier: llm.TierDistill, Request: distiller.OutcomeRestatementRequest(doc, l1.OutcomeResolved, cfg.Repo.LLM.Tiers[llm.TierDistill].MaxTokens), Response: llm.Response{JSON: w.recorded["restatement"], StopReason: llm.StopEnd}}); err != nil {
				t.Fatal(err)
			}
			doc.Body.OutcomeKind, doc.Body.Outcome = l1.OutcomeResolved, "The lock now precedes the write."
			doc, _, err = doc.WithBody(doc.Body)
			if err != nil {
				t.Fatal(err)
			}
			add(doc)
		}
	}
	steps := []llm.JudgmentStep{}
	names := []string{"outcome_proposed", "outcome_resolved", "stance", "shortlist", "topic"}
	if multi {
		names[3], names[4] = "shortlist_broad", "topic_broad"
	}
	for _, name := range names {
		var r llm.JudgmentResponse
		if err = json.Unmarshal(w.recorded[name], &r); err != nil {
			t.Fatal(err)
		}
		steps = append(steps, llm.JudgmentStep{Response: r})
	}
	if mode == "failure" {
		steps = []llm.JudgmentStep{{Error: fmt.Errorf("recorded outage")}, {Error: fmt.Errorf("recorded outage")}, {Error: fmt.Errorf("recorded outage")}}
	}
	if mode == "budget" {
		steps = append([]llm.JudgmentStep{steps[0]}, steps...)
		cfg.Repo.Boost.AssertionJobBudget = time.Second
		cfg.Repo.Boost.MaxRetries = 1
		cfg.Repo.Boost.Timeout = 2 * time.Second
		cfg.Repo.Boost.TotalTimeout = 3 * time.Second
		cfg.Repo.Boost.CooldownFailures = 100
		// Three stance comparisons, broader-pool shortlisting, then a retried
		// topic call whose Retry-After must fit the SAME remaining job budget.
		stance := steps[3]
		stance.Delay = 30 * time.Millisecond
		shortlist := steps[4]
		shortlist.Delay = 30 * time.Millisecond
		steps = append(steps[:3], stance, stance, stance, shortlist, llm.JudgmentStep{Delay: 30 * time.Millisecond, Error: &llm.Error{Status: 429, Retryable: true, RetryAfter: 30 * time.Millisecond}}, llm.JudgmentStep{Delay: 5 * time.Second, Response: steps[5].Response})
	}
	w.fake = llm.NewFakeJudgmentProvider(steps...)
	w.provider = &timedJudgmentProvider{inner: w.fake}
	w.judgments, err = judgmentCapability(&cfg, func(llm.JudgmentConfig, string) llm.JudgmentProvider { return w.provider })
	if err != nil {
		t.Fatal(err)
	}
	if mode == "refused" {
		_, err = pool.Exec(t.Context(), `CREATE FUNCTION refuse_boost_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.state='committed' THEN RAISE EXCEPTION 'fixture refuses commit'; END IF; RETURN NEW; END $$; CREATE TRIGGER refuse_boost_commit BEFORE INSERT OR UPDATE ON judgment_journal FOR EACH ROW EXECUTE FUNCTION refuse_boost_commit()`)
		if err != nil {
			t.Fatal(err)
		}
	}
	registry, err := llm.NewFake(cfg.Repo.LLM, fx)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 2)
	// The CLI constructs these same dependencies, including one shared instance.
	for _, name := range []string{distiller.Name, assertworker.Name} {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		w.probes = append(w.probes, "http://"+listener.Addr().String()+"/readyz")
		go func() {
			if name == distiller.Name {
				done <- distiller.Run(ctx, &cfg, distiller.Deps{Pool: pool, LLM: registry, Judgments: w.judgments, Listener: listener, Pump: distiller.PumpOptions{Interval: 5 * time.Millisecond}, Concurrency: 1})
			} else {
				done <- assertworker.Run(ctx, &cfg, assertworker.Deps{Pool: pool, LLM: registry, Judgments: w.judgments, Listener: listener, Connectors: connectorRegistry(cfg.Repo.Principals, cfg.Repo.Sources)})
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
	w.calls, err = api.NewCalls(pool, cfg.Repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	w.checkReady(t, "")
	return w
}

func boostWait(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("running pipeline did not reach the expected state")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
func (w *boostWorld) ingest(t *testing.T, i int) {
	t.Helper()
	if _, err := l0.New(w.pool).Append(t.Context(), w.events[i]); err != nil {
		t.Fatal(err)
	}
}
func (w *boostWorld) awaitAsserted(t *testing.T, i int) {
	t.Helper()
	boostWait(t, func() bool {
		var n int
		err := w.pool.QueryRow(t.Context(), `SELECT count(*) FROM l2_asserted WHERE doc_id=$1`, w.docs[i].ID).Scan(&n)
		return err == nil && n > 0
	})
}
func (w *boostWorld) checkReady(t *testing.T, reason string) {
	t.Helper()
	client := http.Client{Timeout: time.Second}
	for _, url := range w.probes {
		boostWait(t, func() bool {
			resp, err := client.Get(url)
			if err != nil {
				return false
			}
			defer resp.Body.Close()
			var body struct {
				Status    string
				Judgments llm.JudgmentStatus
			}
			if json.NewDecoder(resp.Body).Decode(&body) != nil {
				return false
			}
			return resp.StatusCode == 200 && body.Status == "ok" && body.Judgments.Enabled == w.cfg.Repo.Boost.Enabled && (reason == "" || (!body.Judgments.Healthy && string(body.Judgments.LastFallback) == reason))
		})
	}
}

func TestBoostRunningDistillAssert(t *testing.T) {
	for _, mode := range []string{"off", "shadow", "qualified", "missing", "failure", "refused"} {
		t.Run(mode, func(t *testing.T) {
			w := newBoostWorld(t, mode, false)
			w.ingest(t, 0)
			w.awaitAsserted(t, 0)
			w.ingest(t, 1)
			w.awaitAsserted(t, 1)
			doc, err := l1.New(w.pool).Get(t.Context(), w.docs[1].ID)
			if err != nil {
				t.Fatal(err)
			}
			wantBody := w.docs[1].Body
			wantJudgment := "changes"
			if mode == "qualified" {
				wantBody.OutcomeKind = l1.OutcomeResolved
				wantBody.Outcome = "The lock now precedes the write."
				wantJudgment = "restates"
			}
			actualJSON, _ := json.Marshal(doc.Body)
			expectedJSON, _ := json.Marshal(wantBody)
			if !bytes.Equal(actualJSON, expectedJSON) {
				t.Fatalf("L1=%+v want %+v", doc.Body, wantBody)
			}
			var judgment, position string
			if err = w.pool.QueryRow(t.Context(), `SELECT judgement,position FROM l2_stances WHERE evidence @> ARRAY[$1]::text[] ORDER BY stated_at DESC LIMIT 1`, doc.ID).Scan(&judgment, &position); err != nil {
				t.Fatal(err)
			}
			if judgment != wantJudgment || position != "Take the lock before writing." {
				t.Fatalf("L2=%s %s", judgment, position)
			}
			if mode == "off" || mode == "missing" {
				if w.fake.Calls() != 0 {
					t.Fatal("unexpected credential/provider use")
				}
			}
			if mode == "missing" {
				w.checkReady(t, "missing_credential")
			}
			if mode == "failure" {
				w.checkReady(t, "cooldown")
			}
			var committed int
			if err = w.pool.QueryRow(t.Context(), `SELECT count(*) FROM judgment_journal WHERE state='committed'`).Scan(&committed); err != nil {
				t.Fatal(err)
			}
			if mode == "off" || mode == "refused" {
				if committed != 0 {
					t.Fatalf("committed=%d", committed)
				}
			} else if committed < 5 {
				t.Fatalf("missing end-to-end judgments: %d", committed)
			}
			if mode == "qualified" {
				var tasks int
				if err = w.pool.QueryRow(t.Context(), `SELECT count(DISTINCT task) FROM judgment_journal WHERE state='committed' AND mode='accepted' AND outcome='answer'`).Scan(&tasks); err != nil || tasks != 4 {
					t.Fatalf("accepted tasks=%d %v", tasks, err)
				}
			}
			// Same effective database and principal, fresh API call layers with boost
			// on/off. Compare bytes and check zero additional provider calls.
			before := w.fake.Calls()
			var first []byte
			for _, enabled := range []bool{false, true} {
				repo := w.cfg.Repo
				repo.Boost.Enabled = enabled
				calls, err := api.NewCalls(w.pool, repo, nil)
				if err != nil {
					t.Fatal(err)
				}
				got, err := calls.Call(t.Context(), api.Caller{Principal: "pat"}, "get_bundle", json.RawMessage(`{"scope":"tracker:github:acme/api#1"}`))
				if err != nil {
					t.Fatal(err)
				}
				if first == nil {
					first = got
				} else if !bytes.Equal(first, got) {
					t.Fatalf("boost changed get_bundle bytes: %s / %s", first, got)
				}
			}
			if w.fake.Calls() != before {
				t.Fatal("read path called judgments")
			}
			if mode != "refused" {
				os, err := l1.New(w.pool).ObservationsFor(t.Context(), l1.Reader{Effective: principal.Effective{Human: "pat", Grant: principal.Grant{Scopes: principal.Scopes{All: true}}}, Audience: connector.ACL{{Kind: connector.ACLPublic}}})
				if err != nil {
					t.Fatal(err)
				}
				for _, o := range os {
					wantRequests := int64(0)
					wantAttempts := int64(0)
					if mode != "off" {
						if o.Stage == "distill" {
							wantRequests = 1
							wantAttempts = 1
						} else if slices.Contains(o.Inputs.Documents, w.docs[1].ID) {
							wantRequests = 3
							wantAttempts = 3
						}
					}
					if mode == "missing" {
						wantAttempts = 0
					}
					if mode == "failure" && o.Stage == "assert" && wantRequests > 0 {
						wantAttempts = 1
					}
					if o.JudgmentRequests == nil || *o.JudgmentRequests != wantRequests || o.JudgmentAttempts == nil || *o.JudgmentAttempts != wantAttempts {
						t.Fatalf("logical judgment counts: stage=%s documents=%v requests=%v attempts=%v want=%d/%d", o.Stage, o.Inputs.Documents, o.JudgmentRequests, o.JudgmentAttempts, wantRequests, wantAttempts)
					}

					if o.JudgmentRequests == nil || o.GenerativeCalls == nil || o.GenerativeAttempts == nil || *o.GenerativeCalls != *o.GenerativeAttempts {
						t.Fatalf("missing stage accounting: %+v", o)
					}
				}
			}
		})
	}
}

func indexName(name string) int { var n int; _, _ = fmt.Sscanf(name, "Lock question %d", &n); return n }

type judgmentTiming struct{ start, end, deadline time.Time }
type timedJudgmentProvider struct {
	inner  llm.JudgmentProvider
	mu     sync.Mutex
	assert []judgmentTiming
}

func (p *timedJudgmentProvider) Judge(ctx context.Context, r llm.JudgmentRequest) (llm.JudgmentResponse, error) {
	start := time.Now()
	deadline, _ := ctx.Deadline()
	out, err := p.inner.Judge(ctx, r)
	if r.Questions[0].ID != "outcome" {
		p.mu.Lock()
		p.assert = append(p.assert, judgmentTiming{start, time.Now(), deadline})
		p.mu.Unlock()
	}
	return out, err
}
