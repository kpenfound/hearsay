# internal/telemetry

Logging, tracing and metrics (ADR-0008).

**Belongs here:** the `log/slog` handler construction, the logger carried in
`context.Context`, and — when they land — the OpenTelemetry trace and metric
providers, which are no-ops unless an OTLP endpoint is configured. Nothing is
exported by default: Hearsay must run with no observability stack at all.

**How logging works:** the process attaches a logger with `service`, `version`
and `instance` at startup and puts it in the context. Work that descends adds
its own fields once with `telemetry.With(ctx, ...)`; everything below inherits
them. Use the standard field names in ADR-0008 (`scope`, `source`, `job_kind`,
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

See [ADR-0008](../../docs/adr/0008-observability-slog-and-opentelemetry.md).
