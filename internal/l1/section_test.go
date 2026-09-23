package l1_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
)

func TestSplitWikiSections(t *testing.T) {
	const page = "---\ntags: [#alpha]\n# YAML comment\n---\nIntro [[Home]]\n# Alpha\nDecision here.\n## Detail\nNested fact.\n```md\n# code, not a heading\n```\n# Beta\nUnrelated outcome."
	sections := l1.SplitWikiSections("file-1", page)
	if len(sections) != 4 {
		t.Fatalf("sections = %+v", sections)
	}
	for i, want := range []string{"---\ntags: [#alpha]\n# YAML comment\n---\nIntro [[Home]]", "# Alpha\nDecision here.", "## Detail\nNested fact.\n```md\n# code, not a heading\n```", "# Beta\nUnrelated outcome."} {
		if sections[i].Text != want {
			t.Errorf("section %d = %q, want %q", i, sections[i].Text, want)
		}
	}
	if sections[0].Heading != "" || sections[1].Heading != "Alpha" || sections[2].Heading != "Detail" {
		t.Errorf("headings = %+v", sections)
	}
	if got := l1.SplitWikiSections("file-1", "No headings.\n#tag [[Wiki]]"); len(got) != 1 || got[0].Text != "No headings.\n#tag [[Wiki]]" || got[0].Key != sections[0].Key {
		t.Errorf("no-heading fallback = %+v", got)
	}
	changed := l1.SplitWikiSections("file-1", strings.Replace(page, "# Alpha", "# Renamed", 1))
	if changed[1].Key == sections[1].Key || changed[2].Key != sections[2].Key || changed[3].Key != sections[3].Key {
		t.Errorf("heading edit changed unrelated identities: %+v", changed)
	}
	if !slices.EqualFunc(sections, l1.SplitWikiSections("file-1", page), func(a, b l1.WikiSection) bool { return a == b }) {
		t.Fatal("same input changed sections")
	}
	duplicate := l1.SplitWikiSections("file-1", "# Same\nFirst.\n# Same\nSecond.")
	if len(duplicate) != 2 || duplicate[0].Key == duplicate[1].Key || duplicate[0].Key == l1.SplitWikiSections("file-2", "# Same\nFirst.")[0].Key {
		t.Errorf("duplicate or source identities collided: %+v", duplicate)
	}
}

func TestBuildWikiSections(t *testing.T) {
	root := event(connector.KindDocument, "file-1", at(0), who("u1", "kpenfound"), "Design", "# A\nDecision on #31.\n# B\nNotes with token=longsecretvalue and [[Wiki]].")
	root.ACL = connector.ACL{{Kind: connector.ACLGroup, Source: source, NativeID: "team"}}
	docs, err := l1.BuildWikiSections(root, testPrincipals(t), testRepo)
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 2 {
		t.Fatalf("documents = %+v", docs)
	}
	for _, doc := range docs {
		if doc.Kind != l1.KindWikiSection || doc.ID != l1.DocID(source, doc.Source.NativeID) || doc.Source.URL != root.Payload.URL {
			t.Errorf("envelope = %+v", doc)
		}
		if !slices.Equal(doc.L0Refs, []string{connector.EventID(source, root.NativeID)}) || !slices.Equal(doc.ACL, root.ACL) {
			t.Errorf("provenance/ACL = %+v", doc)
		}
	}
	if !slices.Contains(docs[0].References, l1.Reference{Type: l1.RefTrackerItem, ID: repo + "#31"}) || slices.Contains(docs[1].References, l1.Reference{Type: l1.RefTrackerItem, ID: repo + "#31"}) {
		t.Errorf("references crossed section boundary: %+v", docs)
	}
	if strings.Contains(docs[1].RawText, "longsecretvalue") || !strings.Contains(docs[1].RawText, "[[Wiki]]") {
		t.Errorf("scrub or wikilink = %q", docs[1].RawText)
	}
}
