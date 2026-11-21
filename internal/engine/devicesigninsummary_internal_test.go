package engine

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
)

// THE SUMMARY A DEVICE SIGN-IN'S RE-ACTIVATION WRITES FITS THE ACTIVATION
// POINTER AT ITS WORST: a provider key of any length, every byte escaping to
// six. Past coord.MaxSummaryBytes the re-activation would be refused, and the
// login would reach no other node.
//
// Mutation: drop the key's cut and the summary is past the bound.
func TestADeviceSignInsSummaryFitsThePointer(t *testing.T) {
	t.Parallel()
	if err := coord.HoldSummary(deviceSignInSummary(strings.Repeat("\x01", 10_000))); err != nil {
		t.Errorf("a device sign-in's summary over a long key: %v", err)
	}
}
