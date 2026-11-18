package embeddings

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/textcut"
)

// A text that must be represented WHOLE (a turn's ask whose newest message is
// at the end, a task whose acceptance criteria close it; an opening keeps the
// wrong half) is split into pieces that each fit one input ([Chunks]), embedded
// in one call, and pooled into one vector ([Pool]).
//
// THE POOL IS A LENGTH-WEIGHTED MEAN, L2-normalised: each piece's DIRECTION
// counts in proportion to the text it carries, and the unit result means what a
// cosine against any single embedding means. Each piece is normalised first, as
// a server's unnormalised magnitude is a property of the model, not the text.
// The cost: a mean of a text about several things sits between them, so a
// focused query scores a little lower against a long ask. A text within the
// bound is ONE piece, embedded by [Embedder.Embed] itself, unchanged.

// Chunks splits text into the pieces [EmbedWhole] embeds: the [Prepare]d text,
// cut between words into consecutive pieces of at most maxBytes bytes each.
//
// A text within maxBytes is ONE piece. A word longer than maxBytes (a URL, a
// hash, unspaced script) is cut on rune boundaries, so no piece is invalid
// UTF-8. Every piece is prepared, so it is exactly the bytes sent for it.
//
// maxBytes below [utf8.UTFMax] panics, a caller's mistake. The long-word cut
// ends only because the prefix is never empty: [Prepare] makes the text VALID
// UTF-8 (a rune boundary lies within three bytes of any cut) and maxBytes >= 4
// keeps one whole character. Over invalid bytes a run of continuation bytes
// has no boundary and the loop appended empty pieces without end.
func Chunks(text string, maxBytes int) []string {
	if maxBytes < utf8.UTFMax {
		panic(fmt.Sprintf("embeddings: Chunks: a %d-byte piece cannot hold every "+
			"character (%d bytes)", maxBytes, utf8.UTFMax))
	}
	prepared := Prepare(text)
	if prepared == "" {
		return nil
	}
	if len(prepared) <= maxBytes {
		return []string{prepared}
	}
	var (
		chunks []string
		piece  strings.Builder
	)
	flush := func() {
		if piece.Len() > 0 {
			chunks = append(chunks, piece.String())
			piece.Reset()
		}
	}
	for word := range strings.SplitSeq(prepared, " ") {
		// The word joins the piece if it fits, with the one space that
		// separated them in the text.
		if piece.Len() > 0 && piece.Len()+1+len(word) <= maxBytes {
			piece.WriteByte(' ')
			piece.WriteString(word)
			continue
		}
		flush()
		// A WORD LONGER THAN A PIECE is cut on rune boundaries; every
		// cut but the last is a piece of its own, and the last starts the
		// next piece, so the words after it can join it. Every head is
		// at least one character, because the text is valid UTF-8 and
		// maxBytes ≥ utf8.UTFMax — see the doc above for why the loop
		// rests on both.
		for len(word) > maxBytes {
			head := textcut.Bytes(word, maxBytes)
			chunks = append(chunks, head)
			word = word[len(head):]
		}
		piece.WriteString(word)
	}
	flush()
	return chunks
}

// Pool is the vectors of a text's pieces as one: the mean of the pieces'
// unit vectors weighted by each piece's length in bytes, normalised to unit
// length.
//
// Every vector must be width wide and every weight positive; a vector that is
// not finite, or is all zeros, has no direction to contribute and is refused,
// as is a set whose weighted directions cancel to nothing.
func Pool(vectors [][]float32, weights []int, width int) ([]float32, error) {
	if len(vectors) == 0 {
		return nil, errors.New("embeddings: no vectors to pool")
	}
	if len(weights) != len(vectors) {
		return nil, fmt.Errorf("embeddings: %d weights for %d vectors", len(weights), len(vectors))
	}
	sum := newPooling(width)
	for i, vector := range vectors {
		if err := sum.add(vector, weights[i]); err != nil {
			return nil, err
		}
	}
	return sum.vector()
}

// pooling is a [Pool] kept open: the pieces' weighted unit vectors summed one
// piece at a time, in order, so a text whose pieces are sent across several
// calls — a [Pass] sends a long one a request at a time, and across passes — is
// pooled to the bit as one sent in one call is. [Pool] is this, run over every
// piece at once, which is what holds the two to one arithmetic rather than to
// two copies of it that agree.
type pooling struct {
	width int
	sum   []float64

	// pieces is how many pieces are summed.
	pieces int
}

// newPooling is a sum of nothing yet, width wide.
func newPooling(width int) *pooling {
	return &pooling{width: width, sum: make([]float64, width)}
}

// add sums the next piece's vector, weighted by its length in bytes. A vector
// of the wrong width, a weight that is not positive and a vector with no
// direction are refused before anything is summed, naming the piece.
func (p *pooling) add(vector []float32, weight int) error {
	at := p.pieces
	if len(vector) != p.width {
		return fmt.Errorf("%w: piece %d's vector is %d wide, not the "+
			"configured %d", ErrConfiguration, at, len(vector), p.width)
	}
	if weight <= 0 {
		return fmt.Errorf("embeddings: piece %d has weight %d — every "+
			"piece carries some of the text", at, weight)
	}
	norm, err := directionOf(vector)
	if err != nil {
		return fmt.Errorf("embeddings: piece %d's vector %w", at, err)
	}
	scale := float64(weight) / norm
	for j, v := range vector {
		p.sum[j] += float64(v) * scale
	}
	p.pieces++
	return nil
}

// vector is the pooled vector of every piece summed: their weighted mean,
// normalised to unit length.
func (p *pooling) vector() ([]float32, error) {
	if p.pieces == 0 {
		return nil, errors.New("embeddings: no vectors to pool")
	}
	var squares float64
	for _, v := range p.sum {
		squares += v * v
	}
	norm := math.Sqrt(squares)
	if norm == 0 || math.IsNaN(norm) || math.IsInf(norm, 0) {
		return nil, errors.New("embeddings: the pieces' vectors cancel — their " +
			"weighted mean has no direction")
	}
	out := make([]float32, p.width)
	for j, v := range p.sum {
		out[j] = float32(v / norm)
	}
	return out, nil
}

// clone is a copy that shares nothing with p, so a sum handed between a pass
// and the memory it keeps progress in is never written by two holders.
func (p *pooling) clone() *pooling {
	return &pooling{width: p.width, sum: append([]float64(nil), p.sum...), pieces: p.pieces}
}

// errNoDirection is a vector with no direction: all zeros, or a component that
// is not finite. It is the one rule a pool weighs a piece by and a [Pass]
// judges a single input's answer by, so the two can never disagree about what
// an answer must carry.
var errNoDirection = errors.New("has no direction")

// directionOf is a vector's magnitude, or [errNoDirection] for one with none —
// which no pool can weigh and no cosine can compare.
func directionOf(vector []float32) (float64, error) {
	norm := magnitude(vector)
	if norm == 0 || math.IsNaN(norm) || math.IsInf(norm, 0) {
		return 0, fmt.Errorf("%w (magnitude %v)", errNoDirection, norm)
	}
	return norm, nil
}

// magnitude is a vector's Euclidean length, in float64 so a wide vector of
// small components does not lose it.
func magnitude(vector []float32) float64 {
	var squares float64
	for _, v := range vector {
		squares += float64(v) * float64(v)
	}
	return math.Sqrt(squares)
}

// EmbedWhole is the vector of the whole of text, however long: [Chunks] at the
// embedder's own bound, every piece in ONE [BatchEmbedder.EmbedBatch] call
// (packed by the model's limits), the vectors [Pool]ed.
//
// A text within the bound goes through [Embedder.Embed] itself, so its vector
// is Embed's by construction, not by a server answering an array of one as it
// answers a string, and under Embed's timeout rather than a batch's.
//
// A caller bound by a latency budget bounds the context: a batch request's
// ceiling ([BatchTimeout]) is set for a corpus filling, not a turn starting.
func EmbedWhole(ctx context.Context, e BatchEmbedder, text string) ([]float32, error) {
	chunks := Chunks(text, e.Limits().InputBytes)
	switch len(chunks) {
	case 0:
		return nil, ErrEmpty
	case 1:
		return e.Embed(ctx, chunks[0])
	}
	vectors, err := e.EmbedBatch(ctx, chunks)
	if err != nil {
		return nil, err
	}
	weights := make([]int, len(chunks))
	for i, chunk := range chunks {
		weights[i] = len(chunk)
	}
	return Pool(vectors, weights, e.Width())
}
