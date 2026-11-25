# internal/telemetry

Logging.

**Belongs here:** the `log/slog` handler construction and the logger carried in
`context.Context`. Hearsay runs with no observability stack.

**Logs go to stderr, not stdout.** `hearsay version` and `hearsay help` write
their answers to stdout, and a binary whose logs share that stream cannot be
piped into anything. Every container runtime collects both streams, so
`docker compose logs` is unaffected.

**How logging works:** the process attaches a logger with `service`, `version`
and `instance` at startup and puts it in the context. `instance` is the
hostname, or `HEARSAY_INSTANCE` where that is set. Work that descends adds
its own fields once with `telemetry.With(ctx, ...)`; everything below inherits
them. Use the standard field names (`scope`, `source`, `job_kind`,
`job_id`, `l0_id`, `l1_id`, `topic_id`, `principal`, `provider`, `tier`,
`model`, `duration_ms`) so lines from different services join up.

**Content rules — these are access control, not style:**

- Never log L0 `payload`, L1 `text` or `raw_text`, prompts or model completions
  at `info` or above. Log ids; whoever is debugging follows the id to the row,
  where the ACL still applies.
- Never log credentials, tokens or webhook signatures.
- Principals are logged as principal ids, not names or email addresses.
- The scrub runs before `text` is written, not before it is logged, so logging a
  pre-scrub value at `debug` in the distiller is the specific mistake to watch
  for.
