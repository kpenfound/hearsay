//go:build integration

package l1_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/principal"
)

// The tests here share one database with every other package's, so each takes a
// scope id nothing else uses and searches inside it. That is not a workaround:
// a search is filtered by scope and by the caller's grants, and a test that
// asserted on the whole table would be asserting on somebody else's rows.
func testScope(src string) string { return "code:" + src + ":engine" }

// unitVector is a vector pointing along one axis. Two of them are as far apart
// as cosine distance goes, and one is exactly itself, so a test can say which
// document the similarity half must rank first without depending on a model.
func unitVector(axis int) []float32 {
	v := make([]float32, l1.EmbeddingDimensions)
	v[axis] = 1
	return v
}

// searchDoc is a document in this test's scope, with the words and the access
// list the case is about.
func searchDoc(t *testing.T, src, artifact, rawText string, acl connector.ACL) l1.Document {
	t.Helper()
	return storedDoc(t, src, artifact, func(d *l1.Document) {
		d.Scope = []string{testScope(src)}
		d.RawText = rawText
		d.Text = "a distillation of " + artifact
		if acl != nil {
			d.ACL = acl
		}
	})
}

func put(t *testing.T, store *l1.Store, doc l1.Document) {
	t.Helper()
	if _, err := store.Put(t.Context(), doc); err != nil {
		t.Fatalf("Put(%s) = %v, want no error", doc.ID, err)
	}
}

func embed(t *testing.T, store *l1.Store, doc l1.Document, vector []float32) {
	t.Helper()
	written, err := store.SetEmbedding(t.Context(), doc.ID, doc.Text, vector)
	if err != nil || !written {
		t.Fatalf("SetEmbedding(%s) = %v, %v, want it written", doc.ID, written, err)
	}
}

// reader is a caller who may read one scope and satisfies these access-list
// entries.
func reader(scope string, audience ...connector.ACLEntry) l1.Reader {
	return l1.Reader{
		Effective: principal.Effective{Human: "kyle", Grant: principal.Grant{Scopes: principal.SomeScopes(scope)}},
		Audience:  audience,
	}
}

// found is the hits by document id, so a case can say what it wants without
// depending on the order of the ones it does not care about.
func found(hits []l1.Hit) map[string]l1.Hit {
	out := make(map[string]l1.Hit, len(hits))
	for _, h := range hits {
		out[h.ID] = h
	}
	return out
}

func ids(hits []l1.Hit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.ID
	}
	return out
}

// The acceptance criterion: a document only the words find and a document only
// the vectors find both come back from one query, and a document found by both
// halves beats either.
func TestSearchFusesWhatEachHalfFinds(t *testing.T) {
	pool := newPool(t)
	store := l1.New(pool)
	src := newSource(t)
	query := "the flyway cutover"

	// Only full text finds this one: it holds the query's words and has no
	// embedding at all, which is every document in a deployment with no embed
	// tier configured.
	lexical := searchDoc(t, src, "acme/api#1", "we agreed the flyway cutover happens on Friday", nil)
	// Only the vectors find this one: not a word of the query in it, and an
	// embedding the query embeds to exactly.
	semantic := searchDoc(t, src, "acme/api#2", "moving the numbers across without stopping the shop", nil)
	// Both halves find this one.
	both := searchDoc(t, src, "acme/api#3", "the flyway cutover, discussed again", nil)
	// Neither half should surface this: no words in common, and an embedding
	// as far from the query as one gets.
	neither := searchDoc(t, src, "acme/api#4", "lunch on Thursday, someone bring the good coffee", nil)
	// Nor this one: a query is every word of it, the way a search box reads
	// it, and this holds one of the two.
	partial := searchDoc(t, src, "acme/api#5", "the flyway, on its own", nil)

	for _, doc := range []l1.Document{lexical, semantic, both, neither, partial} {
		put(t, store, doc)
	}
	embed(t, store, semantic, unitVector(0))
	embed(t, store, both, unitVector(0))
	embed(t, store, neither, unitVector(1))

	hits, err := store.Search(t.Context(), reader(testScope(src)), l1.SearchOptions{
		Query:    query,
		Scope:    testScope(src),
		Embedder: queryEmbedder(t, query, unitVector(0)),
	})
	if err != nil {
		t.Fatalf("Search() = %v, want no error", err)
	}

	by := found(hits)
	if got, ok := by[lexical.ID]; !ok || got.TextRank == 0 || got.VectorRank != 0 {
		t.Errorf("the document only full text finds came back as %+v, want a text rank and no vector rank", got)
	}
	if got, ok := by[semantic.ID]; !ok || got.VectorRank == 0 || got.TextRank != 0 {
		t.Errorf("the document only the vectors find came back as %+v, want a vector rank and no text rank", got)
	}
	if got := by[both.ID]; got.TextRank == 0 || got.VectorRank == 0 {
		t.Errorf("the document both halves find came back as %+v, want a rank from each", got)
	}
	// Fusion, not a preference: two halves agreeing beats either one alone.
	if len(hits) == 0 || hits[0].ID != both.ID {
		t.Errorf("the search returned %v, want %s first", ids(hits), both.ID)
	}
	if by[both.ID].Score <= by[lexical.ID].Score || by[both.ID].Score <= by[semantic.ID].Score {
		t.Errorf("scores are %v, want the document both halves found to score highest", scores(hits))
	}
	// The vector half ranks everything it can see, so a document with an
	// embedding and nothing in common with the query is last rather than
	// absent — and the one with neither is absent.
	if _, ok := by[neither.ID]; !ok || hits[len(hits)-1].ID != neither.ID {
		t.Errorf("the search returned %v, want the unrelated embedded document last", ids(hits))
	}
	if _, ok := by[partial.ID]; ok {
		t.Errorf("the search returned %v, want a document holding only some of the query's words left out", ids(hits))
	}

	// With no embed tier the search is its full-text half alone, which is what
	// a deployment that has configured none gets.
	hits, err = store.Search(t.Context(), reader(testScope(src)), l1.SearchOptions{Query: query, Scope: testScope(src)})
	if err != nil {
		t.Fatalf("Search(no embed tier) = %v, want no error", err)
	}
	// Which of the two full text puts first is ts_rank_cd's business; that
	// neither of the documents only the vectors found is here is this test's.
	got, want := ids(hits), []string{both.ID, lexical.ID}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("Search(no embed tier) = %v, want %v", got, want)
	}
	for _, hit := range hits {
		if hit.VectorRank != 0 {
			t.Errorf("%s came back with vector rank %d and there was no embed tier", hit.ID, hit.VectorRank)
		}
	}
}

func scores(hits []l1.Hit) map[string]float64 {
	out := make(map[string]float64, len(hits))
	for _, h := range hits {
		out[h.ID] = h.Score
	}
	return out
}

// The other acceptance criterion: a document the caller may not read does not
// appear at any rank — not last, not at all — even when it is the best match
// both halves have.
func TestSearchNeverReturnsADocumentTheReaderMayNotRead(t *testing.T) {
	pool := newPool(t)
	store := l1.New(pool)
	src := newSource(t)
	query := "the flyway cutover"
	group := connector.ACLEntry{Kind: connector.ACLGroup, Source: src, NativeID: "t1"}

	// The best match there is, and it is private.
	private := searchDoc(t, src, "acme/api#1", "the flyway cutover, in detail",
		connector.ACL{{Kind: connector.ACLGroup, Source: src, NativeID: "t1", Label: "the api team"}})
	// A worse match, readable by anybody.
	public := searchDoc(t, src, "acme/api#2", "the flyway cutover, mentioned in passing", nil)
	put(t, store, private)
	put(t, store, public)
	embed(t, store, private, unitVector(0))
	embed(t, store, public, unitVector(1))

	opts := l1.SearchOptions{Query: query, Scope: testScope(src), Embedder: queryEmbedder(t, query, unitVector(0))}
	hits, err := store.Search(t.Context(), reader(testScope(src)), opts)
	if err != nil {
		t.Fatalf("Search() = %v, want no error", err)
	}
	if got, want := ids(hits), []string{public.ID}; !slices.Equal(got, want) {
		t.Fatalf("Search() = %v, want %v: the private document must not appear at any rank", got, want)
	}
	// It is not that it ranked below: it took no rank at all, so the document
	// the caller may read is the first thing either half found.
	if h := found(hits)[public.ID]; h.TextRank != 1 || h.VectorRank != 1 {
		t.Errorf("the readable document came back as %+v, want rank 1 in both halves", h)
	}

	// The positive control: the same query, by somebody who is in that group —
	// and the source's own label for it is decoration, so a caller holding the
	// same grant under another name holds the same grant.
	group.Label = "what somebody else calls the api team"
	hits, err = store.Search(t.Context(), reader(testScope(src), group), opts)
	if err != nil {
		t.Fatalf("Search(a member) = %v, want no error", err)
	}
	if got, want := ids(hits), []string{private.ID, public.ID}; !slices.Equal(got, want) {
		t.Errorf("Search(a member) = %v, want %v", got, want)
	}
}

// The grants a reader holds come out of the identity mapping, and they have to
// match what a source actually writes into an access list: a group by the id
// the source has for it, a person by theirs.
func TestSearchMatchesTheGrantsTheMappingGives(t *testing.T) {
	pool := newPool(t)
	store := l1.New(pool)
	src := newSource(t)
	res, err := principal.NewResolver([]principal.Principal{{
		ID:         "kyle",
		Kind:       principal.KindHuman,
		Identities: []principal.Identity{{Source: src, NativeID: "u1", Handle: "kpenfound"}},
	}, {
		ID:         "api-team",
		Kind:       principal.KindTeam,
		Members:    []string{"kyle"},
		Identities: []principal.Identity{{Source: src, NativeID: "t1", Handle: "acme/api-team"}},
	}, {
		ID:         "robin",
		Kind:       principal.KindHuman,
		Identities: []principal.Identity{{Source: src, NativeID: "u2", Handle: "robinok"}},
	}})
	if err != nil {
		t.Fatalf("NewResolver() = %v", err)
	}

	docs := map[string]l1.Document{
		"by the group's id":     searchDoc(t, src, "acme/api#1", "the flyway cutover, one", connector.ACL{{Kind: connector.ACLGroup, Source: src, NativeID: "t1"}}),
		"by the group's name":   searchDoc(t, src, "acme/api#2", "the flyway cutover, two", connector.ACL{{Kind: connector.ACLGroup, Source: src, NativeID: "acme/api-team"}}),
		"by their own id":       searchDoc(t, src, "acme/api#3", "the flyway cutover, three", connector.ACL{{Kind: connector.ACLIdentity, Source: src, NativeID: "u1"}}),
		"somebody else's":       searchDoc(t, src, "acme/api#4", "the flyway cutover, four", connector.ACL{{Kind: connector.ACLIdentity, Source: src, NativeID: "u2"}}),
		"the same id elsewhere": searchDoc(t, src, "acme/api#5", "the flyway cutover, five", connector.ACL{{Kind: connector.ACLGroup, Source: "another-source", NativeID: "t1"}}),
		"a group nobody named":  searchDoc(t, src, "acme/api#6", "the flyway cutover, six", connector.ACL{{Kind: connector.ACLGroup, Source: src, NativeID: "t9"}}),
	}
	for _, doc := range docs {
		put(t, store, doc)
	}

	eff, err := principal.HumanRead(mustPrincipal(t, res, "kyle"), principal.Grant{Scopes: principal.SomeScopes(testScope(src))})
	if err != nil {
		t.Fatalf("HumanRead() = %v", err)
	}
	who, err := l1.ReaderFor(res, eff)
	if err != nil {
		t.Fatalf("ReaderFor() = %v", err)
	}
	hits, err := store.Search(t.Context(), who, l1.SearchOptions{Query: "the flyway cutover", Scope: testScope(src)})
	if err != nil {
		t.Fatalf("Search() = %v, want no error", err)
	}
	got := found(hits)
	for name, doc := range docs {
		_, ok := got[doc.ID]
		want := name == "by the group's id" || name == "by the group's name" || name == "by their own id"
		if ok != want {
			t.Errorf("a document readable %s: found %v, want %v", name, ok, want)
		}
	}
}

func mustPrincipal(t *testing.T, res *principal.Resolver, id string) principal.Principal {
	t.Helper()
	p, ok := res.Principal(id)
	if !ok {
		t.Fatalf("the mapping has no %q", id)
	}
	return p
}

// Scopes filter for relevance and never grant permission, and they filter both
// ways: the scope asked for, and the scopes the caller was granted.
func TestSearchFiltersByScope(t *testing.T) {
	pool := newPool(t)
	store := l1.New(pool)
	src := newSource(t)
	here := searchDoc(t, src, "acme/api#1", "the flyway cutover, here", nil)
	elsewhere := storedDoc(t, src, "acme/api#2", func(d *l1.Document) {
		d.Scope = []string{"code:" + src + ":other"}
		d.RawText = "the flyway cutover, elsewhere"
	})
	put(t, store, here)
	put(t, store, elsewhere)

	for _, tc := range []struct {
		name   string
		reader l1.Reader
		scope  string
		want   []string
	}{{
		name:   "one scope",
		reader: reader(testScope(src)),
		scope:  testScope(src),
		want:   []string{here.ID},
	}, {
		name:   "every scope the reader holds",
		reader: reader(testScope(src)),
		want:   []string{here.ID},
	}, {
		name: "a reader granted both, asking for one",
		reader: l1.Reader{Effective: principal.Effective{Human: "kyle", Grant: principal.Grant{
			Scopes: principal.SomeScopes(testScope(src), "code:"+src+":other"),
		}}},
		scope: "code:" + src + ":other",
		want:  []string{elsewhere.ID},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			hits, err := store.Search(t.Context(), tc.reader, l1.SearchOptions{Query: "flyway cutover", Scope: tc.scope})
			if err != nil {
				t.Fatalf("Search() = %v, want no error", err)
			}
			if got := ids(hits); !slices.Equal(got, tc.want) {
				t.Errorf("Search() = %v, want %v", got, tc.want)
			}
		})
	}
}

// A vector is a function of the text it was made from, so text that changes
// takes its embedding with it — and text that does not keeps it, which is what
// stops a re-distillation costing an embedding call.
func TestPutClearsTheEmbeddingWhenTextChanges(t *testing.T) {
	pool := newPool(t)
	store := l1.New(pool)
	src := newSource(t)
	doc := searchDoc(t, src, "acme/api#1", "the flyway cutover", nil)
	put(t, store, doc)
	if embedded(t, pool, doc.ID) {
		t.Fatal("a new document arrived with an embedding")
	}
	embed(t, store, doc, unitVector(0))

	// Writing the same document again does not touch it.
	put(t, store, doc)
	if !embedded(t, pool, doc.ID) {
		t.Error("re-distilling a document that had not changed threw its embedding away")
	}

	// Nor does a change to something other than the text.
	changed := doc
	changed.RawText = "the flyway cutover, and a comment on it"
	put(t, store, changed)
	if !embedded(t, pool, doc.ID) {
		t.Error("a change to raw_text threw the embedding of text away")
	}

	// The text changing does.
	changed.Text = "a different distillation"
	put(t, store, changed)
	if embedded(t, pool, doc.ID) {
		t.Error("the text changed and the embedding of the old text is still there")
	}
}

func embedded(t *testing.T, pool *pgxpool.Pool, id string) bool {
	t.Helper()
	var has bool
	if err := pool.QueryRow(t.Context(), `SELECT embedding IS NOT NULL FROM l1_docs WHERE id = $1`, id).Scan(&has); err != nil {
		t.Fatalf("reading the embedding of %s: %v", id, err)
	}
	return has
}

// Storing a vector is a compare-and-set on the text it was made from: two
// writers need no order between them for the row to end up holding the vector
// of the text it holds.
func TestSetEmbeddingHoldsTheTextItWasMadeFrom(t *testing.T) {
	pool := newPool(t)
	store := l1.New(pool)
	src := newSource(t)
	doc := searchDoc(t, src, "acme/api#1", "the flyway cutover", nil)
	put(t, store, doc)

	if written, err := store.SetEmbedding(t.Context(), doc.ID, "some other distillation", unitVector(0)); written || err != nil {
		t.Errorf("SetEmbedding(text that is not there) = %v, %v, want it refused", written, err)
	}
	if embedded(t, pool, doc.ID) {
		t.Error("a vector made from text the document does not hold was stored anyway")
	}
	if written, err := store.SetEmbedding(t.Context(), l1.DocID(src, "acme/api#404"), doc.Text, unitVector(0)); written || err != nil {
		t.Errorf("SetEmbedding(a document that is not there) = %v, %v, want it refused", written, err)
	}
	// A vector the column could not hold is refused before it is sent.
	_, err := store.SetEmbedding(t.Context(), doc.ID, doc.Text, []float32{1, 2, 3})
	if !errors.Is(err, l1.ErrEmbeddingWidth) {
		t.Errorf("SetEmbedding(3 values) = %v, want l1.ErrEmbeddingWidth", err)
	}
	embed(t, store, doc, unitVector(0))
}

// Embed asks the table what needs a vector rather than assuming, so a
// re-distillation that changed nothing makes no model call, and a document
// whose embedding failed last time gets one the next time round.
func TestEmbedFillsInWhatHasNoVector(t *testing.T) {
	pool := newPool(t)
	store := l1.New(pool)
	src := newSource(t)
	doc := searchDoc(t, src, "acme/api#1", "the flyway cutover", nil)
	put(t, store, doc)

	tier := queryEmbedder(t, l1.EmbedText(doc.Text), unitVector(0))
	written, err := store.Embed(t.Context(), tier, doc.ID)
	if err != nil || !written {
		t.Fatalf("Embed() = %v, %v, want a vector written", written, err)
	}
	// The second call finds nothing to do — and asks the tier for nothing,
	// which the fixtures prove: there is no recording for a second call.
	written, err = store.Embed(t.Context(), tier, doc.ID)
	if err != nil || written {
		t.Fatalf("Embed(again) = %v, %v, want nothing written and no error", written, err)
	}
	// A document that is not there is not an error either: it was retracted
	// while the job was in flight.
	written, err = store.Embed(t.Context(), tier, l1.DocID(src, "acme/api#404"))
	if err != nil || written {
		t.Fatalf("Embed(a document that is not there) = %v, %v, want nothing written and no error", written, err)
	}
	// A tier of the wrong width is refused before the provider is asked.
	_, err = store.Embed(t.Context(), narrowEmbedder{tier}, doc.ID)
	if !errors.Is(err, l1.ErrEmbeddingWidth) {
		t.Errorf("Embed(a tier of the wrong width) = %v, want l1.ErrEmbeddingWidth", err)
	}
}

// narrowEmbedder is a tier that says it produces one value fewer than the
// column holds.
type narrowEmbedder struct{ l1.Embedder }

func (narrowEmbedder) Dimensions() int { return l1.EmbeddingDimensions - 1 }

// queryEmbedder is the embed tier, answering from a recorded fixture: the real
// registry with the network replaced (internal/llm), so a test here exercises
// the same request the process will send and no test calls a provider.
func queryEmbedder(t *testing.T, text string, vector []float32) l1.Embedder {
	t.Helper()
	fixtures := llm.NewFixtures()
	if err := fixtures.AddEmbedding(llm.EmbeddingFixture{
		Texts:   []string{text},
		Vectors: [][]float32{vector},
	}); err != nil {
		t.Fatalf("recording the embedding: %v", err)
	}
	registry, err := llm.NewFake(llm.Config{Tiers: map[llm.Tier]llm.TierConfig{
		llm.TierEmbed: {Provider: "acme", Model: "embed-1", Dimensions: l1.EmbeddingDimensions},
	}}, fixtures)
	if err != nil {
		t.Fatalf("building the fake registry: %v", err)
	}
	embedder, err := registry.Embedder()
	if err != nil {
		t.Fatalf("Embedder() = %v", err)
	}
	return embedder
}
