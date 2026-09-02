package llm_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// THE SET IS CLOSED, and empty means auto.
//
// Every backend maps these onto its vendor's own spelling, and as a bare
// string that mapping was one typo away from silence: `"require"` fell through
// both switches to a warning line and NO tool_choice on the wire, which reads
// to a caller as the model having chosen not to call a tool. Empty stays valid
// because a request that does not care must not have to name a choice.
func TestTheToolChoiceSetIsClosedAndEmptyMeansAuto(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		choice llm.ToolChoice
		valid  bool
	}{
		{"", true},
		{llm.ToolChoiceAuto, true},
		{llm.ToolChoiceRequired, true},
		{llm.ToolChoiceNone, true},
		{"require", false},  // The typo that used to be silent.
		{"REQUIRED", false}, // Not case-folded: no backend's mapping is.
		{"any", false},      // Anthropic's own spelling of required, not the contract's.
	} {
		if got := tc.choice.Valid(); got != tc.valid {
			t.Errorf("ToolChoice(%q).Valid() = %v, want %v", tc.choice, got, tc.valid)
		}
	}
}
