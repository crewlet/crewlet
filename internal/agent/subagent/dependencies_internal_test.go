package subagent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/compact"
)

type answerFitter struct {
	err    error
	budget []int
}

func (f *answerFitter) Fit(_ context.Context, kind compact.Kind, text string, budget int) (compact.Result, error) {
	f.budget = append(f.budget, budget)
	if f.err != nil {
		return compact.Result{From: len(text)}, f.err
	}
	return compact.Result{Text: `{"summary":"condensed"}`, Compacted: true, From: len(text)}, nil
}

// AN ANSWER IS CARRIED WHOLE while the answers together fit: each one is
// bounded where it was made, so a task waiting on two workers is never handed
// a rewrite — and never a fragment.
func TestDependencyAnswersAreWholeWithinTheBudget(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("finding ", 2000) // 16 KB, past the old per-answer cut
	deps := []Result{
		{ID: "a", Status: StatusOK, Text: big},
		{ID: "b", Status: StatusOK, Output: map[string]any{"files": []any{"x.go"}}},
	}
	fit := &answerFitter{}
	got := withDependencies(context.Background(), fit, "do the thing", deps)
	if !strings.Contains(got, big) || !strings.Contains(got, `{"files":["x.go"]}`) {
		t.Fatal("an answer within the budget was not carried whole")
	}
	if len(fit.budget) != 0 {
		t.Fatalf("answers within the budget were rewritten %d time(s)", len(fit.budget))
	}
}

// PAST THE BUDGET, AN ANSWER OVER ITS SHARE IS REWRITTEN — marked as one —
// and one under its share is still whole.
func TestAFanInPastTheBudgetIsCondensedNotCut(t *testing.T) {
	t.Parallel()
	huge := strings.Repeat("row of results\n", 9000) // ~135 KB
	deps := []Result{
		{ID: "wide", Status: StatusOK, Text: huge},
		{ID: "small", Status: StatusOK, Text: "three files changed"},
	}
	fit := &answerFitter{}
	got := withDependencies(context.Background(), fit, "merge them", deps)
	if strings.Contains(got, huge[:1000]+"…") || strings.Contains(got, huge) {
		t.Fatal("the over-share answer was carried whole or cut, not condensed")
	}
	if !strings.Contains(got, "condensed by a model") || !strings.Contains(got, `{"summary":"condensed"}`) {
		t.Fatalf("the rewrite is missing or unmarked:\n%s", got[:400])
	}
	if !strings.Contains(got, "three files changed") {
		t.Fatal("an answer under its share was touched")
	}
	if len(fit.budget) != 1 || fit.budget[0] != dependencyBudget/2 {
		t.Fatalf("budgets asked = %v, want one share of %d", fit.budget, dependencyBudget/2)
	}
}

// NO REWRITE IS NOT A CUT: the dependent task needs its input, so an answer
// that could not be condensed is carried whole.
func TestAnAnswerThatCannotBeCondensedIsCarriedWhole(t *testing.T) {
	t.Parallel()
	huge := strings.Repeat("row of results\n", 9000)
	got := withDependencies(context.Background(), &answerFitter{err: compact.ErrUnavailable},
		"merge", []Result{{ID: "wide", Status: StatusOK, Text: huge}})
	if !strings.Contains(got, huge) {
		t.Fatal("an answer that could not be condensed was not carried whole")
	}
}

// A WORKER'S ERROR REACHES ITS PARENT WHOLE: the cause of a wrapped error is
// at its end, which is what a five-hundred-rune cut used to remove.
func TestAWorkersErrorIsNotCut(t *testing.T) {
	t.Parallel()
	cause := errors.New(strings.Repeat("wrapping context: ", 60) + "the real cause: model key not found")
	for _, err := range []error{cause, &toolloop.BudgetError{Scope: ScopeSubagent}} {
		_, msg := classify("", "", err)
		if msg != err.Error() {
			t.Errorf("classify changed the message: %q", msg)
		}
	}
}
