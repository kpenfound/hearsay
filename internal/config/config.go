// Package config holds Hearsay's configuration: the process settings that come
// from flags and the environment, and the configuration repository that is
// applied like GitOps.
//
// The configuration repository — `sources/`, `scopes/`, `principals/`, `code/`
// and `authority/`, or the single file that expands to it — is read by [Load],
// which reports everything wrong with it rather than the first thing. The
// format is documented in docs/config.md and decided in ADR-0009.
//
// The parsed configuration reaches the four services as the argument ADR-0003
// fixes:
//
//	func Run(ctx context.Context, cfg *config.Config, deps Deps) error
package config

// Config is the whole of Hearsay's configuration, shared by all four services
// (ADR-0003). Per-service sections are added under it where something genuinely
// differs, such as worker concurrency.
type Config struct {
	Log Log
	// Repo is the configuration repository, loaded at startup and never
	// reloaded while the process runs (ADR-0009). Its zero value is a process
	// that was started without one: it ingests nothing and serves no bundles,
	// which is the state `hearsay` runs in until it is pointed at a
	// configuration.
	Repo Repo
}

// Log configures the process logger (ADR-0008).
type Log struct {
	// Level is one of debug, info, warn, error.
	Level string
	// Format is json, text, or auto (text when stderr is a terminal).
	Format string
}

// Default returns the configuration a process uses when nothing is set.
func Default() Config {
	return Config{
		Log: Log{Level: "info", Format: "auto"},
	}
}
