package l1

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/principal"
)

// Input is the events one document is built from, and what is needed to read
// them: the current revision of the artifact, the current revision of every
// comment, review and reply on it, the identity mapping and the configuration
// repository.
//
// Only current revisions belong here. An artifact's earlier revisions are in L0
// and are what a provenance walk reads; a document is what the artifact says
// now, so building one from a superseded revision would distil something that
// is no longer true.
type Input struct {
	// Root is the artifact the document is about.
	Root connector.Event
	// Children are the events that hang off it — the comments on an issue, the
	// reviews and review comments on a pull request. Order does not matter:
	// [Build] puts them in the document's own order. Commands and Hearsay's
	// replies to them are left out of the document.
	Children []connector.Event
	// Resolver maps the identity hints the events carry to principals. A nil
	// resolver resolves nothing, which is what a process with no `principals/`
	// configuration has.
	Resolver *principal.Resolver
	// Repo is the configuration repository, for the scopes that cover the
	// artifact and the code entities its text may name.
	Repo config.Repo
}

// Build makes the deterministic half of a document: everything but the body.
// Nothing here calls a model, so it costs nothing to recompute — which is what
// makes the distiller stateless and what lets reference extraction be a join
// key rather than a stored opinion.
//
// The body arrives separately, through [Document.WithBody], because it is the
// one part that takes a model call.
//
// It returns [ErrNotDistilled] for an artifact whose kind makes no document of
// its own — a comment is part of the issue's document, not a document.
func Build(in Input) (Document, error) {
	if err := in.Root.Validate(); err != nil {
		return Document{}, fmt.Errorf("the root event: %w", err)
	}
	kind, ok := KindFor(in.Root)
	if !ok {
		return Document{}, fmt.Errorf("%w: %s is not an artifact this build distils", ErrNotDistilled, in.Root.Kind)
	}
	return build(in, kind, in.Root.Payload.Artifact, false)
}

// BuildChatWindow builds a channel conversation from its current message
// revisions. The key is a fixed source-time bucket, independent of which
// message arrived first or remains after a deletion.
func BuildChatWindow(key string, messages []connector.Event, resolver *principal.Resolver, repo config.Repo) (Document, error) {
	if len(messages) == 0 {
		return Document{}, ErrNotDistilled
	}
	ordered := slices.Clone(messages)
	slices.SortFunc(ordered, byConversationOrder)
	for _, ev := range ordered {
		if (ev.Kind != connector.KindMessage && ev.Payload.BaseKind != connector.KindMessage) || ev.Payload.Container.Kind != connector.ContainerChannel || ev.Payload.Thread != "" || ChatWindowKey(ev) != key {
			return Document{}, fmt.Errorf("message %s does not belong to chat window %s", ev.NativeID, key)
		}
	}
	return build(Input{Root: ordered[0], Children: ordered[1:], Resolver: resolver, Repo: repo}, KindChatThread, key, true)
}

func build(in Input, kind Kind, artifact string, window bool) (Document, error) {
	if err := in.Root.Validate(); err != nil {
		return Document{}, fmt.Errorf("the root event: %w", err)
	}

	root := in.Root
	class, err := ClassFor(kind, root)
	if err != nil {
		return Document{}, err
	}
	children := slices.DeleteFunc(slices.Clone(in.Children), control)
	for i, child := range children {
		if child.Source != root.Source {
			return Document{}, fmt.Errorf("child %s is from source %q and the artifact is from %q", child.NativeID, child.Source, root.Source)
		}
		if !window && conversationOf(child) != artifact {
			return Document{}, fmt.Errorf("child %s hangs off %q, not off %q", child.NativeID, conversationOf(child), artifact)
		}
		if err := child.Validate(); err != nil {
			return Document{}, fmt.Errorf("child %s: %w", child.NativeID, err)
		}
		children[i] = child
	}
	slices.SortFunc(children, byConversationOrder)

	acl, kept := aclOf(root, children)
	events := append([]connector.Event{root}, kept...)

	refs := make([]string, len(events))
	for i, ev := range events {
		refs[i] = connector.EventID(ev.Source, ev.NativeID)
	}

	doc := Document{
		ID:            DocID(root.Source, artifact),
		Kind:          kind,
		ArtifactClass: class,
		State:         stateOf(root),
		Source: Source{
			System:   root.Source,
			NativeID: artifact,
			URL:      root.Payload.URL,
		},
		L0Refs:       refs,
		Time:         timesOf(root, kept),
		Participants: participantsOf(root, kept, in.Resolver),
		References:   References(events, in.Resolver, in.Repo.Code),
		ACL:          acl,
	}
	doc.Scope = scopeOf(root, in.Repo, doc.References)
	doc.RawText, _ = Scrub(renderRaw(root, kept, in.Resolver))
	doc.RawText = trimLines(doc.RawText)
	if doc.RawText == "" {
		return Document{}, fmt.Errorf("%w: %s has no text left after the scrub", ErrNotDistilled, doc.ID)
	}
	return doc, nil
}

// WithBody returns the document with the body a model produced: scrubbed,
// rendered into the text that is embedded, and validated. It is a value method
// because a document is a value — the caller keeps whichever of the two it
// wants — and it reports the shapes the scrub took out of the body, which is
// the only thing about them that may be logged.
func (d Document) WithBody(b Body) (Document, []string, error) {
	b.Summary = trimLines(b.Summary)
	b.Question = trimLines(b.Question)
	b.Outcome = trimLines(b.Outcome)
	b.Change = trimLines(b.Change)
	questions := make([]string, 0, len(b.OpenQuestions))
	for _, q := range b.OpenQuestions {
		if q = trimLines(q); q != "" {
			questions = append(questions, q)
		}
	}
	b.OpenQuestions = questions

	body, redacted := scrubBody(b)
	d.Body = body
	d.Text = trimLines(renderText(d))
	if d.Text == "" {
		return Document{}, nil, fmt.Errorf("%w: the distilled body of %s is empty", ErrInvalidDocument, d.ID)
	}
	if err := d.Validate(); err != nil {
		return Document{}, nil, err
	}
	return d, redacted, nil
}

func conversationOf(ev connector.Event) string {
	return cmp.Or(ev.Payload.Thread, ev.Payload.Parent)
}

func control(ev connector.Event) bool {
	return ev.Kind == connector.KindCommand || ev.Payload.BaseKind == connector.KindCommand
}

func byConversationOrder(a, b connector.Event) int {
	if c := a.Time.Compare(b.Time); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Payload.Artifact, b.Payload.Artifact); c != 0 {
		return c
	}
	return cmp.Compare(a.NativeID, b.NativeID)
}

func aclOf(root connector.Event, children []connector.Event) (connector.ACL, []connector.Event) {
	acl := slices.Clone(root.ACL)
	kept := make([]connector.Event, 0, len(children))
	for _, child := range children {
		folded := intersectACL(acl, child.ACL)
		if len(folded) == 0 {
			continue
		}
		acl = folded
		kept = append(kept, child)
	}
	return acl, kept
}

func intersectACL(a, b connector.ACL) connector.ACL {
	out := make(connector.ACL, 0, len(a))
	for _, entry := range a {
		if slices.ContainsFunc(b, func(other connector.ACLEntry) bool { return sameGrant(entry, other) }) {
			out = append(out, entry)
		}
	}
	return out
}

func sameGrant(a, b connector.ACLEntry) bool {
	return a.Kind == b.Kind && a.Source == b.Source && a.NativeID == b.NativeID
}

func timesOf(root connector.Event, children []connector.Event) Times {
	t := Times{Created: root.Time.UTC(), Updated: root.Time.UTC(), LastActivity: root.Time.UTC()}
	if rev := root.Payload.Revision; rev != nil && !rev.EditedAt.IsZero() {
		t.Updated = rev.EditedAt.UTC()
	}
	t.LastActivity = latest(t.LastActivity, t.Updated)
	for _, child := range children {
		t.LastActivity = latest(t.LastActivity, child.Time.UTC())
		if rev := child.Payload.Revision; rev != nil && !rev.EditedAt.IsZero() {
			t.LastActivity = latest(t.LastActivity, rev.EditedAt.UTC())
		}
	}
	return t
}

func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

var rolePrecedence = []connector.ParticipantRole{
	connector.RoleAuthor, connector.RoleReviewer, connector.RoleAgent, connector.RoleAttendee,
}

func participantsOf(root connector.Event, children []connector.Event, resolver *principal.Resolver) []Participant {
	roles := map[string]connector.ParticipantRole{}
	take := func(hint *connector.Identity, role connector.ParticipantRole) {
		if hint == nil {
			return
		}
		id := resolvePerson(resolver, *hint)
		if id == "" {
			return
		}
		if held, ok := roles[id]; !ok || slices.Index(rolePrecedence, role) < slices.Index(rolePrecedence, held) {
			roles[id] = role
		}
	}

	take(root.Payload.Author, connector.RoleAuthor)
	for _, p := range root.Payload.Participants {
		take(&p.Identity, p.Role)
	}
	for _, child := range children {
		take(child.Payload.Author, roleFor(child))
		for _, p := range child.Payload.Participants {
			take(&p.Identity, p.Role)
		}
	}

	out := make([]Participant, 0, len(roles))
	for id, role := range roles {
		out = append(out, Participant{PrincipalID: id, Role: role})
	}
	slices.SortFunc(out, func(a, b Participant) int {
		if c := cmp.Compare(slices.Index(rolePrecedence, a.Role), slices.Index(rolePrecedence, b.Role)); c != 0 {
			return c
		}
		return cmp.Compare(a.PrincipalID, b.PrincipalID)
	})
	return out
}

func roleFor(ev connector.Event) connector.ParticipantRole {
	kind := ev.Kind
	if !kind.IsCore() && ev.Payload.BaseKind != "" {
		kind = ev.Payload.BaseKind
	}
	switch {
	case kind == connector.KindReview || kind == connector.KindReviewComment:
		return connector.RoleReviewer
	case ev.Payload.Author != nil && ev.Payload.Author.Kind == connector.IdentityAgent:
		return connector.RoleAgent
	default:
		return connector.RoleAuthor
	}
}

func scopeOf(root connector.Event, repo config.Repo, refs []Reference) []string {
	entities := map[string]bool{}
	for _, id := range workScope(root, repo) {
		entities[id] = true
	}
	for _, ref := range refs {
		if ref.Type == RefSystem {
			entities[ref.ID] = true
		}
	}
	for _, scope := range repo.Scopes {
		if !scope.Covers(root.Source, root.Payload.Container.NativeID) {
			continue
		}
		for _, id := range scope.Entities {
			entities[id] = true
		}
		if item, ok := trackerItemOf(root, scope); ok {
			entities[item] = true
		}
	}
	out := make([]string, 0, len(entities))
	for id := range entities {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

func trackerItemOf(root connector.Event, scope config.Scope) (string, bool) {
	kind, ok := KindFor(root)
	if !ok || (kind != KindIssue && kind != KindPR) {
		return "", false
	}
	if scope.Tracker.Source != root.Source || scope.Tracker.Project != root.Payload.Container.NativeID {
		return "", false
	}
	item := root.Payload.Artifact
	if at := strings.LastIndex(item, "#"); at >= 0 {
		item = item[at+1:]
	}
	return scope.TrackerItemID(item)
}

func stateOf(root connector.Event) *State {
	if root.Payload.State == nil {
		return nil
	}
	state := *root.Payload.State
	if state.Native != nil {
		native := *state.Native
		state.Native = &native
	}
	return &state
}
