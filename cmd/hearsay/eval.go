package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/eval"
)

// runEval is `hearsay eval`: the evaluation metrics over a window
// (docs/design.md#evaluation), for an operator. It reads the database and
// writes nothing, and it prints aggregates and ids only — no L0 payload, L1
// text, stance position or topic name — so it reads everything and takes no
// principal. Standing is computed under the configured authority, which is
// why it needs --config.
func runEval(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs, cfg, configPath := newFlagSet("eval", stderr)
	resolveDatabase := databaseFlag(fs, cfg)
	since := fs.String("since", "", "start of the window, RFC3339; the default is the beginning")
	until := fs.String("until", "", "end of the window, RFC3339, exclusive; the default is now")
	scope := fs.String("scope", "", "only topics and operations in this topic scope, and bundles and next actions on this bundle scope; the default is every scope")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	words, err := parseWords(fs, args)
	if err != nil {
		return err
	}
	if len(words) > 0 {
		return fmt.Errorf("unexpected argument %q", words[0])
	}
	// Everything about the command line is settled before anything is read,
	// so a typo is a typo rather than a connection failure.
	opts := eval.Options{Scope: *scope, Until: time.Now().UTC()}
	if *since != "" {
		if opts.Since, err = time.Parse(time.RFC3339, *since); err != nil {
			return fmt.Errorf("--since %q is not an RFC3339 time", *since)
		}
	}
	if *until != "" {
		if opts.Until, err = time.Parse(time.RFC3339, *until); err != nil {
			return fmt.Errorf("--until %q is not an RFC3339 time", *until)
		}
	}
	if !opts.Since.IsZero() && !opts.Since.Before(opts.Until) {
		return fmt.Errorf("--since %s is not before --until %s", opts.Since.Format(time.RFC3339), opts.Until.Format(time.RFC3339))
	}
	if *configPath == "" {
		return errors.New("eval needs --config: standing is computed under the configured authority")
	}
	ctx, err = withLogger(ctx, "eval", cfg, stderr)
	if err != nil {
		return err
	}
	repo, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	resolveDatabase()
	pool, err := db.Connect(ctx, cfg.Database.URL)
	if err != nil {
		return err
	}
	defer pool.Close()
	report, err := eval.Compute(ctx, pool, repo.Authority, opts)
	if err != nil {
		return err
	}
	if *asJSON {
		return report.WriteJSON(stdout)
	}
	return report.WriteText(stdout)
}
