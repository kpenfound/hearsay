// Package telemetry builds the process logger and, later, the OpenTelemetry
// trace and metric providers described in ADR-0008.
package telemetry

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/kpenfound/hearsay/internal/config"
)

type loggerKey struct{}

// WithLogger returns a context carrying l. Work that descends from ctx should
// enrich the logger rather than pass fields down by hand (ADR-0008).
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, l)
}

// Logger returns the logger carried by ctx, or the default logger if there is
// none. It never returns nil.
func Logger(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.Default()
}

// With returns a context whose logger has the given attributes attached.
func With(ctx context.Context, args ...any) context.Context {
	return WithLogger(ctx, Logger(ctx).With(args...))
}

// NewLogger builds the process logger from cfg, writing to w.
//
// The caller attaches the fields that hold for the whole process — service,
// version, instance — and puts the result in the context with [WithLogger].
func NewLogger(cfg config.Log, w io.Writer) (*slog.Logger, error) {
	level, err := parseLevel(cfg.Level)
	if err != nil {
		return nil, err
	}
	opts := &slog.HandlerOptions{Level: level}

	var h slog.Handler
	switch strings.ToLower(strings.TrimSpace(cfg.Format)) {
	case "json":
		h = slog.NewJSONHandler(w, opts)
	case "text":
		h = slog.NewTextHandler(w, opts)
	case "", "auto":
		if isTerminal(w) {
			h = slog.NewTextHandler(w, opts)
		} else {
			h = slog.NewJSONHandler(w, opts)
		}
	default:
		return nil, fmt.Errorf("unknown log format %q: want json, text or auto", cfg.Format)
	}
	return slog.New(h), nil
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "", "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown log level %q: want debug, info, warn or error", s)
	}
}

// isTerminal reports whether w is a character device, which is as close as the
// standard library gets to asking whether a human is reading.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
