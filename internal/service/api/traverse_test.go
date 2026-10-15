package api_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/principal"
	"github.com/kpenfound/hearsay/internal/service/api"
)

// Invalid paths must be rejected before any store read. All-scopes reach
// needs no database, so a nil DB makes accidental reads fail this unit test.
func TestTraverseInvalidCallsAreBadRequests(t *testing.T) {
	t.Setenv("HEARSAY_TRAVERSE_READER", "test-reader-credential")
	calls, err := api.NewCalls(nil, config.Repo{Principals: []principal.Principal{{ID: "reader", Kind: principal.KindHuman, TokenEnv: "HEARSAY_TRAVERSE_READER", Grant: principal.Grant{Scopes: principal.AllScopes()}}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, raw string }{
		{"empty", `{}`}, {"null", `null`}, {"no starts", `{"start":[],"steps":[{"edge":"topics"}]}`},
		{"too many starts", `{"start":[` + strings.TrimSuffix(strings.Repeat(`{"type":"entity","id":"e"},`, 11), ",") + `],"steps":[{"edge":"topics"}]}`},
		{"unknown type", `{"start":[{"type":"event","id":"e"}],"steps":[{"edge":"topics"}]}`},
		{"principal", `{"start":[{"type":"principal","id":"e"}],"steps":[{"edge":"topics"}]}`},
		{"mixed", `{"start":[{"type":"entity","id":"e"},{"type":"topic","id":"t"}],"steps":[{"edge":"topics"}]}`},
		{"empty id", `{"start":[{"type":"entity","id":""}],"steps":[{"edge":"topics"}]}`},
		{"no steps", `{"start":[{"type":"entity","id":"e"}],"steps":[]}`},
		{"too many steps", `{"start":[{"type":"entity","id":"e"}],"steps":[` + strings.TrimSuffix(strings.Repeat(`{"edge":"has_part"},`, 5), ",") + `]}`},
		{"missing edge", `{"start":[{"type":"entity","id":"e"}],"steps":[{}]}`},
		{"unknown edge", `{"start":[{"type":"entity","id":"e"}],"steps":[{"edge":"unknown"}]}`},
		{"wrong edge", `{"start":[{"type":"entity","id":"e"}],"steps":[{"edge":"stances"}]}`},
		{"principal terminal", `{"start":[{"type":"entity","id":"e"}],"steps":[{"edge":"owners"},{"edge":"documents"}]}`},
		{"nontransitive depth", `{"start":[{"type":"entity","id":"e"}],"steps":[{"edge":"topics","depth":"2"}]}`},
		{"depth over six", `{"start":[{"type":"entity","id":"e"}],"steps":[{"edge":"has_part","depth":"7"}]}`},
		{"reversed range", `{"start":[{"type":"entity","id":"e"}],"steps":[{"edge":"has_part","depth":"3..1"}]}`},
		{"depth malformed", `{"start":[{"type":"entity","id":"e"}],"steps":[{"edge":"has_part","depth":"many"}]}`},
		{"empty depth", `{"start":[{"type":"entity","id":"e"}],"steps":[{"edge":"has_part","depth":""}]}`},
		{"null depth", `{"start":[{"type":"entity","id":"e"}],"steps":[{"edge":"has_part","depth":null}]}`},
		{"unknown step field", `{"start":[{"type":"entity","id":"e"}],"steps":[{"edge":"has_part","query":"x"}]}`},
		{"depth numeric", `{"start":[{"type":"entity","id":"e"}],"steps":[{"edge":"has_part","depth":2}]}`},
		{"unknown top field", `{"start":[],"steps":[],"query":"x"}`},
	}
	base := `{"start":[{"type":"entity","id":"e"}],"steps":[{"edge":"topics"}],"limit":%s}`
	for _, limit := range []string{"0", "-1", "201", "1.5", "\"one\""} {
		cases = append(cases, struct{ name, raw string }{"limit " + limit, fmt.Sprintf(base, limit)})
	}
	for _, tt := range []struct{ edge, key, value string }{
		{"has_part", "tier", `"inferred"`}, {"has_part", "type", `"unknown"`}, {"has_part", "state", `"unknown"`},
		{"documents", "kind", `"unknown"`}, {"documents", "class", `"unknown"`}, {"documents", "state", `"unknown"`}, {"documents", "since", `"yesterday"`},
		{"topics", "tier", `"unknown"`}, {"topics", "since", `false`}, {"topics", "state", `"started"`}, {"owners", "name", `"kyle"`},
	} {
		cases = append(cases, struct{ name, raw string }{tt.edge + "/" + tt.key, fmt.Sprintf(`{"start":[{"type":"entity","id":"e"}],"steps":[{"edge":%q,"where":{%q:%s}}]}`, tt.edge, tt.key, tt.value)})
	}
	cases = append(cases, struct{ name, raw string }{"stance current", `{"start":[{"type":"topic","id":"t"}],"steps":[{"edge":"stances","where":{"current":"yes"}}]}`})
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, err := calls.Call(t.Context(), api.Caller{Principal: "reader"}, "traverse", json.RawMessage(tt.raw))
			var apiErr *api.Error
			if !errors.As(err, &apiErr) || apiErr.Status != 400 {
				t.Fatalf("Call = %v, want 400", err)
			}
		})
	}
	tools := api.Tools()
	names := []string{}
	for _, tool := range tools {
		names = append(names, tool.Name)
		if tool.Name == "traverse" {
			var schema map[string]any
			if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
				t.Fatal(err)
			}
			for _, word := range []string{"get_bundle", "stance_history", "get_l1", "ledger", "every hop", "line"} {
				if !strings.Contains(tool.Description, word) {
					t.Errorf("tool description lacks %q", word)
				}
			}
		}
	}
	if len(names)-len(slices.Compact(slices.Clone(names))) != 0 || !slices.Contains(names, "traverse") {
		t.Fatalf("tools=%v", names)
	}
}
