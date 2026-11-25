package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestNewLogger(t *testing.T) {
	tests := []struct {
		name        string
		level       string
		format      string
		wantErr     bool
		wantJSON    bool
		wantEmitted bool
	}{
		{name: "json format", level: "info", format: "json", wantJSON: true, wantEmitted: true},
		{name: "text format", level: "info", format: "text", wantEmitted: true},
		{name: "auto format on a non-terminal is json", level: "info", format: "auto", wantJSON: true, wantEmitted: true},
		{name: "zero value defaults to info json", level: "", format: "", wantJSON: true, wantEmitted: true},
		{name: "warn level drops info lines", level: "warn", format: "json"},
		{name: "unknown level", level: "chatty", format: "json", wantErr: true},
		{name: "unknown format", level: "info", format: "yaml", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			l, err := NewLogger(tt.level, tt.format, &buf)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("NewLogger(%q, %q) = nil error, want error", tt.level, tt.format)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewLogger(%q, %q) = %v, want no error", tt.level, tt.format, err)
			}
			l.Info("hello", "service", "api")

			if got := buf.Len() > 0; got != tt.wantEmitted {
				t.Fatalf("emitted = %v, want %v (output %q)", got, tt.wantEmitted, buf.String())
			}
			if !tt.wantEmitted {
				return
			}
			if tt.wantJSON {
				var rec map[string]any
				if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
					t.Fatalf("output is not JSON: %v (%q)", err, buf.String())
				}
				if rec["service"] != "api" {
					t.Errorf("service field = %v, want api", rec["service"])
				}
				return
			}
			if !strings.Contains(buf.String(), "service=api") {
				t.Errorf("output %q does not carry the service attribute", buf.String())
			}
		})
	}
}

func TestNewLoggerLevelBoundary(t *testing.T) {
	tests := []struct {
		level       string
		wantDebug   bool
		wantInfo    bool
		wantWarning bool
	}{
		{level: "debug", wantDebug: true, wantInfo: true, wantWarning: true},
		{level: "info", wantInfo: true, wantWarning: true},
		{level: "warn", wantWarning: true},
	}
	for _, tt := range tests {
		t.Run(tt.level, func(t *testing.T) {
			var buf bytes.Buffer
			l, err := NewLogger(tt.level, "text", &buf)
			if err != nil {
				t.Fatal(err)
			}
			l.Debug("at debug")
			l.Info("at info")
			l.Warn("at warn")
			out := buf.String()
			for msg, want := range map[string]bool{"at debug": tt.wantDebug, "at info": tt.wantInfo, "at warn": tt.wantWarning} {
				if got := strings.Contains(out, msg); got != want {
					t.Errorf("level %s: %q emitted = %v, want %v", tt.level, msg, got, want)
				}
			}
		})
	}
}

func TestLoggerFromContext(t *testing.T) {
	t.Run("missing logger falls back to the default", func(t *testing.T) {
		if got := Logger(context.Background()); got != slog.Default() {
			t.Errorf("Logger(background) = %p, want the default logger %p", got, slog.Default())
		}
	})

	t.Run("nil logger falls back to the default", func(t *testing.T) {
		ctx := WithLogger(context.Background(), nil)
		if got := Logger(ctx); got != slog.Default() {
			t.Errorf("Logger(WithLogger(nil)) = %p, want the default logger", got)
		}
	})

	t.Run("With attaches attributes to the carried logger", func(t *testing.T) {
		var buf bytes.Buffer
		ctx := WithLogger(context.Background(), slog.New(slog.NewJSONHandler(&buf, nil)))
		ctx = With(ctx, "service", "distiller")
		ctx = With(ctx, "job_kind", "distill")
		Logger(ctx).Info("done")

		var rec map[string]any
		if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &rec); err != nil {
			t.Fatalf("output is not JSON: %v (%q)", err, buf.String())
		}
		if rec["service"] != "distiller" || rec["job_kind"] != "distill" {
			t.Errorf("record = %v, want both attributes carried", rec)
		}
	})
}
