//go:build integration

package l1_test

import (
	"context"
	"errors"
	"os"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/db"
	"github.com/kpenfound/hearsay/internal/l1"
)

// newPool connects to the database the integration-test check brings up. It
// uses db.Connect rather than Open, so a database the migrations have not been
// run against fails here saying so: tests never build a schema of their own.
func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("HEARSAY_DATABASE_URL")
	if url == "" {
		t.Skip("HEARSAY_DATABASE_URL is not set")
	}
	pool, err := db.Connect(context.Background(), url)
	if err != nil {
		t.Fatalf("connecting to postgres: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// sources is what keeps tests out of each other's way: the database outlives
// one test, so a test takes a source id nothing else uses rather than
// truncating a table another test is reading.
var sources atomic.Int64

func newSource(t *testing.T) string {
	t.Helper()
	return "t" + strconv.FormatInt(time.Now().UnixNano(), 36) + "x" + strconv.FormatInt(sources.Add(1), 36)
}

// storedDoc is a document ready to be written, from a source nothing else uses.
func storedDoc(t *testing.T, src, artifact string, with func(*l1.Document)) l1.Document {
	t.Helper()
	doc := l1.Document{
		ID:     l1.DocID(src, artifact),
		Kind:   l1.KindPR,
		Source: l1.Source{System: src, NativeID: artifact, URL: "https://github.com/" + artifact},
		L0Refs: []string{connector.EventID(src, artifact)},
		Time:   l1.Times{Created: day, Updated: day, LastActivity: day},
		Participants: []l1.Participant{
			{PrincipalID: "kyle", Role: connector.RoleAuthor},
			{PrincipalID: "sam", Role: connector.RoleReviewer},
		},
		Scope:      []string{"code:acme/api:engine"},
		References: []l1.Reference{{Type: l1.RefTrackerItem, ID: "acme/api#12"}},
		ACL:        connector.ACL{{Kind: connector.ACLPublic}},
		Text:       "a distillation",
		RawText:    "what was said",
		Body: l1.Body{
			Summary:       "a distillation",
			Change:        "moves the lock",
			Outcome:       "merged",
			OutcomeKind:   l1.OutcomeResolved,
			OpenQuestions: []string{"does the queue need it too?"},
		},
	}
	if with != nil {
		with(&doc)
	}
	return doc
}

// The acceptance criterion, at the level of one row: writing the same document
// twice writes it once, and the second write does not touch the row.
func TestPutIsIdempotent(t *testing.T) {
	store := l1.New(newPool(t))
	src := newSource(t)
	doc := storedDoc(t, src, "acme/api#31", nil)

	written, err := store.Put(t.Context(), doc)
	if err != nil {
		t.Fatalf("Put() = %v", err)
	}
	if !written {
		t.Error("Put(first).written = false, want true")
	}
	first, err := store.Get(t.Context(), doc.ID)
	if err != nil {
		t.Fatalf("Get() = %v", err)
	}

	// A second run of a stateless distiller rebuilds the same document from the
	// same events. Nothing about the row may move, distilled_at included.
	written, err = store.Put(t.Context(), doc)
	if err != nil {
		t.Fatalf("Put(again) = %v", err)
	}
	if written {
		t.Error("Put(again).written = true, want false: the document had not changed")
	}
	second, err := store.Get(t.Context(), doc.ID)
	if err != nil {
		t.Fatalf("Get(again) = %v", err)
	}
	if !second.DistilledAt.Equal(first.DistilledAt) {
		t.Errorf("DistilledAt moved from %s to %s on a re-distillation that changed nothing", first.DistilledAt, second.DistilledAt)
	}
	assertSameDocument(t, first.Document, second.Document)

	// And a document that did change is written, with the row's timestamp
	// moving with it.
	changed := doc
	changed.Body.Outcome = "reverted"
	changed.Text = "a different distillation"
	written, err = store.Put(t.Context(), changed)
	if err != nil {
		t.Fatalf("Put(changed) = %v", err)
	}
	if !written {
		t.Fatal("Put(changed).written = false, want true")
	}
	third, err := store.Get(t.Context(), doc.ID)
	if err != nil {
		t.Fatalf("Get(changed) = %v", err)
	}
	if !third.DistilledAt.After(first.DistilledAt) {
		t.Errorf("DistilledAt = %s after a change, want later than %s", third.DistilledAt, first.DistilledAt)
	}
	if third.Body.Outcome != "reverted" {
		t.Errorf("Outcome = %q, want the one that was written", third.Body.Outcome)
	}
}

// Every field of the envelope survives the round trip, which is what makes a
// stored document the one that was built.
func TestPutAndGetRoundTrip(t *testing.T) {
	store := l1.New(newPool(t))
	src := newSource(t)
	doc := storedDoc(t, src, "acme/api#31", func(d *l1.Document) {
		d.Time.Updated = day.Add(time.Hour)
		d.Time.LastActivity = day.Add(2 * time.Hour)
		d.ACL = connector.ACL{
			{Kind: connector.ACLGroup, Source: src, NativeID: "acme/api", Label: "collaborators"},
			{Kind: connector.ACLIdentity, Source: src, NativeID: "u9"},
		}
		d.Scope = []string{"code:acme/api:engine", "tracker:" + src + ":acme/api#31"}
		d.References = []l1.Reference{
			{Type: l1.RefPerson, ID: "sam"},
			{Type: l1.RefSystem, ID: "code:acme/api:engine"},
		}
	})
	if _, err := store.Put(t.Context(), doc); err != nil {
		t.Fatalf("Put() = %v", err)
	}
	stored, err := store.Get(t.Context(), doc.ID)
	if err != nil {
		t.Fatalf("Get() = %v", err)
	}
	assertSameDocument(t, stored.Document, doc)
	if stored.Time.Created.Location() != time.UTC {
		t.Errorf("the times came back in %s, want UTC", stored.Time.Created.Location())
	}
}

func TestGetAndDelete(t *testing.T) {
	store := l1.New(newPool(t))
	src := newSource(t)
	doc := storedDoc(t, src, "acme/api#31", nil)

	if _, err := store.Get(t.Context(), doc.ID); !errors.Is(err, l1.ErrNotFound) {
		t.Fatalf("Get(missing) = %v, want l1.ErrNotFound", err)
	}
	if deleted, err := store.Delete(t.Context(), doc.ID); err != nil || deleted {
		t.Fatalf("Delete(missing) = %v, %v, want false and no error", deleted, err)
	}
	if _, err := store.Put(t.Context(), doc); err != nil {
		t.Fatalf("Put() = %v", err)
	}
	if deleted, err := store.Delete(t.Context(), doc.ID); err != nil || !deleted {
		t.Fatalf("Delete() = %v, %v, want true and no error", deleted, err)
	}
	if _, err := store.Get(t.Context(), doc.ID); !errors.Is(err, l1.ErrNotFound) {
		t.Errorf("Get(deleted) = %v, want l1.ErrNotFound", err)
	}
}

// The table refuses a document the layer would refuse, so a row written by
// anything else behaves the same.
func TestPutRefusesADocumentTheLayerRefuses(t *testing.T) {
	store := l1.New(newPool(t))
	src := newSource(t)
	for _, tt := range []struct {
		name string
		with func(*l1.Document)
	}{
		{"an outcome kind that is not one of the five", func(d *l1.Document) { d.Body.OutcomeKind = "merged" }},
		{"no provenance", func(d *l1.Document) { d.L0Refs = nil }},
		{"an empty access list", func(d *l1.Document) { d.ACL = nil }},
		{"an id that is not derived", func(d *l1.Document) { d.ID = "l1:" + src + ":something-else" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := store.Put(t.Context(), storedDoc(t, src, "acme/api#41", tt.with)); err == nil {
				t.Fatal("Put() = nil, want an error")
			}
		})
	}
}

// Every filter on a listing narrows it, and one that nobody set contributes
// nothing.
func TestList(t *testing.T) {
	store := l1.New(newPool(t))
	src := newSource(t)
	other := newSource(t)

	pr := storedDoc(t, src, "acme/api#1", func(d *l1.Document) {
		d.Kind = l1.KindPR
		d.Body.OutcomeKind = l1.OutcomeResolved
		d.Time.LastActivity = day.Add(3 * time.Hour)
		d.Scope = []string{"code:acme/api:engine"}
	})
	issue := storedDoc(t, src, "acme/api#2", func(d *l1.Document) {
		d.Kind = l1.KindIssue
		d.Body.OutcomeKind = l1.OutcomeOpen
		d.Body.Change = ""
		d.Time.LastActivity = day.Add(time.Hour)
		d.Scope = []string{"code:acme/api:queue"}
	})
	elsewhere := storedDoc(t, other, "acme/other#1", func(d *l1.Document) {
		d.Body.OutcomeKind = l1.OutcomeNone
	})
	for _, doc := range []l1.Document{pr, issue, elsewhere} {
		if _, err := store.Put(t.Context(), doc); err != nil {
			t.Fatalf("Put(%s) = %v", doc.ID, err)
		}
	}

	tests := []struct {
		name string
		opts l1.ListOptions
		want []string
	}{
		{"one source, newest activity first", l1.ListOptions{Source: src}, []string{pr.ID, issue.ID}},
		{"oldest first", l1.ListOptions{Source: src, Oldest: true}, []string{issue.ID, pr.ID}},
		{"one kind", l1.ListOptions{Source: src, Kind: l1.KindIssue}, []string{issue.ID}},
		{"one outcome kind", l1.ListOptions{Source: src, OutcomeKind: l1.OutcomeResolved}, []string{pr.ID}},
		{"what the assertion pipeline reads", l1.ListOptions{Source: src, Asserting: true}, []string{pr.ID}},
		{"one entity", l1.ListOptions{Source: src, Scope: "code:acme/api:queue"}, []string{issue.ID}},
		{"an entity nothing is about", l1.ListOptions{Source: src, Scope: "code:acme/api:nothing"}, nil},
		{"a limit", l1.ListOptions{Source: src, Limit: 1}, []string{pr.ID}},
		{"another source entirely", l1.ListOptions{Source: other}, []string{elsewhere.ID}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			docs, err := store.List(t.Context(), tt.opts)
			if err != nil {
				t.Fatalf("List() = %v", err)
			}
			var ids []string
			for _, doc := range docs {
				ids = append(ids, doc.ID)
			}
			if !slices.Equal(ids, tt.want) {
				t.Errorf("List() = %v, want %v", ids, tt.want)
			}
		})
	}
}

// assertSameDocument compares every field of the envelope, so that a column
// nothing else looks at is still checked.
func assertSameDocument(t *testing.T, got, want l1.Document) {
	t.Helper()
	if got.ID != want.ID || got.Kind != want.Kind || got.Source != want.Source {
		t.Errorf("envelope = %+v, want %+v", got.Source, want.Source)
	}
	if !slices.Equal(got.L0Refs, want.L0Refs) {
		t.Errorf("L0Refs = %v, want %v", got.L0Refs, want.L0Refs)
	}
	if !got.Time.Created.Equal(want.Time.Created) || !got.Time.Updated.Equal(want.Time.Updated) || !got.Time.LastActivity.Equal(want.Time.LastActivity) {
		t.Errorf("Time = %+v, want %+v", got.Time, want.Time)
	}
	if !slices.Equal(got.Participants, want.Participants) {
		t.Errorf("Participants = %v, want %v", got.Participants, want.Participants)
	}
	if !slices.Equal(got.Scope, want.Scope) {
		t.Errorf("Scope = %v, want %v", got.Scope, want.Scope)
	}
	if !slices.Equal(got.References, want.References) {
		t.Errorf("References = %v, want %v", got.References, want.References)
	}
	if !slices.Equal(got.ACL, want.ACL) {
		t.Errorf("ACL = %v, want %v", got.ACL, want.ACL)
	}
	if got.Text != want.Text || got.RawText != want.RawText {
		t.Errorf("Text = %q and RawText = %q, want %q and %q", got.Text, got.RawText, want.Text, want.RawText)
	}
	if got.Body.Summary != want.Body.Summary || got.Body.Question != want.Body.Question ||
		got.Body.Outcome != want.Body.Outcome || got.Body.OutcomeKind != want.Body.OutcomeKind ||
		got.Body.Change != want.Body.Change || !slices.Equal(got.Body.OpenQuestions, want.Body.OpenQuestions) {
		t.Errorf("Body = %+v, want %+v", got.Body, want.Body)
	}
}
