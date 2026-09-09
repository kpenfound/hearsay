package config

import "slices"

// AnyValue is the entry that widens a list of principals, sources or scopes to
// all of them. It is spelled the way the ingest allowlist spells it
// ([connector.AllowAll]), and like that one it is a choice a person makes
// rather than a default.
const AnyValue = "*"

// ArtifactClass is the kind of artifact a stance came out of, for the purpose
// of authority. It is coarser than an L0 kind and than an L1 kind on purpose:
// authority is a judgement about *where* something was said — a merged change,
// a meeting, a chat thread, a DM — and a merged pull request and an open one
// are the same L0 kind with very different weight.
//
// The mapping from L1 documents onto these classes is in docs/config.md and is
// what L1 has to satisfy.
type ArtifactClass string

// The artifact classes. The set is closed: a class nothing can ever produce is
// a silent typo in an authority policy, so the loader rejects an unknown one.
const (
	// ArtifactMergedPR is a change proposal that was merged. It is the
	// artifact the design doc names as authoritative: the team did not merely
	// say it, they shipped it.
	ArtifactMergedPR ArtifactClass = "merged_pr"
	// ArtifactSpec is a wiki page, design doc or ADR — something written to be
	// referred back to.
	ArtifactSpec ArtifactClass = "spec"
	// ArtifactMeeting is a segment of a meeting transcript.
	ArtifactMeeting ArtifactClass = "meeting"
	// ArtifactIssue is a tracker item and its comments.
	ArtifactIssue ArtifactClass = "issue"
	// ArtifactPullRequest is a change proposal that was not merged, and the
	// reviews on it.
	ArtifactPullRequest ArtifactClass = "pull_request"
	// ArtifactCommit is a commit on a watched branch.
	ArtifactCommit ArtifactClass = "commit"
	// ArtifactChatThread is a thread or burst in an ingested channel.
	ArtifactChatThread ArtifactClass = "chat_thread"
	// ArtifactDM is a direct message. DMs are not ingested unless a person
	// lists them (docs/design.md#access-control), and they rank last among
	// human artifacts when they are.
	ArtifactDM ArtifactClass = "dm"
	// ArtifactAgent is an agent turn or an assertion an agent wrote. It ranks
	// below every human artifact: agents propose, people decide.
	ArtifactAgent ArtifactClass = "agent"
)

// defaultRanking is the source ranking every scope gets unless it overrides it,
// highest authority first. The design doc fixes four of the positions — a
// merged pull request outranks a meeting, which outranks a chat thread, which
// outranks a DM (docs/design.md#topics-and-stances) — and the rest follow the
// same principle: something written down deliberately outranks something said
// in passing, and an agent's own words outrank nothing.
var defaultRanking = []ArtifactClass{
	ArtifactMergedPR,
	ArtifactSpec,
	ArtifactMeeting,
	ArtifactIssue,
	ArtifactPullRequest,
	ArtifactCommit,
	ArtifactChatThread,
	ArtifactDM,
	ArtifactAgent,
}

// ArtifactClasses returns every artifact class, highest default authority
// first.
func ArtifactClasses() []ArtifactClass { return slices.Clone(defaultRanking) }

// Valid reports whether c is one of the artifact classes.
func (c ArtifactClass) Valid() bool { return slices.Contains(defaultRanking, c) }

// Policy is the authority in force for one scope: how artifacts rank against
// each other when stances disagree, and what makes a stance ratified rather
// than inferred (docs/design.md#topics-and-stances).
type Policy struct {
	// Scope is the scope this policy is for, or [AnyValue] for the default
	// that every other scope inherits from.
	Scope string
	// Ranking is the artifact classes in authority order, highest first. A
	// class that is not listed ranks below every class that is.
	Ranking []ArtifactClass
	// RatifiedBy is what can make a stance ratified in this scope.
	RatifiedBy Ratifiers
}

// Ratifiers is what may produce a ratified stance: agents act on ratified
// stances without asking, so this is the most consequential thing in the
// configuration.
type Ratifiers struct {
	// Principals may ratify by hand — the emoji reaction, the slash command
	// (docs/design.md#human-feedback-loop). [AnyValue] lets anyone who can
	// write to the scope ratify; an empty list lets nobody.
	Principals []string
	// Sources restricts which sources an artifact may come from for the
	// Artifacts rule below to apply. [AnyValue] accepts any source. It does
	// not restrict Principals: a person ratifying is a person, not a source.
	Sources []string
	// Artifacts are the artifact classes that ratify on their own, with no
	// human in the loop, because landing there is itself the team's decision.
	Artifacts []ArtifactClass
}

// DefaultPolicy is the authority in force where nothing is configured: the
// default ranking, anybody may ratify by hand, and a merged pull request
// ratifies on its own.
//
// A frozen spec and CODEOWNERS are named alongside a merged pull request in the
// design doc, but Hearsay cannot tell a frozen spec from a draft one, so adding
// `spec` is a deliberate one-line override rather than a default.
func DefaultPolicy() Policy {
	return Policy{
		Scope:   AnyValue,
		Ranking: slices.Clone(defaultRanking),
		RatifiedBy: Ratifiers{
			Principals: []string{AnyValue},
			Sources:    []string{AnyValue},
			Artifacts:  []ArtifactClass{ArtifactMergedPR},
		},
	}
}

// Rank is c's authority under this policy: higher outranks lower. The second
// result is false for a class the policy does not rank, which ranks 0 — below
// everything it does rank, and level with every other unranked class.
func (p Policy) Rank(c ArtifactClass) (int, bool) {
	i := slices.Index(p.Ranking, c)
	if i < 0 {
		return 0, false
	}
	return len(p.Ranking) - i, true
}

// Outranks reports whether an artifact of class a beats one of class b when
// their stances disagree. Two classes of equal rank do not outrank each other,
// which is what leaves the tie to be broken by time.
func (p Policy) Outranks(a, b ArtifactClass) bool {
	ra, _ := p.Rank(a)
	rb, _ := p.Rank(b)
	return ra > rb
}

// RatifiedByPrincipal reports whether this principal may ratify a stance in the
// scope.
func (p Policy) RatifiedByPrincipal(id string) bool {
	if id == "" {
		return false
	}
	return slices.Contains(p.RatifiedBy.Principals, id) ||
		slices.Contains(p.RatifiedBy.Principals, AnyValue)
}

// RatifiedByArtifact reports whether a stance evidenced by an artifact of this
// class, from this source, is ratified with no human in the loop.
func (p Policy) RatifiedByArtifact(c ArtifactClass, source string) bool {
	if source == "" || !slices.Contains(p.RatifiedBy.Artifacts, c) {
		return false
	}
	return slices.Contains(p.RatifiedBy.Sources, source) ||
		slices.Contains(p.RatifiedBy.Sources, AnyValue)
}

// Authority is the `authority/` directory: the default policy and the scopes
// that override it. Its zero value is the default policy for every scope, so a
// configuration with no `authority/` still answers every question asked of it.
type Authority struct {
	// def is the policy for scopes with no policy of their own. It is
	// [DefaultPolicy] with whatever the `*` policy set on top of it.
	def Policy
	// scopes holds the per-scope policies, each already merged onto def, so a
	// lookup is one map read and nothing is merged twice.
	scopes map[string]Policy
}

// ForScope is the policy in force for a scope.
func (a Authority) ForScope(scope string) Policy {
	if p, ok := a.scopes[scope]; ok {
		return p
	}
	return a.Default()
}

// Default is the policy scopes with no policy of their own get. On a zero
// Authority — no `authority/` directory at all — it is [DefaultPolicy]; a
// ranking is never empty in a loaded one, because the loader rejects an empty
// one rather than letting it mean two things.
func (a Authority) Default() Policy {
	if len(a.def.Ranking) == 0 {
		return DefaultPolicy()
	}
	return a.def
}

// Scopes returns the scope ids that have a policy of their own, in no
// particular order.
func (a Authority) Scopes() []string {
	ids := make([]string, 0, len(a.scopes))
	for id := range a.scopes {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// newAuthority merges the configured policies onto [DefaultPolicy]. A policy
// sets a field or inherits it; nothing is appended to an inherited list,
// because a ranking half-inherited from somewhere else is not readable from the
// file in front of you.
func newAuthority(policies []Policy) Authority {
	a := Authority{def: DefaultPolicy()}
	for _, p := range policies {
		if p.Scope == AnyValue {
			a.def = mergePolicy(a.def, p)
		}
	}
	for _, p := range policies {
		if p.Scope == AnyValue {
			continue
		}
		if a.scopes == nil {
			a.scopes = make(map[string]Policy, len(policies))
		}
		a.scopes[p.Scope] = mergePolicy(a.def, p)
	}
	return a
}

// mergePolicy layers over onto base. A nil list is unset and inherits; a list
// that is present but empty is a decision and is kept, which is how a scope
// says that nobody may ratify by hand.
func mergePolicy(base, over Policy) Policy {
	merged := base
	merged.Scope = over.Scope
	if over.Ranking != nil {
		merged.Ranking = over.Ranking
	}
	if over.RatifiedBy.Principals != nil {
		merged.RatifiedBy.Principals = over.RatifiedBy.Principals
	}
	if over.RatifiedBy.Sources != nil {
		merged.RatifiedBy.Sources = over.RatifiedBy.Sources
	}
	if over.RatifiedBy.Artifacts != nil {
		merged.RatifiedBy.Artifacts = over.RatifiedBy.Artifacts
	}
	return merged
}
