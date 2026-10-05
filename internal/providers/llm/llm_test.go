package llm_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// THE SET IS CLOSED, and empty means no level was named. A backend maps these
// onto two vendors' parameters and takes the lower of two of them, so a value
// outside the set is one it can neither send nor compare.
func TestTheEffortSetIsClosedAndEmptyMeansUnnamed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		effort llm.Effort
		valid  bool
	}{
		{"", true},
		{llm.EffortLow, true},
		{llm.EffortMedium, true},
		{llm.EffortHigh, true},
		{llm.EffortXHigh, true},
		{llm.EffortMax, true},
		{"x-high", false},  // The spelling a person reaches for.
		{"HIGH", false},    // Not case-folded: neither vendor's parameter is.
		{"minimal", false}, // OpenAI-only, and below what the contract orders.
		{"none", false},
	} {
		if got := tc.effort.Valid(); got != tc.valid {
			t.Errorf("Effort(%q).Valid() = %v, want %v", tc.effort, got, tc.valid)
		}
	}
}

// A CEILING ONLY EVER LOWERS. The caller knows what a call is for, the
// operator what the entry costs, and neither may raise the other: a `low`
// classifier on a `high` entry runs low, a `max` request on a `medium` entry
// stays medium, and an entry with no level is left alone, since lowering a
// level nobody named could raise it above the vendor's own default.
func TestAtMostTakesTheLowerAndNeverRaises(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		entry, ceiling, want llm.Effort
	}{
		{llm.EffortHigh, llm.EffortLow, llm.EffortLow},
		{llm.EffortMedium, llm.EffortMax, llm.EffortMedium},
		{llm.EffortXHigh, llm.EffortXHigh, llm.EffortXHigh},
		{llm.EffortMax, llm.EffortXHigh, llm.EffortXHigh},
		{llm.EffortXHigh, llm.EffortHigh, llm.EffortHigh},
		{llm.EffortLow, llm.EffortMedium, llm.EffortLow},
		{llm.EffortHigh, "", llm.EffortHigh},
		{"", llm.EffortLow, ""},
		{"", "", ""},
		{llm.EffortHigh, "bogus", llm.EffortHigh},
	} {
		if got := tc.entry.AtMost(tc.ceiling); got != tc.want {
			t.Errorf("Effort(%q).AtMost(%q) = %q, want %q", tc.entry, tc.ceiling, got, tc.want)
		}
	}
}

// THE STOP VOCABULARY IS CLOSED, and the zero value is not in it: it is a
// backend that reported no reason, which the tool loop reads as an ordinary
// end rather than as one of these.
func TestTheStopReasonSetIsClosedAndEmptyIsUnreported(t *testing.T) {
	for _, r := range []llm.StopReason{
		llm.StopEnd, llm.StopToolUse, llm.StopMaxTokens,
		llm.StopRefusal, llm.StopContextExceeded, llm.StopPaused,
	} {
		if !r.Valid() {
			t.Errorf("%q: Valid() = false, want true", r)
		}
	}
	for _, r := range []llm.StopReason{"", "end_turn", "length", "stop", "tool_calls"} {
		if r.Valid() {
			t.Errorf("%q: Valid() = true — a vendor's own spelling must be mapped, never passed through", r)
		}
	}
}

// A REFUSAL IS NOT HANDED DOWN THE CHAIN AND BENCHES NO KEY. Retryable is
// what the fallback chain reads, and a refusal it read as retryable would be
// routed round every model until one answered — circumventing the decision
// rather than recovering from a fault. ExhaustsCredential is what the key pool
// reads, and a policy decision about content says nothing about the key.
func TestARefusalIsNeitherRetriedNorAKeysFault(t *testing.T) {
	if llm.KindRefusal.Retryable() {
		t.Error("KindRefusal is retryable: the chain would ask the next model")
	}
	if llm.KindRefusal.ExhaustsCredential() {
		t.Error("KindRefusal exhausts a credential: a refusal would bench a healthy key")
	}
	if got := llm.KindRefusal.String(); got != "refusal" {
		t.Errorf("String() = %q, want refusal — it is the error_kind a record carries", got)
	}
	// The controls: every other kind keeps its answer.
	for kind, retryable := range map[llm.ErrorKind]bool{
		llm.KindFatal: false, llm.KindRateLimit: true, llm.KindAuth: true,
		llm.KindTimeout: true, llm.KindServer: true,
	} {
		if kind.Retryable() != retryable {
			t.Errorf("%s: Retryable() = %v, want %v", kind, kind.Retryable(), retryable)
		}
	}
}

// The classified error a backend returns keeps the refusal reachable under it,
// with what the vendor said and what the refused call cost.
func TestRefusedCarriesTheRefusalAndItsCompletion(t *testing.T) {
	comp := &llm.Completion{Model: "m", InputTokens: 7, StopReason: llm.StopRefusal}
	err := error(llm.Refused("anthropic", "m",
		&llm.Refusal{Category: "cyber", Explanation: "not this", Completion: comp}))
	if llm.KindOf(err) != llm.KindRefusal {
		t.Fatalf("KindOf = %s, want refusal", llm.KindOf(err))
	}
	var refusal *llm.Refusal
	if !errors.As(err, &refusal) {
		t.Fatal("the *Refusal is not reachable under the classified error")
	}
	if refusal.Category != "cyber" || refusal.Completion != comp {
		t.Errorf("refusal = %+v", refusal)
	}
	if msg := err.Error(); !strings.Contains(msg, "declined") || !strings.Contains(msg, "cyber") ||
		!strings.Contains(msg, "not this") {
		t.Errorf("Error() = %q, want the decline, its category and its explanation", msg)
	}
}
