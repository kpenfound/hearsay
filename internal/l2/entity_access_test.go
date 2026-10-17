package l2_test

import (
	"testing"

	"github.com/kpenfound/hearsay/internal/l2"
)

func TestWorkEntityVisibilityTreatsMissingAsHidden(t *testing.T) {
	visibility := l2.WorkEntityVisibility{"work:projects:readable": false, "work:projects:private": true, "work:projects:configured": false}
	for _, tt := range []struct {
		name, id string
		want     bool
	}{
		{"readable source entity", "work:projects:readable", false},
		{"unreadable source entity", "work:projects:private", true},
		{"missing work entity", "work:projects:missing", true},
		{"other origin in work namespace", "work:projects:configured", false},
		{"other namespaces retain behavior", "code:missing", false},
		{"optional scope omitted", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := visibility.Hidden(tt.id); got != tt.want {
				t.Errorf("Hidden(%q)=%v,want %v", tt.id, got, tt.want)
			}
		})
	}
}
