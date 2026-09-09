package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/service"
	"github.com/kpenfound/hearsay/internal/service/api"
	"github.com/kpenfound/hearsay/internal/service/assertworker"
	"github.com/kpenfound/hearsay/internal/service/connectors"
	"github.com/kpenfound/hearsay/internal/service/distiller"
	"github.com/kpenfound/hearsay/internal/telemetry"
	"github.com/kpenfound/hearsay/internal/version"
)

// errNotImplemented is what a subcommand returns while it is still a stub, so
// that running it fails loudly rather than looking like it did something.
var errNotImplemented = errors.New("not implemented yet")

// command is one hearsay subcommand.
type command struct {
	name    string
	args    string // argument summary for the usage line
	summary string
	run     func(ctx context.Context, args []string, stdout, stderr io.Writer) error
}

// commands is the whole command surface, in the order `hearsay help` prints it.
// The four service names are fixed by ADR-0003 and are a contract with the
// Dagger module and the deployment manifests; do not rename them casually.
func commands() []command {
	return []command{
		{connectors.Name, "[--source name]...", "Run the source connectors. Writes L0 only.", runConnectors},
		{distiller.Name, "", "Run the distiller. L0 to L1.", runService(distiller.Name, func(ctx context.Context, cfg *config.Config) error {
			return distiller.Run(ctx, cfg, distiller.Deps{})
		})},
		{assertworker.Name, "", "Run the assertion worker. L1 to L2.", runService(assertworker.Name, func(ctx context.Context, cfg *config.Config) error {
			return assertworker.Run(ctx, cfg, assertworker.Deps{})
		})},
		{api.Name, "", "Run the read and assert API over MCP and HTTP.", runService(api.Name, func(ctx context.Context, cfg *config.Config) error {
			return api.Run(ctx, cfg, api.Deps{})
		})},
		{"all", "", "Run all four services in one process. Local development only.", runAll},
		{"migrate", "up|status|up-to <n>|down", "Apply schema migrations and exit.", runMigrate},
		{"version", "", "Print version, commit and build date.", runVersion},
		{"help", "", "Print this message.", runHelp},
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		usage(stderr)
		return errors.New("no subcommand given")
	}

	name := args[0]
	if name == "-h" || name == "--help" {
		name = "help"
	}
	if name == "--version" {
		name = "version"
	}

	for _, cmd := range commands() {
		if cmd.name != name {
			continue
		}
		if err := cmd.run(ctx, args[1:], stdout, stderr); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil
			}
			return fmt.Errorf("%s: %w", cmd.name, err)
		}
		return nil
	}

	usage(stderr)
	return fmt.Errorf("unknown subcommand %q", args[0])
}

func usage(w io.Writer) {
	fmt.Fprint(w, "hearsay is a context platform for teams using coding agents.\n\nUsage:\n\n  hearsay <subcommand> [flags]\n\nSubcommands:\n\n")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, cmd := range commands() {
		fmt.Fprintf(tw, "  %s\t%s\n", strings.TrimSpace(cmd.name+" "+cmd.args), cmd.summary)
	}
	_ = tw.Flush()
	fmt.Fprint(w, "\nRun `hearsay <subcommand> --help` for the flags a subcommand takes.\nSee docs/design.md for what any of this means.\n")
}

// newFlagSet builds a flag set for a subcommand, with the flags every
// subcommand shares already registered on it. The returned config carries the
// defaults, overridden by the environment; parsing args overrides both.
func newFlagSet(name string, w io.Writer) (*flag.FlagSet, *config.Config) {
	fs := flag.NewFlagSet("hearsay "+name, flag.ContinueOnError)
	fs.SetOutput(w)

	cfg := config.Default()
	cfg.Log.Level = envOr("HEARSAY_LOG_LEVEL", cfg.Log.Level)
	cfg.Log.Format = envOr("HEARSAY_LOG_FORMAT", cfg.Log.Format)
	fs.StringVar(&cfg.Log.Level, "log-level", cfg.Log.Level, "log level: debug, info, warn or error")
	fs.StringVar(&cfg.Log.Format, "log-format", cfg.Log.Format, "log format: json, text or auto")
	return fs, &cfg
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

// withLogger builds the process logger from cfg and attaches it to ctx, along
// with the fields that hold for the whole process (ADR-0008).
//
// An empty service name leaves the `service` field off, which is what
// `hearsay all` needs: it runs four of them, and each gets its own name from
// [service.RunAll]. Setting it here as well would put the field on every line
// twice.
func withLogger(ctx context.Context, serviceName string, cfg *config.Config, stderr io.Writer) (context.Context, error) {
	log, err := telemetry.NewLogger(cfg.Log, stderr)
	if err != nil {
		return ctx, err
	}
	log = log.With("version", version.Info().Version, "instance", instanceName())
	if serviceName != "" {
		log = log.With("service", serviceName)
	}
	return telemetry.WithLogger(ctx, log), nil
}

// instanceName is what this process reports as `instance`: which replica of a
// service a line came from, so two containers running `hearsay distiller` can be
// told apart. The hostname is that identity in every container runtime worth
// naming; HEARSAY_INSTANCE overrides it for anything that knows better.
func instanceName() string {
	if v := envOr("HEARSAY_INSTANCE", ""); v != "" {
		return v
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "unknown"
}

// runService returns the handler for a subcommand that runs a long-lived
// service: parse the common flags, build the logger, hand over to start.
func runService(name string, start func(ctx context.Context, cfg *config.Config) error) func(context.Context, []string, io.Writer, io.Writer) error {
	return func(ctx context.Context, args []string, stdout, stderr io.Writer) error {
		fs, cfg := newFlagSet(name, stderr)
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() > 0 {
			return fmt.Errorf("unexpected argument %q", fs.Arg(0))
		}
		ctx, err := withLogger(ctx, name, cfg, stderr)
		if err != nil {
			return err
		}
		return start(ctx, cfg)
	}
}

func runConnectors(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs, cfg := newFlagSet(connectors.Name, stderr)
	var sources sourceList
	fs.Var(&sources, "source", "run only this configured connector; repeat the flag for several. The default is all of them.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	ctx, err := withLogger(ctx, connectors.Name, cfg, stderr)
	if err != nil {
		return err
	}
	return connectors.Run(ctx, cfg, connectors.Deps{Sources: sources})
}

// runAll runs the four services in one process. It exists so that evaluating
// Hearsay does not need four terminals; it is never a deployment target
// (ADR-0003).
func runAll(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs, cfg := newFlagSet("all", stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	// No service name on the process logger: RunAll names each of the four.
	ctx, err := withLogger(ctx, "", cfg, stderr)
	if err != nil {
		return err
	}

	return service.RunAll(ctx, map[string]service.RunFunc{
		connectors.Name: func(ctx context.Context) error {
			return connectors.Run(ctx, cfg, connectors.Deps{})
		},
		distiller.Name: func(ctx context.Context) error {
			return distiller.Run(ctx, cfg, distiller.Deps{})
		},
		assertworker.Name: func(ctx context.Context) error {
			return assertworker.Run(ctx, cfg, assertworker.Deps{})
		},
		api.Name: func(ctx context.Context) error {
			return api.Run(ctx, cfg, api.Deps{})
		},
	})
}

// runMigrate will apply the embedded goose migrations (ADR-0006). The
// migrations and the database connection land with the L0 store; until then it
// refuses rather than pretending the schema is current.
func runMigrate(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs, _ := newFlagSet("migrate", stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	action := "up"
	if fs.NArg() > 0 {
		action = fs.Arg(0)
	}
	switch action {
	case "up", "status", "up-to", "down":
		// No "migrate" prefix here: run wraps the error with the subcommand name.
		return fmt.Errorf("%s: %w, see docs/adr/0006-schema-migrations-with-goose.md", action, errNotImplemented)
	default:
		return fmt.Errorf("unknown action %q: want up, status, up-to or down", action)
	}
}

func runVersion(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if err := parseNoFlags("version", args, stderr); err != nil {
		return err
	}
	fmt.Fprintln(stdout, version.Info())
	return nil
}

func runHelp(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if err := parseNoFlags("help", args, stderr); err != nil {
		return err
	}
	usage(stdout)
	return nil
}

// parseNoFlags is the flag handling for a subcommand that takes neither flags
// nor arguments. It exists so that `hearsay version --log-level bogus extra`
// fails the way `hearsay api --log-level bogus extra` does, rather than printing
// the version and exiting 0 as though the extra words meant something.
func parseNoFlags(name string, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("hearsay "+name, flag.ContinueOnError)
	fs.SetOutput(w)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	return nil
}

// sourceList collects a repeated --source flag.
type sourceList []string

func (s *sourceList) String() string { return strings.Join(*s, ",") }

func (s *sourceList) Set(v string) error {
	if v == "" {
		return errors.New("source name is empty")
	}
	*s = append(*s, v)
	return nil
}
