package llm_test

import (
	"encoding/json"
	"testing"

	"github.com/kpenfound/hearsay/internal/llm"
)

// distillSchema is the shape a distillation prompt asks for, which is the
// reason this validator exists.
const distillSchema = `{
  "type": "object",
  "properties": {
    "summary": {"type": "string", "minLength": 1, "maxLength": 400},
    "outcome_kind": {"type": "string", "enum": ["decided", "proposed", "resolved", "informational", "open"]},
    "open_questions": {"type": "array", "items": {"type": "string"}, "maxItems": 5},
    "confidence": {"type": "number", "minimum": 0, "maximum": 1}
  },
  "required": ["summary", "outcome_kind"],
  "additionalProperties": false
}`

func schema(definition string) *llm.Schema {
	return &llm.Schema{Name: "distillation", Description: "one L1 document", Definition: json.RawMessage(definition)}
}

func TestSchemaValidate(t *testing.T) {
	for _, tt := range []struct {
		name    string
		schema  llm.Schema
		wantErr string
	}{
		{
			name:   "the shape a distiller asks for",
			schema: *schema(distillSchema),
		},
		{
			name:    "no name",
			schema:  llm.Schema{Definition: json.RawMessage(`{"type":"object"}`)},
			wantErr: "no name",
		},
		{
			name:    "a name a tool cannot have",
			schema:  llm.Schema{Name: "distill this!", Definition: json.RawMessage(`{"type":"object"}`)},
			wantErr: "letters, digits",
		},
		{
			name:    "no definition",
			schema:  llm.Schema{Name: "distillation"},
			wantErr: "empty",
		},
		{
			name:    "not an object at the top",
			schema:  *schema(`{"type": "string"}`),
			wantErr: `"type": "object"`,
		},
		{
			name:    "a keyword nothing here can enforce",
			schema:  *schema(`{"type":"object","properties":{"id":{"type":"string","pattern":"^[a-z]+$"}}}`),
			wantErr: `"pattern" is not understood`,
		},
		{
			name:    "a type that is not a JSON type",
			schema:  *schema(`{"type":"object","properties":{"n":{"type":"int"}}}`),
			wantErr: `no such type "int"`,
		},
		{
			name:    "an empty enum, which nothing satisfies",
			schema:  *schema(`{"type":"object","properties":{"k":{"enum":[]}}}`),
			wantErr: "enum is empty",
		},
		{
			name:    "additionalProperties as a schema",
			schema:  *schema(`{"type":"object","additionalProperties":{"type":"string"}}`),
			wantErr: "true or false",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.schema.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Validate() = %v, want no error", err)
			case tt.wantErr == "":
				return
			case err == nil:
				t.Fatalf("Validate() = nil, want an error about %q", tt.wantErr)
			case !contains(err.Error(), tt.wantErr):
				t.Errorf("Validate() = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestSchemaValidateJSON(t *testing.T) {
	for _, tt := range []struct {
		name       string
		definition string
		answer     string
		wantErr    string
	}{
		{
			name:       "the shape it asked for",
			definition: distillSchema,
			answer:     `{"summary":"they agreed to ship on Friday","outcome_kind":"decided","open_questions":[],"confidence":0.9}`,
		},
		{
			name:       "only what is required",
			definition: distillSchema,
			answer:     `{"summary":"s","outcome_kind":"open"}`,
		},
		{
			name:       "a required field missing",
			definition: distillSchema,
			answer:     `{"summary":"s"}`,
			wantErr:    `the field "outcome_kind" is missing`,
		},
		{
			name:       "a value outside the enum",
			definition: distillSchema,
			answer:     `{"summary":"s","outcome_kind":"maybe"}`,
			wantErr:    `"maybe" is not one of`,
		},
		{
			name:       "the wrong type",
			definition: distillSchema,
			answer:     `{"summary":7,"outcome_kind":"open"}`,
			wantErr:    "want string, found an integer",
		},
		{
			name:       "a field the schema closed the door on",
			definition: distillSchema,
			answer:     `{"summary":"s","outcome_kind":"open","vibes":"good"}`,
			wantErr:    `no such field "vibes"`,
		},
		{
			name:       "too many items",
			definition: distillSchema,
			answer:     `{"summary":"s","outcome_kind":"open","open_questions":["a","b","c","d","e","f"]}`,
			wantErr:    "6 items, want at most 5",
		},
		{
			name:       "an item of the wrong type",
			definition: distillSchema,
			answer:     `{"summary":"s","outcome_kind":"open","open_questions":["a",2]}`,
			wantErr:    "/open_questions/1",
		},
		{
			name:       "a number out of range",
			definition: distillSchema,
			answer:     `{"summary":"s","outcome_kind":"open","confidence":1.5}`,
			wantErr:    "want at most 1",
		},
		{
			name:       "a string shorter than the minimum",
			definition: distillSchema,
			answer:     `{"summary":"","outcome_kind":"open"}`,
			wantErr:    "0 characters, want at least 1",
		},
		{
			name:       "not JSON at all",
			definition: distillSchema,
			answer:     "I could not do that",
			wantErr:    "not JSON",
		},
		{
			name:       "JSON followed by more JSON",
			definition: distillSchema,
			answer:     `{"summary":"s","outcome_kind":"open"} {"summary":"t","outcome_kind":"open"}`,
			wantErr:    "followed by more JSON",
		},
		{
			name:       "an integer where a whole number is wanted",
			definition: `{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`,
			answer:     `{"n":4}`,
		},
		{
			name:       "a fraction is not an integer",
			definition: `{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`,
			answer:     `{"n":4.5}`,
			wantErr:    "want integer, found a number",
		},
		{
			name:       "a whole float is an integer",
			definition: `{"type":"object","properties":{"n":{"type":"integer"}},"required":["n"]}`,
			answer:     `{"n":4.0}`,
		},
		{
			name:       "null where the type allows it",
			definition: `{"type":"object","properties":{"n":{"type":["string","null"]}}}`,
			answer:     `{"n":null}`,
		},
		{
			name:       "null where it does not",
			definition: `{"type":"object","properties":{"n":{"type":"string"}}}`,
			answer:     `{"n":null}`,
			wantErr:    "want string, found null",
		},
		{
			name:       "a nested object",
			definition: `{"type":"object","properties":{"who":{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}},"required":["who"]}`,
			answer:     `{"who":{}}`,
			wantErr:    `/who: the field "id" is missing`,
		},
		{
			name:       "a constant",
			definition: `{"type":"object","properties":{"v":{"const":1}}}`,
			answer:     `{"v":2}`,
			wantErr:    "want the constant 1",
		},
		{
			name:       "a number matches its enum however it is written",
			definition: `{"type":"object","properties":{"v":{"enum":[1,2]}}}`,
			answer:     `{"v":1.0}`,
		},
		{
			name:       "the answer is not an object",
			definition: distillSchema,
			answer:     `["a"]`,
			wantErr:    "the answer: want object, found an array",
		},
		{
			name:       "too few items",
			definition: `{"type":"object","properties":{"xs":{"type":"array","minItems":2}}}`,
			answer:     `{"xs":["a"]}`,
			wantErr:    "1 items, want at least 2",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := schema(tt.definition).ValidateJSON([]byte(tt.answer))
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("ValidateJSON() = %v, want no error", err)
			case tt.wantErr == "":
				return
			case err == nil:
				t.Fatalf("ValidateJSON() = nil, want an error about %q", tt.wantErr)
			case !contains(err.Error(), tt.wantErr):
				t.Errorf("ValidateJSON() = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

// Every keyword the package says it understands has to be one it accepts, and
// nothing outside the list may be accepted in silence.
func TestSchemaKeywordsAreTheOnesUnderstood(t *testing.T) {
	values := map[string]string{
		"additionalProperties": "false",
		"const":                `"x"`,
		"default":              `"x"`,
		"description":          `"what it is"`,
		"enum":                 `["x"]`,
		"items":                `{"type": "string"}`,
		"maxItems":             "3",
		"maxLength":            "3",
		"maximum":              "3",
		"minItems":             "1",
		"minLength":            "1",
		"minimum":              "0",
		"properties":           `{"x": {"type": "string"}}`,
		"required":             `[]`,
		"title":                `"a title"`,
		"type":                 `"object"`,
	}
	for _, keyword := range llm.SchemaKeywords {
		value, ok := values[keyword]
		if !ok {
			t.Fatalf("SchemaKeywords has %q and this test does not exercise it", keyword)
		}
		definition := `{"type":"object","properties":{"field":{` + `"` + keyword + `": ` + value + `}}}`
		if err := schema(definition).Validate(); err != nil {
			t.Errorf("a schema using %q is refused: %v", keyword, err)
		}
	}
	if err := schema(`{"type":"object","$ref":"#/$defs/thing"}`).Validate(); err == nil {
		t.Error("a schema using $ref is accepted, and nothing here resolves one")
	}
}
