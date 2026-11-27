package learning

import "context"

// BudgetGate reports whether the seat named by handle may start auxiliary work
// now: its windows and the company's, or the company's alone for a person (a
// unit's lead). Every optional pass that calls a model asks it first.
//
// Three-valued: an error is "the counter could not be read", which the pass
// treats as NO ROOM ([SkipBudgetUnknown]): each pass is optional work its next
// cycle repeats, while running one against a ceiling nobody could read is
// spend past a limit the company set.
//
// A FALSE IS A REFUSAL, not an observation: the pass asks just before the work
// it gates and makes no call on false, and the engine's gate stamps the
// window's refusal. Never ask it only to look.
type BudgetGate func(ctx context.Context, handle string) (bool, error)

// SkipBudgetUnknown is a pass declining to start because the budget's
// counter could not be read ([BudgetGate]).
const SkipBudgetUnknown = "budget_unknown"

// budgetSkip asks gate for handle before pass's first model call, and is the
// reason it may not start: "", [SkipNoBudget] or [SkipBudgetUnknown]. A nil
// gate (a node that counts nothing) never declines.
func budgetSkip(ctx context.Context, gate BudgetGate, pass, handle string) string {
	if gate == nil {
		return ""
	}
	ok, err := gate(ctx, handle)
	switch {
	case err != nil:
		log.WarnContext(ctx, "learning_pass_budget_unknown", "pass", pass, "agent_handle", handle,
			"error", err, "detail", "the pass does not start, and its next cycle tries again")
		return SkipBudgetUnknown
	case !ok:
		log.InfoContext(ctx, "learning_pass_skipped_no_budget", "pass", pass, "agent_handle", handle,
			"detail", "the company or this seat is at its token ceiling, so no "+
				"auxiliary call is made until the window turns")
		return SkipNoBudget
	}
	return ""
}
