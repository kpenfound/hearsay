// Package connectors hosts the source connectors. Connectors write L0 and
// nothing else: they turn a Slack thread, a GitHub webhook or a Drive change
// into events, and every layer above is someone else's job.
//
// One process can host several connectors; which ones it hosts is the
// `--source` selection (ADR-0003).
package connectors

import (
	"context"
	"strings"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/service"
	"github.com/kpenfound/hearsay/internal/telemetry"
)

// Name is the subcommand this service runs as (ADR-0003).
const Name = "connectors"

// Deps are the dependencies the process builds and hands to Run: the L0 store
// and the queue, once they exist.
type Deps struct {
	// Sources selects which configured connectors to host. Empty means all of
	// them, which is the default.
	Sources []string
}

// Run blocks until ctx is cancelled or the service fails.
func Run(ctx context.Context, cfg *config.Config, deps Deps) error {
	return service.Stub(telemetry.With(ctx, "source", Selection(deps.Sources)))
}

// Selection renders which connectors a process is hosting for the `source` log
// field: the names it was given, or "all" when it was given none. Every line
// this service logs carries it, because "which connectors is this container
// running" is the first question asked of a process hosting a subset.
func Selection(sources []string) string {
	if len(sources) == 0 {
		return "all"
	}
	return strings.Join(sources, ",")
}
