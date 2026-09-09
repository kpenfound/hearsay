package connector

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// MaxNativeIDLen is the longest native id a connector may emit, in bytes. It is
// a limit on the event id too, which is derived from it, and it exists so that
// the id can be a key in every store it passes through.
const MaxNativeIDLen = 512

// ErrInvalidEvent is returned by [Event.Validate] and wrapped by every
// validation failure, so a caller can tell a malformed event from an IO error
// without matching on strings.
var ErrInvalidEvent = errors.New("invalid event")

// Event is one thing that happened at one source, in the shape every layer
// above L0 expects: {id, source, native_id, kind, time, payload, acl}
// (docs/design.md#l0-events). Connectors emit events and nothing else.
//
// The contract a third party implements against — the id format, the kind
// vocabulary, the required payload metadata, idempotency, edits and deletions —
// is docs/connector-contract.md. This type is that document in Go.
type Event struct {
	// ID is Hearsay's id for the event, derived from Source and NativeID by
	// [EventID] and therefore stable across re-emission. A connector may leave
	// it empty and let the ingest path stamp it; a connector that sets it must
	// set the derived value.
	ID string `json:"id,omitempty"`

	// Source is the id of the configured source instance the event came from —
	// `github-acme`, not `github`. It is the unit the ingest allowlist works
	// in, so one connector type serving two orgs is two sources.
	Source string `json:"source"`

	// NativeID identifies this observation within the source, and is what
	// ingest is idempotent on: re-emitting an event with a native id already in
	// L0 writes nothing. For an artifact that can change it is
	// `<artifact>@<revision>`, so that an edit is a new event rather than an
	// overwrite. See Payload.Artifact.
	NativeID string `json:"native_id"`

	// Kind is what the event is: a core kind, or a connector's own
	// `<vendor>.<name>` extension carrying Payload.BaseKind.
	Kind Kind `json:"kind"`

	// Time is when the artifact happened at the source, not when Hearsay saw
	// it. Ingest time belongs to the store.
	Time time.Time `json:"time"`

	// Payload is the standard metadata the distiller reads plus whatever else
	// the connector wants to keep. It is one JSONB column in L0.
	Payload Payload `json:"payload"`

	// ACL is who may read the event, as the source described it at ingest. It
	// is never empty: an event nobody may read is a bug, not a private event,
	// and L1 inherits this list (docs/design.md#access-control).
	ACL ACL `json:"acl"`
}

// Payload is the metadata every event carries, in one shape for every source,
// so that the distiller works on a new connector's events without knowing
// anything about it. Everything a source has that does not fit goes in Native.
type Payload struct {
	// Artifact is the source's stable id for the thing this event is about,
	// with no revision in it: the message id, the pull request number, the
	// Drive file id. Every revision of an artifact, and any tombstone for it,
	// carries the same value, which is what makes an artifact's history
	// queryable. NativeID must be Artifact or `Artifact@<revision>`.
	Artifact string `json:"artifact"`

	// BaseKind is the core kind an extension kind behaves like. It is required
	// on extension kinds and empty on core ones, and it is what lets the
	// distiller handle a kind it has never heard of.
	BaseKind Kind `json:"base_kind,omitempty"`

	// Container is the repository, channel or folder the artifact lives in: the
	// unit the ingest allowlist names, and the unit a human thinks in when
	// deciding what Hearsay may see.
	Container Container `json:"container"`

	// URL is the permalink a human would follow to see the artifact.
	URL string `json:"url,omitempty"`

	// Title is the artifact's own title where the source has one — an issue
	// title, a document name, a thread name.
	Title string `json:"title,omitempty"`

	// Text is what a human reads in the source, as plain text or markdown, with
	// the source's own markup left alone. It is the distiller's input, and for
	// an artifact whose content is attachments it describes them.
	Text string `json:"text,omitempty"`

	// Author is the identity that produced the artifact, as the source names
	// it. It is a hint: mapping identities to principals is internal/principal.
	Author *Identity `json:"author,omitempty"`

	// Participants are everyone else involved, with the role the source gives
	// them. Reviewers on a pull request, attendees on a transcript.
	Participants []Participant `json:"participants,omitempty"`

	// Mentions are identities the text refers to, where the source resolves
	// mentions itself. They are hints like Author.
	Mentions []Identity `json:"mentions,omitempty"`

	// Links are URLs the artifact carries, as the source gives them. L1 turns
	// them into typed references; a connector does not.
	Links []string `json:"links,omitempty"`

	// Parent is the artifact id of the thing this one hangs off: the issue a
	// comment is on, the message a reply answers.
	Parent string `json:"parent,omitempty"`

	// Thread is the artifact id of the root of the conversation, which is what
	// the distiller assembles a thread from. On a two-level source it is the
	// same value as Parent.
	Thread string `json:"thread,omitempty"`

	// Revision describes an edit: it is set on every event whose NativeID
	// carries a revision, and absent on an artifact's first appearance.
	Revision *Revision `json:"revision,omitempty"`

	// Target is the artifact id a tombstone retracts. It is required on
	// KindTombstone and empty everywhere else.
	Target string `json:"target,omitempty"`

	// Native is whatever else the connector wants to keep — the source's own
	// object, verbatim. Nothing above L0 reads it, and a connector must not
	// hide anything the fields above ask for in here.
	Native json.RawMessage `json:"native,omitempty"`
}

// Container is where in a source an artifact lives.
type Container struct {
	Kind ContainerKind `json:"kind"`
	// NativeID is the source's id for the container. It is what the ingest
	// allowlist matches, so it must be the stable id — a repository's full
	// name, a channel id, a folder id — and never a display name.
	NativeID string `json:"native_id"`
	// Name is the human-readable name, for people reading config and logs.
	Name string `json:"name,omitempty"`
}

// ContainerKind is what sort of container an artifact lives in. The listed
// values are the ones the ingest allowlist is written in terms of; a source
// with a container of its own sort may use another lowercase word.
type ContainerKind string

// The container kinds Hearsay's own connectors use.
const (
	ContainerRepository ContainerKind = "repository"
	ContainerChannel    ContainerKind = "channel"
	ContainerFolder     ContainerKind = "folder"
	// ContainerWorkspace is the whole source, for a source with no smaller
	// boundary — an agent's own session stream, for instance.
	ContainerWorkspace ContainerKind = "workspace"
)

// Revision describes one edit of an artifact.
type Revision struct {
	// Token is the source's own version token: an edit timestamp, an ETag, a
	// Drive revision id. It is opaque to Hearsay, and it is the part of the
	// native id after the `@`.
	Token string `json:"token"`
	// EditedAt is when the edit happened, where the source says.
	EditedAt time.Time `json:"edited_at,omitzero"`
}

// Identity is who a source says someone is. It is a hint: a connector never
// mints or resolves a Hearsay principal id (that is internal/principal, #6),
// and it never guesses that two sources mean the same person.
type Identity struct {
	// Source is the source the identity belongs to. It is usually the event's
	// own source, and differs when one source reports another's identities.
	Source string `json:"source"`
	// Kind separates people from bots and agents, because authority differs.
	Kind IdentityKind `json:"kind"`
	// NativeID is the source's stable id for the identity — a Discord user id,
	// a GitHub node id, a Google account id. Never a login or a display name:
	// those get renamed, and an identity mapping keyed on one silently breaks.
	NativeID string `json:"native_id"`
	// Handle is the login or @-name, for a human reading the mapping.
	Handle string `json:"handle,omitempty"`
	// DisplayName is the name the source shows.
	DisplayName string `json:"display_name,omitempty"`
	// Email is set only where the source gives one. It is a hint for identity
	// mapping, and like the rest of the payload it is never logged (ADR-0008).
	Email string `json:"email,omitempty"`
}

// IdentityKind separates the sorts of actor a source reports.
type IdentityKind string

// The identity kinds.
const (
	IdentityUser  IdentityKind = "user"
	IdentityBot   IdentityKind = "bot"
	IdentityAgent IdentityKind = "agent"
)

// Participant is one identity's part in an artifact.
type Participant struct {
	Identity Identity        `json:"identity"`
	Role     ParticipantRole `json:"role"`
}

// ParticipantRole is the part an identity played. The four values are the ones
// L1 carries (docs/design.md#l1-distilled-documents).
type ParticipantRole string

// The participant roles.
const (
	RoleAuthor   ParticipantRole = "author"
	RoleReviewer ParticipantRole = "reviewer"
	RoleAttendee ParticipantRole = "attendee"
	RoleAgent    ParticipantRole = "agent"
)

// ACL is who may read an event. An empty ACL is invalid rather than private:
// reads fail closed, so an event with no entries would be unreadable forever.
type ACL []ACLEntry

// ACLEntry is one grant, as the source described it. Membership of a group is
// not resolved here: the entry names the group, and access control resolves it
// at read time against the identity mapping.
type ACLEntry struct {
	Kind ACLKind `json:"kind"`
	// Source is the source the group or identity belongs to. Required except
	// on ACLPublic.
	Source string `json:"source,omitempty"`
	// NativeID is the source's id for the group or identity. Required except
	// on ACLPublic.
	NativeID string `json:"native_id,omitempty"`
	// Label is a human-readable name for the grant, for config and audit.
	Label string `json:"label,omitempty"`
}

// ACLKind is the sort of grant an ACL entry makes.
type ACLKind string

// The ACL kinds.
const (
	// ACLPublic means every principal Hearsay knows may read the event. It is
	// scoped to the organisation running Hearsay, never to the internet: a
	// public GitHub repository is ACLPublic because everyone in the
	// organisation may read it, not because anybody may.
	ACLPublic ACLKind = "public"
	// ACLGroup names a source-native group whose members may read: a channel's
	// members, a repository's collaborators, a Google group.
	ACLGroup ACLKind = "group"
	// ACLIdentity names one identity that may read, for an artifact shared with
	// a person rather than with a group.
	ACLIdentity ACLKind = "identity"
)

// Kind is what an event is.
type Kind string

// The core kinds. A connector emits one of these wherever it can, because
// everything above L0 is written against them. Where a source has something
// genuinely different, it emits an extension kind instead: see [Kind.Valid].
const (
	KindMessage       Kind = "message"        // a chat message, or a comment on anything
	KindThread        Kind = "thread"         // a container for messages
	KindReaction      Kind = "reaction"       // an emoji reaction; the feedback loop reads these
	KindIssue         Kind = "issue"          // a tracker item
	KindPullRequest   Kind = "pull_request"   // a change proposal
	KindReview        Kind = "review"         // a verdict on a change proposal
	KindReviewComment Kind = "review_comment" // a comment anchored in a diff
	KindCommit        Kind = "commit"         // a commit on a watched branch
	KindDocument      Kind = "document"       // a wiki page, a design doc, a Drive file
	KindTranscript    Kind = "transcript"     // a meeting transcript
	KindAgentSession  Kind = "agent_session"  // an agent session starting or ending
	KindAgentTurn     Kind = "agent_turn"     // one turn of an agent session
	KindToolCall      Kind = "tool_call"      // a tool call an agent made
	KindAssertion     Kind = "assertion"      // a stance written through assert()
	KindAudit         Kind = "audit"          // a bundle served: who asked, for what
	KindTombstone     Kind = "tombstone"      // an artifact deleted at the source
)

// kindRule is what validation requires of a kind. The two flags are per kind
// because the sources differ: a chat message may be nothing but an image, and
// a pull request always has a title.
type kindRule struct {
	// requiresAuthor is set where the source always knows who acted.
	requiresAuthor bool
	// requiresContent is set where the artifact always has a title or text.
	// An event with neither is nothing for the distiller to read.
	requiresContent bool
}

// coreKinds is the vocabulary, and the rules each kind carries. The table is
// the same one docs/connector-contract.md prints.
var coreKinds = map[Kind]kindRule{
	KindMessage:       {requiresAuthor: true},
	KindThread:        {requiresAuthor: true, requiresContent: true},
	KindReaction:      {requiresAuthor: true},
	KindIssue:         {requiresAuthor: true, requiresContent: true},
	KindPullRequest:   {requiresAuthor: true, requiresContent: true},
	KindReview:        {requiresAuthor: true},
	KindReviewComment: {requiresAuthor: true, requiresContent: true},
	KindCommit:        {requiresAuthor: true, requiresContent: true},
	KindDocument:      {requiresAuthor: true, requiresContent: true},
	KindTranscript:    {requiresContent: true},
	KindAgentSession:  {requiresAuthor: true},
	KindAgentTurn:     {requiresAuthor: true, requiresContent: true},
	KindToolCall:      {requiresAuthor: true},
	KindAssertion:     {requiresAuthor: true, requiresContent: true},
	KindAudit:         {requiresAuthor: true},
	KindTombstone:     {},
}

// CoreKinds returns the core kind vocabulary, sorted, for a connector that
// emits everything and for documentation that must not drift from the code.
func CoreKinds() []Kind {
	kinds := make([]Kind, 0, len(coreKinds))
	for k := range coreKinds {
		kinds = append(kinds, k)
	}
	sortKinds(kinds)
	return kinds
}

// IsCore reports whether k is one of the core kinds.
func (k Kind) IsCore() bool {
	_, ok := coreKinds[k]
	return ok
}

// Valid reports whether k is a core kind or a well-formed extension kind.
//
// An extension kind is `<vendor>.<name>`, both lowercase words — `figma.file`,
// `jira.sprint`. The vendor prefix is what keeps two connectors from meaning
// different things by the same word, and an event carrying one must also carry
// Payload.BaseKind so that everything above L0 has a core kind to fall back to.
func (k Kind) Valid() bool {
	if k.IsCore() {
		return true
	}
	vendor, name, ok := strings.Cut(string(k), ".")
	return ok && isLowerWord(vendor) && isLowerWord(name)
}

// EventID returns the event id for a source and native id. It is a pure
// function of the two, so a connector that re-emits an artifact produces the
// same id, and ingest can be idempotent on it.
//
// The format is `evt:<source>:<native id>`, with any byte outside the safe set
// percent-encoded. Sources contain no colon, so the id splits on its first two
// and [ParseEventID] gets the native id back byte for byte.
func EventID(source, nativeID string) string {
	return "evt:" + source + ":" + encodeSegment(nativeID)
}

// ParseEventID returns the source and native id an event id was built from.
func ParseEventID(id string) (source, nativeID string, err error) {
	rest, ok := strings.CutPrefix(id, "evt:")
	if !ok {
		return "", "", fmt.Errorf("event id %q: missing the evt: prefix", id)
	}
	source, encoded, ok := strings.Cut(rest, ":")
	if !ok {
		return "", "", fmt.Errorf("event id %q: missing the native id", id)
	}
	if !isSourceID(source) {
		return "", "", fmt.Errorf("event id %q: %q is not a source id", id, source)
	}
	nativeID, err = decodeSegment(encoded)
	if err != nil {
		return "", "", fmt.Errorf("event id %q: %w", id, err)
	}
	return source, nativeID, nil
}

// Validate reports whether the event is one L0 may store. Every rule it
// enforces is a rule docs/connector-contract.md states, and the ingest path
// calls it on every event, so a connector that passes validation here behaves
// the same as one written against the document.
//
// Errors wrap [ErrInvalidEvent] and name the field that is wrong.
func (e Event) Validate() error {
	if !isSourceID(e.Source) {
		return fmt.Errorf("%w: source %q is not a source id (lowercase letters, digits, - and _)", ErrInvalidEvent, e.Source)
	}
	switch {
	case e.NativeID == "":
		return fmt.Errorf("%w: native_id is empty", ErrInvalidEvent)
	case len(e.NativeID) > MaxNativeIDLen:
		return fmt.Errorf("%w: native_id is %d bytes, the limit is %d", ErrInvalidEvent, len(e.NativeID), MaxNativeIDLen)
	case strings.ContainsFunc(e.NativeID, isSpaceOrControl):
		return fmt.Errorf("%w: native_id contains whitespace or a control character", ErrInvalidEvent)
	}
	if e.ID != "" && e.ID != EventID(e.Source, e.NativeID) {
		return fmt.Errorf("%w: id %q is not the id derived from the source and native_id", ErrInvalidEvent, e.ID)
	}
	if !e.Kind.Valid() {
		return fmt.Errorf("%w: kind %q is neither a core kind nor <vendor>.<name>", ErrInvalidEvent, e.Kind)
	}
	if e.Time.IsZero() {
		return fmt.Errorf("%w: time is zero", ErrInvalidEvent)
	}
	if err := e.ACL.validate(); err != nil {
		return err
	}
	return e.Payload.validate(e)
}

func (p Payload) validate(e Event) error {
	rule, core := coreKinds[e.Kind]
	switch {
	case core && p.BaseKind != "":
		return fmt.Errorf("%w: base_kind is set on core kind %q", ErrInvalidEvent, e.Kind)
	case !core && p.BaseKind == "":
		return fmt.Errorf("%w: extension kind %q has no base_kind", ErrInvalidEvent, e.Kind)
	case !core:
		var ok bool
		if rule, ok = coreKinds[p.BaseKind]; !ok {
			return fmt.Errorf("%w: base_kind %q is not a core kind", ErrInvalidEvent, p.BaseKind)
		}
	}

	switch {
	case p.Artifact == "":
		return fmt.Errorf("%w: payload.artifact is empty", ErrInvalidEvent)
	case e.NativeID != p.Artifact && !strings.HasPrefix(e.NativeID, p.Artifact+"@"):
		return fmt.Errorf("%w: native_id %q is neither payload.artifact %q nor a revision of it", ErrInvalidEvent, e.NativeID, p.Artifact)
	case p.Container.Kind == "":
		return fmt.Errorf("%w: payload.container.kind is empty", ErrInvalidEvent)
	case p.Container.NativeID == "":
		return fmt.Errorf("%w: payload.container.native_id is empty", ErrInvalidEvent)
	case rule.requiresAuthor && p.Author == nil:
		return fmt.Errorf("%w: kind %q requires payload.author", ErrInvalidEvent, e.Kind)
	case rule.requiresContent && p.Title == "" && p.Text == "":
		return fmt.Errorf("%w: kind %q requires payload.title or payload.text", ErrInvalidEvent, e.Kind)
	}

	if p.Author != nil {
		if err := p.Author.validate("payload.author"); err != nil {
			return err
		}
	}
	for i, part := range p.Participants {
		switch part.Role {
		case RoleAuthor, RoleReviewer, RoleAttendee, RoleAgent:
		default:
			return fmt.Errorf("%w: payload.participants[%d].role %q is not a role", ErrInvalidEvent, i, part.Role)
		}
		if err := part.Identity.validate(fmt.Sprintf("payload.participants[%d].identity", i)); err != nil {
			return err
		}
	}
	for i, m := range p.Mentions {
		if err := m.validate(fmt.Sprintf("payload.mentions[%d]", i)); err != nil {
			return err
		}
	}

	tombstone := e.Kind == KindTombstone || p.BaseKind == KindTombstone
	switch {
	case tombstone && p.Target == "":
		return fmt.Errorf("%w: a tombstone requires payload.target", ErrInvalidEvent)
	case tombstone && p.Target == p.Artifact:
		// A tombstone is its own artifact — `<target>:tombstone` by
		// convention — so that it is emitted the same way twice and so that it
		// does not read as a revision of the thing it retracts.
		return fmt.Errorf("%w: a tombstone's payload.artifact is its own, not payload.target %q", ErrInvalidEvent, p.Target)
	case !tombstone && p.Target != "":
		return fmt.Errorf("%w: payload.target is set on kind %q, which is not a tombstone", ErrInvalidEvent, e.Kind)
	case p.Revision != nil && p.Revision.Token == "":
		return fmt.Errorf("%w: payload.revision.token is empty", ErrInvalidEvent)
	}
	return nil
}

func (i Identity) validate(field string) error {
	switch {
	case !isSourceID(i.Source):
		return fmt.Errorf("%w: %s.source %q is not a source id", ErrInvalidEvent, field, i.Source)
	case i.NativeID == "":
		return fmt.Errorf("%w: %s.native_id is empty", ErrInvalidEvent, field)
	}
	switch i.Kind {
	case IdentityUser, IdentityBot, IdentityAgent:
		return nil
	default:
		return fmt.Errorf("%w: %s.kind %q is not an identity kind", ErrInvalidEvent, field, i.Kind)
	}
}

func (a ACL) validate() error {
	if len(a) == 0 {
		return fmt.Errorf("%w: acl is empty, and an event nobody may read cannot be read back", ErrInvalidEvent)
	}
	for i, entry := range a {
		switch entry.Kind {
		case ACLPublic:
			if entry.NativeID != "" {
				return fmt.Errorf("%w: acl[%d] is public and names %q", ErrInvalidEvent, i, entry.NativeID)
			}
		case ACLGroup, ACLIdentity:
			if !isSourceID(entry.Source) {
				return fmt.Errorf("%w: acl[%d].source %q is not a source id", ErrInvalidEvent, i, entry.Source)
			}
			if entry.NativeID == "" {
				return fmt.Errorf("%w: acl[%d] is a %s with no native_id", ErrInvalidEvent, i, entry.Kind)
			}
		default:
			return fmt.Errorf("%w: acl[%d].kind %q is not an acl kind", ErrInvalidEvent, i, entry.Kind)
		}
	}
	return nil
}
