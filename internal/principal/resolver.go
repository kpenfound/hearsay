package principal

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/kpenfound/hearsay/internal/connector"
)

// ErrIdentityClaimed is returned by [NewResolver] when two principals claim the
// same identity in the same source. Configuration rejects it where it is
// written; a resolver built from anything else refuses rather than picking one.
var ErrIdentityClaimed = errors.New("identity claimed by two principals")

// Resolver maps the identity hints a connector emits to Hearsay principals.
//
// It is built once from the `principals/` mapping and is read-only after
// [NewResolver] returns, so it is safe for concurrent use by every connector at
// once.
type Resolver struct {
	byKey       map[identityKey]string
	byID        map[string]Principal
	workSources []string
}

type identityKey struct {
	source string
	kind   keyKind
	value  string
}

type keyKind uint8

const (
	keyNative keyKind = iota
	keyHandle
)

func nativeKey(source, id string) identityKey {
	return identityKey{source: source, kind: keyNative, value: id}
}

func handleKey(source, handle string) identityKey {
	return identityKey{source: source, kind: keyHandle, value: FoldHandle(handle)}
}

// NewResolver builds a resolver from the configured principals and sources.
// Work sources use principal ids as native identities without explicit mapping;
// every other source, including agent sessions, uses only configured identities.
//
// It returns [ErrIdentityClaimed] if two principals claim one identity, rather
// than letting map iteration order decide authorship.
//
// An identity with no source, or with nothing but blank keys, is skipped and
// never indexed. Configuration reports it where it is written, and indexing it
// would make two of them collide — reporting a conflict between two identities
// that name nobody, on top of the real error, and refusing to build a resolver
// over it.
func NewResolver(principals []Principal, sources ...connector.SourceConfig) (*Resolver, error) {
	r := &Resolver{
		byKey: make(map[identityKey]string, 2*len(principals)),
		byID:  make(map[string]Principal, len(principals)),
	}
	for _, source := range sources {
		if source.Type == "work" {
			r.workSources = append(r.workSources, source.ID)
		}
	}
	slices.Sort(r.workSources)
	r.workSources = slices.Compact(r.workSources)
	for _, p := range principals {
		if _, dup := r.byID[p.ID]; dup {
			return nil, fmt.Errorf("principal %q is defined twice", p.ID)
		}
		r.byID[p.ID] = p
		for _, source := range r.workSources {
			r.byKey[nativeKey(source, p.ID)] = p.ID
		}
	}
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

// WorkSources returns the configured sources whose native identities are
// principal ids. Agent session sources retain their explicit identity mapping.
func (r *Resolver) WorkSources() []string { return slices.Clone(r.workSources) }

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
// An identity that does not resolve is left unresolved: the event keeps the
// hint — L0 is append-only — so authorship is recovered by re-distilling once
// the mapping is fixed, and no placeholder principal is minted that would
// outlive the gap.
func (r *Resolver) Resolve(hint connector.Identity) Resolution {
	return r.lookup(hintKeys(hint))
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
	return r.lookup(groupKeys(source, group))
}

func groupKeys(source, group string) []identityKey {
	if source == "" || FoldHandle(group) == "" {
		return nil
	}
	return []identityKey{nativeKey(source, group), handleKey(source, group)}
}

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

// Claims reports which principal a source's own spelling of a name belongs to,
// taking the two namespaces together: the spelling is looked up as a native id
// and as a handle, and the answer is a principal only when both agree or only
// one of them matches at all.
//
// It is what a caller needs when it holds a name a source wrote down and cannot
// tell which namespace the source meant — an access list entry's `native_id`,
// which the contract says is the source's id but which a source with no
// separate id for a group fills with the group's name
// (docs/connector-contract.md). [NewResolver] refuses two principals claiming
// one native id, and refuses two claiming one handle, but a handle and a native
// id are different namespaces and may legitimately collide: this is how a
// caller that cannot honour that distinction finds out that it has one.
//
// False means the spelling names nobody, or names more than one principal. Both
// are answers a caller reading on somebody's behalf has to treat the same way,
// which is to grant nothing.
func (r *Resolver) Claims(source, spelling string) (string, bool) {
	res := r.lookup([]identityKey{nativeKey(source, spelling), handleKey(source, spelling)})
	if res.Status != Resolved {
		return "", false
	}
	return res.Principal.ID, true
}

// Teams returns the teams that list this principal as a member, sorted by id.
// It is the reverse of [Resolver.Members], and it is what a read needs: a
// document's access list names groups, and whether the caller is in one is a
// question about which teams they belong to.
//
// A team may not contain a team, so this is one level and not a walk, and a
// team asked about its own memberships has none.
func (r *Resolver) Teams(id string) []Principal {
	var out []Principal
	for _, p := range r.byID {
		if p.Kind != KindTeam || !slices.Contains(p.Members, id) {
			continue
		}
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b Principal) int { return strings.Compare(a.ID, b.ID) })
	return out
}
