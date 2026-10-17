//go:build integration

package l1_test

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
)

func TestStateRoundTripsAndMovesWatermark(t *testing.T) {
	pool := newPool(t)
	store := l1.New(pool)
	doc := storedDoc(t, newSource(t), "acme/api#state", nil)
	// A repeatable snapshot isolates watermark changes from other integration
	// suites while keeping this transaction's own trigger inserts visible.
	tx, err := pool.BeginTx(t.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	store = l1.New(tx)
	label, renamed := " In review ", "Waiting"
	var previous time.Time
	for _, tt := range []struct {
		name    string
		state   *l1.State
		changed bool
	}{
		{"initial absent", nil, true},
		{"set", &l1.State{Core: connector.CoreStateStarted, Native: &label}, true},
		{"unchanged", &l1.State{Core: connector.CoreStateStarted, Native: &label}, false},
		{"core only changes", &l1.State{Core: connector.CoreStateBlocked, Native: &label}, true},
		{"native only changes", &l1.State{Core: connector.CoreStateBlocked, Native: &renamed}, true},
		{"remove native", &l1.State{Core: connector.CoreStateBlocked}, true},
		{"remove state", nil, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var before, after int64
			if err := tx.QueryRow(t.Context(), "SELECT count(*) FROM bundle_changes").Scan(&before); err != nil {
				t.Fatal(err)
			}
			doc.State = tt.state
			written, err := store.Put(t.Context(), doc)
			if err != nil || written != tt.changed {
				t.Fatalf("Put = %v, %v", written, err)
			}
			if err := tx.QueryRow(t.Context(), "SELECT count(*) FROM bundle_changes").Scan(&after); err != nil {
				t.Fatal(err)
			}
			delta := int64(0)
			if tt.changed {
				delta = 1
			}
			if after-before != delta {
				t.Fatalf("watermark advanced %d, want %d", after-before, delta)
			}
			got, err := store.Get(t.Context(), doc.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.State, tt.state) {
				t.Fatalf("Get state = %+v, want %+v", got.State, tt.state)
			}
			if !tt.changed && !got.DistilledAt.Equal(previous) {
				t.Fatal("unchanged state rewrote row")
			}
			previous = got.DistilledAt
			r := reader(doc.Scope[0])
			opts := l1.ListOptions{Source: doc.Source.System}
			list, err := store.List(t.Context(), opts)
			if err != nil || len(list) != 1 {
				t.Fatalf("List = %v, %v", list, err)
			}
			readable, err := store.ListFor(t.Context(), r, opts)
			if err != nil || len(readable) != 1 {
				t.Fatalf("ListFor = %v, %v", readable, err)
			}
			hits, err := store.Search(t.Context(), r, l1.SearchOptions{Query: "what was said"})
			if err != nil {
				t.Fatal(err)
			}
			hit, ok := found(hits)[doc.ID]
			if !ok {
				t.Fatal("search missed document")
			}
			prs, err := store.PRsByReference(t.Context(), doc.Source.NativeID)
			if err != nil || len(prs) != 1 {
				t.Fatalf("PRsByReference = %v, %v", prs, err)
			}
			for _, result := range []l1.Stored{list[0], readable[0], hit.Stored, prs[0]} {
				if !reflect.DeepEqual(result.State, tt.state) {
					t.Fatalf("read state = %+v, want %+v", result.State, tt.state)
				}
			}
		})
	}
}

func TestListForStatesAppliesReachAndACL(t *testing.T) {
	store := l1.New(newPool(t))
	src := newSource(t)
	scope := "state:" + src
	private := connector.ACLEntry{Kind: connector.ACLGroup, Source: src, NativeID: "team"}
	for _, tt := range []struct {
		key, scope string
		state      connector.CoreState
		restricted bool
	}{
		{"started", scope, connector.CoreStateStarted, false}, {"blocked", scope, connector.CoreStateBlocked, false},
		{"completed", scope, connector.CoreStateCompleted, false}, {"absent", scope, "", false},
		{"hidden", scope, connector.CoreStateStarted, true}, {"outside", "outside:" + src, connector.CoreStateStarted, false},
	} {
		doc := storedDoc(t, src, tt.key, func(d *l1.Document) {
			d.Scope = []string{tt.scope}
			if tt.state != "" {
				d.State = &l1.State{Core: tt.state}
			}
			if tt.restricted {
				d.ACL = connector.ACL{private}
			}
		})
		put(t, store, doc)
	}
	for _, tt := range []struct {
		name   string
		states []string
		grant  bool
		want   int
	}{
		{"started", []string{"started"}, false, 1}, {"either", []string{"started", "blocked"}, false, 2},
		{"ACL granted", []string{"started"}, true, 2}, {"no match", []string{"abandoned"}, false, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := reader(scope)
			if tt.grant {
				r = reader(scope, private)
			}
			opts := l1.ListOptions{Source: src, States: tt.states}
			docs, err := store.ListFor(t.Context(), r, opts)
			if err != nil || len(docs) != tt.want {
				t.Fatalf("ListFor = %d, %v, want %d", len(docs), err, tt.want)
			}
			for _, d := range docs {
				if d.State == nil {
					t.Fatal("stateless document matched")
				}
			}
			if _, err := store.List(t.Context(), opts); !errors.Is(err, l1.ErrInvalidDocument) {
				t.Fatalf("List error = %v", err)
			}
		})
	}
	if _, err := store.ListFor(t.Context(), reader(scope), l1.ListOptions{States: []string{"closed"}}); !errors.Is(err, l1.ErrInvalidDocument) {
		t.Fatalf("unknown state error = %v", err)
	}
}

func TestStateOnlyChangesRewriteDistilledAt(t *testing.T) {
	store := l1.New(newPool(t))
	doc := storedDoc(t, newSource(t), "state-time", nil)
	put(t, store, doc)
	previous, err := store.Get(t.Context(), doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []*l1.State{{Core: connector.CoreStateStarted}, {Core: connector.CoreStateCompleted}, nil} {
		doc.State = state
		written, err := store.Put(t.Context(), doc)
		if err != nil || !written {
			t.Fatalf("state-only Put = %v, %v", written, err)
		}
		current, err := store.Get(t.Context(), doc.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !current.DistilledAt.After(previous.DistilledAt) {
			t.Fatal("state-only edit did not move distilled_at")
		}
		previous = current
	}
}
