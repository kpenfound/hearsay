package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/connector/agent"
	"github.com/kpenfound/hearsay/internal/connector/conversation"
	"github.com/kpenfound/hearsay/internal/connector/discord"
	"github.com/kpenfound/hearsay/internal/connector/drive"
	"github.com/kpenfound/hearsay/internal/connector/github"
	"github.com/kpenfound/hearsay/internal/connector/obsidian"
	"github.com/kpenfound/hearsay/internal/connector/slack"
	"github.com/kpenfound/hearsay/internal/connector/tracker"
	"github.com/kpenfound/hearsay/internal/connector/work"
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/l2"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/llm/claudesbx"
	"github.com/kpenfound/hearsay/internal/llm/jev"
	"github.com/kpenfound/hearsay/internal/llm/providers"
	"github.com/kpenfound/hearsay/internal/principal"
	"github.com/kpenfound/hearsay/internal/service"
	"github.com/kpenfound/hearsay/internal/service/api"
	"github.com/kpenfound/hearsay/internal/service/assertworker"
	"github.com/kpenfound/hearsay/internal/service/connectors"
	"github.com/kpenfound/hearsay/internal/service/distiller"
	"github.com/kpenfound/hearsay/internal/tailnet"
	"github.com/kpenfound/hearsay/internal/telemetry"
	"github.com/kpenfound/hearsay/internal/version"
)

type command struct {
	name    string
	args    string
	summary string
	run     func(ctx context.Context, args []string, stdout, stderr io.Writer) error
}

func commands() []command {
	return []command{
		{connectors.Name, "[--source name]...", "Run the source connectors. Writes L0, and applies the chat commands they receive.", runConnectors},
		{distiller.Name, "", "Run the distiller. L0 to L1.", runDistiller},
		{assertworker.Name, "", "Run the assertion worker. L1 to L2.", runAssertWorker},
		{api.Name, "", "Run the read and assert API over MCP and HTTP.", runAPI},
		{"all", "", "Run all four services in one process. Local development only.", runAll},
		{"init", "[--github-repo owner/name]... [--discord-guild id --discord-channel id...] [--drive-folder id]... [--force]", "Write a new team's hearsay.yaml and the env file for its secrets.", runInit},
		{"config", "validate [path]", "Check a configuration repository and say what is wrong with it.", runConfig},
		{"migrate", "up|status|up-to <n>|down", "Apply schema migrations and exit.", runMigrate},
		{"l0", "list|get <id>|count|tail", "Inspect the L0 event store.", runL0},
		{"identities", "list [--json|--yaml]", "List unmapped and ambiguous source identities.", runIdentities},
		{"aliases", "list|confirm <entity> <name>|reject <entity> <name>", "Review learned entity names.", runAliases},
		{"topics", "list <scope>|merge <from> <into>|split <topic> --stance <id>... --name <name>|undo <op>|ops", "List topics and their operations; merge, split and undo by hand.", runTopics},
		{"gestures", "ratify|demote|pin|unpin <document>|undo <gesture-id>|list", "Record and list human stance and pin gestures.", runGestures},
		{"eval", "[--since <time>] [--until <time>] [--scope <id>] [--json]", "Print evaluation metrics and pipeline accounting; eval judgments compares supplied Jev predictions with a labelled corpus offline.", runEval},
		{"delete", "--event <id>|--artifact <source> <id>|--author <identity> [--apply] | list | show <id>", "Preview deletion provenance; --apply redacts L0 and re-distills; list and show read the records.", runDelete},
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

func envBool(key string) bool {
	v, err := strconv.ParseBool(os.Getenv(key))
	return err == nil && v
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

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

func instanceName() string {
	if v := envOr("HEARSAY_INSTANCE", ""); v != "" {
		return v
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "unknown"
}

func runDistiller(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs, cfg, configPath := newFlagSet(distiller.Name, stderr)
	pumpOptions := distillFlags(fs)
	listen := fs.String("listen", envOr("HEARSAY_DISTILLER_LISTEN", distiller.DefaultListen), "address the distiller serves /healthz and /readyz on")
	resolveDatabase := databaseFlag(fs, cfg)
	if err := fs.Parse(args); err != nil {
		return err
	}
	pump, err := pumpOptions()
	if err != nil {
		return err
	}
	resolveDatabase()
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *listen == "" {
		return errors.New("--listen is empty: it is the address the health probes are served on")
	}
	ctx, err = withLogger(ctx, distiller.Name, cfg, stderr)
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
	registry, releaseRegistry, err := modelRegistry(ctx, cfg)
	if err != nil {
		return err
	}
	defer releaseRegistry()
	judgments, err := judgmentCapability(cfg, nil)
	if err != nil {
		return err
	}
	return distiller.Run(ctx, cfg, distiller.Deps{Pool: pool, LLM: registry, Judgments: judgments, Listen: *listen, Pump: pump, Concurrency: distillConcurrency(cfg)})
}

func runAssertWorker(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs, cfg, configPath := newFlagSet(assertworker.Name, stderr)
	listen := fs.String("listen", envOr("HEARSAY_ASSERT_WORKER_LISTEN", assertworker.DefaultListen), "address the assertion worker serves /healthz and /readyz on")
	resolveDatabase := databaseFlag(fs, cfg)
	if err := fs.Parse(args); err != nil {
		return err
	}
	resolveDatabase()
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *listen == "" {
		return errors.New("--listen is empty: it is the address the health probes are served on")
	}
	ctx, err := withLogger(ctx, assertworker.Name, cfg, stderr)
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
	registry, releaseRegistry, err := modelRegistry(ctx, cfg)
	if err != nil {
		return err
	}
	defer releaseRegistry()
	repos, err := repoReaders(cfg.Repo, os.LookupEnv)
	if err != nil {
		return err
	}
	replies, err := commandRepliers(cfg.Repo, os.LookupEnv)
	if err != nil {
		return err
	}
	judgments, err := judgmentCapability(cfg, nil)
	if err != nil {
		return err
	}
	return assertworker.Run(ctx, cfg, assertworker.Deps{Pool: pool, LLM: registry, Judgments: judgments, Connectors: connectorRegistry(cfg.Repo.Principals, cfg.Repo.Sources), Repos: repos, Replies: replies, Listen: *listen})
}

func repoReaders(repo config.Repo, lookup func(string) (string, bool)) (l2.RepoReader, error) {
	readers := assertworker.Repos{}
	for _, c := range repo.Code {
		id := c.Repo.Source
		if _, done := readers[id]; done || id == "" {
			continue
		}
		src, ok := repo.Source(id)
		if !ok || src.Type != github.Type {
			continue
		}
		resolved, err := githubToken(src, lookup)
		if err != nil {
			return nil, err
		}
		reader, err := github.NewReader(resolved)
		if err != nil {
			return nil, fmt.Errorf("source %q: %w", id, err)
		}
		readers[id] = reader
	}
	if len(readers) == 0 {
		return nil, nil
	}
	return readers, nil
}

func commandRepliers(repo config.Repo, lookup func(string) (string, bool)) (assertworker.Replies, error) {
	replies := assertworker.Replies{}
	for _, src := range repo.Sources {
		if src.Type != github.Type || src.ReadOnly {
			continue
		}
		resolved, err := githubToken(src, lookup)
		if err != nil {
			return nil, err
		}
		replier, err := github.NewReplier(resolved)
		if err != nil {
			return nil, fmt.Errorf("source %q: %w", src.ID, err)
		}
		replies[src.ID] = replier
	}
	return replies, nil
}

func githubToken(src connector.SourceConfig, lookup func(string) (string, bool)) (connector.SourceConfig, error) {
	if env, ok := src.Secrets[github.SecretToken]; ok {
		src.Secrets = map[string]string{github.SecretToken: env}
	} else {
		src.Secrets = nil
	}
	return connector.ResolveSecrets(src, lookup)
}

func runAPI(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs, cfg, configPath := newFlagSet(api.Name, stderr)
	listen := fs.String("listen", envOr("HEARSAY_API_LISTEN", api.DefaultListen),
		"address the API serves HTTP (/v1/<call>), MCP (/mcp) and its health on")
	tailnetName := fs.String("tailnet", os.Getenv("HEARSAY_API_TAILNET"),
		"hostname to also serve the API as on the tailnet, through an embedded Tailscale node; empty serves only --listen")
	tailnetDir := fs.String("tailnet-dir", os.Getenv("HEARSAY_API_TAILNET_DIR"),
		"the tailnet node's state directory, which must persist; required with --tailnet")
	tailnetHTTP := fs.Bool("tailnet-http", envBool("HEARSAY_API_TAILNET_HTTP"),
		"serve plain HTTP on the tailnet's port 80 instead of HTTPS on 443")
	resolveDatabase := databaseFlag(fs, cfg)
	if err := fs.Parse(args); err != nil {
		return err
	}
	resolveDatabase()
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *listen == "" {
		return errors.New("--listen is empty: it is the address the API is served on")
	}
	ctx, err := withLogger(ctx, api.Name, cfg, stderr)
	if err != nil {
		return err
	}
	node, err := apiTailnet(ctx, *tailnetName, *tailnetDir, *tailnetHTTP)
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
	registry, releaseRegistry, err := modelRegistry(ctx, cfg)
	if err != nil {
		return err
	}
	defer releaseRegistry()
	apps, err := discordApps(cfg.Repo, os.LookupEnv)
	if err != nil {
		return err
	}
	return api.Run(ctx, cfg, api.Deps{Pool: pool, LLM: registry, Listen: *listen, Discord: apps, Tailnet: node})
}

func apiTailnet(ctx context.Context, hostname, dir string, http bool) (*tailnet.Node, error) {
	if hostname == "" {
		return nil, nil
	}
	if !tailnet.ValidHostname(hostname) {
		return nil, fmt.Errorf("--tailnet %q is not a tailnet hostname: one DNS label of lowercase letters, digits and hyphens", hostname)
	}
	if dir == "" {
		return nil, errors.New("--tailnet needs --tailnet-dir (or HEARSAY_API_TAILNET_DIR): the node's state has to persist, or every start is a new node")
	}
	return tailnet.New(tailnet.Config{Hostname: hostname, Dir: dir, HTTP: http, Join: tailnet.JoinTailscale(telemetry.Logger(ctx))}), nil
}

func discordApps(repo config.Repo, lookup func(string) (string, bool)) ([]*discord.App, error) {
	var apps []*discord.App
	for _, src := range repo.Sources {
		if src.Type != discord.Type || !discord.HasApp(src) {
			continue
		}
		if env, ok := src.Secrets[discord.SecretToken]; ok {
			src.Secrets = map[string]string{discord.SecretToken: env}
		} else {
			src.Secrets = nil
		}
		resolved, err := connector.ResolveSecrets(src, lookup)
		if err != nil {
			return nil, err
		}
		app, err := discord.NewApp(resolved)
		if err != nil {
			return nil, fmt.Errorf("source %q: %w", src.ID, err)
		}
		if app != nil {
			apps = append(apps, app)
		}
	}
	return apps, nil
}

func stoppingEarly(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func modelRegistry(ctx context.Context, cfg *config.Config) (llm.Registry, func(), error) {
	if cfg.Repo.Path == "" {
		telemetry.Logger(ctx).WarnContext(ctx, "no configuration: no model tiers are built, pass --config")
		return nil, func() {}, nil
	}
	registry, err := llm.NewRegistry(cfg.Repo.LLM, providers.All())
	if err != nil {
		return nil, nil, err
	}
	release := func() {
		closing, cancel := context.WithTimeout(context.WithoutCancel(ctx), registryCloseTimeout)
		defer cancel()
		if err := registry.Close(closing); err != nil {
			telemetry.Logger(ctx).WarnContext(ctx, "closing the model tiers", "error", err)
		}
	}
	return registry, release, nil
}

func distillConcurrency(cfg *config.Config) int {
	if cfg.Repo.LLM.Tiers[llm.TierDistill].Provider == llm.ProviderClaudeSbx {
		return claudesbx.Sandboxes
	}
	return 0
}

const registryCloseTimeout = 30 * time.Second

func runConnectors(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs, cfg, configPath := newFlagSet(connectors.Name, stderr)
	var sources sourceList
	fs.Var(&sources, "source", "run only this configured connector; repeat the flag for several. The default is all of them.")
	listen := listenFlag(fs)
	resolveDatabase := databaseFlag(fs, cfg)
	if err := fs.Parse(args); err != nil {
		return err
	}
	resolveDatabase()
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if err := checkListen(*listen); err != nil {
		return err
	}
	ctx, err := withLogger(ctx, connectors.Name, cfg, stderr)
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
	return connectors.Run(ctx, cfg, connectors.Deps{
		Sources:  sources,
		Pool:     pool,
		Registry: connectorRegistry(cfg.Repo.Principals, cfg.Repo.Sources),
		Listen:   *listen,
	})
}

func listenFlag(fs *flag.FlagSet) *string {
	return fs.String("listen", envOr("HEARSAY_LISTEN", connectors.DefaultListen),
		"address the connectors service serves push connectors' webhooks and its health on")
}

func checkListen(addr string) error {
	if addr == "" {
		return errors.New("--listen is empty: it is the address webhooks and health are served on")
	}
	return nil
}

func connectorRegistry(principals []principal.Principal, sources []connector.SourceConfig) *connector.Registry {
	registry := connector.NewRegistry()
	_ = registry.Register(github.Type, github.Factory)
	_ = registry.Register(discord.Type, discord.Factory)
	_ = registry.Register(slack.Type, slack.Factory)
	_ = registry.RegisterReactionGestures(discord.Type, discord.ReactionGestureConfig)
	_ = registry.RegisterReactionGestures(slack.Type, slack.ReactionGestureConfig)
	_ = registry.Register(drive.Type, drive.Factory)
	_ = registry.Register(obsidian.Type, obsidian.Factory)
	_ = registry.Register(tracker.Type, tracker.Factory)
	_ = registry.Register(agent.Type, agent.NewFactory(principals, os.LookupEnv))
	_ = registry.Register(conversation.Type, conversation.NewFactory(principals, os.LookupEnv))
	sourceIDs := make([]string, 0, len(sources))
	for _, source := range sources {
		sourceIDs = append(sourceIDs, source.ID)
	}
	_ = registry.Register(work.Type, work.NewFactory(principals, sourceIDs, os.LookupEnv, work.WithSources(sources)))
	return registry
}

func runAll(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs, cfg, configPath := newFlagSet("all", stderr)
	pumpOptions := distillFlags(fs)
	resolveDatabase := databaseFlag(fs, cfg)
	listen := listenFlag(fs)
	distillerListen := fs.String("distiller-listen", envOr("HEARSAY_DISTILLER_LISTEN", distiller.DefaultListen), "address the distiller serves health probes on")
	assertWorkerListen := fs.String("assert-worker-listen", envOr("HEARSAY_ASSERT_WORKER_LISTEN", assertworker.DefaultListen), "address the assertion worker serves health probes on")
	apiListen := fs.String("api-listen", envOr("HEARSAY_API_LISTEN", api.DefaultListen),
		"address the API service serves HTTP, MCP and its health on")
	if err := fs.Parse(args); err != nil {
		return err
	}
	pump, err := pumpOptions()
	if err != nil {
		return err
	}
	resolveDatabase()
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if err := checkListen(*listen); err != nil {
		return err
	}
	if *apiListen == "" {
		return errors.New("--api-listen is empty: it is the address the API is served on")
	}
	if *distillerListen == "" || *assertWorkerListen == "" {
		return errors.New("worker health probe listen address is empty")
	}
	ctx, err = withLogger(ctx, "", cfg, stderr)
	if err != nil {
		return err
	}
	setup := telemetry.With(ctx, "service", "all")
	if err := loadConfig(setup, cfg, *configPath); err != nil {
		return err
	}
	if cfg.Database.URL == "" {
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
	registry, releaseRegistry, err := modelRegistry(setup, cfg)
	if err != nil {
		return err
	}
	defer releaseRegistry()
	repos, err := repoReaders(cfg.Repo, os.LookupEnv)
	if err != nil {
		return err
	}
	replies, err := commandRepliers(cfg.Repo, os.LookupEnv)
	if err != nil {
		return err
	}
	apps, err := discordApps(cfg.Repo, os.LookupEnv)
	if err != nil {
		return err
	}

	judgments, err := judgmentCapability(cfg, nil)
	if err != nil {
		return err
	}
	return service.RunAll(ctx, map[string]service.RunFunc{
		connectors.Name: func(ctx context.Context) error {
			return connectors.Run(ctx, cfg, connectors.Deps{Pool: pool, Registry: connectorRegistry(cfg.Repo.Principals, cfg.Repo.Sources), Listen: *listen})
		},
		distiller.Name: func(ctx context.Context) error {
			return distiller.Run(ctx, cfg, distiller.Deps{Pool: pool, LLM: registry, Judgments: judgments, Listen: *distillerListen, Pump: pump, Concurrency: distillConcurrency(cfg)})
		},
		assertworker.Name: func(ctx context.Context) error {
			return assertworker.Run(ctx, cfg, assertworker.Deps{Pool: pool, LLM: registry, Judgments: judgments, Connectors: connectorRegistry(cfg.Repo.Principals, cfg.Repo.Sources), Repos: repos, Replies: replies, Listen: *assertWorkerListen})
		},
		api.Name: func(ctx context.Context) error {
			return api.Run(ctx, cfg, api.Deps{Pool: pool, LLM: registry, Listen: *apiListen, Discord: apps})
		},
	})
}

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

type sourceList []string

func (s *sourceList) String() string { return strings.Join(*s, ",") }

func (s *sourceList) Set(v string) error {
	if v == "" {
		return errors.New("source name is empty")
	}
	*s = append(*s, v)
	return nil
}

func distillFlags(fs *flag.FlagSet) func() (distiller.PumpOptions, error) {
	quiet := fs.String("distill-quiet", envOr("HEARSAY_DISTILL_QUIET", "30s"), "quiet period before distilling a document (Go duration, 0 disables)")
	maxDelay := fs.String("distill-max-delay", envOr("HEARSAY_DISTILL_MAX_DELAY", "5m"), "maximum debounce delay from a document's first enqueue (Go duration)")
	return func() (distiller.PumpOptions, error) {
		q, err := time.ParseDuration(*quiet)
		if err != nil {
			return distiller.PumpOptions{}, fmt.Errorf("parsing --distill-quiet: %w", err)
		}
		m, err := time.ParseDuration(*maxDelay)
		if err != nil {
			return distiller.PumpOptions{}, fmt.Errorf("parsing --distill-max-delay: %w", err)
		}
		if q < 0 {
			return distiller.PumpOptions{}, errors.New("--distill-quiet must be non-negative")
		}
		if q > 0 && m < q {
			return distiller.PumpOptions{}, errors.New("--distill-max-delay must be at least --distill-quiet when debouncing is enabled")
		}
		return distiller.PumpOptions{Quiet: q, MaxDelay: m}, nil
	}
}

func judgmentCapability(cfg *config.Config, factory llm.JudgmentFactory) (llm.Judgments, error) {
	if cfg.Repo.Path == "" {
		return llm.Unconfigured(), nil
	}
	if factory == nil {
		factory = func(c llm.JudgmentConfig, key string) llm.JudgmentProvider { return jev.New(c, key) }
	}
	return llm.NewJudgments(cfg.Repo.Judgments, factory, os.Getenv)
}
