# Deploying Hearsay

How to run Hearsay for a team on one host, with Docker Compose. The files are
in [`deploy/compose/`](../deploy/compose/):

| File | What it is |
|---|---|
| `compose.yaml` | Postgres, the migration job and the four services. |
| `env.example` | Every variable the deployment reads, with the service that reads it. Copied to `.env`, which is never committed. |
| `config/hearsay.yaml` | An example configuration that runs with no source credentials. Replace it with your own. |

This is the production shape: four containers from one image, one per service,
and never `hearsay all`, which is for development only (ADR-0003). Development
is `dagger up`; see [CONTRIBUTING.md](../CONTRIBUTING.md).

## What runs

| Service | Command | Listens on | Published | Reads |
|---|---|---|---|---|
| `postgres` | pgvector on Postgres 16 | 5432 | no | `POSTGRES_PASSWORD` |
| `migrate` | `hearsay migrate up`, then exits | — | — | `HEARSAY_DATABASE_URL` |
| `connectors` | `hearsay connectors` | 8081: `/hooks/<source id>`, `/healthz`, `/readyz` | `127.0.0.1:8081` | `.env` |
| `distiller` | `hearsay distiller` | 8082: `/healthz`, `/readyz` | no | `.env` |
| `assert-worker` | `hearsay assert-worker` | 8083: `/healthz`, `/readyz` | no | `.env` |
| `api` | `hearsay api` | 8080: `/v1/<call>`, `/mcp`, `/discord/<source id>/interactions`, `/healthz`, `/readyz` | `127.0.0.1:8080` | `.env` |

Postgres keeps its data on the named volume `pgdata`. Every Hearsay service
waits for Postgres to be healthy and for `migrate` to exit 0. Each has a Docker
healthcheck on its own `/readyz`: 200 when its database is reachable and its
schema is current, and for the connectors, only while no source reports itself
failed. `/healthz` answers 200 whenever the process is up.

The four services run as `nobody` and read their configuration from
`/etc/hearsay`, a read-only bind mount of `./config`, through
`HEARSAY_CONFIG`. Only the API and the connectors are published, on the host's
loopback interface only. The distiller and the assertion worker have no port
outside the compose network, and neither does Postgres.

## First deployment

On a host with Docker Engine and a current Compose v2 plugin
(`compose-smoke` below runs v2.40):

1. Copy `deploy/compose/` to the host. That directory is the compose project.
2. Pick the image (next section).
3. Write the configuration into `config/`, replacing the example (see
   [Configuration](#configuration)).
4. Write `.env` (see [The env file](#the-env-file)) and `chmod 600 .env`.
5. Start it:

   ```sh
   docker compose up -d --wait
   docker compose ps          # migrate: exited (0); the rest: healthy
   ```

   `--wait` returns once every service is healthy, and fails if one is not.
6. Put the TLS proxy in front (see [TLS](#tls-in-front-of-the-api)), and point
   the webhooks at it (see [Webhook ingress](#webhook-ingress)).

## Choosing the image

The image is `ghcr.io/kpenfound/hearsay:<version>`, one multi-architecture
manifest for `linux/amd64` and `linux/arm64`. The same image runs every
service and the migration job; the subcommand is the argument. The tag is the
version stamped into the binary, such as `v0.10.0`. There is no `latest`, so an
upgrade is always a deliberate choice of version. A fork that publishes its own
(CONTRIBUTING.md, "Publishing a release image") names its own address.

Set `HEARSAY_IMAGE` in `.env` to the whole reference. For a deployment that
must not change under you, use the digest that `publish` printed:
`ghcr.io/kpenfound/hearsay:v0.10.0@sha256:…`. Check what you are running with:

```sh
docker compose run --rm --no-deps api version
```

The Postgres image is `pgvector/pgvector:pg16` in `compose.yaml`. Pin it to a
digest there too for production. Any pgvector build of Postgres 16 or later
works (ADR-0004). A stock Postgres image fails the first migration, which
creates the `vector` extension. A major-version change of Postgres is a dump
and restore, not a tag edit.

## Configuration

`./config` is the configuration repository: either one `hearsay.yaml` or the
directory form. [docs/config.md](config.md) is the schema. The example in
`deploy/compose/config/` exists so the stack starts with no source credentials.
It ingests nothing but agent sessions and its principals are placeholders, so
replace it.

`hearsay init` writes a team's first configuration and its env file. It needs
no database, and it runs from the image:

```sh
docker run --rm -it --user "$(id -u):$(id -g)" -v "$PWD:/work" -w /work \
  -e HEARSAY_GITHUB_TOKEN \
  "$HEARSAY_IMAGE" init --out config/hearsay.yaml --env-file hearsay.env
docker run --rm -v "$PWD/config:/etc/hearsay:ro" "$HEARSAY_IMAGE" config validate /etc/hearsay
```

The services run as `nobody` (uid 65534), so every file in `config/` has to be
readable by others (`chmod -R a+rX config`). Keep secrets out of it: it is
mounted into every service, and a configuration names environment variables,
never their values.

## The env file

`.env` sits beside `compose.yaml`. Compose reads it twice: to fill in the
`${...}` references in `compose.yaml`, and as the `env_file` of the four
Hearsay services. Postgres receives only `POSTGRES_PASSWORD`, and the
migration job only `HEARSAY_DATABASE_URL`. Start from `env.example`, or from
the `hearsay.env` that `hearsay init` wrote, plus the deployment block of
`env.example`.

| Variable | Read by | Value |
|---|---|---|
| `HEARSAY_IMAGE` | compose | **Supply.** The image reference. `env.example`'s value is an example. |
| `POSTGRES_PASSWORD` | `postgres` | **Supply** (`openssl rand -hex 32`). Used only when the volume is first created; changing it later is an `ALTER ROLE`. |
| `HEARSAY_DATABASE_URL` | `migrate`, all four services | **Supply**: `postgres://hearsay:<password>@postgres:5432/hearsay?sslmode=disable`, with the password percent-encoded if it needs it. |
| The `token_env` of every human and agent | `api`, all of them; `connectors`, the agents', when an `agent` source is configured | **Supply** one random, distinct value each (`openssl rand -hex 32`). `hearsay init` generates them. The names are the configuration's; `HEARSAY_OPERATOR_TOKEN` and `HEARSAY_ASSISTANT_TOKEN` are the example's. |
| `ANTHROPIC_API_KEY`, or a tier's `api_key_env` | `distiller`, `assert-worker`, `api` | **Supply.** Each builds the model tiers at startup and refuses to start without the key. |
| `HEARSAY_GITHUB_TOKEN` | `connectors`; `assert-worker` for a GitHub source that is not `read_only` or that `code/` names | **Supply** if the configuration names it. |
| `HEARSAY_GITHUB_WEBHOOK_SECRET` | `connectors` | **Supply** if the configuration names it: the secret the webhooks sign with. |
| `HEARSAY_DISCORD_TOKEN` | `connectors`; `api` for a Discord source with `application_id` and `public_key` that is not `read_only` | **Supply** if the configuration names it. |
| `HEARSAY_DRIVE_CREDENTIALS` | `connectors` | **Supply** if the configuration names it: the service account's JSON key, on one line, in single quotes. |
| `HEARSAY_LOG_LEVEL`, `HEARSAY_LOG_FORMAT` | all four services | Optional. `info` and JSON (the format when there is no terminal) by default. |
| `HEARSAY_INSTANCE` | all four services | Optional. The replica name on every log line; the container's hostname by default. |

The source-credential names are the ones `hearsay init` writes. A configuration
can name any variable in `secrets:` and `token_env`, and whatever it names is
what the services read. Every service receives all of `.env`, because only the
configuration knows which names it uses.

`HEARSAY_CONFIG` is set by `compose.yaml` and does not belong in `.env`.
Neither do the listen addresses (`HEARSAY_API_LISTEN`, `HEARSAY_LISTEN`,
`HEARSAY_DISTILLER_LISTEN`, `HEARSAY_ASSERT_WORKER_LISTEN`): the ports and
healthchecks assume the defaults.

Compose expands `$` in an unquoted value, so single-quote a value that contains
one. Keep `.env` mode 0600 and out of version control. Whoever can read it, or
run `docker compose config` in the project, holds every credential Hearsay has.

## Migrations

Migrations run only in the `migrate` job, never when a service starts
([ADR-0006](adr/0006-schema-migrations-with-goose.md)). The job runs
`hearsay migrate up` from the image being deployed. The services depend on it
completing successfully, so a failed migration leaves them stopped or on the
old version, and `docker compose logs migrate` says why. Each service checks
the schema version at startup and refuses a database that is behind its
binary. A database ahead of the binary is accepted, which is what lets a
rollout migrate first. Two concurrent `migrate up` runs are safe: goose's
advisory lock makes the second wait.

`docker compose up` runs the job every time. With nothing pending it applies
nothing and exits 0. To look without changing anything:

```sh
docker compose run --rm migrate migrate status
```

To roll forward in steps, which a release note may ask for, run
`docker compose run --rm migrate migrate up-to <n>` before bringing up the new
image. `migrate down` refuses without `--i-know` and is for development:
recovery in production is a restore.

Back up before an upgrade. Everything Hearsay has is in the one database
(ADR-0004):

```sh
docker compose exec -T postgres pg_dump -U hearsay -Fc hearsay > hearsay-$(date +%F).dump
```

## Upgrading

1. Read the release notes for anything that must happen in steps.
2. Back up.
3. Set `HEARSAY_IMAGE` to the new version in `.env`.
4. `docker compose up -d --wait`.

Compose pulls the image and runs `migrate` from it. The services still running
the old image keep running meanwhile, then are recreated on the new one. Every
migration is written so the binary already deployed keeps working against the
new schema (expand and contract, ADR-0006). A rollback is the old
`HEARSAY_IMAGE` and `docker compose up -d`, as long as no migration since has
removed anything that version needs. That is why a destructive change always
comes a release after the one that stopped using what it removes.

## TLS in front of the API

The API authenticates every call with a bearer token per principal
([ADR-0014](adr/0014-api-callers-use-per-principal-bearer-tokens.md)) and
serves plain HTTP. Anyone who sees a request can replay the token, so the API
is reached only through a TLS terminator. That is why `compose.yaml` publishes
it on `127.0.0.1` and not on every interface. Put the proxy on the same host,
or add it to the compose project and remove the published port. The hop from
the proxy to Hearsay has to be one nobody else can read: loopback, or the
compose network.

Health endpoints are unauthenticated, and the connectors' `/readyz` names every
source and its status. Route only what callers and sources need through the
proxy. With [Caddy](https://caddyserver.com), which gets and renews its own
certificate:

```caddy
hearsay.example.com {
	handle /hooks/* {
		reverse_proxy 127.0.0.1:8081
	}
	handle /v1/* /mcp /mcp/* /discord/* {
		reverse_proxy 127.0.0.1:8080
	}
	respond 404
}
```

Any proxy works. It has to pass request headers and bodies through
unchanged: the API reads `Authorization` and the `Hearsay-*` headers, and
GitHub webhooks and Discord interactions are verified by a signature over the
exact bytes. It also has to allow at least 30 seconds for a response, because
`watch` holds a request open for up to 25 seconds. `hearsay.example.com` is an
example hostname.

## Webhook ingress

The connectors service polls or streams most sources, and those need only
outbound access. The sources that push to Hearsay need a public HTTPS URL
through the proxy:

| Source | Delivers to | Configure |
|---|---|---|
| GitHub (`type: github`) | `https://<host>/hooks/<source id>` on the connectors | A repository or organization webhook, content type `application/json`, the secret in the configuration's `webhook_secret` variable, for the events [docs/config.md](config.md#github-source) lists. |
| Google Drive (`type: drive`) | `https://<host>/hooks/<source id>` on the connectors | `settings.notification_url`, set to that URL. Without it the connector polls on `refresh`. |
| Agent sessions (`type: agent`) | `https://<host>/hooks/<source id>` on the connectors | Agents post their session events there with their own bearer token. An agent inside the compose network can post to `http://connectors:8081/hooks/<source id>` instead. |
| Discord commands | `https://<host>/discord/<source id>/interactions` on the **API** | The application's Interactions Endpoint URL in the Discord developer portal, for a Discord source whose settings name `application_id` and `public_key` and that is not `read_only`. Discord's messages and reactions arrive over the Gateway, which is outbound. |

Obsidian (`type: obsidian`) reads a vault from disk. Mount it read-only into
the connectors container at the configuration's `settings.root` with a
`compose.override.yaml` beside `compose.yaml`, which Compose reads on its own:

```yaml
services:
  connectors:
    volumes:
      - /srv/vault:/mnt/vault:ro
```

The vault has to be readable by uid 65534.

## Replicas and serialization

Compose runs one container per service, which is enough for a team. Every
service keeps its state in Postgres, so replicas never disagree about where
they are. What more than one buys differs:

| Service | More than one | Why |
|---|---|---|
| `api` | Yes, behind a load balancer. | Stateless. Each replica has its own bundle cache and registers the same Discord commands at startup, which is harmless. |
| `distiller` | Yes, to distil faster. | Replicas share one position in the L0 change feed and claim `distill` jobs from the queue, and jobs for one document are deduplicated. Each replica runs 4 model calls at once, so the provider's rate limit is the ceiling, not CPU. |
| `assert-worker` | Safe, rarely useful. | `assert` jobs are serialized per scope by the queue (ADR-0007): one scope's stances are written one at a time, whatever the replica count. More replicas help only across many busy scopes. Each replica seeds entities at startup. |
| `connectors` | Not by copying it. | Two replicas hosting the same source both poll it and both hold its stream, such as a second Discord Gateway session. Appends are idempotent, so nothing is duplicated, but every call is made twice. Scale out by splitting sources instead: one container per group of sources, each started with `connectors --source <id>`, and route each source's `/hooks/<id>` to the container that hosts it. |
| `migrate` | Never needed. | A job. A second concurrent run waits on the first. |
| `postgres` | One. | Hearsay has one database (ADR-0004). Replication and failover are Postgres's, below Hearsay. |

Splitting the connectors is an override that adds a service, not an edit to
`compose.yaml`:

```yaml
# compose.override.yaml: Discord in a container of its own.
services:
  connectors:
    command: ["connectors", "--source", "github", "--source", "agent-sessions"]
  connectors-discord:
    extends: {file: compose.yaml, service: connectors}
    command: ["connectors", "--source", "discord"]
    ports: !reset []
```

`extends` copies the service, including its healthcheck on `/readyz`.
`!reset []` drops the published port, which only one container can hold. The
source ids are examples.

## Changing configuration

Configuration is read once, at startup ([ADR-0009](adr/0009-configuration-as-a-gitops-directory.md)).
A change is a restart, and a service that finds it invalid refuses to start
rather than running on part of it. Roll it like a deploy:

1. Validate before anything restarts:

   ```sh
   docker run --rm -v "$PWD/config:/etc/hearsay:ro" "$HEARSAY_IMAGE" config validate /etc/hearsay
   ```

   It prints the configuration's digest.
2. Restart the services. A change to a file in `config/` does not change the
   compose project, so Compose does not notice it. Restart one service at a
   time and wait for it to be healthy before the next:

   ```sh
   for s in connectors distiller assert-worker api; do
     docker compose restart "$s" && docker compose up -d --wait --no-deps "$s"
   done
   ```

   A change to `.env` does change the project: `docker compose up -d --wait`
   recreates exactly the services that read it. A new token, a rotated
   credential or a new principal's `token_env` needs its variable in `.env`
   before the restart, or the `api` (and, for an agent, the `connectors`) will
   refuse to start.
3. Confirm every service is running the new configuration. Each logs
   `configuration loaded` with a `config_digest` at startup, which has to equal
   the digest `config validate` printed:

   ```sh
   docker compose logs --no-log-prefix connectors distiller assert-worker api | grep config_digest
   ```

A service that refused to start keeps restarting with the reason in its log,
and the old containers are already gone. Fix the configuration and restart
again, or put the previous configuration back.

## Checking a deployment

```sh
docker compose ps                                   # migrate exited 0; four services healthy
curl -s 127.0.0.1:8081/readyz                       # each source's own status
curl -s 127.0.0.1:8080/readyz
curl -s -X POST 127.0.0.1:8080/v1/get_bundle \
  -H 'Hearsay-Principal: operator' -H "Authorization: Bearer $HEARSAY_OPERATOR_TOKEN" \
  -d '{"scope":"team"}'
```

`operator` and `team` are the example configuration's. Use a principal and a
scope of your own.

For contributors, `dagger check hearsay:compose-check` validates
`deploy/compose` with Compose's own parser and holds it to the shape described
here. It needs no Docker daemon. `dagger api call hearsay compose-smoke` runs
the stack for real, in a Docker daemon of its own, from the image built from the
working tree. It checks that the migration job succeeds, the four services
become healthy, only 8080 and 8081 are published, and an authenticated
`get_bundle` answers 200.
