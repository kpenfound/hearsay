package main

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/principal"
)

type hints []connector.Identity

func (h hints) IdentityHints(_ context.Context, visit func(connector.Identity) error) error {
	for _, hint := range h {
		if err := visit(hint); err != nil {
			return err
		}
	}
	return nil
}

func TestIdentityGroupingAndFormats(t *testing.T) {
	people := []principal.Principal{
		{ID: "alice", Name: "Alice Smith", Identities: []principal.Identity{{Source: "github", NativeID: "a", Handle: "alice"}}},
		{ID: "bob", Name: "Bob Jones", Identities: []principal.Identity{{Source: "discord", NativeID: "b", Handle: "bob"}, {Source: "github", NativeID: "b-gh", Handle: "bob"}}},
	}
	r, err := principal.NewResolver(people)
	if err != nil {
		t.Fatal(err)
	}
	seen := hints{
		{Source: "discord", Kind: connector.IdentityUser, NativeID: "u2", Handle: "nobody"},
		{Source: "github", Kind: connector.IdentityUser, NativeID: "u1", Handle: "a-new"},
		{Source: "github", Kind: connector.IdentityUser, NativeID: "u3", Handle: "z"},
		{Source: "github", Kind: connector.IdentityUser, NativeID: "u1", Handle: "a-new", DisplayName: "Alice Smith"},
		{Source: "github", Kind: connector.IdentityUser, NativeID: "u4", Handle: "bob", Email: "alice"},
		{Source: "github", Kind: connector.IdentityBot, NativeID: "bot", Handle: "bot"},
		{Source: "hearsay", Kind: connector.IdentityUser, NativeID: "self"},
		{Source: "discord", Kind: connector.IdentityUser, NativeID: "b", Handle: "bob"},
	}
	groups, err := collectIdentities(t.Context(), seen, r, people)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 || groups[0].Source != "discord" || groups[1].Source != "github" {
		t.Fatalf("groups: %+v", groups)
	}
	got := groups[1].Identities
	if len(got) != 3 || got[0].NativeID != "u1" || got[0].Count != 2 || got[1].NativeID != "u3" || got[2].NativeID != "u4" {
		t.Fatalf("order and counts: %+v", got)
	}
	if got[0].Status != "unknown" || !reflect.DeepEqual(got[0].Suggestions, []string{"alice"}) {
		t.Errorf("suggestion: %+v", got[0])
	}
	if got[2].Status != "ambiguous" || !reflect.DeepEqual(got[2].Candidates, []string{"alice", "bob"}) {
		t.Errorf("ambiguity: %+v", got[2])
	}
	var j, y bytes.Buffer
	enc := json.NewEncoder(&j)
	enc.SetIndent("", "  ")
	if err := enc.Encode(groups); err != nil {
		t.Fatal(err)
	}
	if err := printIdentityYAML(&y, groups); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Principals []struct {
			ID         string `yaml:"id"`
			Identities []struct {
				Source   string `yaml:"source"`
				NativeID string `yaml:"native_id"`
			} `yaml:"identities"`
		} `yaml:"principals"`
	}
	if err := yaml.Unmarshal(y.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Principals) != 4 || doc.Principals[0].ID != "alice" || doc.Principals[0].Identities[0].NativeID != "u1" {
		t.Fatalf("YAML shape: %s", y.String())
	}
	for _, output := range []string{j.String(), y.String()} {
		for _, excluded := range []string{"bot", "self"} {
			if strings.Contains(output, `native_id: `+excluded) || strings.Contains(output, `"native_id": "`+excluded+`"`) {
				t.Errorf("excluded %s in %s", excluded, output)
			}
		}
	}
	again, err := collectIdentities(t.Context(), seen, r, people)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(groups, again) {
		t.Fatal("unstable groups")
	}
	var repeat bytes.Buffer
	if err := printIdentityYAML(&repeat, again); err != nil {
		t.Fatal(err)
	}
	if repeat.String() != y.String() {
		t.Fatal("unstable YAML")
	}
}

func TestIdentitiesRejectsMissingInputsAndPrincipal(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"identities", "list", "--database-url", "postgres://unused"}, "needs --config"},
		{[]string{"identities", "list", "--config", "x"}, "needs --database-url"},
		{[]string{"identities", "list", "--principal", "alice"}, "flag provided but not defined"},
		{[]string{"identities", "list", "--json", "--yaml"}, "cannot be combined"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			t.Setenv("HEARSAY_CONFIG", "")
			t.Setenv("HEARSAY_DATABASE_URL", "")
			var out, errout bytes.Buffer
			if err := run(t.Context(), tc.args, &out, &errout); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("run error = %v, want %q", err, tc.want)
			}
		})
	}
}
