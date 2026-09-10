package l1

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
)

// The errors a caller distinguishes.
var (
	// ErrInvalidDocument is wrapped by everything [Document.Validate] refuses,
	// so a caller can tell a malformed document from an IO error.
	ErrInvalidDocument = errors.New("invalid l1 document")

	// ErrNotDistilled is an artifact whose events do not make a document: a
	// kind no L1 kind covers, or an artifact every visible event of which a
	// tombstone has taken away. It is not a failure — the distiller reports it
	// and moves on — which is why it is a sentinel rather than a string.
	ErrNotDistilled = errors.New("the artifact does not distil to a document")

	// ErrNotFound is a document id the store does not hold.
	ErrNotFound = errors.New("no such l1 document")
)

// MaxDocIDLen bounds a document id. It is what [DocID] can produce from the
// longest source id and the longest artifact id docs/connector-contract.md
// allows, and it is well under the queue's own limit on a target id, which is
// what a distill job carries.
const MaxDocIDLen = len("l1:") + connector.MaxSourceIDLen + len(":") + connector.MaxNativeIDLen

// Kind is what an L1 document is. It is L1's own vocabulary rather than L0's:
// several L0 kinds make one document (a pull request with its reviews and its
// comments is one `pr`), and one L0 kind can make several (a meeting
// transcript is one `meeting_segment` per topic, later).
//
// docs/design.md#l1-distilled-documents lists the whole vocabulary. The three
// here are the ones this build distils; the rest arrive with the sources that
// produce them.
type Kind string

// The document kinds this build produces.
const (
	// KindIssue is a tracker item and the conversation on it.
	KindIssue Kind = "issue"
	// KindPR is a change proposal with its reviews and its comments.
	KindPR Kind = "pr"
	// KindCommit is one commit on a watched branch.
	KindCommit Kind = "commit"
)

// rootKinds maps the L0 kind of an artifact that makes a document of its own to
// the kind of document it makes. An L0 kind that is not in it is either part of
// somebody else's document — a `message` on an issue, a `review` on a pull
// request — or a kind no source this build ingests produces yet.
var rootKinds = map[connector.Kind]Kind{
	connector.KindIssue:       KindIssue,
	connector.KindPullRequest: KindPR,
	connector.KindCommit:      KindCommit,
}

// Kinds is every document kind this build produces, in the order they are
// documented.
func Kinds() []Kind { return []Kind{KindIssue, KindPR, KindCommit} }

// Valid reports whether k is a kind this build produces.
func (k Kind) Valid() bool {
	switch k {
	case KindIssue, KindPR, KindCommit:
		return true
	}
	return false
}

// String makes a Kind print as its name.
func (k Kind) String() string { return string(k) }

// KindFor is the document kind an event makes on its own, and false for an
// event that is part of another artifact's document or that this build does not
// distil.
//
// An extension kind is read through its `payload.base_kind`, which is what
// docs/connector-contract.md promises: a connector emitting `jira.story` with a
// base kind of `issue` gets an `issue` document with no code here that knows
// about Jira.
func KindFor(ev connector.Event) (Kind, bool) {
	if k, ok := rootKinds[ev.Kind]; ok {
		return k, true
	}
	if ev.Payload.BaseKind != "" {
		k, ok := rootKinds[ev.Payload.BaseKind]
		return k, ok
	}
	return "", false
}

// OutcomeKind is what a document concluded, and it is the L2 trigger: only
// `decided`, `proposed` and `resolved` enter the assertion pipeline
// (docs/design.md#l1-distilled-documents). Every document has one — a
// conversation that concluded nothing is `none`, not an absent field.
type OutcomeKind string

// The five outcome kinds. The set is the design's and is closed: the column
// carries a CHECK over exactly these values.
const (
	// OutcomeResolved is a question that was answered or work that was
	// finished: a merged pull request, a bug someone fixed.
	OutcomeResolved OutcomeKind = "resolved"
	// OutcomeDecided is a decision taken that binds later work.
	OutcomeDecided OutcomeKind = "decided"
	// OutcomeProposed is something put forward and not yet agreed.
	OutcomeProposed OutcomeKind = "proposed"
	// OutcomeOpen is a question still being worked out.
	OutcomeOpen OutcomeKind = "open"
	// OutcomeNone is a document that concluded nothing.
	OutcomeNone OutcomeKind = "none"
)

// OutcomeKinds is the five, in the order the design lists them.
var OutcomeKinds = []OutcomeKind{OutcomeResolved, OutcomeDecided, OutcomeProposed, OutcomeOpen, OutcomeNone}

// Valid reports whether o is one of the five.
func (o OutcomeKind) Valid() bool {
	for _, k := range OutcomeKinds {
		if o == k {
			return true
		}
	}
	return false
}

// Asserts reports whether a document with this outcome enters the assertion
// pipeline. It is the design's rule in one place, so that L2's worker and this
// package cannot disagree about which documents it reads.
func (o OutcomeKind) Asserts() bool {
	return o == OutcomeDecided || o == OutcomeProposed || o == OutcomeResolved
}

// String makes an OutcomeKind print as its name.
func (o OutcomeKind) String() string { return string(o) }

// ParseOutcomeKind reads an outcome kind, which is what a model answers and
// what the column holds.
func ParseOutcomeKind(s string) (OutcomeKind, error) {
	o := OutcomeKind(s)
	if !o.Valid() {
		return "", fmt.Errorf("no such outcome kind %q: want one of %s", s, strings.Join(outcomeNames(), ", "))
	}
	return o, nil
}

func outcomeNames() []string {
	names := make([]string, len(OutcomeKinds))
	for i, o := range OutcomeKinds {
		names[i] = string(o)
	}
	return names
}

// Source is where a document came from: the configured source instance, the
// artifact id within it, and the permalink a human would follow.
type Source struct {
	// System is the configured source instance — `github-acme`, not `github` —
	// which is the unit the ingest allowlist and the identity mapping work in.
	System string `json:"system"`
	// NativeID is the artifact id within that source: `acme/api#31`. It is the
	// artifact, never one observation of it, so it does not move when the
	// artifact is edited.
	NativeID string `json:"native_id"`
	// URL is the permalink, where the source gave one.
	URL string `json:"url,omitempty"`
}

// Times is when the artifact happened, as against when Hearsay saw it.
type Times struct {
	// Created is the artifact's own time, which never moves: every revision of
	// an artifact carries it (docs/connector-contract.md).
	Created time.Time `json:"created"`
	// Updated is when the artifact itself was last edited — the newest
	// revision's edit time — and equals Created for one that never changed.
	Updated time.Time `json:"updated"`
	// LastActivity is the newest of anything in the document: the artifact, its
	// edits, and every comment and review on it. It is what "recent activity on
	// a scope" is ordered by.
	LastActivity time.Time `json:"last_activity"`
}

// Participant is one principal's part in a document. The four roles are
// [connector.ParticipantRole]'s, because they are one vocabulary: a document
// says the same word about a reviewer that the event it came from does.
type Participant struct {
	// PrincipalID is the Hearsay principal. An identity that does not resolve
	// is not a participant: no placeholder id is minted, and the resolver keeps
	// the sighting for a person to map (internal/principal).
	PrincipalID string `json:"principal_id"`
	// Role is the part they played.
	Role connector.ParticipantRole `json:"role"`
}

// Document is one L1 document: the common envelope from
// docs/design.md#l1-distilled-documents plus the per-kind body. It is one row
// of one table, and it is derived — everything in it can be rebuilt from the
// events [Document.L0Refs] names.
type Document struct {
	// ID is `l1:<source>:<artifact>` ([DocID]), so a document id can be read
	// back to the artifact it distils without a lookup.
	ID string `json:"id"`
	// Kind is what sort of document this is.
	Kind Kind `json:"kind"`
	// Source is the artifact this document distils.
	Source Source `json:"source"`
	// L0Refs are the event ids this document was built from, in the order the
	// document reads: the current revision of the artifact, then the current
	// revision of each comment, review and reply on it. Provenance is required
	// — a document that names no events cannot be regenerated or followed back.
	L0Refs []string `json:"l0_refs"`
	// Time is when the artifact happened and when it last moved.
	Time Times `json:"time"`
	// Participants are the principals who took part, sorted.
	Participants []Participant `json:"participants,omitempty"`
	// Scope is the entity ids this document is about: the code entities of
	// every configured scope that covers it, the ones its text names, and the
	// tracker item it is, where a scope maps one. Sorted.
	Scope []string `json:"scope,omitempty"`
	// References are what the document points at, extracted deterministically
	// and never by a model: links, @-mentions resolved through the principal
	// resolver, and the code entities `code/` configuration names. They are the
	// join key L2 matches topics on, and they cost nothing to recompute.
	References []Reference `json:"references,omitempty"`
	// ACL is who may read the document, inherited from the events it is built
	// from and re-synced whenever they change, because a re-synced access list
	// is a new revision of the artifact (docs/design.md#access-control).
	ACL connector.ACL `json:"acl"`
	// Text is the distillation, and it is what is embedded: the title, the
	// summary, the outcome and the open questions, rendered. It is scrubbed of
	// secrets and PII before it is written.
	Text string `json:"text"`
	// RawText is the artifact in the team's own words — the body, then every
	// comment and review — for the full-text index. It is never embedded, and
	// it is scrubbed like Text.
	RawText string `json:"raw_text"`
	// Body is what the distill tier concluded.
	Body Body `json:"body"`
}

// Body is what a model produced from the document: the common fields every kind
// carries, plus the per-kind ones, which are empty on the kinds that do not
// have them.
type Body struct {
	// Summary is what happened, in a few sentences.
	Summary string `json:"summary"`
	// Question is what is being asked, on a kind where that is the point of the
	// artifact. Issues have one; a commit does not.
	Question string `json:"question,omitempty"`
	// Outcome is what was concluded, empty where nothing was.
	Outcome string `json:"outcome,omitempty"`
	// OutcomeKind classifies Outcome, and is set on every document.
	OutcomeKind OutcomeKind `json:"outcome_kind"`
	// OpenQuestions are what the artifact leaves unanswered.
	OpenQuestions []string `json:"open_questions,omitempty"`
	// Change is what a change proposal does, on the kinds that propose one: a
	// pull request and a commit.
	Change string `json:"change,omitempty"`
}

// DocID is the id of the document for an artifact. It is a pure function of the
// two, so re-distilling an artifact writes the same row rather than a second
// one, and it is parseable back because a source id contains no colon
// (docs/connector-contract.md) while an artifact id may.
func DocID(source, artifact string) string { return "l1:" + source + ":" + artifact }

// ParseDocID returns the source and artifact a document id was built from.
func ParseDocID(id string) (source, artifact string, err error) {
	rest, ok := strings.CutPrefix(id, "l1:")
	if !ok {
		return "", "", fmt.Errorf("document id %q: missing the l1: prefix", id)
	}
	source, artifact, ok = strings.Cut(rest, ":")
	if !ok || artifact == "" {
		return "", "", fmt.Errorf("document id %q: missing the artifact id", id)
	}
	if !connector.ValidSourceID(source) {
		return "", "", fmt.Errorf("document id %q: %q is not a source id", id, source)
	}
	return source, artifact, nil
}

// Validate reports a document the store refuses. Every rule it enforces is one
// the table also enforces, so a row written by anything else behaves the same.
func (d Document) Validate() error {
	switch {
	case !d.Kind.Valid():
		return fmt.Errorf("%w: kind %q is not one this build distils", ErrInvalidDocument, d.Kind)
	case !connector.ValidSourceID(d.Source.System):
		return fmt.Errorf("%w: source.system %q is not a source id", ErrInvalidDocument, d.Source.System)
	case d.Source.NativeID == "":
		return fmt.Errorf("%w: source.native_id is empty", ErrInvalidDocument)
	case d.ID != DocID(d.Source.System, d.Source.NativeID):
		return fmt.Errorf("%w: id %q is not the id derived from source.system and source.native_id", ErrInvalidDocument, d.ID)
	case len(d.ID) > MaxDocIDLen:
		return fmt.Errorf("%w: id is %d bytes, the limit is %d", ErrInvalidDocument, len(d.ID), MaxDocIDLen)
	case len(d.L0Refs) == 0:
		return fmt.Errorf("%w: l0_refs is empty, so the document could not be regenerated or followed back", ErrInvalidDocument)
	case len(d.ACL) == 0:
		return fmt.Errorf("%w: acl is empty, and a document nobody may read cannot be read back", ErrInvalidDocument)
	case d.Time.Created.IsZero():
		return fmt.Errorf("%w: time.created is zero", ErrInvalidDocument)
	case d.Time.Updated.Before(d.Time.Created):
		return fmt.Errorf("%w: time.updated %s is before time.created %s", ErrInvalidDocument, stamp(d.Time.Updated), stamp(d.Time.Created))
	case d.Time.LastActivity.Before(d.Time.Created):
		return fmt.Errorf("%w: time.last_activity %s is before time.created %s", ErrInvalidDocument, stamp(d.Time.LastActivity), stamp(d.Time.Created))
	case d.RawText == "":
		return fmt.Errorf("%w: raw_text is empty", ErrInvalidDocument)
	case d.Text == "":
		return fmt.Errorf("%w: text is empty", ErrInvalidDocument)
	case !d.Body.OutcomeKind.Valid():
		return fmt.Errorf("%w: body.outcome_kind %q is not one of %s", ErrInvalidDocument, d.Body.OutcomeKind, strings.Join(outcomeNames(), ", "))
	}
	for i, ref := range d.L0Refs {
		if ref == "" {
			return fmt.Errorf("%w: l0_refs[%d] is empty", ErrInvalidDocument, i)
		}
	}
	for i, entry := range d.ACL {
		switch entry.Kind {
		case connector.ACLPublic, connector.ACLGroup, connector.ACLIdentity:
		default:
			return fmt.Errorf("%w: acl[%d].kind %q is not an acl kind", ErrInvalidDocument, i, entry.Kind)
		}
	}
	for i, p := range d.Participants {
		switch {
		case p.PrincipalID == "":
			return fmt.Errorf("%w: participants[%d].principal_id is empty", ErrInvalidDocument, i)
		case p.Role != connector.RoleAuthor && p.Role != connector.RoleReviewer &&
			p.Role != connector.RoleAttendee && p.Role != connector.RoleAgent:
			return fmt.Errorf("%w: participants[%d].role %q is not a role", ErrInvalidDocument, i, p.Role)
		}
	}
	for i, ref := range d.References {
		if err := ref.validate(); err != nil {
			return fmt.Errorf("%w: references[%d]: %w", ErrInvalidDocument, i, err)
		}
	}
	return nil
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }
