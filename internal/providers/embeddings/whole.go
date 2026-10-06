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

// # Representing a text longer than one input, whole
//
// Some callers' text must be represented WHOLE: a turn's ask is a coalesced
// burst whose newest message is at the end, or a task description whose
// acceptance criteria close it, and an opening keeps exactly the wrong half
// while a refusal keeps nothing. So such a text is split into pieces that each
// fit one input ([Chunks]), every piece is embedded in one call, and the
// pieces' vectors are pooled into one ([Pool]).
//
// THE POOL IS A LENGTH-WEIGHTED MEAN, L2-normalised, which is the
// representation a reader of one vector expects: each piece's DIRECTION
// counts in proportion to how much of the text it carries, and the result is
// a unit vector like any single embedding — so a cosine against it means what
// a cosine against any other vector means. Each piece's vector is normalised
// first, because a server that returns unnormalised vectors would otherwise
// weight a piece by a magnitude that is a property of the model rather than of
// the text.
//
// What it costs is stated rather than hidden: a mean of a text about several
// things sits between them, so a focused query scores a little lower against a
// long pooled ask than against a short one. That is still a representation of
// the whole text, which an opening is not; and a text within the bound is ONE
// piece, embedded by [Embedder.Embed] itself, so for everything short this is
// exactly the vector it always was.

// Chunks splits text into the pieces [EmbedWhole] embeds: the [Prepare]d text,
// cut between words into consecutive pieces of at most maxBytes bytes each.
//
// A text within maxBytes is ONE piece, itself. A word longer than maxBytes —
// a URL, a hash, a run of a script written without spaces — is cut on rune
// boundaries, so no piece is ever invalid UTF-8 and no character is split
// across two. Every piece is itself prepared (no space at either end), so a
// piece is exactly the bytes an embedder sends for it, and the pieces joined
// with single spaces are the prepared text again wherever a cut fell between
// words.
//
// maxBytes is the embedder's [Limits.InputBytes]; one below [utf8.UTFMax]
// cannot hold every character, and is a caller's mistake this refuses by
// panicking rather than by looping or returning pieces nothing can send.
//
// # Why the long-word cut always ends
//
// Each cut of a long word takes the longest prefix of at most maxBytes that
// ends on a rune boundary, and the loop advances by that prefix — so it ends
// only if the prefix is never empty. Two facts make that so, and both are
// needed: the text is [Prepare]d, which makes it VALID UTF-8 (a stray byte
// becomes a whole U+FFFD, as the request's JSON encoding makes it), and
// maxBytes is at least [utf8.UTFMax]. In valid UTF-8 a rune boundary lies at
// most three bytes before any cut, so a cut at maxBytes ≥ 4 keeps at least one
// whole character. Over invalid bytes there is no such boundary — a run of
// continuation bytes has none at all — and the prefix was empty on every pass,
// appending empty pieces without end.
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
	sum := make([]float64, width)
	for i, vector := range vectors {
		if len(vector) != width {
			return nil, fmt.Errorf("%w: piece %d's vector is %d wide, not the "+
				"configured %d", ErrConfiguration, i, len(vector), width)
		}
		if weights[i] <= 0 {
			return nil, fmt.Errorf("embeddings: piece %d has weight %d — every "+
				"piece carries some of the text", i, weights[i])
		}
		norm := magnitude(vector)
		if norm == 0 || math.IsNaN(norm) || math.IsInf(norm, 0) {
			return nil, fmt.Errorf("embeddings: piece %d's vector has no "+
				"direction (magnitude %v)", i, norm)
		}
		scale := float64(weights[i]) / norm
		for j, v := range vector {
			sum[j] += float64(v) * scale
		}
	}
	var squares float64
	for _, v := range sum {
		squares += v * v
	}
	norm := math.Sqrt(squares)
	if norm == 0 || math.IsNaN(norm) || math.IsInf(norm, 0) {
		return nil, errors.New("embeddings: the pieces' vectors cancel — their " +
			"weighted mean has no direction")
	}
	out := make([]float32, width)
	for j, v := range sum {
		out[j] = float32(v / norm)
	}
	return out, nil
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
// embedder's own bound, every piece in ONE [BatchEmbedder.EmbedBatch] call —
// packed into requests by the model's limits — and the pieces' vectors
// [Pool]ed.
//
// A text within the bound is one piece, and that piece goes through
// [Embedder.Embed] itself — so its vector is Embed's by construction rather
// than by a server answering an array of one exactly as it answers a string,
// and it is held to Embed's own timeout rather than a batch request's.
//
// A caller bound by a latency budget bounds the context: a long text is one
// batch call, and a batch request's ceiling ([BatchTimeout]) is set for a
// corpus filling, not for a turn starting.
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
