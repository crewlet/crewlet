package engine

import (
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// THE ROOM AN EXECUTOR LAUNCH NEEDS, read in one place.
//
// A launch is held to a floor (ADR-0066): below it the seat's windows cannot
// carry the run's first request, so the launch is refused as a budget refusal
// and waits — its delivery parked, or its resume held — for the window to
// turn over ([executorHeadroom]). A launch that may CONTINUE a session sends
// that session's whole context before it reads a word of its brief, so its
// floor is the larger of the static floor and the session's context with the
// brief that continues it beside it, never the static floor alone — the
// session the turn kept, or the job before's own copy in its box, whichever
// the launch may take up, each held to its own floor and dropped alone where
// that one cannot be carried ([agentLauncher.continuedFloor]). The static floor
// (turn_engine.executor_min_budget_tokens) already counts the harness's own
// prompt, its tools and the seat's system prompt, which the context's last
// request counted too, so the two are compared, never added. A session no
// capture measured has no known context, and its launch is held to the static
// floor.
//
// AND THE FLOOR A SEAT IS HELD TO IS THE FLOOR ITS LAUNCH WAS REFUSED BELOW.
// The launch's refusal carries it ([floorRefusal]) to the hold that waits on it
// ([Engine.heldByBudget], [Engine.budgetHold]), which asks the counters for a
// window that leaves less than it ([meter.starved]) and says so in those words
// ([starvedReason]). Read anywhere as the static floor, a resume refused below
// its own found no window starved, was not held, and was retried into the same
// refusal. A relaunch's floor is HELD on its resumed segment, which has no mail
// to park; a turn's FIRST launch, which continues the session its
// conversation's last turn left where it can, is refused on its dispatch, and
// the dispatcher PARKS the seat's mail on the floor that launch was refused
// below ([Engine.budgetPark]) — parked on the static floor, it found the window
// not starved and NAKed the delivery into the same refusal until the broker
// dead-lettered it.
//
// AND A FLOOR NO WINDOW CAN EVER LEAVE ROOM FOR IS NO FLOOR TO WAIT ON: a
// session whose context with its brief is past the smallest ceiling a seat's or
// its company's capped windows hold is not continued ([launchFloor.fits]): its
// launch continues the other session it may take up where that one's own floor
// fits, and otherwise starts a session of its own on the static floor, which
// config already holds every ceiling above (validateExecutorFloor). Waited for,
// it would hold the turn for ever. Decided by the launch, where the brief that
// continues the session exists — never by the size of the session alone: a
// seat with no capped window carries any.
//
// AND NEITHER IS A SESSION THE LAUNCH'S MODEL CANNOT CARRY: a Claude Code run's
// window is its model's own ([sandbox.ClaudeCodeWindow]), and a session kept under
// a larger one, continued on a member whose model has a smaller window, is past
// what one request of it can send ([launchFloor.carriedBy]).

// launchFloor is the room one executor launch needs.
type launchFloor struct {
	// static is turn_engine.executor_min_budget_tokens: what a launch that
	// continues no session needs.
	static int

	// context is the whole input of the last request of the session the
	// launch continues, cache included, and brief the estimate of the brief
	// it is handed; both zero for a launch that continues none.
	context int64
	brief   int
}

// staticFloor is the floor of a launch that continues no session under c.
func staticFloor(c *Company) launchFloor {
	if c == nil || c.Config == nil {
		return launchFloor{}
	}
	return launchFloor{static: c.Config.TurnEngine.ExecutorFloor()}
}

// continuing is f for a launch that continues a session whose last request
// carried context tokens, handed a brief of about brief tokens.
func (f launchFloor) continuing(context int64, brief int) launchFloor {
	f.context, f.brief = max(context, 0), max(brief, 0)
	return f
}

// tokens is the floor, in tokens.
func (f launchFloor) tokens() int {
	if f.context <= 0 {
		return f.static
	}
	return max(f.static, int(f.context)+f.brief)
}

// atLeast is the larger of f and g: the floor of a launch that may take either
// path, which it learns only once it has its box.
func (f launchFloor) atLeast(g launchFloor) launchFloor {
	if g.tokens() > f.tokens() {
		return g
	}
	return f
}

// fits is why no window under b can ever leave room for f, "" where one can: a
// floor past the smallest ceiling of every capped window of the seat's and the
// company's is one no window turning over ever meets.
func (f launchFloor) fits(b budgetBasis) string {
	smallest, capped := b.smallestCeiling()
	if !capped || f.tokens() <= smallest {
		return ""
	}
	return fmt.Sprintf("a launch that continues the session needs %d tokens (its last request carried %d, "+
		"and its brief about %d), past %d, the smallest ceiling of the seat's capped windows",
		f.tokens(), f.context, f.brief, smallest)
}

// carriedBy is why one request under w cannot carry the session f continues, ""
// where it can or f continues none.
func (f launchFloor) carriedBy(w sandbox.ExecutorWindow) string {
	if f.context <= 0 || w.Carries(f.context, f.brief) {
		return ""
	}
	return fmt.Sprintf("a launch that continues the session sends %d tokens first (its last request "+
		"carried %d, and its brief about %d), more than one request under the %d-token window of the "+
		"model the launch runs can carry", f.context+int64(f.brief), f.context, f.brief, w.Tokens)
}

// smallestCeiling is the smallest ceiling of any capped window of either scope,
// false where the basis caps none.
func (b budgetBasis) smallestCeiling() (int, bool) {
	smallest, capped := 0, false
	for _, caps := range []coord.Caps{b.org, b.seat} {
		for _, ceiling := range caps {
			if ceiling > 0 && (!capped || ceiling < smallest) {
				smallest, capped = ceiling, true
			}
		}
	}
	return smallest, capped
}

// floorRefusal is a launch refused below its floor: a budget refusal
// ([toolloop.ErrBudgetExhausted]) that carries the floor and what the seat had
// left, so the hold that waits on it waits for room for THIS launch.
type floorRefusal struct {
	floor launchFloor
	left  int
}

func (r *floorRefusal) Error() string {
	if r.floor.context > 0 {
		return fmt.Sprintf("this seat has %d tokens left and an executor run that continues its "+
			"session needs at least %d: its session's last request carried %d tokens and its brief "+
			"is about %d (turn_engine.executor_min_budget_tokens is %d): %v", r.left, r.floor.tokens(),
			r.floor.context, r.floor.brief, r.floor.static, toolloop.ErrBudgetExhausted)
	}
	return fmt.Sprintf("this seat has %d tokens left and an executor run needs at least %d "+
		"(turn_engine.executor_min_budget_tokens): %v", r.left, r.floor.tokens(), toolloop.ErrBudgetExhausted)
}

func (r *floorRefusal) Unwrap() error { return toolloop.ErrBudgetExhausted }

// floorOf is the floor err was refused below, the static floor of c where it
// names none.
func floorOf(c *Company, err error) launchFloor {
	var refused *floorRefusal
	if errors.As(err, &refused) {
		return refused.floor
	}
	return staticFloor(c)
}
