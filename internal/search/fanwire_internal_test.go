package search

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/queue"
)

// A SLICE WEIGHS ITS QUERY'S TERMS: each distinct term at the postings its
// scan may read and hold, beside the reply's bound, so a repeated word costs
// nothing more; and the longest query a slice takes is still one the node's
// answer budget admits.
//
// Mutations, each red: weigh every slice alike; count a repeated term twice;
// drop the reply's bound.
func TestASliceWeighsItsQuerysTerms(t *testing.T) {
	t.Parallel()
	perTerm := int64(maxPostingScan * postingHeldBytes)
	for _, tc := range []struct {
		text  string
		terms int64
	}{
		{"", 0},
		{"deploy", 1},
		{"deploy rollback deploy", 2},
	} {
		if got, want := sliceWeight(tc.text), tc.terms*perTerm+sliceReplyBytes; got != want {
			t.Errorf("a slice for %q weighs %d, want %d terms at %d and the reply's %d", tc.text, got, tc.terms,
				perTerm, sliceReplyBytes)
		}
	}
	var longest strings.Builder
	for i := 0; longest.Len()+3 <= knowledge.MaxQueryBytes; i++ {
		longest.WriteString(string(rune('a'+i/26%26)) + string(rune('a'+i%26)) + " ")
	}
	if got := sliceWeight(longest.String()); got > queue.AnswerBudgetBytes {
		t.Errorf("the longest query a slice takes weighs %d, past the node's answer budget of %d", got,
			queue.AnswerBudgetBytes)
	}
}
