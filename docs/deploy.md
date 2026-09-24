# Deploying Hearsay

How to run Hearsay for a team: on one host with Docker Compose, or on
Kubernetes with the Helm chart ([Kubernetes with Helm](#kubernetes-with-helm)).
Most of this guide is about Compose. The chart deploys the same shape, and the
sections on the image, the environment, TLS, webhooks and replicas apply to
both.

The Compose files are in [`deploy/compose/`](../deploy/compose/):

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
| `HEARSAY_SLACK_APP_TOKEN`, `HEARSAY_SLACK_BOT_TOKEN` | `connectors` | **Supply** if the configuration names them: the Slack app's app-level token (`xapp-…`) and bot token (`xoxb-…`). |
| `HEARSAY_TRACKER_TOKEN` | `connectors` | **Supply** if the configuration names it: the bearer token a generic tracker source's sender posts with (`openssl rand -hex 32`). |
| `HEARSAY_DRIVE_CREDENTIALS` | `connectors` | **Supply** if the configuration names it: the service account's JSON key, on one line, in single quotes. |
| `HEARSAY_LOG_LEVEL`, `HEARSAY_LOG_FORMAT` | all four services | Optional. `info` and JSON (the format when there is no terminal) by default. |
| `HEARSAY_INSTANCE` | all four services | Optional. The replica name on every log line; the container's hostname by default. |

The source-credential names are the ones `hearsay init` writes, and for the
generic tracker the one [docs/config.md](config.md#generic-tracker-source) uses. A configuration
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
outbound access. Slack is one of them: its Socket Mode connection is dialed from
the connectors, so it needs no ingress. The sources that push to Hearsay need a public HTTPS URL
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

## Kubernetes with Helm

The chart is [`deploy/helm/hearsay/`](../deploy/helm/hearsay/). It is not
published to a chart repository: install it from a checkout of the release you
are deploying. [`values.yaml`](../deploy/helm/hearsay/values.yaml) documents
every value.

It deploys the Compose shape, minus Postgres. `<name>` below is
`<release>-hearsay`, or just the release name if it already contains
`hearsay`:

| Object | What it is |
|---|---|
| Job `<name>-migrate` | `hearsay migrate up`, a pre-install and pre-upgrade hook. |
| Deployment and Service `<name>-connectors` | `hearsay connectors`, port 8081: `/hooks/<source id>`, `/healthz`, `/readyz`. |
| Deployment `<name>-distiller` | `hearsay distiller`, port 8082, probes only. No Service. |
| Deployment `<name>-assert-worker` | `hearsay assert-worker`, port 8083, probes only. No Service. |
| Deployment and Service `<name>-api` | `hearsay api`, port 8080: `/v1/<call>`, `/mcp`, `/discord/<source id>/interactions`, `/healthz`, `/readyz`. |
| ConfigMap `<name>-config` | The configuration, unless it comes from `config.volume`. |
| Ingress `<name>` | Optional, off by default. |

Every service's pod has a startup and a liveness probe on `/healthz` and a
readiness probe on `/readyz`. The Services are `ClusterIP`. The pods run as `nobody` with
a read-only root filesystem, no privilege escalation, no capabilities and the
`RuntimeDefault` seccomp profile. They get no service account token, because
Hearsay never calls the Kubernetes API, and no service-link variables, which
would otherwise put `<SERVICE>_PORT` variables starting with `HEARSAY_` into a
release named `hearsay`.

### What you supply

The chart refuses to render without three values: `image.tag`,
`database.existingSecret` and a configuration. It never takes a secret as a
value and renders no Secret. Every credential is a reference to a Secret you
create first.

1. **Postgres** 16 or later with pgvector (ADR-0004), reachable from the
   cluster: a managed database, or one your Postgres operator runs. The chart
   runs no database. Its migration hook runs before any other object in the
   release exists, so a database in the same release would not be there for it
   yet.
2. **The database URL**, in a Secret:

   ```sh
   kubectl create secret generic hearsay-database \
     --from-literal=HEARSAY_DATABASE_URL='postgres://hearsay:<password>@db.internal:5432/hearsay?sslmode=require'
   ```

   `database.key` names the key if it is not `HEARSAY_DATABASE_URL`. The
   migration job reads this and nothing else.
3. **Everything else the services read** ([The env file](#the-env-file) lists
   it): the API tokens, the model key and the source credentials. The
   simplest form is one Secret from the env file `hearsay init` wrote, plus
   `ANTHROPIC_API_KEY`:

   ```sh
   kubectl create secret generic hearsay-env --from-env-file=hearsay.env
   ```

   `kubectl` takes every value literally. Remove the single quotes around
   `HEARSAY_DRIVE_CREDENTIALS` first, or they become part of the key.
4. **A values file**:

   ```yaml
   image:
     tag: v0.10.0
     # digest: sha256:…   # the digest `publish` printed
   database:
     existingSecret: hearsay-database
   existingSecrets: [hearsay-env]
   ```

Then validate the configuration, as for Compose, and install:

```sh
helm install hearsay deploy/helm/hearsay -f values.yaml \
  --set-file config.hearsayYaml=config/hearsay.yaml --wait
```

The values that decide where credentials go:

| Value | What it does |
|---|---|
| `existingSecrets` | Secrets every service reads whole (`envFrom`): each key becomes a variable of that name. |
| `secretEnv` | Single keys: `VARIABLE: {secret: <name>, key: <key>}`. |
| `env` | Plain values that are not secret, such as `HEARSAY_LOG_LEVEL`. |
| `connectors.*`, `distiller.*`, `assertWorker.*`, `api.*` | Each takes its own `existingSecrets`, `secretEnv` and `env`, added to the shared ones. |

Every service gets the shared ones, as every Compose service gets all of
`.env`. To keep a credential away from the services that do not read it, put it
under the service that does, using the "Read by" column of the env table. For
example, the GitHub webhook secret goes under `connectors.secretEnv`. The chart
sets `HEARSAY_CONFIG`, `HEARSAY_DATABASE_URL` and the listen addresses itself,
and refuses them in any of these.

### Configuration

Set exactly one of:

| Value | Form |
|---|---|
| `config.hearsayYaml` | The single file, usually `--set-file config.hearsayYaml=hearsay.yaml`. Rendered into the ConfigMap. |
| `config.files` | The directory form, as a map from path to contents (`sources/github.yaml: \|` …). Rendered into the ConfigMap and mounted with each file at its path. A ConfigMap key cannot hold a slash, so `sources/github.yaml` is stored under the key `sources.github.yaml`. |
| `config.volume` | Any volume source, such as `configMap: {name: team-config}` for a ConfigMap you manage, or a PersistentVolumeClaim. |

Whichever it is, it is mounted read-only at `/etc/hearsay`. Keep secrets out of
it, for the same reason as with Compose.

### Migrations

The hook Job runs `hearsay migrate up` from the image being deployed
([ADR-0006](adr/0006-schema-migrations-with-goose.md)). It runs before
`helm install` creates the services and before `helm upgrade` changes
anything. Helm waits for it, and if it fails the install or upgrade fails with
nothing rolled. The services check the schema at startup, as they do under
Compose, and no service migrates for itself.

The Job is kept after it succeeds, so its log can be read, and deleted when the
next install or upgrade creates a new one:

```sh
kubectl logs job/hearsay-migrate
kubectl exec deploy/hearsay-api -- hearsay migrate status
```

Helm waits for the hook for `--timeout` (5 minutes by default), and the Job
gives up after `migrate.activeDeadlineSeconds` (600). Raise both for a release
whose notes warn of a long migration. `migrate.enabled=false` leaves migrating
to you. `helm rollback` runs no hook: the old image runs against the newer
schema, which expand and contract allows, as with a Compose rollback.

### Upgrading and changing configuration

An upgrade is the new `image.tag` and the same configuration flags as before:

```sh
helm upgrade hearsay deploy/helm/hearsay -f values.yaml \
  --set image.tag=v0.11.0 --set-file config.hearsayYaml=config/hearsay.yaml --wait
```

Back up first. The hook migrates, then the Deployments roll.

Configuration is read once, at startup (ADR-0009), so a change has to replace
the pods:

- With `config.hearsayYaml` or `config.files`, every pod carries a checksum of
  the ConfigMap. A `helm upgrade` with a changed configuration rolls all four
  Deployments.
- With `config.volume`, the chart cannot see the contents. Set `config.digest`
  to the digest `hearsay config validate` printed. It is a pod annotation, so a
  new digest rolls the pods. Or restart them yourself.
- A changed Secret rolls nothing. Restart after rotating a credential, and
  update the Secret before the configuration when you add a principal:

  ```sh
  kubectl rollout restart deployment -l app.kubernetes.io/instance=hearsay
  ```

A service that finds the configuration invalid exits, so its new pod restarts
and never becomes ready. The rolling update keeps the old pod serving until the
new one is ready, so the old configuration stays up and `helm upgrade --wait` fails.
Fix the configuration and upgrade again, or `helm rollback`. To confirm the
rollout, compare the `config_digest` each pod logs at startup with the one
`config validate` printed:

```sh
kubectl logs -l app.kubernetes.io/instance=hearsay --tail=-1 | grep config_digest
```

### Ingress and TLS

With `ingress.enabled=true` the chart renders one Ingress for `ingress.host`.
It routes `/hooks` to the connectors and `/v1`, `/mcp` and `/discord` to the
API, as the Caddy example does. The health endpoints stay inside the cluster.

TLS is on by default. `ingress.tls.secretName` is required, and names an
existing TLS Secret or the one cert-manager writes for an issuer you name in
`ingress.annotations`. Set `ingress.tls.enabled=false` only when something in
front of the controller terminates TLS: bearer tokens cross this hop. The
requirements on the proxy still hold. It must pass headers and bodies through
unchanged, and allow at least 30 seconds for a response. With ingress-nginx,
the default 60-second `proxy-read-timeout` is enough.

Without the Ingress, the API and the webhooks are reachable only inside the
cluster, at `http://hearsay-api:8080` and `http://hearsay-connectors:8081`.
The webhook sources in [Webhook ingress](#webhook-ingress) need a public HTTPS
URL, so put whatever ingress you use in front of the connectors' Service.

### Replicas

Every Deployment defaults to one replica. `api.replicas`, `distiller.replicas`
and `assertWorker.replicas` can be raised, as
[Replicas and serialization](#replicas-and-serialization) describes. Keep
`connectors.replicas` at 1. `connectors.sources` limits the connectors
Deployment to some of the configured sources (`--source`). The chart runs one
connectors Deployment, so splitting sources across several is not something it
does.

A connectors pod whose `/readyz` fails, because a source reports itself
failed, leaves its Service's endpoints until the source recovers. While it is
out, no webhook reaches that pod, for any source. `kubectl get pods` shows it
0/1, and its `/readyz` names the source.

To mount an Obsidian vault into the connectors, use `connectors.extraVolumes`
and `connectors.extraVolumeMounts`, read-only at the source's `settings.root`.

### Checking a Kubernetes deployment

```sh
kubectl get pods,jobs -l app.kubernetes.io/instance=hearsay   # migrate complete; four pods 1/1
kubectl port-forward svc/hearsay-api 8080 &
curl -s -X POST 127.0.0.1:8080/v1/get_bundle \
  -H 'Hearsay-Principal: operator' -H "Authorization: Bearer $HEARSAY_OPERATOR_TOKEN" \
  -d '{"scope":"team"}'
```

## For contributors

`dagger check hearsay:helm-check` lints the chart and renders it with the
values files in `deploy/helm/ci/`, for install and upgrade. It holds the output
to the shape described here and checks that the chart refuses what it must.
It also lays each config form out the way the kubelet mounts a ConfigMap and
runs `hearsay config validate` on it. It needs no cluster.

`dagger check hearsay:compose-check` validates
`deploy/compose` with Compose's own parser and holds it to the shape described
here. It needs no Docker daemon. `dagger api call hearsay compose-smoke` runs
the stack for real, in a Docker daemon of its own, from the image built from the
working tree. It checks that the migration job succeeds, the four services
become healthy, only 8080 and 8081 are published, and an authenticated
`get_bundle` answers 200.
