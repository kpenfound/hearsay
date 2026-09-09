// Package config holds the parsed Hearsay configuration.
//
// The configuration format itself — the GitOps repo layout, the single-file
// default and the authority schema — is designed in issue #5 and built in #37.
// Until then this package carries only what the scaffold genuinely uses, so
// that the service entry points can already take the signature ADR-0003 fixes:
//
//	func Run(ctx context.Context, cfg *config.Config, deps Deps) error
package config

// Config is the whole of Hearsay's configuration, shared by all four services
// (ADR-0003). Per-service sections are added under it where something genuinely
// differs, such as worker concurrency.
type Config struct {
	Log Log
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
