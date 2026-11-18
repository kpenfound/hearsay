# internal/tailnet

The embedded Tailscale node a service can serve on, beside its own listener, so
that callers on the owner's tailnet reach it at
`https://<hostname>.<tailnet>.ts.net` with no proxy. The API is the one service
that uses it (`hearsay api --tailnet`).

What belongs here: joining (`tsnet`), waiting for approval, choosing HTTPS on
443 with the node's certificate or HTTP on 80, and serving a handler the
caller already built. Retrying, and reporting the node's state.

What does not:

- **Authorization.** The tailnet decides who can connect, never who may read.
  The handler a service passes still checks every bearer token.
- **Dialing out.** Nothing reaches a model, Jev or the database through the
  node; that traffic uses the host's network.
- **Failing the service.** A node that cannot join, is not approved or loses
  its listener is logged and retried. The service's own listener keeps serving,
  and its readiness is about its database, not the tailnet.

`TS_AUTHKEY` is read on the first login only and is redacted from every log
line Tailscale writes. The node's state directory must persist; without it every
start is a new node to approve. Tests use a fake `Backend`, never a tailnet.
