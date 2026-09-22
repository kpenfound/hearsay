# 14. API callers use per-principal bearer tokens

- Status: accepted
- Date: 2026-09-22

## Context

The API filters reads for the human named by `Hearsay-Principal`, and, when
present, the agent named by `Hearsay-Agent`. Those names were supplied by the
request without proof. A caller could name a person who can read a private
document and receive it. Both `POST /v1/<call>` and `/mcp` had the gap (#76).

The configuration repository already maps source identities to Hearsay
principals. It may be checked in, so it must name credentials rather than hold
them, as `sources[].secrets` does.

## Decision

Each human and agent that can call the API has a `token_env` in `principals/`:
the name of an environment variable holding a distinct, random bearer token.
The API resolves all of them at startup. It refuses to start if a human or
agent lacks a name, its variable is missing or empty, or two credentials have
the same value. Teams cannot call the API. Other services may load the same
identity mapping without these credentials; `hearsay config validate` checks
the environment variable's name, not its deployment value.

A direct human request sends `Hearsay-Principal: <human id>` and
`Authorization: Bearer <human token>`. An agent request also sends
`Hearsay-Agent: <agent id>` and `Hearsay-Agent-Token: <agent token>`. Both
credentials must match the respective configured principals. The human token
is the person's delegation to the agent; the agent token proves which agent
is acting. The existing effective-principal calculation still intersects the
human's and agent's grants and applies the source ACLs. A human token holder
can call directly; issuance and custody of that token are the deployment's
responsibility.

Both HTTP transports reject absent or mismatched credentials with HTTP 401,
before the shared call layer sees the request. An unknown name or a name of
the wrong kind receives HTTP 403. MCP authentication failure is an HTTP error,
not a successful JSON-RPC response containing a tool error. Health and
readiness endpoints remain unauthenticated. Tokens are never logged or placed
in the configuration repository; only their hashes are retained for request
comparison. Token changes require an API restart, consistent with ADR-0009.

Bearer tokens require TLS between the caller and the TLS terminator, and a
protected hop from there to Hearsay. This decision does not add transport
encryption to the service.

## Alternatives considered

- **mTLS at the API.** Certificates would prove clients strongly, but would
  require a certificate issuer, rotation and TLS configuration in the service,
  even where a deployment already terminates TLS at a proxy. Mapping two
  certificates to one agent-on-behalf-of-human request would still need a
  delegation contract.
- **Trusted-proxy headers plus a shared secret.** This makes the proxy the
  identity provider, but one shared secret authenticates the proxy, not the
  person or agent it names. A misrouted or compromised proxy could assert any
  principal, recreating the original failure at another hop. It also requires
  a proxy-specific header-stripping contract.
- **An agent token alone with `Hearsay-Principal`.** It proves the agent but
  lets any agent choose any human, so the intersection is bounded by an
  unverified human identity. Requiring the human's token closes that gap.

## Consequences

- Existing clients must obtain a human token and send it with each read. Agent
  integrations must obtain both credentials. A token holder can use all of its
  principal's current grants; this version has no narrower, expiring
  delegation. A token's compromise requires replacing its value and restarting
  the API.
- An API process with no configuration can still start and serve health
  endpoints, but serves no authenticated calls. A process with configured
  humans or agents and missing credentials fails startup instead of serving
  unauthenticated reads.
