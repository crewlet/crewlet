package claudemodel_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm/anthropic/claudemodel"
)

// The default is sent to every model that takes effort, unasked — so a row
// that takes effort but not the default would make every call to it a 400.
func TestEveryModelThatTakesEffortTakesTheDefault(t *testing.T) {
	t.Parallel()
	for _, id := range append(claudemodel.IDs(), "an-unknown-id") {
		row, _ := claudemodel.Lookup(id)
		if len(row.Efforts) > 0 && !row.Takes(claudemodel.DefaultEffort) {
			t.Errorf("%s takes %v but not the default %s", id, row.Efforts, claudemodel.DefaultEffort)
		}
	}
}

func TestResolveReadsTheOverrideOnlyForAnIDTheTableCannot(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		model, claudeModel string
		wantID             string
		wantErr            error
	}{
		{"claude-opus-5-5", "", "claude-opus-5-5", nil},
		{"anthropic.claude-haiku-4-5-20251001-v1:0", "", "claude-haiku-4-5", nil},
		{"gw-alias", "", "", nil}, // Modern
		{"gw-alias", "claude-haiku-4-5", "claude-haiku-4-5", nil},
		{"gw-alias", "claude-opus-4", "claude-opus-4-0", nil}, // an alias row answers as its canonical id
		// EXACT: an override names a row, never a spelling of one.
		{"gw-alias", "claude-haiku-4-5-20251001", "", claudemodel.ErrNotInTable},
		{"gw-alias", "claude-haiku-4", "", claudemodel.ErrNotInTable},
		// Two answers to "which model is this", whether they agree or not.
		{"claude-opus-5-5", "claude-haiku-4-5", "", claudemodel.ErrOverrideNotNeeded},
		{"claude-opus-5-5", "claude-opus-5-5", "", claudemodel.ErrOverrideNotNeeded},
	} {
		got, err := claudemodel.Resolve(tc.model, tc.claudeModel)
		if !errors.Is(err, tc.wantErr) || (err == nil && got.ID != tc.wantID) {
			t.Errorf("Resolve(%q, %q) = %q, %v; want %q, %v",
				tc.model, tc.claudeModel, got.ID, err, tc.wantID, tc.wantErr)
		}
	}
}

func TestThinksIsAlwaysOnAnAdaptiveModelAndOnlyWithABudgetOtherwise(t *testing.T) {
	t.Parallel()
	opus, _ := claudemodel.Lookup("claude-opus-5-5")
	haiku, _ := claudemodel.Lookup("claude-haiku-4-5")
	for _, tc := range []struct {
		profile claudemodel.Profile
		budget  int
		want    bool
	}{
		{opus, 0, true}, {opus, 4096, true}, {haiku, 0, false}, {haiku, 4096, true},
	} {
		if got := tc.profile.Thinks(tc.budget); got != tc.want {
			t.Errorf("%s budget %d: Thinks = %v, want %v", tc.profile.ID, tc.budget, got, tc.want)
		}
	}
}

func TestCheckEffortRefusesWhatTheModelAnswersWithA400(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		model  string
		effort claudemodel.Effort
		want   error
	}{
		{"claude-opus-5-5", "", nil},
		{"claude-opus-5-5", "xhigh", nil},
		{"claude-opus-4-6", "max", nil},
		{"claude-opus-4-6", "xhigh", claudemodel.ErrEffortLevel},
		{"claude-opus-4-5", "max", claudemodel.ErrEffortLevel},
		{"claude-haiku-4-5", "", nil},
		{"claude-haiku-4-5", "low", claudemodel.ErrTakesNoEffort},
		{"claude-sonnet-4-5", "high", claudemodel.ErrTakesNoEffort},
		{"claude-opus-5-5", "extreme", claudemodel.ErrEffortLevel},
		{"an-unknown-id", "xhigh", nil},
	} {
		row, _ := claudemodel.Lookup(tc.model)
		err := row.CheckEffort(tc.model, tc.effort)
		if !errors.Is(err, tc.want) || (tc.want == nil) != (err == nil) {
			t.Errorf("%s effort %q: %v, want %v", tc.model, tc.effort, err, tc.want)
		}
		if err != nil && !strings.Contains(err.Error(), tc.model) && !strings.Contains(err.Error(), string(tc.effort)) {
			t.Errorf("%s effort %q: %q names neither the model nor the level", tc.model, tc.effort, err)
		}
	}
}

func TestCheckBudgetRefusesWhatTheModelAnswersWithA400(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		model  string
		budget int
		want   error
	}{
		{"claude-opus-5-5", 0, nil},
		{"claude-opus-5-5", 4096, claudemodel.ErrTakesNoBudget},
		{"claude-opus-4-6", 4096, claudemodel.ErrTakesNoBudget}, // deprecated there; depth is effort
		{"an-unknown-id", 4096, claudemodel.ErrTakesNoBudget},
		{"claude-haiku-4-5", 0, nil},
		{"claude-haiku-4-5", claudemodel.MinThinkingBudget, nil},
		{"claude-haiku-4-5", claudemodel.MinThinkingBudget - 1, claudemodel.ErrBudgetRange},
		{"claude-haiku-4-5", 63999, nil},
		{"claude-haiku-4-5", 64000, claudemodel.ErrBudgetRange}, // budget_tokens must be below max_tokens
		{"claude-haiku-4-5", -5, claudemodel.ErrBudgetRange},
	} {
		row, _ := claudemodel.Lookup(tc.model)
		err := row.CheckBudget(tc.model, tc.budget)
		if !errors.Is(err, tc.want) || (tc.want == nil) != (err == nil) {
			t.Errorf("%s budget %d: %v, want %v", tc.model, tc.budget, err, tc.want)
		}
	}
	// An unknown id is named as written, and said to be unknown, so the
	// message points at the alias rather than at a row it is not.
	row, _ := claudemodel.Lookup("gw-alias")
	if err := row.CheckBudget("gw-alias", 4096); err == nil ||
		!strings.Contains(err.Error(), `"gw-alias"`) || !strings.Contains(err.Error(), "not in the capability table") {
		t.Errorf("unknown id: %v, want it named as written and called unknown", err)
	}
}
