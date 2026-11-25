package harness_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
)

// THE KEPT SUMMARY'S BOUND IS THE LARGEST COMPACTION OUTPUT A RUNNER'S HARNESS
// SETS, AS THE ENGINE ESTIMATES OUTPUT: executorkeep.SummaryMaxBytes is
// OpenCode 2.3.2's 256,000-token ceiling at EstimateTokens' own ratio, so a
// change of the ratio re-derives the bound rather than leaving it stale.
//
// Mutation: estimate output at three bytes a token, or set the bound to 64 KiB,
// and the bound is no longer 256,000 tokens of output.
func TestTheSummaryBoundIsTheLargestCompactionCapAsOutputIsEstimated(t *testing.T) {
	t.Parallel()
	if got := harness.EstimateTokens(strings.Repeat("a", executorkeep.SummaryMaxBytes)); got != 256_000 {
		t.Fatalf("a summary at its bound of %d bytes estimates at %d tokens of output, want OpenCode's "+
			"256,000-token ceiling on its compaction call", executorkeep.SummaryMaxBytes, got)
	}
}
