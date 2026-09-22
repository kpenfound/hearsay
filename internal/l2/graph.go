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

// The tiers.
const (
	// TierRatified is a stance a human confirmed or an authoritative artifact
	// carried. Agents act on it.
	TierRatified Tier = "ratified"
	// TierInferred is the assertion pipeline's best reading. Agents cite it
	// and proceed.
	TierInferred Tier = "inferred"
	// TierContested is a stance recent ones disagree with. Agents ask. Nothing
	// in this build writes it yet.
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
	// ACL is who may read the topic: the access list of the document that
	// opened it.
	ACL connector.ACL
	// OpenedBy is the L1 document the topic was opened from.
	OpenedBy  string
	CreatedAt time.Time
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
	ID       string
	TopicID  string
	Position string
	// Author is the principal id of whoever took the position, empty where the
	// document's author did not resolve.
	Author string
	// StatedAt is when the evidence says the position was taken.
	StatedAt time.Time
	// Evidence is the L1 document ids the stance rests on. The first is the
	// document it was read from.
	Evidence []string
	// Supersedes is the stance this one replaced on its topic, empty for the
	// first.
	Supersedes string
	Tier       Tier
	// ACL is the most restrictive access list of the evidence.
	ACL       connector.ACL
	CreatedAt time.Time
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
	return nil
}

// Current is the stance a topic stands at, given its history in the order
// [Store.StanceHistory] returns it: the newest stated that a later reading of
// its own document has not retired. It is [RetiredSQL]'s rule, and internal/l3's,
// for a history already read. It reports false for a topic with no stance.
func Current(history []Stance) (Stance, bool) {
	from := make(map[string]string, len(history))
	for _, st := range history {
		from[st.ID] = st.Evidence[0]
	}
	retired := map[string]bool{}
	for _, st := range history {
		if st.Supersedes != "" && from[st.Supersedes] == st.Evidence[0] {
			retired[st.Supersedes] = true
		}
	}
	for i := len(history) - 1; i >= 0; i-- {
		if !retired[history[i].ID] {
			return history[i], true
		}
	}
	return Stance{}, false
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

// digest is a short hex id over parts that cannot run into each other: each is
// length-prefixed, so ("ab", "c") and ("a", "bc") differ.
func digest(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%d:%s;", len(p), p)
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}
