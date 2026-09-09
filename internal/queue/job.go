package queue

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

// channelPrefix is what a kind's LISTEN/NOTIFY channel is named after it.
const channelPrefix = "hearsay_job_"

// MaxKindLen is the longest a kind name may be. A channel name is a Postgres
// identifier, truncated at NAMEDATALEN-1 = 63 bytes, and two kinds whose names
// only differ past that would share a channel and wake each other's workers.
const MaxKindLen = 63 - len(channelPrefix)

// ErrInvalidJob is what a request or a kind the queue refuses wraps. It is a
// sentinel so that a caller can tell "I built this job wrong" from "the
// database said no".
var ErrInvalidJob = errors.New("invalid job")

// Kind is a job kind and how the queue runs it. It is a value both ends of a
// kind share: the service that enqueues the work and the worker that runs it
// declare the same Kind, because the queue itself cannot tell from a row
// whether the kind it belongs to is serialized (ADR-0007 makes that a property
// of the kind, changed by revisiting the ADR rather than by configuration).
type Kind struct {
	// Name is the kind as it is stored and as its channel is named:
	// `distill`, `assert`.
	Name string

	// Serialized makes every job of this kind carry a serial key, and two jobs
	// sharing one key never run at the same time. It is what the assertion
	// worker's per-scope serialization is (ADR-0007), and it costs throughput:
	// a serialized kind claims one job per round trip.
	Serialized bool
}

// Validate reports a kind the queue cannot use.
func (k Kind) Validate() error {
	switch {
	case k.Name == "":
		return fmt.Errorf("%w: a job kind has no name", ErrInvalidJob)
	case len(k.Name) > MaxKindLen:
		return fmt.Errorf("%w: job kind %q is longer than %d bytes, which is what fits in a channel name", ErrInvalidJob, k.Name, MaxKindLen)
	}
	for i, r := range k.Name {
		switch {
		case r >= 'a' && r <= 'z':
		case r == '_', r >= '0' && r <= '9':
			if i == 0 {
				return fmt.Errorf("%w: job kind %q does not start with a letter", ErrInvalidJob, k.Name)
			}
		default:
			return fmt.Errorf("%w: job kind %q holds %q; a kind is lower-case letters, digits and underscores, because it names a channel", ErrInvalidJob, k.Name, r)
		}
	}
	return nil
}

// Channel is the LISTEN/NOTIFY channel workers of this kind wake on. One
// channel per kind, so a busy kind does not wake the workers of a quiet one.
func (k Kind) Channel() string { return channelPrefix + k.Name }

// State is where a job is. A job is pending, then running, then done or
// failed; a retry puts it back to pending, and only the two terminal states
// stay put.
type State string

const (
	// StatePending is waiting to run, at or after its run_after.
	StatePending State = "pending"
	// StateRunning is claimed by a worker that holds a live lease on it.
	StateRunning State = "running"
	// StateDone is finished, and is deleted once it is older than the
	// retention window.
	StateDone State = "done"
	// StateFailed has run out of attempts. It is kept: a failed job is a
	// visible row and a metric, not a lost event (ADR-0007).
	StateFailed State = "failed"
)

// Valid reports a state the queue knows.
func (s State) Valid() bool {
	switch s {
	case StatePending, StateRunning, StateDone, StateFailed:
		return true
	}
	return false
}

// Request is a job to enqueue.
type Request struct {
	// Kind is the job kind, and it decides what else this request needs: a
	// serialized kind requires SerialKey, an unserialized one refuses it.
	Kind Kind

	// TargetID is the row the job is about — an L0 event id, an L1 document
	// id. Job payloads stay small (ADR-0007): the handler reads the row, so a
	// job never carries a stale copy of the thing it is about, and
	// (Kind, TargetID) is what a duplicate enqueue collapses on.
	TargetID string

	// SerialKey is the scope this job must not share with a running one — the
	// assertion worker sets it to the scope id. Required on a serialized kind
	// and refused on any other, because a key that nothing enforces reads like
	// serialization that is not there.
	SerialKey string

	// Priority runs a job ahead of its kind's backlog. Higher first, default 0.
	Priority int

	// Delay holds the job back for this long. It is measured by Postgres, not
	// by the enqueuing process, so a skewed clock cannot schedule a job into
	// the past or the far future.
	Delay time.Duration

	// TraceContext is the propagation carrier of the trace that enqueued the
	// job (ADR-0008), handed back to the worker so the work joins the trace
	// that caused it. It carries trace ids, never job data.
	TraceContext map[string]string
}

// Validate reports a request the queue refuses. Enqueue calls it before any
// SQL runs, so a bad request can never be the thing that aborts the caller's
// transaction.
func (r Request) Validate() error {
	if err := r.Kind.Validate(); err != nil {
		return err
	}
	switch {
	case r.TargetID == "":
		return fmt.Errorf("%w: a %s job has no target id", ErrInvalidJob, r.Kind.Name)
	case r.Kind.Serialized && r.SerialKey == "":
		return fmt.Errorf("%w: %s is a serialized kind and this job has no serial key", ErrInvalidJob, r.Kind.Name)
	case !r.Kind.Serialized && r.SerialKey != "":
		return fmt.Errorf("%w: %s is not a serialized kind, so its jobs must not carry a serial key (%q would be ignored)", ErrInvalidJob, r.Kind.Name, r.SerialKey)
	case r.Delay < 0:
		return fmt.Errorf("%w: a %s job is delayed by %s, which is in the past", ErrInvalidJob, r.Kind.Name, r.Delay)
	case r.Priority > math.MaxInt32 || r.Priority < math.MinInt32:
		return fmt.Errorf("%w: priority %d does not fit the column, which is a 32-bit integer", ErrInvalidJob, r.Priority)
	}
	return nil
}

// Enqueued is what one enqueue did.
type Enqueued struct {
	// ID is the job's id, and is zero when Stored is false: the row that
	// collapsed this one may belong to a transaction that has not committed,
	// so reading its id back is not something an enqueue can promise.
	ID int64

	// Stored reports that this call wrote a row. False means a job of the same
	// kind for the same target was already pending and this enqueue collapsed
	// into it, which is the ordinary outcome of re-deriving something.
	Stored bool
}

// Job is one row of the queue: what a claim hands a worker, and what an
// operator sees when listing a kind's failures.
type Job struct {
	ID   int64
	Kind Kind

	TargetID  string
	SerialKey string
	Priority  int
	State     State

	// Attempt counts runs rather than retries: the claim increments it, so a
	// running job's Attempt is the number of the run in progress and 1 is the
	// first. With ID it identifies one run, which is what lets the queue refuse
	// an outcome reported by a worker whose lease has since been reclaimed.
	Attempt int

	// TraceContext is what the enqueuing side passed.
	TraceContext map[string]string

	EnqueuedAt time.Time
	// RunAfter is when the job became, or becomes, claimable.
	RunAfter time.Time
	// StartedAt is when the current run began, and is zero on a job that is
	// not running or has been put back to pending.
	StartedAt time.Time
	// FinishedAt is zero until the job is done or failed.
	FinishedAt time.Time
	// LastError is why the most recent attempt did not succeed. It survives a
	// retry, so a job on its third attempt says what happened on its second,
	// and it is cleared when the job succeeds.
	LastError string
}

// maxErrorLen bounds what a handler's error contributes to a row. A job's
// error is written to the table and logged, so it is a message about the
// failure and not a place to put the document that failed (ADR-0008).
const maxErrorLen = 2000

// errorText is the message stored for a failed attempt, bounded.
func errorText(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if len(msg) <= maxErrorLen {
		return msg
	}
	// Cut on a rune boundary: the column is text, and a half-encoded rune in
	// it would be a second bug to find later.
	cut := maxErrorLen
	for cut > 0 && !utf8.RuneStart(msg[cut]) {
		cut--
	}
	return msg[:cut] + "…"
}

// String makes a job printable in a log line without printing anything but ids.
func (j Job) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s job %d for %s", j.Kind.Name, j.ID, j.TargetID)
	if j.SerialKey != "" {
		fmt.Fprintf(&b, " on %s", j.SerialKey)
	}
	fmt.Fprintf(&b, " (attempt %d)", j.Attempt)
	return b.String()
}
