package secretsapi

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
)

// THE SUMMARY A SECRET WRITE'S RE-ACTIVATION WRITES FITS THE ACTIVATION
// POINTER AT ITS WORST: a batch of MaxRevealNames names of a kilobyte each,
// and one such name alone, every byte escaping to six. Past
// coord.MaxSummaryBytes the re-activation is refused, and the values stay
// sealed and inactive for words the caller never wrote.
//
// Mutation: drop either cut and its summary is past the bound.
func TestTheSummaryASecretWritesReloadWritesFitsThePointer(t *testing.T) {
	t.Parallel()
	names := make([]string, MaxRevealNames)
	for i := range names {
		names[i] = strings.Repeat("\x01", 1<<10)
	}
	for _, read := range [][]string{names, names[:1]} {
		if err := coord.HoldSummary(reloadSummary(read, "written")); err != nil {
			t.Errorf("the summary of a write of %d names: %v", len(read), err)
		}
	}
}
