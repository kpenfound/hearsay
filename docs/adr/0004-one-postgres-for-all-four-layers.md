# 4. One Postgres for all four layers

- Status: accepted
- Date: 2026-09-08

## Context

The layer model in `docs/design.md` puts four different shapes of data in the system:

- **L0**, append-only events with a JSONB payload and an ACL.
- **L1**, distilled documents, flat rows in one schema, each with an embedding, a `text`
  field that is embedded, and a `raw_text` field that is full-text indexed only.
- **L2**, entities, topics and stances with edges between them: `part_of` hierarchy,
  `supersedes` chains, evidence links back to L1.
- **L3**, derived views, never stored, rebuilt from L2 on read.

The obvious decomposition is one store per shape: a document store for L0 and L1, a
vector database for embeddings, a search engine for full text, a graph database for L2.
Three properties of the design push the other way. Deletion walks provenance forward
from an L0 tombstone through L1 to L2 and must leave nothing behind, which is a
transaction across all of them. Retrieval is hybrid: embedding similarity and full-text
rank fused over the same L1 rows, then filtered by ACL, which is a join. And Hearsay is
self-hosted per organization, where every additional stateful service is something an
operator has to run, back up and restore consistently.

## Decision

One PostgreSQL database holds all four layers.

- **Vectors** are a column on the L1 table, indexed with **pgvector**. Not a separate
  store, not a separate table. An L1 document and its embedding are one row, so an ACL
  filter, a metadata filter and a similarity search are one query with no fan-out.
- **Full-text search** is Postgres' own: a `tsvector` column over `raw_text` with a GIN
  index. Hybrid retrieval fuses the vector ranking and the text ranking in SQL
  (reciprocal rank fusion) and returns L1 rows.
- **L2 is relational.** Entities, topics, stances and edges are tables. Hierarchy
  (`part_of`) and supersession chains are walked with recursive CTEs. There is no graph
  database.
- **L3 is not stored.** Derived views are queries, materialized only if measurement
  later says they must be.
- The driver is **pgx v5**, used natively rather than through `database/sql`. This is
  not incidental: the queue in ADR-0007 uses `LISTEN`/`NOTIFY`, JSONB and array handling
  are better without the `database/sql` conversion layer, and `pgvector-go` supports pgx
  directly.
- One database, one schema, one connection pool per process. Services are separated by
  process (ADR-0003), not by store.

The minimum server version is Postgres 16 with the `vector` extension available.
`CREATE EXTENSION vector` is the first migration (ADR-0006).

## Alternatives considered

- **A dedicated vector database (Qdrant, Weaviate, Milvus, pgvector's hosted cousins).**
  Faster at very large vector counts and better at index tuning. Rejected on the join:
  every Hearsay retrieval is filtered by ACL and scope, which live in Postgres, so a
  separate vector store means either replicating ACLs into it (two places to get
  permissions wrong, which is the worst possible thing to duplicate) or fetching a
  candidate set and filtering after, which breaks result counts. A single-organization
  corpus of team communication is small enough that pgvector's ceiling is not the
  binding constraint.
- **Elasticsearch or OpenSearch for full text, and possibly for vectors too.** Better
  text search than Postgres, genuinely. Same objection: it is a second copy of L1 with a
  second ACL model and its own consistency lag, and deletion would have to walk into it.
  Postgres full-text is weaker at ranking English prose but adequate for a corpus this
  size, and the hybrid fusion recovers a lot of what it misses.
- **A graph database for L2 (Neo4j, Memgraph, or an embedded graph).** The natural
  shape for entities and stances, and the thing every comparable system reaches for.
  Rejected for now on size and shape: the L2 graph here is shallow (a `part_of`
  hierarchy a few levels deep, supersession chains that are lists, evidence edges that
  are one hop to L1). Recursive CTEs handle all of it, and the queries the bundle needs
  are known and few rather than exploratory. Traversals that are cheap in Cypher and
  awkward in SQL are the signal to revisit this.
- **SQLite for single-team deployments.** A tempting zero-dependency story, and
  sqlite-vec exists. Rejected: no `LISTEN`/`NOTIFY` for the queue, weaker concurrent
  write behaviour with four processes, and maintaining two dialects of every query for
  the life of the project.
- **Separate databases per layer, same server.** Gains isolation, loses the transaction
  that deletion needs, and doubles the migration story for no operational benefit at
  this scale.

## Consequences

- Backup and restore is one `pg_dump`. Point-in-time recovery restores all four layers
  to a consistent instant. Deletion and provenance walks are one transaction.
- The compose file (#29) needs a Postgres image with pgvector (`pgvector/pgvector`),
  not stock Postgres, and the Dagger integration test service (#3) must use the same.
  A plain Postgres will fail on the first migration.
- The embedding column has a fixed dimension, `vector(N)`. Changing the embedding model
  to one with a different dimension is a migration plus a full re-embed of L1, not a
  config edit. ADR-0005 makes the embed tier config, so this is the one tier change that
  is not free, and it needs to be said where someone will read it.
- pgvector index choice (HNSW versus IVFFlat, and its parameters) is a tuning decision
  deferred to the search work (#10), not fixed here. HNSW is the expected default.
- `raw_text` is indexed but never embedded, per the design. The schema enforces this by
  keeping them separate columns rather than deriving one from the other.
- L2 access goes through a repository interface rather than SQL scattered through the
  assertion worker, so that moving L2 to a graph store later is a change in one package.
  This is the escape hatch the design asks for when it says the layering allows moving
  L2 alone.
- Postgres becomes the scaling ceiling for the whole system. That is a deliberate trade:
  one thing to operate, and a clear signal when it is time to split, rather than four
  things to operate from day one.
