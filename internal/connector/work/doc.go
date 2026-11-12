// Package work accepts projects, workstreams, updates, documents and decisions
// posted by factories and agents to POST /hooks/<source id>. It writes ordinary
// issue, message, document and tombstone events, and a work.decision extension
// kind, using push for authentication, JSON handling and revisions.
//
// A document or decision id names at most one held record in its project,
// across the work source, its owner source and all its workstreams ([Resolve]
// is the one resolution). A work source whose setting owner_of names another is
// that source's owner source: it holds only documents and decisions about the
// named source's projects and carries their access list. The two share one
// per-project lock through [Shared], which needs them in one process.
//
// Containers must be exactly ["*"], and the source takes no secrets. Settings
// posters defaults to ["*"] (all agent principals with token_env); attributors
// defaults to [] and names posters trusted to supply a configured principal as
// author. Explicit poster and attributor ids must name agents with token_env.
// Every allowed poster's token is loaded at startup; missing, empty and shared
// tokens fail startup. Principal class and API grants do not govern posting.
//
// Repos and touches may name only configured source ids. Projects with repos
// defer access to those containers; projects without repos are private to their
// poster and author. Children inherit the project's current list. A project
// post finishes only after reconciling every live child's list. Poll performs
// the same reconciliation on the runtime's immediate startup pass and retries
// it on subsequent passes; no cursor or in-memory debt survives a restart.
//
// Run only one connectors process per source, as the shipped deployments do.
// Posts and reconciliation share a per-project process lock. Multiple replicas
// accepting one source would require a distributed lock. Terminate TLS at the
// ingress (for example tailscale serve), and route /hooks/<source id> to the
// connectors listener. Payloads, credentials and attributed text are not logged.
package work
