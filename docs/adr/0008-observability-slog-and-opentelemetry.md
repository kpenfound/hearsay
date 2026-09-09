# 8. Observability: `log/slog` and OpenTelemetry

- Status: accepted
- Date: 2026-09-08

## Context

Four services (ADR-0003) share one database (ADR-0004) and one job queue (ADR-0007). A
single unit of work crosses all of them: an event arrives at a connector, becomes an L0
row, is queued, is distilled into L1 by a model call, is queued again, becomes an L2
stance, and eventually appears in a bundle. When the answer to "why is this decision not
in my bundle" is somewhere in that chain, correlating by hand across four containers is
the whole debugging cost.

Two things make this system harder to instrument than an ordinary service. Model calls
are the dominant cost and the dominant latency, so cost has to be observable per tier
(ADR-0005) rather than inferred from a provider's billing page a month later. And
Hearsay handles private team communication under an explicit access-control model, so
logs are a leak channel: the distilled `text` of a Slack thread in a log line has escaped
every ACL in the design.

Hearsay is self-hosted. An operator may have a full observability stack or may have
`docker compose logs`. Neither can be assumed, and requiring a collector to run the
software at all would be a bad first experience.

## Decision

### Logging: `log/slog`, structured, one logger through context

- The standard library's `log/slog`. No third-party logging library.
- JSON handler in production, text handler when the terminal is a TTY or
  `HEARSAY_LOG_FORMAT=text`. Level from config, default `info`.
- The logger is carried in `context.Context` and enriched as work descends. A handler
  attached at process start carries `service` (the subcommand name from ADR-0003),
  `version`, and `instance`. Each job or request adds its own fields once, so every line
  below it has them without being passed them.
- Standard field names, used everywhere: `service`, `scope`, `source`, `job_kind`,
  `job_id`, `l0_id`, `l1_id`, `topic_id`, `principal`, `provider`, `tier`, `model`,
  `trace_id`, `span_id`, `error`, `duration_ms`.
- Levels mean something specific:
  - `error` — a human needs to act. A failed job that has exhausted retries. Not a
    single transient provider 429.
  - `warn` — degraded but handled: a retry, a rate limit, a connector reconnect, a
    dropped bundle section.
  - `info` — one line per completed unit of work (job, request, connector poll) with its
    outcome and duration. Not a narration of steps.
  - `debug` — off in production, and where prompts and payload shapes go.

**Content rules, which are access control and not style:**

- Never log L0 `payload`, L1 `text` or `raw_text`, prompts, or model completions at
  `info` or above. Log ids. Someone debugging follows the id to the row, where the ACL
  still applies.
- Never log credentials, tokens or webhook signatures.
- Principals are logged as principal ids, not email addresses or display names.
- The secrets and PII scrub runs before `text` is written, not before it is logged, so
  logging text at `debug` on a pre-scrub value is the specific mistake to watch for in
  the distiller.

### Tracing and metrics: OpenTelemetry, off unless configured

- OpenTelemetry for both, exported over OTLP. No Prometheus client library and no
  vendor SDK in the code; an operator who wants Prometheus scrapes runs a collector, or
  enables the Prometheus exporter on the API's `/metrics`.
- **Nothing is exported by default.** With no OTLP endpoint configured the providers are
  no-ops. Hearsay runs with no observability stack; adding one is setting an endpoint.
- Trace context propagates through the queue: the enqueuing span's context is stored on
  the job row and restored by the worker as a link, so distillation and assertion for one
  event join up across services and across the hours between them.

**Spans**, at the boundaries that cost time or money:

- `connector.fetch`, `connector.ingest` — per source poll or push, attributes `source`,
  event count.
- `queue.enqueue`, `queue.claim`, `job.run` — attributes `job_kind`, `attempt`,
  `serial_key` presence.
- `llm.complete`, `llm.embed` — attributes `tier`, `provider`, `model`, input and output
  token counts, stop reason. Never the prompt.
- `db.query` for the named queries that matter (hybrid search, bundle assembly, the L2
  recursive CTEs), not every statement.
- `bundle.assemble` and each section it builds, plus what was dropped to fit the budget.
- HTTP and MCP request spans on the API.

**Metrics**, the small set an operator would actually look at:

| Metric | Why |
|---|---|
| `hearsay.queue.depth{kind}`, `hearsay.queue.oldest_pending_age{kind}` | Is the pipeline keeping up. The first thing to check. |
| `hearsay.job.duration{kind,outcome}`, `hearsay.job.failures{kind}` | Which stage is slow or broken. |
| `hearsay.llm.tokens{tier,provider,model,direction}`, `hearsay.llm.duration{tier}`, `hearsay.llm.errors{tier,reason}` | Cost and rate limits, per tier, which is the point of ADR-0005. |
| `hearsay.connector.events{source}`, `hearsay.connector.lag{source}` | Is a source silently stopped. |
| `hearsay.bundle.duration`, `hearsay.bundle.sections_dropped{section}` | The read path's latency budget and whether bundles are being truncated. |
| `hearsay.db.pool.*` | Connection exhaustion, the usual first symptom of the shared-database trade in ADR-0004. |

Attribute cardinality is bounded on purpose: `scope`, `principal`, `topic_id` and
document ids are log and span attributes, never metric labels.

### What is not observability

The evaluation metrics in `docs/design.md` — drill-down rate, conflict-flag precision,
time to ratification, topic merge and split rate — are product measurements about
whether the distillation is any good. They are computed from L0 audit events in the
database (#27, #28), not from this pipeline. Telemetry can be sampled and dropped;
evaluation data cannot.

### Health

Each service exposes `/healthz` (process is up) and `/readyz` (database reachable,
schema version acceptable per ADR-0006, provider credentials present). Workers get an
HTTP listener for this alone.

## Alternatives considered

- **zerolog or zap.** Faster, and zap's structured API is nicer than `slog`'s in places.
  `slog` is in the standard library, is structured, has a levelling and handler model
  good enough here, and logging is not on a hot path in an IO-bound system that spends
  its time waiting on models and Postgres. One less dependency in every package.
- **Prometheus client library directly, with a `/metrics` endpoint per service.** The
  most common Go setup and simpler than OTel's API. Rejected because tracing is the
  thing that actually helps here — the debugging question is "where did this event go",
  which is a trace question, not a counter question — and running OTel for traces plus
  Prometheus for metrics means two instrumentation APIs in the same code.
- **Traces only, no metrics.** Traces answer "what happened to this event" but not "is
  the queue backing up", which is what an operator needs at 2am and what alerts on.
- **OpenTelemetry logs as well, replacing `slog` output.** The OTel logging story is the
  least mature of the three signals, and an operator reading `docker compose logs` should
  see readable JSON on stdout. `slog` writes to stdout; the trace correlation comes from
  the `trace_id` field, which is enough.
- **Export by default to a bundled collector.** Better observability out of the box,
  worse first run: another container in the compose file before the user has ingested a
  single event.

## Consequences

- Trace context on the queue row is a column the queue schema must carry (ADR-0007), and
  it is a *link* rather than a parent, because the job may run long after the request
  that created it ended.
- The log content rules need enforcing, not just stating. A reviewer checklist item, and
  worth a lint rule if a leak is ever found in review.
- `/readyz` failing on a schema mismatch is what turns the version check in ADR-0006
  into a deployment that stops rather than one that half-works.
- Every service needs an HTTP listener, including the two workers that otherwise serve
  nothing. Small cost, and it is also where the optional Prometheus exporter lives.
- Token metrics per tier make the cost consequence of a prompt change visible in the same
  place as its latency, which is the number that decides whether a distillation prompt is
  affordable at ingest volume.
