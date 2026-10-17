package l1

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/principal"
)

// Reader is who a read of this table runs as: the effective principal — who
// asked, on whose behalf, and how far that combination reaches — together with
// the access-list entries that principal satisfies.
//
// Both halves are filters and they are not the same mechanism: the reach
// decides what is relevant, the audience decides what may be seen
// (docs/design.md#access-control). A read applies both before it ranks
// anything, so a document the reader may not read never takes a rank.
//
// The zero value reads nothing, which is what failing closed means here: it
// names no principal, so there is nobody the read is for and no query is run.
// An audience that is empty is a different thing and reads what is public — a
// person nobody has mapped into any source is still a person the team's public
// documents are for.
//
// What a reader may do beyond reading is not consulted here. How far it reads
// is: [principal.Reach] composes the person's scopes, the agent's and the
// agent's class into the entity ids Effective.Grant.Scopes holds by the time a
// read runs, and every filter here reads that and nothing else.
type Reader struct {
	// Effective is the principal the read is for. Its Grant.Scopes is the
	// reach: the entity ids the read may read documents about ([Reader.InReach]).
	Effective principal.Effective
	// Audience are the access-list entries this reader satisfies: an
	// `identity` entry for each source-native identity that is them, and a
	// `group` entry for each identity of every team they belong to, and the
	// `containers` entries proven against those base grants. Resolved container
	// entries carry only kind and native_id. A document is readable when its acl
	// holds `public` or any entry here.
	//
	// Entries are compared by (kind, source, native_id) and never by label:
	// the label is what the source calls a grant for a person to read, and two
	// grants naming the same thing are the same grant whatever it is labelled
	// (internal/l1).
	Audience []connector.ACLEntry
}

// ReaderFor is the one place a [Reader] is derived from the identity mapping,
// so that two callers cannot disagree about which grants a principal holds.
//
// The audience is the *human's*, not the agent's, when an agent reads on
// someone's behalf: an agent is not a member of a source's repositories and
// channels, and intersecting its (empty) audience with theirs would make every
// delegated read return nothing. What the agent narrows is the grant, which
// [principal.AgentRead] has already intersected by the time the effective
// principal reaches here, and [principal.Reach] has narrowed further. A
// per-agent audience of its own could only ever narrow what this returns.
//
// A principal the mapping does not hold, or one that is not a person, is an
// error rather than an empty audience: a read nobody is accountable for is not
// a read Hearsay serves.
//
// Container grants present in L1 are resolved once per call against current
// L0 revisions. Every list of every named container must allow the base reader;
// a missing container or a database error never grants access.
func ReaderFor(ctx context.Context, q Querier, res *principal.Resolver, eff principal.Effective) (Reader, error) {
	if res == nil {
		return Reader{}, fmt.Errorf("%w: there is no identity mapping to read %q's grants from", principal.ErrCannotAct, eff.Human)
	}
	who, ok := res.Principal(eff.Human)
	if !ok {
		return Reader{}, fmt.Errorf("%w: %q is not a configured principal", principal.ErrCannotAct, eff.Human)
	}
	if who.Kind != principal.KindHuman {
		return Reader{}, fmt.Errorf("%w: %q is of kind %q, and a read is for a person", principal.ErrCannotAct, who.ID, who.Kind)
	}
	audience := make([]connector.ACLEntry, 0, len(who.Identities))
	for _, id := range who.Identities {
		audience = append(audience, entriesFor(res, connector.ACLIdentity, who.ID, id)...)
	}
	for _, team := range res.Teams(who.ID) {
		for _, id := range team.Identities {
			audience = append(audience, entriesFor(res, connector.ACLGroup, team.ID, id)...)
		}
	}
	for _, source := range res.WorkSources() {
		audience = append(audience, connector.ACLEntry{Kind: connector.ACLIdentity, Source: source, NativeID: who.ID})
	}
	base := Reader{Effective: eff, Audience: sortEntries(audience)}
	entries, err := containerEntries(ctx, q)
	if err != nil {
		return Reader{}, err
	}
	// Only the base audience proves container access. Resolved entries never
	// prove other entries, regardless of the order L1 returned them in.
	resolved := []connector.ACLEntry{}
	access := make(map[connector.ContainerRef]bool)
	for _, entry := range entries {
		containers, key, err := connector.CanonicalContainers(entry.Containers)
		if err != nil || key != entry.NativeID || entry.Source != "" {
			continue
		}
		allowed := true
		for _, container := range containers {
			readable, checked := access[container]
			if !checked {
				lists, err := l0.New(q).ContainerAccess(ctx, container.Source, container.NativeID)
				if err != nil {
					return Reader{}, err
				}
				readable = len(lists) > 0
				for _, acl := range lists {
					readable = readable && base.Allows(acl)
				}
				access[container] = readable
			}
			if !readable {
				allowed = false
				break
			}
		}
		if allowed {
			resolved = append(resolved, connector.ACLEntry{Kind: connector.ACLContainers, NativeID: key})
		}
	}
	base.Audience = sortEntries(append(base.Audience, resolved...))
	return base, nil
}

// Close the L1 result before querying L0, so a single-connection pool and a
// transaction can resolve entries too.
func containerEntries(ctx context.Context, q Querier) ([]connector.ACLEntry, error) {
	if q == nil {
		return nil, fmt.Errorf("reading container grants needs a database")
	}
	rows, err := q.Query(ctx, `SELECT DISTINCT entry
  FROM l1_docs CROSS JOIN LATERAL jsonb_array_elements(acl) entry
 WHERE acl @> '[{"kind":"containers"}]'::jsonb AND entry->>'kind' = 'containers'`)
	if err != nil {
		return nil, fmt.Errorf("reading container grants: %w", err)
	}
	defer rows.Close()
	var entries []connector.ACLEntry
	for rows.Next() {
		var encoded []byte
		if err := rows.Scan(&encoded); err != nil {
			return nil, fmt.Errorf("reading container grant: %w", err)
		}
		var entry connector.ACLEntry
		if err := json.Unmarshal(encoded, &entry); err != nil {
			return nil, fmt.Errorf("decoding container grant: %w", err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading container grants: %w", err)
	}
	return entries, nil
}

// entriesFor is every access-list entry one configured identity satisfies, for
// the principal that identity belongs to.
//
// An entry's `native_id` is the source's id for the thing
// (docs/connector-contract.md), so a configured native id is taken as it
// stands: [principal.NewResolver] refuses two principals claiming one native id
// in one source, and matching in SQL is byte-exact, as it is everywhere an id
// is compared.
//
// A handle is taken as well — a source with no separate id for a group emits
// the group's name, and `acme/api-team` is a handle in the mapping — but only
// where it names this principal and nobody else *in either namespace*
// ([principal.Resolver.Claims]). The mapping keeps native ids and handles apart
// and lets them collide, deliberately: a GitHub node id and a GitHub login are
// different names for different things and either may be any string. An access
// list entry carries one string with no namespace on it, so a handle that
// happens to be somebody else's native id says nothing about who may read, and
// taking it as a grant would hand one person's documents to another. Both
// spellings of a handle are offered, because folding one side of a byte
// comparison and not the other matches nothing.
//
// What is left is fail-closed in both directions. A spelling nobody configured
// matches no entry, and a spelling two principals answer to is a grant to
// neither: the first costs a person a document they may read, and the second is
// the one that has to be impossible (docs/design.md#access-control).
func entriesFor(res *principal.Resolver, kind connector.ACLKind, owner string, id principal.Identity) []connector.ACLEntry {
	if id.Source == "" {
		return nil
	}
	var out []connector.ACLEntry
	if id.NativeID != "" {
		out = append(out, connector.ACLEntry{Kind: kind, Source: id.Source, NativeID: id.NativeID})
	}
	for _, name := range []string{id.Handle, principal.FoldHandle(id.Handle)} {
		if name == "" || name == id.NativeID {
			continue
		}
		if claimed, ok := res.Claims(id.Source, name); !ok || claimed != owner {
			continue
		}
		out = append(out, connector.ACLEntry{Kind: kind, Source: id.Source, NativeID: name})
	}
	return out
}

// sortEntries puts an audience in one order with no duplicates, so that the
// predicate a reader produces is a function of the reader and nothing else.
func sortEntries(entries []connector.ACLEntry) []connector.ACLEntry {
	slices.SortFunc(entries, func(a, b connector.ACLEntry) int {
		if c := strings.Compare(string(a.Kind), string(b.Kind)); c != 0 {
			return c
		}
		if c := strings.Compare(a.Source, b.Source); c != 0 {
			return c
		}
		return strings.Compare(a.NativeID, b.NativeID)
	})
	return slices.CompactFunc(entries, func(a, b connector.ACLEntry) bool {
		return a.Kind == b.Kind && a.Source == b.Source && a.NativeID == b.NativeID
	})
}
