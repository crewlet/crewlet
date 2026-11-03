package engine

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/period"
)

// A CONTINUED LAUNCH IS HELD ON ITS OWN FLOOR, NEVER THE STATIC ONE. A relaunch
// that continues its turn's session sends the session's context before its
// brief, so its floor is the two together; refused below it, the refusal
// carries that floor to the hold, which waits for a window with room for THIS
// launch and says what it is made of. Held to the static floor instead, a
// window with room for that alone released the resume into the same refusal
// at its launch, round after round.
//
// Mutation: hold on the static floor, or drop the floor from the refusal, and
// nothing holds the resume.
func TestARefusedContinuedLaunchIsHeldOnItsOwnFloor(t *testing.T) {
	t.Parallel()
	r := newFlooredParkRig(t, "100", "50")
	r.spend(t, 40)
	floor := staticFloor(r.e.Company()).continuing(55, 10)
	refused := executorHeadroom(t.Context(), staticRemaining{left: 60}, floor)
	if !errors.Is(refused, toolloop.ErrBudgetExhausted) || !strings.Contains(refused.Error(), "continues its session") {
		t.Fatalf("a continued launch with 60 tokens left under a floor of 65 = %v, want a budget refusal "+
			"saying what its floor is made of", refused)
	}
	held := r.e.heldByBudget(t.Context(), r.e.Company(), "lead", refused)
	if held == nil || held.Refusal.Condition != waitBudget || !held.Refusal.Until.Equal(berlinMidnight) ||
		!strings.Contains(held.Refusal.Reason, "continues its session") {
		t.Fatalf("held = %+v, want the resume held on the day that cannot carry its continued launch", held)
	}
	if refusal, held := r.e.budgetHold(t.Context(), r.e.Company(), "lead", staticFloor(r.e.Company())); held {
		t.Fatalf("the static floor holds %+v, where the window has room for it: the case is not the one "+
			"a continued launch's own floor decides", refusal)
	}
}

// A SESSION NO WINDOW CAN CARRY IS NOT CONTINUED. A floor past the smallest
// ceiling of any capped window of the seat's or the company's is one no window
// turning over ever meets, so its relaunch would wait for ever: such a session
// starts fresh, on the static floor every ceiling is already held above. And
// the floor is the larger of the static floor and the context with the brief,
// never their sum: the static floor counts the harness's own prompt, which the
// context's last request counted too.
//
// Mutation: add the static floor to the context, or compare against the
// largest ceiling, and a row goes red.
func TestASessionNoWindowCanCarryIsNotContinued(t *testing.T) {
	t.Parallel()
	capped := budgetBasis{org: coord.Caps{period.Day: 500}, seat: coord.Caps{period.Day: 120, period.Week: 900}}
	static := launchFloor{static: 50}
	for name, tc := range map[string]struct {
		floor  launchFloor
		basis  budgetBasis
		tokens int
		fits   bool
	}{
		"within the smallest ceiling": {floor: static.continuing(90, 15), basis: capped, tokens: 105, fits: true},
		"past the smallest ceiling":   {floor: static.continuing(110, 15), basis: capped, tokens: 125},
		"under the static floor":      {floor: static.continuing(30, 5), basis: capped, tokens: 50, fits: true},
		"no session":                  {floor: static, basis: capped, tokens: 50, fits: true},
		"nothing capped":              {floor: static.continuing(110, 15), tokens: 125, fits: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := tc.floor.tokens(); got != tc.tokens {
				t.Errorf("the floor is %d tokens, want %d", got, tc.tokens)
			}
			why := tc.floor.fits(tc.basis)
			if (why == "") != tc.fits || (!tc.fits && !strings.Contains(why, "120")) {
				t.Errorf("fits = %q, want fitting %v, else naming the smallest ceiling", why, tc.fits)
			}
		})
	}
}
