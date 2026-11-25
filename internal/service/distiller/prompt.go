package distiller

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/llm"
)

// MaxPromptBytes bounds how much of a document is sent to the model. A
// conversation longer than this is truncated deterministically on a rune
// boundary, with a marker.
const MaxPromptBytes = 120_000

const truncated = "\n\n[the rest of this conversation was left out: it is longer than the distiller sends]"

func systemPrompt(kind l1.Kind) string {
	return preamble + "\n\n" + kindPrompts[kind] + "\n\n" + outcomeRules
}

const preamble = `You are Hearsay's distiller. You turn one artifact from a team's tools into one short document that a coding agent will read later, when the artifact itself is long gone from anybody's memory.

Write for someone who has to make a decision and has not read any of this. Say what the team worked out and why, not what the software does. Keep every claim to what the document in front of you says: if something is not there, leave the field empty rather than filling it in from what usually happens.

Never quote a credential, a token, a key or a personal email address, even if the document contains one.`

var kindPrompts = map[l1.Kind]string{
	l1.KindMeetingSegment: `This is one topic of a meeting transcript. Use only the transcript lines in this segment.

- summary: preserve the discussion and reasoning on this topic alone.
- outcome: only a decision, resolution, or proposal explicitly made in these lines. Empty when these lines conclude nothing.
- open_questions: questions left unanswered in these lines.

Do not import an outcome from another topic.`,
	l1.KindConversationSegment: `This is one topic of a conversation between a person (the owner) and their assistant (Kern). Each numbered line is one message, labelled Owner, Owner (interrupted) for words the owner left unfinished, or Kern.

- summary: what was discussed on this topic, including what Kern answered, for someone who has not read it.
- question: the main question the owner asked, if one was asked. Empty otherwise.
- outcome: only what the OWNER's own lines decide, resolve or propose, as one sentence. Kern's lines are context, never the source of an outcome: something only Kern recommended, decided or concluded is not an outcome, even if the owner did not object. Empty when the owner's lines conclude nothing.
- outcome_lines: the numbers of the Owner lines the outcome rests on. Empty when the outcome is empty. Never cite a Kern line.
- open_questions: questions this topic leaves unanswered, one per entry.

Do not import an outcome from another topic.`,
	l1.KindWikiSection: `This is one section of a document or wiki page. The markdown heading and body are source text, including any frontmatter, tags, and wikilinks.

- summary: preserve the facts and reasoning in this section only.
- outcome: only a decision, resolution or proposal this section itself states. Empty for descriptive material. Do not carry conclusions from other sections.
- open_questions: questions left unanswered in this section, one per entry.`,
	l1.KindChatBurst: `This is one author's contiguous tangent inside a longer chat conversation.

- summary: preserve the distinct facts or reasoning in this tangent, using only these messages.
- outcome: empty; the containing thread owns any conclusion.
- open_questions: empty.

Do not repeat the containing thread's conclusion.`,
	l1.KindChatThread: `This is a conversation in a chat channel or native thread.

- summary: briefly describe what people discussed.
- question: the main question, if one was asked. Empty otherwise.
- outcome: only what participants explicitly decided, resolved or proposed. Empty for a routine exchange.
- open_questions: questions the conversation leaves unanswered, one per entry.
- code_names: short names participants explicitly use for code in this thread, without guessing what they refer to. Empty if none.`,
	l1.KindIssue: `This is a tracker issue and the conversation on it.

- summary: what is being asked for or reported, and where the conversation got to.
- question: the question the issue is really asking, in one sentence. Empty if it reports rather than asks.
- outcome: what was concluded, if anything was.
- open_questions: what the thread leaves unanswered, one per entry, each a question.`,

	l1.KindPR: `This is a change proposal with its reviews and its comments.

- summary: what is being proposed and what the review said about it.
- change: what the change does, in one or two sentences. Behaviour, not files.
- outcome: what the change settles or puts forward, if anything.
- open_questions: what the review raised and nobody answered, one per entry.`,

	l1.KindCommit: `This is one commit on a watched branch.

- summary: what changed and why, from the message. Say "no reason given" rather than inventing one.
- change: what the commit does, in one sentence.
- outcome: what it settles, if the message says it settles anything.
- open_questions: usually empty. A commit message rarely leaves a question.`,
}

const outcomeRules = `outcome is one sentence stating the substance of what the artifact concluded, settled or put forward, such as "take the lock before the write". Do not say whether anyone agreed to it or whether it was carried out; that is judged separately. Leave it empty when the artifact states nothing of the kind. An outcome you had to infer is not an outcome.`

type answer struct {
	Summary       string   `json:"summary"`
	Question      string   `json:"question,omitempty"`
	Outcome       string   `json:"outcome,omitempty"`
	OpenQuestions []string `json:"open_questions,omitempty"`
	Change        string   `json:"change,omitempty"`
	CodeNames     []string `json:"code_names,omitempty"`

	OutcomeLines []int `json:"outcome_lines,omitempty"`
}

func (a answer) body() l1.Body {
	return l1.Body{
		Summary:       a.Summary,
		Question:      a.Question,
		Outcome:       a.Outcome,
		OpenQuestions: a.OpenQuestions,
		Change:        a.Change,
		CodeNames:     a.CodeNames,
	}
}

const (
	maxSummary  = 2000
	maxOutcome  = 1000
	maxQuestion = 500
	maxChange   = 1000
	maxOpen     = 8
	maxOpenLen  = 400

	maxOutcomeLines = 16
)

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
			Description: "The substance of what was concluded, settled or put forward, in one sentence. Empty when nothing was.",
			MaxLength:   maxOutcome,
		},
		"open_questions": {
			Type:        "array",
			Description: "What the artifact leaves unanswered, one question per entry.",
			MaxItems:    maxOpen,
			Items:       &schemaNode{Type: "string", MinLength: 1, MaxLength: maxOpenLen},
		},
	}
	switch kind {
	case l1.KindConversationSegment:
		properties["question"] = schemaNode{
			Type:        "string",
			Description: "The main question the owner asked, in one sentence. Empty when none was asked.",
			MaxLength:   maxQuestion,
		}
		properties["outcome_lines"] = schemaNode{
			Type:        "array",
			Description: "The numbers of the Owner lines the outcome rests on. Empty when there is no outcome.",
			MaxItems:    maxOutcomeLines,
			Items:       &schemaNode{Type: "integer"},
		}
	case l1.KindIssue, l1.KindChatThread:
		properties["question"] = schemaNode{
			Type:        "string",
			Description: "The question the issue is asking, in one sentence. Empty when it reports rather than asks.",
			MaxLength:   maxQuestion,
		}
		if kind == l1.KindChatThread {
			properties["code_names"] = schemaNode{Type: "array", Description: "Short names explicitly used for code in this conversation; do not infer them.", MaxItems: 8, Items: &schemaNode{Type: "string", MinLength: 1, MaxLength: 100}}
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
		Required:             []string{"summary"},
		AdditionalProperties: &closed,
	})
	if err != nil {
		panic(fmt.Sprintf("encoding the %s schema: %v", kind, err))
	}
	return &llm.Schema{
		Name:        "l1_" + string(kind),
		Description: "The distillation of one " + string(kind) + ".",
		Definition:  definition,
	}
}

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
// budget is the answer budget. A call leaves it zero and lets the registry fill
// in the tier's; a recording passes the resolved budget.
func RequestFor(doc l1.Document, budget int) llm.Request {
	return llm.Request{
		System:    systemPrompt(doc.Kind),
		Messages:  []llm.Message{{Role: llm.RoleUser, Text: promptFor(doc)}},
		MaxTokens: budget,
		Schema:    schemaFor(doc.Kind),
	}
}

func promptFor(doc l1.Document) string {
	text := doc.RawText
	if doc.Kind == l1.KindConversationSegment {
		text = numbered(text)
	}
	if len(text) <= MaxPromptBytes {
		return text
	}
	cut := MaxPromptBytes

	for cut > 0 && !utf8ValidCut(text, cut) {
		cut--
	}
	return text[:cut] + truncated
}

func numbered(raw string) string {
	lines := strings.Split(raw, "\n")
	for i, line := range lines {
		lines[i] = fmt.Sprintf("%d. %s", i+1, line)
	}
	return strings.Join(lines, "\n")
}

func utf8ValidCut(s string, at int) bool {
	if at <= 0 || at >= len(s) {
		return true
	}
	return s[at]&0xC0 != 0x80
}
