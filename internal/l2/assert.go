package l2

import (
	"context"
	"slices"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/queue"
)

// AssertKindName is the queue kind the assertion worker consumes. It is a
// contract with whatever enqueues one — the distiller, in the transaction that
// writes the document — and with an operator reading `queue_job`.
const AssertKindName = "assert"

// AssertKind is the kind both ends of an assert job share. It is serialized:
// two jobs on one scope never run at once, because two workers matching one
// document each against a topic list that does not yet hold the other's topic
// both open one, and two stances appended to one topic at once both supersede
// the same predecessor (ADR-0007).
func AssertKind() queue.Kind { return queue.Kind{Name: AssertKindName, Serialized: true} }

// ScopeKey is the serial key a document's assert job runs under, and the scope
// every topic it opens belongs to: the first configured scope, by id, that
// covers the artifact's container, and `source:<source id>` for an artifact no
// scope covers.
//
// Topics are only matched within one key. That is what makes serializing per
// key enough: a job can only read and write topics under its own key, so two
// jobs that run at once cannot race on a topic. It is also the limit of the
// minimal graph — a document two scopes cover takes positions in the first of
// them only.
//
// A scope id is lowercase letters, digits, `-` and `_` (docs/config.md), so it
// cannot collide with the `source:` form.
func ScopeKey(repo config.Repo, source, container string) string {
	var ids []string
	for _, s := range repo.Scopes {
		if s.Covers(source, container) {
			ids = append(ids, s.ID)
		}
	}
	if len(ids) == 0 {
		return "source:" + source
	}
	slices.Sort(ids)
	return ids[0]
}

// EnqueueAssertion adds the assert job for a document, inside whatever
// transaction the caller is in, and does nothing for a document whose outcome
// does not enter the assertion pipeline (docs/design.md#l1-distilled-documents).
// It reports whether a job was asked for.
//
// The container is the artifact's, from its root event: a document does not
// carry one, and the scope key needs it.
func EnqueueAssertion(ctx context.Context, q queue.Querier, repo config.Repo, doc l1.Document, container string) (bool, error) {
	if !doc.Body.OutcomeKind.Asserts() {
		return false, nil
	}
	_, err := queue.Enqueue(ctx, q, queue.Request{
		Kind:      AssertKind(),
		TargetID:  doc.ID,
		SerialKey: ScopeKey(repo, doc.Source.System, container),
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

// JoinKeys is what a document is matched to topics on: the tracker items,
// commits and links it points at, and the artifact it is itself, so that a pull
// request that says "fixes #12" joins the issue it fixes.
//
// Issues, pull requests and bare `#12` references are one key space, `item:`,
// because on GitHub they share one numbering and a document rarely says which it
// means. People and code entities are not join keys: everybody's documents name
// the same people and the same systems, and a key everything shares joins
// everything, which is the same as joining nothing.
func JoinKeys(doc l1.Document) []string {
	var keys []string
	switch doc.Kind {
	case l1.KindIssue, l1.KindPR:
		keys = append(keys, "item:"+doc.Source.NativeID)
	case l1.KindCommit:
		keys = append(keys, "commit:"+doc.Source.NativeID)
	}
	for _, ref := range doc.References {
		switch ref.Type {
		case l1.RefIssue, l1.RefPR, l1.RefTrackerItem:
			keys = append(keys, "item:"+ref.ID)
		case l1.RefCommit:
			keys = append(keys, "commit:"+ref.ID)
		case l1.RefURL:
			keys = append(keys, "url:"+ref.ID)
		}
	}
	return sortedUnique(keys)
}

// TrackerItems is the `tracker_item` entities a document names: the issue or
// pull request it is, and every item it references, wherever a configured
// scope's tracker maps that repository in the document's source. An item no
// tracker maps is not an entity — no id is invented for it — and stays a join
// key only.
func TrackerItems(repo config.Repo, doc l1.Document) []Entity {
	var items []string
	if doc.Kind == l1.KindIssue || doc.Kind == l1.KindPR {
		items = append(items, doc.Source.NativeID)
	}
	for _, ref := range doc.References {
		if ref.Type == l1.RefIssue || ref.Type == l1.RefPR || ref.Type == l1.RefTrackerItem {
			items = append(items, ref.ID)
		}
	}
	byID := map[string]*Entity{}
	for _, item := range items {
		if e, ok := TrackerItem(repo, doc.Source.System, item); ok {
			byID[e.ID] = &e
		}
	}
	return sorted(byID)
}
