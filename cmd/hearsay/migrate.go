package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/telemetry"
)

func databaseFlag(fs *flag.FlagSet, cfg *config.Config) func() {
	flagged := fs.String("database-url", "", "Postgres connection URL; also HEARSAY_DATABASE_URL")
	return func() {
		cfg.Database.URL = *flagged
		if cfg.Database.URL == "" {
			cfg.Database.URL = envOr("HEARSAY_DATABASE_URL", "")
		}
	}
}

func runMigrate(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs, cfg, _ := newFlagSet("migrate", stderr)
	resolveDatabase := databaseFlag(fs, cfg)
	iKnow := fs.Bool("i-know", false, "for down: yes, roll back the last migration and lose what it held")
	action, actionArgs, err := parseAction(fs, args, "up")
	if err != nil {
		return err
	}
	resolveDatabase()
	do, err := migrateAction(fs, action, actionArgs, iKnow)
	if err != nil {
		return err
	}

	ctx, err = withLogger(ctx, "migrate", cfg, stderr)
	if err != nil {
		return err
	}
	migrator, err := db.NewMigrator(ctx, cfg.Database.URL, telemetry.Logger(ctx))
	if err != nil {
		return err
	}
	defer func() { _ = migrator.Close() }()
	return do(ctx, migrator, stdout)
}

func migrateAction(fs *flag.FlagSet, action string, args []string, iKnow *bool) (func(context.Context, *db.Migrator, io.Writer) error, error) {
	switch action {
	case "up":
		if err := checkArgs(fs, action, args); err != nil {
			return nil, err
		}
		return func(ctx context.Context, m *db.Migrator, w io.Writer) error {
			applied, err := m.Up(ctx)
			return report(ctx, w, m, applied, err)
		}, nil

	case "up-to":
		if err := checkFlags(fs, action); err != nil {
			return nil, err
		}
		if len(args) != 1 {
			return nil, errors.New("up-to takes one argument: the migration version to stop at")
		}
		version, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a migration version: %w", args[0], err)
		}
		return func(ctx context.Context, m *db.Migrator, w io.Writer) error {
			applied, err := m.UpTo(ctx, version)
			return report(ctx, w, m, applied, err)
		}, nil

	case "down":
		if err := checkArgs(fs, action, args, "i-know"); err != nil {
			return nil, err
		}
		if !*iKnow {
			return nil, errors.New("down rolls back the last migration and loses what it held; pass --i-know if that is what you want")
		}
		return func(ctx context.Context, m *db.Migrator, w io.Writer) error {
			applied, err := m.Down(ctx)
			return report(ctx, w, m, applied, err)
		}, nil

	case "status":
		if err := checkArgs(fs, action, args); err != nil {
			return nil, err
		}
		return printStatus, nil

	default:
		return nil, fmt.Errorf("unknown action %q: want up, status, up-to or down", action)
	}
}

func parseAction(fs *flag.FlagSet, argv []string, fallback string) (action string, args []string, err error) {
	words, err := parseWords(fs, argv)
	if err != nil {
		return "", nil, err
	}
	if len(words) == 0 {
		return fallback, nil, nil
	}
	return words[0], words[1:], nil
}

func parseWords(fs *flag.FlagSet, argv []string) ([]string, error) {
	head, tail := argv, []string(nil)
	if i := slices.Index(argv, "--"); i >= 0 {
		head, tail = argv[:i], argv[i+1:]
	}
	var words []string
	for {
		if err := fs.Parse(head); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return append(words, tail...), nil
		}
		words = append(words, fs.Arg(0))
		head = fs.Args()[1:]
	}
}

func checkArgs(fs *flag.FlagSet, action string, args []string, reads ...string) error {
	if len(args) > 0 {
		return fmt.Errorf("unexpected argument %q: %s takes none", args[0], action)
	}
	return checkFlags(fs, action, reads...)
}

var commonFlags = []string{"log-level", "log-format", "database-url"}

func checkFlags(fs *flag.FlagSet, action string, reads ...string) error {
	read := make(map[string]bool, len(commonFlags)+len(reads))
	for _, name := range append(commonFlags, reads...) {
		read[name] = true
	}
	var ignored []string
	fs.Visit(func(f *flag.Flag) {
		if !read[f.Name] {
			ignored = append(ignored, "--"+f.Name)
		}
	})
	if len(ignored) > 0 {
		return fmt.Errorf("%s does not read %s", action, strings.Join(ignored, ", "))
	}
	return nil
}

func report(ctx context.Context, w io.Writer, migrator *db.Migrator, applied []db.Applied, err error) error {
	if len(applied) > 0 {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		for _, a := range applied {
			state := "ok"
			if a.Empty {
				state = "empty"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", state, a.Direction, a.Name, a.Duration.Round(time.Millisecond))
		}
		_ = tw.Flush()
	}
	if err != nil {
		return err
	}
	version, err := migrator.Version(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "schema version %d\n", version)
	return nil
}

func printStatus(ctx context.Context, migrator *db.Migrator, w io.Writer) error {
	statuses, err := migrator.Status(ctx)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprint(tw, "VERSION\tSTATE\tAPPLIED AT\tMIGRATION\n")
	for _, s := range statuses {
		state, appliedAt := "pending", ""
		if s.Applied {
			state, appliedAt = "applied", s.AppliedAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\n", s.Version, state, appliedAt, s.Name)
	}
	_ = tw.Flush()
	return nil
}

func migrateForDev(ctx context.Context, cfg *config.Config) error {
	log := telemetry.Logger(ctx)
	migrator, err := db.NewMigrator(ctx, cfg.Database.URL, log)
	if err != nil {
		return err
	}
	defer func() { _ = migrator.Close() }()
	applied, err := migrator.Up(ctx)
	if err != nil {
		return err
	}
	version, err := migrator.Version(ctx)
	if err != nil {
		return err
	}
	log.InfoContext(ctx, "schema is current", "migrations_applied", len(applied), "schema_version", version)
	return nil
}
