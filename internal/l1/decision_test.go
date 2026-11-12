package l1_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/work"
	"github.com/kpenfound/hearsay/internal/l1"
)

// pairRepo is a work source and its owner source.
func pairRepo() config.Repo {
	return config.Repo{Sources: []connector.SourceConfig{
		{ID: "projects", Type: "work"},
		{ID: "projects-owner", Type: "work", Settings: json.RawMessage(`{"owner_of":"projects"}`)},
		{ID: "tracker", Type: "tracker"},
	}}
}

// record is a document or decision event as the work source writes it.
func record(source, artifact string, kind connector.Kind, native work.Native) connector.Event {
	ev := event(kind, artifact, at(0), nil, "Title", "Text")
	ev.Source = source
	ev.Payload.Container = connector.Container{Kind: "project", NativeID: native.Project}
	ev.Payload.Author = &connector.Identity{Source: source, Kind: connector.IdentityUser, NativeID: "kyle"}
	if kind == work.KindDecision {
		ev.Payload.BaseKind = connector.KindDocument
	}
	raw, _ := json.Marshal(native)
	ev.Payload.Native = raw
	ev = revised(ev, "r1", at(1))
	return ev
}

func decisionNative(id, decider string) work.Native {
	return work.Native{Project: "p1", Workstream: "w1", Decision: id, Outcome: "Retry twice.", Decider: decider, Poster: "osmia",
		Context: "Because of token sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJ", Rationale: "Cheap.", RejectedOptions: []string{"never"},
		Supersedes: "d0", SpecDocument: "charter", PullRequest: "https://github.com/acme/api/pull/4", Question: "q-1"}
}

// A decision's document is built from its fields alone: the class and the
// source are the posting source's, whoever decided, and the entities it is
// about are the work source's, even for the owner source's.
func TestBuildDecision(t *testing.T) {
	for _, tt := range []struct {
		name, source, decider string
		wantScope             []string
	}{
		{"owner source, owner decider", "projects-owner", "owner", []string{"work:projects:p1", "work:projects:p1#w1"}},
		{"work source, owner decider", "projects", "owner", []string{"work:projects:p1", "work:projects:p1#w1"}},
		{"work source, chief decider", "projects", "chief_of_staff", []string{"work:projects:p1", "work:projects:p1#w1"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := record(tt.source, "p1#w1:decision:d1", work.KindDecision, decisionNative("d1", tt.decider))
			doc, err := l1.BuildDecision(root, nil, pairRepo())
			if err != nil {
				t.Fatal(err)
			}
			if doc.Kind != l1.KindDecision || doc.ArtifactClass != config.ArtifactDecision || doc.Source.System != tt.source || doc.Source.NativeID != "p1#w1:decision:d1" {
				t.Errorf("document = %+v, want a decision of class decision from %s", doc, tt.source)
			}
			if !slices.Equal(doc.Scope, tt.wantScope) {
				t.Errorf("scope = %v, want %v", doc.Scope, tt.wantScope)
			}
			d := doc.Body.Decision
			if d == nil || d.ID != "d1" || d.WorkSource != "projects" || d.Project != "p1" || d.Workstream != "w1" || d.Decider != tt.decider ||
				d.Supersedes != "d0" || d.SpecDocument != "charter" || d.Question != "q-1" || d.PullRequest != "https://github.com/acme/api/pull/4" ||
				!slices.Equal(d.RejectedOptions, []string{"never"}) || d.Rationale != "Cheap." {
				t.Errorf("decision = %+v, want the record's fields", d)
			}
			if doc.Body.Outcome != "Retry twice." || doc.Body.Summary != "Retry twice." || doc.Body.OutcomeKind != l1.OutcomeDecided {
				t.Errorf("body = %+v, want the outcome as the summary of a decided document", doc.Body)
			}
			if strings.Contains(doc.RawText, "sk-ant") || strings.Contains(d.Context, "sk-ant") || !strings.Contains(doc.RawText, "Decider: "+tt.decider) {
				t.Errorf("text %q, context %q: want the secret scrubbed and the decider read", doc.RawText, d.Context)
			}
			if err := doc.Validate(); err != nil {
				t.Errorf("Validate() = %v", err)
			}
			again, err := l1.BuildDecision(root, nil, pairRepo())
			if err != nil || !reflect.DeepEqual(doc, again) {
				t.Errorf("a second build differs: %v", err)
			}
		})
	}
	t.Run("not a decision", func(t *testing.T) {
		doc := record("projects", "p1:document:charter", connector.KindDocument, work.Native{Project: "p1", Document: "charter", Poster: "osmia"})
		if _, err := l1.BuildDecision(doc, nil, pairRepo()); err == nil {
			t.Error("BuildDecision(document) = nil, want a refusal")
		}
	})
}

// A work document of either source is about the work source's entities, and
// no entity is named after the owner source.
func TestWorkDocumentScope(t *testing.T) {
	for _, tt := range []struct {
		name, source, workstream string
		want                     []string
	}{
		{"owner source, project", "projects-owner", "", []string{"work:projects:p1"}},
		{"owner source, workstream", "projects-owner", "w1", []string{"work:projects:p1", "work:projects:p1#w1"}},
		{"work source, project", "projects", "", []string{"work:projects:p1"}},
		{"work source, workstream", "projects", "w1", []string{"work:projects:p1", "work:projects:p1#w1"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			native := work.Native{Project: "p1", Workstream: tt.workstream, Document: "charter", Poster: "osmia"}
			artifact := "p1:document:charter"
			if tt.workstream != "" {
				artifact = "p1#w1:document:charter"
			}
			root := record(tt.source, artifact, connector.KindDocument, native)
			root.Payload.Text = "We ship small."
			docs, err := l1.BuildWikiSections(root, nil, pairRepo())
			if err != nil || len(docs) != 1 {
				t.Fatalf("BuildWikiSections() = %d, %v", len(docs), err)
			}
			if got := docs[0]; got.ArtifactClass != config.ArtifactSpec || got.Kind != l1.KindWikiSection || got.Source.System != tt.source || !slices.Equal(got.Scope, tt.want) {
				t.Errorf("section = class %s kind %s source %s scope %v, want a spec of %s about %v", got.ArtifactClass, got.Kind, got.Source.System, got.Scope, tt.source, tt.want)
			}
		})
	}
}

func TestWorkSourceOf(t *testing.T) {
	for id, want := range map[string]string{"projects": "projects", "projects-owner": "projects", "tracker": "", "absent": ""} {
		if got, ok := l1.WorkSourceOf(pairRepo(), id); got != want || ok != (want != "") {
			t.Errorf("WorkSourceOf(%s) = %q, %v, want %q", id, got, ok, want)
		}
	}
}
