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
			wantErr:    `not one of "decided", "proposed"`,
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
		// A schema is bytes somebody typed and an answer is a value the
		// decoder produced. Everything below is the same value written two
		// ways, and an answer that is right has to pass.
		{
			name:       "an enum member with a byte an encoder escapes",
			definition: `{"type":"object","properties":{"label":{"type":"string","enum":["a & b","x < y","plain"]}}}`,
			answer:     `{"label":"a & b"}`,
		},
		{
			name:       "a constant with angle brackets",
			definition: `{"type":"object","properties":{"t":{"const":"<tag>"}}}`,
			answer:     `{"t":"<tag>"}`,
		},
		{
			name:       "an enum member that is genuinely not the answer",
			definition: `{"type":"object","properties":{"label":{"type":"string","enum":["a & b"]}}}`,
			answer:     `{"label":"a & c"}`,
			wantErr:    `not one of "a & b"`,
		},
		{
			name:       "an object constant whose keys are not in alphabetical order",
			definition: `{"type":"object","properties":{"c":{"const":{"b":1,"a":2}}}}`,
			answer:     `{"c":{"b":1,"a":2}}`,
		},
		{
			name:       "an object constant the answer does not match",
			definition: `{"type":"object","properties":{"c":{"const":{"b":1,"a":2}}}}`,
			answer:     `{"c":{"b":1,"a":3}}`,
			wantErr:    "want the constant",
		},
		{
			name:       "a number inside a constant, written the other way",
			definition: `{"type":"object","properties":{"c":{"const":{"a":1.0,"xs":[2e0]}}}}`,
			answer:     `{"c":{"a":1,"xs":[2]}}`,
		},
		{
			name:       "an id beyond a float64's exact range",
			definition: `{"type":"object","properties":{"c":{"const":{"id":9007199254740993}}}}`,
			answer:     `{"c":{"id":9007199254740993}}`,
		},
		{
			name:       "the id one away from it",
			definition: `{"type":"object","properties":{"c":{"const":{"id":9007199254740993}}}}`,
			answer:     `{"c":{"id":9007199254740992}}`,
			wantErr:    "want the constant",
		},
		{
			name:       "a string that is not the number it looks like",
			definition: `{"type":"object","properties":{"v":{"enum":[1]}}}`,
			answer:     `{"v":"1"}`,
			wantErr:    "not one of",
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

// An enum is matched against a completion, and the error goes to the caller,
// which for the distiller and the assertion worker is a queue handler: its text
// is written to last_error and logged at error level (ADR-0007). ADR-0008 says
// a completion reaches neither. So the message says what was allowed and what
// kind of thing came back, and not one byte of what came back.
func TestASchemaViolationDoesNotQuoteTheAnswer(t *testing.T) {
	answered := "the team decided to defer until the Acme contract closes"
	for _, tt := range []struct {
		name       string
		definition string
		answer     string
		want       string
	}{
		{
			name:       "an enum",
			definition: `{"type":"object","properties":{"outcome_kind":{"enum":["decided","proposed","blocked"]}}}`,
			answer:     `{"outcome_kind":"` + answered + `"}`,
			want:       `not one of "decided", "proposed", "blocked"`,
		},
		{
			name:       "a constant",
			definition: `{"type":"object","properties":{"outcome_kind":{"const":"decided"}}}`,
			answer:     `{"outcome_kind":"` + answered + `"}`,
			want:       `want the constant "decided"`,
		},
		{
			name:       "an enum answered with an object, which prints whole",
			definition: `{"type":"object","properties":{"outcome_kind":{"enum":["decided"]}}}`,
			answer:     `{"outcome_kind":{"verdict":"` + answered + `"}}`,
			want:       "found an object",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := schema(tt.definition).ValidateJSON([]byte(tt.answer))
			if err == nil {
				t.Fatal("ValidateJSON() = nil, want the violation")
			}
			if contains(err.Error(), answered) || contains(err.Error(), "Acme") {
				t.Errorf("ValidateJSON() = %q, want the answer kept out of it", err)
			}
			if !contains(err.Error(), tt.want) {
				t.Errorf("ValidateJSON() = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

// The one thing a model produced that does reach an error is the name of a
// field the schema does not allow — it is what tells a prompt's author what the
// model wrote instead — and it is bounded, because a model that answered with a
// sentence for a key must not put the sentence in a log line.
func TestARejectedFieldNameIsNamedAndBounded(t *testing.T) {
	long := "the team decided to defer until the Acme contract closes and the paperwork is signed"
	err := schema(distillSchema).ValidateJSON([]byte(
		`{"summary":"s","outcome_kind":"open","` + long + `":"x"}`))
	if err == nil {
		t.Fatal("ValidateJSON() = nil, want the field refused")
	}
	if contains(err.Error(), long) || contains(err.Error(), "paperwork") {
		t.Errorf("ValidateJSON() = %q, want the name cut", err)
	}
	if !contains(err.Error(), `no such field "the team decided`) || !contains(err.Error(), "…") {
		t.Errorf("ValidateJSON() = %q, want the start of the name and a mark that it was cut", err)
	}
	if len(err.Error()) > 128 {
		t.Errorf("ValidateJSON() is %d bytes: %q", len(err.Error()), err)
	}
	// A name of an ordinary size is not cut.
	err = schema(distillSchema).ValidateJSON([]byte(`{"summary":"s","outcome_kind":"open","vibes":"good"}`))
	if err == nil || !contains(err.Error(), `no such field "vibes"`) {
		t.Errorf("ValidateJSON() = %v, want the whole name of a short field", err)
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
