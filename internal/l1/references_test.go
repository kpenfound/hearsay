package l1_test

import (
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/principal"
)

// testPrincipals is the identity mapping every test here resolves against: two
// people, an agent and a team, each with the node id a source keys on and the
// login a person types.
func testPrincipals(t *testing.T) *principal.Resolver {
	t.Helper()
	resolver, err := principal.NewResolver([]principal.Principal{{
		ID:         "kyle",
		Kind:       principal.KindHuman,
		Identities: []principal.Identity{{Source: source, NativeID: "u1", Handle: "kpenfound"}},
	}, {
		ID:         "sam",
		Kind:       principal.KindHuman,
		Identities: []principal.Identity{{Source: source, NativeID: "u2", Handle: "samr"}},
	}, {
		ID:         "shed",
		Kind:       principal.KindAgent,
		Class:      principal.ClassWorker,
		Identities: []principal.Identity{{Source: source, NativeID: "b1", Handle: "shed-bot"}},
	}, {
		ID:         "api-team",
		Kind:       principal.KindTeam,
		Members:    []string{"kyle", "sam"},
		Identities: []principal.Identity{{Source: source, NativeID: "t1", Handle: "acme/api-team"}},
	}})
	if err != nil {
		t.Fatalf("NewResolver() = %v", err)
	}
	return resolver
}

// testCode is the `code/` vocabulary: how people talk about the code, and the
// entity id each way of talking maps to.
var testCode = []config.CodeEntity{{
	ID:      "code:acme/api:engine",
	Type:    config.TypeModule,
	Name:    "engine",
	Aliases: []string{"the engine", "engine server"},
}, {
	ID:      "code:acme/api:queue",
	Type:    config.TypeModule,
	Name:    "queue",
	Aliases: []string{"the job queue"},
}}

func TestReferences(t *testing.T) {
	tests := []struct {
		name  string
		title string
		text  string
		links []string
		want  []l1.Reference
	}{{
		name: "nothing to point at",
		text: "Looks good to me.",
		want: nil,
	}, {
		name: "a link to a pull request is typed as one",
		text: "superseded by https://github.com/dagger/dagger/pull/1234",
		want: []l1.Reference{{Type: l1.RefPR, ID: "dagger/dagger#1234"}},
	}, {
		name: "a link to an issue, with a fragment, is one reference",
		text: "see https://github.com/dagger/dagger/issues/99#issuecomment-7 and https://github.com/dagger/dagger/issues/99",
		want: []l1.Reference{{Type: l1.RefIssue, ID: "dagger/dagger#99"}},
	}, {
		name: "a link to a commit",
		text: "broken by https://github.com/acme/api/commit/deadbeef",
		want: []l1.Reference{{Type: l1.RefCommit, ID: "acme/api@deadbeef"}},
	}, {
		name: "a link is normalised, so two spellings of one are one reference",
		text: "the runbook is at HTTPS://Wiki.Example/Ops/ and at https://wiki.example/Ops",
		want: []l1.Reference{{Type: l1.RefURL, ID: "https://wiki.example/Ops"}},
	}, {
		name: "an upper-case scheme is still a link, not prose",
		text: "the runbook is at HTTPS://Wiki.Example/Ops",
		want: []l1.Reference{{Type: l1.RefURL, ID: "https://wiki.example/Ops"}},
	}, {
		name: "a fragment is a position in a page, not a different page",
		text: "see https://wiki.example/ops#taking-the-lock and https://wiki.example/ops",
		want: []l1.Reference{{Type: l1.RefURL, ID: "https://wiki.example/ops"}},
	}, {
		name: "credentials are not part of what a link points at",
		text: "status is at https://deploy:hunter2@internal.example.com/status",
		want: []l1.Reference{{Type: l1.RefURL, ID: "https://internal.example.com/status"}},
	}, {
		name: "a secret a link carries is scrubbed like any other stored string",
		text: "callback https://example.test/cb?token=abcd1234efgh",
		want: []l1.Reference{{Type: l1.RefURL, ID: "https://example.test/cb?token=[redacted:secret]"}},
	}, {
		name: "an @-mention starts a word, so an email address is not one",
		text: "ping sam@samr.dev about it",
		want: nil,
	}, {
		name: "an @-mention still resolves next to punctuation",
		text: "(@kpenfound) and @samr, please look",
		want: []l1.Reference{
			{Type: l1.RefPerson, ID: "kyle"},
			{Type: l1.RefPerson, ID: "sam"},
		},
	}, {
		name: "a number inside a link is a position in it, not a tracker item",
		text: "see https://example.test/page#31",
		want: []l1.Reference{{Type: l1.RefURL, ID: "https://example.test/page"}},
	}, {
		name: "a handle inside a link is not a mention",
		text: "the avatar is https://example.test/u/@kpenfound/pic",
		want: []l1.Reference{{Type: l1.RefURL, ID: "https://example.test/u/@kpenfound/pic"}},
	}, {
		name: "an entity name inside a link path is not a system reference",
		text: "the page is https://example.test/engine/docs",
		want: []l1.Reference{{Type: l1.RefURL, ID: "https://example.test/engine/docs"}},
	}, {
		name: "a bare number beside a link is still read as a tracker item",
		text: "same as #31, see https://example.test/page",
		want: []l1.Reference{
			{Type: l1.RefTrackerItem, ID: "acme/api#31"},
			{Type: l1.RefURL, ID: "https://example.test/page"},
		},
	}, {
		name: "an alias next to a letter of another script is not a match",
		text: "the enginé is fine and théengine is fine",
		want: nil,
	}, {
		name: "a link the shape does not cover is the link itself",
		text: "the runbook is at https://wiki.example/ops/runbook?page=2",
		want: []l1.Reference{{Type: l1.RefURL, ID: "https://wiki.example/ops/runbook?page=2"}},
	}, {
		name:  "a link a connector lifted out for us",
		links: []string{"https://github.com/acme/api/pull/7"},
		text:  "see the linked change",
		want:  []l1.Reference{{Type: l1.RefPR, ID: "acme/api#7"}},
	}, {
		name: "a bare number is read against the artifact's own repository",
		text: "same cause as #31",
		want: []l1.Reference{{Type: l1.RefTrackerItem, ID: "acme/api#31"}},
	}, {
		name: "a qualified short reference names its own repository",
		text: "and dagger/dagger#5 has the same shape",
		want: []l1.Reference{{Type: l1.RefTrackerItem, ID: "dagger/dagger#5"}},
	}, {
		name: "a short reference collapses into the link that spells it out",
		text: "reverting #31, see https://github.com/acme/api/pull/31",
		want: []l1.Reference{{Type: l1.RefPR, ID: "acme/api#31"}},
	}, {
		name: "an @-mention becomes the principal, never the handle",
		text: "@kpenfound what do you think? @nobody-here is not in the mapping",
		want: []l1.Reference{{Type: l1.RefPerson, ID: "kyle"}},
	}, {
		name: "an @-mention of a team resolves through the group mapping",
		text: "@acme/api-team owns this",
		want: []l1.Reference{{Type: l1.RefPerson, ID: "api-team"}},
	}, {
		name: "a code entity is matched on its name and on its aliases",
		text: "The Engine takes the lock before the job queue does",
		want: []l1.Reference{
			{Type: l1.RefSystem, ID: "code:acme/api:engine"},
			{Type: l1.RefSystem, ID: "code:acme/api:queue"},
		},
	}, {
		name: "an alias inside a longer word is not a match",
		text: "the engineering team owns queueing",
		want: nil,
	}, {
		name:  "the title is read as well as the text",
		title: "engine: take the lock first",
		text:  "as discussed",
		want:  []l1.Reference{{Type: l1.RefSystem, ID: "code:acme/api:engine"}},
	}, {
		name: "everything at once, sorted and deduplicated",
		text: "@kpenfound @kpenfound the engine broke in #31, see https://github.com/acme/api/pull/40",
		// Sorted by type and then by id, which is what makes the list a
		// value two runs can be compared as.
		want: []l1.Reference{
			{Type: l1.RefPerson, ID: "kyle"},
			{Type: l1.RefPR, ID: "acme/api#40"},
			{Type: l1.RefSystem, ID: "code:acme/api:engine"},
			{Type: l1.RefTrackerItem, ID: "acme/api#31"},
		},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolver := testPrincipals(t)
			ev := event(connector.KindIssue, repo+"#1", at(0), who("u1", "kpenfound"), tt.title, tt.text)
			ev.Payload.Links = tt.links
			if tt.title == "" {
				ev.Payload.Title = "an issue"
			}

			got := l1.References([]connector.Event{ev}, resolver, testCode)
			assertRefs(t, got, tt.want)

			// Extraction is deterministic: the same events always give the same
			// list, which is what lets a document be rebuilt and compare equal
			// to the row already stored.
			again := l1.References([]connector.Event{ev}, resolver, testCode)
			assertRefs(t, again, tt.want)
		})
	}
}

// A source's own mention list is a hint like any other, and it is resolved the
// same way as an @-mention in the text: through the mapping, to a principal, or
// not at all.
func TestReferencesResolveTheMentionsASourceProvides(t *testing.T) {
	resolver := testPrincipals(t)
	ev := event(connector.KindIssue, repo+"#1", at(0), who("u1", "kpenfound"), "an issue", "please look")
	ev.Payload.Mentions = []connector.Identity{
		{Source: source, Kind: connector.IdentityUser, NativeID: "u2"},
		{Source: source, Kind: connector.IdentityUser, NativeID: "u404", Handle: "ghost"},
	}
	assertRefs(t, l1.References([]connector.Event{ev}, resolver, testCode), []l1.Reference{
		{Type: l1.RefPerson, ID: "sam"},
	})
}

// Nothing unresolved is invented and nothing unresolved is dropped in silence:
// the reference list has no entry, and the resolver has the sighting for a
// person to map.
func TestReferencesMintNoPrincipalForAnUnknownHandle(t *testing.T) {
	resolver := testPrincipals(t)
	ev := event(connector.KindIssue, repo+"#1", at(0), who("u1", "kpenfound"), "an issue", "@stranger take a look")
	for _, ref := range l1.References([]connector.Event{ev}, resolver, testCode) {
		if ref.Type == l1.RefPerson && ref.ID != "kyle" {
			t.Errorf("References() minted %q for an identity nothing maps", ref.ID)
		}
	}
	unresolved := resolver.Unresolved()
	if len(unresolved) == 0 {
		t.Fatal("the resolver kept no record of the handle nothing maps")
	}
	var handles []string
	for _, u := range unresolved {
		handles = append(handles, u.Identity.Handle)
	}
	if !contains(handles, "stranger") {
		t.Errorf("the resolver recorded %v, want the handle it could not map", handles)
	}
}

// A resolver is what turns an identity into a principal. A process with no
// `principals/` configuration has none, and a document from one carries no
// people rather than carrying handles.
func TestReferencesWithoutAResolverCarryNoPeople(t *testing.T) {
	ev := event(connector.KindIssue, repo+"#1", at(0), who("u1", "kpenfound"), "an issue", "@kpenfound look")
	for _, ref := range l1.References([]connector.Event{ev}, nil, nil) {
		if ref.Type == l1.RefPerson {
			t.Errorf("References() = %v with no resolver, want no people", ref)
		}
	}
}

// The cap is what keeps one pathological document from joining to everything.
func TestReferencesAreCapped(t *testing.T) {
	var b strings.Builder
	for i := range l1.MaxReferences + 50 {
		b.WriteString(" https://example.test/page/")
		b.WriteString(strings.Repeat("a", 1+i%20))
		b.WriteString("/")
		b.WriteString(itoa(i))
	}
	ev := event(connector.KindIssue, repo+"#1", at(0), who("u1", "kpenfound"), "an issue", b.String())
	got := l1.References([]connector.Event{ev}, nil, nil)
	if len(got) != l1.MaxReferences {
		t.Fatalf("References() returned %d, want the cap %d", len(got), l1.MaxReferences)
	}
	// The cap is applied after sorting, so what survives is the same list every
	// time rather than whichever comment was read first.
	again := l1.References([]connector.Event{ev}, nil, nil)
	assertRefs(t, again, got)
}

func assertRefs(t *testing.T, got, want []l1.Reference) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("References() = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("References()[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func contains(all []string, want string) bool {
	for _, s := range all {
		if s == want {
			return true
		}
	}
	return false
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
