package distiller

import (
	"encoding/json"
	"fmt"

	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/llm"
)

// The prompts belong to this package, not to internal/l1: what a document is,
// and what may be stored in one, is the layer's; how a model is asked to
// produce one is the distiller's (ADR-0005). A second consumer of L1 does not
// inherit these words.
//
// Both halves of the ask are here — the system prompt and the schema the answer
// has to satisfy — because they say the same thing twice and drift apart if
// they are written in two places. The schema is enforced by internal/llm before
// an answer is returned, so what the prompt asks for and what is accepted
// cannot differ.

// MaxPromptBytes bounds how much of a document is sent to the model. A
// conversation longer than this is truncated on a rune boundary with a marker,
// deterministically, so that the same artifact always produces the same
// request — a prompt that is cut differently on each run would miss its
// recorded fixture and, worse, distil differently each time.
//
// It is bytes rather than tokens because tokens are the provider's unit and
// this abstraction does not model them. Roughly 30,000 tokens of English, which
// is well inside the context window of any model a `distill` tier would name
// and far beyond any pull request a person would read.
const MaxPromptBytes = 120_000

// truncated is what the marker says. It is in the document the model reads, so
// it is written as something a reader would understand rather than as a code.
const truncated = "\n\n[the rest of this conversation was left out: it is longer than the distiller sends]"

// systemPrompt is what the distill tier is told it is doing. There is one per
// document kind: a commit and a pull request want different questions asked,
// and a single prompt with three branches in it is a prompt that does none of
// them well.
func systemPrompt(kind l1.Kind) string {
	return preamble + "\n\n" + kindPrompts[kind] + "\n\n" + outcomeRules
}

const preamble = `You are Hearsay's distiller. You turn one artifact from a team's tools into one short document that a coding agent will read later, when the artifact itself is long gone from anybody's memory.

Write for someone who has to make a decision and has not read any of this. Say what the team worked out and why, not what the software does. Keep every claim to what the document in front of you says: if something is not there, leave the field empty rather than filling it in from what usually happens.

Never quote a credential, a token, a key or a personal email address, even if the document contains one.`

// kindPrompts is what each kind of artifact is for, and what is worth keeping
// from it.
var kindPrompts = map[l1.Kind]string{
	l1.KindIssue: `This is a tracker issue and the conversation on it.

- summary: what is being asked for or reported, and where the conversation got to.
- question: the question the issue is really asking, in one sentence. Empty if it reports rather than asks.
- outcome: what was concluded, if anything was.
- open_questions: what the thread leaves unanswered, one per entry, each a question.`,

	l1.KindPR: `This is a change proposal with its reviews and its comments.

- summary: what is being proposed and what the review said about it.
- change: what the change does, in one or two sentences. Behaviour, not files.
- outcome: what was decided about it — merged, rejected, reworked, still open.
- open_questions: what the review raised and nobody answered, one per entry.`,

	l1.KindCommit: `This is one commit on a watched branch.

- summary: what changed and why, from the message. Say "no reason given" rather than inventing one.
- change: what the commit does, in one sentence.
- outcome: what it settles, if the message says it settles anything.
- open_questions: usually empty. A commit message rarely leaves a question.`,
}

// outcomeRules is the classification, which is the same for every kind because
// it is the L2 trigger: only decided, proposed and resolved enter the assertion
// pipeline (docs/design.md#l1-distilled-documents), so a document classified
// generously puts noise into the graph and one classified meanly loses a
// decision.
const outcomeRules = `outcome_kind classifies the outcome, and it is always one of five:

- resolved: a question was answered or the work was finished. A merged change, a bug someone fixed.
- decided: the team took a decision that binds later work, whether or not it has been carried out.
- proposed: something was put forward and nobody has agreed to it yet.
- open: it is still being worked out, and the artifact says so.
- none: nothing was concluded. A note, a report nobody answered, a routine change.

Use none rather than guessing. An outcome you had to infer is not an outcome.`

// answer is the JSON the model returns, which internal/llm has already checked
// against [schemaFor] by the time this package decodes it.
type answer struct {
	Summary       string   `json:"summary"`
	Question      string   `json:"question,omitempty"`
	Outcome       string   `json:"outcome,omitempty"`
	OutcomeKind   string   `json:"outcome_kind"`
	OpenQuestions []string `json:"open_questions,omitempty"`
	Change        string   `json:"change,omitempty"`
}

// body turns an answer into the document body.
func (a answer) body() (l1.Body, error) {
	kind, err := l1.ParseOutcomeKind(a.OutcomeKind)
	if err != nil {
		return l1.Body{}, err
	}
	return l1.Body{
		Summary:       a.Summary,
		Question:      a.Question,
		Outcome:       a.Outcome,
		OutcomeKind:   kind,
		OpenQuestions: a.OpenQuestions,
		Change:        a.Change,
	}, nil
}

// The bounds the schema puts on an answer. They are the shape of a distillation
// — a paragraph, not a page — and they are enforced rather than requested: an
// answer over them is refused by internal/llm before this package sees it.
const (
	maxSummary  = 2000
	maxOutcome  = 1000
	maxQuestion = 500
	maxChange   = 1000
	maxOpen     = 8
	maxOpenLen  = 400
)

// schemaFor is the shape a kind's answer has to take. The per-kind fields are
// present only on the kinds that have them, so a model cannot answer with a
// `change` for an issue: `additionalProperties` is false and the field is not
// declared, and the answer is refused rather than quietly dropped.
func schemaFor(kind l1.Kind) *llm.Schema {
	properties := map[string]schemaNode{
		"summary": {
			Type:        "string",
			Description: "What happened, in a few sentences, for someone who has not read the artifact.",
			MinLength:   1,
			MaxLength:   maxSummary,
		},
		"outcome": {
			Type:        "string",
			Description: "What was concluded. Empty when nothing was.",
			MaxLength:   maxOutcome,
		},
		"outcome_kind": {
			Type:        "string",
			Description: "How to classify the outcome.",
			Enum:        outcomeEnum(),
		},
		"open_questions": {
			Type:        "array",
			Description: "What the artifact leaves unanswered, one question per entry.",
			MaxItems:    maxOpen,
			Items:       &schemaNode{Type: "string", MinLength: 1, MaxLength: maxOpenLen},
		},
	}
	switch kind {
	case l1.KindIssue:
		properties["question"] = schemaNode{
			Type:        "string",
			Description: "The question the issue is asking, in one sentence. Empty when it reports rather than asks.",
			MaxLength:   maxQuestion,
		}
	case l1.KindPR, l1.KindCommit:
		properties["change"] = schemaNode{
			Type:        "string",
			Description: "What the change does, in behaviour rather than in files.",
			MaxLength:   maxChange,
		}
	}

	closed := false
	definition, err := json.Marshal(schemaNode{
		Type:                 "object",
		Description:          "One distilled document.",
		Properties:           properties,
		Required:             []string{"summary", "outcome_kind"},
		AdditionalProperties: &closed,
	})
	if err != nil {
		// The tree is this file's and holds no channel, function or NaN, so
		// this cannot happen; a schema that did not encode would be a request
		// with no shape at all, which is worse than a panic in a constructor.
		panic(fmt.Sprintf("encoding the %s schema: %v", kind, err))
	}
	return &llm.Schema{
		Name:        "l1_" + string(kind),
		Description: "The distillation of one " + string(kind) + ".",
		Definition:  definition,
	}
}

// outcomeEnum is the five outcome kinds, taken from internal/l1 rather than
// written out again: the enum a model answers from and the values the column
// accepts are one list.
func outcomeEnum() []string {
	out := make([]string, len(l1.OutcomeKinds))
	for i, o := range l1.OutcomeKinds {
		out[i] = string(o)
	}
	return out
}

// schemaNode is one level of a JSON Schema, in the subset internal/llm enforces.
// Writing it as a type rather than as a string is what keeps the enum and the
// bounds above from drifting from the values the rest of the package uses.
type schemaNode struct {
	Type                 string                `json:"type"`
	Description          string                `json:"description,omitempty"`
	Enum                 []string              `json:"enum,omitempty"`
	MinLength            int                   `json:"minLength,omitempty"`
	MaxLength            int                   `json:"maxLength,omitempty"`
	MaxItems             int                   `json:"maxItems,omitempty"`
	Items                *schemaNode           `json:"items,omitempty"`
	Properties           map[string]schemaNode `json:"properties,omitempty"`
	Required             []string              `json:"required,omitempty"`
	AdditionalProperties *bool                 `json:"additionalProperties,omitempty"`
}

// RequestFor is the model call one document takes: the prompt this package
// owns, the document in the team's own words, and the schema the answer has to
// satisfy.
//
// It is exported so that whatever records a fixture asks for exactly what the
// distiller asks for rather than a copy of it that can drift: a recording is
// keyed on the request, so a prompt edited in one of two places would be a
// recording of a call that never happens.
//
// budget is the answer budget. A call leaves it zero and lets the registry fill
// in the tier's; a recording cannot, because what the recording is keyed on is
// the request the adapter sees, and by then the budget is resolved.
func RequestFor(doc l1.Document, budget int) llm.Request {
	return llm.Request{
		System:    systemPrompt(doc.Kind),
		Messages:  []llm.Message{{Role: llm.RoleUser, Text: promptFor(doc)}},
		MaxTokens: budget,
		Schema:    schemaFor(doc.Kind),
	}
}

// promptFor is the document as the model reads it: the team's own words, already
// scrubbed of secrets and personal data, truncated to what this package sends.
func promptFor(doc l1.Document) string {
	text := doc.RawText
	if len(text) <= MaxPromptBytes {
		return text
	}
	cut := MaxPromptBytes
	// Cut on a rune boundary: half a rune is not a character, and it would make
	// the request differ from a re-run that cut the same place.
	for cut > 0 && !utf8ValidCut(text, cut) {
		cut--
	}
	return text[:cut] + truncated
}

// utf8ValidCut reports whether cutting the string here lands between runes.
func utf8ValidCut(s string, at int) bool {
	if at <= 0 || at >= len(s) {
		return true
	}
	return s[at]&0xC0 != 0x80
}
