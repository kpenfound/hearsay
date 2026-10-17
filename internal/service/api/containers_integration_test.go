//go:build integration

package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/service/api"
)

func TestContainerAudienceFiltersHandlesOverBothInterfaces(t *testing.T) {
	w := newReachWorld(t)
	pool := newPool(t)
	containers, key, err := connector.CanonicalContainers([]connector.ContainerRef{
		{Source: w.src, NativeID: "access-repo-a"},
		{Source: w.src, NativeID: "access-repo-b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	acl := connector.ACL{{Kind: connector.ACLContainers, NativeID: key, Containers: containers}}
	ev := connector.NewFake(connector.SourceConfig{ID: w.src}).NewEvent(connector.KindIssue, "container-project", "containeraccessproof")
	ev.ACL = acl
	if _, err := w.events.Append(t.Context(), ev); err != nil {
		t.Fatal(err)
	}
	doc := l1.Document{
		ID: l1.DocID(w.src, ev.Payload.Artifact), Kind: l1.KindIssue, ArtifactClass: config.ArtifactIssue,
		Source: l1.Source{System: w.src, NativeID: ev.Payload.Artifact},
		L0Refs: []string{connector.EventID(w.src, ev.NativeID)},
		Time:   l1.Times{Created: ev.Time, Updated: ev.Time, LastActivity: ev.Time},
		Scope:  []string{w.item}, ACL: acl, Text: "containeraccessproof", RawText: "containeraccessproof",
		Body: l1.Body{Summary: "containeraccessproof", OutcomeKind: l1.OutcomeNone},
	}
	if _, err := l1.New(pool).Put(t.Context(), doc); err != nil {
		t.Fatal(err)
	}
	// Readers inspect every deferred grant in L1, so this fixture must not
	// leave a repository dependency behind for unrelated API tests.
	defer func() {
		if _, err := l1.New(pool).Delete(t.Context(), doc.ID); err != nil {
			t.Error(err)
		}
	}()
	public := connector.ACL{{Kind: connector.ACLPublic}}
	private := connector.ACL{{Kind: connector.ACLIdentity, Source: w.src, NativeID: kyleNode}}
	appendRevision := func(container, artifact, token string, hour int, access connector.ACL) {
		t.Helper()
		event := connector.NewFake(connector.SourceConfig{ID: w.src}).NewEvent(connector.KindIssue, artifact+"@"+token, "repository content")
		event.Payload.Artifact = artifact
		event.Payload.Revision = &connector.Revision{Token: token, EditedAt: event.Time.Add(time.Duration(hour) * time.Hour)}
		event.Payload.Container = connector.Container{Kind: connector.ContainerRepository, NativeID: container}
		event.ACL = access
		if _, err := w.events.Append(t.Context(), event); err != nil {
			t.Fatal(err)
		}
	}
	appendRevision("access-repo-a", "repo-a-issue", "public", 0, public)
	appendRevision("access-repo-a", "repo-a-second", "public", 0, public)

	for _, tt := range []struct {
		name, change string
		annReads     bool
	}{
		{"missing repository denies access", "", false},
		{"both repositories public", "public-b", true},
		{"one private revision denies during resync", "private-a", false},
		{"all revisions private", "private-second", false},
		{"private revision superseded without repost", "public-a", true},
		{"second repository unreadable", "private-b", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			switch tt.change {
			case "public-b":
				appendRevision("access-repo-b", "repo-b-issue", "public", 0, public)
			case "private-a":
				appendRevision("access-repo-a", "repo-a-issue", "private", 1, private)
			case "private-second":
				appendRevision("access-repo-a", "repo-a-second", "private", 1, private)
			case "public-a":
				appendRevision("access-repo-a", "repo-a-issue", "public-again", 2, public)
				appendRevision("access-repo-a", "repo-a-second", "public-again", 2, public)
			case "private-b":
				appendRevision("access-repo-b", "repo-b-issue", "private", 1, private)
			}
			for _, caller := range []api.Caller{{Principal: "kyle"}, annAlone} {
				want := tt.annReads || caller.Principal == "kyle" && tt.change != ""
				if got := w.reads(t, caller, doc.ID); got != want {
					t.Fatalf("get_l1 for %s readable=%v, want %v", caller.Principal, got, want)
				}
				httpResult, mcpResult := w.both(t, caller, "search", map[string]string{"scope": w.item, "query": "containeraccessproof"})
				if httpResult.status != http.StatusOK || mcpResult.isError || httpResult.body != mcpResult.body {
					t.Fatalf("search interfaces disagree: HTTP %+v MCP %+v", httpResult, mcpResult)
				}
				if got := strings.Contains(httpResult.body, doc.ID); got != want {
					t.Fatalf("search for %s found document=%v, want %v: %s", caller.Principal, got, want, httpResult.body)
				}
				if !want {
					missing := l1.DocID(w.src, "nonexistent")
					hiddenHTTP, hiddenMCP := w.both(t, caller, "get_l1", map[string]string{"id": doc.ID})
					missingHTTP, missingMCP := w.both(t, caller, "get_l1", map[string]string{"id": missing})
					// Only the id the caller supplied may differ in the not-found answer.
					quotedDoc, _ := json.Marshal(doc.ID)
					quotedMissing, _ := json.Marshal(missing)
					hiddenHTTP.body = strings.ReplaceAll(hiddenHTTP.body, string(quotedDoc)[1:len(quotedDoc)-1], string(quotedMissing)[1:len(quotedMissing)-1])
					hiddenMCP.body = strings.ReplaceAll(hiddenMCP.body, string(quotedDoc)[1:len(quotedDoc)-1], string(quotedMissing)[1:len(quotedMissing)-1])
					if hiddenHTTP != missingHTTP || hiddenMCP != missingMCP {
						t.Fatalf("hidden differs from missing: %+v %+v; %+v %+v", hiddenHTTP, hiddenMCP, missingHTTP, missingMCP)
					}
				}
			}
		})
	}
}
