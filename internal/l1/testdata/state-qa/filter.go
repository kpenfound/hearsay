// This QA helper calls the exported ListFor surface against the QA database.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/principal"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()
	pool, err := db.Connect(ctx, os.Getenv("HEARSAY_DATABASE_URL"))
	if err != nil {
		return err
	}
	defer pool.Close()
	reader := l1.Reader{Effective: principal.Effective{Human: "qa", Grant: principal.Grant{Scopes: principal.SomeScopes("tracker:qa:ENG#QA-1")}}}
	for _, tt := range []struct {
		state string
		want  int
	}{{"started", 1}, {"completed", 0}} {
		docs, err := l1.New(pool).ListFor(ctx, reader, l1.ListOptions{Source: "qa", States: []string{tt.state}})
		if err != nil {
			return err
		}
		if len(docs) != tt.want {
			return fmt.Errorf("ListFor(%s) = %d documents, want %d", tt.state, len(docs), tt.want)
		}
		fmt.Printf("ListFor(%s): %d documents\n", tt.state, len(docs))
		if len(docs) > 0 && (docs[0].State == nil || docs[0].State.Native == nil || *docs[0].State.Native != "In review") {
			return fmt.Errorf("ListFor lost the native state")
		}
	}
	return nil
}
