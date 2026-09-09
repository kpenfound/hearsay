package principal

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/kpenfound/hearsay/internal/connector"
)

// MaxUnresolved is how many distinct unresolved identities one resolver
// remembers. The record is a queue of work for a person, not a log: once it
// holds this many, a sighting of an identity that has no entry yet is counted
// by [Resolver.Overflow] and not kept, because the keys come from sources and
// an ingest process that ran for a month would otherwise hold every bot that
// ever posted.
const MaxUnresolved = 4096

// ErrIdentityClaimed is returned by [NewResolver] when two principals claim the
// same identity in the same source. Configuration rejects it where it is
// written; a resolver built from anything else refuses rather than picking one.
var ErrIdentityClaimed = errors.New("identity claimed by two principals")

// Resolver maps the identity hints a connector emits to Hearsay principals.
//
// It is built once from the `principals/` mapping and is then read-only except
// for the record of what it could not resolve, so it is safe for concurrent use
// by every connector at once: the mapping is never written after
// [NewResolver] returns, and the record is behind a mutex.
type Resolver struct {
	// byKey maps a source-scoped identity key to a principal id. It is written
	// only by NewResolver.
	byKey map[identityKey]string
	// byID maps a principal id to the principal. Written only by NewResolver.
	byID map[string]Principal

	mu         sync.Mutex
	unresolved map[identityKey]*Unresolved
	overflow   int
}

// identityKey is one way of naming an identity inside one source. Matching is
// source-scoped: a handle in one source is no evidence about a handle in
// another, and the mapping lists a principal's identity in each source it
// appears in.
type identityKey struct {
	source string
	kind   keyKind
	value  string
}

// keyKind separates the stable id from the typed name, because they live in
// different namespaces: a GitHub node id and a GitHub login could collide as
// bare strings and mean two different people.
type keyKind uint8

const (
	keyNative keyKind = iota
	keyHandle
)

func nativeKey(source, id string) identityKey {
	return identityKey{source: source, kind: keyNative, value: id}
}

// handleKey folds, because handles are case-insensitive to the people who type
// them ([FoldHandle]).
func handleKey(source, handle string) identityKey {
	return identityKey{source: source, kind: keyHandle, value: FoldHandle(handle)}
}

// NewResolver builds a resolver from the configured principals.
//
// It returns [ErrIdentityClaimed] if two principals claim one identity, rather
// than letting map iteration order decide authorship.
//
// An identity with no source, or with nothing but blank keys, is skipped and
// never indexed. Configuration reports it where it is written, and indexing it
// would make two of them collide — reporting a conflict between two identities
// that name nobody, on top of the real error, and refusing to build a resolver
// over it.
func NewResolver(principals []Principal) (*Resolver, error) {
	r := &Resolver{
		byKey:      make(map[identityKey]string, 2*len(principals)),
		byID:       make(map[string]Principal, len(principals)),
		unresolved: make(map[identityKey]*Unresolved),
	}
	for _, p := range principals {
		if _, dup := r.byID[p.ID]; dup {
			return nil, fmt.Errorf("principal %q is defined twice", p.ID)
		}
		r.byID[p.ID] = p
	}
	// The claims are taken in a second pass so that the error names both
	// principals whatever order they were configured in.
	for _, p := range principals {
		for _, id := range p.Identities {
			if id.Source == "" {
				continue
			}
			for _, k := range identityKeys(id) {
				if owner, taken := r.byKey[k]; taken && owner != p.ID {
					return nil, fmt.Errorf("%w: %s in source %q is both %q and %q",
						ErrIdentityClaimed, k.describe(), id.Source, owner, p.ID)
				}
				r.byKey[k] = p.ID
			}
		}
	}
	return r, nil
}

// identityKeys is every key a configured identity is matched by.
func identityKeys(id Identity) []identityKey {
	keys := make([]identityKey, 0, 2)
	if id.NativeID != "" {
		keys = append(keys, nativeKey(id.Source, id.NativeID))
	}
	if FoldHandle(id.Handle) != "" {
		keys = append(keys, handleKey(id.Source, id.Handle))
	}
	return keys
}

func (k identityKey) describe() string {
	if k.kind == keyNative {
		return fmt.Sprintf("native id %q", k.value)
	}
	return fmt.Sprintf("handle %q", k.value)
}

// Principal returns the principal with this id.
func (r *Resolver) Principal(id string) (Principal, bool) {
	p, ok := r.byID[id]
	return p, ok
}

// Status is the outcome of resolving one identity.
type Status uint8

// The resolution outcomes. Callers handle all three: an unknown or ambiguous
// author is recorded for a person to map, never guessed at.
const (
	// Unknown means no configured identity matched.
	Unknown Status = iota
	// Resolved means exactly one principal matched.
	Resolved
	// Ambiguous means the hint's keys matched more than one principal, which
	// is a configuration that has fallen behind the source.
	Ambiguous
)

// String names the status, for logs and error messages.
func (s Status) String() string {
	switch s {
	case Unknown:
		return "unknown"
	case Resolved:
		return "resolved"
	case Ambiguous:
		return "ambiguous"
	default:
		return "invalid"
	}
}

// Resolution is what one identity hint resolved to.
type Resolution struct {
	Status Status
	// Principal is set when Status is [Resolved], and is the zero value
	// otherwise.
	Principal Principal
	// Candidates are the principal ids the hint matched when Status is
	// [Ambiguous], sorted. It holds the single id when Status is [Resolved]
	// and is empty when Status is [Unknown].
	Candidates []string
}

// Resolve maps one identity hint from an event to a principal.
//
// A hint carries up to three keys — the source's stable id, the handle, and the
// email address where the source gives one — and the email is matched against
// handles, because an email address is how a `drive` or calendar identity is
// written down (docs/config.md). Every key that matches is taken, and a hint
// whose keys name different principals is [Ambiguous] rather than being decided
// by preferring the stable id: they disagree only when the mapping has fallen
// behind a rename, and preferring one key would hide that for good instead of
// putting it in front of a person.
//
// Anything that does not resolve is recorded ([Resolver.Unresolved]). The event
// keeps the hint either way — L0 is append-only — so authorship is recovered by
// re-distilling once the mapping is fixed, and no placeholder principal is
// minted that would outlive the gap.
func (r *Resolver) Resolve(hint connector.Identity) Resolution {
	keys := hintKeys(hint)
	res := r.lookup(keys)
	if res.Status != Resolved {
		r.record(keys, hint, res)
	}
	return res
}

// ResolveGroup maps a source-native group — an ACL entry's group, a team named
// in CODEOWNERS — to the principal that claims it, which is a team. It is the
// lookup [connector.ACLEntry] defers to read time: the entry names the group,
// and membership is resolved against this mapping.
//
// The group is matched by both keys, the same way an identity hint is, because
// a source names a group both ways and configuration accepts either: a GitHub
// team is a node id in an ACL and the slug `acme/api-team` in CODEOWNERS, and a
// team that only has a slug written down would otherwise be reachable by no
// lookup at all. Two keys means two principals can match, and then the group is
// [Ambiguous] like any other identity.
//
// Pass the group as the source spells it, with no decoration: `acme/api-team`,
// not the `@acme/api-team` a CODEOWNERS line carries. Stripping the source's
// own syntax is the caller's job, the way a scope's tracker item is a bare
// `1234` and not `#1234`.
//
// A group that resolves to a person or an agent is a configuration mistake, and
// it is reported as [Resolved] all the same: the caller knows what it asked
// for, and this is not the place that decides what an ACL means.
func (r *Resolver) ResolveGroup(source, group string) Resolution {
	keys := groupKeys(source, group)
	res := r.lookup(keys)
	if res.Status != Resolved {
		r.record(keys, connector.Identity{Source: source, NativeID: group}, res)
	}
	return res
}

// groupKeys is every key a source-native group is matched by.
//
// The native key comes first, so that is what an unresolved group is filed
// under, and the filing key therefore never folds. That is the safe way round:
// folding it would file two node ids differing only in case as one group, and a
// node id is base64. The price is that a source spelling one slug two ways
// leaves two entries in the queue, which is noise rather than a wrong answer.
//
// A group with no source, or with nothing but blank space for a name, gets no
// keys at all, so it matches nothing and is not recorded: it names nobody, and
// there is nothing there for a person to map.
func groupKeys(source, group string) []identityKey {
	if source == "" || FoldHandle(group) == "" {
		return nil
	}
	return []identityKey{nativeKey(source, group), handleKey(source, group)}
}

// hintKeys is every key an identity hint is matched by. The email is a handle
// key: docs/config.md writes an email address in an identity's `handle`,
// because it is what a person types.
func hintKeys(hint connector.Identity) []identityKey {
	if hint.Source == "" {
		return nil
	}
	keys := make([]identityKey, 0, 3)
	if hint.NativeID != "" {
		keys = append(keys, nativeKey(hint.Source, hint.NativeID))
	}
	for _, name := range []string{hint.Handle, hint.Email} {
		if FoldHandle(name) == "" {
			continue
		}
		k := handleKey(hint.Source, name)
		if !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	return keys
}

// lookup matches keys against the mapping without recording anything.
func (r *Resolver) lookup(keys []identityKey) Resolution {
	var ids []string
	for _, k := range keys {
		if id, ok := r.byKey[k]; ok && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	switch len(ids) {
	case 0:
		return Resolution{Status: Unknown}
	case 1:
		return Resolution{Status: Resolved, Principal: r.byID[ids[0]], Candidates: ids}
	default:
		slices.Sort(ids)
		return Resolution{Status: Ambiguous, Candidates: ids}
	}
}

// Unresolved is one identity that did not resolve, and how many sightings of it
// did not. It is the queue of mappings a person has to write: an author nobody
// can name is not dropped in silence.
//
// Sightings of one identity are one entry, filed under the strongest key the
// first of them carried. Two sightings can fail differently — one carries a
// handle the other did not — so the entry says how the last one failed, and
// counts the ones that failed. A sighting that resolves is not counted and does
// not clear the entry: the identity still has sightings nobody could attribute,
// and that is the work.
//
// The identity it carries is source data — a display name, an email address —
// so it is subject to the same rule as any payload and is never logged above
// debug (ADR-0008).
type Unresolved struct {
	// Identity is the hint, as it was last seen.
	Identity connector.Identity
	// Status is [Unknown] or [Ambiguous], from the last sighting.
	Status Status
	// Candidates are the principals the last sighting matched, when it was
	// ambiguous.
	Candidates []string
	// Count is how many sightings of this identity did not resolve.
	Count int
}

// record notes an identity that did not resolve. The first key is the identity
// it is filed under, so that one identity seen a thousand times is one entry.
func (r *Resolver) record(keys []identityKey, hint connector.Identity, res Resolution) {
	if len(keys) == 0 {
		// A hint with no keys at all names nobody. It is an invalid event
		// (connector validation requires an author to have a native id), and
		// there is nothing here for a person to map.
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if u, ok := r.unresolved[keys[0]]; ok {
		u.Identity = hint
		u.Status = res.Status
		u.Candidates = res.Candidates
		u.Count++
		return
	}
	if len(r.unresolved) >= MaxUnresolved {
		r.overflow++
		return
	}
	r.unresolved[keys[0]] = &Unresolved{
		Identity:   hint,
		Status:     res.Status,
		Candidates: res.Candidates,
		Count:      1,
	}
}

// Unresolved returns every identity that has not resolved, sorted by source and
// then by the key it was filed under. The result is a copy: a caller may hold
// it while ingest carries on.
func (r *Resolver) Unresolved() []Unresolved {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Unresolved, 0, len(r.unresolved))
	for _, u := range r.unresolved {
		c := *u
		c.Candidates = slices.Clone(u.Candidates)
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b Unresolved) int {
		return cmp.Or(
			strings.Compare(a.Identity.Source, b.Identity.Source),
			strings.Compare(a.Identity.NativeID, b.Identity.NativeID),
			strings.Compare(FoldHandle(a.Identity.Handle), FoldHandle(b.Identity.Handle)),
			strings.Compare(FoldHandle(a.Identity.Email), FoldHandle(b.Identity.Email)),
		)
	})
	return out
}

// Overflow reports how many *sightings* were dropped rather than recorded
// because the record was already holding [MaxUnresolved] identities. A non-zero
// count means the mapping is far enough behind that [Resolver.Unresolved] is a
// sample rather than the list.
//
// It counts sightings and not the identities they belong to — one new bot
// posting three times counts three — because counting identities would mean
// remembering which ones had been seen, and that memory is exactly what the
// bound exists to refuse.
func (r *Resolver) Overflow() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.overflow
}

// Members returns the principals belonging to a team, in the order the team
// lists them, skipping any member that is not configured. It is empty for
// anything that is not a team.
func (r *Resolver) Members(teamID string) []Principal {
	team, ok := r.byID[teamID]
	if !ok || team.Kind != KindTeam {
		return nil
	}
	out := make([]Principal, 0, len(team.Members))
	for _, id := range team.Members {
		if m, ok := r.byID[id]; ok {
			out = append(out, m)
		}
	}
	return out
}

// Expand turns a list of principal ids — a code entity's owners, an authority
// policy's ratifiers — into the individual principals they stand for, replacing
// each team with its members. The result is deduplicated and keeps the order
// the ids were given in, so an owners list reads the way it was written.
//
// A team may not contain a team, so this is one level of expansion and not a
// walk.
func (r *Resolver) Expand(ids []string) []Principal {
	out := make([]Principal, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	add := func(p Principal) {
		if seen[p.ID] {
			return
		}
		seen[p.ID] = true
		out = append(out, p)
	}
	for _, id := range ids {
		p, ok := r.byID[id]
		if !ok {
			continue
		}
		if p.Kind == KindTeam {
			for _, m := range r.Members(id) {
				add(m)
			}
			continue
		}
		add(p)
	}
	return out
}
