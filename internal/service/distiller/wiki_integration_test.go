//go:build integration

package distiller_test

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/service/distiller"
)

func TestWikiSectionsTrackSourceEdits(t *testing.T) {
	pool := newPool(t)
	src := newSource(t)
	base := event(src, connector.KindDocument, "file-1", at(0), who(src, "u1", "kpenfound"), "Design notes", "# Alpha\nWe decided to use the engine.\n# Beta\nBackground with [[Home]] and #tag.")
	base.ACL = connector.ACL{{Kind: connector.ACLGroup, Source: src, NativeID: "team"}}
	first := revised(base, "r1", at(1))
	secondBase := base
	secondBase.Payload.Text = "# Alpha\nWe propose using the queue.\n# Beta\nBackground with [[Home]] and #tag."
	second := revised(secondBase, "r2", at(2))
	thirdBase := secondBase
	thirdBase.Payload.Text = "# Gamma\nWe propose using the queue.\n# Beta\nBackground with [[Home]] and #tag."
	third := revised(thirdBase, "r3", at(3))
	fourthBase := thirdBase
	fourthBase.Payload.Text = "# Beta\nBackground with [[Home]] and #tag."
	fourth := revised(fourthBase, "r4", at(4))

	fixtures := llm.NewFixtures()
	seen := map[string]bool{}
	for _, revision := range []connector.Event{first, second, third, fourth} {
		sections, err := l1.BuildWikiSections(revision, nil, testRepo(src))
		if err != nil {
			t.Fatal(err)
		}
		for _, section := range sections {
			req := distiller.RequestFor(section, distillBudget())
			key := llm.FixtureKey(llm.TierDistill, req)
			if seen[key] {
				continue
			}
			seen[key] = true
			answer := map[string]any{"summary": "Background notes.", "outcome_kind": "none"}
			if section.RawText != "" && section.RawText[0] == '#' && section.RawText != "# Beta\nBackground with [[Home]] and #tag." {
				answer = map[string]any{"summary": "A choice about the implementation.", "outcome": "Use the engine.", "outcome_kind": "decided"}
				if revision.NativeID != first.NativeID {
					answer = map[string]any{"summary": "A queue approach was suggested.", "outcome": "Use the queue.", "outcome_kind": "proposed"}
				}
			}
			body, err := json.Marshal(answer)
			if err != nil {
				t.Fatal(err)
			}
			if err := fixtures.Add(llm.CompletionFixture{Tier: llm.TierDistill, Request: req, Response: llm.Response{JSON: body, StopReason: llm.StopEnd, Model: "recorded"}}); err != nil {
				t.Fatal(err)
			}
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

	ingest(t, pool, []connector.Event{first})
	if result, err := d.Distill(t.Context(), rootID); err != nil || !result.Written {
		t.Fatalf("first distillation = %+v, %v", result, err)
	}
	initial, err := l1.BuildWikiSections(first, nil, testRepo(src))
	if err != nil {
		t.Fatal(err)
	}
	alphaID, betaID := initial[0].ID, initial[1].ID
	alpha, err := docs.Get(t.Context(), alphaID)
	if err != nil {
		t.Fatal(err)
	}
	beta, err := docs.Get(t.Context(), betaID)
	if err != nil {
		t.Fatal(err)
	}
	if alpha.Body.OutcomeKind != l1.OutcomeDecided || beta.Body.OutcomeKind != l1.OutcomeNone {
		t.Errorf("outcomes crossed sections: alpha=%s beta=%s", alpha.Body.OutcomeKind, beta.Body.OutcomeKind)
	}
	for _, stored := range []l1.Stored{alpha, beta} {
		if !slices.Equal(stored.L0Refs, []string{connector.EventID(src, first.NativeID)}) || !slices.Equal(stored.ACL, first.ACL) {
			t.Errorf("provenance/ACL = %+v", stored)
		}
	}
	if result, err := d.Distill(t.Context(), rootID); err != nil || result.Written || result.Deleted {
		t.Fatalf("repeat = %+v, %v", result, err)
	}

	ingest(t, pool, []connector.Event{second})
	if result, err := d.Distill(t.Context(), rootID); err != nil || !result.Written {
		t.Fatalf("edited section = %+v, %v", result, err)
	}
	alpha2, err := docs.Get(t.Context(), alphaID)
	if err != nil {
		t.Fatal(err)
	}
	beta2, err := docs.Get(t.Context(), betaID)
	if err != nil {
		t.Fatal(err)
	}
	if alpha2.Body.OutcomeKind != l1.OutcomeProposed || !alpha2.DistilledAt.After(alpha.DistilledAt) {
		t.Errorf("edited section not updated: %+v", alpha2)
	}
	if !beta2.DistilledAt.Equal(beta.DistilledAt) || !slices.Equal(beta2.L0Refs, beta.L0Refs) {
		t.Error("unrelated section was rewritten")
	}

	ingest(t, pool, []connector.Event{third})
	if result, err := d.Distill(t.Context(), rootID); err != nil || !result.Deleted {
		t.Fatalf("renamed heading = %+v, %v", result, err)
	}
	if _, err := docs.Get(t.Context(), alphaID); !errors.Is(err, l1.ErrNotFound) {
		t.Errorf("old heading remains: %v", err)
	}
	thirdSections, err := l1.BuildWikiSections(third, nil, testRepo(src))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := docs.Get(t.Context(), thirdSections[0].ID); err != nil {
		t.Errorf("renamed heading missing: %v", err)
	}
	beta3, err := docs.Get(t.Context(), betaID)
	if err != nil {
		t.Fatal(err)
	}
	if !beta3.DistilledAt.Equal(beta.DistilledAt) {
		t.Error("renaming another heading rewrote Beta")
	}

	ingest(t, pool, []connector.Event{fourth})
	if result, err := d.Distill(t.Context(), rootID); err != nil || !result.Deleted {
		t.Fatalf("removed heading = %+v, %v", result, err)
	}
	if _, err := docs.Get(t.Context(), thirdSections[0].ID); !errors.Is(err, l1.ErrNotFound) {
		t.Errorf("removed heading remains: %v", err)
	}
	if _, err := docs.Get(t.Context(), betaID); err != nil {
		t.Errorf("Beta disappeared: %v", err)
	}

	aclBase := fourthBase
	aclBase.ACL = connector.ACL{{Kind: connector.ACLIdentity, Source: src, NativeID: "u1"}}
	aclRevision := revised(aclBase, "r5", at(5))
	ingest(t, pool, []connector.Event{aclRevision})
	if result, err := d.Distill(t.Context(), rootID); err != nil || !result.Written {
		t.Fatalf("ACL edit = %+v, %v", result, err)
	}
	privateBeta, err := docs.Get(t.Context(), betaID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(privateBeta.ACL, aclRevision.ACL) || !slices.Equal(privateBeta.L0Refs, []string{connector.EventID(src, aclRevision.NativeID)}) {
		t.Errorf("ACL revision not inherited: %+v", privateBeta)
	}

	deleted := event(src, connector.KindTombstone, "file-1:deleted", at(6), nil, "", "")
	deleted.Payload.Target = "file-1"
	ingest(t, pool, []connector.Event{deleted})
	if result, err := d.Distill(t.Context(), rootID); err != nil || !result.Deleted {
		t.Fatalf("retracted source = %+v, %v", result, err)
	}
	if _, err := docs.Get(t.Context(), betaID); !errors.Is(err, l1.ErrNotFound) {
		t.Errorf("retracted section remains: %v", err)
	}
}
