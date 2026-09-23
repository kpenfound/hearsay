//go:build integration

package distiller_test

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/service/distiller"
)

func TestThreadAliasVotesFromTouchedPR(t *testing.T) {
	pool := newPool(t)
	src := newSource(t)
	cfg := config.Default()
	cfg.Repo = chatRepo(src)
	cfg.Repo.Code[0].Repo = config.SourceRef{Source: src, Project: repo}
	cfg.Repo.Code[0].PathPatterns = []string{"engine/**"}
	resolver, err := cfg.Repo.Resolver()
	if err != nil {
		t.Fatal(err)
	}
	pr := event(src, connector.KindPullRequest, repo+"#90", day, who(src, "u1", "kpenfound"), "Change the server", "Change the server")
	pr.Payload.Paths = []string{"engine/server/main.go"}
	prDoc, err := l1.Build(l1.Input{Root: pr, Resolver: resolver, Repo: cfg.Repo})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(prDoc.References, l1.Reference{Type: l1.RefSystem, ID: cfg.Repo.Code[0].ID}) {
		t.Fatalf("PR references = %+v", prDoc.References)
	}
	prDoc, _, err = prDoc.WithBody(l1.Body{Summary: "Server changed.", OutcomeKind: l1.OutcomeNone})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = l1.New(pool).Put(t.Context(), prDoc); err != nil {
		t.Fatal(err)
	}

	private := connector.ACL{{Kind: connector.ACLGroup, Source: src, NativeID: "C1"}}
	messages := []connector.Event{
		chatMessage(src, "alias-first", "We call it the engine room. https://github.com/acme/api/pull/90", day.Add(24*time.Hour)),
		chatMessage(src, "alias-second", "The engine room fix is in https://github.com/acme/api/pull/90", day.Add(48*time.Hour)),
	}
	messages[1].ACL = private
	fixtures := llm.NewFixtures()
	for _, ev := range messages {
		doc, err := l1.BuildChatWindow(l1.ChatWindowKey(ev), []connector.Event{ev}, resolver, cfg.Repo)
		if err != nil {
			t.Fatal(err)
		}
		answer, _ := json.Marshal(map[string]any{"summary": "The engine room was discussed.", "outcome_kind": "none", "code_names": []string{"Engine Room"}})
		budget, _ := cfg.Repo.LLM.Tier(llm.TierDistill)
		if err := fixtures.Add(llm.CompletionFixture{Tier: llm.TierDistill, Request: distiller.RequestFor(doc, budget.MaxTokens), Response: llm.Response{JSON: answer, StopReason: llm.StopEnd}}); err != nil {
			t.Fatal(err)
		}
	}
	registry, err := llm.NewFake(cfg.Repo.LLM, fixtures)
	if err != nil {
		t.Fatal(err)
	}
	d, err := distiller.New(pool, registry, &cfg)
	if err != nil {
		t.Fatal(err)
	}
	graph := l2.New(pool)
	for i, ev := range messages {
		ingest(t, pool, []connector.Event{ev})
		id := l1.DocID(src, l1.ChatWindowKey(ev))
		if _, err := d.Distill(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Distill(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		candidates, err := graph.AliasCandidates(t.Context(), cfg.Repo.Code[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(candidates) != 1 {
			t.Fatalf("candidate after document %d = %+v", i, candidates)
		}
		c := candidates[0]
		if c.Alias != "engine room" || c.Votes != i+1 || len(c.Evidence) != i+2 || !slices.Contains(c.Evidence, prDoc.ID) || !slices.Contains(c.Evidence, id) {
			t.Errorf("candidate after document %d = %+v", i, c)
		}
		if i == 1 && !slices.Equal(c.ACL, private) {
			t.Errorf("private candidate ACL = %+v, want %+v", c.ACL, private)
		}
	}
	matches, err := graph.Resolve(t.Context(), "engine room")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Errorf("unconfirmed alias resolved: %+v", matches)
	}
}
