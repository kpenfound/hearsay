package l0_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l0"
)

func TestCursorRoundTrips(t *testing.T) {
	tests := []struct {
		name string
		s    string
		want string // what String gives back; empty means the zero cursor
	}{
		{name: "the zero cursor is the empty string", s: "", want: ""},
		{name: "a position", s: "1234.7", want: "1234.7"},
		{name: "the first row of a transaction", s: "1234.0", want: "1234.0"},
		{name: "a transaction id above what int64 holds", s: "18446744073709551615.1", want: "18446744073709551615.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cursor, err := l0.ParseCursor(tt.s)
			if err != nil {
				t.Fatalf("ParseCursor(%q) = %v, want no error", tt.s, err)
			}
			if got := cursor.String(); got != tt.want {
				t.Errorf("ParseCursor(%q).String() = %q, want %q", tt.s, got, tt.want)
			}
			if got := cursor.IsZero(); got != (tt.want == "") {
				t.Errorf("ParseCursor(%q).IsZero() = %v, want %v", tt.s, got, tt.want == "")
			}
		})
	}
}

func TestParseCursorRejects(t *testing.T) {
	tests := []struct {
		name string
		s    string
	}{
		{name: "no separator", s: "1234"},
		{name: "no sequence", s: "1234."},
		{name: "no transaction", s: ".7"},
		{name: "not a number", s: "yesterday.7"},
		{name: "a negative sequence", s: "1234.-7"},
		{name: "transaction zero, which no row carries", s: "0.0"},
		{name: "a transaction id that does not fit", s: "18446744073709551616.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cursor, err := l0.ParseCursor(tt.s)
			if err == nil {
				t.Fatalf("ParseCursor(%q) = %v, want an error", tt.s, cursor)
			}
			if !strings.Contains(err.Error(), tt.s) {
				t.Errorf("ParseCursor(%q) = %v, want the error to quote what was passed", tt.s, err)
			}
		})
	}
}

func TestLimit(t *testing.T) {
	tests := []struct {
		name string
		in   int
		want int
	}{
		{name: "none asked for is the default", in: 0, want: l0.DefaultLimit},
		{name: "negative is the default", in: -1, want: l0.DefaultLimit},
		{name: "one is one", in: 1, want: 1},
		{name: "the cap is the cap", in: l0.MaxLimit, want: l0.MaxLimit},
		{name: "above the cap is the cap", in: l0.MaxLimit + 1, want: l0.MaxLimit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := l0.Limit(tt.in); got != tt.want {
				t.Errorf("Limit(%d) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

// An event the contract rejects never reaches the database: the store validates
// first, so the table holds only events docs/connector-contract.md allows. The
// store here has no database at all, which is what proves nothing was sent.
func TestAppendValidatesBeforeItTouchesTheDatabase(t *testing.T) {
	fake := connector.NewFake(connector.SourceConfig{ID: "unit", Type: connector.FakeType})
	tests := []struct {
		name  string
		event connector.Event
	}{
		{name: "no source", event: func() connector.Event {
			ev := fake.NewEvent(connector.KindMessage, "m1", "hello")
			ev.Source = ""
			return ev
		}()},
		{name: "no acl", event: func() connector.Event {
			ev := fake.NewEvent(connector.KindMessage, "m1", "hello")
			ev.ACL = nil
			return ev
		}()},
		{name: "a native id that is not the artifact", event: func() connector.Event {
			ev := fake.NewEvent(connector.KindMessage, "m1", "hello")
			ev.NativeID = "m2"
			return ev
		}()},
		{name: "a tombstone with no target", event: func() connector.Event {
			ev := fake.NewEvent(connector.KindTombstone, "m1:tombstone", "")
			return ev
		}()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := l0.New(nil)
			if _, err := store.Append(t.Context(), tt.event); !errors.Is(err, connector.ErrInvalidEvent) {
				t.Fatalf("Append(%s) = %v, want it to carry connector.ErrInvalidEvent", tt.name, err)
			}
		})
	}
}

// An artifact id only means something inside the source that minted it, so a
// listing that names one without the other is refused rather than answered
// across every source at once. Again: no database, so the check is first.
func TestListRefusesAnArtifactWithoutASource(t *testing.T) {
	store := l0.New(nil)
	if _, err := store.List(t.Context(), l0.ListOptions{Artifact: "acme/api#1"}); err == nil {
		t.Fatal("List(artifact without source) = nil, want an error")
	}
}

func TestCursorsCompareInFeedOrder(t *testing.T) {
	parse := func(s string) l0.Cursor {
		t.Helper()
		c, err := l0.ParseCursor(s)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	tests := []struct {
		name string
		a, b string
		want int
	}{
		{name: "the same position", a: "1234.7", b: "1234.7", want: 0},
		{name: "an earlier row of one transaction", a: "1234.6", b: "1234.7", want: -1},
		{name: "a later row of one transaction", a: "1234.8", b: "1234.7", want: 1},
		{name: "an earlier transaction, whatever its sequence", a: "1233.9", b: "1234.0", want: -1},
		{name: "a later transaction, whatever its sequence", a: "1235.0", b: "1234.9", want: 1},
		{name: "the beginning is before everything", a: "", b: "1.0", want: -1},
		{name: "the beginning is itself", a: "", b: "", want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parse(tt.a).Compare(parse(tt.b)); got != tt.want {
				t.Errorf("Compare(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
			}
		})
	}
}
