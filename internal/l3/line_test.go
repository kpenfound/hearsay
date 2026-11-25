package l3_test

import (
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/l3"
)

func TestLine(t *testing.T) {
	long := strings.Repeat("é", l3.MaxLine)
	tests := []struct {
		name, in, want string
	}{
		{"the first line", "one\ntwo", "one"},
		{"blank lines are skipped", "\n  \n\tthree  words  here\n", "three words here"},
		{"nothing", "", ""},
		{"a long line is cut on a character boundary", long, strings.Repeat("é", (l3.MaxLine-len("…"))/2) + "…"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := l3.Line(tt.in)
			if got != tt.want {
				t.Errorf("Line(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if len(got) > l3.MaxLine {
				t.Errorf("Line is %d bytes, over %d", len(got), l3.MaxLine)
			}
		})
	}
}
