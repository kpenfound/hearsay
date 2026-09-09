// Package service holds the four long-running processes Hearsay ships as
// subcommands of one binary (ADR-0003): connectors, distiller, assertworker and
// api. Each subpackage exposes the same entry point:
//
//	func Run(ctx context.Context, cfg *config.Config, deps Deps) error
//
// This package holds only what is common to all four: running a group of them
// in one process for `hearsay all`, and the placeholder loop the stubs use
// until they are built.
package service

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync"

	"github.com/kpenfound/hearsay/internal/telemetry"
)

// RunFunc is a service already bound to its config and dependencies.
type RunFunc func(ctx context.Context) error

// RunAll runs every service in fns concurrently and blocks until they have all
// returned. The first service to return — with an error or without one — stops
// the others: a process that is running four services is not useful with three.
//
// It is what `hearsay all` is built from, and it is deliberately the only way
// that subcommand differs from running the four services separately, so the dev
// path cannot drift from the deployed one.
//
// ADR-0003 says `all` "calls all four in one errgroup". This is a WaitGroup and
// [errors.Join] instead, which keeps the module free of dependencies, and the
// semantics differ on purpose: an errgroup cancels on the first *error*, and a
// service of ours returning nil is just as much a reason to stop as one
// returning an error. Every error is reported rather than only the first.
func RunAll(ctx context.Context, fns map[string]RunFunc) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	names := slices.Sorted(maps.Keys(fns))
	errs := make([]error, len(names))

	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer cancel()
			errs[i] = fns[name](telemetry.With(ctx, "service", name))
		}()
	}
	wg.Wait()

	return errors.Join(errs...)
}

// Stub blocks until ctx is cancelled. It stands in for a service that has not
// been built yet, so that `hearsay <service>` and `hearsay all` already have
// the shutdown behaviour the real services must have.
//
// It names no service: the caller has already put one on the context's logger,
// and repeating it here is how a log line ends up with the field twice.
func Stub(ctx context.Context) error {
	log := telemetry.Logger(ctx)
	log.WarnContext(ctx, "service is a stub and does no work yet")
	<-ctx.Done()
	log.InfoContext(ctx, "service stopped")
	return nil
}
