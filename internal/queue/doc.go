// Package queue is Hearsay's Postgres-backed job queue (ADR-0007).
//
// Enqueue is transactional with the write that caused it, so a job cannot exist
// for an event that was rolled back. Delivery is at-least-once, which makes
// idempotent handlers a requirement rather than a nicety. A job may carry a
// serial_key, and jobs sharing one are never run concurrently — that is how the
// assertion worker gets per-scope serialization.
//
// See docs/adr/0007-postgres-backed-job-queue.md.
package queue
