package l2

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
)

// MaxAssertionEvidence is how many L1 documents one assertion may cite. Every
// one is checked for the caller and again on every read of the stance; a
// position that needs more than this is several positions.
const MaxAssertionEvidence = 32

// Assertion is a stance an agent wrote through the API's `assert` call
// (docs/design.md#read-and-assert-api), as the payload.native of the L0
// `assertion` event that carries it. The API writes the event and the
// assertion worker appends the stance from it, with no model call: the agent
// said what the position is and which topic it is on.
type Assertion struct {
	// Topic is the id of an existing topic.
	Topic string `json:"topic"`
	// Position is what the agent says, cleaned the way a model's positions are
	// and within [MaxPosition].
	Position string `json:"position"`
	// Evidence is the L1 document ids the agent cites, sorted and without
	// duplicates, so that two orders of one citation are one request.
	Evidence []string `json:"evidence"`
	// Agent is the principal id of the agent that asserted, and the stance's
	// author.
	Agent string `json:"agent"`
	// Principal is the person the agent acted for.
	Principal string `json:"principal"`
}

// NativeID is the native id of the event that carries the assertion. It is
// derived from everything in it, so a retry of the same request is the same
// event (docs/connector-contract.md#idempotency-edits-and-deletions) and one
// that differs in anything is another.
func (a Assertion) NativeID() string {
	parts := append([]string{a.Agent, a.Principal, a.Topic, a.Position}, a.Evidence...)
	return "assertion:" + digest(parts...)
}

// AssertionOf reads the assertion an L0 event carries.
func AssertionOf(ev connector.Event) (Assertion, error) {
	if ev.Kind != connector.KindAssertion {
		return Assertion{}, fmt.Errorf("%w: event %s is a %s, not an assertion", ErrInvalid, ev.ID, ev.Kind)
	}
	var a Assertion
	if err := json.Unmarshal(ev.Payload.Native, &a); err != nil {
		return Assertion{}, fmt.Errorf("%w: event %s does not carry an assertion: %w", ErrInvalid, ev.ID, err)
	}
	switch {
	case a.Topic == "" || a.Position == "" || a.Agent == "":
		return Assertion{}, fmt.Errorf("%w: event %s names no topic, position or agent", ErrInvalid, ev.ID)
	case len(a.Evidence) == 0 || len(a.Evidence) > MaxAssertionEvidence:
		return Assertion{}, fmt.Errorf("%w: event %s cites %d documents, want 1 to %d", ErrInvalid, ev.ID, len(a.Evidence), MaxAssertionEvidence)
	}
	return a, nil
}

// AssertedEvidence is what authority knows about a stance an agent asserted:
// class `agent`, from the source of the event it was read from. It is the
// stance's own, whatever the documents it cites are — an agent citing a merged
// pull request is still an agent proposing, and under the default policy it
// ranks last and does not ratify on its own (docs/config.md#authority).
func AssertedEvidence(eventID string) Evidence {
	source, _, err := connector.ParseEventID(eventID)
	if err != nil {
		// Stance.Validate refused this before it was stored. No source is a
		// source no policy ratifies from.
		source = ""
	}
	return Evidence{Class: config.ArtifactAgent, Source: source}
}

// AssertedTier is the tier an asserted stance is written with: ratified only
// where the policy lets class `agent` from the event's source ratify on its
// own. Like [RecordedTier] it is a record of the moment; a read computes the
// tier with [Stand].
func AssertedTier(policy config.Policy, eventID string) Tier {
	ev := AssertedEvidence(eventID)
	if policy.RatifiedByArtifact(ev.Class, ev.Source) {
		return TierRatified
	}
	return TierInferred
}

// UnappendedAssertions is every `assertion` event Hearsay wrote that no stance
// was appended from yet, oldest first. It is what lets the assertion worker's
// startup pick up an assertion whose job ran out of attempts.
func (s *Store) UnappendedAssertions(ctx context.Context) ([]string, error) {
	rows, err := s.db.Query(ctx, `
SELECT e.id FROM l0_events e
WHERE e.source = $1 AND e.kind = $2
  AND NOT EXISTS (SELECT 1 FROM l2_stances s WHERE s.assertion = e.id)
ORDER BY e.occurred_at, e.id`, connector.SelfSource, string(connector.KindAssertion))
	if err != nil {
		return nil, fmt.Errorf("listing assertions with no stance: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("listing assertions with no stance: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing assertions with no stance: %w", err)
	}
	return out, nil
}

// SortedEvidence is a citation as an [Assertion] holds it: sorted, without
// duplicates.
func SortedEvidence(ids []string) []string { return sortedUnique(ids) }
