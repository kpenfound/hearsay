//go:build integration

package api

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/principal"
)

func TestContainerAccessChangeInvalidatesBundleWithoutRepost(t *testing.T) {
	pool, err := db.Connect(t.Context(), cacheTestDB(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	t.Setenv("CONTAINER_CACHE_READER", "reader-token")
	repo := config.Repo{
		Sources:    []connector.SourceConfig{{ID: "cache-work", Type: "work"}},
		Principals: []principal.Principal{{ID: "reader", Kind: principal.KindHuman, TokenEnv: "CONTAINER_CACHE_READER", Grant: principal.Grant{Scopes: principal.AllScopes()}}},
	}
	calls, err := NewCalls(pool, repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	const scope = "code:cache-container"
	if err := l2.New(pool).PutEntity(t.Context(), l2.Entity{ID: scope, Name: "Repository", Type: l2.TypeProject, Origin: l2.OriginConfig}); err != nil {
		t.Fatal(err)
	}
	fake := connector.NewFake(connector.SourceConfig{ID: "cache-repository"})
	root := fake.NewEvent(connector.KindIssue, "issue", "repository content")
	root.Payload.Container = connector.Container{Kind: connector.ContainerRepository, NativeID: "repo"}
	events := l0.New(pool)
	if _, err := events.Append(t.Context(), root); err != nil {
		t.Fatal(err)
	}
	second := fake.NewEvent(connector.KindIssue, "second", "repository content")
	second.Payload.Container = root.Payload.Container
	if _, err := events.Append(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	containers, key, err := connector.CanonicalContainers([]connector.ContainerRef{{Source: root.Source, NativeID: "repo"}})
	if err != nil {
		t.Fatal(err)
	}
	docs := l1.New(pool)
	doc := l1.Document{
		ID: l1.DocID("cache-work", "project"), Kind: l1.KindIssue, ArtifactClass: config.ArtifactIssue,
		Source: l1.Source{System: "cache-work", NativeID: "project"}, L0Refs: []string{connector.EventID("cache-work", "project")},
		Time: l1.Times{Created: root.Time, Updated: root.Time, LastActivity: root.Time}, Scope: []string{scope},
		ACL:  connector.ACL{{Kind: connector.ACLContainers, NativeID: key, Containers: containers}},
		Text: "The project mirrors its repository.", RawText: "The project mirrors its repository.",
		Body: l1.Body{Summary: "The project mirrors its repository.", OutcomeKind: l1.OutcomeNone},
	}
	if _, err := docs.Put(t.Context(), doc); err != nil {
		t.Fatal(err)
	}
	personal := doc
	personal.ID, personal.Source.NativeID = l1.DocID("cache-work", "personal"), "personal"
	personal.ACL = connector.ACL{{Kind: connector.ACLIdentity, Source: "cache-work", NativeID: "reader"}}
	if _, err := docs.Put(t.Context(), personal); err != nil {
		t.Fatal(err)
	}
	request := func() []byte {
		t.Helper()
		args, _ := json.Marshal(map[string]string{"scope": scope})
		body, err := calls.Call(t.Context(), Caller{Principal: "reader"}, "get_bundle", args)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(body, []byte(personal.ID)) {
			t.Fatalf("implicit work identity did not read its personal document: %s", body)
		}
		return body
	}
	first := request()
	if !bytes.Contains(first, []byte(doc.ID)) {
		t.Fatalf("public repository's work document missing: %s", first)
	}
	again := request()
	if &first[0] != &again[0] {
		t.Fatal("unchanged bundle did not use cached encoding")
	}
	// Only one of the repository's artifacts has re-synced. No L1 row or
	// work event changes, and the other repository artifact is still public.
	root.NativeID = root.Payload.Artifact + "@private"
	root.Payload.Revision = &connector.Revision{Token: "private", EditedAt: root.Time.Add(time.Hour)}
	root.ACL = connector.ACL{{Kind: connector.ACLGroup, Source: root.Source, NativeID: "private-team"}}
	if _, err := events.Append(t.Context(), root); err != nil {
		t.Fatal(err)
	}
	after := request()
	if bytes.Contains(after, []byte(doc.ID)) {
		t.Fatalf("cached bundle leaked document during ACL resync: %s", after)
	}
	if calls.cache.order.Len() != 2 {
		t.Fatalf("cache entries = %d, want old and new L0 watermark", calls.cache.order.Len())
	}
	stored, err := docs.Get(t.Context(), doc.ID)
	if err != nil || !stored.ACL.Equal(doc.ACL) {
		t.Fatalf("L1 ACL changed: %+v, %v", stored.ACL, err)
	}
}
