// Package api serves the read and assert API over MCP and HTTP: bundle
// assembly, the handles a consumer follows, and the audit trail. Reads are
// structured lookups — there is no model in the read path.
package api

import (
	"context"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/service"
)

// Name is the subcommand this service runs as (ADR-0003).
const Name = "api"

// Deps are the dependencies the process builds and hands to Run: the database
// and the bundle assembler, once they exist.
type Deps struct{}

// Run blocks until ctx is cancelled or the service fails.
func Run(ctx context.Context, cfg *config.Config, deps Deps) error {
	return service.Stub(ctx)
}
