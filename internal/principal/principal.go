package principal

import (
	"slices"
	"strings"

	"github.com/kpenfound/hearsay/internal/connector"
)

// Principal is one person, one agent or one team: what a stance's author, a
// code entity's owner and an authority policy all name.
//
// It is the parsed form of one entry of the configuration repository's
// `principals/` (docs/config.md), and it lives here rather than in
// internal/config because the identity model is what the rest of Hearsay
// consumes, the same way sources come out of configuration as
// [connector.SourceConfig] values.
type Principal struct {
	// ID is the Hearsay principal id, minted by hand in configuration: 1 to 64
	// bytes of lowercase letters, digits, `-` and `_`, starting with a letter
	// or a digit ([ValidID]). Nothing derives it from a source, because a
	// principal outlives any one source's idea of who they are.
	ID string
	// Name is the person's, agent's or team's name, for a human reading
	// configuration. It is a display name, never a key.
	Name string
	// Kind is what sort of principal this is, because what each may do differs.
	Kind Kind
	// Class is an agent's access class (docs/design.md#access-control). It is
	// required on an agent and empty on a human and on a team.
	Class Class
	// Identities are the source-native identities that are this principal: the
	// mapping [Resolver] resolves an event's identity hints against.
	Identities []Identity
	// Members are the principal ids belonging to a team, and are empty on
	// everything else. A team is a container, so its members are people and
	// agents: a team may not contain a team.
	Members []string
}

// EntityID is the id this principal has as an L2 entity of type `person`,
// `agent` or `team` (docs/design.md#entities). Entity ids are namespaced by
// what they name — `code:`, `tracker:` — so a principal's is namespaced by its
// kind, and the bare [Principal.ID] is what owners, authority policies and an
// L1 participant use.
//
// It is empty for a principal whose kind is unknown, because an id that cannot
// say what it names is worse than no id.
func (p Principal) EntityID() string {
	switch p.Kind {
	case KindHuman:
		return "person:" + p.ID
	case KindAgent, KindTeam:
		return string(p.Kind) + ":" + p.ID
	default:
		return ""
	}
}

// Identity is one principal in one source, as configuration writes it down. It
// is the mapping; [connector.Identity] is the hint an event carries, and
// [Resolver] is what turns the second into a principal using the first.
type Identity struct {
	// Source is the source id the identity belongs to. Matching is scoped to
	// it: a handle in one source says nothing about a handle in another.
	Source string
	// NativeID is the source's stable id for the identity — a Discord user id,
	// a GitHub node id, a Google account id. It is what a mapping should be
	// keyed on, because it survives a rename, and it is matched byte for byte:
	// a GitHub node id is base64 and its case is meaning, not spelling.
	NativeID string
	// Handle is the login, @-name or email address the source shows. It is
	// what a person can type, and it is the fallback key when no native id is
	// configured: a handle-only identity stops matching the day its owner
	// renames, which is why NativeID exists. It is matched case-insensitively
	// ([FoldHandle]).
	Handle string
}

// Kind is what sort of principal an entry is.
type Kind string

// The principal kinds.
const (
	// KindHuman is a person. It is the default, because most principals are
	// people.
	KindHuman Kind = "human"
	// KindAgent is an agent. Agents are principals too, and an agent's class
	// decides what it may read and write.
	KindAgent Kind = "agent"
	// KindTeam is a group of people and agents. A team owns things and appears
	// in a source's ACLs as a group; it never acts, because a team cannot say
	// anything — one of its members does ([AgentRead], [HumanRead]).
	KindTeam Kind = "team"
)

// kinds is every value [Kind] may take, in the order an error message lists
// them.
var kinds = []Kind{KindHuman, KindAgent, KindTeam}

// Kinds returns every principal kind.
func Kinds() []Kind { return slices.Clone(kinds) }

// Valid reports whether k is a principal kind.
func (k Kind) Valid() bool { return slices.Contains(kinds, k) }

// FoldHandle is how two handles are compared: case-insensitively, with
// surrounding space ignored. GitHub logins, Discord handles and email addresses
// are all case-insensitive to the people who type them, and a mapping that
// matched `Kyle@acme.example` but not `kyle@acme.example` would drop authorship
// over a capital letter.
//
// Configuration and the resolver both fold with this function, so an identity
// two principals claim in different cases is rejected where it is written
// rather than becoming an ambiguity at ingest.
func FoldHandle(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// MaxIDLen is the longest principal id configuration may mint.
const MaxIDLen = connector.MaxSourceIDLen

// ValidID reports whether s is a well-formed principal id: 1 to [MaxIDLen]
// bytes of lowercase letters, digits, `-` and `_`, starting with a letter or a
// digit.
//
// It is the source id rule, deliberately: one shape covers every name a person
// types into configuration, and there is only one of them to keep in step.
func ValidID(s string) bool { return connector.ValidSourceID(s) }
