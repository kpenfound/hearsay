//go:build integration

package distiller_test

import (
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/service/distiller"
)

func TestMeetingSegmentsTrackEditsAndDeletion(t *testing.T) {
	pool := newPool(t)
	src := newSource(t)
	transcript, err := os.ReadFile("testdata/meetings/planning.txt")
	if err != nil {
		t.Fatal(err)
	}
	base := event(src, connector.KindTranscript, "meeting-1", at(0), nil, "Weekly planning", strings.TrimSpace(string(transcript)))
	base.Payload.Container = connector.Container{Kind: connector.ContainerFolder, NativeID: "meet"}
	base.Payload.Participants = []connector.Participant{{Identity: *who(src, "u1", "kpenfound"), Role: connector.RoleAttendee}, {Identity: *who(src, "u2", "samr"), Role: connector.RoleAttendee}}
	base.ACL = connector.ACL{{Kind: connector.ACLGroup, Source: src, NativeID: "team"}}
	first := revised(base, "r1", at(1))
	editBase := base
	editBase.Payload.Text = strings.Replace(base.Payload.Text, "retries failed requests three times", "retries failed requests at most three times", 1)
	second := revised(editBase, "r2", at(2))
	trimmedBase := editBase
	trimmedBase.Payload.Text = strings.Join(l1.MeetingLines(editBase.Payload.Text)[:4], "\n")
	third := revised(trimmedBase, "r3", at(3))
	privateBase := trimmedBase
	privateBase.ACL = connector.ACL{{Kind: connector.ACLIdentity, Source: src, NativeID: "u1"}}
	fourth := revised(privateBase, "r4", at(4))
	emptyBase := privateBase
	emptyBase.Payload.Text = "Meeting canceled; no discussion took place."
	fifth := revised(emptyBase, "r5", at(5))
	revisions := []connector.Event{first, second, third, fourth, fifth}
	recorded, err := os.ReadFile("testdata/meetings/planning.segments.json")
	if err != nil {
		t.Fatal(err)
	}
	var planned struct {
		Segments []l1.MeetingSpan `json:"segments"`
	}
	if err := json.Unmarshal(recorded, &planned); err != nil {
		t.Fatal(err)
	}
	spans := [][]l1.MeetingSpan{
		planned.Segments,
		{{Topic: "API retry policy", Start: 2, End: 4}, {Topic: "Job queue dashboard", Start: 5, End: 7}},
		{{Topic: "API retry policy", Start: 2, End: 4}},
		{{Topic: "API retry policy", Start: 2, End: 4}},
		{},
	}
	fixtures := llm.NewFixtures()
	seen := map[string]bool{}
	add := func(req llm.Request, response any) {
		t.Helper()
		key := llm.FixtureKey(llm.TierDistill, req)
		if seen[key] {
			return
		}
		seen[key] = true
		data, err := json.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}
		if err := fixtures.Add(llm.CompletionFixture{Tier: llm.TierDistill, Request: req, Response: llm.Response{JSON: data, StopReason: llm.StopEnd, Model: "recorded"}}); err != nil {
			t.Fatal(err)
		}
	}
	for i, revision := range revisions {
		add(distiller.MeetingSegmentationRequest(revision, distillBudget()), map[string]any{"segments": spans[i]})
		docs, err := l1.BuildMeetingSegments(revision, spans[i], nil, testRepo(src))
		if err != nil {
			t.Fatal(err)
		}
		for j, doc := range docs {
			answer := map[string]any{"summary": "The retry limit was discussed.", "outcome": "Cap retries at three.", "outcome_kind": "decided"}
			if j == 1 {
				answer = map[string]any{"summary": "A dashboard was proposed and left open.", "outcome_kind": "open"}
			}
			add(distiller.RequestFor(doc, distillBudget()), answer)
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
	initial, err := l1.BuildMeetingSegments(first, spans[0], nil, testRepo(src))
	if err != nil {
		t.Fatal(err)
	}
	retryID, dashboardID := initial[0].ID, initial[1].ID
	ingest(t, pool, []connector.Event{first})
	if result, err := d.Distill(t.Context(), rootID); err != nil || !result.Written {
		t.Fatalf("first = %+v, %v", result, err)
	}
	retry, err := docs.Get(t.Context(), retryID)
	if err != nil {
		t.Fatal(err)
	}
	dashboard, err := docs.Get(t.Context(), dashboardID)
	if err != nil {
		t.Fatal(err)
	}
	if retry.Body.OutcomeKind != l1.OutcomeDecided || dashboard.Body.OutcomeKind != l1.OutcomeOpen || !slices.Equal(retry.ACL, first.ACL) || !slices.Equal(retry.L0Refs, []string{connector.EventID(src, first.NativeID)}) || len(retry.Participants) != 2 {
		t.Errorf("first segments = %+v, %+v", retry, dashboard)
	}
	if !slices.Contains(retry.References, l1.Reference{Type: l1.RefTrackerItem, ID: "acme/api#31"}) || slices.Contains(dashboard.References, l1.Reference{Type: l1.RefTrackerItem, ID: "acme/api#31"}) || !slices.Contains(dashboard.References, l1.Reference{Type: l1.RefPR, ID: "acme/api#42"}) {
		t.Errorf("references = %+v, %+v", retry.References, dashboard.References)
	}
	if result, err := d.Distill(t.Context(), rootID); err != nil || result.Written || result.Deleted {
		t.Fatalf("repeat = %+v, %v", result, err)
	}
	ingest(t, pool, []connector.Event{second})
	if result, err := d.Distill(t.Context(), rootID); err != nil || !result.Written {
		t.Fatalf("edit = %+v, %v", result, err)
	}
	retry2, err := docs.Get(t.Context(), retryID)
	if err != nil {
		t.Fatal(err)
	}
	dashboard2, err := docs.Get(t.Context(), dashboardID)
	if err != nil {
		t.Fatal(err)
	}
	if retry2.RawText == retry.RawText || !dashboard2.DistilledAt.Equal(dashboard.DistilledAt) {
		t.Errorf("edit isolation = %+v, %+v", retry2, dashboard2)
	}
	ingest(t, pool, []connector.Event{third})
	if result, err := d.Distill(t.Context(), rootID); err != nil || !result.Deleted {
		t.Fatalf("removed topic = %+v, %v", result, err)
	}
	if _, err := docs.Get(t.Context(), dashboardID); !errors.Is(err, l1.ErrNotFound) {
		t.Errorf("removed topic remains: %v", err)
	}
	ingest(t, pool, []connector.Event{fourth})
	if result, err := d.Distill(t.Context(), rootID); err != nil || !result.Written {
		t.Fatalf("ACL edit = %+v, %v", result, err)
	}
	private, err := docs.Get(t.Context(), retryID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(private.ACL, fourth.ACL) || !slices.Equal(private.L0Refs, []string{connector.EventID(src, fourth.NativeID)}) {
		t.Errorf("ACL/provenance = %+v", private)
	}
	ingest(t, pool, []connector.Event{fifth})
	if result, err := d.Distill(t.Context(), rootID); err != nil || !result.Deleted {
		t.Fatalf("nonmeeting = %+v, %v", result, err)
	}
	if _, err := docs.Get(t.Context(), retryID); !errors.Is(err, l1.ErrNotFound) {
		t.Errorf("nonmeeting segment remains: %v", err)
	}
	// A subsequent valid revision and a tombstone prove deletion of a live segment.
	sixth := revised(privateBase, "r6", at(6))
	ingest(t, pool, []connector.Event{sixth})
	if result, err := d.Distill(t.Context(), rootID); err != nil || !result.Written {
		t.Fatalf("restored = %+v, %v", result, err)
	}
	tombstone := event(src, connector.KindTombstone, "meeting-1:deleted", at(7), nil, "", "")
	tombstone.Payload.Target = base.Payload.Artifact
	ingest(t, pool, []connector.Event{tombstone})
	if result, err := d.Distill(t.Context(), rootID); err != nil || !result.Deleted {
		t.Fatalf("deleted = %+v, %v", result, err)
	}
	if _, err := docs.Get(t.Context(), retryID); !errors.Is(err, l1.ErrNotFound) {
		t.Errorf("deleted segment remains: %v", err)
	}
}
