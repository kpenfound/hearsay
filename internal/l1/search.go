package l1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

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
	//
	// Rank fusion rather than a weighted sum of the two scores, because the
	// scores are not comparable — ts_rank_cd is a lexical density and a cosine
	// distance is an angle, and neither is calibrated against the other or
	// stable across queries. Ranks are. 60 is the constant the method was
	// published with: large enough that the top few ranks do not swamp
	// everything below them, small enough that rank 1 still beats rank 20.
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
	// no `embed` tier configured gets (ADR-0005, docs/config.md).
	Embedder Embedder
	// Limit is how many documents to return, defaulting to
	// [DefaultSearchLimit] and capped at [MaxSearchLimit].
	Limit int
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

// searchSQL is the hybrid read: the two halves, each ranked over the documents
// the caller may see, fused by reciprocal rank.
//
// The filter is inside both halves rather than around the result. A document
// the caller may not read never enters a ranking, so it can neither be returned
// nor push a document they may read out of the candidates
// (docs/design.md#access-control) — filtering afterwards would do the second
// even when it stops the first. The two halves repeat the predicate for the
// same reason a view would not do: each has to rank what is left after it, and
// the planner is free to use whichever index the predicate offers.
//
// The six substitutions, in order: the query's placeholder, the filter (used
// twice), the query vector's placeholder (twice), the fusion constant (twice),
// the candidate cutoff (twice) and the limit.
const searchSQL = `
WITH matched AS (
    SELECT id, row_number() OVER (
        ORDER BY ts_rank_cd(to_tsvector('english', raw_text), tsq.q) DESC, id
    ) AS rank
    FROM l1_docs, websearch_to_tsquery('english', %[1]s) AS tsq(q)
    WHERE %[2]s AND to_tsvector('english', raw_text) @@ tsq.q
),
near AS (
    SELECT id, row_number() OVER (ORDER BY embedding <=> %[3]s::text::vector, id) AS rank
    FROM l1_docs
    WHERE %[2]s AND %[3]s::text IS NOT NULL AND embedding IS NOT NULL
),
fused AS (
    SELECT coalesce(m.id, n.id) AS doc_id,
           (coalesce(1.0 / (%[4]s + m.rank), 0) + coalesce(1.0 / (%[4]s + n.rank), 0))::float8 AS score,
           coalesce(m.rank, 0) AS text_rank,
           coalesce(n.rank, 0) AS vector_rank
    FROM (SELECT id, rank FROM matched WHERE rank <= %[5]s) m
    FULL JOIN (SELECT id, rank FROM near WHERE rank <= %[5]s) n ON m.id = n.id
)
SELECT ` + docColumns + `, fused.score, fused.text_rank, fused.vector_rank
FROM l1_docs JOIN fused ON l1_docs.id = fused.doc_id
ORDER BY fused.score DESC, last_activity_at DESC, l1_docs.id
LIMIT %[6]s`

// Search is hybrid retrieval over the document table: similarity over the
// embedding of `text` and full text over `raw_text`, each ranked over the
// documents this reader may see, fused by reciprocal rank
// (docs/design.md#read-and-assert-api). It returns documents, not answers.
//
// It is the one place a query becomes a ranking, so it is also the one place
// that embeds a query: [SearchOptions.Embedder] is called once, with the query,
// through the same door a document goes through ([EmbedText]). That is the only
// model call any read here makes, and the search runs without it.
//
// A reader who may see nothing gets nothing — no query, no error. Fail-closed
// is the whole of what this filter is for: a caller with no scopes, no audience
// or no principal reads no documents rather than all of them.
func (s *Store) Search(ctx context.Context, reader Reader, opts SearchOptions) ([]Hit, error) {
	if strings.TrimSpace(opts.Query) == "" {
		return nil, fmt.Errorf("%w: there is nothing to search for", ErrInvalidSearch)
	}
	q := &query{}
	filter, ok := reader.predicate(q, opts.Scope)
	if !ok {
		return []Hit{}, nil
	}

	// The query is embedded before the statement is built, so that a tier that
	// fails takes the search with it rather than quietly halving it: a caller
	// that asked for both halves and silently got one would not be able to
	// tell.
	vector, err := embedQuery(ctx, opts.Embedder, opts.Query)
	if err != nil {
		return nil, err
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
		return nil, fmt.Errorf("searching documents: %w", err)
	}
	defer rows.Close()

	hits := []Hit{}
	for rows.Next() {
		var (
			d   docScan
			hit Hit
		)
		if err := rows.Scan(append(d.dests(), &hit.Score, &hit.TextRank, &hit.VectorRank)...); err != nil {
			return nil, fmt.Errorf("searching documents: %w", err)
		}
		if hit.Stored, err = d.done(); err != nil {
			return nil, fmt.Errorf("searching documents: %w", err)
		}
		hits = append(hits, hit)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("searching documents: %w", err)
	}
	return hits, nil
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

// embedQuery is the query as a vector, and a nil vector where there is no embed
// tier to make one — which is what turns the similarity half off.
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
		// The query is a person's words: it is named as a length and never
		// quoted, the same rule the write path holds to (ADR-0008).
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

// predicate is what this reader may see, as SQL: the scope filter and the
// access-list filter, both of which have to hold. The second return is false
// when the reader can see nothing at all, which is a query not worth running.
//
// Both filters fail closed. A reader with no scopes reads nothing rather than
// everything, and a document is readable only when its access list says so —
// there is no branch here that skips the access-list check.
func (r Reader) predicate(q *query, scope string) (string, bool) {
	scopes := r.Effective.Grant.Scopes
	var scoped string
	switch {
	case r.Effective.Human == "":
		// Nothing is accountable for this read (docs/design.md#access-control).
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

	// Containment rather than equality, so an entry the source labelled
	// matches the same grant written without a label: two grants are one when
	// the kind, the source and the native id agree (internal/l1).
	allowed := []string{"acl @> " + q.placeholder(entryJSON(connector.ACLEntry{Kind: connector.ACLPublic})) + "::jsonb"}
	for _, entry := range r.Audience {
		allowed = append(allowed, "acl @> "+q.placeholder(entryJSON(entry))+"::jsonb")
	}
	return scoped + " AND (" + strings.Join(allowed, " OR ") + ")", true
}

// entryJSON is one access-list entry as the one-element array the containment
// operator takes. The entry is written without its label: the label is what a
// source calls the grant, and a document is not less readable for having been
// labelled differently.
func entryJSON(entry connector.ACLEntry) string {
	entry.Label = ""
	body, err := json.Marshal([]connector.ACLEntry{entry})
	if err != nil {
		// connector.ACLEntry is four strings; there is nothing here that can
		// fail to encode, and returning an array that matches nothing is the
		// fail-closed answer if that ever stops being true.
		return "[]"
	}
	return string(body)
}
