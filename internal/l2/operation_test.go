package l2_test

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/l2"
)

func TestOperationRequestValidate(t *testing.T) {
	tests := []struct {
		name string
		req  l2.OperationRequest
		want string // "" is valid
	}{
		{name: "merge", req: l2.OperationRequest{Kind: l2.OperationMerge, Principal: "kyle", Into: "a", From: "b"}},
		{name: "split", req: l2.OperationRequest{Kind: l2.OperationSplit, Principal: "kyle", Topic: "a", Name: "the other question", Stances: []string{"s1"}}},
		{name: "undo", req: l2.OperationRequest{Kind: l2.OperationUndo, Principal: "kyle", Undoes: 3}},
		{name: "no principal", req: l2.OperationRequest{Kind: l2.OperationMerge, Into: "a", From: "b"}, want: "no principal"},
		{name: "unknown kind", req: l2.OperationRequest{Kind: "rename", Principal: "kyle"}, want: "kind"},
		{name: "merge of one topic", req: l2.OperationRequest{Kind: l2.OperationMerge, Principal: "kyle", Into: "a"}, want: "two topics"},
		{name: "merge into itself", req: l2.OperationRequest{Kind: l2.OperationMerge, Principal: "kyle", Into: "a", From: "a"}, want: "into itself"},
		{name: "merge with stances", req: l2.OperationRequest{Kind: l2.OperationMerge, Principal: "kyle", Into: "a", From: "b", Stances: []string{"s1"}}, want: "only the topic"},
		{name: "split with no topic", req: l2.OperationRequest{Kind: l2.OperationSplit, Principal: "kyle", Name: "x", Stances: []string{"s1"}}, want: "no topic"},
		{name: "split with no name", req: l2.OperationRequest{Kind: l2.OperationSplit, Principal: "kyle", Topic: "a", Name: "  ", Stances: []string{"s1"}}, want: "name"},
		{name: "split with a long name", req: l2.OperationRequest{Kind: l2.OperationSplit, Principal: "kyle", Topic: "a", Name: strings.Repeat("n", l2.MaxTopicName+1), Stances: []string{"s1"}}, want: "name"},
		{name: "split of nothing", req: l2.OperationRequest{Kind: l2.OperationSplit, Principal: "kyle", Topic: "a", Name: "x"}, want: "moves no stance"},
		{name: "split naming a stance twice", req: l2.OperationRequest{Kind: l2.OperationSplit, Principal: "kyle", Topic: "a", Name: "x", Stances: []string{"s1", "s1"}}, want: "twice"},
		{name: "split with an empty stance", req: l2.OperationRequest{Kind: l2.OperationSplit, Principal: "kyle", Topic: "a", Name: "x", Stances: []string{""}}, want: "empty"},
		{name: "split with an undo", req: l2.OperationRequest{Kind: l2.OperationSplit, Principal: "kyle", Topic: "a", Name: "x", Stances: []string{"s1"}, Undoes: 1}, want: "only a topic"},
		{name: "undo of nothing", req: l2.OperationRequest{Kind: l2.OperationUndo, Principal: "kyle"}, want: "no operation"},
		{name: "undo with a topic", req: l2.OperationRequest{Kind: l2.OperationUndo, Principal: "kyle", Undoes: 1, Into: "a"}, want: "only the operation"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Validate()
			if tt.want == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, l2.ErrInvalid) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() = %v, want ErrInvalid saying %q", err, tt.want)
			}
		})
	}
}

// scope is a ledger's starting point: topics a (stances a1, a2), b (b1, b2)
// and c (c1).
func scope(ops ...l2.Operation) l2.ScopeState {
	return l2.ScopeState{
		Scope:      "eng",
		Topics:     []string{"a", "b", "c"},
		Stances:    map[string]string{"a1": "a", "a2": "a", "b1": "b", "b2": "b", "c1": "c"},
		Operations: ops,
	}
}

func merge(id int64, into, from string, stances ...string) l2.Operation {
	return l2.Operation{ID: id, Kind: l2.OperationMerge, Scope: "eng", Principal: "kyle", Topics: []string{into, from}, Stances: stances}
}

func split(id int64, topic, name string, stances ...string) l2.Operation {
	return l2.Operation{ID: id, Kind: l2.OperationSplit, Scope: "eng", Principal: "kyle", Name: name, Stances: stances,
		Topics: []string{topic, l2.SplitTopicID("eng", topic, name, stances)}}
}

func undone(op l2.Operation, by int64) l2.Operation {
	op.UndoneBy = by
	return op
}

func undo(id int64, of l2.Operation) l2.Operation {
	return l2.Operation{ID: id, Kind: l2.OperationUndo, Scope: "eng", Principal: "kyle", Topics: of.Topics, Stances: of.Stances, Undoes: of.ID}
}

func TestDecide(t *testing.T) {
	m1 := merge(1, "a", "b", "b1", "b2")
	s2 := split(2, "a", "the other lock", "b1")
	newTopic := s2.Topics[1]
	tests := []struct {
		name  string
		state l2.ScopeState
		req   l2.OperationRequest
		want  l2.Operation
		err   error
		// conflicting is the ids a ConflictError must name.
		conflicting []int64
		says        string
	}{
		{
			name:  "a merge covers every stance on the merged topic",
			state: scope(),
			req:   l2.OperationRequest{Kind: l2.OperationMerge, Principal: "kyle", Into: "a", From: "b"},
			want:  merge(0, "a", "b", "b1", "b2"),
		},
		{
			name:  "a merge of a topic from another scope",
			state: scope(),
			req:   l2.OperationRequest{Kind: l2.OperationMerge, Principal: "kyle", Into: "a", From: "elsewhere"},
			err:   l2.ErrInvalid, says: `not a topic in scope "eng"`,
		},
		{
			name:  "a topic merged away cannot be merged again",
			state: scope(m1),
			req:   l2.OperationRequest{Kind: l2.OperationMerge, Principal: "kyle", Into: "c", From: "b"},
			err:   l2.ErrInvalid, says: "merged away by operation 1",
		},
		{
			name:  "a merge after a merge carries the first merge's stances",
			state: scope(m1),
			req:   l2.OperationRequest{Kind: l2.OperationMerge, Principal: "kyle", Into: "c", From: "a"},
			want:  merge(0, "c", "a", "a1", "a2", "b1", "b2"),
		},
		{
			name:  "a topic whose merge was undone stands again",
			state: scope(undone(m1, 2), undo(2, m1)),
			req:   l2.OperationRequest{Kind: l2.OperationMerge, Principal: "kyle", Into: "c", From: "b"},
			want:  merge(0, "c", "b", "b1", "b2"),
		},
		{
			name:  "a split records the new topic's name and the stances it moves",
			state: scope(),
			req:   l2.OperationRequest{Kind: l2.OperationSplit, Principal: "kyle", Topic: "a", Name: "the other lock", Stances: []string{"a2"}},
			want: l2.Operation{Kind: l2.OperationSplit, Scope: "eng", Principal: "kyle", Name: "the other lock", Stances: []string{"a2"},
				Topics: []string{"a", l2.SplitTopicID("eng", "a", "the other lock", []string{"a2"})}},
		},
		{
			name:  "a split can take back part of a merge",
			state: scope(m1),
			req:   l2.OperationRequest{Kind: l2.OperationSplit, Principal: "kyle", Topic: "a", Name: "the other lock", Stances: []string{"b1"}},
			want:  split(0, "a", "the other lock", "b1"),
		},
		{
			name:  "a split of a stance on another topic",
			state: scope(),
			req:   l2.OperationRequest{Kind: l2.OperationSplit, Principal: "kyle", Topic: "a", Name: "x", Stances: []string{"b1"}},
			err:   l2.ErrInvalid, says: "stance b1 is not on topic a",
		},
		{
			name:  "a split that leaves the topic empty",
			state: scope(),
			req:   l2.OperationRequest{Kind: l2.OperationSplit, Principal: "kyle", Topic: "a", Name: "x", Stances: []string{"a1", "a2"}},
			err:   l2.ErrInvalid, says: "leaves it empty",
		},
		{
			name:  "a split of a split's new topic",
			state: scope(m1, s2),
			req:   l2.OperationRequest{Kind: l2.OperationMerge, Principal: "kyle", Into: "c", From: newTopic},
			want:  merge(0, "c", newTopic, "b1"),
		},
		{
			name:  "an undo repeats what it reverses",
			state: scope(m1),
			req:   l2.OperationRequest{Kind: l2.OperationUndo, Principal: "kyle", Undoes: 1},
			want:  undo(0, m1),
		},
		{
			name:  "an undo of an operation the scope does not hold",
			state: scope(m1),
			req:   l2.OperationRequest{Kind: l2.OperationUndo, Principal: "kyle", Undoes: 9},
			err:   l2.ErrNotFound,
		},
		{
			name:  "an undo of an undo",
			state: scope(undone(m1, 2), undo(2, m1)),
			req:   l2.OperationRequest{Kind: l2.OperationUndo, Principal: "kyle", Undoes: 2},
			err:   l2.ErrInvalid, says: "is an undo",
		},
		{
			name:  "an undo of what is already undone names the undo",
			state: scope(undone(m1, 2), undo(2, m1)),
			req:   l2.OperationRequest{Kind: l2.OperationUndo, Principal: "kyle", Undoes: 1},
			err:   l2.ErrConflict, conflicting: []int64{2}, says: "already undone by: operation 2",
		},
		{
			name:  "an undo under a later operation on its topics names it",
			state: scope(m1, s2, merge(3, "c", "a", "a1", "a2", "b2"), split(4, "c", "y", "c1")),
			req:   l2.OperationRequest{Kind: l2.OperationUndo, Principal: "kyle", Undoes: 1},
			err:   l2.ErrConflict, conflicting: []int64{2, 3}, says: "operation 2, 3",
		},
		{
			name:  "a later operation that was undone is not in the way",
			state: scope(m1, undone(s2, 3), undo(3, s2)),
			req:   l2.OperationRequest{Kind: l2.OperationUndo, Principal: "kyle", Undoes: 1},
			want:  undo(0, m1),
		},
		{
			name:  "a later operation on other topics is not in the way",
			state: scope(split(1, "a", "x", "a1"), merge(2, "c", "b", "b1", "b2")),
			req:   l2.OperationRequest{Kind: l2.OperationUndo, Principal: "kyle", Undoes: 1},
			want:  undo(0, split(1, "a", "x", "a1")),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.state.Decide(tt.req)
			if tt.err != nil {
				if !errors.Is(err, tt.err) || !strings.Contains(err.Error(), tt.says) {
					t.Fatalf("Decide() = %+v, %v, want %v saying %q", got, err, tt.err, tt.says)
				}
				var conflict *l2.ConflictError
				if tt.conflicting != nil && (!errors.As(err, &conflict) || !slices.Equal(conflict.Conflicting, tt.conflicting)) {
					t.Fatalf("Decide() = %v, want a conflict naming %v", err, tt.conflicting)
				}
				return
			}
			if err != nil {
				t.Fatalf("Decide() = %v, want %+v", err, tt.want)
			}
			if got.Kind != tt.want.Kind || got.Scope != tt.want.Scope || got.Principal != tt.want.Principal ||
				got.Name != tt.want.Name || got.Undoes != tt.want.Undoes ||
				!slices.Equal(got.Topics, tt.want.Topics) || !slices.Equal(got.Stances, tt.want.Stances) {
				t.Fatalf("Decide() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// withSplitRow is a state in which the split's topic has a row, with stance
// n1 written on it after the split.
func withSplitRow(s l2.ScopeState, of l2.Operation) l2.ScopeState {
	s.Topics = append(slices.Clone(s.Topics), of.Topics[1])
	s.Stances = maps.Clone(s.Stances)
	s.Stances["n1"] = of.Topics[1]
	return s
}

// A split's topic gets a row when a stance is first written on it after the
// split (Store.Target). The row is a topic only while a split creating it is
// in force; its stances are otherwise on the topic the split took from.
func TestDecideOverASplitsRow(t *testing.T) {
	s1 := split(1, "a", "the other lock", "a2")
	row := s1.Topics[1]
	tests := []struct {
		name  string
		state l2.ScopeState
		req   l2.OperationRequest
		want  l2.Operation
		says  string
	}{
		{
			name:  "the split's topic holds what it moved and what was written on it",
			state: withSplitRow(scope(s1), s1),
			req:   l2.OperationRequest{Kind: l2.OperationMerge, Principal: "kyle", Into: "c", From: row},
			want:  merge(0, "c", row, "a2", "n1"),
		},
		{
			name:  "undone, what was written on it is on the topic it split from",
			state: withSplitRow(scope(undone(s1, 2), undo(2, s1)), s1),
			req:   l2.OperationRequest{Kind: l2.OperationMerge, Principal: "kyle", Into: "c", From: "a"},
			want:  merge(0, "c", "a", "a1", "a2", "n1"),
		},
		{
			name:  "undone, the row is no topic",
			state: withSplitRow(scope(undone(s1, 2), undo(2, s1)), s1),
			req:   l2.OperationRequest{Kind: l2.OperationMerge, Principal: "kyle", Into: "c", From: row},
			says:  "is not a topic",
		},
		{
			name:  "undone after its source was merged away, it follows the merge",
			state: withSplitRow(scope(undone(s1, 2), undo(2, s1), merge(3, "b", "a", "a1", "a2")), s1),
			req:   l2.OperationRequest{Kind: l2.OperationMerge, Principal: "kyle", Into: "c", From: "b"},
			want:  merge(0, "c", "b", "a1", "a2", "b1", "b2", "n1"),
		},
		{
			name:  "made again, it holds what was written on it again",
			state: withSplitRow(scope(undone(s1, 2), undo(2, s1), split(3, "a", "the other lock", "a2")), s1),
			req:   l2.OperationRequest{Kind: l2.OperationMerge, Principal: "kyle", Into: "c", From: row},
			want:  merge(0, "c", row, "a2", "n1"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.state.Decide(tt.req)
			if tt.says != "" {
				if !errors.Is(err, l2.ErrInvalid) || !strings.Contains(err.Error(), tt.says) {
					t.Fatalf("Decide() = %+v, %v, want ErrInvalid saying %q", got, err, tt.says)
				}
				return
			}
			if err != nil || !slices.Equal(got.Topics, tt.want.Topics) || !slices.Equal(got.Stances, tt.want.Stances) {
				t.Fatalf("Decide() = %+v, %v, want %+v", got, err, tt.want)
			}
		})
	}
}

func TestSplitTopicIDIgnoresTheOrderOfStances(t *testing.T) {
	if l2.SplitTopicID("eng", "a", "x", []string{"s1", "s2"}) != l2.SplitTopicID("eng", "a", "x", []string{"s2", "s1"}) {
		t.Error("SplitTopicID depends on the order the stances were named in")
	}
	if l2.SplitTopicID("eng", "a", "x", []string{"s1"}) == l2.SplitTopicID("eng", "a", "y", []string{"s1"}) {
		t.Error("SplitTopicID is the same for two names")
	}
}

// loadRepo is a configuration with a scope `eng`, humans kyle and sam, an
// agent, and whatever authority is given.
func loadRepo(t *testing.T, authority string) config.Repo {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"sources/github.yaml": "id: github\ntype: github\ncontainers: [acme/api]\n",
		"scopes/eng.yaml":     "id: eng\nsources: [github]\n",
		"principals/p.yaml": "- id: kyle\n  identities: [{source: github, native_id: u1}]\n" +
			"- id: sam\n  identities: [{source: github, native_id: u2}]\n" +
			"- id: bot\n  kind: agent\n  class: orchestrator\n  scopes: ['*']\n  token_env: BOT_TOKEN\n  identities: [{source: github, native_id: u3}]\n",
	}
	if authority != "" {
		files["authority/a.yaml"] = authority
	}
	for name, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	repo, err := config.Load(dir)
	if err != nil {
		t.Fatalf("config.Load() = %v", err)
	}
	return repo
}

func TestCheckOperator(t *testing.T) {
	anyone := loadRepo(t, "")
	restricted := loadRepo(t, "scope: eng\nratified_by:\n  principals: [kyle]\n")
	tests := []struct {
		name  string
		repo  config.Repo
		id    string
		allow bool
	}{
		{name: "a human anyone may ratify for", repo: anyone, id: "sam", allow: true},
		{name: "an agent", repo: anyone, id: "bot"},
		{name: "a principal nobody configured", repo: anyone, id: "stranger"},
		{name: "nobody", repo: anyone, id: ""},
		{name: "a human named in ratified_by", repo: restricted, id: "kyle", allow: true},
		{name: "a human not named in ratified_by", repo: restricted, id: "sam"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := l2.CheckOperator(tt.repo, "eng", tt.id)
			if tt.allow != (err == nil) || (err != nil && !errors.Is(err, l2.ErrNotAllowed)) {
				t.Fatalf("CheckOperator(%q) = %v, want allowed %v", tt.id, err, tt.allow)
			}
		})
	}
}
