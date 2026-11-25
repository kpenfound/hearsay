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

// MaxAssertions is the most positions one document takes.
const MaxAssertions = 4

const maxTopicName = 300

const maxPosition = 1000

var systemPrompt = `You are Hearsay's assertion worker. You read one distilled document from a team's tools and write down the positions it takes: what the team proposed, decided or did, each as an answer to one question.

A topic is a question the team takes positions on, phrased so that a later document about the same question would recognise it — "where the engine takes its lock relative to the write", not "pull request 31". A position is one answer to that question, in one sentence, as this document states it.

Give each position the question it answers as its topic name. Which existing topic a position continues, and whether it changes the position recorded there, is decided after you answer. Never take two positions on one question.

Take at most ` + strconv.Itoa(MaxAssertions) + ` positions, and none where the document takes none: an empty list is a correct answer. Keep every position to what the document says. Never quote a credential, a token, a key or a personal email address.`

// Candidate is one existing topic a document may be continuing, as Jev is
// shown it: its name and the position currently recorded on it, when the
// document's readers may read that position. It carries no id: a question
// names it by label.
type Candidate struct {
	Name    string
	Current string
}

func label(i int) string { return "T" + strconv.Itoa(i+1) }

// RequestFor is the model call one document takes. It depends on the document
// alone: the model extracts positions and is shown no topics. budget is the
// answer budget, left zero by a call so the registry fills in the tier's.
func RequestFor(doc l1.Document, budget int) llm.Request {
	return llm.Request{
		System:    systemPrompt,
		Messages:  []llm.Message{{Role: llm.RoleUser, Text: promptFor(doc)}},
		MaxTokens: budget,
		Schema:    schema,
	}
}

func promptFor(doc l1.Document) string {
	return fmt.Sprintf("This is a %s document. What it concluded is classified as %s.\n\n", doc.Kind, doc.Body.OutcomeKind) +
		strings.TrimSpace(doc.StanceText())
}

type answer struct {
	Assertions []assertion `json:"assertions"`
}

type assertion struct {
	TopicName string `json:"topic_name"`
	Position  string `json:"position"`
}

var schema = schemaFor()

func schemaFor() *llm.Schema {
	closed := false
	definition, err := json.Marshal(node{
		Type:        "object",
		Description: "The positions one document takes.",
		Properties: map[string]node{
			"assertions": {
				Type:        "array",
				Description: "One entry per position, at most one per question. Empty when the document takes none.",
				MaxItems:    MaxAssertions,
				Items: &node{
					Type: "object",
					Properties: map[string]node{
						"topic_name": {
							Type:        "string",
							Description: "The question this position answers, in a line.",
							MinLength:   1,
							MaxLength:   maxTopicName,
						},
						"position": {
							Type:        "string",
							Description: "The position, in one sentence, as the document states it.",
							MinLength:   1,
							MaxLength:   maxPosition,
						},
					},
					Required:             []string{"topic_name", "position"},
					AdditionalProperties: &closed,
				},
			},
		},
		Required:             []string{"assertions"},
		AdditionalProperties: &closed,
	})
	if err != nil {
		panic(fmt.Sprintf("encoding the assertion schema: %v", err))
	}
	return &llm.Schema{
		Name:        "l2_assertions",
		Description: "The topics and positions one document asserts.",
		Definition:  definition,
	}
}

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

const (
	_ = uint(l2.MaxTopicName - maxTopicName)
	_ = uint(l2.MaxPosition - maxPosition)
)
