// Package distiller turns L0 events into L1 documents. It is a stateless,
// parallel, retryable worker on the `distill` model tier (ADR-0005): every job
// it runs can be run again, and re-running one for an event must produce the
// same document.
package distiller

import (
	"context"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/service"
)

// Name is the subcommand this service runs as (ADR-0003).
const Name = "distiller"

// Deps are the dependencies the process builds and hands to Run: the database,
// the queue and the LLM tier registry, once they exist.
type Deps struct{}

// Run blocks until ctx is cancelled or the service fails.
func Run(ctx context.Context, cfg *config.Config, deps Deps) error {
	return service.Stub(ctx)
}
