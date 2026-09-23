package assertworker

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/llm"
)

// The prompts belong to this package, not to internal/l2: what a topic and a
// stance are is the layer's; how a model is asked to find them in a document is
// the assertion worker's (ADR-0005). The system prompt and the schema are both
// here because they say the same thing twice.

// MaxAssertions is the most positions one document takes. A document is about
// one thing; one that takes more than a handful of positions is one the model is
// reading too generously.
const MaxAssertions = 4

// NewTopic is the answer that opens a topic rather than continuing a candidate.
const NewTopic = "new"

// maxTopicName bounds what the model may call a topic. It is a question in a
// line, far under what the table allows.
const maxTopicName = 300

// maxPosition bounds one position: a sentence or two.
const maxPosition = 1000

var systemPrompt = `You are Hearsay's assertion worker. You read one distilled document from a team's tools and write down the positions it takes: what the team proposed, decided or did, each as an answer to one question.

A topic is a question the team takes positions on, phrased so that a later document about the same question would recognise it — "where the engine takes its lock relative to the write", not "pull request 31". A position is one answer to that question, in one sentence, as this document states it.

You are shown the existing topics this document may be continuing, each with a label and the position currently recorded on it. Where the document takes a position on one of them, answer with that label and judge whether this position changes or restates the current position shown, even if the words differ. Judge against the position shown, including when this document was read before. Answer "new" only for a question none of them asks, and omit the judgement for a new topic. Never put two positions from this document on one topic.

Take at most ` + strconv.Itoa(MaxAssertions) + ` positions, and none where the document takes none: an empty list is a correct answer. Keep every position to what the document says. Never quote a credential, a token, a key or a personal email address.`

// Candidate is one existing topic a document may be continuing, as the model
// is shown it: its name and the position currently recorded on it. It carries
// no id — the model answers with a label — so that the request does not depend
// on anything but what the team said.
type Candidate struct {
	Name    string
	Current string
}

// label is how the model refers to the i-th candidate.
func label(i int) string { return "T" + strconv.Itoa(i+1) }

// RequestFor is the model call one document takes, given the topics it may be
// continuing. It is exported so that whatever records a fixture asks for
// exactly what the worker asks for; budget is the answer budget, left zero by a
// call so the registry fills in the tier's.
func RequestFor(doc l1.Document, candidates []Candidate, budget int) llm.Request {
	return llm.Request{
		System:    systemPrompt,
		Messages:  []llm.Message{{Role: llm.RoleUser, Text: promptFor(doc, candidates)}},
		MaxTokens: budget,
		Schema:    schemaFor(len(candidates)),
	}
}

func promptFor(doc l1.Document, candidates []Candidate) string {
	var b strings.Builder
	fmt.Fprintf(&b, "This is a %s document. What it concluded is classified as %s.\n\n", doc.Kind, doc.Body.OutcomeKind)
	b.WriteString(strings.TrimSpace(doc.Text))
	if len(candidates) == 0 {
		b.WriteString("\n\nThere are no existing topics this document could be continuing.")
		return b.String()
	}
	b.WriteString("\n\nExisting topics this document may be continuing:")
	for i, c := range candidates {
		current := c.Current
		if current == "" {
			current = "none recorded"
		}
		fmt.Fprintf(&b, "\n\n%s: %s\nCurrent position: %s", label(i), c.Name, current)
	}
	return b.String()
}

// answer is the JSON the model returns, already checked against schemaFor.
type answer struct {
	Assertions []assertion `json:"assertions"`
}

type assertion struct {
	Topic     string        `json:"topic"`
	TopicName string        `json:"topic_name"`
	Position  string        `json:"position"`
	Judgement *l2.Judgement `json:"judgement"`
}

// schemaFor is the answer's shape for a prompt with n candidates. The topic is
// an enum of exactly the labels the prompt showed and "new", so a model cannot
// answer with a topic it was not offered.
func schemaFor(n int) *llm.Schema {
	labels := []string{NewTopic}
	for i := range n {
		labels = append(labels, label(i))
	}
	closed := false
	definition, err := json.Marshal(node{
		Type:        "object",
		Description: "The positions one document takes.",
		Properties: map[string]node{
			"assertions": {
				Type:        "array",
				Description: "One entry per position, at most one per topic. Empty when the document takes none.",
				MaxItems:    MaxAssertions,
				Items: &node{
					Type: "object",
					Properties: map[string]node{
						"topic": {
							Type:        "string",
							Description: "The label of the existing topic this position is on, or \"new\".",
							Enum:        labels,
						},
						"topic_name": {
							Type:        "string",
							Description: "The question this position answers, in a line. For an existing topic, its name.",
							MinLength:   1,
							MaxLength:   maxTopicName,
						},
						"position": {
							Type:        "string",
							Description: "The position, in one sentence, as the document states it.",
							MinLength:   1,
							MaxLength:   maxPosition,
						},
						"judgement": {
							Type:        "string",
							Description: "For an existing topic only: changes or restates the current position shown. Omit for new topics.",
							Enum:        []string{string(l2.JudgementChanges), string(l2.JudgementRestates)},
						},
					},
					Required:             []string{"topic", "topic_name", "position"},
					AdditionalProperties: &closed,
				},
			},
		},
		Required:             []string{"assertions"},
		AdditionalProperties: &closed,
	})
	if err != nil {
		// The tree is this file's and holds nothing that fails to encode.
		panic(fmt.Sprintf("encoding the assertion schema: %v", err))
	}
	return &llm.Schema{
		Name:        "l2_assertions",
		Description: "The topics and positions one document asserts.",
		Definition:  definition,
	}
}

// node is one level of a JSON Schema, in the subset internal/llm enforces.
type node struct {
	Type                 string          `json:"type"`
	Description          string          `json:"description,omitempty"`
	Enum                 []string        `json:"enum,omitempty"`
	MinLength            int             `json:"minLength,omitempty"`
	MaxLength            int             `json:"maxLength,omitempty"`
	MaxItems             int             `json:"maxItems,omitempty"`
	Items                *node           `json:"items,omitempty"`
	Properties           map[string]node `json:"properties,omitempty"`
	Required             []string        `json:"required,omitempty"`
	AdditionalProperties *bool           `json:"additionalProperties,omitempty"`
}

// The schema's bounds have to fit the table's, or an answer the schema accepts
// is one the store refuses. This does not compile if they stop fitting.
const (
	_ = uint(l2.MaxTopicName - maxTopicName)
	_ = uint(l2.MaxPosition - maxPosition)
)
