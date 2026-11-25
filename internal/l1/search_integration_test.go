//go:build integration

package l1_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kpenfound/hearsay/internal/connector"
	"github.com/kpenfound/hearsay/internal/db/dbtest"
	"github.com/kpenfound/hearsay/internal/l1"
	"github.com/kpenfound/hearsay/internal/llm"
	"github.com/kpenfound/hearsay/internal/principal"
)

func testScope(src string) string { return "code:" + src + ":engine" }

func unitVector(axis int) []float32 {
	v := make([]float32, l1.EmbeddingDimensions)
	v[axis] = 1
	return v
}

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

func reader(scope string, audience ...connector.ACLEntry) l1.Reader {
	return l1.Reader{
		Effective: principal.Effective{Human: "kyle", Grant: principal.Grant{Scopes: principal.SomeScopes(scope)}},
		Audience:  audience,
	}
}

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

func TestSearchFusesWhatEachHalfFinds(t *testing.T) {
	pool := dbtest.Pool(t)
	store := l1.New(pool)
	src := newSource(t)
	query := "the flyway cutover"

	lexical := searchDoc(t, src, "acme/api#1", "we agreed the flyway cutover happens on Friday", nil)
	semantic := searchDoc(t, src, "acme/api#2", "moving the numbers across without stopping the shop", nil)
	both := searchDoc(t, src, "acme/api#3", "the flyway cutover, discussed again", nil)
	neither := searchDoc(t, src, "acme/api#4", "lunch on Thursday, someone bring the good coffee", nil)
	partial := searchDoc(t, src, "acme/api#5", "the flyway, on its own", nil)

	for _, doc := range []l1.Document{lexical, semantic, both, neither, partial} {
		put(t, store, doc)
	}
	embed(t, store, semantic, unitVector(0))
	embed(t, store, both, unitVector(0))
	embed(t, store, neither, unitVector(1))

	hits, err := searchHits(store.Search(t.Context(), reader(testScope(src)), l1.SearchOptions{
		Query:    query,
		Scope:    testScope(src),
		Embedder: queryEmbedder(t, query, unitVector(0)),
	}))
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
	if len(hits) == 0 || hits[0].ID != both.ID {
		t.Errorf("the search returned %v, want %s first", ids(hits), both.ID)
	}
	if by[both.ID].Score <= by[lexical.ID].Score || by[both.ID].Score <= by[semantic.ID].Score {
		t.Errorf("scores are %v, want the document both halves found to score highest", scores(hits))
	}
	if _, ok := by[neither.ID]; !ok || hits[len(hits)-1].ID != neither.ID {
		t.Errorf("the search returned %v, want the unrelated embedded document last", ids(hits))
	}
	if _, ok := by[partial.ID]; ok {
		t.Errorf("the search returned %v, want a document holding only some of the query's words left out", ids(hits))
	}

	hits, err = searchHits(store.Search(t.Context(), reader(testScope(src)), l1.SearchOptions{Query: query, Scope: testScope(src)}))
	if err != nil {
		t.Fatalf("Search(no embed tier) = %v, want no error", err)
	}
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

func TestSearchNeverReturnsADocumentTheReaderMayNotRead(t *testing.T) {
	pool := dbtest.Pool(t)
	store := l1.New(pool)
	src := newSource(t)
	query := "the flyway cutover"
	group := connector.ACLEntry{Kind: connector.ACLGroup, Source: src, NativeID: "t1"}

	private := searchDoc(t, src, "acme/api#1", "the flyway cutover, in detail",
		connector.ACL{{Kind: connector.ACLGroup, Source: src, NativeID: "t1", Label: "the api team"}})
	public := searchDoc(t, src, "acme/api#2", "the flyway cutover, mentioned in passing", nil)
	put(t, store, private)
	put(t, store, public)
	embed(t, store, private, unitVector(0))
	embed(t, store, public, unitVector(1))

	opts := l1.SearchOptions{Query: query, Scope: testScope(src), Embedder: queryEmbedder(t, query, unitVector(0))}
	hits, err := searchHits(store.Search(t.Context(), reader(testScope(src)), opts))
	if err != nil {
		t.Fatalf("Search() = %v, want no error", err)
	}
	if got, want := ids(hits), []string{public.ID}; !slices.Equal(got, want) {
		t.Fatalf("Search() = %v, want %v: the private document must not appear at any rank", got, want)
	}
	if h := found(hits)[public.ID]; h.TextRank != 1 || h.VectorRank != 1 {
		t.Errorf("the readable document came back as %+v, want rank 1 in both halves", h)
	}

	group.Label = "what somebody else calls the api team"
	hits, err = searchHits(store.Search(t.Context(), reader(testScope(src), group), opts))
	if err != nil {
		t.Fatalf("Search(a member) = %v, want no error", err)
	}
	if got, want := ids(hits), []string{private.ID, public.ID}; !slices.Equal(got, want) {
		t.Errorf("Search(a member) = %v, want %v", got, want)
	}
}

func TestSearchMatchesTheGrantsTheMappingGives(t *testing.T) {
	pool := dbtest.Pool(t)
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
	who, err := l1.ReaderFor(t.Context(), pool, res, eff)
	if err != nil {
		t.Fatalf("ReaderFor() = %v", err)
	}
	hits, err := searchHits(store.Search(t.Context(), who, l1.SearchOptions{Query: "the flyway cutover", Scope: testScope(src)}))
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

func TestSearchFiltersByScope(t *testing.T) {
	pool := dbtest.Pool(t)
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
			hits, err := searchHits(store.Search(t.Context(), tc.reader, l1.SearchOptions{Query: "flyway cutover", Scope: tc.scope}))
			if err != nil {
				t.Fatalf("Search() = %v, want no error", err)
			}
			if got := ids(hits); !slices.Equal(got, tc.want) {
				t.Errorf("Search() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPutClearsTheEmbeddingWhenTextChanges(t *testing.T) {
	pool := dbtest.Pool(t)
	store := l1.New(pool)
	src := newSource(t)
	doc := searchDoc(t, src, "acme/api#1", "the flyway cutover", nil)
	put(t, store, doc)
	if embedded(t, pool, doc.ID) {
		t.Fatal("a new document arrived with an embedding")
	}
	embed(t, store, doc, unitVector(0))

	put(t, store, doc)
	if !embedded(t, pool, doc.ID) {
		t.Error("re-distilling a document that had not changed threw its embedding away")
	}

	changed := doc
	changed.RawText = "the flyway cutover, and a comment on it"
	put(t, store, changed)
	if !embedded(t, pool, doc.ID) {
		t.Error("a change to raw_text threw the embedding of text away")
	}

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

func TestSetEmbeddingHoldsTheTextItWasMadeFrom(t *testing.T) {
	pool := dbtest.Pool(t)
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
	_, err := store.SetEmbedding(t.Context(), doc.ID, doc.Text, []float32{1, 2, 3})
	if !errors.Is(err, l1.ErrEmbeddingWidth) {
		t.Errorf("SetEmbedding(3 values) = %v, want l1.ErrEmbeddingWidth", err)
	}
	embed(t, store, doc, unitVector(0))
}

func TestEmbedFillsInWhatHasNoVector(t *testing.T) {
	pool := dbtest.Pool(t)
	store := l1.New(pool)
	src := newSource(t)
	doc := searchDoc(t, src, "acme/api#1", "the flyway cutover", nil)
	put(t, store, doc)

	tier := queryEmbedder(t, l1.EmbedText(doc.Text), unitVector(0))
	written, err := store.Embed(t.Context(), tier, doc.ID)
	if err != nil || !written {
		t.Fatalf("Embed() = %v, %v, want a vector written", written, err)
	}
	written, err = store.Embed(t.Context(), tier, doc.ID)
	if err != nil || written {
		t.Fatalf("Embed(again) = %v, %v, want nothing written and no error", written, err)
	}
	written, err = store.Embed(t.Context(), tier, l1.DocID(src, "acme/api#404"))
	if err != nil || written {
		t.Fatalf("Embed(a document that is not there) = %v, %v, want nothing written and no error", written, err)
	}
	_, err = store.Embed(t.Context(), narrowEmbedder{tier}, doc.ID)
	if !errors.Is(err, l1.ErrEmbeddingWidth) {
		t.Errorf("Embed(a tier of the wrong width) = %v, want l1.ErrEmbeddingWidth", err)
	}
}

type narrowEmbedder struct{ l1.Embedder }

func (narrowEmbedder) Dimensions() int { return l1.EmbeddingDimensions - 1 }

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
