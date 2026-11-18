package claudesbx

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// agentcore attaches claude's own text to some of its warnings. The logger the
// runner is given keeps the warning and drops the text.
func TestRunnerLoggerDropsAttributesThatCanCarryText(t *testing.T) {
	const marker = "LEAK-MARKER"
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	l := quiet()
	l.Info("session start", "session", "distill", "dir", "/tmp/x")
	l.Warn("session diagnostic", "session", "distill", "diagnostic", marker+" a diagnostic")
	l.Warn("session output error", "session", "distill", "err", errors.New(marker+" a line of output"))
	l.Warn("something later agentcore adds", "session", "distill", "detail", marker+" unknown")
	l.With("err", errors.New(marker)).Warn("remove sandbox", "sandbox", "s1")
	l.WithGroup("g").Warn("grouped", "err", errors.New(marker), "sandbox", "s2")

	out := buf.String()
	if strings.Contains(out, marker) {
		t.Errorf("a record carries text:\n%s", out)
	}
	if strings.Contains(out, "session start") {
		t.Errorf("an INFO record was passed on:\n%s", out)
	}
	for _, want := range []string{"session diagnostic", "session output error", `"err_type":"*errors.errorString"`, `"sandbox":"s1"`, `"session":"distill"`} {
		if !strings.Contains(out, want) {
			t.Errorf("the log lacks %s:\n%s", want, out)
		}
	}
	if l.Handler().Enabled(context.Background(), slog.LevelInfo) {
		t.Error("INFO is enabled")
	}
}
