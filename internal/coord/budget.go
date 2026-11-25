package coord

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/period"
)

// The token counters: what every seat and the whole company have spent in each
// calendar window, shared by the fleet (ADR-0019). A scope (the company or one
// seat) has ONE record holding a slot per period (day, ISO week, month), each
// with its window's LABEL (`2026-09-23`, `2026-W39`, `2026-09`), its spend and
// when the gate last refused in it, so a charge is one compare-and-swap per
// scope, the company's then the seat's ([Budgets.Charge]).
//
// THE ROLL IS THE RESET: a charge finding a slot on an earlier window rolls it
// (label moved on, spend and refusal cleared) in the same compare-and-swap, so an
// allowance comes back exactly when its window turns over, on whichever node
// charges next. Every node computes labels alike (internal/period), so two nodes
// rolling one slot write the same label and the compare-and-swap decides.

// OrgScope is the company-wide token counter's key.
const OrgScope = "org"

// AgentScope is one seat's counter key, on the seat's DERIVED agent id (the
// uuid over its handle, ADR-0013), as its lease, mailbox and diary are.
func AgentScope(agentID string) string { return "agent:" + agentID }

// LearningScope is one seat's learning counter: the share of its spend that
// went on learning, read beside [AgentScope] and judged by no ceiling.
func LearningScope(agentID string) string { return "learning:" + agentID }

// PersonScope is one person's counter key: their Dashboard Assistant spend,
// judged by the company's `assistant.token_budget` (internal/engine). Keyed on
// the person, never a seat: the assistant acts as the person and spends nothing
// a seat's ceiling was set for. A row of its own only where a company caps a
// person, so a listing holds no scope no ceiling can judge.
func PersonScope(person uuid.UUID) string { return "person:" + person.String() }

// Windows is the window of each period a charge is counted in, or a read is
// read against, in [period.Periods] order, cut on the company's clock by the
// CALLER: the store never reads a clock, so a charge and the read after it
// agree on "now" exactly when handed the same Windows.
type Windows [len(period.Periods)]period.Window

// WindowsAt is the day, the week and the month t falls in on the clock loc.
func WindowsAt(t time.Time, loc *time.Location) Windows {
	var w Windows
	for i, p := range period.Periods {
		w[i] = period.At(p, t, loc)
	}
	return w
}

// Validate refuses a Windows that does not name one window of each period in
// order. Backends MUST call it: a zero Window has no label, and a slot rolled to
// an empty label reads as never charged, handing every scope its whole allowance
// on every charge.
func (w Windows) Validate() error {
	for i, p := range period.Periods {
		switch {
		case w[i].Period != p:
			return fmt.Errorf("coord: windows[%d] is a %q window; a charge names the day, "+
				"the week and the month in that order (coord.WindowsAt)", i, w[i].Period)
		case w[i].Label == "":
			return fmt.Errorf("coord: the %s window has no label; cut it with period.At "+
				"(coord.WindowsAt) on a moment inside the years 0000-9999", p)
		case w[i].Shift(0).Label != w[i].Label:
			// A LABEL period.At DID NOT CUT: a slot keeps it, so one assembled
			// by hand would put words of any length in a counter's record.
			return fmt.Errorf("coord: the %s window's label %q is none period.At cuts; cut it "+
				"with period.At (coord.WindowsAt)", p, w[i].Label)
		}
	}
	return nil
}

// Caps is one scope's ceilings as a charge carries them: for each capped
// period, the most the scope may spend in that period's current window. AN
// ABSENT PERIOD IS UNCAPPED, the only way to leave one open. A ceiling below 1 is
// REFUSED, here as at the config (config.TokenBudget): 0 reads as "unlimited" to
// some and as "nothing may be spent" to others. org.TokenCeilings converts to it
// directly.
type Caps map[period.Period]int

// Validate refuses a period that is not one and a ceiling below one token.
func (c Caps) Validate() error {
	for p, ceiling := range c {
		if !p.Valid() {
			return fmt.Errorf("coord: %q is not a period; a ceiling is per day, week or month", p)
		}
		if ceiling < 1 {
			return fmt.Errorf("coord: the %s ceiling is %d; a ceiling is at least 1 token, "+
				"and an uncapped window is one the caps leave out", p, ceiling)
		}
	}
	return nil
}

// ChargeRequest is one round's charge, and what it is judged against: a struct,
// as [ActivationRequest] is, because two sets of ceilings side by side in a
// signature are how a caller passes the seat's where the company's belong.
type ChargeRequest struct {
	// Seat is the seat's counter key, [AgentScope] of its derived id.
	Seat string

	// Tokens is what the round SPENT, read off the model's reply. Zero or less
	// is no charge at all ([Budgets.Charge]).
	Tokens int

	// Windows is the current window of each period on the company clock.
	Windows Windows

	// OrgCaps and SeatCaps are the company's ceilings and the seat's own, both
	// from the epoch the turn is pinned to. Not Org: beside the scope key Seat it
	// would read as the company's key, which is always [OrgScope] and never passed.
	OrgCaps  Caps
	SeatCaps Caps
}

// Validate refuses a request a backend cannot count: no seat, windows that are
// not the day, week and month, or a ceiling below one.
func (r ChargeRequest) Validate() error {
	if r.Seat == "" {
		return errors.New("coord: a charge needs a seat scope")
	}
	if err := r.Windows.Validate(); err != nil {
		return err
	}
	if err := r.OrgCaps.Validate(); err != nil {
		return fmt.Errorf("the company's caps: %w", err)
	}
	if err := r.SeatCaps.Validate(); err != nil {
		return fmt.Errorf("the seat's caps: %w", err)
	}
	return nil
}

// SeatUncountedError is a charge or a post-charge whose company write landed
// and whose seat write then failed: the one partial the built order (company
// first, [Budgets.Charge]) can leave. It is NAMED, not undone: the spend was
// billed before it was recorded, so the company's record is true, and almost
// nothing that records spend asks again, so taking it back would leave the
// counter every seat is judged against short of spend already paid for.
//
// The seat's counter is short by Tokens in the spend's windows until they turn
// over, the fail-open direction for that one seat's cap, bounded by how often a
// write fails right after one landed, and logged by the backend. A caller that offers
// the spend again (the sandbox, when a collected run's resume fails) records the
// seat's share alone ([Budgets.PostChargeSeat]): the company already holds it.
type SeatUncountedError struct {
	// Seat is the seat's counter key, and Tokens what its counter is missing.
	Seat   string
	Tokens int

	// Err is why the seat's write failed.
	Err error
}

func (e *SeatUncountedError) Error() string {
	return fmt.Sprintf("coord: the company's counter holds %d tokens that %s's does not, "+
		"because the seat's write failed: %v", e.Tokens, e.Seat, e.Err)
}

func (e *SeatUncountedError) Unwrap() error { return e.Err }

// Spend is what one charge did.
type Spend struct {
	// OK is false when a scope refused. The Refused fields then say WHICH
	// scope, in WHICH window, and by how much: "the company is out for the
	// month" and "this seat is out for the day" send an operator to different
	// places.
	OK           bool
	RefusedScope string

	// RefusedPeriod and RefusedWindow name the window the scope refused
	// in: of several, the one that ENDS LAST, when the scope can next admit
	// the charge without a raised ceiling (an earlier one's turnover changes
	// nothing). Windows ending together (the last day of a month that is a
	// Sunday) name the longer period. Every reader naming the window a scope
	// waits on (the budget park, the dashboard's attention queue) chooses by
	// the same rule, [Outlasts].
	RefusedPeriod period.Period
	RefusedWindow period.Window

	// RefusedUsed is that window's spend AS THE CHARGE LEFT IT, the refused
	// round included ([Budgets.Charge]), and RefusedLimit its ceiling: the
	// figure every later reader of the counter is shown.
	RefusedUsed  int
	RefusedLimit int

	// Org and Agent are both counters AS THE CHARGE LEFT THEM, against the
	// request's windows, a refusal's included: a refusal names ONE window of
	// ONE scope, and the round it records can fill another (a seat's refusal
	// leaving the company's day at its ceiling), which a caller keeping what
	// an answer makes certain (the engine's turn meter) has to see.
	//
	// A scope whose write did not land is the zero Usage, its Scope empty:
	// the seat, on a company refusal whose seat write failed (logged, never an
	// error; [Budgets.Charge]).
	Org   Usage
	Agent Usage
}

// WindowUsage is one window's slot of a scope's counter.
type WindowUsage struct {
	// Window is the window the slot counts, on the caller's clock: the one
	// asked about, or the LATER window the slot is already on (a peer's clock
	// ran ahead across a boundary, or the company's zone moved west); see
	// [Tally.Usage] for why a read never answers the asked window unspent.
	Window period.Window

	Used int

	// RefusedAt is when this window last turned a call away, zero once it
	// has admitted a charge since. It does NOT decide room (Used against the
	// ceiling does): a ceiling raised since leaves a stale stamp on a window
	// with room, and a post-charge can fill a window with no stamp. Kept in
	// the shared counter because every node reports it; a per-node stamp
	// would flicker on a dashboard. Stamped by a refused charge and by
	// [Budgets.Refuse] (work turned away before any call), on each window
	// that could not fit, of the refusing scope only. Cleared by an ADMITTED
	// charge (every window of both scopes it charged) and by turnover, by
	// nothing weaker; a charge refused overall leaves other scopes' stamps
	// alone, so the answer never depends on which scope a backend tests first.
	// Post-charges neither stamp nor clear: they decide nothing about room.
	RefusedAt time.Time
}

// Usage is one scope's counter, read against a set of windows.
type Usage struct {
	Scope string

	// UpdatedAt is when the counter last moved: a charge, admitted or
	// refused, or a post-charge; never a bare [Budgets.Refuse], which spends
	// nothing. Zero only for a scope nothing has charged.
	UpdatedAt time.Time

	// Windows is one slot per period, in [period.Periods] order, each
	// exactly as a charge against the same windows would find it
	// ([Tally.Usage]).
	Windows [len(period.Periods)]WindowUsage
}

// Unspent is a scope nothing has charged, read against w: every window at
// zero. It is what a listing's absent row means, spelled so a reader holding
// one never has to invent the windows it covers.
func Unspent(scope string, w Windows) Usage {
	return Tally{}.Usage(scope, w)
}

// In is the slot of one period.
func (u Usage) In(p period.Period) WindowUsage {
	if i := slotOf(p); i >= 0 {
		return u.Windows[i]
	}
	return WindowUsage{}
}

// Outlasts reports whether a scope waits on window a longer than on b: a ends
// later, or ends at the same instant and is the longer period. Exported so a
// caller choosing ACROSS scopes takes the one tie-break [Spend.RefusedWindow]
// does, rather than a copy that could drift.
func Outlasts(a, b period.Window) bool {
	return outlasts(a, slotOf(a.Period), b, slotOf(b.Period))
}

// outlasts is [Outlasts] with a and b at positions ai and bi in
// [period.Periods]: the one tie-break every "which window" answer takes,
// turning on the window's end and the period, never on arrival order.
func outlasts(a period.Window, ai int, b period.Window, bi int) bool {
	if c := a.End.Compare(b.End); c != 0 {
		return c > 0
	}
	return ai > bi
}

// SortUsage puts the org counter first, then the seats by scope. Shared by
// the backends: "org" does NOT sort before "agent:…", and an order that
// differed between backends would make a diff of two captures unreadable.
func SortUsage(rows []Usage) {
	rank := func(u Usage) int {
		if u.Scope == OrgScope {
			return 0
		}
		return 1
	}
	slices.SortFunc(rows, func(a, b Usage) int {
		return cmp.Or(cmp.Compare(rank(a), rank(b)), cmp.Compare(a.Scope, b.Scope))
	})
}

// Budgets is the fleet's token counters. USAGE IS SHARED, CAPS AND THE
// CALENDAR ARE NOT: ceilings and the company clock belong to a config epoch
// (ADR-0018), so they travel IN on every call and the store holds only spend per
// window label, never reading a clock; the counter is one number per window
// fleet-wide, since per-node counters let N nodes each spend the whole allowance.
//
// A backend a fleet shares COALESCES each node's charges on one scope into one
// write (coord/kv's writeLines), each decided in arrival order exactly as a
// serial order would answer it, so no answer depends on its batch and a key's
// contenders are nodes, not every round in flight (a count no attempt bound
// covers, failing closed while the store answers). The memory twin is the same
// serial order behind one mutex.
type Budgets interface {
	// Charge counts a round on the org's counter, then the seat's, in every
	// window, admitting it only if EVERY capped window of BOTH had room before. A
	// REFUSAL COUNTS THE ROUND TOO: it was billed when its reply came, and the
	// refusal stops only what follows (its tool calls, the next round), so a refused
	// window reads past its ceiling until it turns over or the ceiling is raised.
	// Org first, so a refusal names the company when both are out; a seat the org
	// refused is still counted, with no verdict of its own. A seat write failing
	// after the org's landed is a kept [SeatUncountedError]; once the org's lands,
	// the seat's runs on a context outliving the caller's. An error (store
	// unreachable, request invalid) FAILS CLOSED and is never reported as a refusal.
	// Zero tokens or fewer writes nothing and answers OK, so a provider omitting
	// usage stops nothing. Stamps: [WindowUsage.RefusedAt]; answers: [Spend.Org].
	Charge(ctx context.Context, req ChargeRequest) (Spend, error)

	// PostCharge adds spend that has ALREADY HAPPENED to the org's counter and
	// the seat's, and never refuses: spend with no verdict waiting on it (a
	// detached executor launch, collected later, maybe on another node), in the
	// windows it is collected in. It answers OK with both counters, for the caller
	// to compare with its caps; a charge of nothing writes and reads nothing.
	//
	// It neither stamps nor clears refusals ([WindowUsage.RefusedAt]). Two writes,
	// company first, NEITHER taken back and the seat's outliving a caller that hangs
	// up, as Charge's. On a [SeatUncountedError] a retry records the seat's share
	// alone ([Budgets.PostChargeSeat]); any other error is the company's write,
	// which may have landed: offering it again can only over-state the company,
	// tripping a cap early rather than late.
	PostCharge(ctx context.Context, seat string, tokens int, windows Windows) (Spend, error)

	// PostChargeSeat adds spend that has ALREADY HAPPENED to one SEAT's counter
	// alone and never refuses, answering the seat's counter (an empty [Usage] for a
	// charge of nothing, as PostCharge). It FINISHES a post-charge whose company
	// half landed and seat half did not ([SeatUncountedError]) without counting
	// the company twice. Like every post-charge it leaves the refusal stamps alone.
	PostChargeSeat(ctx context.Context, seat string, tokens int, windows Windows) (Usage, error)

	// PostChargeOrg adds spend that has ALREADY HAPPENED to the COMPANY's counter
	// alone and never refuses, answering it (an empty [Usage] for a charge of
	// nothing). It is for spend no seat's TURN made: a person's dashboard question
	// to the company's knowledge (`answer_knowledge`) runs on the auxiliary model of
	// the seat they are bound to, but a seat's ceiling is set for its turns, so
	// there is no seat half. The caller gates BEFORE the call on [Budgets.Used];
	// this records what it cost, past a ceiling included, leaving refusal stamps
	// alone.
	PostChargeOrg(ctx context.Context, tokens int, windows Windows) (Usage, error)

	// Refuse records a refusal the gate made WITHOUT A CHARGE: on the one scope
	// named, it stamps [WindowUsage.RefusedAt] on every capped window with no room
	// for a single token, and counts nothing. An answered caller knows a full window
	// refuses every charge, so the engine turns work away before any call (a turn's
	// meter, the budget park, a person's question, reflection) and records it here;
	// else a full window refuses everything while saying it refused nothing. Judged
	// against the counter rolled to windows as a charge would be, never the caller's
	// memory, and of that scope only, named as a charge's refusal is (company before
	// seat). It writes only what it stamps: no roll persisted or record created
	// otherwise, [Usage.UpdatedAt] unmoved. It answers the scope's counter; an error
	// (unreachable, invalid request) is never a refusal or an admission.
	Refuse(ctx context.Context, scope string, caps Caps, windows Windows) (Usage, error)

	// Used reports one scope's counter against the given windows. A scope
	// never charged reads [Unspent]; an unreachable store is an error,
	// never a zero.
	Used(ctx context.Context, scope string, windows Windows) (Usage, error)

	// Usage returns every counter the store holds, read against the given
	// windows, in [SortUsage] order so two reads of an unchanged counter are
	// byte-identical. A scope whose windows have all turned over is listed at zero
	// until its record ages out ([BudgetRetention] after its last write, on KV).
	Usage(ctx context.Context, windows Windows) ([]Usage, error)
}

// ---- the arithmetic, shared by every backend ------------------------------ //

// Tally is one scope's counter as a backend stores it: a slot per period, and
// when the counter last moved. Pure arithmetic over values, exported so both
// backends run one implementation of the roll, the fit, the stamps and which
// window a refusal names, differing only in how they make a read and a write
// atomic: a rule each spelled for itself could diverge where the contract suite
// cannot reach. Every method returns a new Tally and leaves its receiver alone.
type Tally struct {
	// Slots holds one slot per period, in [period.Periods] order.
	Slots [len(period.Periods)]Slot

	// At is when the counter last moved. See [Usage.UpdatedAt].
	At time.Time
}

// Slot is one period's slot of a [Tally].
type Slot struct {
	// Label is the label of the window this slot counts
	// ([period.Window.Label]), and empty for a slot nothing has written.
	Label string

	Used      int
	RefusedAt time.Time
}

// slotOf is the position of p in [period.Periods], or -1.
func slotOf(p period.Period) int {
	return slices.Index(period.Periods[:], p)
}

// Roll moves every slot on an earlier window than w's onto w's window, unspent
// and not refusing. A slot on w's or a LATER window is kept, NEVER ROLLED BACK:
// labels sort in calendar order, so a node whose clock trails a peer's across a
// boundary counts on the next window. That fails closed: a round charged a
// second early to tomorrow costs tomorrow a round, where a roll back would hand
// tomorrow its whole allowance again on every such charge.
func (t Tally) Roll(w Windows) Tally {
	for i := range t.Slots {
		if t.Slots[i].Label < w[i].Label {
			t.Slots[i] = Slot{Label: w[i].Label}
		}
	}
	return t
}

// Refusing is every capped period whose slot has no room for tokens more, in
// [period.Periods] order. The tally must already be rolled to the charge's
// windows, and must be the counter as it stood BEFORE the tokens were added:
// the verdict is whether they fitted the room there was.
func (t Tally) Refusing(tokens int, caps Caps) []period.Period {
	var out []period.Period
	for i, p := range period.Periods {
		if ceiling, capped := caps[p]; capped && t.Slots[i].Used+tokens > ceiling {
			out = append(out, p)
		}
	}
	return out
}

// Add counts tokens in every slot, moving the counter's clock to at; the tally
// must already be rolled to the charge's windows. Only ever spend, never credit
// ([SeatUncountedError]); every caller has returned already for a charge of
// nothing.
func (t Tally) Add(tokens int, at time.Time) Tally {
	for i := range t.Slots {
		t.Slots[i].Used += tokens
	}
	t.At = at
	return t
}

// Count is one scope's share of a charge: the tally rolled to w, tokens counted
// in every slot at instant at, and the verdict (every capped period with no room
// for them BEFORE they were added) stamped. Nil caps judge nothing: a
// post-charge, or a seat counted for a round the company refused. The round is
// counted whatever the verdict ([Budgets.Charge]). An admission leaves the
// stamps, because only the caller knows whether the charge fitted OVERALL (a
// seat's refusal must not erase the company's); that caller clears them
// ([Tally.ClearAll], [Tally.Clear]).
func (t Tally) Count(tokens int, caps Caps, w Windows, at time.Time) (Tally, []period.Period) {
	rolled := t.Roll(w)
	refusing := rolled.Refusing(tokens, caps)
	return rolled.Add(tokens, at).Stamp(refusing, rolled, at), refusing
}

// Refuse is one scope's share of [Budgets.Refuse]: the tally rolled to w, and
// at stamped on every capped period with no room for a single token, answering
// the periods it stamped. That is [Tally.Refusing] of ONE token, the rule itself:
// a window refuses a charge of any size exactly when it refuses the smallest.
// Nothing is counted and the counter's clock does not move.
func (t Tally) Refuse(caps Caps, w Windows, at time.Time) (Tally, []period.Period) {
	rolled := t.Roll(w)
	full := rolled.Refusing(1, caps)
	return rolled.Stamp(full, rolled, at), full
}

// Stamp records at as the last refusal of each period in periods, on slots
// still on the window refused holds for it; one rolled on since is left alone,
// its refused window being over. The counter's clock is [Tally.Add]'s to move.
func (t Tally) Stamp(periods []period.Period, refused Tally, at time.Time) Tally {
	for _, p := range periods {
		i := slotOf(p)
		if i >= 0 && t.Slots[i].Label == refused.Slots[i].Label {
			t.Slots[i].RefusedAt = at
		}
	}
	return t
}

// Refused reports whether any slot carries a refusal stamp.
func (t Tally) Refused() bool {
	for _, s := range t.Slots {
		if !s.RefusedAt.IsZero() {
			return true
		}
	}
	return false
}

// Clear drops every refusal stamp the tally seen carried, and only those: a
// slot stamped again since, or rolled onto another window, keeps what it has,
// because that refusal is still true.
func (t Tally) Clear(seen Tally) Tally {
	for i := range t.Slots {
		s := seen.Slots[i]
		if !s.RefusedAt.IsZero() && t.Slots[i].Label == s.Label && t.Slots[i].RefusedAt.Equal(s.RefusedAt) {
			t.Slots[i].RefusedAt = time.Time{}
		}
	}
	return t
}

// ClearAll drops every refusal stamp, which is what an admitted charge does to
// the scope it charged: it has just had room in every window.
func (t Tally) ClearAll() Tally {
	for i := range t.Slots {
		t.Slots[i].RefusedAt = time.Time{}
	}
	return t
}

// Usage reads the tally against w EXACTLY AS A CHARGE AGAINST w WOULD FIND
// IT: rolled to w, so a slot on an earlier window reads as w's window unspent,
// and one already on a LATER window reads as that window, spend and refusal,
// never zero. A slot never rolls back, so the next charge is refused against
// that later spend; answering w's window unspent would hand every reader (a
// turn's headroom, the learning gate, the budgets answer, the live meter) room
// the gate will not admit: seconds for a peer's clock ahead, up to a day after
// the company's zone moves west (ADR-0018 allows a live edit).
func (t Tally) Usage(scope string, w Windows) Usage {
	rolled := t.Roll(w)
	u := Usage{Scope: scope, UpdatedAt: t.At}
	for i, s := range rolled.Slots {
		u.Windows[i] = WindowUsage{Window: rolled.window(i, w), Used: s.Used, RefusedAt: s.RefusedAt}
	}
	return u
}

// window is the window slot i counts, on w's clock: w's own, or the later one
// a leading slot is on; the tally must be rolled to w. A later label that does
// not parse (only a record this build did not write) is named as w's window, so
// a reader still has instants; the spend stays the slot's, which the gate uses.
func (t Tally) window(i int, w Windows) period.Window {
	window := w[i]
	if label := t.Slots[i].Label; label != window.Label {
		if later, err := period.Parse(window.Period, label, window.Start.Location()); err == nil {
			return later
		}
	}
	return window
}

// Refusal is the answer a scope's refusal gives: the refusing window that ends
// last (at a tie the longer period, by [outlasts], never by the order periods
// arrive in), its spend and its ceiling. scope is the name reported ("org" or
// "agent"), periods what [Tally.Refusing] found, and t the counter as the charge
// LEFT it, rolled to w with the refused round added, so [Spend.RefusedUsed] is
// what every later reader is shown. A slot a peer's clock already moved onto a
// later window is named as that window, read on w's clock.
func (t Tally) Refusal(scope string, periods []period.Period, caps Caps, w Windows) Spend {
	out := Spend{RefusedScope: scope}
	named := -1
	for _, p := range periods {
		i := slotOf(p)
		if i < 0 {
			continue
		}
		window := t.window(i, w)
		if named >= 0 && !outlasts(window, i, out.RefusedWindow, named) {
			continue
		}
		named = i
		out.RefusedPeriod, out.RefusedWindow = p, window
		out.RefusedUsed, out.RefusedLimit = t.Slots[i].Used, caps[p]
	}
	return out
}
