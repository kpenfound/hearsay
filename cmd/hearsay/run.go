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
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/llm/providers"
	"github.com/kpenfound/hearsay/internal/service"
	"github.com/kpenfound/hearsay/internal/service/api"
	"github.com/kpenfound/hearsay/internal/service/assertworker"
	"github.com/kpenfound/hearsay/internal/service/connectors"
	"github.com/kpenfound/hearsay/internal/service/distiller"
	"github.com/kpenfound/hearsay/internal/telemetry"
	"github.com/kpenfound/hearsay/internal/version"
)

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
		{distiller.Name, "", "Run the distiller. L0 to L1.", runDistiller},
		{assertworker.Name, "", "Run the assertion worker. L1 to L2.", runService(assertworker.Name, func(ctx context.Context, cfg *config.Config) error {
			return assertworker.Run(ctx, cfg, assertworker.Deps{})
		})},
		{api.Name, "", "Run the read and assert API over MCP and HTTP.", runService(api.Name, func(ctx context.Context, cfg *config.Config) error {
			return api.Run(ctx, cfg, api.Deps{})
		})},
		{"all", "", "Run all four services in one process. Local development only.", runAll},
		{"config", "validate [path]", "Check a configuration repository and say what is wrong with it.", runConfig},
		{"migrate", "up|status|up-to <n>|down", "Apply schema migrations and exit.", runMigrate},
		{"l0", "list|get <id>|count|tail", "Inspect the L0 event store.", runL0},
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
// defaults, overridden by the environment; parsing args overrides both. The
// third result is where `--config` put the path of the configuration
// repository, which is empty when the process was given none.
func newFlagSet(name string, w io.Writer) (*flag.FlagSet, *config.Config, *string) {
	fs := flag.NewFlagSet("hearsay "+name, flag.ContinueOnError)
	fs.SetOutput(w)

	cfg := config.Default()
	cfg.Log.Level = envOr("HEARSAY_LOG_LEVEL", cfg.Log.Level)
	cfg.Log.Format = envOr("HEARSAY_LOG_FORMAT", cfg.Log.Format)
	fs.StringVar(&cfg.Log.Level, "log-level", cfg.Log.Level, "log level: debug, info, warn or error")
	fs.StringVar(&cfg.Log.Format, "log-format", cfg.Log.Format, "log format: json, text or auto")
	configPath := fs.String("config", envOr("HEARSAY_CONFIG", ""), "the configuration repository: a directory, or a single YAML file")
	return fs, &cfg, configPath
}

// loadConfig loads the configuration repository into cfg and logs what it
// found, or says that there is none.
//
// A process started without a configuration runs empty — it ingests nothing and
// serves no bundles — which is what evaluating the binary looks like and is why
// this warns rather than refuses. An invalid configuration is a startup failure
// (ADR-0009): configuration is read once, so a mistake in it must stop the
// process rather than surface on the first request.
func loadConfig(ctx context.Context, cfg *config.Config, path string) error {
	log := telemetry.Logger(ctx)
	if path == "" {
		log.WarnContext(ctx, "no configuration: nothing is ingested and no bundle can be served, pass --config")
		return nil
	}
	repo, err := config.Load(path)
	if err != nil {
		return err
	}
	cfg.Repo = repo
	log.InfoContext(ctx, "configuration loaded",
		"config_path", repo.Path,
		"config_digest", repo.Digest,
		"sources", len(repo.Sources),
		"scopes", len(repo.Scopes),
		"principals", len(repo.Principals),
		"code_entities", len(repo.Code),
		"model_tiers", len(repo.LLM.Configured()),
	)
	return nil
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
	log, err := telemetry.NewLogger(cfg.Log.Level, cfg.Log.Format, stderr)
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
		fs, cfg, configPath := newFlagSet(name, stderr)
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
		if err := loadConfig(ctx, cfg, *configPath); err != nil {
			return err
		}
		return start(ctx, cfg)
	}
}

// runDistiller runs the distiller, which unlike the other three services has
// dependencies: Postgres, and a model tier registry built from the
// configuration. Both are the process's to build and to own (ADR-0003), and
// both are refused up front rather than at the first job — configuration is
// read once, so a process that cannot make a model call should not start
// (ADR-0005, ADR-0009).
func runDistiller(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs, cfg, configPath := newFlagSet(distiller.Name, stderr)
	resolveDatabase := databaseFlag(fs, cfg)
	if err := fs.Parse(args); err != nil {
		return err
	}
	resolveDatabase()
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	ctx, err := withLogger(ctx, distiller.Name, cfg, stderr)
	if err != nil {
		return err
	}
	if cfg.Database.URL == "" {
		return db.ErrNoDatabaseURL
	}
	if err := loadConfig(ctx, cfg, *configPath); err != nil {
		return err
	}
	pool, err := db.Connect(ctx, cfg.Database.URL)
	if err != nil {
		return stoppingEarly(ctx, err)
	}
	defer pool.Close()
	registry, err := modelRegistry(ctx, cfg)
	if err != nil {
		return err
	}
	return distiller.Run(ctx, cfg, distiller.Deps{Pool: pool, LLM: registry})
}

// stoppingEarly turns a startup failure that happened because the process was
// asked to stop into a clean stop. A service is a thing somebody runs in a
// terminal and ends with Ctrl-C, and one interrupted while it is still opening
// a connection has not failed.
func stoppingEarly(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// modelRegistry builds the model tiers the configuration names (ADR-0005).
//
// A process with no configuration repository gets no registry at all, and the
// service it is handed to starts and does nothing — the same as every other
// service without one (ADR-0009). It is not an error here because it is not a
// mistake: a configuration is what says there is anything to do.
//
// A configuration that names a tier the process cannot build — an unknown
// provider, a credential that is not in the environment — is a startup failure,
// which is what ADR-0005 asks for: configuration is read once, so a process
// that cannot make a model call should not be serving.
func modelRegistry(ctx context.Context, cfg *config.Config) (llm.Registry, error) {
	if cfg.Repo.Path == "" {
		telemetry.Logger(ctx).WarnContext(ctx, "no configuration: nothing is distilled, pass --config")
		return nil, nil
	}
	return llm.NewRegistry(cfg.Repo.LLM, providers.All())
}

func runConnectors(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs, cfg, configPath := newFlagSet(connectors.Name, stderr)
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
	if err := loadConfig(ctx, cfg, *configPath); err != nil {
		return err
	}
	return connectors.Run(ctx, cfg, connectors.Deps{Sources: sources})
}

// runAll runs the four services in one process. It exists so that evaluating
// Hearsay does not need four terminals; it is never a deployment target
// (ADR-0003).
func runAll(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs, cfg, configPath := newFlagSet("all", stderr)
	resolveDatabase := databaseFlag(fs, cfg)
	if err := fs.Parse(args); err != nil {
		return err
	}
	resolveDatabase()
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	// No service name on the process logger: RunAll names each of the four.
	ctx, err := withLogger(ctx, "", cfg, stderr)
	if err != nil {
		return err
	}
	// The process logger has no service field here, because RunAll gives each of
	// the four its own. Loading the configuration and bringing the schema up are
	// the command's own work and happen before any of them start, so those lines
	// say `all` rather than going out without the field every other line carries.
	setup := telemetry.With(ctx, "service", "all")
	if err := loadConfig(setup, cfg, *configPath); err != nil {
		return err
	}
	if cfg.Database.URL == "" {
		// `all` runs the distiller, which reads L0 and writes L1. It used to be
		// four stubs and could be looked at with nothing behind it; it is not
		// any more, and a process that cannot store what it distils is not
		// worth starting.
		return db.ErrNoDatabaseURL
	}
	if err := migrateForDev(setup, cfg); err != nil {
		return err
	}
	pool, err := db.Connect(setup, cfg.Database.URL)
	if err != nil {
		return stoppingEarly(ctx, err)
	}
	defer pool.Close()
	registry, err := modelRegistry(setup, cfg)
	if err != nil {
		return err
	}

	return service.RunAll(ctx, map[string]service.RunFunc{
		connectors.Name: func(ctx context.Context) error {
			return connectors.Run(ctx, cfg, connectors.Deps{})
		},
		distiller.Name: func(ctx context.Context) error {
			return distiller.Run(ctx, cfg, distiller.Deps{Pool: pool, LLM: registry})
		},
		assertworker.Name: func(ctx context.Context) error {
			return assertworker.Run(ctx, cfg, assertworker.Deps{})
		},
		api.Name: func(ctx context.Context) error {
			return api.Run(ctx, cfg, api.Deps{})
		},
	})
}

// runConfig is the configuration subcommand. `validate` is the whole of it: it
// loads a configuration repository the way a service would and reports
// everything wrong with it, so that a bad change is caught by CI on the
// configuration repository rather than by a deployment (ADR-0009).
func runConfig(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs, _, configPath := newFlagSet("config", stderr)
	action, actionArgs, err := parseAction(fs, args, "")
	if err != nil {
		return err
	}
	if action == "" {
		return errors.New("no action given: want validate")
	}
	if action != "validate" {
		return fmt.Errorf("unknown action %q: want validate", action)
	}
	if len(actionArgs) > 1 {
		return fmt.Errorf("unexpected argument %q", actionArgs[1])
	}

	// The path is the argument, the --config flag, or the working directory,
	// which is what running this in a checkout of the configuration repository
	// should mean.
	path := *configPath
	if len(actionArgs) == 1 {
		path = actionArgs[0]
	}
	if path == "" {
		path = "."
	}

	repo, err := config.Load(path)
	if err != nil {
		return err
	}
	printSummary(stdout, repo)
	return nil
}

// printSummary is what `hearsay config validate` prints when there is nothing
// wrong: enough for a person to see that the configuration is the one they
// meant, and the digest a running process reports so the two can be compared.
func printSummary(w io.Writer, repo config.Repo) {
	fmt.Fprintf(w, "%s is valid\n", repo.Path)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "  sources\t%d\t%s\n", len(repo.Sources), summarize(sourceIDs(repo)))
	fmt.Fprintf(tw, "  scopes\t%d\t%s\n", len(repo.Scopes), summarize(scopeIDs(repo)))
	fmt.Fprintf(tw, "  principals\t%d\n", len(repo.Principals))
	fmt.Fprintf(tw, "  code entities\t%d\n", len(repo.Code))
	fmt.Fprintf(tw, "  authority\t%d\t%s\n", len(repo.Authority.Scopes()), summarize(repo.Authority.Scopes()))
	fmt.Fprintf(tw, "  model tiers\t%d\t%s\n", len(repo.LLM.Configured()), summarize(modelTiers(repo)))
	fmt.Fprintf(tw, "  digest\t\t%s\n", repo.Digest)
	_ = tw.Flush()
}

// modelTiers is what each model tier resolves to, which is worth printing
// because most of it is usually a default that is nowhere in the files
// (ADR-0005).
func modelTiers(repo config.Repo) []string {
	tiers := repo.LLM.Configured()
	out := make([]string, 0, len(tiers))
	for _, t := range tiers {
		tc, _ := repo.LLM.Tier(t)
		out = append(out, fmt.Sprintf("%s=%s/%s", t, tc.Provider, tc.Model))
	}
	return out
}

func sourceIDs(repo config.Repo) []string {
	ids := make([]string, 0, len(repo.Sources))
	for _, s := range repo.Sources {
		ids = append(ids, s.ID)
	}
	return ids
}

func scopeIDs(repo config.Repo) []string {
	ids := make([]string, 0, len(repo.Scopes))
	for _, s := range repo.Scopes {
		ids = append(ids, s.ID)
	}
	return ids
}

// summarize lists ids for the summary, keeping a long list to one line.
func summarize(ids []string) string {
	const max = 6
	if len(ids) == 0 {
		return ""
	}
	if len(ids) > max {
		return strings.Join(ids[:max], ", ") + ", ..."
	}
	return strings.Join(ids, ", ")
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
