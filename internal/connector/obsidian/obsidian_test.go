package obsidian_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/obsidian"
)

func source(root string) connector.SourceConfig {
	settings, _ := json.Marshal(obsidian.Settings{Root: root, Owner: connector.Identity{Source: "people", Kind: connector.IdentityUser, NativeID: "owner-1", Handle: "owner"}, Templates: []string{"Notes/Templates"}})
	return connector.SourceConfig{ID: "vault", Type: obsidian.Type, Containers: []string{"Notes"}, Settings: settings}
}
func withSettings(t *testing.T, src connector.SourceConfig, change func(*obsidian.Settings)) connector.SourceConfig {
	t.Helper()
	var settings obsidian.Settings
	if err := json.Unmarshal(src.Settings, &settings); err != nil {
		t.Fatal(err)
	}
	change(&settings)
	raw, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	src.Settings = raw
	return src
}
func put(t *testing.T, root, name, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}
func gate(src connector.SourceConfig, c *obsidian.Connector, rec *connector.Recorder) *connector.Gate {
	return connector.NewGate(rec, src.ID, c.Describe(), connector.NewAllowlist(src))
}
func walk(t *testing.T, c *obsidian.Connector, sink connector.Sink, from connector.Cursor) []connector.Cursor {
	t.Helper()
	cursors := []connector.Cursor{}
	for i := 0; i < 100; i++ {
		result, err := c.Backfill(t.Context(), sink, from)
		if err != nil {
			t.Fatal(err)
		}
		if result.Done {
			return cursors
		}
		if result.Next == "" {
			t.Fatal("unfinished backfill has no cursor")
		}
		cursors = append(cursors, result.Next)
		from = result.Next
	}
	t.Fatal("backfill did not finish")
	return nil
}
func backfillFixture(t *testing.T) (connector.SourceConfig, []connector.Event) {
	t.Helper()
	root := t.TempDir()
	put(t, root, "Notes/a.md", "---\ntags: [one, two]\nstatus: draft\n---\n# A\n[[B]]\n")
	put(t, root, "Notes/Deep/b.md", "# B")
	put(t, root, "Notes/With Space.md", "# Space")
	put(t, root, "Notes/Templates/blank.md", "secret")
	put(t, root, "Notes/sketch.canvas", "canvas")
	put(t, root, "Notes/image.png", "image")
	put(t, root, "Other/out.md", "other")
	put(t, root, ".obsidian/config.md", "config")
	put(t, root, ".trash/deleted.md", "deleted")
	outside := t.TempDir()
	put(t, outside, "escape.md", "escape")
	if err := os.Symlink(outside, filepath.Join(root, "Notes", "Link")); err != nil {
		t.Fatal(err)
	}
	src := source(root)
	c, err := obsidian.New(src)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(t.Context())
	rec := &connector.Recorder{}
	walk(t, c, gate(src, c, rec), "")
	return src, rec.Events()
}

func TestBackfillEmitsOnlyAllowlistedMarkdown(t *testing.T) {
	_, events := backfillFixture(t)
	ids := []string{}
	for _, ev := range events {
		ids = append(ids, ev.Payload.Artifact)
	}
	if !slices.Equal(ids, []string{"Notes/Deep/b.md", "Notes/With%20Space.md", "Notes/a.md"}) {
		t.Fatalf("artifacts = %v", ids)
	}
}

func TestBackfillEventPayload(t *testing.T) {
	_, events := backfillFixture(t)
	a := events[2]
	if a.Payload.Container.NativeID != "Notes" || a.Payload.Text != "---\ntags: [one, two]\nstatus: draft\n---\n# A\n[[B]]\n" || a.Payload.Author.NativeID != "owner-1" || a.Payload.Author.Source != "people" {
		t.Errorf("event = %+v", a)
	}
	var native map[string]any
	if err := json.Unmarshal(a.Payload.Native, &native); err != nil || native["status"] != "draft" {
		t.Errorf("frontmatter = %s, %v", a.Payload.Native, err)
	}
	if events[0].Payload.Container.NativeID != "Notes/Deep" {
		t.Errorf("nested container = %+v", events[0].Payload.Container)
	}
}

func TestBackfillEventIdentity(t *testing.T) {
	src, events := backfillFixture(t)
	a := events[2]
	if !strings.HasPrefix(a.NativeID, "Notes/a.md@") || a.ID != connector.EventID(src.ID, a.NativeID) {
		t.Errorf("identity = %q %q", a.NativeID, a.ID)
	}
}

func TestBackfillEventACLNamesOwnerAlone(t *testing.T) {
	_, events := backfillFixture(t)
	a := events[2]
	if len(a.ACL) != 1 || a.ACL[0].Kind != connector.ACLIdentity || a.ACL[0].NativeID != "owner-1" {
		t.Errorf("acl = %+v", a.ACL)
	}
}

func TestBackfillIsBoundedAndCursorSurvivesRestart(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 220; i++ {
		put(t, root, filepath.Join("Notes", strings.Repeat("x", 3), string(rune('a'+i/26))+string(rune('a'+i%26))+".md"), "note")
	}
	src := source(root)
	c, err := obsidian.New(src)
	if err != nil {
		t.Fatal(err)
	}
	rec := &connector.Recorder{}
	first, err := c.Backfill(t.Context(), gate(src, c, rec), "")
	if err != nil || first.Done || first.Events > 100 || first.Next == "" {
		t.Fatalf("first page = %+v, %v", first, err)
	}
	c.Close(t.Context())
	c, err = obsidian.New(src)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(t.Context())
	walk(t, c, gate(src, c, rec), first.Next)
	if len(rec.Events()) != 220 {
		t.Fatalf("events = %d, want 220", len(rec.Events()))
	}
}

func TestACLPermissionRevision(t *testing.T) {
	root := t.TempDir()
	put(t, root, "Notes/a.md", "same content")
	src := source(root)
	rec := &connector.Recorder{}
	c, err := obsidian.New(src)
	if err != nil {
		t.Fatal(err)
	}
	walk(t, c, gate(src, c, rec), "")
	c.Close(t.Context())
	src = withSettings(t, src, func(s *obsidian.Settings) { s.Public = true })
	c, err = obsidian.New(src)
	if err != nil {
		t.Fatal(err)
	}
	walk(t, c, gate(src, c, rec), "")
	c.Close(t.Context())
	if len(rec.Events()) != 2 || rec.Events()[0].NativeID == rec.Events()[1].NativeID || rec.Events()[1].ACL[0].Kind != connector.ACLPublic {
		t.Fatalf("permission revisions = %+v", rec.Events())
	}
	src = withSettings(t, src, func(s *obsidian.Settings) {
		s.Public = false
		s.Owner.NativeID = "owner-2"
	})
	c, err = obsidian.New(src)
	if err != nil {
		t.Fatal(err)
	}
	walk(t, c, gate(src, c, rec), "")
	c.Close(t.Context())
	if rec.Events()[2].ACL[0].NativeID != "owner-2" || rec.Events()[2].NativeID == rec.Events()[0].NativeID {
		t.Fatalf("owner revision = %+v", rec.Events()[2])
	}
}

func TestFolderWithSpaceIsIngested(t *testing.T) {
	root := t.TempDir()
	put(t, root, "Side Projects/Hearsay.md", "# Hearsay")
	put(t, root, "Side Projects/Deep Dive/notes.md", "# Notes")
	src := source(root)
	src.Containers = []string{"Side Projects"}
	c, err := obsidian.New(src)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(t.Context())
	rec := &connector.Recorder{}
	walk(t, c, gate(src, c, rec), "")
	titles := []string{}
	for _, ev := range rec.Events() {
		titles = append(titles, ev.Payload.Title)
	}
	slices.Sort(titles)
	if !slices.Equal(titles, []string{"Hearsay.md", "notes.md"}) {
		t.Fatalf("ingested %v, want both notes under the spaced folder", titles)
	}
}

func TestInvalidConfigurationAndPathSafety(t *testing.T) {
	root := t.TempDir()
	put(t, root, "Notes/a.md", "a")
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		change func(*connector.SourceConfig)
	}{
		{"missing root", func(s *connector.SourceConfig) {
			s.Settings = []byte(`{"owner":{"source":"people","kind":"user","native_id":"x"}}`)
		}},
		{"missing owner", func(s *connector.SourceConfig) { s.Settings = []byte(`{"root":"` + root + `"}`) }},
		{"parent traversal", func(s *connector.SourceConfig) { s.Containers = []string{"../other"} }},
		{"absolute container", func(s *connector.SourceConfig) { s.Containers = []string{"/other"} }},
		{"wildcard", func(s *connector.SourceConfig) { s.Containers = []string{"*"} }},
		{"control character", func(s *connector.SourceConfig) { s.Containers = []string{"Notes\tx"} }},
		{"escaping symlink", func(s *connector.SourceConfig) { s.Containers = []string{"link"} }},
		{"missing folder", func(s *connector.SourceConfig) { s.Containers = []string{"missing"} }},
		{"no folders", func(s *connector.SourceConfig) { s.Containers = nil }},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			s := source(root)
			tt.change(&s)
			c, err := obsidian.New(s)
			if err == nil {
				c.Close(t.Context())
				t.Fatal("New accepted unsafe config")
			}
		})
	}
}
