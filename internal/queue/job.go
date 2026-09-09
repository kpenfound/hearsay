package queue

import (
	"errors"
	"fmt"
	"math"
	"strconv"
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

// MaxIDLen bounds both TargetID and SerialKey. The column is `text` and has no
// limit of its own, so this is the queue's: an id long enough to matter is a
// caller passing something that is not an id.
//
// It is set above the longest id anything mints today — `internal/connector`
// bounds an L0 event id at 1605 bytes — with room for the L1 and L2 ids that
// come later. The number is not read from that package on purpose: the queue
// takes an opaque id and does not depend on the layers that mint one.
const MaxIDLen = 2048

// validID refuses what the text columns cannot store. Postgres rejects a NUL
// byte and any invalid UTF-8 with a statement error, and a statement error
// inside the caller's transaction aborts it — which is the one thing Enqueue
// promises it cannot do (ADR-0007). So these are refused here, before any SQL
// runs, rather than discovered there.
func validID(kind, field, value string) error {
	switch {
	case !utf8.ValidString(value):
		return fmt.Errorf("%w: the %s of a %s job is not valid UTF-8, which Postgres would refuse mid-transaction", ErrInvalidJob, field, kind)
	case strings.IndexByte(value, 0) >= 0:
		// NUL is valid UTF-8 and still not storable in a text column, so it
		// needs saying separately.
		return fmt.Errorf("%w: the %s of a %s job holds a NUL byte, which a text column cannot store", ErrInvalidJob, field, kind)
	case len(value) > MaxIDLen:
		return fmt.Errorf("%w: the %s of a %s job is %d bytes, over the %d-byte limit", ErrInvalidJob, field, kind, len(value), MaxIDLen)
	}
	return nil
}

// Validate reports a request the queue refuses. Enqueue calls it before any
// SQL runs, so a bad request can never be the thing that aborts the caller's
// transaction.
func (r Request) Validate() error {
	if err := r.Kind.Validate(); err != nil {
		return err
	}
	if err := validID(r.Kind.Name, "target id", r.TargetID); err != nil {
		return err
	}
	if err := validID(r.Kind.Name, "serial key", r.SerialKey); err != nil {
		return err
	}
	// The carrier goes to a jsonb column, which refuses a \u0000 escape
	// (22P05) exactly as the text columns refuse a NUL byte — a third way to
	// abort the caller's transaction, so it is checked the same way. Invalid
	// UTF-8 would not abort anything here, because encoding/json substitutes
	// U+FFFD for it, but a trace id quietly rewritten is not a trace id.
	for key, value := range r.TraceContext {
		if err := validID(r.Kind.Name, "trace context key "+strconv.Quote(key), key); err != nil {
			return err
		}
		if err := validID(r.Kind.Name, "trace context value for "+strconv.Quote(key), value); err != nil {
			return err
		}
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

// errorText is the message stored for a failed attempt: a handler's bytes made
// storable, and then bounded.
//
// A handler's error is the least controlled string this package writes — it is
// whatever a provider, a decoder or an HTTP body put in it — and a text column
// refuses a NUL byte and invalid UTF-8 alike. Written as they came, either
// raises 22021 from inside [Client.Fail], which then records nothing: the row
// keeps a lease nobody is renewing, the cause never reaches the row an
// operator reads, and the job waits for a reclaim instead of its backoff. So
// these bytes are coerced rather than trusted.
func errorText(err error) string {
	if err == nil {
		return ""
	}
	// Coercion comes before the bound, because the bound assumes valid UTF-8:
	// utf8.RuneStart reads one byte's top bits, which says nothing about bytes
	// that were never part of a rune. A run of invalid bytes collapses to one
	// replacement character, so this shortens rather than grows.
	msg := strings.ToValidUTF8(err.Error(), "\uFFFD")
	// A NUL is valid UTF-8 and still not storable. It is dropped rather than
	// replaced: it carries nothing a reader of the row wants.
	msg = strings.ReplaceAll(msg, "\x00", "")
	if len(msg) <= maxErrorLen {
		return msg
	}
	// Cut on a rune boundary, which the coercion above is what makes possible:
	// the column is text, and a half-encoded rune in it would be a second bug
	// to find later.
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
