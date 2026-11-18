// Package config holds Hearsay's configuration: the process settings that come
// from flags and the environment, and the configuration repository that is
// applied like GitOps.
//
// The configuration repository — `sources/`, `scopes/`, `principals/`, `code/`,
// `authority/` and `llm/`, or the single file that expands to it — is read by
// [Load], which reports everything wrong with it rather than the first. The
// format is documented in docs/config.md.
//
// The parsed configuration reaches the four services as the fixed argument:
//
//	func Run(ctx context.Context, cfg *config.Config, deps Deps) error
package config

// Config is the whole of Hearsay's configuration, shared by all four
// services. Per-service sections are added under it where something genuinely
// differs, such as worker concurrency.
type Config struct {
	Log Log
	// Database is the Postgres connection. It is a process setting
	// rather than part of the configuration repository: the repository is
	// checked in, and a connection URL carries a password.
	Database Database
	// Repo is the configuration repository, loaded at startup and never
	// reloaded while the process runs. Its zero value is a process
	// that was started without one: it ingests nothing and serves no bundles,
	// which is the state `hearsay` runs in until it is pointed at a
	// configuration.
	Repo Repo
}

// Database is how a process reaches Postgres.
type Database struct {
	// URL is the connection URL, for example
	// postgres://hearsay@localhost:5432/hearsay. Empty means the process was
	// given no database, which every command that needs one refuses.
	URL string
}

// Log configures the process logger.
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
