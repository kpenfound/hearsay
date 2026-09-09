// Command hearsay is the single binary Hearsay ships. Each of the four
// services is a subcommand of it (ADR-0003); `hearsay help` lists them.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "hearsay: %v\n", err)
		os.Exit(1)
	}
}
