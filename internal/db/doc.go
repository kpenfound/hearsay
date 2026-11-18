// Package db is the Postgres layer: connection pool, migrations and the shared
// query plumbing every layer sits on.
//
// One database holds all four layers, with pgvector for embeddings and Postgres
// full-text search for keywords. Migrations live in db/migrations, are embedded
// into the binary and are applied by `hearsay migrate`, never automatically on
// service startup.
package db
