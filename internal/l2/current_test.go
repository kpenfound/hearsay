package l2_test

import (
	"testing"

	"github.com/kpenfound/hearsay/internal/l2"
)

func TestCurrent(t *testing.T) {
	st := func(id, doc, supersedes string) l2.Stance {
		return l2.Stance{ID: id, Evidence: []string{doc}, Supersedes: supersedes}
	}
	tests := []struct {
		name    string
		history []l2.Stance
		want    string
	}{
		{name: "no stance", history: nil, want: ""},
		{name: "the newest stated", history: []l2.Stance{st("a", "d1", ""), st("b", "d2", "a")}, want: "b"},
		{
			name:    "a late read forks behind the head",
			history: []l2.Stance{st("a", "d1", ""), st("late", "d3", "a"), st("b", "d2", "a")},
			want:    "b",
		},
		{
			name:    "a newer stance its own document retired",
			history: []l2.Stance{st("a", "d1", ""), st("again", "d2", "b"), st("b", "d2", "a")},
			want:    "again",
		},
		{
			name:    "superseded by another document is not retired",
			history: []l2.Stance{st("late", "d2", "b"), st("b", "d1", "")},
			want:    "b",
		},
		{
			// It cites the document but was not read from it.
			name: "superseded by an assertion citing its document is not retired",
			history: []l2.Stance{
				{ID: "asserted", Evidence: []string{"d1"}, Supersedes: "a", Assertion: "evt:hearsay:assertion:1"},
				st("a", "d1", ""),
			},
			want: "a",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := l2.Current(tt.history)
			if got.ID != tt.want || ok != (tt.want != "") {
				t.Errorf("Current() = %q, %v, want %q", got.ID, ok, tt.want)
			}
		})
	}
}
