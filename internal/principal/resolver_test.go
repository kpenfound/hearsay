package principal_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/principal"
)

// mapping is the `principals/` a resolver is built from in these tests: two
// people who are the same person in several sources, an agent, and a team that
// claims a GitHub team as its identity.
func mapping() []principal.Principal {
	return []principal.Principal{
		{
			ID:   "kyle",
			Name: "Kyle Penfound",
			Kind: principal.KindHuman,
			Identities: []principal.Identity{
				{Source: "github", NativeID: "MDQ6VXNlcjE=", Handle: "kpenfound"},
				{Source: "discord", NativeID: "302100000000000003", Handle: "kyle"},
				{Source: "drive", Handle: "Kyle@acme.example"},
			},
		},
		{
			ID:   "robin",
			Kind: principal.KindHuman,
			Identities: []principal.Identity{
				{Source: "github", Handle: "robinok"},
				{Source: "discord", Handle: "robin"},
			},
		},
		{
			ID:    "shed",
			Kind:  principal.KindAgent,
			Class: principal.ClassWorker,
			Identities: []principal.Identity{
				{Source: "github", Handle: "shed-agent[bot]"},
			},
		},
		{
			ID:      "api-team",
			Kind:    principal.KindTeam,
			Members: []string{"kyle", "robin", "shed"},
			Identities: []principal.Identity{
				{Source: "github", NativeID: "MDQ6VGVhbTE=", Handle: "acme/api-team"},
			},
		},
		{
			// A team written down the way onboarding actually writes one: the
			// slug a person can type, and no node id. It is the only form
			// CODEOWNERS gives.
			ID:         "eng",
			Kind:       principal.KindTeam,
			Identities: []principal.Identity{{Source: "github", Handle: "acme/eng"}},
		},
	}
}

func newResolver(t *testing.T, principals []principal.Principal) *principal.Resolver {
	t.Helper()
	r, err := principal.NewResolver(principals)
	if err != nil {
		t.Fatalf("NewResolver: %v", err)
	}
	return r
}

func TestResolve(t *testing.T) {
	tests := []struct {
		name       string
		hint       connector.Identity
		want       principal.Status
		wantID     string
		candidates []string
	}{{
		name:   "native id",
		hint:   connector.Identity{Source: "github", Kind: connector.IdentityUser, NativeID: "MDQ6VXNlcjE="},
		want:   principal.Resolved,
		wantID: "kyle",
	}, {
		// The native id is what survives a rename: the source says this user
		// now calls themselves something else, and it is still Kyle.
		name:   "native id wins over a handle nobody claims",
		hint:   connector.Identity{Source: "github", NativeID: "MDQ6VXNlcjE=", Handle: "kyle-p"},
		want:   principal.Resolved,
		wantID: "kyle",
	}, {
		name:   "handle only",
		hint:   connector.Identity{Source: "github", Kind: connector.IdentityUser, Handle: "robinok"},
		want:   principal.Resolved,
		wantID: "robin",
	}, {
		name:   "handle in another case",
		hint:   connector.Identity{Source: "github", Handle: "RobinOK"},
		want:   principal.Resolved,
		wantID: "robin",
	}, {
		name:   "handle with surrounding space",
		hint:   connector.Identity{Source: "github", Handle: "  robinok "},
		want:   principal.Resolved,
		wantID: "robin",
	}, {
		// A calendar attendee or a Drive file's owner arrives as an email and
		// nothing else. It matches the handle an email was written as.
		name:   "email matches a handle",
		hint:   connector.Identity{Source: "drive", Handle: "", Email: "kyle@ACME.example"},
		want:   principal.Resolved,
		wantID: "kyle",
	}, {
		name:   "bot handle",
		hint:   connector.Identity{Source: "github", Kind: connector.IdentityBot, Handle: "shed-agent[bot]"},
		want:   principal.Resolved,
		wantID: "shed",
	}, {
		// Two keys, one principal, is one match and not an ambiguity.
		name:   "every key agrees",
		hint:   connector.Identity{Source: "discord", NativeID: "302100000000000003", Handle: "kyle"},
		want:   principal.Resolved,
		wantID: "kyle",
	}, {
		// The mapping has fallen behind the source: robin took over a handle
		// that is still written down as kyle's. Preferring the native id would
		// hide it for good.
		name:       "keys name different principals",
		hint:       connector.Identity{Source: "discord", NativeID: "302100000000000003", Handle: "robin"},
		want:       principal.Ambiguous,
		candidates: []string{"kyle", "robin"},
	}, {
		name:       "handle and email name different principals",
		hint:       connector.Identity{Source: "discord", Handle: "kyle", Email: "robin"},
		want:       principal.Ambiguous,
		candidates: []string{"kyle", "robin"},
	}, {
		name: "nobody is mapped to it",
		hint: connector.Identity{Source: "github", Kind: connector.IdentityUser, NativeID: "MDQ6VXNlcjk5", Handle: "stranger"},
		want: principal.Unknown,
	}, {
		// Matching is source-scoped: a handle in one source says nothing about
		// a handle in another, and the mapping lists a principal per source.
		name: "right handle, wrong source",
		hint: connector.Identity{Source: "discord", Handle: "robinok"},
		want: principal.Unknown,
	}, {
		name: "no source",
		hint: connector.Identity{Handle: "robinok"},
		want: principal.Unknown,
	}, {
		name: "no keys at all",
		hint: connector.Identity{Source: "github", Kind: connector.IdentityUser},
		want: principal.Unknown,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := newResolver(t, mapping()).Resolve(tt.hint)
			if got.Status != tt.want {
				t.Fatalf("Resolve(%+v).Status = %s, want %s", tt.hint, got.Status, tt.want)
			}
			switch tt.want {
			case principal.Resolved:
				if got.Principal.ID != tt.wantID {
					t.Errorf("resolved to %q, want %q", got.Principal.ID, tt.wantID)
				}
				// The whole principal comes back, not just the id: a caller
				// deciding what an agent may do needs its kind and class.
				if got.Principal.Kind == "" || len(got.Principal.Identities) == 0 {
					t.Errorf("resolved principal = %+v, want the configured entry", got.Principal)
				}
				if !slices.Equal(got.Candidates, []string{tt.wantID}) {
					t.Errorf("Candidates = %v, want just %q", got.Candidates, tt.wantID)
				}
			case principal.Ambiguous:
				if !slices.Equal(got.Candidates, tt.candidates) {
					t.Errorf("Candidates = %v, want %v", got.Candidates, tt.candidates)
				}
				if got.Principal.ID != "" {
					t.Errorf("Principal = %+v, want the zero value on an ambiguous hint", got.Principal)
				}
			case principal.Unknown:
				if got.Principal.ID != "" || len(got.Candidates) != 0 {
					t.Errorf("Resolve = %+v, want nothing", got)
				}
			}
		})
	}
}

// An identity that does not resolve is put in front of a person rather than
// dropped, and one identity seen many times is one entry to map.
func TestUnresolvedIsRecorded(t *testing.T) {
	r := newResolver(t, mapping())

	stranger := connector.Identity{Source: "github", Kind: connector.IdentityUser, NativeID: "MDQ6VXNlcjk5", Handle: "stranger"}
	for range 3 {
		r.Resolve(stranger)
	}
	r.Resolve(connector.Identity{Source: "discord", NativeID: "302100000000000003", Handle: "robin"})
	// A hint that resolves leaves no trace.
	r.Resolve(connector.Identity{Source: "github", Handle: "robinok"})
	// A hint with no keys names nobody, so there is nothing to map.
	r.Resolve(connector.Identity{Source: "github", Kind: connector.IdentityUser})

	got := r.Unresolved()
	if len(got) != 2 {
		t.Fatalf("Unresolved() = %+v, want two entries", got)
	}
	if got[0].Identity.Source != "discord" || got[0].Status != principal.Ambiguous {
		t.Errorf("first entry = %+v, want the ambiguous discord identity", got[0])
	}
	if !slices.Equal(got[0].Candidates, []string{"kyle", "robin"}) {
		t.Errorf("candidates = %v, want kyle and robin", got[0].Candidates)
	}
	if got[0].Count != 1 {
		t.Errorf("count = %d, want 1", got[0].Count)
	}
	if got[1].Identity != stranger || got[1].Status != principal.Unknown {
		t.Errorf("second entry = %+v, want the unknown github identity", got[1])
	}
	if got[1].Count != 3 {
		t.Errorf("count = %d, want the three sightings counted as one entry", got[1].Count)
	}
	if len(got[1].Candidates) != 0 {
		t.Errorf("candidates = %v, want none on an unknown identity", got[1].Candidates)
	}
	if n := r.Overflow(); n != 0 {
		t.Errorf("Overflow() = %d, want 0", n)
	}

	// The result is a copy, all the way down, so a caller may hold it and pick
	// it apart while ingest carries on.
	got[1].Count = 99
	got[0].Candidates[0] = "nobody"
	again := r.Unresolved()
	if again[1].Count != 3 {
		t.Errorf("count after mutating the result = %d, want 3", again[1].Count)
	}
	if !slices.Equal(again[0].Candidates, []string{"kyle", "robin"}) {
		t.Errorf("candidates after mutating the result = %v, want kyle and robin", again[0].Candidates)
	}

	// An entry shows the identity as it was last seen. The source renamed the
	// stranger, and the entry is filed under the native id either way: a
	// person mapping it needs the name the source shows now.
	renamed := stranger
	renamed.Handle = "stranger-2"
	renamed.DisplayName = "A Stranger"
	r.Resolve(renamed)
	if got := r.Unresolved(); got[1].Identity != renamed || got[1].Count != 4 {
		t.Errorf("entry after a rename = %+v, want %+v seen 4 times", got[1], renamed)
	}
}

// Two sightings of one identity can fail differently — the second carries a
// handle the first did not — and the entry says how the last one failed. An
// entry that still said "unknown" would send a person looking for a mapping
// that is there and wrong rather than missing.
func TestUnresolvedTracksTheLatestOutcome(t *testing.T) {
	r := newResolver(t, mapping())
	base := connector.Identity{Source: "discord", Kind: connector.IdentityUser, NativeID: "302199999999999999"}

	r.Resolve(base)
	entry := func(t *testing.T) principal.Unresolved {
		t.Helper()
		got := r.Unresolved()
		if len(got) != 1 {
			t.Fatalf("Unresolved() = %+v, want one entry: every sighting is the same native id", got)
		}
		return got[0]
	}
	if got := entry(t); got.Status != principal.Unknown || len(got.Candidates) != 0 {
		t.Errorf("first sighting = %+v, want unknown with no candidates", got)
	}

	// The source now gives a handle and an email, and they are two people.
	ambiguous := base
	ambiguous.Handle = "kyle"
	ambiguous.Email = "robin"
	r.Resolve(ambiguous)
	if got := entry(t); got.Status != principal.Ambiguous ||
		!slices.Equal(got.Candidates, []string{"kyle", "robin"}) || got.Count != 2 {
		t.Errorf("second sighting = %+v, want ambiguous between kyle and robin, seen twice", got)
	}

	// And back: a sighting that matches nobody clears the candidates rather
	// than leaving two names against an identity that no longer names them.
	unknown := base
	unknown.Handle = "nobody"
	r.Resolve(unknown)
	if got := entry(t); got.Status != principal.Unknown || len(got.Candidates) != 0 || got.Count != 3 {
		t.Errorf("third sighting = %+v, want unknown with no candidates, seen three times", got)
	}
}

// The record is a queue of work, not a log: it is bounded, and says how much it
// dropped rather than pretending the list is complete.
func TestUnresolvedIsBounded(t *testing.T) {
	r := newResolver(t, mapping())
	const extra = 5
	for i := range principal.MaxUnresolved + extra {
		r.Resolve(connector.Identity{Source: "github", NativeID: fmt.Sprintf("bot-%d", i)})
	}
	if n := len(r.Unresolved()); n != principal.MaxUnresolved {
		t.Errorf("Unresolved() holds %d, want %d", n, principal.MaxUnresolved)
	}
	if n := r.Overflow(); n != extra {
		t.Errorf("Overflow() = %d, want %d", n, extra)
	}
	// An identity already recorded is still counted once the record is full.
	r.Resolve(connector.Identity{Source: "github", NativeID: "bot-0"})
	for _, u := range r.Unresolved() {
		if u.Identity.NativeID == "bot-0" && u.Count != 2 {
			t.Errorf("bot-0 count = %d, want 2", u.Count)
		}
	}
	if n := r.Overflow(); n != extra {
		t.Errorf("Overflow() = %d after a repeat sighting, want %d", n, extra)
	}

	// Overflow counts sightings, not the identities they belong to: one new
	// bot posting three times counts three. Counting identities would mean
	// remembering the ones the bound refused to keep.
	for range 3 {
		r.Resolve(connector.Identity{Source: "github", NativeID: "one-new-bot"})
	}
	if n := r.Overflow(); n != extra+3 {
		t.Errorf("Overflow() = %d after three sightings of one new identity, want %d", n, extra+3)
	}
	if n := len(r.Unresolved()); n != principal.MaxUnresolved {
		t.Errorf("Unresolved() holds %d, want the bound to still be %d", n, principal.MaxUnresolved)
	}
}

// Every connector resolves at once, so the mapping is read and the record
// written from many goroutines. Run with -race.
func TestResolveIsConcurrencySafe(t *testing.T) {
	r := newResolver(t, mapping())
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 50 {
				r.Resolve(connector.Identity{Source: "github", Handle: "robinok"})
				r.Resolve(connector.Identity{Source: "github", NativeID: fmt.Sprintf("bot-%d", (i*50+j)%13)})
				r.Unresolved()
				r.Overflow()
			}
		}()
	}
	wg.Wait()
	if n := len(r.Unresolved()); n != 13 {
		t.Errorf("Unresolved() holds %d distinct identities, want 13", n)
	}
	var total int
	for _, u := range r.Unresolved() {
		total += u.Count
	}
	if total != 8*50 {
		t.Errorf("sightings counted = %d, want %d", total, 8*50)
	}
}

// A source-native group — an ACL entry, a team in CODEOWNERS — resolves to the
// team that claims it.
// A source names a group two ways — a node id in an ACL, a slug in CODEOWNERS —
// and configuration accepts either, so both are looked up. A team written down
// with a slug and no node id, which is what onboarding produces and the only
// form CODEOWNERS has, must be reachable.
func TestResolveGroup(t *testing.T) {
	tests := []struct {
		name   string
		group  string
		want   principal.Status
		wantID string
	}{
		{"a node id", "MDQ6VGVhbTE=", principal.Resolved, "api-team"},
		{"the slug of the same team", "acme/api-team", principal.Resolved, "api-team"},
		{"a slug is all a CODEOWNERS team has", "acme/eng", principal.Resolved, "eng"},
		{"a slug in another case", "ACME/Eng", principal.Resolved, "eng"},
		// The `@` is CODEOWNERS syntax, and stripping it is the caller's job.
		{"a slug still wearing its CODEOWNERS @", "@acme/eng", principal.Unknown, ""},
		{"a group nobody claims", "MDQ6VGVhbTk5", principal.Unknown, ""},
		{"no group at all", "", principal.Unknown, ""},
		// A group that resolves to a person is a configuration mistake, and it
		// is reported as resolved all the same: what an ACL means is not
		// decided here.
		{"a group that is really a person", "kpenfound", principal.Resolved, "kyle"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := newResolver(t, mapping()).ResolveGroup("github", tt.group)
			if got.Status != tt.want || got.Principal.ID != tt.wantID {
				t.Errorf("ResolveGroup(%q) = %+v, want %s %q", tt.group, got, tt.want, tt.wantID)
			}
		})
	}

	// An unmapped group is a mapping a person has to write, like any other; a
	// group that resolved leaves no trace, and a group of nothing names nobody
	// to map.
	r := newResolver(t, mapping())
	r.ResolveGroup("github", "MDQ6VGVhbTk5")
	r.ResolveGroup("github", "acme/api-team")
	r.ResolveGroup("github", "")
	if n := len(r.Unresolved()); n != 1 {
		t.Errorf("Unresolved() = %+v, want just the unmapped group", r.Unresolved())
	}
}

// A resolver refuses a mapping that would make it decide authorship by map
// iteration order.
func TestNewResolverRejects(t *testing.T) {
	claimed := func(a, b principal.Identity) []principal.Principal {
		return []principal.Principal{
			{ID: "kyle", Kind: principal.KindHuman, Identities: []principal.Identity{a}},
			{ID: "robin", Kind: principal.KindHuman, Identities: []principal.Identity{b}},
		}
	}
	tests := []struct {
		name       string
		principals []principal.Principal
		wantErr    error
		want       string
	}{{
		name:       "one native id, two principals",
		principals: claimed(principal.Identity{Source: "github", NativeID: "N1"}, principal.Identity{Source: "github", NativeID: "N1"}),
		wantErr:    principal.ErrIdentityClaimed,
	}, {
		name:       "one handle, two principals",
		principals: claimed(principal.Identity{Source: "github", Handle: "kp"}, principal.Identity{Source: "github", Handle: "kp"}),
		wantErr:    principal.ErrIdentityClaimed,
	}, {
		name:       "one handle in two cases",
		principals: claimed(principal.Identity{Source: "github", Handle: "KP"}, principal.Identity{Source: "github", Handle: "kp"}),
		wantErr:    principal.ErrIdentityClaimed,
	}, {
		name:       "duplicate principal id",
		principals: []principal.Principal{{ID: "kyle"}, {ID: "kyle"}},
		want:       "defined twice",
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := principal.NewResolver(tt.principals)
			if err == nil {
				t.Fatal("NewResolver succeeded, want an error")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("error = %v, want %v", err, tt.wantErr)
			}
			if tt.want != "" && !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to mention %q", err, tt.want)
			}
		})
	}

	// The same principal claiming a native id and a handle that happen to be
	// equal is not a conflict: they are different namespaces, and it is one
	// principal either way.
	ok := []principal.Principal{{ID: "kyle", Identities: []principal.Identity{
		{Source: "github", NativeID: "kp", Handle: "kp"},
		{Source: "discord", NativeID: "kp"},
	}}}
	if _, err := principal.NewResolver(ok); err != nil {
		t.Errorf("NewResolver: %v", err)
	}
}

// The identities configuration reports as errors are skipped rather than
// indexed. Two of them would otherwise collide as one key, so a mapping with
// one real mistake in it would also be told that two identities naming nobody
// are the same person — and no resolver would be built at all.
func TestNewResolverSkipsEmptyKeys(t *testing.T) {
	blank := []principal.Identity{{Source: "", Handle: "kpenfound"}, {Source: "discord", Handle: " "}}
	if _, err := principal.NewResolver([]principal.Principal{
		{ID: "kyle", Kind: principal.KindHuman, Identities: blank},
		{ID: "robin", Kind: principal.KindHuman, Identities: blank},
	}); err != nil {
		t.Errorf("NewResolver over two principals with the same unusable identities: %v", err)
	}

	r := newResolver(t, []principal.Principal{
		{ID: "kyle", Kind: principal.KindHuman, Identities: []principal.Identity{
			{Source: "github", Handle: "kpenfound"},
			{Source: ""},
			{Source: "discord", Handle: "  "},
		}},
	})
	for _, hint := range []connector.Identity{
		{Source: "discord"},
		{Source: "discord", Handle: " "},
		{Handle: "kpenfound"},
	} {
		if got := r.Resolve(hint); got.Status != principal.Unknown {
			t.Errorf("Resolve(%+v) = %+v, want unknown", hint, got)
		}
	}
	if got := r.Resolve(connector.Identity{Source: "github", Handle: "kpenfound"}); got.Status != principal.Resolved {
		t.Errorf("Resolve(a good identity) = %+v, want resolved", got)
	}
}

func TestPrincipalLookup(t *testing.T) {
	r := newResolver(t, mapping())
	if p, ok := r.Principal("shed"); !ok || p.Class != principal.ClassWorker {
		t.Errorf("Principal(shed) = %+v, %v", p, ok)
	}
	if _, ok := r.Principal("nobody"); ok {
		t.Error("Principal(nobody) found something")
	}
}

func TestMembersAndExpand(t *testing.T) {
	r := newResolver(t, append(mapping(), principal.Principal{
		ID: "empty-team", Kind: principal.KindTeam, Members: []string{"gone"},
	}))

	if got := ids(r.Members("api-team")); !slices.Equal(got, []string{"kyle", "robin", "shed"}) {
		t.Errorf("Members(api-team) = %v", got)
	}
	// A member nobody configured is skipped: configuration reported it, and
	// the resolver does not invent a principal to stand in for it.
	if got := r.Members("empty-team"); len(got) != 0 {
		t.Errorf("Members(empty-team) = %+v, want none", got)
	}
	// Only a team has members.
	if got := r.Members("kyle"); got != nil {
		t.Errorf("Members(kyle) = %+v, want nil", got)
	}
	if got := r.Members("nobody"); got != nil {
		t.Errorf("Members(nobody) = %+v, want nil", got)
	}

	tests := []struct {
		name string
		ids  []string
		want []string
	}{
		{"a team expands to its members", []string{"api-team"}, []string{"kyle", "robin", "shed"}},
		{"people are left alone", []string{"robin", "kyle"}, []string{"robin", "kyle"}},
		{"order is the order they were written in", []string{"shed", "api-team"}, []string{"shed", "kyle", "robin"}},
		{"a member listed twice appears once", []string{"kyle", "api-team", "kyle"}, []string{"kyle", "robin", "shed"}},
		{"an unconfigured id is skipped", []string{"nobody", "kyle"}, []string{"kyle"}},
		{"nothing expands to nothing", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ids(r.Expand(tt.ids)); !slices.Equal(got, tt.want) {
				t.Errorf("Expand(%v) = %v, want %v", tt.ids, got, tt.want)
			}
		})
	}
}

func ids(ps []principal.Principal) []string {
	if len(ps) == 0 {
		return nil
	}
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.ID
	}
	return out
}
