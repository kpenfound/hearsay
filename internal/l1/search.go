package l1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kpenfound/hearsay/internal/connector"
)

// What a search returns and how much it looks at.
const (
	// DefaultSearchLimit is how many documents a search with no limit returns.
	// It is smaller than [DefaultLimit] because a search answers a question
	// and a listing enumerates a table.
	DefaultSearchLimit = 20
	// SearchCandidates is how many documents each half of the search ranks
	// before the two are fused. Everything the filter allows is ranked; this
	// is where the ranking is cut off, and it is the reach of the search: a
	// document neither half puts in its first this many cannot be fused into
	// the answer.
	SearchCandidates = 200
	// MaxSearchLimit is the most a search returns. It is [SearchCandidates]
	// because that is the most one half can offer, and asking for more than
	// the search can see would be a limit that is not a limit.
	MaxSearchLimit = SearchCandidates
	// RRFK is the constant of the reciprocal rank fusion the two halves are
	// combined with: a document at rank r in a half contributes 1/(RRFK+r).
	RRFK = 60
)

// ErrInvalidSearch is wrapped by everything [Store.Search] refuses before it
// runs, so a caller can tell a bad request from a database error.
var ErrInvalidSearch = errors.New("invalid search")

// SearchOptions is one query over the document table.
type SearchOptions struct {
	// Query is what to look for, in a person's words. It is required: a
	// search with nothing to look for is a listing, which is [Store.List].
	//
	// It is read as a web search box reads it — quoted phrases, `or`, and a
	// leading `-` for "without" — and never as an operator language, so no
	// input is a syntax error.
	Query string
	// Scope narrows the search to documents about one entity. It is the
	// `scope` of design.md's search(scope, query), and it is optional: a
	// search with none looks in every scope the reader holds.
	//
	// A scope the reader was not granted returns nothing rather than an error.
	// Scopes filter for relevance and never grant permission, so a scope
	// somebody was not granted is not evidence about what is in it.
	Scope string
	// Embedder is the `embed` tier, used to embed the query. With none, the
	// search is its full-text half alone: fewer documents, all of them
	// matching words the team actually wrote. That is what a deployment with
	// no `embed` tier configured gets (docs/config.md).
	Embedder Embedder
	// Limit is how many documents to return, defaulting to
	// [DefaultSearchLimit] and capped at [MaxSearchLimit].
	Limit int
	// Since and Until are the window, as RFC 3339 timestamps, and each is
	// optional. A document is in the window when it was created before Until
	// and was last active at or after Since: its activity overlaps the window.
	// A timestamp that does not parse, or a Since later than Until, is
	// [ErrInvalidSearch]. The window narrows the documents both halves rank,
	// like the scope and the access list, so it is applied before ranking.
	Since string
	Until string
}

// SearchResult is what a search found and whether it stopped short of
// everything that matched.
type SearchResult struct {
	// Hits are the documents, best first, at most the limit.
	Hits []Hit
	// Truncated is true when more documents matched than Hits holds: either
	// the two halves together found more than the limit, or a half had a
	// document ranked past [SearchCandidates], which the fusion never saw.
	// It counts only documents the reader may read, because the filter runs
	// inside both halves; a document they may not read changes nothing here.
	Truncated bool
}

// Hit is one document a search found, with where each half of the search found
// it.
type Hit struct {
	Stored
	// Score is the fused rank score, higher first. It is a rank fusion and not
	// a similarity: it says this document beat that one for this query, and
	// nothing about how relevant either is.
	Score float64
	// TextRank is where the full-text half ranked this document, counting from
	// 1, and 0 for a document that half did not find at all.
	TextRank int
	// VectorRank is the same for the similarity half. A document with a
	// TextRank and no VectorRank was found by the words in it; one with a
	// VectorRank and no TextRank was found by what it is about.
	VectorRank int
}

const searchDocument = `(to_tsvector('english', raw_text)
        || to_tsvector('english', coalesce(body ->> 'summary', ''))
        || to_tsvector('english', coalesce(body ->> 'outcome', '')))`

const searchSQL = `
WITH matched AS (
    SELECT id, row_number() OVER (
        ORDER BY ts_rank_cd(` + searchDocument + `, tsq.q) DESC, id
    ) AS rank
    FROM l1_docs, websearch_to_tsquery('english', %[1]s) AS tsq(q)
    WHERE %[2]s AND ` + searchDocument + ` @@ tsq.q
),
near AS (
    SELECT id, row_number() OVER (ORDER BY embedding <=> %[3]s::text::vector, id) AS rank
    FROM l1_docs
    WHERE %[2]s AND %[3]s::text IS NOT NULL AND embedding IS NOT NULL
),
beyond AS (
    SELECT EXISTS (SELECT 1 FROM matched WHERE rank > %[5]s)
        OR EXISTS (SELECT 1 FROM near WHERE rank > %[5]s) AS beyond
),
fused AS (
    SELECT coalesce(m.id, n.id) AS doc_id,
           (coalesce(1.0 / (%[4]s + m.rank), 0) + coalesce(1.0 / (%[4]s + n.rank), 0))::float8 AS score,
           coalesce(m.rank, 0) AS text_rank,
           coalesce(n.rank, 0) AS vector_rank
    FROM (SELECT id, rank FROM matched WHERE rank <= %[5]s) m
    FULL JOIN (SELECT id, rank FROM near WHERE rank <= %[5]s) n ON m.id = n.id
)
SELECT ` + docColumns + `, fused.score, fused.text_rank, fused.vector_rank,
       count(*) OVER () AS fused_total, (SELECT beyond FROM beyond) AS beyond
FROM l1_docs JOIN fused ON l1_docs.id = fused.doc_id
ORDER BY fused.score DESC, last_activity_at DESC, l1_docs.id
LIMIT %[6]s`

// Search is hybrid retrieval over the document table: similarity over the
// embedding of `text` and full text over `raw_text` and the distilled summary
// and outcome, each ranked over the documents this reader may see and in the
// window, fused by reciprocal rank (docs/design.md#read-and-assert-api). It
// returns documents, not answers, and says whether it stopped short of
// everything that matched ([SearchResult.Truncated]).
//
// It is the one place a query becomes a ranking, so it is also the one place
// that embeds a query: [SearchOptions.Embedder] is called once, with the query,
// through the same door a document goes through ([EmbedText]). That is the only
// model call any read here makes, and the search runs without it.
//
// A reader who may see nothing gets nothing — no query, no error. That is a
// caller with no principal behind the read, or with no scopes, or asking for a
// scope they were not granted. It is *not* a caller with an empty audience:
// public is public, so somebody who satisfies no access-list entry of their own
// still reads every public document in the scopes they hold, which is what an
// access list saying `public` means (docs/design.md#access-control).
func (s *Store) Search(ctx context.Context, reader Reader, opts SearchOptions) (SearchResult, error) {
	if strings.TrimSpace(opts.Query) == "" {
		return SearchResult{}, fmt.Errorf("%w: there is nothing to search for", ErrInvalidSearch)
	}
	since, until, err := searchWindow(opts.Since, opts.Until)
	if err != nil {
		return SearchResult{}, err
	}
	q := &query{}
	filter, ok := reader.predicate(q, opts.Scope)
	if !ok {
		return SearchResult{Hits: []Hit{}}, nil
	}
	if since != nil {
		filter += " AND last_activity_at >= " + q.placeholder(*since) + "::timestamptz"
	}
	if until != nil {
		filter += " AND created_at < " + q.placeholder(*until) + "::timestamptz"
	}

	vector, err := embedQuery(ctx, opts.Embedder, opts.Query)
	if err != nil {
		return SearchResult{}, err
	}

	sql := fmt.Sprintf(searchSQL,
		q.placeholder(opts.Query),
		filter,
		q.placeholder(vector),
		strconv.Itoa(RRFK),
		strconv.Itoa(SearchCandidates),
		q.placeholder(int64(SearchLimit(opts.Limit))),
	)
	rows, err := s.db.Query(ctx, sql, q.args...)
	if err != nil {
		return SearchResult{}, fmt.Errorf("searching documents: %w", err)
	}
	defer rows.Close()

	result := SearchResult{Hits: []Hit{}}
	for rows.Next() {
		var (
			d      docScan
			hit    Hit
			total  int
			beyond bool
		)
		if err := rows.Scan(append(d.dests(), &hit.Score, &hit.TextRank, &hit.VectorRank, &total, &beyond)...); err != nil {
			return SearchResult{}, fmt.Errorf("searching documents: %w", err)
		}
		if hit.Stored, err = d.done(); err != nil {
			return SearchResult{}, fmt.Errorf("searching documents: %w", err)
		}
		result.Hits = append(result.Hits, hit)
		result.Truncated = beyond || total > SearchLimit(opts.Limit)
	}
	if err := rows.Err(); err != nil {
		return SearchResult{}, fmt.Errorf("searching documents: %w", err)
	}
	return result, nil
}

func searchWindow(since, until string) (*time.Time, *time.Time, error) {
	parse := func(name, value string) (*time.Time, error) {
		if value == "" {
			return nil, nil
		}
		t, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return nil, fmt.Errorf("%w: %s is not an RFC 3339 timestamp", ErrInvalidSearch, name)
		}
		return &t, nil
	}
	from, err := parse("since", since)
	if err != nil {
		return nil, nil, err
	}
	to, err := parse("until", until)
	if err != nil {
		return nil, nil, err
	}
	if from != nil && to != nil && from.After(*to) {
		return nil, nil, fmt.Errorf("%w: since is later than until", ErrInvalidSearch)
	}
	return from, to, nil
}

// SearchLimit is how many documents a search with this limit actually returns:
// [DefaultSearchLimit] when none was asked for, [MaxSearchLimit] when too many
// were.
func SearchLimit(limit int) int {
	switch {
	case limit <= 0:
		return DefaultSearchLimit
	case limit > MaxSearchLimit:
		return MaxSearchLimit
	default:
		return limit
	}
}

func embedQuery(ctx context.Context, e Embedder, q string) (*string, error) {
	if e == nil {
		return nil, nil
	}
	if got := e.Dimensions(); got != EmbeddingDimensions {
		return nil, fmt.Errorf("%w: the embed tier produces %d values and the column holds %d",
			ErrEmbeddingWidth, got, EmbeddingDimensions)
	}
	vectors, err := e.Embed(ctx, []string{EmbedText(q)})
	if err != nil {
		return nil, fmt.Errorf("embedding a %d byte query: %w", len(q), err)
	}
	if len(vectors) != 1 {
		return nil, fmt.Errorf("embedding the query: the embed tier answered with %d vectors for 1 text", len(vectors))
	}
	if len(vectors[0]) != EmbeddingDimensions {
		return nil, fmt.Errorf("%w: the query embedded to %d values and the column holds %d",
			ErrEmbeddingWidth, len(vectors[0]), EmbeddingDimensions)
	}
	literal := vectorLiteral(vectors[0])
	return &literal, nil
}

func (r Reader) predicate(q *query, scope string) (string, bool) {
	scopes := r.Effective.Grant.Scopes
	var scoped string
	switch {
	case r.Effective.Human == "":
		return "", false
	case scope != "" && !scopes.Has(scope):
		return "", false
	case scope != "":
		scoped = "scope @> ARRAY[" + q.placeholder(scope) + "]::text[]"
	case scopes.All:
		scoped = "true"
	case len(scopes.IDs) == 0:
		return "", false
	default:
		scoped = "scope && " + q.placeholder(scopes.IDs) + "::text[]"
	}

	allowed := []string{"acl @> " + q.placeholder(entryJSON(connector.ACLEntry{Kind: connector.ACLPublic})) + "::jsonb"}
	for _, entry := range r.Audience {
		allowed = append(allowed, "acl @> "+q.placeholder(entryJSON(entry))+"::jsonb")
	}
	return scoped + " AND (" + strings.Join(allowed, " OR ") + ")", true
}

func entryJSON(entry connector.ACLEntry) string {
	entry.Label = ""
	body, err := json.Marshal([]connector.ACLEntry{entry})
	if err != nil {
		return "[]"
	}
	return string(body)
}
