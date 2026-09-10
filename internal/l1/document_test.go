package l1_test

import (
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
)

func TestDocID(t *testing.T) {
	tests := []struct {
		name     string
		source   string
		artifact string
		want     string
		parse    bool // the id parses back to the two it was built from
	}{
		{name: "an issue", source: "github-acme", artifact: "acme/api#12", want: "l1:github-acme:acme/api#12", parse: true},
		{name: "an artifact id with colons in it", source: "github-acme", artifact: "acme/api#31:review:77", want: "l1:github-acme:acme/api#31:review:77", parse: true},
		{name: "a commit", source: "github-acme", artifact: "acme/api@deadbeef", want: "l1:github-acme:acme/api@deadbeef", parse: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := l1.DocID(tt.source, tt.artifact)
			if got != tt.want {
				t.Fatalf("DocID(%q, %q) = %q, want %q", tt.source, tt.artifact, got, tt.want)
			}
			source, artifact, err := l1.ParseDocID(got)
			if err != nil {
				t.Fatalf("ParseDocID(%q) = %v", got, err)
			}
			if source != tt.source || artifact != tt.artifact {
				t.Errorf("ParseDocID(%q) = %q, %q, want %q, %q", got, source, artifact, tt.source, tt.artifact)
			}
		})
	}
}

func TestParseDocIDRefusesWhatIsNotOne(t *testing.T) {
	for _, id := range []string{
		"",
		"acme/api#12",
		"l1:",
		"l1:github-acme",
		"l1:github-acme:",
		"l1:Github Acme:acme/api#12",
		"evt:github-acme:acme/api#12",
	} {
		if _, _, err := l1.ParseDocID(id); err == nil {
			t.Errorf("ParseDocID(%q) = nil, want an error", id)
		}
	}
}

// The kind a document takes is the L0 kind's, or the base kind of an extension
// kind, and everything else is part of somebody else's document.
func TestKindFor(t *testing.T) {
	tests := []struct {
		kind     connector.Kind
		baseKind connector.Kind
		want     l1.Kind
		ok       bool
	}{
		{kind: connector.KindIssue, want: l1.KindIssue, ok: true},
		{kind: connector.KindPullRequest, want: l1.KindPR, ok: true},
		{kind: connector.KindCommit, want: l1.KindCommit, ok: true},
		{kind: connector.KindMessage},
		{kind: connector.KindReview},
		{kind: connector.KindReviewComment},
		{kind: connector.KindTombstone},
		{kind: connector.KindThread},
		{kind: "jira.story", baseKind: connector.KindIssue, want: l1.KindIssue, ok: true},
		{kind: "figma.file", baseKind: connector.KindDocument},
	}
	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			ev := connector.Event{Kind: tt.kind, Payload: connector.Payload{BaseKind: tt.baseKind}}
			got, ok := l1.KindFor(ev)
			if ok != tt.ok || got != tt.want {
				t.Errorf("KindFor(%q) = %q, %v, want %q, %v", tt.kind, got, ok, tt.want, tt.ok)
			}
		})
	}
}

// The outcome kind is the L2 trigger, so which of the five enter the assertion
// pipeline is a rule stated in one place.
func TestOutcomeKinds(t *testing.T) {
	if len(l1.OutcomeKinds) != 5 {
		t.Fatalf("there are %d outcome kinds, want the design's five", len(l1.OutcomeKinds))
	}
	asserting := map[l1.OutcomeKind]bool{l1.OutcomeDecided: true, l1.OutcomeProposed: true, l1.OutcomeResolved: true}
	for _, o := range l1.OutcomeKinds {
		if !o.Valid() {
			t.Errorf("%q is in OutcomeKinds and is not valid", o)
		}
		if o.Asserts() != asserting[o] {
			t.Errorf("%q.Asserts() = %v, want %v", o, o.Asserts(), asserting[o])
		}
		parsed, err := l1.ParseOutcomeKind(string(o))
		if err != nil || parsed != o {
			t.Errorf("ParseOutcomeKind(%q) = %q, %v", o, parsed, err)
		}
	}
	for _, bad := range []string{"", "merged", "Resolved", "none "} {
		if _, err := l1.ParseOutcomeKind(bad); err == nil {
			t.Errorf("ParseOutcomeKind(%q) = nil, want an error", bad)
		}
	}
}

// Validate is what the table enforces, in Go. Each case changes one thing about
// a document that is otherwise fine.
func TestValidate(t *testing.T) {
	good := func() l1.Document {
		return l1.Document{
			ID:      "l1:github-acme:acme/api#12",
			Kind:    l1.KindIssue,
			Source:  l1.Source{System: "github-acme", NativeID: "acme/api#12"},
			L0Refs:  []string{"evt:github-acme:acme%2Fapi%2312"},
			Time:    l1.Times{Created: day, Updated: day, LastActivity: day},
			ACL:     connector.ACL{{Kind: connector.ACLPublic}},
			Text:    "a distillation",
			RawText: "what was said",
			Body:    l1.Body{Summary: "a distillation", OutcomeKind: l1.OutcomeOpen},
		}
	}
	if err := good().Validate(); err != nil {
		t.Fatalf("Validate() on a good document = %v", err)
	}

	tests := []struct {
		name string
		with func(*l1.Document)
		want string
	}{
		{"no kind", func(d *l1.Document) { d.Kind = "" }, "kind"},
		{"a kind this build does not distil", func(d *l1.Document) { d.Kind = "slack_thread" }, "kind"},
		{"an id that is not derived", func(d *l1.Document) { d.ID = "l1:github-acme:something-else" }, "id"},
		{"no source", func(d *l1.Document) { d.Source.System = ""; d.ID = "l1::acme/api#12" }, "source.system"},
		{"no artifact", func(d *l1.Document) { d.Source.NativeID = ""; d.ID = "l1:github-acme:" }, "source.native_id"},
		{"no provenance", func(d *l1.Document) { d.L0Refs = nil }, "l0_refs"},
		{"an empty provenance entry", func(d *l1.Document) { d.L0Refs = []string{""} }, "l0_refs[0]"},
		{"no access list", func(d *l1.Document) { d.ACL = nil }, "acl"},
		{"an access list entry of no kind", func(d *l1.Document) {
			d.ACL = connector.ACL{{Kind: "everyone"}}
		}, "acl[0].kind"},
		{"no time", func(d *l1.Document) { d.Time = l1.Times{} }, "time.created"},
		{"an edit before the artifact", func(d *l1.Document) { d.Time.Updated = day.Add(-1) }, "time.updated"},
		{"activity before the artifact", func(d *l1.Document) { d.Time.LastActivity = day.Add(-1) }, "time.last_activity"},
		{"nothing to search", func(d *l1.Document) { d.RawText = "" }, "raw_text"},
		{"nothing to embed", func(d *l1.Document) { d.Text = "" }, "text"},
		{"an outcome kind that is not one of the five", func(d *l1.Document) { d.Body.OutcomeKind = "merged" }, "outcome_kind"},
		{"a participant with no principal", func(d *l1.Document) {
			d.Participants = []l1.Participant{{Role: connector.RoleAuthor}}
		}, "principal_id"},
		{"a participant with no role", func(d *l1.Document) {
			d.Participants = []l1.Participant{{PrincipalID: "kyle"}}
		}, "role"},
		{"a reference of no type", func(d *l1.Document) {
			d.References = []l1.Reference{{Type: "thing", ID: "x"}}
		}, "references[0]"},
		{"a reference with no id", func(d *l1.Document) {
			d.References = []l1.Reference{{Type: l1.RefPR}}
		}, "references[0]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := good()
			tt.with(&doc)
			err := doc.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want an error about %s", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Validate() = %v, want it to name %s", err, tt.want)
			}
		})
	}
}
