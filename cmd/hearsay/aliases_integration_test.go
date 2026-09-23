//go:build integration

package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
)

func TestAliasOperatorCommands(t *testing.T) {
	url := os.Getenv("HEARSAY_DATABASE_URL")
	if url == "" {
		t.Skip("HEARSAY_DATABASE_URL is not set")
	}
	cfg := writeConfig(t, map[string]string{
		"sources/github.yaml":      "id: github\ntype: github\ncontainers: [acme/api]\n",
		"scopes/api.yaml":          "id: api\nsources: [github]\n",
		"principals/kyle.yaml":     "id: kyle\nidentities: [{source: github, native_id: u1}]\n",
		"principals/outsider.yaml": "id: outsider\nidentities: [{source: github, native_id: u2}]\n",
	})
	pool, err := db.Connect(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	graph := l2.New(pool)
	src := "aliascli" + time.Now().Format("20060102150405000000000")
	entity := "code:" + src + ":engine"
	if err := graph.PutEntity(t.Context(), l2.Entity{ID: entity, Type: l2.TypeModule, Name: src + " module", Origin: l2.OriginConfig}); err != nil {
		t.Fatal(err)
	}
	private := connector.ACL{{Kind: connector.ACLIdentity, Source: "github", NativeID: "u1"}}
	public := connector.ACL{{Kind: connector.ACLPublic}}
	put := func(name string, acl connector.ACL) string {
		when := time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC)
		doc := l1.Document{ID: l1.DocID(src, name), Kind: l1.KindIssue, ArtifactClass: config.ArtifactIssue,
			Source: l1.Source{System: src, NativeID: name}, L0Refs: []string{"evt:" + src + ":" + name},
			Time: l1.Times{Created: when, Updated: when, LastActivity: when}, ACL: acl,
			Text: name, RawText: name, Body: l1.Body{Summary: name, OutcomeKind: l1.OutcomeDecided}}
		if _, err := l1.New(pool).Put(t.Context(), doc); err != nil {
			t.Fatal(err)
		}
		return doc.ID
	}
	secretDoc := put("secret", private)
	openDoc := put("open", public)
	if err := graph.VoteAlias(t.Context(), entity, "secret engine", secretDoc, secretDoc, private, private); err != nil {
		t.Fatal(err)
	}
	if err := graph.VoteAlias(t.Context(), entity, "open engine", openDoc, openDoc, public, public); err != nil {
		t.Fatal(err)
	}
	invoke := func(who, action string, words ...string) (string, error) {
		args := append([]string{"aliases", "--config", cfg, "--principal", who, action}, words...)
		var out, stderr bytes.Buffer
		err := run(t.Context(), args, &out, &stderr)
		return out.String(), err
	}
	got, err := invoke("outsider", "list")
	if err != nil || strings.Contains(got, "secret") || !strings.Contains(got, "open engine") || !strings.Contains(got, "proposed") || !strings.Contains(got, "1") {
		t.Fatalf("outsider list = %q, %v", got, err)
	}
	if _, err := invoke("outsider", "confirm", entity, "secret engine"); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("outsider confirm = %v", err)
	}
	got, err = invoke("kyle", "list")
	if err != nil || !strings.Contains(got, "secret engine") {
		t.Fatalf("kyle list = %q, %v", got, err)
	}
	if _, err := invoke("kyle", "confirm", entity, "secret engine"); err != nil {
		t.Fatal(err)
	}
	if _, err := invoke("kyle", "reject", entity, "open engine"); err != nil {
		t.Fatal(err)
	}
	candidates, err := graph.AliasCandidates(t.Context(), entity)
	if err != nil || len(candidates) != 2 || candidates[0].State != "rejected" || candidates[1].State != "confirmed" {
		t.Fatalf("decisions = %+v, %v", candidates, err)
	}
}
