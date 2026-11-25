//go:build integration

package l1_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/config"
	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/db/dbtest"
	"github.com/kpenfound/hearsay/internal/l1"
)

func storedDoc(t *testing.T, src, artifact string, with func(*l1.Document)) l1.Document {
	t.Helper()
	doc := l1.Document{
		ID:   l1.DocID(src, artifact),
		Kind: l1.KindPR, ArtifactClass: config.ArtifactPullRequest,
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

func TestChangingArtifactClassUpdatesRow(t *testing.T) {
	store := l1.New(dbtest.Pool(t))
	doc := storedDoc(t, dbtest.UniqueID(), "acme/api#class", nil)
	seenMerged := false
	for _, class := range []config.ArtifactClass{config.ArtifactPullRequest, config.ArtifactMergedPR, config.ArtifactMergedPR} {
		doc.ArtifactClass = class
		changed, err := store.Put(t.Context(), doc)
		if err != nil {
			t.Fatal(err)
		}
		if changed != (class != config.ArtifactMergedPR || !seenMerged) {
			t.Errorf("Put(%s) changed = %v", class, changed)
		}
		got, err := store.Get(t.Context(), doc.ID)
		if err != nil || got.ArtifactClass != class {
			t.Errorf("Get() class = %q, %v; want %q", got.ArtifactClass, err, class)
		}
		if class == config.ArtifactMergedPR {
			seenMerged = true
		}
	}
}

func TestPutIsIdempotent(t *testing.T) {
	store := l1.New(dbtest.Pool(t))
	src := dbtest.UniqueID()
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

func TestPutAndGetRoundTrip(t *testing.T) {
	store := l1.New(dbtest.Pool(t))
	src := dbtest.UniqueID()
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

func TestPutADocumentWithNothingOptionalOnIt(t *testing.T) {
	store := l1.New(dbtest.Pool(t))
	src := dbtest.UniqueID()
	doc := storedDoc(t, src, "acme/api#51", func(d *l1.Document) {
		d.Participants = nil
		d.Scope = nil
		d.References = nil
		d.Body.OpenQuestions = nil
	})
	if _, err := store.Put(t.Context(), doc); err != nil {
		t.Fatalf("Put() = %v", err)
	}
	stored, err := store.Get(t.Context(), doc.ID)
	if err != nil {
		t.Fatalf("Get() = %v", err)
	}
	if len(stored.Scope) != 0 || len(stored.References) != 0 || len(stored.Participants) != 0 {
		t.Errorf("the empty columns came back as %v, %v, %v", stored.Scope, stored.References, stored.Participants)
	}
	empty := doc
	empty.Scope = []string{}
	empty.References = []l1.Reference{}
	empty.Participants = []l1.Participant{}
	if written, err := store.Put(t.Context(), empty); err != nil || written {
		t.Errorf("Put(the same document with empty lists) = %v, %v, want no write", written, err)
	}
}

func TestGetAndDelete(t *testing.T) {
	store := l1.New(dbtest.Pool(t))
	src := dbtest.UniqueID()
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

func TestTheTableRefusesWhatTheLayerRefuses(t *testing.T) {
	pool := dbtest.Pool(t)
	src := dbtest.UniqueID()
	insert := `INSERT INTO l1_docs (id, kind, artifact_class, source, source_native_id, source_url, l0_refs,
		created_at, updated_at, last_activity_at, participants, scope, refs, acl,
		text, raw_text, body, outcome_kind)
		VALUES ($1, 'pr', 'pull_request', $2, $3, '', $4, $5, $5, $5, '[]'::jsonb, '{}', '[]'::jsonb, $6::jsonb,
		        'text', 'raw', $7::jsonb, $8)`

	tests := []struct {
		name       string
		artifact   string
		l0Refs     []string
		acl        string
		body       string
		outcome    string
		constraint string
	}{
		{name: "an outcome kind that is not one of the five", artifact: "acme/api#1",
			l0Refs: []string{"evt:x"}, acl: `[{"kind":"public"}]`,
			body: `{"outcome_kind":"merged"}`, outcome: "merged", constraint: "outcome_kind"},
		{name: "no provenance", artifact: "acme/api#2",
			l0Refs: []string{}, acl: `[{"kind":"public"}]`,
			body: `{"outcome_kind":"none"}`, outcome: "none", constraint: "provenance"},
		{name: "an empty access list", artifact: "acme/api#3",
			l0Refs: []string{"evt:x"}, acl: `[]`,
			body: `{"outcome_kind":"none"}`, outcome: "none", constraint: "acl"},
		{name: "a body that is not an object", artifact: "acme/api#4",
			l0Refs: []string{"evt:x"}, acl: `[{"kind":"public"}]`,
			body: `"none"`, outcome: "none", constraint: "body"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := pool.Exec(t.Context(), insert,
				l1.DocID(src, tt.artifact), src, tt.artifact, tt.l0Refs, day, tt.acl, tt.body, tt.outcome)
			if err == nil {
				t.Fatalf("the table accepted %s", tt.name)
			}
			if !strings.Contains(err.Error(), tt.constraint) {
				t.Errorf("the row was refused by %v, want the constraint about %s", err, tt.constraint)
			}
		})
	}

	_, err := pool.Exec(t.Context(), insert,
		"l1:"+src+":something-else", src, "acme/api#5", []string{"evt:x"}, day,
		`[{"kind":"public"}]`, `{"outcome_kind":"none"}`, "none")
	if err == nil {
		t.Fatal("the table accepted an id that is not the derived one")
	}
	if !strings.Contains(err.Error(), "id_is_derived") {
		t.Errorf("the row was refused by %v, want the constraint about the id", err)
	}
}

func TestList(t *testing.T) {
	store := l1.New(dbtest.Pool(t))
	src := dbtest.UniqueID()
	other := dbtest.UniqueID()

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
	if !got.ACL.Equal(want.ACL) {
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
