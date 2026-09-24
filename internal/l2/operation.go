package l2

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// OperationKind is what a person did to a scope's topics.
type OperationKind string

// The operation kinds.
const (
	// OperationMerge folds one topic into another in the same scope.
	OperationMerge OperationKind = "merge"
	// OperationSplit moves chosen stances off a topic onto a new one.
	OperationSplit OperationKind = "split"
	// OperationUndo reverses one earlier merge or split.
	OperationUndo OperationKind = "undo"
)

// Valid reports whether k is one of the three.
func (k OperationKind) Valid() bool {
	return k == OperationMerge || k == OperationSplit || k == OperationUndo
}

// ErrConflict is what an operation that the scope's later history makes
// ambiguous wraps. [ConflictError] names the operations in the way.
var ErrConflict = errors.New("conflicting topic operation")

// ErrNotAllowed is what an operation by a principal who may not correct the
// scope's topics wraps.
var ErrNotAllowed = errors.New("not allowed to change topics")

// ConflictError is an undo refused because of what the scope's ledger holds
// after the operation it would reverse.
type ConflictError struct {
	// Operation is the operation the undo was asked for.
	Operation int64
	// Conflicting are the operations in the way, oldest first: the undo that
	// already reversed it, or the later operations still in force on the
	// topics it covers.
	Conflicting []int64
	// Reason says which of the two.
	Reason string
}

func (e *ConflictError) Error() string {
	ids := make([]string, len(e.Conflicting))
	for i, id := range e.Conflicting {
		ids[i] = fmt.Sprint(id)
	}
	return fmt.Sprintf("%s: operation %d %s: operation %s", ErrConflict, e.Operation, e.Reason, strings.Join(ids, ", "))
}

// Is makes a conflict an [ErrConflict].
func (e *ConflictError) Is(target error) bool { return target == ErrConflict }

// OperationRequest is a merge, a split or an undo a person asks for. Which
// fields it uses is decided by its kind; [OperationRequest.Validate] refuses
// one that sets another kind's.
type OperationRequest struct {
	Kind OperationKind
	// Principal is the configured human asking.
	Principal string

	// Into and From are a merge's topics: From is folded into Into.
	Into, From string

	// Topic, Name and Stances are a split's: the stances on Topic that move to
	// a new topic called Name.
	Topic   string
	Name    string
	Stances []string

	// Undoes is the operation an undo reverses.
	Undoes int64
}

// Validate reports a request malformed on its face, before anything is read.
// Whether its topics and stances exist, share a scope and are where it says
// is [ScopeState.Decide]'s to answer.
func (r OperationRequest) Validate() error {
	if r.Principal == "" {
		return fmt.Errorf("%w: a topic operation names no principal", ErrInvalid)
	}
	merge := r.Into != "" || r.From != ""
	split := r.Topic != "" || r.Name != "" || len(r.Stances) > 0
	undo := r.Undoes != 0
	switch r.Kind {
	case OperationMerge:
		switch {
		case split || undo:
			return fmt.Errorf("%w: a merge names only the topic to merge and the topic it goes into", ErrInvalid)
		case r.Into == "" || r.From == "":
			return fmt.Errorf("%w: a merge needs two topics", ErrInvalid)
		case r.Into == r.From:
			return fmt.Errorf("%w: topic %s cannot be merged into itself", ErrInvalid, r.Into)
		}
	case OperationSplit:
		switch {
		case merge || undo:
			return fmt.Errorf("%w: a split names only a topic, a new name and the stances to move", ErrInvalid)
		case r.Topic == "":
			return fmt.Errorf("%w: a split names no topic", ErrInvalid)
		case strings.TrimSpace(r.Name) == "" || len(r.Name) > MaxTopicName:
			return fmt.Errorf("%w: a split's new topic has a name of %d bytes, want 1 to %d", ErrInvalid, len(r.Name), MaxTopicName)
		case len(r.Stances) == 0:
			return fmt.Errorf("%w: a split of topic %s moves no stance", ErrInvalid, r.Topic)
		}
		for i, id := range r.Stances {
			if id == "" {
				return fmt.Errorf("%w: a split's stances[%d] is empty", ErrInvalid, i)
			}
			if slices.Contains(r.Stances[:i], id) {
				return fmt.Errorf("%w: a split names stance %s twice", ErrInvalid, id)
			}
		}
	case OperationUndo:
		switch {
		case merge || split:
			return fmt.Errorf("%w: an undo names only the operation it reverses", ErrInvalid)
		case r.Undoes <= 0:
			return fmt.Errorf("%w: an undo names no operation", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: topic operation kind %q", ErrInvalid, r.Kind)
	}
	return nil
}

// Operation is one entry of a scope's topic ledger: what a person did, what it
// covered, and whether it has been undone. It is appended and never changed;
// the topic and stance rows it is about are not touched by it.
type Operation struct {
	ID        int64
	Kind      OperationKind
	Scope     string
	Principal string
	// Topics are the topics it covers: a merge's [into, from], a split's
	// [topic, new topic], and for an undo those of the operation it reverses.
	Topics []string
	// Stances are the stances it covers: every stance on the merged topic when
	// it was merged, the stances a split moved, and for an undo those of the
	// operation it reverses. Sorted.
	Stances []string
	// Name is a split's new topic name.
	Name string
	// Undoes is the operation an undo reverses.
	Undoes int64
	// UndoneBy is the undo that reversed this operation, and zero while it is
	// in force. It is read from the ledger, not stored on the row.
	UndoneBy int64
	At       time.Time
}

// InForce reports whether the operation shapes the scope's topics now: a
// merge or a split that has not been undone. An undo is a record of a
// reversal and shapes nothing itself.
func (o Operation) InForce() bool { return o.Kind != OperationUndo && o.UndoneBy == 0 }

// SplitTopicID is the id of the topic a split creates. It is a function of
// what the split was asked for, so a split undone and made again creates the
// same topic.
func SplitTopicID(scope, topic, name string, stances []string) string {
	sorted := slices.Clone(stances)
	slices.Sort(sorted)
	return "topic:" + digest(append([]string{"split", scope, topic, name}, sorted...)...)
}

// ScopeState is what a scope holds that an operation is decided against:
// its topic rows, the stance rows on them and its ledger.
type ScopeState struct {
	Scope string
	// Topics are the ids of the topic rows in the scope.
	Topics []string
	// Stances maps each stance on those topics to the topic row it was
	// written on.
	Stances map[string]string
	// Operations is the scope's ledger, oldest first, with UndoneBy set.
	Operations []Operation
}

// arrangement is what the operations in force make of a scope's topics: which
// topics stand, what a split named them, and which topic each stance is on.
type arrangement struct {
	live    map[string]bool
	stances map[string]string
	// mergedBy is the operation that merged a topic away, and into the topic
	// it went into.
	mergedBy map[string]int64
	into     map[string]string
	// source is the topic each split in the ledger, in force or not, took its
	// stances from, keyed by the topic it created.
	source map[string]string
}

// arrange replays the operations in force, oldest first, over the rows. An
// undo is only allowed where nothing later in force touches what it reverses,
// so leaving an undone operation out of the replay is reversing it.
//
// A split's topic has no row until a stance is written on it after the split
// ([Store.Target]). Such a row is a topic only while a split that creates it is
// in force: before that, and once it is undone, the stances written on it are
// on the topic the split took its stances from, as that topic is now.
func (s ScopeState) arrange() arrangement {
	a := arrangement{
		live: map[string]bool{}, stances: map[string]string{},
		mergedBy: map[string]int64{}, into: map[string]string{}, source: map[string]string{},
	}
	for _, op := range s.Operations {
		if op.Kind == OperationSplit {
			a.source[op.Topics[1]] = op.Topics[0]
		}
	}
	for _, t := range s.Topics {
		if _, split := a.source[t]; !split {
			a.live[t] = true
		}
	}
	for st, t := range s.Stances {
		a.stances[st] = t
	}
	for _, op := range s.Operations {
		if !op.InForce() {
			continue
		}
		switch op.Kind {
		case OperationMerge:
			into, from := op.Topics[0], op.Topics[1]
			for st, t := range a.stances {
				if t == from {
					a.stances[st] = into
				}
			}
			delete(a.live, from)
			a.mergedBy[from], a.into[from] = op.ID, into
		case OperationSplit:
			for _, st := range op.Stances {
				a.stances[st] = op.Topics[1]
			}
			a.live[op.Topics[1]] = true
			delete(a.mergedBy, op.Topics[1])
			delete(a.into, op.Topics[1])
		}
	}
	for st, t := range a.stances {
		if !a.live[t] {
			a.stances[st] = a.effective(t)
		}
	}
	return a
}

// effective is the topic a topic is now: itself while it stands, the topic a
// merge put it into, and for a split's topic that does not stand, the topic
// the split took its stances from, each as it is now in turn. A topic the
// scope does not know is itself.
func (a arrangement) effective(topic string) string {
	for seen := map[string]bool{}; !seen[topic]; {
		seen[topic] = true
		if a.live[topic] {
			return topic
		}
		if into, ok := a.into[topic]; ok {
			topic = into
			continue
		}
		if from, ok := a.source[topic]; ok {
			topic = from
			continue
		}
		return topic
	}
	return topic
}

// on is the stances the arrangement puts on a topic, sorted.
func (a arrangement) on(topic string) []string {
	var out []string
	for st, t := range a.stances {
		if t == topic {
			out = append(out, st)
		}
	}
	slices.Sort(out)
	return out
}

// standing refuses a topic the arrangement does not have.
func (a arrangement) standing(scope, topic string) error {
	if a.live[topic] {
		return nil
	}
	if by, ok := a.mergedBy[topic]; ok {
		return fmt.Errorf("%w: topic %s was merged away by operation %d", ErrInvalid, topic, by)
	}
	return fmt.Errorf("%w: topic %s is not a topic in scope %q", ErrInvalid, topic, scope)
}

// Decide is the operation a request records against this state, without its
// id or time, or why it may not be recorded: a topic or stance that is not
// where the request says in this scope ([ErrInvalid]), an undo of an operation
// the scope does not hold ([ErrNotFound]), or an undo that the ledger makes
// ambiguous ([ConflictError]).
//
// Topics and stances are where the operations in force put them, so a split
// can take back part of a merge, and a topic merged away can be neither
// merged nor split until the merge is undone.
func (s ScopeState) Decide(req OperationRequest) (Operation, error) {
	if err := req.Validate(); err != nil {
		return Operation{}, err
	}
	op := Operation{Kind: req.Kind, Scope: s.Scope, Principal: req.Principal}
	a := s.arrange()
	switch req.Kind {
	case OperationMerge:
		for _, t := range []string{req.Into, req.From} {
			if err := a.standing(s.Scope, t); err != nil {
				return Operation{}, err
			}
		}
		op.Topics = []string{req.Into, req.From}
		op.Stances = orEmpty(a.on(req.From))
	case OperationSplit:
		if err := a.standing(s.Scope, req.Topic); err != nil {
			return Operation{}, err
		}
		on := a.on(req.Topic)
		for _, st := range req.Stances {
			if !slices.Contains(on, st) {
				return Operation{}, fmt.Errorf("%w: stance %s is not on topic %s", ErrInvalid, st, req.Topic)
			}
		}
		if len(req.Stances) == len(on) {
			return Operation{}, fmt.Errorf("%w: a split of topic %s moves every stance on it and leaves it empty", ErrInvalid, req.Topic)
		}
		op.Name = req.Name
		op.Stances = slices.Sorted(slices.Values(req.Stances))
		op.Topics = []string{req.Topic, SplitTopicID(s.Scope, req.Topic, req.Name, req.Stances)}
	case OperationUndo:
		i := slices.IndexFunc(s.Operations, func(o Operation) bool { return o.ID == req.Undoes })
		if i < 0 {
			return Operation{}, fmt.Errorf("%w: operation %d in scope %q", ErrNotFound, req.Undoes, s.Scope)
		}
		target := s.Operations[i]
		if target.Kind == OperationUndo {
			return Operation{}, fmt.Errorf("%w: operation %d is an undo, and an undo is not undone: make the operation again", ErrInvalid, target.ID)
		}
		if target.UndoneBy != 0 {
			return Operation{}, &ConflictError{Operation: target.ID, Conflicting: []int64{target.UndoneBy}, Reason: "was already undone by"}
		}
		var later []int64
		for _, o := range s.Operations[i+1:] {
			if o.InForce() && slices.ContainsFunc(o.Topics, func(t string) bool { return slices.Contains(target.Topics, t) }) {
				later = append(later, o.ID)
			}
		}
		if len(later) > 0 {
			return Operation{}, &ConflictError{Operation: target.ID, Conflicting: later, Reason: "has later operations on its topics still in force; undo them first"}
		}
		op.Topics, op.Stances, op.Undoes = slices.Clone(target.Topics), slices.Clone(target.Stances), target.ID
	}
	return op, nil
}
