package connector_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/connector"
)

func TestCanonicalContainers(t *testing.T) {
	a := connector.ContainerRef{Source: "github-a", NativeID: "acme/api"}
	b := connector.ContainerRef{Source: "github-a", NativeID: "acme/web"}
	c := connector.ContainerRef{Source: "github-b", NativeID: "acme/api"}
	want := []connector.ContainerRef{a, b, c}
	const wantJSON = `[{"source":"github-a","native_id":"acme/api"},{"source":"github-a","native_id":"acme/web"},{"source":"github-b","native_id":"acme/api"}]`
	const wantKey = "538b0d065ce23ef471fb6486d61e24e858d8e7ed8b9e251ce4db01ec7deb6b6e"
	for _, tt := range []struct {
		name  string
		input []connector.ContainerRef
	}{
		{"sorted", []connector.ContainerRef{a, b, c}},
		{"reversed", []connector.ContainerRef{c, b, a}},
		{"duplicates", []connector.ContainerRef{b, a, c, b, a}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := slices.Clone(tt.input)
			got, key, err := connector.CanonicalContainers(tt.input)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, want) || key != wantKey {
				t.Errorf("CanonicalContainers() = %v, %q; want %v, %q", got, key, want, wantKey)
			}
			encoded, err := json.Marshal(got)
			if err != nil || string(encoded) != wantJSON {
				t.Errorf("canonical JSON = %s, %v; want %s", encoded, err, wantJSON)
			}
			if !slices.Equal(tt.input, before) {
				t.Errorf("input changed: got %v, want %v", tt.input, before)
			}
		})
	}
}

func TestEventContainersACL(t *testing.T) {
	refs := func(n int) []connector.ContainerRef {
		out := make([]connector.ContainerRef, n)
		for i := range out {
			out[i] = connector.ContainerRef{Source: "github-acme", NativeID: fmt.Sprintf("acme/repo-%02d", i)}
		}
		return out
	}
	for _, tt := range []struct {
		name   string
		list   []connector.ContainerRef
		mutate func(*connector.ACLEntry)
		want   string
	}{
		{name: "valid", list: refs(1)},
		{name: "maximum", list: refs(16)},
		{name: "duplicate", list: []connector.ContainerRef{refs(1)[0], refs(1)[0]}},
		{name: "unsorted", list: []connector.ContainerRef{refs(2)[1], refs(2)[0]}},
		{name: "key mismatch", list: refs(1), mutate: func(e *connector.ACLEntry) { e.NativeID = strings.Repeat("0", 64) }, want: "does not match"},
		{name: "missing key", list: refs(1), mutate: func(e *connector.ACLEntry) { e.NativeID = "" }, want: "does not match"},
		{name: "uppercase key", list: refs(1), mutate: func(e *connector.ACLEntry) { e.NativeID = strings.ToUpper(e.NativeID) }, want: "does not match"},
		{name: "empty", list: nil, want: "1–16"},
		{name: "empty array", list: []connector.ContainerRef{}, want: "1–16"},
		{name: "too many", list: refs(17), want: "1–16"},
		{name: "too many duplicates", list: slices.Repeat(refs(1), 17), want: "1–16"},
		{name: "invalid source", list: []connector.ContainerRef{{Source: "GitHub", NativeID: "acme/api"}}, want: "source"},
		{name: "missing source", list: []connector.ContainerRef{{NativeID: "acme/api"}}, want: "source"},
		{name: "long source", list: []connector.ContainerRef{{Source: strings.Repeat("a", 65), NativeID: "acme/api"}}, want: "source"},
		{name: "missing native id", list: []connector.ContainerRef{{Source: "github-acme"}}, want: "native_id is empty"},
		{name: "entry source", list: refs(1), mutate: func(e *connector.ACLEntry) { e.Source = "github-acme" }, want: "names a source"},
		{name: "public with containers", list: refs(1), mutate: func(e *connector.ACLEntry) { e.Kind = connector.ACLPublic; e.NativeID = "" }, want: "containers is set"},
		{name: "identity with containers", list: refs(1), mutate: func(e *connector.ACLEntry) { e.Kind = connector.ACLIdentity; e.Source = "github-acme" }, want: "containers is set"},
		{name: "group with containers", list: refs(1), mutate: func(e *connector.ACLEntry) { e.Kind = connector.ACLGroup; e.Source = "github-acme" }, want: "containers is set"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, key, err := connector.CanonicalContainers(tt.list)
			if tt.want == "" && err != nil {
				t.Fatal(err)
			}
			entry := connector.ACLEntry{Kind: connector.ACLContainers, NativeID: key, Containers: tt.list}
			if tt.mutate != nil {
				tt.mutate(&entry)
			}
			event := validEvent()
			event.ACL = connector.ACL{entry}
			err = event.Validate()
			if tt.want == "" {
				if err != nil {
					t.Errorf("Validate() = %v", err)
				}
			} else if !errors.Is(err, connector.ErrInvalidEvent) || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Validate() = %v, want ErrInvalidEvent containing %q", err, tt.want)
			}
		})
	}
}

func TestContainersAndTouchesJSON(t *testing.T) {
	list, key, err := connector.CanonicalContainers([]connector.ContainerRef{{Source: "github-acme", NativeID: "acme/api"}})
	if err != nil {
		t.Fatal(err)
	}
	event := validEvent()
	event.ACL = connector.ACL{{Kind: connector.ACLContainers, NativeID: key, Containers: list}}
	event.Payload.Touches = []connector.Touch{{Source: "github-acme", Project: "acme/api", Paths: []string{"internal/api.go"}}}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"acl":[{"kind":"containers","native_id":"` + key + `","containers":[{"source":"github-acme","native_id":"acme/api"}]}]`,
		`"touches":[{"source":"github-acme","project":"acme/api","paths":["internal/api.go"]}]`,
	} {
		if !strings.Contains(string(encoded), want) {
			t.Errorf("JSON = %s, want %s", encoded, want)
		}
	}
	var back connector.Event
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(event, back) {
		t.Errorf("round trip changed event: got %+v, want %+v", back, event)
	}
	if err := back.Validate(); err != nil {
		t.Errorf("round trip Validate() = %v", err)
	}
}

func TestACLEquality(t *testing.T) {
	entry := connector.ACLEntry{Kind: connector.ACLContainers, NativeID: "key", Containers: []connector.ContainerRef{{Source: "github", NativeID: "acme/api"}}}
	for _, tt := range []struct {
		name   string
		mutate func(*connector.ACLEntry)
		want   bool
	}{
		{"equal separate slices", func(*connector.ACLEntry) {}, true},
		{"kind", func(e *connector.ACLEntry) { e.Kind = connector.ACLIdentity }, false},
		{"source", func(e *connector.ACLEntry) { e.Source = "other" }, false},
		{"key", func(e *connector.ACLEntry) { e.NativeID = "other" }, false},
		{"label", func(e *connector.ACLEntry) { e.Label = "other" }, false},
		{"container", func(e *connector.ACLEntry) { e.Containers[0].NativeID = "acme/web" }, false},
		{"no containers", func(e *connector.ACLEntry) { e.Containers = nil }, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			other := entry
			other.Containers = slices.Clone(entry.Containers)
			tt.mutate(&other)
			if got := entry.Equal(other); got != tt.want {
				t.Errorf("entry Equal() = %v, want %v", got, tt.want)
			}
			if got := (connector.ACL{entry}).Equal(connector.ACL{other}); got != tt.want {
				t.Errorf("ACL Equal() = %v, want %v", got, tt.want)
			}
		})
	}
	if !(connector.ACL(nil)).Equal(connector.ACL{}) {
		t.Error("nil and empty ACLs differ")
	}
	if (connector.ACL{entry}).Equal(nil) {
		t.Error("nonempty and nil ACLs are equal")
	}
}

func TestExistingACLEntryJSON(t *testing.T) {
	for _, tt := range []struct {
		name  string
		entry connector.ACLEntry
		want  string
	}{
		{"public", connector.ACLEntry{Kind: connector.ACLPublic}, `{"kind":"public"}`},
		{"group", connector.ACLEntry{Kind: connector.ACLGroup, Source: "github", NativeID: "acme/api", Label: "collaborators"}, `{"kind":"group","source":"github","native_id":"acme/api","label":"collaborators"}`},
		{"identity", connector.ACLEntry{Kind: connector.ACLIdentity, Source: "github", NativeID: "u1"}, `{"kind":"identity","source":"github","native_id":"u1"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			encoded, err := json.Marshal(tt.entry)
			if err != nil || string(encoded) != tt.want {
				t.Errorf("Marshal() = %s, %v; want %s", encoded, err, tt.want)
			}
			var back connector.ACLEntry
			if err := json.Unmarshal(encoded, &back); err != nil {
				t.Fatal(err)
			}
			if !back.Equal(tt.entry) {
				t.Errorf("JSON round trip changed entry: got %+v, want %+v", back, tt.entry)
			}
		})
	}
}

func TestACLEqualityPreservesFieldAndOrderSemantics(t *testing.T) {
	public := connector.ACLEntry{Kind: connector.ACLPublic}
	group := connector.ACLEntry{Kind: connector.ACLGroup, Source: "github", NativeID: "acme/api", Label: "collaborators"}
	identity := connector.ACLEntry{Kind: connector.ACLIdentity, Source: "github", NativeID: "u1", Label: "reader"}
	containers := connector.ACLEntry{Kind: connector.ACLContainers, NativeID: "key", Containers: []connector.ContainerRef{
		{Source: "github", NativeID: "acme/api"},
		{Source: "github", NativeID: "acme/web"},
	}}
	for _, tt := range []struct {
		name  string
		entry connector.ACLEntry
	}{
		{"public", public},
		{"group", group},
		{"identity", identity},
		{"containers", containers},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, change := range []struct {
				name   string
				mutate func(*connector.ACLEntry)
				want   bool
			}{
				{"equal", func(*connector.ACLEntry) {}, true},
				{"kind differs", func(e *connector.ACLEntry) { e.Kind = "other" }, false},
				{"source differs", func(e *connector.ACLEntry) { e.Source += "-other" }, false},
				{"native id differs", func(e *connector.ACLEntry) { e.NativeID += "-other" }, false},
				{"label differs", func(e *connector.ACLEntry) { e.Label += "-other" }, false},
			} {
				t.Run(change.name, func(t *testing.T) {
					other := tt.entry
					other.Containers = slices.Clone(tt.entry.Containers)
					change.mutate(&other)
					if got := tt.entry.Equal(other); got != change.want {
						t.Errorf("entry Equal() = %v, want %v", got, change.want)
					}
					if got := (connector.ACL{tt.entry}).Equal(connector.ACL{other}); got != change.want {
						t.Errorf("ACL Equal() = %v, want %v", got, change.want)
					}
				})
			}
		})
	}
	for _, tt := range []struct {
		name        string
		left, right connector.ACL
		want        bool
	}{
		{"same order", connector.ACL{public, group, identity, containers}, connector.ACL{public, group, identity, containers}, true},
		{"different order", connector.ACL{public, group, identity}, connector.ACL{identity, group, public}, false},
		{"containers order", connector.ACL{public, containers}, connector.ACL{containers, public}, false},
		{"duplicate entry", connector.ACL{group}, connector.ACL{group, group}, false},
		{"container list order", connector.ACL{containers}, connector.ACL{{Kind: containers.Kind, NativeID: containers.NativeID, Containers: []connector.ContainerRef{containers.Containers[1], containers.Containers[0]}}}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.left.Equal(tt.right); got != tt.want {
				t.Errorf("ACL Equal() = %v, want %v", got, tt.want)
			}
		})
	}
}
