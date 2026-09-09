package service_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/service"
	"github.com/kpenfound/hearsay/internal/service/api"
	"github.com/kpenfound/hearsay/internal/service/assertworker"
	"github.com/kpenfound/hearsay/internal/service/connectors"
	"github.com/kpenfound/hearsay/internal/service/distiller"
)

// Every service must return when its context is cancelled: `hearsay all`
// composes them, so one that ignores cancellation hangs the dev process
// (ADR-0003).
func TestServicesStopOnContextCancellation(t *testing.T) {
	cfg := config.Default()
	tests := []struct {
		name string
		run  service.RunFunc
	}{
		{connectors.Name, func(ctx context.Context) error { return connectors.Run(ctx, &cfg, connectors.Deps{}) }},
		{distiller.Name, func(ctx context.Context) error { return distiller.Run(ctx, &cfg, distiller.Deps{}) }},
		{assertworker.Name, func(ctx context.Context) error { return assertworker.Run(ctx, &cfg, assertworker.Deps{}) }},
		{api.Name, func(ctx context.Context) error { return api.Run(ctx, &cfg, api.Deps{}) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan error, 1)
			go func() { done <- tt.run(ctx) }()
			cancel()

			select {
			case err := <-done:
				if err != nil {
					t.Errorf("Run() after cancellation = %v, want nil", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Run() did not return within 5s of cancellation")
			}
		})
	}
}

func TestRunAll(t *testing.T) {
	t.Run("runs every service and returns when the context is cancelled", func(t *testing.T) {
		var started atomic.Int64
		fns := map[string]service.RunFunc{}
		for _, name := range []string{"a", "b", "c"} {
			fns[name] = func(ctx context.Context) error {
				started.Add(1)
				<-ctx.Done()
				return nil
			}
		}

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- service.RunAll(ctx, fns) }()
		cancel()

		if err := waitFor(t, done); err != nil {
			t.Errorf("RunAll() = %v, want nil", err)
		}
		if got := started.Load(); got != 3 {
			t.Errorf("started %d services, want 3", got)
		}
	})

	t.Run("one service returning stops the others", func(t *testing.T) {
		boom := errors.New("boom")
		stopped := make(chan struct{})
		fns := map[string]service.RunFunc{
			"failing": func(ctx context.Context) error { return boom },
			"patient": func(ctx context.Context) error {
				<-ctx.Done()
				close(stopped)
				return nil
			},
		}

		err := waitForFunc(t, func() error { return service.RunAll(t.Context(), fns) })
		if !errors.Is(err, boom) {
			t.Errorf("RunAll() = %v, want it to carry %v", err, boom)
		}
		select {
		case <-stopped:
		default:
			t.Error("the other service was not cancelled")
		}
	})

	t.Run("collects every error", func(t *testing.T) {
		first, second := errors.New("first"), errors.New("second")
		fns := map[string]service.RunFunc{
			"one": func(ctx context.Context) error { return first },
			"two": func(ctx context.Context) error { <-ctx.Done(); return second },
		}

		err := waitForFunc(t, func() error { return service.RunAll(t.Context(), fns) })
		if !errors.Is(err, first) || !errors.Is(err, second) {
			t.Errorf("RunAll() = %v, want it to carry both %v and %v", err, first, second)
		}
	})

	t.Run("no services", func(t *testing.T) {
		if err := service.RunAll(t.Context(), nil); err != nil {
			t.Errorf("RunAll(nil) = %v, want nil", err)
		}
	})
}

func waitForFunc(t *testing.T, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	return waitFor(t, done)
}

func waitFor(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("RunAll() did not return within 5s")
		return nil
	}
}
