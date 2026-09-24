package l2

import (
	"slices"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/l1"
)

// Evidence is what authority knows about one L1 document a stance rests on:
// the class of artifact it came out of and the source it came from.
type Evidence struct {
	Class  config.ArtifactClass
	Source string
}

// EvidenceOf is a document's evidence for authority.
func EvidenceOf(doc l1.Document) Evidence {
	return Evidence{Class: doc.ArtifactClass, Source: doc.Source.System}
}

// TierInputs is everything a topic's standing is computed from. Nothing in it
// is the wall clock, so an unchanged graph under an unchanged policy stands the
// same way on every read.
type TierInputs struct {
	// History is every stance on the topic, in the order
	// [Store.StanceHistory] returns them.
	History []Stance
	// Evidence is keyed by L1 document id. A document missing from it — one
	// that was retracted, say — has no class: it ranks level with a class the
	// policy leaves out, below everything it ranks, and ratifies nothing.
	Evidence map[string]Evidence
	// Policy is the authority in force for the topic's scope.
	Policy config.Policy
	// Ratified is the stances a person ratified by hand, and Demoted the ones
	// a person demoted: what the gestures in force say ([Store.Corrections]).
	Ratified, Demoted []string
	// Readable reports whether the reader the topic is assessed for may read
	// a stance ([Access.Stance]). Only a stance they may read can make the
	// topic contested for them. Nil counts every stance.
	Readable func(Stance) bool
}

// Standing is where a topic stands under a policy: the stance it is at and the
// tier that stance is served at.
type Standing struct {
	Current Stance
	Tier    Tier
}

// Stand computes a topic's standing (docs/design.md#topics-and-stances). It is
// what every read serves; the tier stored on a stance row is what the stance's
// own evidence carried when it was written ([RecordedTier]), and is not
// consulted here, so a change to the policy takes effect on the next read.
//
//   - The stances compared are the live ones — not retired by a later reading
//     of their own document ([Current]) — stated no more than the policy's
//     contested window before the newest live stance. The window is measured
//     between stances and includes its boundary.
//   - The current stance is the one among them whose evidence ranks highest
//     under the policy; the most recently stated wins a tie.
//   - The topic is contested where a person demoted the current stance, and
//     where another stance compared disagrees with the current one, the
//     current one does not strictly outrank it, and the reader may read it
//     ([TierInputs.Readable]): a stance hidden from them does not contest
//     anything for them. The current stance is chosen from every stance,
//     readable or not; one the reader may not read is withheld from them by
//     the caller, not replaced by an older one. A demotion is of a stance, so
//     a newer stance the topic stands at instead is not contested by it.
//   - Otherwise it is ratified where the current stance's evidence ratifies on
//     its own under `ratified_by.artifacts` and `ratified_by.sources`, or a
//     person ratified that stance; and inferred where it is not.
//
// Two stances disagree only where a recorded judgement says so: somewhere on
// the supersession path between them a stance was judged to change the position
// it followed. A restatement joins its predecessor's position, and a stance
// with no recorded judgement is not read as a disagreement — reads do not
// compare position text. Stances with no path between them have no recorded
// relation and do not disagree.
//
// It reports false for a topic with no live stance.
func Stand(in TierInputs) (Standing, bool) {
	retired := retiredIn(in.History)
	var live []Stance
	for _, st := range in.History {
		if !retired[st.ID] && !st.Withdrawn {
			live = append(live, st)
		}
	}
	if len(live) == 0 {
		return Standing{}, false
	}
	newest := live[0].StatedAt
	for _, st := range live[1:] {
		if st.StatedAt.After(newest) {
			newest = st.StatedAt
		}
	}
	from := newest.Add(-in.Policy.Window())
	compared := slices.DeleteFunc(live, func(st Stance) bool { return st.StatedAt.Before(from) })

	current, currentRank, currentBy := compared[0], -1, Evidence{}
	for _, st := range compared {
		rank, by := in.rank(st)
		if rank > currentRank || rank == currentRank && !st.StatedAt.Before(current.StatedAt) {
			current, currentRank, currentBy = st, rank, by
		}
	}

	if slices.Contains(in.Demoted, current.ID) {
		return Standing{Current: current, Tier: TierContested}, true
	}
	paths := newSupersession(in.History)
	for _, other := range compared {
		if other.ID == current.ID {
			continue
		}
		if in.Readable != nil && !in.Readable(other) {
			continue
		}
		if rank, _ := in.rank(other); currentRank <= rank && paths.disagree(current.ID, other.ID) {
			return Standing{Current: current, Tier: TierContested}, true
		}
	}
	if in.Policy.RatifiedByArtifact(currentBy.Class, currentBy.Source) || slices.Contains(in.Ratified, current.ID) {
		return Standing{Current: current, Tier: TierRatified}, true
	}
	return Standing{Current: current, Tier: TierInferred}, true
}

// rank is a stance's authority under the policy: that of its highest-ranked
// evidence document, the first listed on a tie, and that document's evidence.
// A stance none of whose evidence is known ranks 0 and names no evidence. A
// stance an agent asserted ranks as its own evidence, class `agent`
// ([AssertedEvidence]), whatever it cites.
func (in TierInputs) rank(st Stance) (int, Evidence) {
	if st.Assertion != "" {
		by := AssertedEvidence(st.Assertion)
		rank, _ := in.Policy.Rank(by.Class)
		return rank, by
	}
	best, by := -1, Evidence{}
	for _, id := range st.Evidence {
		ev, ok := in.Evidence[id]
		if !ok {
			continue
		}
		if rank, _ := in.Policy.Rank(ev.Class); rank > best {
			best, by = rank, ev
		}
	}
	if best < 0 {
		return 0, Evidence{}
	}
	return best, by
}

// RecordedTier is the tier a stance is written with: ratified where its own
// document ratifies on its own under the policy in force when it is written,
// inferred otherwise. It is a record of the moment, part of the stance's id
// ([StanceID]); what a read serves is [Stand]'s.
func RecordedTier(policy config.Policy, doc l1.Document) Tier {
	ev := EvidenceOf(doc)
	if policy.RatifiedByArtifact(ev.Class, ev.Source) {
		return TierRatified
	}
	return TierInferred
}

// supersession is a topic's stances as the forest their supersedes pointers
// make, each edge carrying the judgement of the stance that made it.
type supersession struct {
	parent  map[string]string
	changes map[string]bool
}

func newSupersession(history []Stance) supersession {
	s := supersession{parent: map[string]string{}, changes: map[string]bool{}}
	known := make(map[string]bool, len(history))
	for _, st := range history {
		known[st.ID] = true
	}
	for _, st := range history {
		if st.Supersedes != "" && known[st.Supersedes] {
			s.parent[st.ID] = st.Supersedes
		}
		s.changes[st.ID] = st.Judgement == JudgementChanges
	}
	return s
}

// disagree reports whether the path between two stances crosses an edge judged
// `changes`. It walks up from a to every ancestor, noting whether a change was
// crossed on the way, then up from b until it meets one of them.
func (s supersession) disagree(a, b string) bool {
	changedTo := map[string]bool{}
	changed := false
	for id := a; ; {
		if _, seen := changedTo[id]; seen {
			break
		}
		changedTo[id] = changed
		up, ok := s.parent[id]
		if !ok {
			break
		}
		changed = changed || s.changes[id]
		id = up
	}
	changed = false
	seen := map[string]bool{}
	for id := b; !seen[id]; {
		if before, ok := changedTo[id]; ok {
			return changed || before
		}
		seen[id] = true
		up, ok := s.parent[id]
		if !ok {
			return false
		}
		changed = changed || s.changes[id]
		id = up
	}
	return false
}
