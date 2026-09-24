package l2

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
)

// Tier is how far a stance is to be trusted (docs/design.md#topics-and-stances).
type Tier string

// Judgement records how a stance relates to the current position offered to
// the assertion worker. Empty means no comparison was recorded.
type Judgement string

const (
	// JudgementUnknown is a stance opened on a new topic or a historical row.
	JudgementUnknown Judgement = ""
	// JudgementChanges means the new position differs from the one shown.
	JudgementChanges Judgement = "changes"
	// JudgementRestates means it says the same thing, even in other words.
	JudgementRestates Judgement = "restates"
)

// Valid reports whether the judgement can be stored.
func (j Judgement) Valid() bool {
	return j == JudgementUnknown || j == JudgementChanges || j == JudgementRestates
}

// The tiers.
const (
	// TierRatified is a stance a human confirmed or an authoritative artifact
	// carried. Agents act on it.
	TierRatified Tier = "ratified"
	// TierInferred is the assertion pipeline's best reading. Agents cite it
	// and proceed.
	TierInferred Tier = "inferred"
	// TierContested is a stance recent ones disagree with. Agents ask. It is
	// only ever computed ([Stand]); nothing writes it on a row.
	TierContested Tier = "contested"
)

// Valid reports whether t is one of the three.
func (t Tier) Valid() bool { return t == TierRatified || t == TierInferred || t == TierContested }

// Bounds on what a topic and a stance hold. They are the table's, and they are
// what the assertion worker's schema asks a model for.
const (
	MaxTopicName = 1000
	MaxPosition  = 2000
)

// Topic is a question the team has taken positions on.
type Topic struct {
	ID string
	// Scope is the serial key the topic belongs to ([ScopeKey]).
	Scope string
	Name  string
	// About is the entity ids the topic is about.
	About []string
	// JoinKeys are the join keys of every document with a stance on it.
	JoinKeys []string
	// ACL is the access list of the document that opened the topic, as it was
	// when the topic was opened. It is a record, not who may read the topic
	// now: a read decides that from the document's current access list
	// ([Access]).
	ACL connector.ACL
	// OpenedBy is the L1 document the topic was opened from. A read of a
	// split's topic leaves it empty: no document opened it.
	OpenedBy  string
	CreatedAt time.Time
	// Operations are, on a read, the merges and splits in force that shaped
	// the topic, oldest first: a merge into it or of a topic merged into it,
	// the split that created it, and a split that moved stances off it
	// ([Store.Topic]). Nothing writes them on the row.
	Operations []Operation
}

// Validate reports a topic the store refuses.
func (t Topic) Validate() error {
	switch {
	case t.ID == "":
		return fmt.Errorf("%w: a topic has no id", ErrInvalid)
	case t.Scope == "":
		return fmt.Errorf("%w: topic %s has no scope", ErrInvalid, t.ID)
	case t.Name == "" || len(t.Name) > MaxTopicName:
		return fmt.Errorf("%w: topic %s has a name of %d bytes, want 1 to %d", ErrInvalid, t.ID, len(t.Name), MaxTopicName)
	case len(t.ACL) == 0:
		return fmt.Errorf("%w: topic %s has an empty access list, and a topic nobody may read cannot be read back", ErrInvalid, t.ID)
	case t.OpenedBy == "":
		return fmt.Errorf("%w: topic %s names no document it was opened from", ErrInvalid, t.ID)
	}
	return nil
}

// Stance is one position on a topic, from one source, at one time.
type Stance struct {
	ID        string
	TopicID   string
	Position  string
	Judgement Judgement
	// Author is the principal id of whoever took the position, empty where the
	// document's author did not resolve.
	Author string
	// StatedAt is when the evidence says the position was taken.
	StatedAt time.Time
	// Evidence is the L1 document ids the stance rests on. The first is the
	// document it was read from, unless Assertion is set.
	Evidence []string
	// Assertion is the L0 `assertion` event the stance was read from, for a
	// stance an agent wrote through the API's `assert` call, and empty for one
	// the assertion worker read from a document. Its evidence is what the agent
	// cited, none of which it was read from, and its authority is its own
	// ([AssertedEvidence]) rather than that of what it cites.
	Assertion string
	// Withdrawn marks a history entry made when none of its evidence survives.
	// Its position explains the change, but it cannot be a current position.
	Withdrawn bool
	// Supersedes is the stance this one replaced on its topic, empty for the
	// first.
	Supersedes string
	// Tier is the tier the stance was written with ([RecordedTier]), a record
	// of that moment and part of its id. The tier a read serves is computed
	// ([Stand]).
	Tier Tier
	// ACL is the access list of the document the stance was read from, as it
	// was when it was read. It is a record, not who may read the stance now: a
	// read decides that from every piece of evidence's current access list
	// ([Access]).
	ACL       connector.ACL
	CreatedAt time.Time

	// The rest is set on a read, from the ledger and the other rows, and never
	// written.

	// Retired reports that a later reading of the stance's own origin
	// superseded it, on whatever topic that reading is now ([RetiredSQL]).
	Retired bool
	// SupersedesTopic is the topic the stance Supersedes names is on now,
	// where that is not this stance's topic: the edge crosses a split, or a
	// merge since undone. Empty where the edge stays on the topic.
	SupersedesTopic string
	// SupersededAcross are the stances on other topics now that supersede this
	// one: the same crossing edges, seen from this end.
	SupersededAcross []StanceRef
}

// Validate reports a stance the store refuses.
func (s Stance) Validate() error {
	switch {
	case s.ID == "":
		return fmt.Errorf("%w: a stance has no id", ErrInvalid)
	case s.TopicID == "":
		return fmt.Errorf("%w: stance %s has no topic", ErrInvalid, s.ID)
	case s.Position == "" || len(s.Position) > MaxPosition:
		return fmt.Errorf("%w: stance %s has a position of %d bytes, want 1 to %d", ErrInvalid, s.ID, len(s.Position), MaxPosition)
	case !s.Judgement.Valid():
		return fmt.Errorf("%w: stance %s has judgement %q", ErrInvalid, s.ID, s.Judgement)
	case s.StatedAt.IsZero():
		return fmt.Errorf("%w: stance %s has no time", ErrInvalid, s.ID)
	case len(s.Evidence) == 0:
		return fmt.Errorf("%w: stance %s has no evidence, so it could not be followed back or re-run", ErrInvalid, s.ID)
	case !s.Tier.Valid():
		return fmt.Errorf("%w: stance %s has tier %q", ErrInvalid, s.ID, s.Tier)
	case len(s.ACL) == 0:
		return fmt.Errorf("%w: stance %s has an empty access list", ErrInvalid, s.ID)
	case s.Supersedes == s.ID:
		return fmt.Errorf("%w: stance %s supersedes itself", ErrInvalid, s.ID)
	}
	for i, id := range s.Evidence {
		if id == "" {
			return fmt.Errorf("%w: stance %s: evidence[%d] is empty", ErrInvalid, s.ID, i)
		}
	}
	if s.Assertion != "" {
		if _, _, err := connector.ParseEventID(s.Assertion); err != nil {
			return fmt.Errorf("%w: stance %s: assertion: %w", ErrInvalid, s.ID, err)
		}
	}
	return nil
}

// origin is what a stance was read from: its assertion event, or its first
// piece of evidence. One origin holds at most one live stance per topic.
func (s Stance) origin() string {
	if s.Assertion != "" {
		return s.Assertion
	}
	return s.Evidence[0]
}

// Current is the head of a topic's supersession chain, given its history in
// the order [Store.StanceHistory] returns it: the newest stated that a later
// reading of its own document has not retired. It is [RetiredSQL]'s rule for a
// history already read, and it is the position the assertion worker shows a
// model and the stance a new one supersedes. It is not what a read serves as
// the topic's current stance — that is [Stand]'s, which weighs authority. It
// reports false for a topic with no stance.
func Current(history []Stance) (Stance, bool) {
	retired := retiredIn(history)
	for i := len(history) - 1; i >= 0; i-- {
		if !retired[history[i].ID] && !history[i].Withdrawn {
			return history[i], true
		}
	}
	return Stance{}, false
}

// retiredIn is the stances in a history that a later reading of their own
// origin replaced.
func retiredIn(history []Stance) map[string]bool {
	from := make(map[string]string, len(history))
	for _, st := range history {
		from[st.ID] = st.origin()
	}
	retired := map[string]bool{}
	for _, st := range history {
		if st.Withdrawn || st.Retired {
			retired[st.ID] = true
		}
		if st.Supersedes != "" && from[st.Supersedes] == st.origin() {
			retired[st.Supersedes] = true
		}
	}
	return retired
}

// TopicID is the id of a topic opened from a document. It is a function of the
// scope, the document, the topic's name and where in the answer it came, so a
// job run twice over the same answer opens the same topic rather than two.
func TopicID(scope, docID string, index int, name string) string {
	return "topic:" + digest(scope, docID, fmt.Sprint(index), name)
}

// StanceID identifies one reading of a document taking a position on a topic
// at a tier. A new document version or tier gets a new row even when its words
// match an earlier reading; a retry of the same reading gets the same id.
func StanceID(topicID, docID, position string, distilledAt time.Time, tier Tier) string {
	return "stance:" + digest(topicID, docID, position, distilledAt.UTC().Format(time.RFC3339Nano), string(tier))
}

// AssertionStanceID identifies the stance an `assertion` event writes on its
// topic. The event id already names everything the agent said — its native id
// is [Assertion.NativeID] — so a retry of the same request, and a job run
// twice, get the same id.
func AssertionStanceID(topicID, eventID string) string {
	return "stance:" + digest("assertion", topicID, eventID)
}

// digest is a short hex id over parts that cannot run into each other: each is
// length-prefixed, so ("ab", "c") and ("a", "bc") differ.
func digest(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%d:%s;", len(p), p)
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}
