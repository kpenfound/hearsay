package l1

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// EmbeddingDimensions is the width of the vectors this table holds: the N of
// the `vector(N)` column migration 7 adds.
//
// It is a constant here and a number in a migration, and the two are one
// number. An `embed` tier producing anything else is refused before a vector is
// written — by [Store.Embed] here, and by llm.CheckDimensions at the startup of
// whatever process does the writing — because a model of another width means
// re-embedding every row behind a migration, not a configuration edit
// (ADR-0005, docs/config.md).
const EmbeddingDimensions = 1536

// MaxEmbedBytes bounds how much of a document's text is embedded.
//
// It is the answer to "chunk long documents or not": not. `text` is a
// distillation — the model's own summary of one artifact, bounded by the
// `distill` tier's answer budget — so one vector describes one thing, which is
// what a similarity search is for; splitting it into chunks would put several
// vectors of one document into one ranking and crowd out other documents. The
// bound is here for the document that is somehow longer anyway: it is cut on a
// rune boundary, deterministically, so the same document always embeds to the
// same request.
const MaxEmbedBytes = 32_000

// ErrEmbeddingWidth is returned when a vector is not [EmbeddingDimensions]
// values wide. The column would refuse it, and the point of catching it here is
// that the tier is wrong rather than the row.
var ErrEmbeddingWidth = errors.New("wrong number of values for the embedding column")

// Embedder is the `embed` tier as this package uses it: internal/llm's
// Embedder, restated so that storing a vector does not depend on the provider
// abstraction. Whatever satisfies llm.Embedder satisfies this.
type Embedder interface {
	// Embed returns one vector per input, in order.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	// Dimensions is the width of the vectors this tier produces.
	Dimensions() int
}

// EmbedText is what is actually embedded for a document: its text, cut to
// [MaxEmbedBytes] on a rune boundary. It is exported because search embeds a
// query through the same door, and because a caller recording a fixture has to
// be able to ask for exactly what the store will send.
func EmbedText(text string) string {
	if len(text) <= MaxEmbedBytes {
		return text
	}
	cut := MaxEmbedBytes
	// Half a rune is not a character, and cutting one differently on each run
	// would embed the same document to two different vectors.
	for cut > 0 && text[cut]&0xC0 == 0x80 {
		cut--
	}
	return text[:cut]
}

// Embed gives one document the vector its text asks for, and reports whether it
// wrote one.
//
// It does nothing at all — and makes no model call — for a document that
// already has a vector, so it is safe to call after every distillation: the
// ordinary outcome of re-distilling an artifact nothing has happened to is a
// row that did not change, whose embedding is therefore still the one its text
// asks for. [Store.Put] is the other half of that: it clears the vector
// whenever it writes a different `text`.
//
// A document that has gone, or whose text changed while the tier was
// answering, writes nothing and is not an error: the write that changed it
// cleared the vector, and the next call embeds the words that are actually
// there.
func (s *Store) Embed(ctx context.Context, e Embedder, id string) (bool, error) {
	if e == nil {
		return false, errors.New("embedding a document needs the embed tier")
	}
	if got := e.Dimensions(); got != EmbeddingDimensions {
		return false, fmt.Errorf("%w: the embed tier produces %d values and the column holds %d",
			ErrEmbeddingWidth, got, EmbeddingDimensions)
	}
	var text string
	err := s.db.QueryRow(ctx, `SELECT text FROM l1_docs WHERE id = $1 AND embedding IS NULL`, id).Scan(&text)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("reading the text of %s: %w", id, err)
	}
	vectors, err := e.Embed(ctx, []string{EmbedText(text)})
	if err != nil {
		// The error is a log line and a queue_job.last_error column: it names
		// the document and never a word of what was sent (ADR-0008).
		return false, fmt.Errorf("embedding %s: %w", id, err)
	}
	if len(vectors) != 1 {
		return false, fmt.Errorf("embedding %s: the embed tier answered with %d vectors for 1 text", id, len(vectors))
	}
	return s.SetEmbedding(ctx, id, text, vectors[0])
}

// SetEmbedding stores the vector made from text, and reports whether the
// document still holds that text.
//
// The text is the condition and not just the payload. A document is
// re-distilled while nothing waits for it, so the row this vector was made from
// may already have been rewritten; comparing what is stored now against what
// was embedded is a compare-and-set, and it needs no ordering between the two
// writers to be right, which is what nothing here has (internal/l1).
func (s *Store) SetEmbedding(ctx context.Context, id, text string, vector []float32) (bool, error) {
	if len(vector) != EmbeddingDimensions {
		return false, fmt.Errorf("%w: %s was embedded to %d values and the column holds %d",
			ErrEmbeddingWidth, id, len(vector), EmbeddingDimensions)
	}
	var updated string
	err := s.db.QueryRow(ctx,
		`UPDATE l1_docs SET embedding = $3::text::vector WHERE id = $1 AND text = $2 RETURNING id`,
		id, text, vectorLiteral(vector)).Scan(&updated)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("writing the embedding of %s: %w", id, err)
	}
	return true, nil
}

// vectorLiteral is a vector as pgvector's input function reads it. It goes to
// Postgres as text and is cast there, which is what keeps this package off a
// pgvector driver: the values are float32 and are formatted to the shortest
// decimal that reads back as the same float32, so the round trip is exact.
func vectorLiteral(vector []float32) string {
	out := make([]byte, 0, 2+len(vector)*8)
	out = append(out, '[')
	for i, v := range vector {
		if i > 0 {
			out = append(out, ',')
		}
		out = strconv.AppendFloat(out, float64(v), 'g', -1, 32)
	}
	return string(append(out, ']'))
}
