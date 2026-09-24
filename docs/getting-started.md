# Getting started: from nothing to a first bundle

This guide takes a team using GitHub, Discord and Google Drive from source
credentials to its first authenticated context bundle. It uses the published
Compose image and keeps secrets in an untracked `.env` file.

## 1. Prepare source access

You will need an operator who can create source credentials and configure
webhooks. Hearsay does not create these credentials for you.

- **GitHub:** create a fine-grained token or GitHub App installation token with
  Metadata and Contents read access, and Issues and Pull requests read and write
  access. A classic token needs `repo` (`public_repo` is enough only for public
  repositories). Choose a random webhook secret and keep it for the webhook
  configuration. After deployment, point each repository webhook to
  `https://<public-host>/hooks/github`, use `application/json`, the same secret,
  and the `issues`, `issue_comment`, `pull_request`, `pull_request_review`,
  `pull_request_review_comment`, `push`, `repository` and `sub_issues` events.
  See the [GitHub connector package comments](https://github.com/kpenfound/hearsay/blob/main/internal/connector/github/github.go)
  for the exact contract and read-only alternative.
- **Discord:** create a bot, enable the privileged Message Content intent, and
  grant it View Channel and Read Message History for the channels to ingest.
  `hearsay init` configures message and reaction Gateway intents. Slash commands
  are optional: to enable them, add the application's `application_id` and
  `public_key` to the generated Discord source, invite with `bot` and
  `applications.commands`, and set the Interactions Endpoint URL to
  `https://<public-host>/discord/discord/interactions`. With `read_only: true`,
  slash commands and the interactions endpoint are not used. See the
  [Discord connector package comments](https://github.com/kpenfound/hearsay/blob/main/internal/connector/discord/discord.go)
  for intents, permissions and command details.
- **Slack (optional):** create an app from the manifest in the
  [Slack connector package comment](https://github.com/kpenfound/hearsay/blob/main/internal/connector/slack/slack.go).
  Generate an app-level token with `connections:write`, install the app,
  and invite it to each public channel. Pass `--slack-workspace T…` and
  `--slack-channel C…` to `init`; export `HEARSAY_SLACK_BOT_TOKEN` to verify
  channels and, with `users:read` and `users:read.email`, match identities.
  The generated env file leaves both Slack token entries empty.
- **Drive:** create a Google Cloud service account, enable the Drive API, and
  share every configured folder and document with the service account's email.
  Native Google Docs need owner, organizer, file organizer or writer access to
  read revisions. `hearsay init` leaves an empty env entry for the service
  account JSON key; it never copies the credential it read. See the
  [Drive connector package comments](https://github.com/kpenfound/hearsay/blob/main/internal/connector/drive/drive.go)
  and [Drive configuration details](config.md#google-drive-source).

The GitHub and Discord callback URLs need a public HTTPS address through a TLS
proxy. Compose binds the API and connectors to loopback; see
[Webhook ingress](deploy.md#webhook-ingress) before exposing either endpoint.
Source setup details and optional settings are in [docs/config.md](config.md).

## 2. Generate the team configuration

Use a published image tag; `deploy/compose/env.example` shows the current
example tag and deployment variables. From the repository root:

```sh
export HEARSAY_IMAGE=ghcr.io/kpenfound/hearsay:v0.10.0
cd deploy/compose
docker run --rm -it --user "$(id -u):$(id -g)" -v "$PWD:/work" -w /work \
  -e HEARSAY_GITHUB_TOKEN -e HEARSAY_DISCORD_TOKEN -e HEARSAY_DRIVE_CREDENTIALS \
  "$HEARSAY_IMAGE" init \
  --operator kyle --operator-name "Kyle Penfound" --operator-github kpenfound \
  --github-repo acme/api \
  --discord-guild 824100000000000000 --discord-channel 824100000000000001 \
  --drive-folder 1AbCdEfGhIjKlMnOpQrStUvWxYz \
  --out config/hearsay.yaml --env-file hearsay.env
```

Replace the sample IDs and names with your own. Export the three source
credentials in the shell before running this command so `init` can look up
collaborators and confirm any linked identities. `init` writes
`config/hearsay.yaml` with sources, scopes, principals, identities and policy
defaults. It writes `hearsay.env` with distinct generated API tokens and empty
source/model credential entries. It needs no database. The generated file is
mode 0600; keep it and `.env` out of version control.

Start the Compose env file and copy the generated principal token assignments
from `hearsay.env` into `.env`:

```sh
cp env.example .env
chmod 600 .env
```

Edit `.env`: set `HEARSAY_IMAGE`, `POSTGRES_PASSWORD`, and
`HEARSAY_DATABASE_URL` as described in [The env file](deploy.md#the-env-file);
copy every generated `HEARSAY_<PRINCIPAL>_TOKEN` assignment from `hearsay.env`;
and fill `ANTHROPIC_API_KEY`, `HEARSAY_GITHUB_TOKEN`,
`HEARSAY_GITHUB_WEBHOOK_SECRET`, `HEARSAY_DISCORD_TOKEN` and
`HEARSAY_DRIVE_CREDENTIALS`. Leave out source variables for sources you did not
configure. For a Drive key, keep the JSON on one line and single-quote a value
that contains `$` for Compose. The generated token names in the config are
authoritative; do not use the example operator and assistant token names unless
your config uses them.

Review `config/hearsay.yaml`. In particular, add Discord `application_id` and
`public_key` under its `settings` if you want slash commands; set
`read_only: true` there if Discord should not provide commands or gestures.
Validate the config before starting:

```sh
docker run --rm -v "$PWD/config:/etc/hearsay:ro" "$HEARSAY_IMAGE" config validate /etc/hearsay
```

## 3. Start Compose and check readiness

Run these commands from `deploy/compose`:

```sh
docker compose up -d --wait
docker compose ps
curl -fsS 127.0.0.1:8081/readyz
curl -fsS 127.0.0.1:8080/readyz
```

The migration job should exit 0 and the four services should be healthy. The
connectors readiness response reports each source's status. Configure the
GitHub webhooks and, if using Discord slash commands, its Interactions Endpoint
URL now that the TLS proxy is routing to this deployment.

## 4. Watch backfill and distillation

Connectors begin backfilling configured sources on startup. Check the L0 event
count using the Compose image and the service environment:

```sh
docker compose run --rm --no-deps api l0 count
docker compose logs -f connectors distiller assert-worker
```

The count should rise as sources are read. The distiller turns eligible L0
events into L1 documents automatically; look for `document distilled` in the
distiller logs. The assertion worker then derives topics and stances from
decision-bearing documents. This requires a working Anthropic API key. Agent
session events are not distilled. L0 is the input event store, not the bundle
itself, so allow the workers to catch up before checking for useful content.

## 5. Map remaining people

Initial setup can confirm linked identities, but people with no confirmed match
remain unmapped. List those identities after backfill:

```sh
docker compose run --rm --no-deps api identities list --yaml
```

For each person, add the suggested source identity to that human's entry in
`config/hearsay.yaml`. `--yaml` prints principal fragments to paste into the
configuration. Identity suggestions are based on source identity hints; review
them before assigning an account. Restart the services after editing config
(the deployment guide describes a safe restart).

## 6. Request the first bundle

Load the env values into the shell, then call `get_bundle`. Use the scope id
written by `init` (for this example, `api`):

```sh
set -a
. ./.env
set +a
curl -fsS -X POST http://127.0.0.1:8080/v1/get_bundle \
  -H 'Content-Type: application/json' \
  -H 'Hearsay-Principal: kyle' \
  -H "Authorization: Bearer $HEARSAY_KYLE_TOKEN" \
  -d '{"scope":"api"}'
```

An agent acting for a human sends the human bearer token plus its own token and
identity. Use these headers on an MCP client connecting to
`https://<public-host>/mcp` (or on a direct `/v1/get_bundle` request):

```text
Hearsay-Principal: kyle
Authorization: Bearer <human token>
Hearsay-Agent: reviewer
Hearsay-Agent-Token: <agent token>
```

Configure the MCP client to POST to `/mcp` with those headers and the agent's
MCP transport. The human and agent tokens must be distinct values from their
respective `token_env` entries. Put TLS in front of the API; bearer tokens must
not travel over an untrusted plain HTTP connection. See
[ADR-0014](adr/0014-api-callers-use-per-principal-bearer-tokens.md) and
[the API authentication section](config.md#principals).

## QA without live sources or a model

For a dependency-free deployment check, run `dagger api call hearsay
compose-smoke`. It starts the example Compose configuration, waits for the
migration and services, then verifies an authenticated `get_bundle` returns
HTTP 200. That example uses only the agent session source and needs no source
credentials or model provider. It verifies the first bundle request and
deployment wiring; exercising source backfill and meaningful distilled
content requires operator-provided GitHub, Discord and Drive credentials plus
an Anthropic API key as described above.
