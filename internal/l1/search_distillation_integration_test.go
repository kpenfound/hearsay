//go:build integration

package l1_test

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/db/dbtest"
	"github.com/kpenfound/hearsay/internal/l1"
)

func distilled(t *testing.T, src, artifact, summary, outcome string) l1.Document {
	t.Helper()
	doc := searchDoc(t, src, artifact, "an ordinary exchange about nothing in particular", nil)
	doc.Body.Summary = summary
	doc.Body.Outcome = outcome
	return doc
}

func TestSearchFindsDistilledSummaryAndOutcomeWithoutEmbeddings(t *testing.T) {
	store := l1.New(dbtest.Pool(t))
	src := dbtest.UniqueID()

	inSummary := distilled(t, src, "conv#1", "The owner settled on the zephyrine migration path.", "")
	inOutcome := distilled(t, src, "conv#2", "A short exchange.", "Agreed to adopt the quillfeather schema.")
	inRaw := searchDoc(t, src, "conv#3", "we talked about the marmalade rollout", nil)
	unrelated := distilled(t, src, "conv#4", "Greetings.", "")
	for _, doc := range []l1.Document{inSummary, inOutcome, inRaw, unrelated} {
		put(t, store, doc)
	}

	for _, tc := range []struct {
		name  string
		query string
		want  []string
	}{
		{name: "a summary-only word", query: "zephyrine", want: []string{inSummary.ID}},
		{name: "an outcome-only word", query: "quillfeather", want: []string{inOutcome.ID}},
		{name: "a raw_text word", query: "marmalade", want: []string{inRaw.ID}},
		{name: "a stemmed summary word", query: "migrations", want: []string{inSummary.ID}},
		{name: "a word nobody wrote", query: "xylophone", want: []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := store.Search(t.Context(), reader(testScope(src)), l1.SearchOptions{Query: tc.query, Scope: testScope(src)})
			if err != nil {
				t.Fatalf("Search() = %v, want no error", err)
			}
			if got := ids(result.Hits); !slices.Equal(got, tc.want) {
				t.Errorf("Search(%q) = %v, want %v", tc.query, got, tc.want)
			}
			if result.Truncated {
				t.Errorf("Search(%q) is truncated, want everything that matched returned", tc.query)
			}
		})
	}
}

func TestSearchWindow(t *testing.T) {
	store := l1.New(dbtest.Pool(t))
	src := dbtest.UniqueID()
	at := func(day int) time.Time { return time.Date(2026, 3, day, 12, 0, 0, 0, time.UTC) }
	span := func(artifact string, created, last int) l1.Document {
		doc := searchDoc(t, src, artifact, "the windowed topic", nil)
		doc.Time = l1.Times{Created: at(created), Updated: at(last), LastActivity: at(last)}
		put(t, store, doc)
		return doc
	}
	early := span("w#early", 1, 2)
	spanning := span("w#spanning", 2, 20)
	late := span("w#late", 18, 19)

	rfc := func(day int) string { return at(day).Format(time.RFC3339) }
	for _, tc := range []struct {
		name         string
		since, until string
		want         []l1.Document
	}{
		{name: "no window", want: []l1.Document{early, spanning, late}},
		{name: "since only", since: rfc(10), want: []l1.Document{spanning, late}},
		{name: "until only", until: rfc(10), want: []l1.Document{early, spanning}},
		{name: "both", since: rfc(5), until: rfc(15), want: []l1.Document{spanning}},
		{name: "since is inclusive of last activity", since: rfc(19), want: []l1.Document{spanning, late}},
		{name: "until is exclusive of creation", until: rfc(18), want: []l1.Document{early, spanning}},
		{name: "an empty window", since: rfc(10), until: rfc(10), want: []l1.Document{spanning}},
		{name: "a window nothing is in", since: rfc(25), want: nil},
		{name: "an offset timestamp", since: at(10).In(time.FixedZone("x", 5*3600)).Format(time.RFC3339), want: []l1.Document{spanning, late}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := store.Search(t.Context(), reader(testScope(src)), l1.SearchOptions{
				Query: "windowed", Scope: testScope(src), Since: tc.since, Until: tc.until,
			})
			if err != nil {
				t.Fatalf("Search() = %v, want no error", err)
			}
			got := ids(result.Hits)
			want := make([]string, len(tc.want))
			for i, d := range tc.want {
				want[i] = d.ID
			}
			slices.Sort(got)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("Search() = %v, want %v", got, want)
			}
		})
	}
}

func TestSearchRefusesABadWindow(t *testing.T) {
	store := l1.New(dbtest.Pool(t))
	src := dbtest.UniqueID()
	for _, tc := range []struct{ name, since, until string }{
		{name: "malformed since", since: "yesterday"},
		{name: "malformed until", until: "2026-03-01"},
		{name: "inverted", since: "2026-03-10T00:00:00Z", until: "2026-03-09T00:00:00Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := store.Search(t.Context(), reader(testScope(src)), l1.SearchOptions{
				Query: "windowed", Scope: testScope(src), Since: tc.since, Until: tc.until,
			})
			if !errors.Is(err, l1.ErrInvalidSearch) {
				t.Errorf("Search() = %v, want l1.ErrInvalidSearch", err)
			}
		})
	}
}

func TestSearchReportsTruncation(t *testing.T) {
	store := l1.New(dbtest.Pool(t))
	src := dbtest.UniqueID()
	for i := range l1.SearchCandidates + 1 {
		put(t, store, searchDoc(t, src, fmt.Sprintf("many#%03d", i), "the sprawling topic", nil))
	}
	for i := range 3 {
		put(t, store, searchDoc(t, src, fmt.Sprintf("few#%d", i), "the compact subject", nil))
	}

	for _, tc := range []struct {
		name  string
		query string
		limit int
		want  int
		cut   bool
	}{
		{name: "everything matched was returned", query: "compact", limit: 3, want: 3},
		{name: "fewer matched than the limit", query: "compact", limit: 10, want: 3},
		{name: "more matched than the limit", query: "compact", limit: 2, want: 2, cut: true},
		{name: "the default limit under many matches", query: "sprawling", want: l1.DefaultSearchLimit, cut: true},
		{name: "a half past its cutoff with every candidate returned", query: "sprawling", limit: l1.MaxSearchLimit, want: l1.SearchCandidates, cut: true},
		{name: "nothing matched", query: "xylophone", want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := store.Search(t.Context(), reader(testScope(src)), l1.SearchOptions{
				Query: tc.query, Scope: testScope(src), Limit: tc.limit,
			})
			if err != nil {
				t.Fatalf("Search() = %v, want no error", err)
			}
			if len(result.Hits) != tc.want || result.Truncated != tc.cut {
				t.Errorf("Search() = %d hits, truncated %v, want %d, %v", len(result.Hits), result.Truncated, tc.want, tc.cut)
			}
		})
	}
}

func TestSearchTruncationIgnoresWhatTheReaderMayNotRead(t *testing.T) {
	store := l1.New(dbtest.Pool(t))
	src := dbtest.UniqueID()
	group := connector.ACLEntry{Kind: connector.ACLGroup, Source: src, NativeID: "t1"}
	for i := range 3 {
		put(t, store, searchDoc(t, src, fmt.Sprintf("open#%d", i), "the shared topic", nil))
	}
	put(t, store, searchDoc(t, src, "closed#0", "the shared topic",
		connector.ACL{{Kind: connector.ACLGroup, Source: src, NativeID: "t1"}}))
	opts := l1.SearchOptions{Query: "shared", Scope: testScope(src), Limit: 3}

	outsider, err := store.Search(t.Context(), reader(testScope(src)), opts)
	if err != nil {
		t.Fatalf("Search() = %v, want no error", err)
	}
	if len(outsider.Hits) != 3 || outsider.Truncated {
		t.Errorf("Search(outsider) = %d hits, truncated %v, want 3 and not truncated", len(outsider.Hits), outsider.Truncated)
	}

	member, err := store.Search(t.Context(), reader(testScope(src), group), opts)
	if err != nil {
		t.Fatalf("Search(member) = %v, want no error", err)
	}
	if len(member.Hits) != 3 || !member.Truncated {
		t.Errorf("Search(member) = %d hits, truncated %v, want 3 and truncated", len(member.Hits), member.Truncated)
	}

	windowed := opts
	windowed.Since = time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	none, err := store.Search(t.Context(), reader(testScope(src), group), windowed)
	if err != nil || len(none.Hits) != 0 || none.Truncated {
		t.Errorf("Search(member, empty window) = %+v, %v, want nothing and not truncated", none, err)
	}
}
