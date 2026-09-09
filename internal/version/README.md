# internal/version

The build identity of the running binary: version, commit and build date, behind
`hearsay version`.

Values come from `-ldflags -X` at release build time and fall back to the Go
module's own build info, so a binary from `go build` still reports a truthful
commit rather than a placeholder.

The same values go on every log line as `version` (ADR-0008), which is what makes
a log from a rolling deployment attributable to a build.
