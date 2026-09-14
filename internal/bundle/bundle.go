package bundle

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/l3"
)

// The caps and the budget (docs/design.md#the-context-bundle).
const (
	// RecentCap is how many documents `recent` holds: a count, never an age.
	RecentCap = 5
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
// The field order is the design's, and it is the drop order read from the
// bottom. Every time is absolute: a relative age would make the same bundle
// differ from one minute to the next.
type Bundle struct {
	Scope         Scope      `json:"scope"`
	Anchors       []Anchor   `json:"anchors"`
	Stances       []Stance   `json:"stances"`
	Recent        Recent     `json:"recent"`
	OpenQuestions []Question `json:"open_questions"`
	Conflicts     []string   `json:"conflicts"`
	Handles       []string   `json:"handles"`
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

// Anchor is a durable artifact that defines the scope. Nothing produces one in
// this build: anchors are later work, and the section is here, empty, so a
// consumer reads the design's shape.
type Anchor struct {
	L1   string `json:"l1"`
	Kind string `json:"kind"`
	Line string `json:"line"`
}

// Stance is the current stance on one topic.
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
// the reader was not allowed to see, and how much the budget dropped. Counts
// only — which documents were withheld is what the reader may not learn.
type Report struct {
	Withheld Withheld `json:"withheld"`
	Trimmed  Trimmed  `json:"trimmed"`
	// Tokens is the estimated size of the bundle served.
	Tokens int `json:"tokens"`
}

// Withheld is what access control left out.
type Withheld struct {
	Documents int `json:"documents"`
	Stances   int `json:"stances"`
}

// Trimmed is what the budget dropped, per section.
type Trimmed struct {
	OpenQuestions int `json:"open_questions"`
	Recent        int `json:"recent"`
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
	views  *l3.Views
	docs   *l1.Store
	budget int
}

// New returns an assembler over a pool or a transaction, with the default budget.
func New(q l3.Querier) *Assembler {
	return &Assembler{views: l3.New(q), docs: l1.New(q), budget: DefaultBudget}
}

// WithBudget returns a copy of the assembler with another budget, in estimated
// tokens.
func (a *Assembler) WithBudget(tokens int) *Assembler {
	c := *a
	c.budget = tokens
	return &c
}

// Assemble builds the bundle for one entity and one reader. It is a structured
// lookup and calls no model. A scope the reader was not granted gets a bundle
// with nothing in it rather than an error: scopes filter for relevance, and a
// refusal would say something about what is there.
func (a *Assembler) Assemble(ctx context.Context, reader l1.Reader, scope string) (Bundle, Report, error) {
	in := Inputs{Scope: scope}
	var report Report
	if reader.Effective.Human != "" && reader.Effective.Grant.Scopes.Has(scope) {
		var err error
		if in, report.Withheld, err = a.gather(ctx, reader, scope); err != nil {
			return Bundle{}, Report{}, err
		}
	}
	b, trimmed, tokens, err := Build(in, a.budget)
	if err != nil {
		return Bundle{}, Report{}, err
	}
	report.Trimmed, report.Tokens = trimmed, tokens
	return b, report, nil
}

func (a *Assembler) gather(ctx context.Context, reader l1.Reader, scope string) (Inputs, Withheld, error) {
	in := Inputs{Scope: scope}
	var withheld Withheld
	direct := []string{scope}
	subject, ok, err := a.views.Subject(ctx, reader, scope)
	if err != nil {
		return in, withheld, err
	}
	if ok {
		in.Subject = &subject
		for _, id := range subject.Scope {
			if !slices.Contains(direct, id) {
				direct = append(direct, id)
			}
		}
	}
	if in.Entities, err = a.views.Entities(ctx, direct); err != nil {
		return in, withheld, err
	}
	in.Direct = direct
	if in.Stances, withheld.Stances, err = a.views.CurrentStances(ctx, reader, scope, direct[1:]); err != nil {
		return in, withheld, err
	}
	if in.Recent, err = a.views.Recent(ctx, reader, scope, RecentCap); err != nil {
		return in, withheld, err
	}
	if in.Questions, err = a.views.OpenQuestions(ctx, reader, scope, QuestionDocuments); err != nil {
		return in, withheld, err
	}
	if withheld.Documents, err = a.docs.Withheld(ctx, reader, scope); err != nil {
		return in, withheld, err
	}
	return in, withheld, nil
}

// Inputs are what a bundle is built from, already filtered for its reader.
type Inputs struct {
	Scope string
	// Direct are the entity ids the scope is about: the scope, then the
	// entities its own document is about.
	Direct []string
	// Entities are the ones of Direct the graph holds.
	Entities []l2.Entity
	// Subject is the document the scope is, where there is one.
	Subject   *l1.Stored
	Stances   []l3.CurrentStance
	Recent    l3.Activity
	Questions []l3.Question
}

// Build lays inputs out as a bundle and holds it to a budget, dropping from the
// bottom: open questions first, then `recent`, then inherited stances whatever
// their tier, then the scope's own stances that are not ratified, each last
// first. The scope's own ratified stances never drop, so a bundle can end up
// over budget, and the scope and the handles are what the rest points into.
//
// Inherited stances drop whatever their tier because they are not the scope's:
// a tracker item inherits every topic in its repository, and a repository with
// many merged pull requests holds many ratified ones. Holding those would make
// the budget a limit only on a quiet repository.
// It is a pure function of its inputs, which is what makes a bundle cacheable.
func Build(in Inputs, budget int) (Bundle, Trimmed, int, error) {
	b := Bundle{
		Scope:         Scope{ID: in.Scope, Entities: []Entity{}},
		Anchors:       []Anchor{},
		Stances:       []Stance{},
		Recent:        Recent{Items: []Item{}},
		OpenQuestions: []Question{},
		Conflicts:     []string{},
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
	for _, c := range in.Stances {
		b.Stances = append(b.Stances, Stance{
			Topic:      Line(c.Topic.Name),
			TopicID:    c.Topic.ID,
			Current:    Line(c.Stance.Position),
			Tier:       string(c.Stance.Tier),
			Since:      stamp(c.Stance.StatedAt),
			Supersedes: Line(c.Supersedes),
			Evidence:   slices.Clone(c.Stance.Evidence),
			Inherited:  c.Inherited,
		})
	}
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

	var trimmed Trimmed
	for {
		encoded, err := Encode(b)
		if err != nil {
			return Bundle{}, Trimmed{}, 0, err
		}
		tokens := Tokens(encoded)
		if tokens <= budget {
			return b, trimmed, tokens, nil
		}
		switch {
		case len(b.OpenQuestions) > 0:
			b.OpenQuestions = b.OpenQuestions[:len(b.OpenQuestions)-1]
			trimmed.OpenQuestions++
		case len(b.Recent.Items) > 0:
			b.Recent.Items = b.Recent.Items[:len(b.Recent.Items)-1]
			trimmed.Recent++
		default:
			last := slices.IndexFunc(backward(b.Stances), func(s Stance) bool { return s.Inherited })
			if last < 0 {
				last = slices.IndexFunc(backward(b.Stances), func(s Stance) bool { return s.Tier != string(l2.TierRatified) })
			}
			if last >= 0 {
				last = len(b.Stances) - 1 - last
			}
			if last < 0 {
				return b, trimmed, tokens, nil
			}
			b.Stances = slices.Delete(b.Stances, last, last+1)
			trimmed.Stances++
		}
	}
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

// backward is a reversed copy, so that an index search finds the last match.
func backward(s []Stance) []Stance {
	out := slices.Clone(s)
	slices.Reverse(out)
	return out
}
