package search

import (
	"encoding/binary"
	"math"
	"strings"
	"testing"
)

func packed(v ...float32) []byte {
	out := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(out[4*i:], math.Float32bits(f))
	}
	return out
}

// A DOCUMENT IS TOLD APART FROM ITSELF BY ITS KEY, and from its twin by its key
// too.
//
// The two samples are taken by one rule, so a query is usually also in the
// sample it is compared with: a self-pair at cosine one is the document, not
// the corpus. But two DIFFERENT documents whose vectors are identical — a page
// and its copy — are a genuine pair at cosine one, and a test on a distance of
// zero drops both kinds together.
func TestTheMeanCosineSkipsTheSelfPairAndKeepsTheTwin(t *testing.T) {
	t.Parallel()
	docs := []sampled{
		{key: "page:a", vector: packed(1, 0)},
		{key: "page:b", vector: packed(1, 0)}, // a's twin
		{key: "task:c", vector: packed(0, 1)},
	}
	got, err := meanPairCosine(docs, docs, 2)
	if err != nil {
		t.Fatal(err)
	}
	// Six ordered pairs of distinct documents: a–b and b–a at one, the
	// four involving c at zero.
	if want := 2.0 / 6.0; math.Abs(got-want) > 1e-12 {
		t.Fatalf("mean cosine = %.6f, want %.6f — %.6f means the self-pairs "+
			"were counted, %.6f means the twins were dropped with them",
			got, want, 5.0/9.0, 0.0)
	}
}

// A VECTOR WITH NO LENGTH IS IN NO PAIR. Its cosine against anything is a
// division by zero, and one NaN is the whole mean.
func TestTheMeanCosineSkipsAVectorWithNoDirection(t *testing.T) {
	t.Parallel()
	docs := []sampled{
		{key: "page:a", vector: packed(1, 0)},
		{key: "page:b", vector: packed(1, 1)},
		{key: "page:zero", vector: packed(0, 0)},
	}
	got, err := meanPairCosine(docs, docs, 2)
	if err != nil {
		t.Fatal(err)
	}
	if want := 1 / math.Sqrt2; math.IsNaN(got) || math.Abs(got-want) > 1e-12 {
		t.Fatalf("mean cosine = %v, want %v over the two vectors that have a "+
			"direction", got, want)
	}
}

// A VECTOR OF THE WRONG WIDTH IS REFUSED BY NAME, rather than read past its end
// or as a shorter vector than the space says it is.
func TestTheMeanCosineRefusesAVectorOfTheWrongWidth(t *testing.T) {
	t.Parallel()
	docs := []sampled{
		{key: "page:a", vector: packed(1, 0)},
		{key: "page:short", vector: packed(1)},
	}
	_, err := meanPairCosine(docs, docs, 2)
	if err == nil || !strings.Contains(err.Error(), "page:short") {
		t.Fatalf("a 4-byte vector in a 2-dimension space answered %v", err)
	}
}
