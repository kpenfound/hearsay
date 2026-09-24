// Package queue is Hearsay's Postgres-backed job queue (ADR-0007).
//
// Enqueue is transactional with the write that caused it, so a job cannot exist
// for an event that was rolled back. Delivery is at-least-once, which makes
// idempotent handlers a requirement rather than a nicety; a retry whose target
// already has a pending job is superseded by it instead (ADR-0011). A job may carry a
// serial_key, and jobs sharing one are never run concurrently — that is how the
// assertion worker gets per-scope serialization. A caller that is not a worker
// can hold a serial key the same way ([Client.Hold], ADR-0020).
//
// See docs/adr/0007-postgres-backed-job-queue.md.
package queue
