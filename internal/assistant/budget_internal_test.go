package assistant

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/caveat"
)

// A SPENT BUDGET NAMES THE CEILING TO RAISE, on both ways a person meets it: a
// question refused before its first round ([BudgetRefused], answered by the
// route as its `remedy`) and a turn its next round was refused for (the error
// frame's `remedy`). The two read one mapping, so the drawer opens the same
// ceiling whichever way the budget ran out. The brief's own cap is a constant
// of the turn that nobody can raise, and names none.
//
// Mutation: drop the remedy from the turn's error frame, map the person's scope
// to the company's ceiling, or put the reset's timestamp back in the hint, and a
// case fails.
func TestASpentBudgetNamesTheCeilingToRaise(t *testing.T) {
	t.Parallel()
	resets := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		scope string
		want  caveat.Remedy
		code  wire.ErrorCode
	}{
		{"org", caveat.CompanyTokenBudget(), wire.ErrorBudgetExhausted},
		{PersonBudgetScope, caveat.AssistantTokenBudget(), wire.ErrorBudgetExhausted},
		{BriefBudgetScope, caveat.Remedy{}, wire.ErrorBriefInvalid},
	} {
		t.Run(c.scope, func(t *testing.T) {
			t.Parallel()
			refused := &BudgetRefused{Scope: c.scope, ResetsAt: resets}
			if got := refused.Remedy(); got != c.want {
				t.Errorf("a question refused by %q names %+v, want %+v", c.scope, got, c.want)
			}
			err := fmt.Errorf("round: %w", &toolloop.BudgetError{Scope: c.scope, Used: 10,
				Limit: 10, ResetsAt: resets})
			now := resets.Add(-2 * time.Hour)
			stopped := &turn{svc: &Service{now: func() time.Time { return now }}}
			_, failure := stopped.outcome(t.Context(), nil, err)
			if failure == nil || failure.Code != c.code {
				t.Fatalf("a turn stopped by %q ended %+v, want %s", c.scope, failure, c.code)
			}
			// THE RESET IS A WAIT, never a timestamp in a person's sentence.
			if c.code == wire.ErrorBudgetExhausted && (failure.RetryAfterSeconds != 7200 ||
				strings.Contains(failure.Hint, "T00:00:00Z")) {
				t.Errorf("a turn stopped by %q waits %ds with the hint %q, want 7200s and "+
					"words", c.scope, failure.RetryAfterSeconds, failure.Hint)
			}
			var got caveat.Remedy
			if failure.Remedy != nil {
				got = *failure.Remedy
			}
			if got != c.want {
				t.Errorf("a turn stopped by %q names %+v, want %+v", c.scope, got, c.want)
			}
		})
	}
}
