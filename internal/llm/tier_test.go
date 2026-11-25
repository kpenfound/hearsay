package llm_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/llm"
)

func TestParseTier(t *testing.T) {
	for _, tt := range []struct {
		name    string
		in      string
		want    llm.Tier
		wantErr bool
	}{
		{name: "distill", in: "distill", want: llm.TierDistill},
		{name: "assert", in: "assert", want: llm.TierAssert},
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
		})
	}
}

func TestOnlyTheCompletionTiersAreServedByACompleter(t *testing.T) {
	for tier, want := range map[llm.Tier]bool{llm.TierDistill: true, llm.TierAssert: true, llm.TierEmbed: false} {
		if got := tier.Completion(); got != want {
			t.Errorf("%q.Completion() = %v, want %v", tier, got, want)
		}
	}
}

func TestCheckDimensionsAcceptsTheWidthTheEmbedderHas(t *testing.T) {
	if err := llm.CheckDimensions(fixedEmbedder(1024), 1024); err != nil {
		t.Errorf("CheckDimensions(1024, 1024) = %v, want no error", err)
	}
}

func TestCheckDimensionsRefusesAnotherWidthAndNamesBoth(t *testing.T) {
	err := llm.CheckDimensions(fixedEmbedder(1024), 1536)
	if !errors.Is(err, llm.ErrDimensions) {
		t.Fatalf("CheckDimensions(1024, 1536) = %v, want ErrDimensions", err)
	}
	for _, want := range []string{"1024", "1536"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("CheckDimensions error = %q, want it to name %s", err, want)
		}
	}
}
