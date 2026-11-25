package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Schema is the shape a structured answer has to take: a name, and a JSON
// Schema object describing the JSON the caller wants back. The answer is
// checked against the schema before it is returned.
type Schema struct {
	// Name names the shape: letters, digits, `_` and `-`, up to 64 of them.
	Name string `json:"name"`
	// Description says what the shape is for. It reaches the model.
	Description string `json:"description,omitempty"`
	// Definition is the JSON Schema, which has to describe an object.
	//
	// Only the keywords in [SchemaKeywords] are understood. A schema using
	// anything else is refused rather than sent.
	Definition json.RawMessage `json:"definition"`
}

// SchemaKeywords is every JSON Schema keyword this package understands, sorted.
// Every one of them is enforced on the answer except `title`, `description`
// and `default`, which constrain nothing.
var SchemaKeywords = []string{
	"additionalProperties", "const", "default", "description", "enum", "items",
	"maxItems", "maxLength", "maximum", "minItems", "minLength", "minimum",
	"properties", "required", "title", "type",
}

var schemaTypes = []string{"array", "boolean", "integer", "null", "number", "object", "string"}

type node struct {
	types      []string
	properties map[string]*node
	propOrder  []string
	required   []string
	additional *bool
	items      *node
	enum       []json.RawMessage
	constant   *json.RawMessage
	minimum    *float64
	maximum    *float64
	minLength  *int
	maxLength  *int
	minItems   *int
	maxItems   *int
}

// Validate reports what is wrong with the schema itself, before a model is
// asked for anything shaped like it.
func (s *Schema) Validate() error {
	if err := validSchemaName(s.Name); err != nil {
		return err
	}
	root, err := s.parse()
	if err != nil {
		return err
	}
	if !slices.Contains(root.types, "object") {
		return fmt.Errorf(`the top level has to be "type": "object"`)
	}
	return nil
}

func (s *Schema) parse() (*node, error) {
	if len(s.Definition) == 0 {
		return nil, fmt.Errorf("the definition is empty")
	}
	return parseNode(s.Definition, "")
}

// ValidateJSON reports whether data satisfies the schema. The message names the
// field in JSON Pointer form.
func (s *Schema) ValidateJSON(data []byte) error {
	root, err := s.parse()
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return fmt.Errorf("the answer is not JSON: %w", err)
	}
	if dec.More() {
		return fmt.Errorf("the answer is followed by more JSON")
	}
	return root.validate(v, "")
}

func validSchemaName(name string) error {
	if name == "" {
		return fmt.Errorf("the schema has no name: it names the shape, and a provider that gets JSON out of a tool call uses it as the tool's name")
	}
	if len(name) > 64 {
		return fmt.Errorf("the schema name is %d bytes: 64 at most", len(name))
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return fmt.Errorf("the schema name %q has a %q in it: letters, digits, _ and - only", name, r)
		}
	}
	return nil
}

func parseNode(raw json.RawMessage, path string) (*node, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("%s: not a JSON Schema object: %w", in(path), err)
	}
	n := &node{}
	for _, key := range sortedKeys(fields) {
		v := fields[key]
		var err error
		switch key {
		case "type":
			n.types, err = parseTypes(v, path)
		case "properties":
			n.properties, n.propOrder, err = parseProperties(v, path)
		case "required":
			err = json.Unmarshal(v, &n.required)
		case "additionalProperties":
			var b bool
			if err = json.Unmarshal(v, &b); err == nil {
				n.additional = &b
			} else {
				err = fmt.Errorf("additionalProperties is true or false here: a schema for it is not understood")
			}
		case "items":
			n.items, err = parseNode(v, path+"/items")
		case "enum":
			err = json.Unmarshal(v, &n.enum)
			if err == nil && len(n.enum) == 0 {
				err = fmt.Errorf("enum is empty: nothing would satisfy it")
			}
		case "const":
			c := json.RawMessage(slices.Clone(v))
			n.constant = &c
		case "minimum":
			n.minimum, err = parseFloat(v)
		case "maximum":
			n.maximum, err = parseFloat(v)
		case "minLength":
			n.minLength, err = parseInt(v)
		case "maxLength":
			n.maxLength, err = parseInt(v)
		case "minItems":
			n.minItems, err = parseInt(v)
		case "maxItems":
			n.maxItems, err = parseInt(v)
		case "title", "description", "default":

		default:
			err = fmt.Errorf("the keyword %q is not understood: %s only, because a constraint this package cannot check is one the caller would believe was enforced",
				key, strings.Join(SchemaKeywords, ", "))
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", in(path), err)
		}
	}
	return n, nil
}

func parseTypes(v json.RawMessage, path string) ([]string, error) {
	var one string
	types := []string{}
	if err := json.Unmarshal(v, &one); err == nil {
		types = []string{one}
	} else if err := json.Unmarshal(v, &types); err != nil {
		return nil, fmt.Errorf("type is a type name or a list of them")
	}
	if len(types) == 0 {
		return nil, fmt.Errorf("type is empty")
	}
	for _, t := range types {
		if !slices.Contains(schemaTypes, t) {
			return nil, fmt.Errorf("no such type %q: %s", t, strings.Join(schemaTypes, ", "))
		}
	}
	return types, nil
}

func parseProperties(v json.RawMessage, path string) (map[string]*node, []string, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(v, &raw); err != nil {
		return nil, nil, fmt.Errorf("properties is a mapping from a field name to its schema")
	}
	props := make(map[string]*node, len(raw))
	order := sortedKeys(raw)
	for _, name := range order {
		child, err := parseNode(raw[name], path+"/"+name)
		if err != nil {
			return nil, nil, err
		}
		props[name] = child
	}
	return props, order, nil
}

func parseFloat(v json.RawMessage) (*float64, error) {
	var f float64
	if err := json.Unmarshal(v, &f); err != nil {
		return nil, fmt.Errorf("want a number, found %s", v)
	}
	return &f, nil
}

func parseInt(v json.RawMessage) (*int, error) {
	var i int
	if err := json.Unmarshal(v, &i); err != nil {
		return nil, fmt.Errorf("want a whole number, found %s", v)
	}
	if i < 0 {
		return nil, fmt.Errorf("want a number that is not negative, found %d", i)
	}
	return &i, nil
}

func (n *node) validate(v any, path string) error {
	if len(n.types) > 0 {
		if !slices.ContainsFunc(n.types, func(t string) bool { return isType(v, t) }) {
			return fmt.Errorf("%s: want %s, found %s", at(path), strings.Join(n.types, " or "), kindOf(v))
		}
	}

	if n.constant != nil && !equalJSON(*n.constant, v) {
		return fmt.Errorf("%s: want the constant %s, found %s", at(path), *n.constant, kindOf(v))
	}
	if len(n.enum) > 0 {
		if !slices.ContainsFunc(n.enum, func(e json.RawMessage) bool { return equalJSON(e, v) }) {
			return fmt.Errorf("%s: not one of %s, found %s", at(path), enumList(n.enum), kindOf(v))
		}
	}
	switch value := v.(type) {
	case map[string]any:
		return n.validateObject(value, path)
	case []any:
		return n.validateArray(value, path)
	case string:
		if n.minLength != nil && len([]rune(value)) < *n.minLength {
			return fmt.Errorf("%s: %d characters, want at least %d", at(path), len([]rune(value)), *n.minLength)
		}
		if n.maxLength != nil && len([]rune(value)) > *n.maxLength {
			return fmt.Errorf("%s: %d characters, want at most %d", at(path), len([]rune(value)), *n.maxLength)
		}
	case json.Number:
		f, err := value.Float64()
		if err != nil {
			return fmt.Errorf("%s: %s is not a number this package can compare: %w", at(path), value, err)
		}
		if n.minimum != nil && f < *n.minimum {
			return fmt.Errorf("%s: %s, want at least %v", at(path), value, *n.minimum)
		}
		if n.maximum != nil && f > *n.maximum {
			return fmt.Errorf("%s: %s, want at most %v", at(path), value, *n.maximum)
		}
	}
	return nil
}

func (n *node) validateObject(obj map[string]any, path string) error {
	for _, name := range n.required {
		if _, ok := obj[name]; !ok {
			return fmt.Errorf("%s: the field %q is missing", at(path), name)
		}
	}
	if n.additional != nil && !*n.additional {
		var extra []string
		for name := range obj {
			if _, ok := n.properties[name]; !ok {
				extra = append(extra, name)
			}
		}
		if len(extra) > 0 {
			sort.Strings(extra)
			return fmt.Errorf("%s: no such field %q", at(path), boundedField(extra[0]))
		}
	}
	for _, name := range n.propOrder {
		v, ok := obj[name]
		if !ok {
			continue
		}
		if err := n.properties[name].validate(v, path+"/"+name); err != nil {
			return err
		}
	}
	return nil
}

func (n *node) validateArray(items []any, path string) error {
	if n.minItems != nil && len(items) < *n.minItems {
		return fmt.Errorf("%s: %d items, want at least %d", at(path), len(items), *n.minItems)
	}
	if n.maxItems != nil && len(items) > *n.maxItems {
		return fmt.Errorf("%s: %d items, want at most %d", at(path), len(items), *n.maxItems)
	}
	if n.items == nil {
		return nil
	}
	for i, item := range items {
		if err := n.items.validate(item, path+"/"+strconv.Itoa(i)); err != nil {
			return err
		}
	}
	return nil
}

func isType(v any, t string) bool {
	switch t {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "null":
		return v == nil
	case "number":
		_, ok := v.(json.Number)
		return ok
	case "integer":
		num, ok := v.(json.Number)
		if !ok {
			return false
		}
		if _, err := strconv.ParseInt(num.String(), 10, 64); err == nil {
			return true
		}
		f, err := num.Float64()
		return err == nil && f == math.Trunc(f)
	}
	return false
}

func boundedField(name string) string {
	const max = 64
	if len(name) <= max {
		return name
	}
	cut := 0
	for i := range name {
		if i > max {
			break
		}
		cut = i
	}
	return name[:cut] + "…"
}

func kindOf(v any) string {
	switch value := v.(type) {
	case map[string]any:
		return "an object"
	case []any:
		return "an array"
	case string:
		return "a string"
	case bool:
		return "a boolean"
	case nil:
		return "null"
	case json.Number:
		if isType(value, "integer") {
			return "an integer"
		}
		return "a number"
	}
	return "something else"
}

func equalJSON(raw json.RawMessage, v any) bool {
	answer, ok := canonicalValue(v)
	if !ok {
		return false
	}
	return string(canonicalJSON(raw)) == string(answer)
}

func canonicalJSON(raw json.RawMessage) json.RawMessage {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return raw
	}
	canonical, ok := canonicalValue(v)
	if !ok {
		return raw
	}
	return canonical
}

func canonicalValue(v any) (json.RawMessage, bool) {
	encoded, err := json.Marshal(canonicalNumbers(v))
	if err != nil {
		return nil, false
	}
	return encoded, true
}

func canonicalNumbers(v any) any {
	switch value := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(value))
		for k, item := range value {
			out[k] = canonicalNumbers(item)
		}
		return out
	case []any:
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = canonicalNumbers(item)
		}
		return out
	case json.Number:
		return json.Number(canonicalNumber(value))
	}
	return v
}

func canonicalNumber(n json.Number) string {
	if i, err := strconv.ParseInt(n.String(), 10, 64); err == nil {
		return strconv.FormatInt(i, 10)
	}
	f, err := n.Float64()
	if err != nil {
		return n.String()
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func enumList(enum []json.RawMessage) string {
	parts := make([]string, len(enum))
	for i, e := range enum {
		parts[i] = string(e)
	}
	return strings.Join(parts, ", ")
}

func in(path string) string {
	if path == "" {
		return "the schema"
	}
	return "the schema at " + path
}

func at(path string) string {
	if path == "" {
		return "the answer"
	}
	return path
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
