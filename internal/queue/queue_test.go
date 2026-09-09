package queue_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/queue"
)

func TestKindValidate(t *testing.T) {
	tests := []struct {
		name    string
		kind    queue.Kind
		wantErr bool
	}{
		{name: "a kind of the shipped shape", kind: queue.Kind{Name: "distill"}},
		{name: "serialized", kind: queue.Kind{Name: "assert", Serialized: true}},
		{name: "digits and underscores after the first letter", kind: queue.Kind{Name: "distill_v2"}},
		{name: "no name", wantErr: true},
		{name: "upper case", kind: queue.Kind{Name: "Distill"}, wantErr: true},
		{name: "a dash is not a channel name", kind: queue.Kind{Name: "distill-v2"}, wantErr: true},
		{name: "leading digit", kind: queue.Kind{Name: "2distill"}, wantErr: true},
		{name: "leading underscore", kind: queue.Kind{Name: "_distill"}, wantErr: true},
		{name: "at the length limit", kind: queue.Kind{Name: "d" + strings.Repeat("x", queue.MaxKindLen-1)}},
		{name: "past the length limit", kind: queue.Kind{Name: "d" + strings.Repeat("x", queue.MaxKindLen)}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.kind.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Kind(%+v).Validate() = %v, want error: %v", tt.kind, err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, queue.ErrInvalidJob) {
				t.Errorf("Kind(%+v).Validate() = %v, want it to wrap ErrInvalidJob", tt.kind, err)
			}
		})
	}
}

// A channel name is a Postgres identifier, and Postgres truncates one at 63
// bytes. Two kinds whose names differ only past that would share a channel and
// wake each other's workers, so the length limit is on the kind, not on the
// truncation.
func TestAValidKindsChannelFitsAPostgresIdentifier(t *testing.T) {
	const namedatalen = 63
	kind := queue.Kind{Name: "d" + strings.Repeat("x", queue.MaxKindLen-1)}
	if err := kind.Validate(); err != nil {
		t.Fatalf("the longest valid kind does not validate: %v", err)
	}
	if got := len(kind.Channel()); got != namedatalen {
		t.Errorf("len(Channel()) = %d for the longest valid kind, want exactly the identifier limit %d", got, namedatalen)
	}
	if got := (queue.Kind{Name: "distill"}).Channel(); got != "hearsay_job_distill" {
		t.Errorf("Kind{distill}.Channel() = %q, want hearsay_job_distill", got)
	}
}

func TestRequestValidate(t *testing.T) {
	distill := queue.Kind{Name: "distill"}
	assert := queue.Kind{Name: "assert", Serialized: true}

	tests := []struct {
		name    string
		req     queue.Request
		wantErr bool
	}{
		{name: "unserialized", req: queue.Request{Kind: distill, TargetID: "evt:github-acme:pr-1"}},
		{name: "serialized with a key", req: queue.Request{Kind: assert, TargetID: "l1-1", SerialKey: "scope-payments"}},
		{name: "delayed", req: queue.Request{Kind: distill, TargetID: "e1", Delay: time.Minute}},
		{name: "no target", req: queue.Request{Kind: distill}, wantErr: true},
		{name: "an invalid kind is reported by the request", req: queue.Request{Kind: queue.Kind{Name: "Distill"}, TargetID: "e1"}, wantErr: true},
		{name: "serialized without a key", req: queue.Request{Kind: assert, TargetID: "l1-1"}, wantErr: true},
		{name: "a key on an unserialized kind would be ignored", req: queue.Request{Kind: distill, TargetID: "e1", SerialKey: "scope"}, wantErr: true},
		{name: "a delay into the past", req: queue.Request{Kind: distill, TargetID: "e1", Delay: -time.Second}, wantErr: true},
		{name: "a priority the column cannot hold", req: queue.Request{Kind: distill, TargetID: "e1", Priority: 1 << 40}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Request(%+v).Validate() = %v, want error: %v", tt.req, err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, queue.ErrInvalidJob) {
				t.Errorf("Request(%+v).Validate() = %v, want it to wrap ErrInvalidJob", tt.req, err)
			}
		})
	}
}

func TestConfigValidate(t *testing.T) {
	distill := queue.Kind{Name: "distill"}
	assert := queue.Kind{Name: "assert", Serialized: true}

	tests := []struct {
		name    string
		cfg     queue.Config
		wantErr bool
	}{
		{name: "a kind and nothing else", cfg: queue.Config{Kind: distill}},
		{name: "a batch of an unserialized kind", cfg: queue.Config{Kind: distill, Concurrency: 8}},
		{name: "a serialized kind claims one at a time", cfg: queue.Config{Kind: assert, Concurrency: 1}},
		{name: "a serialized kind cannot batch", cfg: queue.Config{Kind: assert, Concurrency: 2}, wantErr: true},
		{name: "no kind", wantErr: true},
		{name: "negative concurrency", cfg: queue.Config{Kind: distill, Concurrency: -1}, wantErr: true},
		{name: "a lease that has already expired", cfg: queue.Config{Kind: distill, Lease: -time.Second}, wantErr: true},
		{name: "no polling floor", cfg: queue.Config{Kind: distill, PollInterval: -time.Second}, wantErr: true},
		{name: "no attempts", cfg: queue.Config{Kind: distill, MaxAttempts: -1}, wantErr: true},
		{name: "a cap below the backoff", cfg: queue.Config{Kind: distill, Backoff: time.Minute, MaxBackoff: time.Second}, wantErr: true},
		{name: "negative retention", cfg: queue.Config{Kind: distill, Retention: -time.Hour}, wantErr: true},
		{name: "negative maintenance interval", cfg: queue.Config{Kind: distill, MaintenanceInterval: -time.Hour}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Validate is what New calls after the defaults are filled in, so
			// that is how the test calls it: a zero duration is a default, not
			// a rejection.
			_, err := queue.New(nil, tt.cfg)
			if err == nil {
				t.Fatal("New(nil, cfg) = no error, want one: a queue needs a pool")
			}
			err = tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Config(%+v).Validate() = %v, want error: %v", tt.cfg, err, tt.wantErr)
			}
		})
	}
}

// The defaults are what a service that configures nothing gets, so they have
// to be the ones ADR-0007 describes rather than whatever the zero value is.
func TestConfigDefaults(t *testing.T) {
	cfg := queue.Config{Kind: queue.Kind{Name: "distill"}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the zero config for a valid kind does not validate: %v", err)
	}

	if cfg.HeartbeatInterval() != queue.DefaultLease/3 {
		// A zero lease means the default lease, and the heartbeat is a third
		// of it: three heartbeats per lease, so two may be lost.
		t.Errorf("HeartbeatInterval() on a zero config = %s, want %s", cfg.HeartbeatInterval(), queue.DefaultLease/3)
	}
	// A zero config is a working one: nothing here may divide by, or draw
	// from, an interval of zero.
	if d := cfg.RetryDelay(1); d <= 0 || d > queue.DefaultBackoff {
		t.Errorf("RetryDelay(1) on a zero config = %s, want a delay in (0, %s]", d, queue.DefaultBackoff)
	}

	// A lease far below the heartbeat floor is renewed at the floor, which is
	// after it has expired. That is a lease measured in tens of milliseconds,
	// which is a test's rather than a deployment's, and the alternative — a
	// heartbeat every few milliseconds — is worse.
	short := queue.Config{Kind: queue.Kind{Name: "distill"}, Lease: 30 * time.Millisecond}
	if hb := short.HeartbeatInterval(); hb < short.Lease {
		t.Errorf("HeartbeatInterval() = %s for a %s lease, want the floor rather than something below it", hb, short.Lease)
	}
	long := queue.Config{Kind: queue.Kind{Name: "distill"}, Lease: 9 * time.Second}
	if hb := long.HeartbeatInterval(); hb != 3*time.Second {
		t.Errorf("HeartbeatInterval() = %s for a %s lease, want a third of it", hb, long.Lease)
	}
}

// The jitter has to span the whole interval and the growth has to stop at the
// cap: a retry storm that comes back together is the failure this exists to
// avoid, and an unbounded doubling would park a job past the retention window.
func TestRetryDelayGrowsToTheCapAndIsJittered(t *testing.T) {
	cfg := queue.Config{Kind: queue.Kind{Name: "distill"}, Backoff: time.Second, MaxBackoff: 8 * time.Second}

	// attempt 1 waits up to the base, and each attempt after it doubles that
	// until the cap.
	ceilings := map[int]time.Duration{
		-1: time.Second, 0: time.Second, 1: time.Second, 2: 2 * time.Second,
		3: 4 * time.Second, 4: 8 * time.Second, 5: 8 * time.Second,
		40: 8 * time.Second, 1 << 20: 8 * time.Second,
	}
	for attempt, want := range ceilings {
		var high time.Duration
		for range 200 {
			d := cfg.RetryDelay(attempt)
			if d <= 0 || d > want {
				t.Fatalf("RetryDelay(%d) = %s, want a delay in (0, %s]", attempt, d, want)
			}
			high = max(high, d)
		}
		// 200 draws over the interval land in its top decile with
		// overwhelming probability; a fixed delay, or jitter over a fraction
		// of the interval, would not.
		if high < want/10*9 {
			t.Errorf("the highest of 200 RetryDelay(%d) draws was %s, want the jitter to reach near %s", attempt, high, want)
		}
	}
}

func TestLimit(t *testing.T) {
	tests := []struct {
		in, want int
	}{
		{in: 0, want: queue.DefaultLimit},
		{in: -1, want: queue.DefaultLimit},
		{in: 10, want: 10},
		{in: queue.MaxLimit + 1, want: queue.MaxLimit},
	}
	for _, tt := range tests {
		if got := queue.Limit(tt.in); got != tt.want {
			t.Errorf("Limit(%d) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestStateValid(t *testing.T) {
	for _, s := range []queue.State{queue.StatePending, queue.StateRunning, queue.StateDone, queue.StateFailed} {
		if !s.Valid() {
			t.Errorf("State(%q).Valid() = false, want true", s)
		}
	}
	for _, s := range []queue.State{"", "claimed", "PENDING"} {
		if s.Valid() {
			t.Errorf("State(%q).Valid() = true, want false", s)
		}
	}
}

// A job goes into log lines, and a log line must not carry what a job is
// about beyond its ids (ADR-0008).
func TestJobStringNamesIdsOnly(t *testing.T) {
	job := queue.Job{ID: 7, Kind: queue.Kind{Name: "assert", Serialized: true}, TargetID: "l1-42", SerialKey: "scope-payments", Attempt: 2}
	want := "assert job 7 for l1-42 on scope-payments (attempt 2)"
	if got := job.String(); got != want {
		t.Errorf("Job.String() = %q, want %q", got, want)
	}
}
