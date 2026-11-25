package claudesbx_test

import (
	"bytes"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/llm/claudesbx/claudesbxtest"
	"github.com/kpenfound/hearsay/internal/telemetry"
)

func debugLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestAFailureLeaksNoResultTextToErrorsOrLogs(t *testing.T) {
	reset := time.Now().Add(time.Hour).Truncate(time.Second)
	for _, tt := range []struct {
		name   string
		stream string
		schema *llm.Schema
		retry  bool
	}{
		{"a failed turn", "failed_turn.jsonl", nil, false},
		{"no structured output, retries exhausted", "structured_retries_exhausted.jsonl", summarySchema, false},
		{"a usage limit", "usage_limit.jsonl", summarySchema, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			logs := debugLog(t)
			claudesbxtest.Install(t, fixture(t, tt.stream, "@RESETS_AT@", strconv.FormatInt(reset.Unix(), 10)))
			reg, err := registry(t, map[llm.Tier]llm.TierConfig{llm.TierDistill: {Model: "m", MaxRetries: -1}})
			if err != nil {
				t.Fatal(err)
			}
			c, _ := reg.Completer(llm.TierDistill)
			ctx := telemetry.WithLogger(t.Context(), slog.Default())
			_, err = c.Complete(ctx, request(tt.schema))
			if err == nil {
				t.Fatal("Complete() = nil, want an error")
			}
			if llm.Retryable(err) != tt.retry {
				t.Errorf("Retryable() = %v, want %v", llm.Retryable(err), tt.retry)
			}
			if strings.Contains(err.Error(), leak) {
				t.Errorf("the error carries result text: %q", err)
			}
			out := logs.String()
			if !strings.Contains(out, "claude-sbx call failed") {
				t.Fatalf("the failure was not logged at debug; log:\n%s", out)
			}
			if strings.Contains(out, leak) || strings.Contains(out, "session limit") || strings.Contains(out, "fell over") {
				t.Errorf("a log record carries result text:\n%s", out)
			}

			for _, want := range []string{`"tier":"distill"`, `"class":`, `"error_type":`} {
				if !strings.Contains(out, want) {
					t.Errorf("the failure's log line lacks %s:\n%s", want, out)
				}
			}
		})
	}
}
