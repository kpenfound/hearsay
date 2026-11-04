# internal/version

The build identity of the running binary: version, commit and build date, behind
`hearsay version`.

Values come from `-ldflags -X` at release build time and fall back to the Go
module's own build info. A binary from `go build` reports its commit when VCS
metadata is available; a file-only build reports `unknown` for the commit and
build date, and an unstamped development version is `devel`.

The same values go on every log line as `version` (ADR-0008), which is what makes
a log from a rolling deployment attributable to a build.
