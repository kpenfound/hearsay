package bundle

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l0"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/l3"
	"github.com/kpenfound/hearsay/internal/principal"
)

// The caps and the budget (docs/design.md#the-context-bundle).
const (
	// RecentCap is how many documents `recent` holds: a count, never an age.
	RecentCap = 5
	// AnchorCap is how many anchors a scope has. More than that is a sign the
	// scope is too broad (docs/design.md#anchors).
	AnchorCap = 4
	// QuestionDocuments is how many of a scope's newest documents that leave
	// something unanswered `open_questions` is read from.
	QuestionDocuments = 5
	// DefaultBudget is the bundle's budget in estimated tokens: the design's
	// roughly 1 to 2k.
	DefaultBudget = 2000
	// BytesPerToken is how the budget is estimated from the encoded bundle. It
	// is an estimate, and a deterministic one: counting with a model's
	// tokenizer would make the bundle depend on which model a deployment
	// named, and a bundle is identical whichever interface or model is behind it.
	BytesPerToken = 4
	// MaxLine is the most bytes one line of distillation takes.
	MaxLine = 200
)

// Handles are the calls a consumer follows a bundle's ids with.
var Handles = []string{"get_l1", "get_l0", "search", "stance_history", "resolve"}

// Bundle is the context bundle: pointers plus one line of distillation per
// pointer, every line carrying an L1 id the consumer can follow with `get_l1`.
//
// The field order is the design's. Read from the bottom it is the drop order
// but for `anchors`, which drop after `recent` and before any stance ([Build]).
// Every time is absolute: a relative age would make the same bundle differ from
// one minute to the next.
type Bundle struct {
	Directive     *Directive `json:"directive,omitempty"`
	Scope         Scope      `json:"scope"`
	Anchors       []Anchor   `json:"anchors"`
	Stances       []Stance   `json:"stances"`
	Recent        Recent     `json:"recent"`
	OpenQuestions []Question `json:"open_questions"`
	Conflicts     []Conflict `json:"conflicts"`
	Handles       []string   `json:"handles"`
}

// Directive is the current, readable L0 instruction that triggered a bundle.
type Directive struct {
	Text string `json:"text"`
	From string `json:"from"`
	Via  string `json:"via"`
	L0   string `json:"l0"`
	// References are extracted from the current event, and never served.
	References []l1.Reference `json:"-"`
}

// Conflict names a readable current position a directive bears on.
type Conflict struct {
	TopicID string `json:"topic_id"`
	Current string `json:"current"`
	Tier    string `json:"tier"`
	Stakes  string `json:"stakes"`
}

// Scope is what the bundle is for.
type Scope struct {
	// ID is the entity the bundle was asked for.
	ID       string   `json:"id"`
	Entities []Entity `json:"entities"`
}

// Entity is one entity the scope is about. Only an entity that is a document
// the reader may read — a tracker item's own issue — has a line, and it carries
// that document's id.
type Entity struct {
	ID     string   `json:"id"`
	Type   string   `json:"type,omitempty"`
	Name   string   `json:"name,omitempty"`
	Owners []string `json:"owners,omitempty"`
	L1     string   `json:"l1,omitempty"`
	Line   string   `json:"line,omitempty"`
}

// Anchor is a durable document that defines the scope: pinned, inferred as the
// most referenced, or defaulted from its `spec` documents, in that order.
type Anchor struct {
	L1   string `json:"l1"`
	Kind string `json:"kind"`
	Line string `json:"line"`
	// PinnedBy is the principal who pinned it, and absent where nobody did.
	PinnedBy string `json:"pinned_by,omitempty"`
}

// Stance is the current stance on one topic, and the tier it is computed at
// under the policy in force for the topic's scope.
type Stance struct {
	Topic string `json:"topic"`
	// TopicID is what `stance_history` takes.
	TopicID string `json:"topic_id"`
	Current string `json:"current"`
	Tier    string `json:"tier"`
	// Since is when the position was taken.
	Since      string `json:"since"`
	Supersedes string `json:"supersedes,omitempty"`
	// Evidence is the L1 documents the stance rests on.
	Evidence  []string `json:"evidence"`
	Inherited bool     `json:"inherited,omitempty"`
}

// Recent is the recent activity on the scope.
type Recent struct {
	LastActivity string `json:"last_activity,omitempty"`
	Items        []Item `json:"items"`
}

// Item is one recent document.
type Item struct {
	L1   string `json:"l1"`
	Kind string `json:"kind"`
	When string `json:"when"`
	Line string `json:"line"`
}

// Question is one open question.
type Question struct {
	Line     string   `json:"line"`
	Evidence []string `json:"evidence"`
}

// Report is what assembling a bundle left out, for the audit record: how much
// the reader was not allowed to see, how much was outside their reach, and how
// much the budget dropped. Counts only — which documents were withheld is what
// the reader may not learn.
type Report struct {
	Withheld Withheld `json:"withheld"`
	// Reach is what the reader's reach left out, apart from what the access
	// lists did: reach applies first, so nothing is counted twice.
	Reach   Reach   `json:"reach"`
	Trimmed Trimmed `json:"trimmed"`
	// Tokens is the estimated size of the bundle served.
	Tokens int `json:"tokens"`
}

// Withheld is what access control left out.
type Withheld struct {
	Documents int `json:"documents"`
	Stances   int `json:"stances"`
}

// Reach is what the reader's reach left out (docs/design.md#access-control).
type Reach struct {
	// Scope is set when the scope asked for is itself out of reach, and the
	// bundle is the one an unknown scope gets. Documents then counts every
	// document about it, whoever may read them.
	Scope     bool `json:"scope,omitempty"`
	Documents int  `json:"documents"`
	Stances   int  `json:"stances"`
	// Entities are the entities the scope's own document is about that are
	// out of reach, and so neither listed nor followed for inherited stances.
	Entities int `json:"entities"`
}

// Trimmed is what the budget dropped, per section.
type Trimmed struct {
	OpenQuestions int `json:"open_questions"`
	Recent        int `json:"recent"`
	Anchors       int `json:"anchors"`
	Stances       int `json:"stances"`
}

// Encode is the one encoding of a bundle every interface serves. Whatever asked,
// these are the bytes.
func Encode(b Bundle) ([]byte, error) {
	body, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("encoding the bundle for %s: %w", b.Scope.ID, err)
	}
	return body, nil
}

// Tokens is the estimated size of an encoded bundle.
func Tokens(encoded []byte) int { return (len(encoded) + BytesPerToken - 1) / BytesPerToken }

// Assembler builds bundles from the views over one database.
type Assembler struct {
	views    *l3.Views
	docs     *l1.Store
	events   *l0.Store
	resolver *principal.Resolver
	repo     config.Repo
	budget   int
}

// New returns an assembler over a pool or a transaction, with the default budget.
func New(q l3.Querier) *Assembler {
	return &Assembler{views: l3.New(q), docs: l1.New(q), events: l0.New(q), budget: DefaultBudget}
}

// WithDirectiveSources supplies the identity and scope mappings used by event directives.
func (a *Assembler) WithDirectiveSources(repo config.Repo, resolver *principal.Resolver) *Assembler {
	c := *a
	c.repo, c.resolver = repo, resolver
	return &c
}

// WithAuthority supplies the authority policies stance tiers are computed
// under. Without it they are computed under the built-in policy.
func (a *Assembler) WithAuthority(authority config.Authority) *Assembler {
	c := *a
	c.views = a.views.WithAuthority(authority)
	return &c
}

// WithBudget returns a copy of the assembler with another budget, in estimated
// tokens.
func (a *Assembler) WithBudget(tokens int) *Assembler {
	c := *a
	c.budget = tokens
	return &c
}

// Assemble builds the bundle for one entity and one reader. It is a structured
// lookup and calls no model. A scope outside the reader's reach gets the bundle
// an unknown scope gets rather than an error: scopes filter for relevance, and
// a refusal would say something about what is there. Inside it, the bundle
// holds only the entities, documents and stances in reach ([l1.Reader.InReach]),
// and the access lists filter what is left.
func (a *Assembler) Assemble(ctx context.Context, reader l1.Reader, scope string) (Bundle, Report, error) {
	return a.AssembleForEvent(ctx, reader, scope, "")
}

// AssembleForEvent adds a directive only from a current, readable event in the scope.
func (a *Assembler) AssembleForEvent(ctx context.Context, reader l1.Reader, scope, eventID string) (Bundle, Report, error) {
	in := Inputs{Scope: scope}
	var report Report
	if reader.Effective.Human != "" && !reader.InReach([]string{scope}) {
		report.Reach.Scope = true
		var err error
		if report.Reach.Documents, err = a.docs.About(ctx, scope); err != nil {
			return Bundle{}, Report{}, err
		}
	}
	if reader.InReach([]string{scope}) {
		var err error
		if in, report.Withheld, report.Reach, err = a.gather(ctx, reader, scope); err != nil {
			return Bundle{}, Report{}, err
		}
		if eventID != "" {
			if in.Directive, err = a.directive(ctx, reader, scope, eventID); err != nil {
				return Bundle{}, Report{}, err
			}
			if in.Directive != nil && slices.ContainsFunc(in.Directive.References, func(ref l1.Reference) bool { return ref.Type == l1.RefURL }) {
				in.EvidenceURLs = map[string]string{}
				for _, current := range in.Stances {
					for _, id := range current.Stance.Evidence {
						if _, seen := in.EvidenceURLs[id]; seen {
							continue
						}
						doc, err := a.docs.Get(ctx, id)
						if err != nil {
							return Bundle{}, Report{}, err
						}
						in.EvidenceURLs[id] = doc.Source.URL
					}
				}
			}
		}
	}
	b, trimmed, tokens, err := Build(in, a.budget)
	if err != nil {
		return Bundle{}, Report{}, err
	}
	report.Trimmed, report.Tokens = trimmed, tokens
	return b, report, nil
}

func (a *Assembler) gather(ctx context.Context, reader l1.Reader, scope string) (Inputs, Withheld, Reach, error) {
	in := Inputs{Scope: scope}
	var withheld Withheld
	var reach Reach
	direct := []string{scope}
	subject, ok, err := a.views.Subject(ctx, reader, scope)
	if err != nil {
		return in, withheld, reach, err
	}
	if ok {
		in.Subject = &subject
		for _, id := range subject.Scope {
			if slices.Contains(direct, id) {
				continue
			}
			// An entity out of reach is neither listed nor followed: its
			// topics are not the reader's to inherit.
			if !reader.InReach([]string{id}) {
				reach.Entities++
				continue
			}
			direct = append(direct, id)
		}
	}
	if in.Entities, err = a.views.Entities(ctx, direct); err != nil {
		return in, withheld, reach, err
	}
	in.Direct = direct
	if in.Stances, withheld.Stances, reach.Stances, err = a.views.CurrentStances(ctx, reader, scope, direct[1:]); err != nil {
		return in, withheld, reach, err
	}
	// Anchors before recent: an anchor is not activity, and is not repeated as
	// it, and does not cost `recent` one of its places.
	if in.Anchors, err = a.views.Anchors(ctx, reader, scope, AnchorCap); err != nil {
		return in, withheld, reach, err
	}
	anchors := make([]string, len(in.Anchors))
	for i, anchor := range in.Anchors {
		anchors[i] = anchor.Doc.ID
	}
	if in.Recent, err = a.views.Recent(ctx, reader, scope, RecentCap, anchors); err != nil {
		return in, withheld, reach, err
	}
	if in.Questions, err = a.views.OpenQuestions(ctx, reader, scope, QuestionDocuments); err != nil {
		return in, withheld, reach, err
	}
	if withheld.Documents, err = a.docs.Withheld(ctx, reader, scope); err != nil {
		return in, withheld, reach, err
	}
	return in, withheld, reach, nil
}

// directive reads the requested event only to identify its artifact, then reads
// the current revision. A tombstone or an ACL change therefore takes effect on
// every assembly, including a repeated request for an old revision's id.
func (a *Assembler) directive(ctx context.Context, reader l1.Reader, scope, id string) (*Directive, error) {
	if a.resolver == nil {
		return nil, nil
	}
	requested, err := a.events.Get(ctx, id)
	if errors.Is(err, l0.ErrNotFound) || errors.Is(err, l0.ErrRetracted) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	current, err := a.events.Current(ctx, l0.ListOptions{Filter: l0.Filter{Source: requested.Source, Artifact: requested.Payload.Artifact}, Limit: 1})
	if err != nil {
		return nil, err
	}
	if len(current) == 0 {
		return nil, nil
	}
	ev := current[0]
	if ev.Kind != connector.KindMessage || !reader.Allows(ev.ACL) || ev.Payload.Text == "" {
		return nil, nil
	}
	// The conversation's document is the authoritative scope mapping once it
	// exists. Before distillation, a configured covering scope is enough.
	conversation := ev.Payload.Thread
	if conversation == "" {
		conversation = ev.Payload.Parent
	}
	if conversation == "" {
		conversation = ev.Payload.Artifact
	}
	doc, err := a.docs.Get(ctx, l1.DocID(ev.Source, conversation))
	scoped := err == nil && slices.Contains(doc.Scope, scope)
	if errors.Is(err, l1.ErrNotFound) {
		for _, configured := range a.repo.Scopes {
			if configured.Covers(ev.Source, ev.Payload.Container.NativeID) && slices.Contains(configured.Entities, scope) {
				scoped = true
			}
		}
	} else if err != nil {
		return nil, err
	}
	if !scoped {
		return nil, nil
	}
	addressed := false
	for _, hint := range ev.Payload.Mentions {
		resolved := a.resolver.Resolve(hint)
		if resolved.Status == principal.Resolved && resolved.Principal.Kind == principal.KindAgent && resolved.Principal.Class.Valid() {
			addressed = true
			break
		}
	}
	if !addressed {
		return nil, nil
	}
	from := ""
	if ev.Payload.Author != nil {
		resolved := a.resolver.Resolve(*ev.Payload.Author)
		if resolved.Status == principal.Resolved {
			from = resolved.Principal.ID
		}
	}
	via := ev.Source + ":" + ev.Payload.Container.NativeID
	if ev.Payload.Thread != "" {
		via += ":" + ev.Payload.Thread
	}
	return &Directive{Text: ev.Payload.Text, From: from, Via: via, L0: ev.ID,
		References: l1.References([]connector.Event{ev}, a.resolver, a.repo.Code)}, nil
}

// Inputs are what a bundle is built from, already filtered for its reader.
type Inputs struct {
	Scope     string
	Directive *Directive
	// Direct are the entity ids the scope is about: the scope, then the
	// entities its own document is about.
	Direct []string
	// Entities are the ones of Direct the graph holds.
	Entities []l2.Entity
	// Subject is the document the scope is, where there is one.
	Subject *l1.Stored
	Anchors []l3.Anchor
	Stances []l3.CurrentStance
	// EvidenceURLs are the source permalinks of readable current evidence.
	EvidenceURLs map[string]string
	Recent       l3.Activity
	Questions    []l3.Question
}

// Build lays inputs out as a bundle and holds it to a budget, dropping from the
// bottom: open questions first, then `recent`, then anchors, then inherited
// stances whatever their tier, then the scope's own stances that are not
// ratified, each last first. The scope's own ratified stances never drop, so a
// bundle can end up over budget, and the scope and the handles are what the
// rest points into.
//
// Anchors sit above stances in the layout but drop before them: an anchor is a
// document an agent can still find with `search`, and a stance is the position
// the team has taken, which it cannot recover as cheaply. They drop after
// `recent` because the design document is what the activity is about.
//
// Inherited stances drop whatever their tier because they are not the scope's:
// a tracker item inherits every topic in its repository, and a repository with
// many merged pull requests holds many ratified ones. Holding those would make
// the budget a limit only on a quiet repository.
// It is a pure function of its inputs, which is what makes a bundle cacheable.
func Build(in Inputs, budget int) (Bundle, Trimmed, int, error) {
	b := Bundle{
		Directive:     in.Directive,
		Scope:         Scope{ID: in.Scope, Entities: []Entity{}},
		Anchors:       []Anchor{},
		Stances:       []Stance{},
		Recent:        Recent{Items: []Item{}},
		OpenQuestions: []Question{},
		Conflicts:     []Conflict{},
		Handles:       slices.Clone(Handles),
	}
	known := map[string]l2.Entity{}
	for _, e := range in.Entities {
		known[e.ID] = e
	}
	for _, id := range in.Direct {
		entity := Entity{ID: id}
		if e, ok := known[id]; ok {
			entity.Type, entity.Name, entity.Owners = string(e.Type), e.Name, slices.Clone(e.Owners)
		}
		if id == in.Scope && in.Subject != nil {
			entity.L1, entity.Line = in.Subject.ID, Line(in.Subject.Body.Summary)
		}
		b.Scope.Entities = append(b.Scope.Entities, entity)
	}
	for _, a := range in.Anchors {
		b.Anchors = append(b.Anchors, Anchor{
			L1: a.Doc.ID, Kind: string(a.Doc.Kind), Line: Line(a.Doc.Body.Summary), PinnedBy: a.PinnedBy,
		})
	}
	for _, c := range in.Stances {
		b.Stances = append(b.Stances, Stance{
			Topic:      Line(c.Topic.Name),
			TopicID:    c.Topic.ID,
			Current:    Line(c.Stance.Position),
			Tier:       string(c.Tier),
			Since:      stamp(c.Stance.StatedAt),
			Supersedes: Line(c.Supersedes),
			Evidence:   slices.Clone(c.Stance.Evidence),
			Inherited:  c.Inherited,
		})
	}
	b.Conflicts = conflicts(in.Directive, in.Stances, in.EvidenceURLs)
	if !in.Recent.LastActivity.IsZero() {
		b.Recent.LastActivity = stamp(in.Recent.LastActivity)
	}
	for _, doc := range in.Recent.Items {
		b.Recent.Items = append(b.Recent.Items, Item{
			L1: doc.ID, Kind: string(doc.Kind), When: stamp(doc.Time.LastActivity), Line: Line(doc.Body.Summary),
		})
	}
	for _, q := range in.Questions {
		b.OpenQuestions = append(b.OpenQuestions, Question{Line: Line(q.Text), Evidence: slices.Clone(q.Evidence)})
	}

	return trim(b, budget)
}

func conflicts(d *Directive, stances []l3.CurrentStance, evidenceURLs map[string]string) []Conflict {
	out := []Conflict{}
	if d == nil {
		return out
	}
	for _, c := range stances {
		if !bearsOn(d, c, evidenceURLs) {
			continue
		}
		stakes := "not yet ratified"
		if c.Tier == l2.TierRatified {
			stakes = "departing from it needs ratification"
		}
		out = append(out, Conflict{TopicID: c.Topic.ID, Current: Line(c.Stance.Position), Tier: string(c.Tier), Stakes: stakes})
	}
	priority := func(t string) int {
		switch l2.Tier(t) {
		case l2.TierRatified:
			return 0
		case l2.TierContested:
			return 1
		default:
			return 2
		}
	}
	slices.SortFunc(out, func(a, b Conflict) int {
		if n := cmp.Compare(priority(a.Tier), priority(b.Tier)); n != 0 {
			return n
		}
		return cmp.Compare(a.TopicID, b.TopicID)
	})
	if len(out) > 3 {
		out = out[:3]
	}
	return out
}

func bearsOn(d *Directive, c l3.CurrentStance, evidenceURLs map[string]string) bool {
	for _, ref := range d.References {
		if ref.Type == l1.RefSystem && slices.Contains(c.Topic.About, ref.ID) {
			return true
		}
		if ref.Type != l1.RefIssue && ref.Type != l1.RefPR && ref.Type != l1.RefTrackerItem && ref.Type != l1.RefURL {
			continue
		}
		if slices.Contains(c.Topic.About, ref.ID) || slices.Contains(c.Stance.Evidence, ref.ID) {
			return true
		}
		// Tracker references name a source artifact; L1 evidence and L2
		// tracker entities prefix it with their layer and source.
		for _, evidence := range c.Stance.Evidence {
			if ref.Type == l1.RefURL && evidenceURLs[evidence] == ref.ID {
				return true
			}
			if strings.HasSuffix(evidence, ":"+ref.ID) {
				return true
			}
		}
		for _, entity := range c.Topic.About {
			if strings.HasSuffix(entity, ":"+ref.ID) {
				return true
			}
		}
	}
	return textMatch(d.Text, c.Topic.Name) || textMatch(d.Text, c.Stance.Position)
}

func textMatch(directive, phrase string) bool {
	phrase = strings.Join(strings.Fields(strings.ToLower(phrase)), " ")
	directive = strings.Join(strings.Fields(strings.ToLower(directive)), " ")
	return phrase != "" && strings.Contains(directive, phrase)
}

// trim holds a laid-out bundle to its budget. The drop order is Build's, one
// element at a time, and the size after each drop is exactly what encoding the
// smaller bundle would give: an array element's encoding does not depend on its
// neighbours, so removing one takes its own bytes and, when others remain, one
// comma. That is what keeps a repository with thousands of inherited stances
// linear — the bundle is encoded once before trimming and once after, never per
// step.
func trim(b Bundle, budget int) (Bundle, Trimmed, int, error) {
	encoded, err := Encode(b)
	if err != nil {
		return Bundle{}, Trimmed{}, 0, err
	}
	size := len(encoded)
	fits := func() bool { return (size+BytesPerToken-1)/BytesPerToken <= budget }
	var trimmed Trimmed

	// drop takes the last kept element of a section off, given its size and
	// how many are kept.
	drop := func(elem int, kept *int) {
		size -= elem
		if *kept > 1 {
			size--
		}
		*kept--
	}
	sizes := func(n int, at func(int) any) ([]int, error) {
		out := make([]int, n)
		for i := range n {
			body, err := json.Marshal(at(i))
			if err != nil {
				return nil, fmt.Errorf("encoding the bundle for %s: %w", b.Scope.ID, err)
			}
			out[i] = len(body)
		}
		return out, nil
	}

	questions, err := sizes(len(b.OpenQuestions), func(i int) any { return b.OpenQuestions[i] })
	if err != nil {
		return Bundle{}, Trimmed{}, 0, err
	}
	keptQuestions := len(b.OpenQuestions)
	for keptQuestions > 0 && !fits() {
		drop(questions[keptQuestions-1], &keptQuestions)
		trimmed.OpenQuestions++
	}

	items, err := sizes(len(b.Recent.Items), func(i int) any { return b.Recent.Items[i] })
	if err != nil {
		return Bundle{}, Trimmed{}, 0, err
	}
	keptItems := len(b.Recent.Items)
	for keptItems > 0 && !fits() {
		drop(items[keptItems-1], &keptItems)
		trimmed.Recent++
	}

	anchors, err := sizes(len(b.Anchors), func(i int) any { return b.Anchors[i] })
	if err != nil {
		return Bundle{}, Trimmed{}, 0, err
	}
	keptAnchors := len(b.Anchors)
	for keptAnchors > 0 && !fits() {
		drop(anchors[keptAnchors-1], &keptAnchors)
		trimmed.Anchors++
	}

	stances, err := sizes(len(b.Stances), func(i int) any { return b.Stances[i] })
	if err != nil {
		return Bundle{}, Trimmed{}, 0, err
	}
	// Inherited stances from the last back, then the scope's own stances that
	// are not ratified from the last back — the order removing "the last
	// inherited one" and then "the last non-ratified one" one at a time takes.
	var order []int
	for i := len(b.Stances) - 1; i >= 0; i-- {
		if b.Stances[i].Inherited {
			order = append(order, i)
		}
	}
	for i := len(b.Stances) - 1; i >= 0; i-- {
		if !b.Stances[i].Inherited && b.Stances[i].Tier != string(l2.TierRatified) {
			order = append(order, i)
		}
	}
	removed := make([]bool, len(b.Stances))
	keptStances := len(b.Stances)
	for _, i := range order {
		if fits() {
			break
		}
		drop(stances[i], &keptStances)
		removed[i] = true
		trimmed.Stances++
	}

	b.OpenQuestions = b.OpenQuestions[:keptQuestions]
	b.Recent.Items = b.Recent.Items[:keptItems]
	b.Anchors = b.Anchors[:keptAnchors]
	kept := make([]Stance, 0, keptStances)
	for i, s := range b.Stances {
		if !removed[i] {
			kept = append(kept, s)
		}
	}
	b.Stances = kept
	// The reported size is the encoding's own, not the count: the count decides
	// what drops, and TestTrimMatchesReEncodingAfterEveryDrop holds the two equal.
	if encoded, err = Encode(b); err != nil {
		return Bundle{}, Trimmed{}, 0, err
	}
	return b, trimmed, Tokens(encoded), nil
}

// Line is one line of distillation: the first non-empty line of a text, its
// whitespace collapsed, cut to [MaxLine] bytes on a character boundary.
func Line(text string) string {
	for l := range strings.Lines(text) {
		l = strings.Join(strings.Fields(l), " ")
		if l == "" {
			continue
		}
		if len(l) <= MaxLine {
			return l
		}
		cut := MaxLine - len("…")
		for cut > 0 && !utf8.RuneStart(l[cut]) {
			cut--
		}
		return l[:cut] + "…"
	}
	return ""
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }
