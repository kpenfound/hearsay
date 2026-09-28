package osmia

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
)

func TestOsmiaConfigurationContractPreservesAuthority(t *testing.T) {
	data, err := os.ReadFile("testdata/memory-setup.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Files map[string]json.RawMessage `json:"files"`
		Scope string                     `json:"hearsay_scope"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for name, raw := range fixture.Files {
		file := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	policy := cfg.Authority.ForScope(fixture.Scope)
	for _, source := range cfg.Sources {
		c, _, _ := setup(t, "work")
		c.source = source.ID
		var settings Settings
		if err := source.DecodeSettings(&settings); err != nil {
			t.Fatal(err)
		}
		c.settings.Channel = settings.Channel
		c.settings.Owner = settings.Owner
		r := doc("proposal", "spec.md", "An agent's proposal", 1)
		expected := l2.TierInferred
		if settings.Channel == "owner" {
			r.Schema = "osmia.trace.ruling"
			r.Decision = "ruling"
			r.OwnerResponse = "The owner's decision"
			r.Actor.Kind = "owner"
			expected = l2.TierRatified
		}
		events, err := c.events(project, r, at)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != 1 {
			t.Fatal(events)
		}
		class, err := l1.ClassFor(l1.KindWikiSection, events[0])
		if err != nil {
			t.Fatal(err)
		}
		document := l1.Document{ArtifactClass: class, Source: l1.Source{System: source.ID}}
		if tier := l2.RecordedTier(policy, document); tier != expected {
			t.Fatalf("%s tier %s want %s", source.ID, tier, expected)
		}
		if events[0].ACL[0].Kind != connector.ACLIdentity {
			t.Fatal("authority did not preserve private ACL")
		}
	}
}
