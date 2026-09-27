package llm_test

import (
	"errors"
	"testing"

	"github.com/kpenfound/hearsay/internal/llm"
)

func TestParseTier(t *testing.T) {
	for _, tt := range []struct {
		name       string
		in         string
		want       llm.Tier
		completion bool
		wantErr    bool
	}{
		{name: "distill", in: "distill", want: llm.TierDistill, completion: true},
		{name: "assert", in: "assert", want: llm.TierAssert, completion: true},
		{name: "embed", in: "embed", want: llm.TierEmbed},
		{name: "unknown", in: "summarize", wantErr: true},
		{name: "empty", in: "", wantErr: true},
		{name: "case matters", in: "Distill", wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := llm.ParseTier(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseTier(%q) = %v, want an error", tt.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTier(%q) = %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("ParseTier(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if got.Completion() != tt.completion {
				t.Errorf("%q.Completion() = %v, want %v", got, got.Completion(), tt.completion)
			}
		})
	}
}

func TestCheckDimensions(t *testing.T) {
	e := fixedEmbedder(1024)
	if err := llm.CheckDimensions(e, 1024); err != nil {
		t.Errorf("CheckDimensions(1024, 1024) = %v, want no error", err)
	}
	err := llm.CheckDimensions(e, 1536)
	if !errors.Is(err, llm.ErrDimensions) {
		t.Fatalf("CheckDimensions(1024, 1536) = %v, want ErrDimensions", err)
	}
	// The message has to say which is which: the fix is either a migration or
	// a configuration edit, and they are not the same fix.
	for _, want := range []string{"1024", "1536"} {
		if !contains(err.Error(), want) {
			t.Errorf("CheckDimensions error = %q, want it to name %s", err, want)
		}
	}
}
