package config

import (
	"strings"

	"github.com/kpenfound/hearsay/internal/connector"
)

// Repo is a configuration repository, loaded and validated: the directory laid
// out as `sources/`, `scopes/`, `principals/`, `code/` and `authority/`, or the
// single file that expands to it (docs/config.md).
//
// Every field is the parsed form of one directory. Sources are
// [connector.SourceConfig] values because that is what the connector runtime
// consumes; nothing reshapes them on the way.
//
// The lookups on it scan, because a configuration repository holds tens of
// entries rather than thousands and a map would have to be kept in step with
// the slice a caller can already range over.
type Repo struct {
	// Path is where the configuration was loaded from, as it was given to
	// [Load]. It is empty on a Repo that was never loaded.
	Path string
	// Digest identifies the configuration as it was read: `sha256:` followed by
	// hex. Two processes reporting the same digest are running the same
	// configuration, which is what makes "restart to reload" checkable
	// (ADR-0009). The two forms of the same configuration have different
	// digests: it identifies the bytes, not the meaning.
	Digest string

	// Sources is the `sources/` directory: every source Hearsay ingests, and
	// the ingest allowlist, which is default deny (docs/design.md#access-control).
	Sources []connector.SourceConfig
	// Scopes is the `scopes/` directory: named bundles of sources.
	Scopes []Scope
	// Principals is the `principals/` directory: identity mapping.
	Principals []Principal
	// Code is the `code/` directory: code entity seeds.
	Code []CodeEntity
	// Authority is the `authority/` directory: who and what may ratify a
	// stance, per scope, and how sources rank against each other.
	Authority Authority
}

// Source returns the source configured with this id.
func (r Repo) Source(id string) (connector.SourceConfig, bool) {
	for _, s := range r.Sources {
		if s.ID == id {
			return s, true
		}
	}
	return connector.SourceConfig{}, false
}

// Scope returns the scope configured with this id.
func (r Repo) Scope(id string) (Scope, bool) {
	for _, s := range r.Scopes {
		if s.ID == id {
			return s, true
		}
	}
	return Scope{}, false
}

// Principal returns the principal configured with this id.
func (r Repo) Principal(id string) (Principal, bool) {
	for _, p := range r.Principals {
		if p.ID == id {
			return p, true
		}
	}
	return Principal{}, false
}

// CodeEntity returns the code entity configured with this id.
func (r Repo) CodeEntity(id string) (CodeEntity, bool) {
	for _, e := range r.Code {
		if e.ID == id {
			return e, true
		}
	}
	return CodeEntity{}, false
}

// Allowlist is the ingest allowlist the configured sources describe. It is
// default deny: a source that is not configured, or a container that is not
// listed, is not ingested.
func (r Repo) Allowlist() connector.Allowlist {
	return connector.NewAllowlist(r.Sources...)
}

// Scope is one entry of `scopes/`: a named bundle of sources, and the tracker
// whose items are `tracker_item` entities within it. Scopes filter for
// relevance; ACLs grant permission, and they are never the same mechanism
// (docs/design.md#access-control).
type Scope struct {
	// ID is the scope id, used in bundle requests, in the queue's serial key
	// and in authority policies.
	ID string
	// Name is what a person calls it.
	Name string
	// Sources are the sources this scope draws on, and which of their
	// containers.
	Sources []ScopeSource
	// Tracker maps this scope's tracker items to entity ids. Its zero value
	// means the scope has no tracker.
	Tracker SourceRef
	// Entities are the code entities this scope is about, by id.
	Entities []string
}

// Covers reports whether an artifact from this container of this source is in
// the scope. An empty container never matches, the way the ingest allowlist
// treats it: a scope filters artifacts, and every artifact has a container.
func (s Scope) Covers(source, container string) bool {
	if container == "" {
		return false
	}
	for _, ss := range s.Sources {
		if ss.Source != source {
			continue
		}
		for _, c := range ss.Containers {
			if c == container || c == connector.AllowAll {
				return true
			}
		}
	}
	return false
}

// TrackerItemID returns the entity id of one item of this scope's tracker, and
// false when the scope has no tracker mapping. The item is the tracker's own
// number or key, without any decoration: `1234`, `ENG-7`.
//
// The shape is `tracker:<source>:<project>#<item>`, so an entity id names the
// configured source instance the item came from — two trackers with the same
// project key in different sources stay apart, and the id can be read back to
// the source that owns it without a lookup, the way an event id can
// (docs/connector-contract.md).
func (s Scope) TrackerItemID(item string) (string, bool) {
	if s.Tracker.Source == "" || item == "" {
		return "", false
	}
	return "tracker:" + s.Tracker.Source + ":" + s.Tracker.Project + "#" + item, true
}

// ScopeSource is one source a scope draws on.
type ScopeSource struct {
	// Source is the source id.
	Source string
	// Containers are the containers of that source the scope covers, by native
	// id. The single entry `*` covers every container the source ingests.
	Containers []string
}

// SourceRef points at something inside a source: a tracker's project, a code
// entity's repository. The project is a container native id of that source —
// a repository full name, a project key — so it is checked against the source's
// ingest allowlist rather than being a free-text label.
type SourceRef struct {
	Source  string
	Project string
}

// Principal is one entry of `principals/`: who someone is, across every source
// they appear in. Without this a stance has no consistent author and authority
// cannot be checked (docs/design.md#configuration).
type Principal struct {
	// ID is the Hearsay principal id: what a stance's author, an owner and an
	// authority policy all name.
	ID string
	// Name is the person's or agent's name, for a human reading config.
	Name string
	// Kind separates people from agents, because what they may do differs.
	Kind PrincipalKind
	// Class is an agent's access class (docs/design.md#access-control). It is
	// required on an agent and empty on a human.
	Class AgentClass
	// Identities are the source-native identities that are this principal.
	Identities []Identity
}

// Identity is one principal in one source.
type Identity struct {
	// Source is the source id the identity belongs to.
	Source string
	// NativeID is the source's stable id for the identity — a Discord user id,
	// a GitHub node id, a Google account id. It is what an identity mapping
	// should be keyed on, because it survives a rename.
	NativeID string
	// Handle is the login, @-name or email address the source shows. It is
	// what a person can type, and it is the fallback key when no native id is
	// configured: a handle-only identity stops matching the day its owner
	// renames, which is why NativeID exists.
	Handle string
}

// PrincipalKind is what sort of principal an entry is.
type PrincipalKind string

// The principal kinds.
const (
	// PrincipalHuman is a person.
	PrincipalHuman PrincipalKind = "human"
	// PrincipalAgent is an agent. Agents are principals too, and an agent's
	// class decides what it may read and write.
	PrincipalAgent PrincipalKind = "agent"
)

// AgentClass is an agent's access class (docs/design.md#access-control).
type AgentClass string

// The agent classes.
const (
	// ClassObserver reads scoped L3 and L1 and writes nothing.
	ClassObserver AgentClass = "observer"
	// ClassWorker reads its scope plus linked code entities and may assert.
	ClassWorker AgentClass = "worker"
	// ClassOrchestrator reads several scopes, may assert and may subscribe.
	ClassOrchestrator AgentClass = "orchestrator"
	// ClassSteward may ratify and merge topics. It exists so the door is
	// there, closed: ratification is a human action by default.
	ClassSteward AgentClass = "steward"
)

// agentClasses is every value [AgentClass] may take.
var agentClasses = []AgentClass{ClassObserver, ClassWorker, ClassOrchestrator, ClassSteward}

// CodeEntity is one entry of `code/`: the map from how people talk about code
// to where the code is and who owns it. Hearsay holds no code, so an entity is
// path patterns and vocabulary, never file contents
// (docs/design.md#entities).
type CodeEntity struct {
	// ID is the entity id, which starts with `code:`.
	ID string
	// Type is what sort of thing it is.
	Type CodeEntityType
	// Name is the display name.
	Name string
	// Aliases are what people call it. They are matched case-insensitively, so
	// "the engine" and "The Engine" are one alias.
	Aliases []string
	// PathPatterns locate it in Repo, resolved against HEAD at read time.
	// Patterns rather than file lists, so a refactor does not invalidate them.
	PathPatterns []string
	// PartOf names the entities this one is part of, which is how the
	// hierarchy is seeded.
	PartOf []string
	// Owners are principal ids.
	Owners []string
	// Repo is the repository the patterns resolve against.
	Repo SourceRef
	// CodeOwners is the path of a CODEOWNERS file within Repo to seed owners
	// from. Config records the intent; the import itself belongs to the code
	// entity work.
	CodeOwners string
}

// CodeEntityType is what sort of thing a code entity is. Documents may also
// reference `tracker_item` and `person` entities, but those come from the
// tracker mapping and from `principals/`, so neither is declared here.
type CodeEntityType string

// The code entity types.
const (
	// TypeProject is a repository or a product-level grouping.
	TypeProject CodeEntityType = "project"
	// TypeService is a deployable.
	TypeService CodeEntityType = "service"
	// TypeModule is a package or directory of code.
	TypeModule CodeEntityType = "module"
	// TypeSymbol is a single named thing in the code.
	TypeSymbol CodeEntityType = "symbol"
)

// codeEntityTypes is every value [CodeEntityType] may take.
var codeEntityTypes = []CodeEntityType{TypeProject, TypeService, TypeModule, TypeSymbol}

// foldAlias is how two aliases are compared: case-insensitively, with
// surrounding space ignored.
func foldAlias(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
