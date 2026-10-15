package l3

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/l2"
)

// NodeRef identifies a typed graph node.
type NodeRef struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// Query is a bounded sequence of typed graph steps.
type Query struct {
	Start []NodeRef `json:"start"`
	Steps []Step    `json:"steps"`
	Limit *int      `json:"limit,omitempty"`
}

// Step follows one edge, optionally transitively and with target filters.
type Step struct {
	Edge  string                     `json:"edge"`
	Depth string                     `json:"depth,omitempty"`
	Where map[string]json.RawMessage `json:"where,omitempty"`
}

// UnmarshalJSON keeps an omitted depth distinct from an explicitly malformed
// empty or null depth, and preserves the call decoder's unknown-field check.
func (s *Step) UnmarshalJSON(data []byte) error {
	var wire struct {
		Edge  string                     `json:"edge"`
		Depth json.RawMessage            `json:"depth"`
		Where map[string]json.RawMessage `json:"where"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wire); err != nil {
		return err
	}
	*s = Step{Edge: wire.Edge, Where: wire.Where}
	if len(wire.Depth) > 0 {
		if err := json.Unmarshal(wire.Depth, &s.Depth); err != nil || s.Depth == "" {
			return invalid("depth must be n or m..n, within 1..6")
		}
	}
	return nil
}

// ValidationError distinguishes malformed traversals from read failures.
type ValidationError struct{ Message string }

func (e *ValidationError) Error() string       { return e.Message }
func invalid(format string, args ...any) error { return &ValidationError{fmt.Sprintf(format, args...)} }

type edgeType struct {
	from, to, stored    string
	inverse, transitive bool
}

var edgeTypes = map[string]edgeType{
	"part_of":       {"entity", "entity", "part_of", false, true},
	"has_part":      {"entity", "entity", "part_of", true, true},
	"topics":        {"entity", "topic", "topics", false, false},
	"about":         {"topic", "entity", "topics", true, false},
	"documents":     {"entity", "document", "documents", false, false},
	"scope":         {"document", "entity", "documents", true, false},
	"anchors":       {"entity", "document", "anchors", false, false},
	"owners":        {"entity", "principal", "owners", false, false},
	"stances":       {"topic", "stance", "stances", false, false},
	"topic":         {"stance", "topic", "stances", true, false},
	"evidence":      {"stance", "document", "evidence", false, false},
	"cited_by":      {"document", "stance", "evidence", true, false},
	"supersedes":    {"stance", "stance", "supersedes", false, true},
	"superseded_by": {"stance", "stance", "supersedes", true, true},
	"author":        {"stance", "principal", "author", false, false},
	"participants":  {"document", "principal", "participants", false, false},
}

var depthPattern = regexp.MustCompile(`^[1-6](\.\.[1-6])?$`)

func (s Step) depths() (int, int, error) {
	d := s.Depth
	if d == "" {
		d = "1"
	}
	if !depthPattern.MatchString(d) {
		return 0, 0, invalid("depth must be n or m..n, within 1..6")
	}
	lo, hi, ranged := strings.Cut(d, "..")
	a, _ := strconv.Atoi(lo)
	b := a
	if ranged {
		b, _ = strconv.Atoi(hi)
	}
	if a > b {
		return 0, 0, invalid("depth range is reversed")
	}
	if b > 1 && !edgeTypes[s.Edge].transitive {
		return 0, 0, invalid("edge %q only allows depth 1", s.Edge)
	}
	return a, b, nil
}

// Validate checks the whole typed path before any node is read.
func (q Query) Validate() error {
	if len(q.Start) < 1 || len(q.Start) > 10 {
		return invalid("start must contain 1 to 10 nodes")
	}
	typ := q.Start[0].Type
	for _, n := range q.Start {
		if n.Type != "entity" && n.Type != "topic" && n.Type != "stance" && n.Type != "document" {
			return invalid("start type %q is not allowed", n.Type)
		}
		if n.Type != typ {
			return invalid("start nodes must have the same type")
		}
		if strings.TrimSpace(n.ID) == "" {
			return invalid("start id is required")
		}
	}
	if len(q.Steps) < 1 || len(q.Steps) > 4 {
		return invalid("steps must contain 1 to 4 steps")
	}
	for _, s := range q.Steps {
		edge, ok := edgeTypes[s.Edge]
		if !ok || edge.from != typ {
			return invalid("edge %q does not leave %s", s.Edge, typ)
		}
		if _, _, err := s.depths(); err != nil {
			return err
		}
		if err := validateWhere(edge.to, s.Where); err != nil {
			return err
		}
		typ = edge.to
	}
	if q.Limit != nil && (*q.Limit < 1 || *q.Limit > 200) {
		return invalid("limit must be 1 to 200")
	}
	return nil
}

func validateWhere(typ string, where map[string]json.RawMessage) error {
	allowed := map[string]string{"entity": " type state ", "document": " kind class state since ", "topic": " tier since ", "stance": " current since "}
	// Sorting also makes the first validation error deterministic.
	for _, key := range sortedWhere(where) {
		raw := where[key]
		if !strings.Contains(allowed[typ], " "+key+" ") {
			return invalid("where key %q does not apply to %s", key, typ)
		}
		if key == "current" {
			var b bool
			if string(raw) == "null" || json.Unmarshal(raw, &b) != nil {
				return invalid("where.current must be a bool")
			}
			continue
		}
		var value string
		if json.Unmarshal(raw, &value) != nil || value == "" {
			return invalid("where.%s must be a nonempty string", key)
		}
		ok := false
		switch key {
		case "type":
			ok = l2.EntityType(value).Valid()
		case "state":
			ok = connector.CoreState(value).Valid()
		case "kind":
			ok = l1.Kind(value).Valid()
		case "class":
			ok = config.ArtifactClass(value).Valid()
		case "tier":
			ok = l2.Tier(value).Valid()
		case "since":
			_, err := time.Parse(time.RFC3339, value)
			ok = err == nil
		}
		if !ok {
			return invalid("where.%s has an invalid value", key)
		}
	}
	return nil
}
