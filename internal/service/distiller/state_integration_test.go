//go:build integration

package distiller_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/service/distiller"
)

func TestWikiStateOnlyEditsRefreshSectionEnvelopes(t *testing.T) {
	pool := newPool(t)
	src := newSource(t)
	base := event(src, connector.KindDocument, "file-state", at(0), who(src, "u1", "kpenfound"), "Notes", "# A\nFirst.\n# B\nSecond.")
	first := revised(base, "r1", at(1))
	first.Payload.State = &connector.State{Core: connector.CoreStateProposed}
	initial, err := l1.BuildWikiSections(first, nil, testRepo(src))
	if err != nil {
		t.Fatal(err)
	}
	fixtures := llm.NewFixtures()
	for _, doc := range initial {
		if err := fixtures.Add(llm.CompletionFixture{Tier: llm.TierDistill, Request: distiller.RequestFor(doc, distillBudget()), Response: llm.Response{JSON: json.RawMessage(`{"summary":"Descriptive notes","outcome_kind":"none"}`), StopReason: llm.StopEnd, Model: "recorded"}}); err != nil {
			t.Fatal(err)
		}
	}
	registry, err := llm.NewFake(testRepo(src).LLM, fixtures)
	if err != nil {
		t.Fatal(err)
	}
	d, err := distiller.New(pool, registry, testConfig(src))
	if err != nil {
		t.Fatal(err)
	}
	docs := l1.New(pool)
	rootID := l1.DocID(src, base.Payload.Artifact)
	label, renamed := "In review", "Waiting"
	for i, state := range []*connector.State{first.Payload.State, {Core: connector.CoreStateStarted, Native: &label}, {Core: connector.CoreStateStarted, Native: &renamed}, nil} {
		revision := revised(base, string(rune('a'+i)), at(i+1))
		revision.Payload.State = state
		ingest(t, pool, []connector.Event{revision})
		before := registry.Usage()
		result, err := d.Distill(t.Context(), rootID)
		if err != nil || !result.Written {
			t.Fatalf("Distill = %+v, %v", result, err)
		}
		if i > 0 && !reflect.DeepEqual(registry.Usage(), before) {
			t.Fatal("state-only edit called model")
		}
		for _, section := range initial {
			got, err := docs.Get(t.Context(), section.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.State, state) || !slices.Equal(got.L0Refs, []string{connector.EventID(src, revision.NativeID)}) {
				t.Fatalf("state/provenance = %+v", got)
			}
			if got.Body.Summary != "Descriptive notes" {
				t.Fatal("metadata change replaced body")
			}
		}
	}
}
