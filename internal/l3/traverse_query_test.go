package l3_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/l3"
)

func TestTraverseValidation(t *testing.T) {
	cases := []struct {
		name, request string
		valid         bool
	}{
		{"simple", `{"start":[{"type":"entity","id":"e"}],"steps":[{"edge":"topics"}]}`, true},
		{"no starts", `{"steps":[{"edge":"topics"}]}`, false},
		{"eleven starts", `{"start":[` + strings.TrimSuffix(strings.Repeat(`{"type":"entity","id":"e"},`, 11), ",") + `],"steps":[{"edge":"topics"}]}`, false},
		{"unknown type", `{"start":[{"type":"l0","id":"e"}],"steps":[{"edge":"topics"}]}`, false},
		{"principal start", `{"start":[{"type":"principal","id":"e"}],"steps":[{"edge":"topics"}]}`, false},
		{"missing id", `{"start":[{"type":"entity"}],"steps":[{"edge":"topics"}]}`, false},
		{"mixed starts", `{"start":[{"type":"entity","id":"e"},{"type":"topic","id":"t"}],"steps":[{"edge":"topics"}]}`, false},
		{"no steps", `{"start":[{"type":"entity","id":"e"}]}`, false},
		{"five steps", `{"start":[{"type":"entity","id":"e"}],"steps":[` + strings.TrimSuffix(strings.Repeat(`{"edge":"has_part"},`, 5), ",") + `]}`, false},
		{"missing edge", `{"start":[{"type":"entity","id":"e"}],"steps":[{}]}`, false},
		{"unknown edge", `{"start":[{"type":"entity","id":"e"}],"steps":[{"edge":"knows"}]}`, false},
		{"wrong origin", `{"start":[{"type":"entity","id":"e"}],"steps":[{"edge":"stances"}]}`, false},
		{"principal terminal", `{"start":[{"type":"entity","id":"e"}],"steps":[{"edge":"owners"},{"edge":"documents"}]}`, false},
		{"next step wrong", `{"start":[{"type":"entity","id":"e"}],"steps":[{"edge":"topics"},{"edge":"documents"}]}`, false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			var q l3.Query
			if err := json.Unmarshal([]byte(tt.request), &q); err != nil {
				t.Fatal(err)
			}
			checkValidation(t, q, tt.valid)
		})
	}
	for _, limit := range []int{-1, 0, 1, 50, 200, 201} {
		t.Run(fmt.Sprintf("limit %d", limit), func(t *testing.T) {
			q := l3.Query{Start: []l3.NodeRef{{Type: "entity", ID: "e"}}, Steps: []l3.Step{{Edge: "topics"}}, Limit: &limit}
			checkValidation(t, q, limit >= 1 && limit <= 200)
		})
	}
}
func checkValidation(t *testing.T, q l3.Query, valid bool) {
	t.Helper()
	err := q.Validate()
	if (err == nil) != valid {
		t.Fatalf("Validate = %v, valid=%v", err, valid)
	}
	if err != nil {
		var validation *l3.ValidationError
		if !errors.As(err, &validation) {
			t.Fatalf("not a ValidationError: %v", err)
		}
	}
}

func TestTraverseDepths(t *testing.T) {
	for _, edge := range []struct {
		name, from string
		transitive bool
	}{
		{"part_of", "entity", true}, {"has_part", "entity", true}, {"supersedes", "stance", true}, {"superseded_by", "stance", true},
		{"topics", "entity", false}, {"about", "topic", false}, {"documents", "entity", false}, {"scope", "document", false}, {"anchors", "entity", false}, {"owners", "entity", false}, {"stances", "topic", false}, {"topic", "stance", false}, {"evidence", "stance", false}, {"cited_by", "document", false}, {"author", "stance", false}, {"participants", "document", false},
	} {
		for _, depth := range []struct {
			value        string
			valid, multi bool
		}{{"", true, false}, {"1", true, false}, {"1..1", true, false}, {"2", true, true}, {"1..6", true, true}, {"6", true, true}, {"0", false, false}, {"7", false, false}, {"6..1", false, false}, {"1..7", false, false}, {"1.2", false, false}, {"+1", false, false}, {"01", false, false}, {"1..", false, false}, {"one", false, false}} {
			t.Run(edge.name+"/"+depth.value, func(t *testing.T) {
				q := l3.Query{Start: []l3.NodeRef{{Type: edge.from, ID: "x"}}, Steps: []l3.Step{{Edge: edge.name, Depth: depth.value}}}
				checkValidation(t, q, depth.valid && (!depth.multi || edge.transitive))
			})
		}
	}
}

func TestTraverseWhereKeysAndValues(t *testing.T) {
	types := []struct {
		target, from, edge string
		keys               map[string]string
	}{
		{"entity", "entity", "has_part", map[string]string{"type": `"tracker_item"`, "state": `"started"`}},
		{"document", "entity", "documents", map[string]string{"kind": `"issue"`, "class": `"issue"`, "state": `"completed"`, "since": `"2026-01-01T00:00:00Z"`}},
		{"topic", "entity", "topics", map[string]string{"tier": `"ratified"`, "since": `"2026-01-01T00:00:00Z"`}},
		{"stance", "topic", "stances", map[string]string{"current": `false`, "since": `"2026-01-01T00:00:00Z"`}},
		{"principal", "entity", "owners", map[string]string{}},
	}
	for _, typ := range types {
		for _, key := range []string{"type", "state", "kind", "class", "since", "tier", "current", "text"} {
			value, valid := typ.keys[key]
			if !valid {
				value = `"anything"`
			}
			for _, test := range []struct {
				name, raw string
				valid     bool
			}{{"valid", value, valid}, {"bad string", `"unknown"`, false}, {"number", `7`, false}, {"array", `[]`, false}, {"null", `null`, false}, {"object", `{}`, false}} {
				t.Run(typ.target+"/"+key+"/"+test.name, func(t *testing.T) {
					q := l3.Query{Start: []l3.NodeRef{{Type: typ.from, ID: "x"}}, Steps: []l3.Step{{Edge: typ.edge, Where: map[string]json.RawMessage{key: json.RawMessage(test.raw)}}}}
					checkValidation(t, q, test.valid)
				})
			}
		}
	}
}
