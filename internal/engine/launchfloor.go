package engine

import (
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// The room an executor launch needs (ADR-0066; package doc, "Executor launch
// floor"). Below its floor a launch is refused as a budget refusal and waits
// for the window to turn over ([executorHeadroom]). The static floor
// (turn_engine.executor_min_budget_tokens) is its first request's fixed part,
// the brief counted BESIDE it; a launch that may continue a measured
// session is held to the larger of that and the session's context with its
// brief, compared, never added, since the context already counted the harness's
// prompt. Every brief the launch may hand its run holds it to that brief's
// floor, the launch to the largest ([agentLauncher.continuedFloor]), and its
// refusal carries that floor ([floorRefusal]) to the hold or park that waits on
// it: read as the static floor, the wait ([meter.starved]) found no window
// starved.

// launchFloor is the room one executor launch needs.
type launchFloor struct {
	// static is turn_engine.executor_min_budget_tokens: the fixed part of a
	// launch's first request, beside which its brief is counted.
	static int

	// context is the whole input, cache included, of the last request of the
	// session the launch continues, zero where it continues none or nothing
	// measured it; brief is the estimate of the brief it is handed, zero for
	// the static floor alone.
	context int64
	brief   int

	// room is set on a fresh brief's floor alone ([launchFloor.fresh]), the
	// zero room where nothing is capped: past it, the floor is held
	// ([launchFloor.held]), brief still saying the estimate it was held from.
	// A session's floor never carries one: past the room, the session is not
	// continued ([launchFloor.fits]).
	room windowRoom

	// waited is the window an earlier try of the launch's work was refused
	// below its floor in, once it has turned over ([launchFloor.after]), else
	// zero. Set on a fresh brief's floor alone, which is then held as one past
	// the room, so the launch waits one turnover at most.
	waited sandbox.BudgetWindow
}

// windowRoom is the most room the seat's capped windows can leave its launch:
// the least, over every capped window of the seat's and the company's, of its
// ceiling less what the launch's own turn has charged in it
// ([budgetBasis.room]). Never the ceiling alone: the turn has charged its way
// into those windows before its launch reads them.
type windowRoom struct {
	// tokens is that least room, and ceiling and spent are the window's that
	// leaves it: its ceiling, and what the turn charged there.
	tokens, ceiling, spent int

	// smallest is the smallest capped ceiling of either scope, which config
	// holds at or above the static floor (validateExecutorFloor).
	smallest int

	// capped is false where no window of either scope is capped, which leaves
	// a launch room for any floor: none is past it, so none is held.
	capped bool
}

// room is the most room b's capped windows can leave a launch whose turn has
// charged spent in them, by period ([meter.chargedIn]), both scopes alike since
// every charge reaches both. A tie goes to the smaller ceiling, so the line
// names one window however the caps are walked.
func (b budgetBasis) room(spent map[period.Period]int) windowRoom {
	var r windowRoom
	for _, caps := range []coord.Caps{b.org, b.seat} {
		for p, ceiling := range caps {
			if ceiling <= 0 {
				continue
			}
			left := ceiling - spent[p]
			if !r.capped || left < r.tokens || (left == r.tokens && ceiling < r.ceiling) {
				r.tokens, r.ceiling, r.spent = left, ceiling, spent[p]
			}
			if !r.capped || ceiling < r.smallest {
				r.smallest = ceiling
			}
			r.capped = true
		}
	}
	return r
}

// says is r as a line names it. Where the turn charged nothing in the window
// that leaves the least, that is the smallest capped ceiling, since every
// other window leaves at most its own.
func (r windowRoom) says() string {
	if r.spent == 0 {
		return fmt.Sprintf("%d, the smallest ceiling of the seat's or the company's capped windows", r.tokens)
	}
	return fmt.Sprintf("%d, the most room the seat's or the company's capped windows can leave the launch "+
		"(a ceiling of %d, less the %d its own turn spent there before it)", max(r.tokens, 0), r.ceiling,
		r.spent)
}

// staticFloor is the static floor under c: the floor of a launch that names no
// brief, such as a gate asked before any launch is built.
func staticFloor(c *Company) launchFloor {
	if c == nil || c.Config == nil {
		return launchFloor{}
	}
	return launchFloor{static: c.Config.TurnEngine.ExecutorFloor()}
}

// continuing is f for a launch that continues a session whose next request
// carries context tokens before its brief (its harness's estimate of that
// request, or its last request's input where nothing estimated it,
// [continuedContext]; zero where nothing measured it either), handed a brief
// of about brief tokens.
func (f launchFloor) continuing(context int64, brief int) launchFloor {
	f.context, f.brief = max(context, 0), max(brief, 0)
	return f
}

// fresh is f for a launch's fresh brief, of about brief tokens, judged against
// r, the most room the capped windows can leave the launch: past r the floor is
// HELD ([launchFloor.held]), never dropped as a session's is, since the fresh
// brief has nothing to be dropped for and, refused in every window, its launch
// would wait for ever.
func (f launchFloor) fresh(brief int, r windowRoom) launchFloor {
	f.context, f.brief, f.room = 0, max(brief, 0), r
	return f
}

// after is f, a fresh brief's floor, for a launch an earlier try of whose work
// was refused below its floor in window w, since turned over: held as a floor
// past the room is ([launchFloor.held]), whatever its brief. A busy company's
// window may never leave the refused floor at the launch's read, so the next
// try waits no longer than a launch held to the static floor alone (package
// doc, "Executor launch floor").
func (f launchFloor) after(w sandbox.BudgetWindow) launchFloor {
	f.waited = w
	return f
}

// held reports whether f, a floor whose context is not known, is held below
// its brief's floor, at [launchFloor.tokens]: its brief beside the static floor
// is past the room, or its launch waited a turnover ([launchFloor.after]).
// Held too where the room is below the static floor, the turn's own spend
// having taken the difference: refused, it would wait for ever. Never under a
// ceiling below the static floor, which config refuses (validateExecutorFloor),
// nor at a room the turn's spend used up, which refuses its every call already.
func (f launchFloor) held() bool {
	r := f.room
	return f.context <= 0 && f.static > 0 && r.capped && r.smallest >= f.static && r.tokens > 0 &&
		(f.static+f.brief > r.tokens || f.waited.Valid())
}

// heldFor is why a held f is held, in its refusal's, hold's and line's words.
// Never "the floor its launch was refused below": the wait records the window
// alone ([floorWaits], [sandbox.LaunchRecord.RelaunchRefusedIn]), and the floor
// it refused may have been a session's, which the next try drops
// ([launchFloor.stillShort]), so those words would name a floor no window
// refused.
func (f launchFloor) heldFor() string {
	if f.static+f.brief > f.room.tokens {
		return "past " + f.room.says()
	}
	return fmt.Sprintf("a floor held after its launch was refused below its floor in the %s window %s, which "+
		"has turned over since", f.waited.Period, f.waited.Label)
}

// tokens is the floor, in tokens: a known context's with its brief, the static
// floor with its brief beside it where no context is known, and a held floor
// ([launchFloor.held]) at the static floor where that fits in its room and at
// the room otherwise.
func (f launchFloor) tokens() int {
	switch {
	case f.context > 0:
		return max(f.static, int(f.context)+f.brief)
	case f.static <= 0:
		// The floor turned off holds no launch whose context is not known,
		// whatever its brief; a known context, above, still holds its own.
		return 0
	case f.held():
		// Never the room above the static floor: that needs a window
		// holding nothing but the turn's own spend, which a busy company's
		// other seats may never leave.
		return min(f.static, f.room.tokens)
	}
	return f.static + f.brief
}

// heldAt says where a held f is held ([launchFloor.tokens]), in its refusal's,
// hold's and line's (`executor_floor_held`) words.
func (f launchFloor) heldAt() string {
	if f.static <= f.room.tokens {
		return "turn_engine.executor_min_budget_tokens alone"
	}
	return "that room, which only a window holding nothing but its own turn's spend leaves"
}

// atLeast is the larger of f and g: the floor of a launch that may take either
// path, which it learns only once it has its box.
func (f launchFloor) atLeast(g launchFloor) launchFloor {
	if g.tokens() > f.tokens() {
		return g
	}
	return f
}

// fits is why no window can ever leave room for f, "" where one can: no
// turnover meets a floor past r, the launch's own turn spending into each
// window before the launch reads it. A session nothing measured is judged at
// the static floor with its brief. A zero floor fits anywhere, a window its own
// turn filled included.
func (f launchFloor) fits(r windowRoom) string {
	if !r.capped || f.tokens() <= max(r.tokens, 0) {
		return ""
	}
	return fmt.Sprintf("a launch that continues the session needs %d tokens (%s), past %s", f.tokens(),
		f.madeOf(), r.says())
}

// stillShort is why a launch that waited a turnover ([launchFloor.after]) does
// not continue the session whose floor f is, "" where it does: left, what the
// windows leave the launch now, is short of f again. Waited for again, that
// floor could hold the launch in every window of a busy company, so it takes
// the other session where that one fits, else its fresh brief. A session the
// window now has room for is still continued.
func (f launchFloor) stillShort(left int, w sandbox.BudgetWindow) string {
	if f.tokens() <= left {
		return ""
	}
	return fmt.Sprintf("a launch that continues the session needs %d tokens (%s), and its windows leave %d, "+
		"short of it again after the %s window %s refused the launch below its floor: a launch waits one "+
		"turnover at most for the room its briefs need", f.tokens(), f.madeOf(), left, w.Period, w.Label)
}

// madeOf is what a session's floor f is made of, in a line's words: what the
// session sends before its brief ([continuedContext]) and its brief, or, where
// nothing measured that context, the static floor and its brief.
func (f launchFloor) madeOf() string {
	if f.context <= 0 {
		return fmt.Sprintf("nothing measured its context, so turn_engine.executor_min_budget_tokens, %d, "+
			"and its brief, about %d", f.static, f.brief)
	}
	return fmt.Sprintf("its session sends %d before its brief, and its brief about %d", f.context, f.brief)
}

// turnedOver reports whether w, the window a launch was refused below its floor
// in, has turned over by m's instant: the current window of w's period has a
// later label, one period's labels sorting in calendar order. False for a w
// naming no window; for one still current (a park an apply released early
// hands its launch back, held to its floor as before); and for a meter that
// caps nothing, which refuses no launch.
func (m *meter) turnedOver(w sandbox.BudgetWindow) bool {
	if m == nil || !m.basis.capped() || !w.Valid() {
		return false
	}
	for _, now := range m.windows() {
		if now.Period == w.Period {
			return w.Label < now.Label
		}
	}
	return false
}

// carriedBy is why one request under w cannot carry the session f continues, ""
// where it can or f continues none.
func (f launchFloor) carriedBy(w sandbox.ExecutorWindow) string {
	if f.context <= 0 || w.Carries(f.context, f.brief) {
		return ""
	}
	return fmt.Sprintf("a launch that continues the session sends %d tokens first (the session %d, "+
		"and its brief about %d), more than one request under the %d-token window Claude "+
		"Code holds the launch's model to can carry", f.context+int64(f.brief), f.context, f.brief, w.Tokens)
}

// floorRefusal is a launch refused below its floor: a budget refusal
// ([toolloop.ErrBudgetExhausted]) that carries the floor and what the seat had
// left, so the hold that waits on it waits for room for THIS launch.
type floorRefusal struct {
	floor launchFloor
	left  int
}

// Error says what the floor is made of, in its figures: a continued session's
// context with its brief, a brief beside the static floor (with why and where
// it is held, [launchFloor.heldFor] and [launchFloor.heldAt]), or the static
// floor alone.
func (r *floorRefusal) Error() string {
	f := r.floor
	switch {
	case f.context > 0:
		return fmt.Sprintf("this seat has %d tokens left and an executor run that continues its "+
			"session needs at least %d: its session sends %d tokens before its brief, which "+
			"is about %d (turn_engine.executor_min_budget_tokens is %d): %v", r.left, f.tokens(),
			f.context, f.brief, f.static, toolloop.ErrBudgetExhausted)
	case f.held():
		return fmt.Sprintf("this seat has %d tokens left and an executor run needs at least %d: its brief "+
			"is about %d tokens beside turn_engine.executor_min_budget_tokens (%d), %s, so it is held "+
			"at %s: %v", r.left, f.tokens(), f.brief, f.static, f.heldFor(), f.heldAt(),
			toolloop.ErrBudgetExhausted)
	case f.brief > 0:
		return fmt.Sprintf("this seat has %d tokens left and an executor run needs at least %d: its brief "+
			"is about %d tokens beside turn_engine.executor_min_budget_tokens (%d): %v", r.left, f.tokens(),
			f.brief, f.static, toolloop.ErrBudgetExhausted)
	}
	return fmt.Sprintf("this seat has %d tokens left and an executor run needs at least %d "+
		"(turn_engine.executor_min_budget_tokens): %v", r.left, f.tokens(), toolloop.ErrBudgetExhausted)
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
