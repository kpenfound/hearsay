package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/db"
)

func TestDistillSettings(t *testing.T) {
	for _, command := range []string{"distiller", "all"} {
		t.Run(command, func(t *testing.T) {
			t.Setenv("HEARSAY_DATABASE_URL", "")
			t.Setenv("HEARSAY_CONFIG", "")
			for _, tt := range []struct {
				name, quiet, max string
				args             []string
				want             string
			}{
				{name: "defaults"},
				{name: "default quiet is 30s", args: []string{"--distill-max-delay=29s"}, want: "must be at least"},
				{name: "default maximum is 5m", args: []string{"--distill-quiet=301s"}, want: "must be at least"},
				{name: "flags", args: []string{"--distill-quiet=1s", "--distill-max-delay=2s"}},
				{name: "equal periods", args: []string{"--distill-quiet=1s", "--distill-max-delay=1s"}},
				{name: "flags refuse max below quiet", args: []string{"--distill-quiet=2s", "--distill-max-delay=1s"}, want: "must be at least"},
				{name: "environment", quiet: "1s", max: "2s"},
				{name: "environment refuses max below quiet", quiet: "2s", max: "1s", want: "must be at least"},
				{name: "flag overrides quiet environment", quiet: "2s", max: "1s", args: []string{"--distill-quiet=500ms"}},
				{name: "flag overrides maximum environment", quiet: "2s", max: "1s", args: []string{"--distill-max-delay=3s"}},
				{name: "zero flag disables", args: []string{"--distill-quiet=0", "--distill-max-delay=0"}},
				{name: "zero environment disables", quiet: "0", max: "-1s"},
				{name: "bad quiet duration", quiet: "soon", want: "parsing --distill-quiet"},
				{name: "bad maximum duration", max: "soon", want: "parsing --distill-max-delay"},
				{name: "negative quiet", quiet: "-1s", want: "must be non-negative"},
			} {
				t.Run(tt.name, func(t *testing.T) {
					t.Setenv("HEARSAY_DISTILL_QUIET", tt.quiet)
					t.Setenv("HEARSAY_DISTILL_MAX_DELAY", tt.max)
					var out, stderr bytes.Buffer
					err := run(t.Context(), append([]string{command}, tt.args...), &out, &stderr)
					if tt.want != "" {
						if err == nil || !strings.Contains(err.Error(), tt.want) {
							t.Fatalf("run = %v, want %q", err, tt.want)
						}
					} else if !errors.Is(err, db.ErrNoDatabaseURL) {
						t.Fatalf("run = %v, want valid settings to reach database check", err)
					}
				})
			}
		})
	}
}
