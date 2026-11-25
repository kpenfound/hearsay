//go:build integration

package distiller_test

import (
	"context"
	"net"
	"testing"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/db/dbtest"
	"github.com/kpenfound/hearsay/internal/service/distiller"
	"github.com/kpenfound/hearsay/internal/service/servicetest"
)

func TestProbes(t *testing.T) {
	pool := dbtest.Pool(t)
	servicetest.Probes(t, pool, func(ctx context.Context, listener net.Listener) error {
		return distiller.Run(ctx, &config.Config{}, distiller.Deps{Pool: pool, Listener: listener})
	})
}
