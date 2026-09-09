package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/telemetry"
)

// databaseFlag registers --database-url on a subcommand that needs Postgres and
// returns the function that resolves it once the flags are parsed: the flag
// where it was given, HEARSAY_DATABASE_URL otherwise. Prefer the variable — a
// URL on the command line carries its password into the process list, which is
// why the Dagger module passes it as a secret.
//
// The variable is deliberately not the flag's *default*: flag.PrintDefaults
// prints defaults, so `hearsay l0 --help` on a machine that has the variable set
// would print the password in it.
func databaseFlag(fs *flag.FlagSet, cfg *config.Config) func() {
	flagged := fs.String("database-url", "", "Postgres connection URL; also HEARSAY_DATABASE_URL")
	return func() {
		cfg.Database.URL = *flagged
		if cfg.Database.URL == "" {
			cfg.Database.URL = envOr("HEARSAY_DATABASE_URL", "")
		}
	}
}

// runMigrate applies the embedded goose migrations (ADR-0006). It is a job that
// runs and exits, never something a service does on startup: four containers
// start against one database in no particular order, so migrating is a
// deployment step of its own.
func runMigrate(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs, cfg, _ := newFlagSet("migrate", stderr)
	resolveDatabase := databaseFlag(fs, cfg)
	// `down` destroys what the migration held, so it takes a second word from
	// whoever runs it (ADR-0006). Recovery in production is a restore.
	iKnow := fs.Bool("i-know", false, "for down: yes, roll back the last migration and lose what it held")
	action, actionArgs, err := parseAction(fs, args, "up")
	if err != nil {
		return err
	}
	resolveDatabase()
	// Work out what to do before opening anything, so that a typo in the action
	// is a typo rather than a connection failure.
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

// migrateAction turns the action word and what follows it into the work to do.
// It is the one place the vocabulary lives.
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

// parseAction handles the flags-anywhere parsing every subcommand with an action
// word needs. `hearsay migrate --database-url x status`,
// `hearsay migrate status --database-url x` and
// `hearsay l0 get <id> --database-url x` are all things people type; flag stops
// at the first word that is not a flag, so the only way to see every flag is to
// go round again after each word. It returns the action — the first word, or
// fallback when there is none — and the words after it.
//
// A flag left unparsed would land in NArg and be reported as a stray argument,
// which is the wrong problem: the two actions that take an argument would say
// "takes one argument" about a command line that gave one.
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

// parseWords parses argv into the flag set and returns the positional words in
// order.
func parseWords(fs *flag.FlagSet, argv []string) ([]string, error) {
	var words []string
	for {
		// `--` ends the flags for good, so everything behind it is a word even
		// if it looks like one. Parse honours that for the run it is given;
		// looping past it would not.
		if len(argv) > 0 && argv[0] == "--" {
			return append(words, argv[1:]...), nil
		}
		if err := fs.Parse(argv); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return words, nil
		}
		words = append(words, fs.Arg(0))
		argv = fs.Args()[1:]
	}
}

// checkArgs rejects a word after an action that takes none, and any flag the
// action does not read.
func checkArgs(fs *flag.FlagSet, action string, args []string, reads ...string) error {
	if len(args) > 0 {
		return fmt.Errorf("unexpected argument %q: %s takes none", args[0], action)
	}
	return checkFlags(fs, action, reads...)
}

// commonFlags are the flags every subcommand has and every action of one uses:
// the logger's, and the database for the subcommands that open one. Everything
// else belongs to an action, and a flag set on an action that does not read it
// is an error rather than a no-op — a filter silently dropped is a wrong answer
// nobody has a reason to doubt.
var commonFlags = []string{"log-level", "log-format", "database-url"}

func checkFlags(fs *flag.FlagSet, action string, reads ...string) error {
	read := make(map[string]bool, len(commonFlags)+len(reads))
	for _, name := range append(commonFlags, reads...) {
		read[name] = true
	}
	// Visit walks the flags that were *set*, so a default nobody asked for —
	// --config's HEARSAY_CONFIG, for one — is not an error.
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

// report prints what a run applied and where the schema ended up. A run that
// applied nothing still says the version, so that `up` on a current database
// answers the question it was asked. Reading the version back never replaces
// the error that got us here.
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

// migrateForDev is the one place a service command migrates rather than
// verifying: `hearsay all` is local development only (ADR-0003), and ADR-0006
// has it run `migrate up` for itself because a local database is nobody's
// production. Every other command that reads the database verifies the schema
// and refuses one that is behind.
//
// A process with no database configured is one somebody is looking at, so this
// says there is nothing to migrate rather than refusing to start.
func migrateForDev(ctx context.Context, cfg *config.Config) error {
	log := telemetry.Logger(ctx)
	if cfg.Database.URL == "" {
		log.WarnContext(ctx, "no database: nothing is stored, pass --database-url")
		return nil
	}
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
