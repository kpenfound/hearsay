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
		name    string
		level   string
		format  string
		wantErr bool
		// wantJSON asserts the output parses as a JSON object.
		wantJSON bool
		// wantEmitted asserts an info line reaches the writer.
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

func TestLoggerFromContext(t *testing.T) {
	t.Run("missing logger falls back to the default", func(t *testing.T) {
		if got := Logger(context.Background()); got != slog.Default() {
			t.Errorf("Logger(background) = %p, want the default logger %p", got, slog.Default())
		}
	})

	t.Run("round trips", func(t *testing.T) {
		var buf bytes.Buffer
		want := slog.New(slog.NewJSONHandler(&buf, nil))
		if got := Logger(WithLogger(context.Background(), want)); got != want {
			t.Errorf("Logger(WithLogger(l)) = %p, want %p", got, want)
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
