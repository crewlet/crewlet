package engine

import (
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// THE ROOM AN EXECUTOR LAUNCH NEEDS, read in one place.
//
// A launch is held to a floor (ADR-0066): below it the seat's windows cannot
// carry the run's first request, so the launch is refused as a budget refusal
// and waits, its delivery parked or its resume held, for the window to turn
// over ([executorHeadroom]). The static floor
// (turn_engine.executor_min_budget_tokens) is that request's fixed part: the
// harness's own prompt, its tools and the seat's system prompt. Its brief is
// BESIDE it, never inside it, so a launch whose context is not known (a fresh
// run, or a session nothing measured: no capture of it, nor, for the job
// before's own copy in its box, that job's record) is held to the static floor
// with its brief beside it. A launch that may CONTINUE a session whose context
// is known sends that whole context before it reads a word of its brief, so
// its floor is the larger of the static floor and that context with the brief
// beside it: the context's last request already counted the harness's own
// prompt, so those two are compared, never added.
//
// EVERY BRIEF THE LAUNCH MAY HAND ITS RUN HOLDS IT TO ITS OWN FLOOR, and the
// launch is held to the largest of them ([agentLauncher.continuedFloor]): the
// session the turn kept, or the job before's own copy in its box, whichever it
// may take up, each held to its own floor and dropped alone where that one
// cannot be carried; and the fresh brief, which its box starts a session of
// its own on wherever it takes none up, a restore that fails in the box
// included. Held to the static floor alone, a launch whose brief told a
// round's calls passed its floor in a window that could not carry its first
// request, the launch the floor exists to stop. A brief is counted by its
// estimate, a token for every four bytes (the harness package's
// EstimateTokens), as every floor here counts one. The floor turned off (0)
// holds no launch whose context is not known, whatever its brief; one whose
// context is known is held to its context with its brief still.
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
// AND A FLOOR NO WINDOW CAN EVER LEAVE ROOM FOR IS NO FLOOR TO WAIT ON. Waited
// for, it would hold the turn for ever. And no window leaves a launch its whole
// ceiling: the turn that launches has spent its way there first, in the very
// windows the launch is judged against, its context assembly on the seat's
// auxiliary chain before a dispatch's launch, its review and its rewrites
// before a relaunch, and every turn that tries again spends it again. So the
// most room a window can ever leave a launch is its ceiling less what the
// launch's own turn has charged in it ([windowRoom], read off the meter the
// turn charges through), and a floor is measured against that, never against
// the ceiling. A session whose floor is past the least such room of the seat's
// and the company's capped windows is not continued ([launchFloor.fits]): its
// launch continues the other session it may take up where that one's own
// floor fits, and otherwise starts a session of its own on its fresh brief.
// That is decided by the launch, where the brief that continues the session
// exists, never by the size of the session alone: a seat with no capped window
// carries any. The FRESH brief's floor past that room is HELD instead
// ([launchFloor.fresh]): config holds every ceiling at or above the static
// floor (validateExecutorFloor), never above the static floor with a brief and
// the turn's own spend beside it, and a launch whose box may start its run on
// its fresh brief has no other brief to drop it for.
//
// IT IS HELD AT THE STATIC FLOOR WHERE THAT FITS IN THE ROOM, and at the room
// where the turn's own spend left less than the static floor
// ([launchFloor.tokens]); the launch says it is held (`executor_floor_held`),
// naming the floor it is held to, a session's where one it offers holds it
// higher ([agentLauncher.continuedFloor]).
// At the static floor it goes out in a window with that much left, whatever
// else was spent there, and may go out with as little as that, so its first
// request can take the window past its ceiling by up to the room less the
// static floor more than a launch held at the room would, which the run's
// charge then says (internal/sandbox's `sandbox_spend_over_budget`). Held at
// the room where the static floor fits, it waited for a window that held
// nothing but its own turn's spend at its read: in a busy company whose own
// ceiling is the smallest, the other seats spend in that window first after
// every turnover, and the launch could wait for ever, which is worse than a run
// that overspends and is reported. At the static floor it goes out wherever a
// launch held to the static floor alone, its brief uncounted, did, so a brief
// past the room never keeps its launch waiting longer than that. Held at the
// ceiling itself, it waited for a window with nothing in it at its own read,
// which its own turn never left it. Where the turn's own spend left less than
// the static floor, the room is the most any window can leave the launch, so it
// is held there and waits for a window holding nothing but that spend, where a
// launch held to the static floor alone could not go out at all.
//
// AND A FLOOR WITHIN THE ROOM IS WAITED FOR ONE TURNOVER AT MOST. The room is
// what the launch's own turn leaves; what other seats spend in the company's
// window before the launch reads it is not in it, so in a busy company whose own
// ceiling is the smallest, where they spend there first after every turnover, a
// floor above the static one and within that much of the room is past what
// every window leaves at the launch's read: a fresh brief's, an unmeasured
// continuation's, a measured session's. Waited for, it was refused in every
// window, where a launch held to the static floor alone, its brief uncounted,
// went out in the first. So the window a launch is refused below its floor in
// rides its wait (on a dispatch, the seat's park on this node, [floorWaits]; on
// a resume, its hold, onto the job's record,
// [sandbox.LaunchRecord.RelaunchRefusedIn]), and its next try, in a window
// after it, holds its fresh brief's floor as one past the room is held
// ([launchFloor.after]) and drops a session whose floor that window still
// leaves no room for ([launchFloor.stillShort]): it goes out wherever a launch
// held to the static floor alone would, one turnover later at most. A seat that
// moves to another node between the two may wait one window more, the record
// being its park's, and a record that holds no window, as a build without it
// wrote it, reads as never refused, which costs the same.
//
// AND NEITHER IS A SESSION THE LAUNCH'S MODEL CANNOT CARRY: a Claude Code run's
// window is its model's own ([sandbox.ClaudeCodeWindow]), and a session kept under
// a larger one, continued on a member whose model has a smaller window, is past
// what one request of it can send ([launchFloor.carriedBy]).

// launchFloor is the room one executor launch needs.
type launchFloor struct {
	// static is turn_engine.executor_min_budget_tokens: the fixed part of a
	// launch's first request, beside which its brief is counted.
	static int

	// context is the whole input of the last request of the session the
	// launch continues, cache included, zero where it continues none or
	// nothing measured it (for the job before's own copy in its box, neither
	// that job's newest capture nor its record); and brief the estimate of the
	// brief it is handed, zero for a floor that names none (the static floor
	// alone).
	context int64
	brief   int

	// room is the most room the seat's and the company's capped windows can
	// leave the launch ([windowRoom]), set on the floor of a launch's fresh
	// brief alone ([launchFloor.fresh]), the zero room where nothing is
	// capped: a floor whose context is not known, past it, is held
	// ([launchFloor.held]), at the static floor where that fits in it and at
	// it otherwise, brief still saying the estimate it was held from. A
	// session's floor never carries one: past that room, the session is not
	// continued ([launchFloor.fits]).
	room windowRoom

	// waited is the window an earlier try of the launch's work was refused
	// below its floor in, where that window has turned over since
	// ([launchFloor.after]), the zero window otherwise: set on the floor of a
	// launch's fresh brief alone, which is then held as one past the room is,
	// whatever its brief, so the launch waits one turnover at most for the
	// room its brief needs.
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
// charged spent in them, by period ([meter.chargedIn]): every capped window of
// the company's and the seat's at its ceiling less that charge, both scopes
// alike, since every charge reaches both. The window that leaves the least is
// the one a line names, the smaller ceiling where two leave the same, so the
// line says one thing however the caps are walked.
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

// says is r as a line names it: the figure and what it is made of, none where
// the turn's own spend has taken a window past its ceiling. Where the turn
// charged nothing in the window that leaves the least, that window's is the
// smallest capped ceiling, since every other window leaves at most its own.
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

// continuing is f for a launch that continues a session whose last request
// carried context tokens (zero where nothing measured it), handed a brief of
// about brief tokens.
func (f launchFloor) continuing(context int64, brief int) launchFloor {
	f.context, f.brief = max(context, 0), max(brief, 0)
	return f
}

// fresh is f for a launch's fresh brief, of about brief tokens, which its box
// starts a session of its own on wherever it takes no session up, judged
// against r, the most room the seat's and the company's capped windows can
// leave the launch: where the static floor with that brief beside it is past
// r, the floor is held ([launchFloor.held]), at the static floor where that
// fits in r and at r otherwise ([launchFloor.tokens]).
//
// HELD, NEVER DROPPED: a session whose floor no window can meet is dropped for
// the brief its launch may hand its run instead, and the fresh brief has none
// to be dropped for. Refused at every window, the launch would wait for ever.
func (f launchFloor) fresh(brief int, r windowRoom) launchFloor {
	f.context, f.brief, f.room = 0, max(brief, 0), r
	return f
}

// after is f, the floor of a launch's fresh brief ([launchFloor.fresh]), for a
// launch an earlier try of whose work was refused below its floor in window w,
// a window that has turned over since: held as a floor past the room is
// ([launchFloor.held]), at the static floor where that fits in the room and at
// the room otherwise, whatever its brief.
//
// ONE TURNOVER, NO MORE: the try that window refused was held to the floor of
// its brief, or of a session it offered, which a busy company's window may
// never leave the launch at its read, its other seats spending there first; so
// the next try waits for no more than a launch held to the static floor alone,
// its brief uncounted, waited for, and goes out where that one would.
func (f launchFloor) after(w sandbox.BudgetWindow) launchFloor {
	f.waited = w
	return f
}

// held reports whether f is held below its brief's floor, at what
// [launchFloor.tokens] says: a floor whose context is not known, whose brief
// beside the static floor is past the most room the seat's and the company's
// capped windows can leave the launch, or whose launch was refused below its
// floor in a window that has turned over since ([launchFloor.after]), whatever
// its brief. Held where that room is below the static floor too, its own turn
// having spent the difference in the window first: refused, the launch would
// wait for ever there as well. Never under a ceiling below the static floor,
// which config refuses (validateExecutorFloor), and which leaves the launch the
// refusal it gets without a brief; and never at a room the turn's own spend
// used up, a window with no room for a single token, which refuses the turn's
// every call already.
func (f launchFloor) held() bool {
	r := f.room
	return f.context <= 0 && f.static > 0 && r.capped && r.smallest >= f.static && r.tokens > 0 &&
		(f.static+f.brief > r.tokens || f.waited.Valid())
}

// heldFor is why a held f is held, in its refusal's, its hold's and its line's
// words: its brief beside the static floor is past the room, or, where it is
// within the room, is a floor held after its launch was refused below its
// floor in a window that has turned over since.
//
// NEVER "THE FLOOR ITS LAUNCH WAS REFUSED BELOW": the wait's record holds the
// window alone ([floorWaits], [sandbox.LaunchRecord.RelaunchRefusedIn]), and
// the floor that window refused was the try's, the largest floor of every
// brief it offered its run: a session's where it offered one whose floor was
// larger, which the try after it drops where the window is still short of it
// ([launchFloor.stillShort]). Said of the fresh brief's floor after such a
// try, those words named a floor no window had refused.
func (f launchFloor) heldFor() string {
	if f.static+f.brief > f.room.tokens {
		return "past " + f.room.says()
	}
	return fmt.Sprintf("a floor held after its launch was refused below its floor in the %s window %s, which "+
		"has turned over since", f.waited.Period, f.waited.Label)
}

// tokens is the floor, in tokens: a known context's with its brief, the static
// floor's with its brief beside it where no context is known, and a held
// floor's ([launchFloor.held]: past the room, or after a turnover its launch
// waited) at the static floor where that fits in its room and at the room
// otherwise.
func (f launchFloor) tokens() int {
	switch {
	case f.context > 0:
		return max(f.static, int(f.context)+f.brief)
	case f.static <= 0:
		// THE FLOOR TURNED OFF holds no launch whose context is not known,
		// whatever its brief, as "0 turns the floor off" says; a known
		// context, above, still holds its own.
		return 0
	case f.held():
		// THE STATIC FLOOR WHERE IT FITS IN THE ROOM, never the room above
		// it: held at that room, the launch needs a window that holds
		// nothing but its own turn's spend at its read, which a busy
		// company's other seats may never leave it. The room only where it
		// is below the static floor, the most a window can leave the launch.
		return min(f.static, f.room.tokens)
	}
	return f.static + f.brief
}

// heldAt says where a held f is held, in its refusal's, its hold's and its
// line's words: at the static floor alone where that fits in its room, and at
// the room otherwise, which only a window holding nothing but its own turn's
// spend leaves ([launchFloor.tokens]).
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

// fits is why no window can ever leave room for f, "" where one can: a floor
// past r, the most room the seat's and the company's capped windows can leave
// the launch, is one no window turning over ever meets, its launch's own turn
// spending its way into each before the launch reads it. A session nothing
// measured is judged at the static floor with its brief beside it, which is
// what its launch needs. A floor of nothing fits anywhere, a window its own
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
// seat's windows leave the launch now, is short of f again. The try window w
// refused was held to that floor, a session's within the most room a window can
// leave the launch and past what a busy window leaves at its read; waited for
// again, it would hold the launch in every window, so the launch offers the
// other session where that one's floor fits, and otherwise starts its run on
// its fresh brief, which [launchFloor.after] holds. A session whose floor the
// window now leaves room for is still continued.
func (f launchFloor) stillShort(left int, w sandbox.BudgetWindow) string {
	if f.tokens() <= left {
		return ""
	}
	return fmt.Sprintf("a launch that continues the session needs %d tokens (%s), and its windows leave %d, "+
		"short of it again after the %s window %s refused the launch below its floor: a launch waits one "+
		"turnover at most for the room its briefs need", f.tokens(), f.madeOf(), left, w.Period, w.Label)
}

// madeOf is what a session's floor f is made of, in a line's words: its last
// request's context and its brief, or, where nothing measured that context
// (no capture of it, nor, for the job before's own copy in its box, that job's
// record), the static floor and its brief.
func (f launchFloor) madeOf() string {
	if f.context <= 0 {
		return fmt.Sprintf("nothing measured its context, so turn_engine.executor_min_budget_tokens, %d, "+
			"and its brief, about %d", f.static, f.brief)
	}
	return fmt.Sprintf("its last request carried %d, and its brief about %d", f.context, f.brief)
}

// turnedOver reports whether w, the window a launch was refused below its floor
// in, has turned over by the instant m cuts its windows at: the window of w's
// period that instant falls in has a later label, which is a later window,
// since the labels of one period sort in calendar order. False for a w that
// names no window; for one still current, which a park an apply released early
// hands its launch back into, where the launch is held to its floor as it was;
// and for a meter that caps nothing, which refuses no launch.
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
	return fmt.Sprintf("a launch that continues the session sends %d tokens first (its last request "+
		"carried %d, and its brief about %d), more than one request under the %d-token window of the "+
		"model the launch runs can carry", f.context+int64(f.brief), f.context, f.brief, w.Tokens)
}

// floorRefusal is a launch refused below its floor: a budget refusal
// ([toolloop.ErrBudgetExhausted]) that carries the floor and what the seat had
// left, so the hold that waits on it waits for room for THIS launch.
type floorRefusal struct {
	floor launchFloor
	left  int
}

// Error says what the floor is made of, in the figures it is made of: a
// continued session's context with its brief, a brief beside the static floor,
// that held where it is past the most room the seat's and the company's capped
// windows can leave the launch or its launch was refused below its floor in a
// window that has turned over since, why and where it is held
// ([launchFloor.heldFor], [launchFloor.heldAt]), or the static floor alone.
func (r *floorRefusal) Error() string {
	f := r.floor
	switch {
	case f.context > 0:
		return fmt.Sprintf("this seat has %d tokens left and an executor run that continues its "+
			"session needs at least %d: its session's last request carried %d tokens and its brief "+
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
