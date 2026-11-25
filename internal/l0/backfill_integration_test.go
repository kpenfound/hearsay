//go:build integration

package l0_test

import (
	"strings"
	"testing"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/db/dbtest"
	"github.com/kpenfound/hearsay/internal/l0"
)

func TestBackfillCursorsRoundTrip(t *testing.T) {
	pool := dbtest.Pool(t)
	cursors := l0.NewBackfillCursors(pool)
	source := newSourceID(t)

	state, err := cursors.Load(t.Context(), source)
	if err != nil {
		t.Fatalf("Load(unknown source) = %v, want no error", err)
	}
	if (state != connector.BackfillState{}) {
		t.Errorf("Load(unknown source) = %+v, want the zero state", state)
	}

	for _, want := range []connector.BackfillState{
		{Cursor: "page-2", Events: 50},
		{Cursor: "page-3", Events: 100},
		{Cursor: "page-3", Done: true, Events: 150},
	} {
		if err := cursors.Save(t.Context(), source, want); err != nil {
			t.Fatalf("Save(%+v) = %v, want no error", want, err)
		}
		got, err := cursors.Load(t.Context(), source)
		if err != nil {
			t.Fatalf("Load() = %v, want no error", err)
		}
		if got != want {
			t.Errorf("Load() = %+v, want %+v", got, want)
		}
	}

	other := newSourceID(t)
	if err := cursors.Save(t.Context(), other, connector.BackfillState{Cursor: "elsewhere"}); err != nil {
		t.Fatalf("Save(other source) = %v, want no error", err)
	}
	got, err := cursors.Load(t.Context(), source)
	if err != nil {
		t.Fatalf("Load() = %v, want no error", err)
	}
	if got.Cursor != "page-3" || !got.Done {
		t.Errorf("Load() = %+v after another source saved, want this source's own position", got)
	}
}

func TestBackfillCursorsRefuseWhatTheColumnCannotHold(t *testing.T) {
	pool := dbtest.Pool(t)
	cursors := l0.NewBackfillCursors(pool)
	source := newSourceID(t)

	tests := []struct {
		name   string
		source string
		state  connector.BackfillState
		want   string
	}{
		{name: "a source id that is not one", source: "Not A Source", want: "is not a source id"},
		{name: "no source at all", source: "", want: "is not a source id"},
		{
			name:   "a source id longer than the column",
			source: strings.Repeat("s", connector.MaxSourceIDLen+1),
			want:   "is not a source id",
		},
		{
			name:   "a cursor longer than the limit",
			source: source,
			state:  connector.BackfillState{Cursor: connector.Cursor(strings.Repeat("p", connector.MaxCursorLen+1))},
			want:   "the limit is",
		},
		{
			name:   "a cursor holding a NUL byte",
			source: source,
			state:  connector.BackfillState{Cursor: "page\x00two"},
			want:   "NUL byte",
		},
		{
			name:   "a cursor that is not UTF-8",
			source: source,
			state:  connector.BackfillState{Cursor: connector.Cursor([]byte{0xff, 0xfe})},
			want:   "UTF-8",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := cursors.Save(t.Context(), tt.source, tt.state)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Save() = %v, want an error about %q", err, tt.want)
			}
			if _, err := cursors.Load(t.Context(), tt.source); err == nil && tt.source != source {
				t.Errorf("Load(%q) = no error, want the same refusal Save gave", tt.source)
			}
		})
	}
}

func TestTheTableRefusesWhatTheStoreRefuses(t *testing.T) {
	pool := dbtest.Pool(t)
	source := newSourceID(t)

	tests := []struct {
		name   string
		source string
		cursor string
		events int64
	}{
		{name: "a source id that is not one", source: "Not A Source", cursor: "p1"},
		{name: "a source id longer than the limit", source: strings.Repeat("s", connector.MaxSourceIDLen+1), cursor: "p1"},
		{name: "a cursor longer than the limit", source: source, cursor: strings.Repeat("p", connector.MaxCursorLen+1)},
		{name: "a negative event count", source: source, cursor: "p1", events: -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := pool.Exec(t.Context(),
				`INSERT INTO l0_backfill_cursors (source, cursor, done, events) VALUES ($1, $2, false, $3)`,
				tt.source, tt.cursor, tt.events)
			if err == nil {
				t.Fatal("the table accepted a row the store refuses")
			}
			if !strings.Contains(err.Error(), "l0_backfill_cursors_") {
				t.Errorf("INSERT = %v, want one of the table's own constraints", err)
			}
		})
	}
}
