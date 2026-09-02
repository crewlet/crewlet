package embeddings

import (
	"context"
	"hash/fnv"
	"math"
	"strings"
)

// Fake is a deterministic embedder for tests and for a company that wants
// similarity without an embedding bill.
//
// # It is a real similarity function, not a stub
//
// A stub returning a constant makes every memory equally similar to every
// task, which is the one answer that makes a recall test meaningless: the
// floor admits everything and the ranking is arbitrary. This hashes TOKENS
// into a bag-of-words vector, so text that shares words scores high and text
// that shares none scores near zero — enough for a test to assert that the
// right memory came back and the wrong one did not.
//
// It is emphatically NOT a semantic embedder: "car" and "automobile" score
// zero here. That is the honest limit, and it is why this is not offered as
// a configured provider type — a company running on it would find its recall
// silently worse in exactly the cases recall exists for.
type Fake struct{ width int }

var _ Embedder = (*Fake)(nil)

// NewFake builds a fake at the given width. Zero takes a small one, because
// a test asserting a store round trip cares about the width matching, not
// about what it is.
func NewFake(width int) *Fake {
	if width <= 0 {
		width = 64
	}
	return &Fake{width: width}
}

// Width implements [Embedder].
func (f *Fake) Width() int { return f.width }

// Embed implements [Embedder].
//
// NORMALISED to unit length, because cosine similarity is what reads these
// and an unnormalised bag-of-words makes a long text similar to everything
// by having a bigger magnitude than anything.
func (f *Fake) Embed(_ context.Context, text string) ([]float32, error) {
	normalized := normalize(text)
	if normalized == "" {
		return nil, ErrEmpty
	}
	vector := make([]float32, f.width)
	for word := range strings.FieldsSeq(strings.ToLower(normalized)) {
		h := fnv.New32a()
		h.Write([]byte(word))
		// THE MODULO IS UNSIGNED. int(h.Sum32()) is non-negative on a
		// 64-bit int and can be negative on a 32-bit one, where the
		// index would panic — a platform difference in a helper whose
		// whole job is being boring.
		vector[h.Sum32()%uint32(f.width)] += 1
	}
	// Normalising needs no zero guard: normalized is non-empty, so
	// Fields yields at least one word, so at least one bucket holds 1
	// and the sum is at least 1.
	var sum float64
	for _, v := range vector {
		sum += float64(v) * float64(v)
	}
	norm := float32(math.Sqrt(sum))
	for i := range vector {
		vector[i] /= norm
	}
	return vector, nil
}
