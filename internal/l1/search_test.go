package l1_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/principal"
)

// recorder is a [l1.Querier] that answers nothing and remembers what it was
// asked, so that a test can tell a search that was refused before it ran from
// one that reached the database.
type recorder struct {
	sql  string
	args []any
	ran  bool
}

var errNotADatabase = errors.New("this is not a database")

func (r *recorder) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	r.sql, r.args, r.ran = sql, args, true
	return nil, errNotADatabase
}

func (r *recorder) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	r.sql, r.args, r.ran = sql, args, true
	return nil
}

func readerFor(human string, scopes principal.Scopes) l1.Reader {
	return l1.Reader{Effective: principal.Effective{Human: human, Grant: principal.Grant{Scopes: scopes}}}
}

// A reader who may see nothing gets nothing, and the database is never asked:
// the filter is not a WHERE clause somebody could forget, it is the reason the
// statement runs at all.
func TestSearchFailsClosedBeforeItRuns(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reader l1.Reader
		scope  string
		want   bool // whether the database is asked
	}{{
		name:   "nobody",
		reader: l1.Reader{},
	}, {
		name:   "a reader with no scopes",
		reader: readerFor("kyle", principal.Scopes{}),
	}, {
		name:   "a scope the reader was not granted",
		reader: readerFor("kyle", principal.SomeScopes("code:acme/api:engine")),
		scope:  "code:acme/api:other",
	}, {
		name:   "a scope with no principal behind it",
		reader: readerFor("", principal.AllScopes()),
		scope:  "code:acme/api:engine",
	}, {
		name:   "a scope the reader was granted",
		reader: readerFor("kyle", principal.SomeScopes("code:acme/api:engine")),
		scope:  "code:acme/api:engine",
		want:   true,
	}, {
		name:   "every scope",
		reader: readerFor("kyle", principal.AllScopes()),
		want:   true,
	}, {
		name:   "some scopes and no scope asked for",
		reader: readerFor("kyle", principal.SomeScopes("code:acme/api:engine")),
		want:   true,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			var rec recorder
			hits, err := l1.New(&rec).Search(t.Context(), tc.reader, l1.SearchOptions{
				Query: "the engine schema",
				Scope: tc.scope,
			})
			if rec.ran != tc.want {
				t.Fatalf("the database was asked: %v, want %v", rec.ran, tc.want)
			}
			if !tc.want {
				if err != nil || len(hits) != 0 {
					t.Fatalf("Search() = %v, %v, want no hits and no error", hits, err)
				}
				return
			}
			if !errors.Is(err, errNotADatabase) {
				t.Fatalf("Search() = %v, want the database's own error", err)
			}
			// Every statement this builds filters by the access list, whatever
			// the scopes said: they are two filters and not one
			// (docs/design.md#access-control).
			if want := strings.Count(rec.sql, "acl @> $"); want != 2 {
				t.Errorf("the statement holds %d access-list predicates, want one in each half of the search", want)
			}
			if !argsHold(rec.args, `"kind":"public"`) {
				t.Error("the statement never allows a public document")
			}
		})
	}
}

func argsHold(args []any, want string) bool {
	for _, a := range args {
		if s, ok := a.(string); ok && strings.Contains(s, want) {
			return true
		}
	}
	return false
}

// A search with nothing to search for is a listing, and saying so is better
// than ranking the whole table by nothing.
func TestSearchRefusesAnEmptyQuery(t *testing.T) {
	for _, query := range []string{"", "   ", "\n\t"} {
		var rec recorder
		_, err := l1.New(&rec).Search(t.Context(), readerFor("kyle", principal.AllScopes()), l1.SearchOptions{Query: query})
		if !errors.Is(err, l1.ErrInvalidSearch) {
			t.Errorf("Search(%q) = %v, want l1.ErrInvalidSearch", query, err)
		}
		if rec.ran {
			t.Errorf("Search(%q) asked the database anyway", query)
		}
	}
}

// The bounds a search is held to.
func TestSearchLimit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		limit int
		want  int
	}{
		{name: "none asked for", limit: 0, want: l1.DefaultSearchLimit},
		{name: "negative", limit: -1, want: l1.DefaultSearchLimit},
		{name: "one", limit: 1, want: 1},
		{name: "the most there is", limit: l1.MaxSearchLimit, want: l1.MaxSearchLimit},
		{name: "more than there is", limit: l1.MaxSearchLimit + 1, want: l1.MaxSearchLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := l1.SearchLimit(tc.limit); got != tc.want {
				t.Errorf("SearchLimit(%d) = %d, want %d", tc.limit, got, tc.want)
			}
		})
	}
	// A search cannot promise more documents than either half of it ranks.
	if l1.MaxSearchLimit > l1.SearchCandidates {
		t.Errorf("a search returns up to %d documents and ranks %d", l1.MaxSearchLimit, l1.SearchCandidates)
	}
}

// stubEmbedder is an embed tier that answers whatever it was built with. No
// test here makes a model call: what the fixture-backed fake in internal/llm is
// for is a test of a request, and this is a test of what happens to the answer.
type stubEmbedder struct {
	dimensions int
	vectors    [][]float32
	err        error
}

func (e stubEmbedder) Dimensions() int { return e.dimensions }

func (e stubEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return e.vectors, e.err
}

// A caller that asked for both halves and silently got one could not tell, so
// an embed tier that cannot answer takes the search with it rather than
// quietly leaving it to full text.
func TestSearchRefusesAnEmbedTierItCannotUse(t *testing.T) {
	full := make([]float32, l1.EmbeddingDimensions)
	for _, tc := range []struct {
		name     string
		embedder l1.Embedder
		want     error
	}{{
		name:     "a tier of the wrong width",
		embedder: stubEmbedder{dimensions: l1.EmbeddingDimensions + 1},
		want:     l1.ErrEmbeddingWidth,
	}, {
		name:     "a tier that answers with the wrong width",
		embedder: stubEmbedder{dimensions: l1.EmbeddingDimensions, vectors: [][]float32{{1, 2, 3}}},
		want:     l1.ErrEmbeddingWidth,
	}, {
		name:     "a tier that fails",
		embedder: stubEmbedder{dimensions: l1.EmbeddingDimensions, err: errNotADatabase},
		want:     errNotADatabase,
	}, {
		name:     "a tier that answers with no vector at all",
		embedder: stubEmbedder{dimensions: l1.EmbeddingDimensions},
		want:     nil, // no sentinel: the message is what a caller reads
	}} {
		t.Run(tc.name, func(t *testing.T) {
			var rec recorder
			_, err := l1.New(&rec).Search(t.Context(), readerFor("kyle", principal.AllScopes()),
				l1.SearchOptions{Query: "the engine schema", Embedder: tc.embedder})
			if err == nil {
				t.Fatal("Search() = no error, want the embed tier's failure")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("Search() = %v, want %v", err, tc.want)
			}
			if rec.ran {
				t.Error("Search() ran the statement with half a search")
			}
			if strings.Contains(err.Error(), "the engine schema") {
				t.Errorf("the error quotes the query: %v", err)
			}
		})
	}
	// The positive control: a tier that answers properly reaches the database.
	var rec recorder
	_, err := l1.New(&rec).Search(t.Context(), readerFor("kyle", principal.AllScopes()),
		l1.SearchOptions{Query: "the engine schema", Embedder: stubEmbedder{
			dimensions: l1.EmbeddingDimensions,
			vectors:    [][]float32{full},
		}})
	if !errors.Is(err, errNotADatabase) || !rec.ran {
		t.Fatalf("Search() with a working embed tier = %v, ran %v, want the database's own error", err, rec.ran)
	}
}
