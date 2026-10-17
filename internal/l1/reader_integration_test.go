//go:build integration

package l1_test

import (
	"encoding/json"
	"testing"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/principal"
)

func TestReaderResolvesAllContainerAccessLists(t *testing.T) {
	pool := newPool(t)
	for _, tt := range []struct {
		name   string
		lists  []string // pub=public, id=reader, group=reader's team, hidden=another identity
		second string   // ACL for the second container, empty means no current revisions
		want   bool
	}{
		{"public", []string{"pub"}, "pub", true},
		{"identity and group", []string{"id", "group", "group"}, "group", true},
		{"one container unreadable", []string{"pub"}, "hidden", false},
		{"one container missing", []string{"pub"}, "", false},
		{"no current revisions", nil, "pub", false},
		{"mixed public and unreadable private during resync", []string{"pub", "hidden"}, "pub", false},
		{"mixed public and readable private during resync", []string{"pub", "group"}, "pub", true},
		{"ACL lists are disjunctions", []string{"or"}, "id", true},
		{"nested containers cannot prove access", []string{"nested"}, "pub", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tx, err := pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(t.Context()) }()
			src := newSource(t)
			other := newSource(t)
			res, err := principal.NewResolver([]principal.Principal{
				{ID: "reader", Kind: principal.KindHuman, Identities: []principal.Identity{{Source: src, NativeID: "u1"}}},
				{ID: "team", Kind: principal.KindTeam, Members: []string{"reader"}, Identities: []principal.Identity{{Source: src, NativeID: "g1"}}},
			})
			if err != nil {
				t.Fatal(err)
			}
			containers, key, err := connector.CanonicalContainers([]connector.ContainerRef{{Source: other, NativeID: "repo"}, {Source: src, NativeID: "repo"}})
			if err != nil {
				t.Fatal(err)
			}
			entry := connector.ACLEntry{Kind: connector.ACLContainers, NativeID: key, Containers: containers, Label: "mirrored repositories"}
			aclFor := func(kind string) connector.ACL {
				switch kind {
				case "pub":
					return connector.ACL{{Kind: connector.ACLPublic}}
				case "id":
					return connector.ACL{{Kind: connector.ACLIdentity, Source: src, NativeID: "u1", Label: "reader"}}
				case "group":
					return connector.ACL{{Kind: connector.ACLGroup, Source: src, NativeID: "g1"}}
				case "or":
					return connector.ACL{{Kind: connector.ACLIdentity, Source: src, NativeID: "outsider"}, {Kind: connector.ACLGroup, Source: src, NativeID: "g1"}}
				case "nested":
					return connector.ACL{entry}
				default:
					return connector.ACL{{Kind: connector.ACLIdentity, Source: src, NativeID: "outsider"}}
				}
			}
			events := l0.New(tx)
			appendEvent := func(source, artifact, kind string) {
				t.Helper()
				ev := connector.NewFake(connector.SourceConfig{ID: source}).NewEvent(connector.KindIssue, artifact, "repository evidence")
				ev.Payload.Container = connector.Container{Kind: connector.ContainerRepository, NativeID: "repo"}
				ev.ACL = aclFor(kind)
				if _, err := events.Append(t.Context(), ev); err != nil {
					t.Fatal(err)
				}
			}
			for i, kind := range tt.lists {
				appendEvent(src, string(rune('a'+i)), kind)
			}
			if tt.second != "" {
				appendEvent(other, "root", tt.second)
			}
			docs := l1.New(tx)
			doc := searchDoc(t, src, "work-project", "accessproof cutover", connector.ACL{entry})
			put(t, docs, doc)
			eff := principal.Effective{Human: "reader", Grant: principal.Grant{Scopes: principal.SomeScopes(testScope(src))}}
			r, err := l1.ReaderFor(t.Context(), tx, res, eff)
			if err != nil {
				t.Fatal(err)
			}
			if got := r.Allows(doc.ACL); got != tt.want {
				t.Fatalf("Allows() = %v, want %v", got, tt.want)
			}
			found := false
			for _, grant := range r.Audience {
				if grant.Kind == connector.ACLContainers {
					encoded, _ := json.Marshal(grant)
					var fields map[string]json.RawMessage
					if err := json.Unmarshal(encoded, &fields); err != nil || len(fields) != 2 || fields["kind"] == nil || fields["native_id"] == nil {
						t.Fatalf("audience entry = %s, want kind and native_id only", encoded)
					}
					if grant.NativeID == key {
						found = true
					}
				}
			}
			if found != tt.want {
				t.Fatalf("resolved grant present = %v, want %v", found, tt.want)
			}
			hits, err := docs.Search(t.Context(), r, l1.SearchOptions{Query: "accessproof", Scope: testScope(src)})
			if err != nil {
				t.Fatal(err)
			}
			if (len(hits) == 1) != tt.want {
				t.Fatalf("Search() returned %d hits, want readable=%v", len(hits), tt.want)
			}
			if len(hits) > 0 && hits[0].ID != doc.ID {
				t.Fatalf("Search() found %+v", hits)
			}
		})
	}
}

func TestReaderRefusesMalformedContainerGrants(t *testing.T) {
	pool := newPool(t)
	for _, tt := range []struct {
		name   string
		change func(*connector.ACLEntry)
	}{
		{"key mismatch", func(e *connector.ACLEntry) { e.NativeID = "unverified" }},
		{"missing container list", func(e *connector.ACLEntry) { e.Containers = nil }},
		{"invalid source", func(e *connector.ACLEntry) { e.Containers[0].Source = "invalid source" }},
		{"source on deferred entry", func(e *connector.ACLEntry) { e.Source = "unexpected" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tx, err := pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(t.Context()) }()
			src := newSource(t)
			containers, key, err := connector.CanonicalContainers([]connector.ContainerRef{{Source: src, NativeID: "repo"}})
			if err != nil {
				t.Fatal(err)
			}
			entry := connector.ACLEntry{Kind: connector.ACLContainers, NativeID: key, Containers: containers}
			tt.change(&entry)
			// L1's envelope checks the kind. Readers must still prove the
			// deferred grant even for a row not derived through the L0 gate.
			put(t, l1.New(tx), searchDoc(t, src, "project", "unproven", connector.ACL{entry}))
			fake := connector.NewFake(connector.SourceConfig{ID: src})
			event := fake.NewEvent(connector.KindIssue, "repo-issue", "public content")
			event.Payload.Container = connector.Container{Kind: connector.ContainerRepository, NativeID: "repo"}
			if _, err := l0.New(tx).Append(t.Context(), event); err != nil {
				t.Fatal(err)
			}
			res, err := principal.NewResolver([]principal.Principal{{ID: "reader", Kind: principal.KindHuman}})
			if err != nil {
				t.Fatal(err)
			}
			r, err := l1.ReaderFor(t.Context(), tx, res, principal.Effective{Human: "reader"})
			if err != nil {
				t.Fatal(err)
			}
			if r.Allows(connector.ACL{entry}) {
				t.Fatal("malformed deferred grant was allowed")
			}
		})
	}
}
