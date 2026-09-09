// Package assertworker turns L1 documents into L2: entities, topics, stances
// and the edges between them. It runs on the `assert` model tier (ADR-0005) and
// is serialized per scope, because two workers deciding the current stance for
// one topic at the same time is how supersession chains get corrupted. The
// queue's serial_key is what enforces that (ADR-0007).
package assertworker

import (
	"context"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/service"
)

// Name is the subcommand this service runs as (ADR-0003).
const Name = "assert-worker"

// Deps are the dependencies the process builds and hands to Run: the database,
// the queue and the LLM tier registry, once they exist.
type Deps struct{}

// Run blocks until ctx is cancelled or the service fails.
func Run(ctx context.Context, cfg *config.Config, deps Deps) error {
	return service.Stub(ctx)
}
