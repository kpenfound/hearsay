package work

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/kpenfound/hearsay/internal/connector"
)

// KindDecision is the L0 extension kind of a decision. It is based on
// document, so everything that knows only core kinds reads it as one, and it is
// distinct so that a layer that gives decisions their own handling can tell
// them from the documents beside them.
const KindDecision connector.Kind = "work.decision"

// ErrAmbiguous is returned when more than one held record answers to an id the
// project-wide rule says names at most one. Intake prevents it, so seeing it
// means something wrote around intake.
var ErrAmbiguous = errors.New("more than one held record has the id")

// Held is one held (live, latest-revision) document or decision of a project,
// on either source of a pair.
type Held struct {
	// Kind is KindDocumentRecord or KindDecisionRecord.
	Kind string
	// Project, Workstream (empty at project level) and ID place the record.
	Project, Workstream, ID string
	// Source and Artifact are where it is held in L0.
	Source, Artifact string
	// Event is the record's current revision.
	Event  connector.Event
	native Native
}

// Where describes where the record is held, for a refusal that says so.
func (h Held) Where() string {
	level := "project level"
	if h.Workstream != "" {
		level = "workstream " + h.Workstream
	}
	return fmt.Sprintf("source %s, %s (artifact %s)", h.Source, level, h.Artifact)
}

// Native returns the record's decoded structured fields.
func (h Held) Native() Native { return h.native }

// Supersedes is the decision id this decision's latest revision names, or "".
func (h Held) Supersedes() string { return h.native.Supersedes }

// SpecDocument is the document id a decision links to, or "".
func (h Held) SpecDocument() string { return h.native.SpecDocument }

// Decode reads a document or decision out of a current L0 event of a work
// source. It reports false for anything else: projects, workstreams, updates,
// tombstones and events of other sources' shapes.
func Decode(ev connector.Event) (Held, bool) {
	var kind string
	switch ev.Kind {
	case connector.KindDocument:
		kind = KindDocumentRecord
	case KindDecision:
		kind = KindDecisionRecord
	default:
		return Held{}, false
	}
	if len(ev.Payload.Native) == 0 {
		return Held{}, false
	}
	var n Native
	if err := json.Unmarshal(ev.Payload.Native, &n); err != nil {
		return Held{}, false
	}
	id := n.Document
	if kind == KindDecisionRecord {
		id = n.Decision
	}
	if id == "" || n.Project == "" {
		return Held{}, false
	}
	return Held{Kind: kind, Project: n.Project, Workstream: n.Workstream, ID: id,
		Source: ev.Source, Artifact: ev.Payload.Artifact, Event: ev, native: n}, true
}

// SourceReader is one source of a pair and the way to read its current
// artifacts.
type SourceReader struct {
	Source string
	Reader connector.ArtifactReader
}

// Collect gathers the held documents and decisions of a project across the
// given sources, which are the work source and its owner source. Artifacts a
// source's reader serves are its live latest revisions, so a deleted record is
// absent. The result is in a stable order.
func Collect(ctx context.Context, project string, sources ...SourceReader) ([]Held, error) {
	var out []Held
	for _, src := range sources {
		events, err := src.Reader.CurrentArtifacts(ctx, src.Source)
		if err != nil {
			return nil, fmt.Errorf("read records of source %s: %w", src.Source, err)
		}
		out = append(out, Records(events, project)...)
	}
	sortHeld(out)
	return out, nil
}

// Records picks the held documents and decisions of a project out of current
// events.
func Records(events []connector.Event, project string) []Held {
	var out []Held
	for _, ev := range events {
		if ev.Payload.Container.NativeID != project {
			continue
		}
		if h, ok := Decode(ev); ok && h.Project == project {
			out = append(out, h)
		}
	}
	sortHeld(out)
	return out
}

func sortHeld(held []Held) {
	slices.SortFunc(held, func(a, b Held) int {
		return strings.Compare(a.Source+"\x00"+a.Artifact, b.Source+"\x00"+b.Artifact)
	})
}

// Resolve is the one resolution of a project-wide id: the held record of kind
// with that id among a project's records, wherever it is held. It is false when
// the id is not held, and an error wrapping [ErrAmbiguous] when it names more
// than one. Intake's duplicate refusal, its supersession and cycle checks and
// every read that follows a `supersedes` or spec-document link go through it, so
// they agree on the target.
func Resolve(records []Held, kind, id string) (Held, bool, error) {
	var found []Held
	for _, h := range records {
		if h.Kind == kind && h.ID == id {
			found = append(found, h)
		}
	}
	switch len(found) {
	case 0:
		return Held{}, false, nil
	case 1:
		return found[0], true, nil
	}
	where := make([]string, len(found))
	for i, h := range found {
		where[i] = h.Where()
	}
	return Held{}, false, fmt.Errorf("%s %q: %w: %s", kind, id, ErrAmbiguous, strings.Join(where, "; "))
}

// ClosesCycle reports whether decision id, naming supersedes, would close a
// cycle: following supersedes from supersedes through each held decision's
// latest revision comes back to id. The decision being posted or revised is
// not read from records, so a revision is judged by its new `supersedes`, not
// the one it replaces.
func ClosesCycle(records []Held, id, supersedes string) (bool, error) {
	seen := map[string]bool{}
	for cur := supersedes; cur != ""; {
		if cur == id {
			return true, nil
		}
		if seen[cur] {
			// A cycle that does not include id: not one this post closes.
			return false, nil
		}
		seen[cur] = true
		next, ok, err := Resolve(records, KindDecisionRecord, cur)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
		cur = next.Supersedes()
	}
	return false, nil
}
