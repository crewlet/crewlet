package search

import (
	"slices"
	"testing"
)

// A HELD-OUT ROW IS NEVER TRAINED ON.
//
// The rows a training measures its recall from are kept out of the k-means
// sample: a query document that shaped the centroid of the list it is filed in
// is a query the index was fitted to answer, and the recall it measured would
// describe that fit rather than a search. The sample is otherwise the seeded
// shuffle it always was — the same seed draws the same rows.
func TestAHeldOutRowIsNeverTrainedOn(t *testing.T) {
	t.Parallel()
	skip := []int{0, 3, 7, 8, 42, 99}
	for _, k := range []int{10, 94, 200} {
		sample := sampleRows(100, k, 7, skip)
		if want := min(k, 100-len(skip)); len(sample) != want {
			t.Fatalf("a sample of %d from %d rows, %d held out, drew %d", k, 100,
				len(skip), len(sample))
		}
		for _, row := range sample {
			if slices.Contains(skip, row) {
				t.Fatalf("held-out row %d was drawn into the training sample", row)
			}
		}
		if again := sampleRows(100, k, 7, skip); !slices.Equal(sample, again) {
			t.Fatal("one seed drew two samples")
		}
	}
}
