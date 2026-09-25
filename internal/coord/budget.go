package coord

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/period"
)

// The token counters: what every seat and the whole company have spent in each
// calendar window, shared by the fleet (ADR-0019).
//
// # One record per scope, one slot per window
//
// A scope — the company, or one seat — has ONE record, and the record holds a
// slot per period: the day, the ISO week and the month, each carrying the LABEL
// of the window it counts (`2026-09-23`, `2026-W39`, `2026-09`), what has been
// spent in it and when it last refused a charge. A charge is therefore still
// one compare-and-swap per scope, and the org-first, seat-compensates protocol
// that makes two keys all-or-nothing is unchanged from the counter that knew no
// calendar.
//
// # The roll is the reset
//
// A slot whose label is not the current window's belongs to an earlier window,
// and the charge that finds it rolls it — label moved on, spend and refusal
// cleared — inside the same compare-and-swap that counts the charge. Nothing is
// scheduled and nothing is reset by hand: a window's allowance comes back
// exactly when the window turns over, on whichever node next charges the scope.
// The label is what every node computes alike (internal/period), so two nodes
// racing to roll one slot write the same label and the compare-and-swap decides
// whose count lands.
//
// A slot NEVER ROLLS BACK. Labels of one period sort in calendar order, so a
// charge from a node whose clock trails a peer's across a boundary finds a slot
// already on the NEXT window and is counted there rather than wiping it — which
// is the fail-closed direction: a round charged a second early to tomorrow
// costs tomorrow a round, where a slot rolled back to today would hand tomorrow
// back its whole allowance on every such charge. A READ behind such a slot
// answers the later window too ([Tally.Usage]), because the charge that follows
// the read is refused against it.
//
// # Caps are the epoch's, the calendar is the caller's
//
// A cap belongs to a config epoch and the company clock is the epoch's too
// (ADR-0018), so both travel in on every call — the current [Windows] and the
// ceilings — and the store holds only labels and spend. A backend never reads
// a clock to decide which window a charge is in: it is told.

// OrgScope is the company-wide token counter's key.
const OrgScope = "org"

// AgentScope is one seat's counter key.
//
// Keyed on the DERIVED agent id rather than the handle, matching the diary and
// the episodes: renaming a handle then starts a fresh budget rather than
// inheriting the spend of whoever held the name before.
func AgentScope(agentID string) string { return "agent:" + agentID }

// WindowedCountersProtocol is the seat-host protocol that moved the token
// counters from one lifetime figure per scope to a slot per calendar window.
//
// A CONSTANT OF ITS OWN rather than [ProtocolVersion], because the rule it
// carries never moves again: the lifetime counters' bucket may be deleted once
// no live lease is held below THIS protocol, and a later bump that raised
// ProtocolVersion must not make the retirement wait for a protocol that has
// nothing to do with the counters. See [LifetimeCounters].
const WindowedCountersProtocol = 4

// Windows is the window of each period a charge is counted in, or a read is
// read against, in [period.Periods] order: the day, the week, the month.
//
// Cut on the company's clock by the CALLER. The store holds labels and never
// reads a clock, so a charge and the read that follows it agree on which
// window "now" is exactly when they were handed the same Windows.
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
// order.
//
// Backends MUST call this rather than trusting the caller: a zero Window has
// no label, and a slot rolled to an empty label would read as never charged,
// so a caller that forgot to cut its windows would hand every scope its whole
// allowance on every charge.
func (w Windows) Validate() error {
	for i, p := range period.Periods {
		switch {
		case w[i].Period != p:
			return fmt.Errorf("coord: windows[%d] is a %q window; a charge names the day, "+
				"the week and the month in that order (coord.WindowsAt)", i, w[i].Period)
		case w[i].Label == "":
			return fmt.Errorf("coord: the %s window has no label; cut it with period.At "+
				"(coord.WindowsAt) on a moment inside the years 0000-9999", p)
		}
	}
	return nil
}

// Caps is one scope's ceilings as a charge carries them: for each capped
// period, the most the scope may spend inside that period's current window.
//
// AN ABSENT PERIOD IS UNCAPPED, and that is the only way to leave one open. A
// ceiling below 1 is REFUSED rather than read: 0 once meant "unlimited" to this
// counter and means "nothing may be spent" to anybody reading the word
// ceiling, so a value that two readers take two opposite ways is refused at
// the store as it is at the config (config.TokenBudget). org.TokenCeilings
// converts to it directly.
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

// Exceeds reports whether a charge of tokens is larger than a whole ceiling,
// which no counter can ever fit however little it has spent — so a backend can
// refuse it before writing anything.
func (c Caps) Exceeds(tokens int) bool {
	for _, ceiling := range c {
		if tokens > ceiling {
			return true
		}
	}
	return false
}

// ChargeRequest is one round's charge, and what it is judged against.
//
// A STRUCT rather than positional arguments, for the reason
// [ActivationRequest] is one: two sets of ceilings side by side in a
// signature are how a caller passes the seat's where the company's belong.
type ChargeRequest struct {
	// Seat is the seat's counter key, [AgentScope] of its derived id.
	Seat string

	// Tokens is what the round is about to spend. Zero or less is not a
	// charge at all: see [Budgets.Charge].
	Tokens int

	// Windows is the current window of each period on the company clock.
	Windows Windows

	// OrgCaps and SeatCaps are the company's ceilings and the seat's own,
	// both from the epoch the turn is pinned to.
	//
	// Both named for what they hold. Seat above is a scope KEY, so a field
	// beside it named Org would read as the company's key — which is
	// always [OrgScope] and is never passed — rather than its ceilings.
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

// Spend is what one charge did.
type Spend struct {
	// OK is false when a scope refused. The Refused fields then say WHICH
	// scope, in WHICH window, and by how much — "the company is out for the
	// month" and "this seat is out for the day" send an operator to
	// different places, and a bare refusal sends them to neither.
	OK           bool
	RefusedScope string

	// RefusedPeriod and RefusedWindow name the window the scope refused
	// in. When more than one of its windows refused, it is the one that
	// ENDS LAST: that is when the scope can next admit the charge without
	// a ceiling being raised, and naming an earlier one would send a
	// caller waiting for a window whose turnover changes nothing. Windows
	// that end together — the last day of a month that is a Sunday — name
	// the longer period, so every backend gives one answer, and every
	// reader naming the window a scope waits on — the budget park
	// ([Outlasts]), the dashboard's attention queue — chooses by the same
	// rule, so none of them names a window the refusal did not.
	RefusedPeriod period.Period
	RefusedWindow period.Window
	RefusedUsed   int
	RefusedLimit  int

	// Org and Agent are both counters after an admitted charge or a
	// post-charge, read against the request's windows. Zero on a refusal.
	Org   Usage
	Agent Usage
}

// WindowUsage is one window's slot of a scope's counter.
type WindowUsage struct {
	// Window is the window the slot counts, with its instants on the
	// caller's clock: the one the caller asked about, or — where the slot
	// is already on a LATER window, because a peer's clock ran ahead
	// across a boundary or the company's zone moved west — that later
	// window. See [Tally.Usage] for why a read never answers the asked
	// window unspent there.
	Window period.Window

	Used int

	// RefusedAt is when this window last turned a charge away, and zero
	// once it has admitted one since.
	//
	// It is what "exhausted" means, and Used compared against the cap is
	// not: a refused charge increments nothing, so a seat charged in
	// 3 000-token rounds against a 100 000 cap stops near 99 000 and never
	// reads as full. Kept HERE, in the shared counter, because the refusal
	// is the gate's own decision and every node reports this counter: a
	// stamp one node kept in memory would appear and vanish on a dashboard
	// as the reports of different nodes arrived.
	//
	// Stamped on each window that could not fit the charge and on no
	// other, of the scope that refused and of no other. Cleared by an
	// ADMITTED charge, which clears every window of both scopes it charged,
	// and by the window turning over, which rolls the slot; by nothing
	// weaker. A charge refused overall leaves every other scope's stamps
	// alone, even where that scope would have had room, so the answer does
	// not depend on which scope a backend happens to test first. A
	// [Budgets.PostCharge] neither stamps nor clears: it is not a decision
	// about room.
	RefusedAt time.Time
}

// Usage is one scope's counter, read against a set of windows.
type Usage struct {
	Scope string

	// UpdatedAt is when the counter last moved — a charge, a post-charge
	// or an unwind. A refusal does not move it, so a scope known only for
	// a refusal has none.
	UpdatedAt time.Time

	// Windows is one slot per period, in [period.Periods] order, each
	// exactly as a charge against the same windows would find it: a slot
	// holding an earlier window than the one asked about reads as that
	// window unspent, which is what the charge that rolls it will make it,
	// and a slot already on a later window reads as that window, with its
	// spend and its refusal, which is what the charge will be refused
	// against. See [Tally.Usage].
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
// later, or ends at the same instant and is the longer period.
//
// Exported so a caller choosing ACROSS scopes — the company's refusing window
// against the seat's — answers with the one tie-break every "which window"
// answer here takes, rather than a copy of it that could drift from the window
// [Spend.RefusedWindow] names.
func Outlasts(a, b period.Window) bool {
	return outlasts(a, slotOf(a.Period), b, slotOf(b.Period))
}

// outlasts reports whether window a, of the period at position ai in
// [period.Periods], is the one a scope waits on longer than b at bi: it ends
// later, or ends at the same instant and is the longer period.
//
// THE ONE TIE-BREAK every "which window" answer takes — the refusal a charge
// names here, and through [Outlasts] the window a parked seat waits on — so
// they cannot drift apart, and it turns on the window's end and the period,
// never on the order windows arrive in.
func outlasts(a period.Window, ai int, b period.Window, bi int) bool {
	if c := a.End.Compare(b.End); c != 0 {
		return c > 0
	}
	return ai > bi
}

// SortUsage puts the org counter first, then the seats by scope.
//
// Shared by the backends rather than left to each: "org" does NOT sort before
// "agent:…" alphabetically, so a backend that just sorted would put the
// company's own counter in the middle of its seats — and a listing whose order
// differed between backends would make a diff of two captures unreadable.
func SortUsage(rows []Usage) {
	// The org counter ranks 0 and everything else 1, so cmp.Or falls
	// through to the alphabetical compare only among the seats.
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

// Budgets is the fleet's token counters.
//
// USAGE IS SHARED, CAPS AND THE CALENDAR ARE NOT. A cap belongs to a config
// epoch — a revision that raises a ceiling takes effect on the next turn —
// and so does the company clock the windows are cut on, while the counter has
// to be one number per window across the fleet, because per-node counters
// mean N nodes each spend the whole allowance and an org cap of 500 000 is
// silently N x 500 000. So the ceilings and the windows travel IN on every
// call and the store holds only what has been spent, and in which window.
type Budgets interface {
	// Charge checks and increments the seat's counter and the org's in
	// every window, and a refusal by either leaves NEITHER charged.
	//
	// A charge is admitted only while EVERY capped window of BOTH scopes
	// has room for it. There is no transaction here — two keys, and a KV
	// store has no way to write both at once — so the atomicity is built
	// rather than borrowed: the ORG is charged first and compensated if
	// the seat then refuses. The windows cost no extra write: a scope's
	// windows share its one record, and a slot on an earlier window is
	// rolled inside the same write that counts the charge.
	//
	// Org first, and not the reverse, for two reasons that point the same
	// way. It makes the refusal report ORG-FIRST for free when both scopes
	// are out of room, and "the company is out" is the fact that matters —
	// raising one seat's ceiling against an exhausted org changes nothing,
	// and an operator sent to the seat first finds that out the slow way.
	// And it puts the compensation on the path a seat refusal ALWAYS
	// takes, rather than on a race between two nodes: an unwind that only
	// a race can reach is an unwind nothing ever proves works.
	//
	// What the compensation cannot cover is a process that dies between
	// the two writes. The org is then over-stated by one round in the
	// windows it was charged in, which trips a cap EARLY — the fail-closed
	// direction, bounded by how often a node dies mid-charge and by the
	// windows turning over, and visible in the counter rather than
	// silently absorbed.
	//
	// FAILS CLOSED: an error stops the round. It is NOT a refusal, and a
	// caller must not report it as one — "the company is out of tokens"
	// is a budget event an operator acts on, and "the counter is
	// unreachable" is an outage. Money leaves the building for every
	// token, so a counter that cannot be reached must not un-cap a
	// company. A request that does not validate ([ChargeRequest.Validate])
	// is an error too, never a refusal and never an admission.
	//
	// A charge of zero tokens or fewer is neither a charge nor an error:
	// it writes nothing and answers OK. A phase whose provider reported no
	// usage still ran, and refusing it would stop a company over a backend
	// that omits the field.
	//
	// A refusal stamps the refusing windows' [WindowUsage.RefusedAt], and
	// an admitted charge clears every stamp on both scopes it charged. See
	// that field for why nothing weaker clears one.
	Charge(ctx context.Context, req ChargeRequest) (Spend, error)

	// PostCharge adds spend that has ALREADY HAPPENED to the seat's counter
	// and the org's, in the given windows, and never refuses. The answer
	// is OK with both counters after the write, for the caller to compare
	// with its caps — except for a charge of nothing, which writes nothing
	// and answers OK with both counters empty rather than reading two
	// counters to report what it did not change.
	//
	// Charge is the gate: it decides whether a round may run, before the
	// round has spent anything. Some spend is only known after it happened
	// (a detached coding run is collected minutes or hours after it
	// started, possibly on another node), and no answer can un-spend it.
	// Put through the gate, it was recorded NOT AT ALL whenever it did not
	// fit, which is exactly when a cap binds: the counter under-stated the
	// company's spend by the whole run, and the next round was admitted
	// against room the run had already used. The windows are the ones the
	// spend is COLLECTED in, which is the only instant the store is told
	// about.
	//
	// It leaves both scopes' refusal stamps alone, because it is not a
	// decision about room: it neither says the gate turned a charge away
	// nor that it had room for one. A counter it takes past a cap is
	// refused by the next Charge, which stamps it then.
	//
	// All or nothing, as Charge is: an error takes the org's half back, so
	// a caller that retries does not count the company twice. The
	// compensation is the same BEST-EFFORT one Charge's is — two keys and
	// no transaction — and a backend that cannot make it says so in its log
	// rather than in the answer, because the caller's answer is already
	// decided. It errs in the one safe direction: the org reads HIGH, so a
	// cap trips early rather than late.
	PostCharge(ctx context.Context, seat string, tokens int, windows Windows) (Spend, error)

	// PostChargeOrg adds spend that has ALREADY HAPPENED to the COMPANY's
	// counter alone, in the given windows, and never refuses. The answer is
	// the company's counter after the write — or an empty [Usage] for a
	// charge of nothing, which writes nothing and reads nothing, exactly as
	// PostCharge answers one.
	//
	// It exists for the spend nobody's SEAT made: a person asking the
	// company's knowledge a question from the dashboard (the operator
	// surface's `answer_knowledge`). A person has no agent scope and no
	// seat ceiling — `token_budget` is on the company and on roles that
	// run turns — so there is no seat half to charge, and inventing a
	// scope for one would put a row in every listing that no ceiling can
	// ever judge. The company's windows are what that spend is judged by,
	// and a counter that did not hear about it would hand the next turn
	// room the answer had already used.
	//
	// AFTER THE FACT for PostCharge's reason: an answer is one model call
	// whose size is known only from its reply. The caller gates BEFORE the
	// call by reading the company's counter ([Budgets.Used]); this records
	// what the call cost, past a ceiling included, and — like PostCharge —
	// leaves the refusal stamps alone, because it is not a decision about
	// room.
	PostChargeOrg(ctx context.Context, tokens int, windows Windows) (Usage, error)

	// Used reports one scope's counter against the given windows. A scope
	// never charged reads [Unspent]; an unreachable store is an error,
	// never a zero.
	Used(ctx context.Context, scope string, windows Windows) (Usage, error)

	// Usage returns every counter the store holds, read against the given
	// windows, org first then seats by scope.
	//
	// Ordered so the operator surface does not have to sort, and so two
	// reads of an unchanged counter are byte-identical — a listing that
	// reshuffled would make a diff of two captures unreadable. A scope
	// whose windows have all turned over since its last charge is listed
	// at zero until its record ages out, which on the KV backend is
	// [BudgetRetention] after its last write.
	Usage(ctx context.Context, windows Windows) ([]Usage, error)
}

// LifetimeCounters is the retirement of the counters an earlier build kept: one
// figure per scope for the life of a deployment, in a bucket with no age.
//
// Those counters are READ BY NOTHING in a build at [WindowedCountersProtocol]
// or later, and they are not migrated — a lifetime total has no window to be
// counted in. But a build before it charges them for as long as it runs, and a
// store that deleted them under a live older node would fail its every charge
// closed. So the retirement is a DECISION rather than a boot step: the
// maintenance duty takes it once no live lease is held below
// WindowedCountersProtocol (internal/maintenance.RetiredBudgetJobs).
type LifetimeCounters interface {
	// RetireLifetimeCounters deletes the lifetime counters, reporting
	// whether there were any to delete. Retiring what is not there is not
	// an error, so the duty's every later tick is a no-op.
	RetireLifetimeCounters(ctx context.Context) (bool, error)
}

// ---- the arithmetic, shared by every backend ------------------------------ //

// Tally is one scope's counter as a backend stores it: a slot per period, and
// when the counter last moved.
//
// PURE ARITHMETIC OVER VALUES, exported so both backends run the one
// implementation of the rules above — the roll, the fit, the stamps and which
// window a refusal names — and differ only in how they make a read and a write
// atomic. A rule a backend spelled for itself is a rule the twin and the store
// a fleet runs on can disagree about, which is the divergence the contract
// suite exists to catch and cannot always reach. Every method returns a new
// Tally and leaves its receiver alone.
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
// and not refusing. A slot on w's window is kept, and so is one on a LATER
// window: a slot never rolls back (see the file doc).
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
// windows.
func (t Tally) Refusing(tokens int, caps Caps) []period.Period {
	var out []period.Period
	for i, p := range period.Periods {
		if ceiling, capped := caps[p]; capped && t.Slots[i].Used+tokens > ceiling {
			out = append(out, p)
		}
	}
	return out
}

// Add counts delta in every slot, moving the counter's clock to at. The tally
// must already be rolled to the charge's windows, which is what makes every
// slot the current one.
//
// A negative delta takes spend back and is floored at zero, so a compensation
// for a charge whose own write was already reaped cannot leave a counter that
// reads as credit.
func (t Tally) Add(delta int, at time.Time) Tally {
	for i := range t.Slots {
		t.Slots[i].Used = max(t.Slots[i].Used+delta, 0)
	}
	t.At = at
	return t
}

// Undo takes back a charge of tokens from the slots it was counted in: those
// still on the window charged holds. A slot that has since rolled on is left
// alone, because what the charge spent belongs to a window that is over, and
// taking it from the next one would hand that window credit.
func (t Tally) Undo(tokens int, charged Tally, at time.Time) Tally {
	for i := range t.Slots {
		if t.Slots[i].Label == charged.Slots[i].Label {
			t.Slots[i].Used = max(t.Slots[i].Used-tokens, 0)
		}
	}
	t.At = at
	return t
}

// Stamp records at as the last refusal of each period in periods, on the slots
// still on the window refused holds for it. A slot that has rolled on since
// the refusal is left alone: the refusal was of a window that is over.
//
// It does not move the counter's clock, so a scope known only for a refusal
// has no charge time.
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
// IT: rolled to w first, so a slot on w's window reads as it stands, a slot on
// an earlier window reads as w's window unspent, and a slot already on a LATER
// window reads as that later window — its spend and its refusal, never zero.
//
// The later window is the case that makes this a rule rather than a detail. A
// slot never rolls back, so the charge that follows is REFUSED against the
// later window's spend; a read that answered w's window unspent there would
// hand every reader of the counter — the headroom a turn is given, the
// learning gate, the budgets answer, the live meter — room the gate will not
// admit. For a peer's clock a few seconds ahead that is a few seconds; after
// the company's zone moves west (ADR-0018 allows a live edit) the slot leads
// the company's own clock by up to a day.
func (t Tally) Usage(scope string, w Windows) Usage {
	rolled := t.Roll(w)
	u := Usage{Scope: scope, UpdatedAt: t.At}
	for i, s := range rolled.Slots {
		u.Windows[i] = WindowUsage{Window: rolled.window(i, w), Used: s.Used, RefusedAt: s.RefusedAt}
	}
	return u
}

// window is the window slot i counts, on w's clock: w's own when the slot is
// on it, and the later window a slot that leads w is on. The tally must
// already be rolled to w, so no slot is on an earlier window.
//
// A later label that does not parse — which only a record this build did not
// write can hold — is named as w's window rather than as no window at all, so
// a reader still gets instants to state; the spend it pairs with is the slot's,
// which is what the gate decides with.
func (t Tally) window(i int, w Windows) period.Window {
	window := w[i]
	if label := t.Slots[i].Label; label != window.Label {
		if later, err := period.Parse(window.Period, label, window.Start.Location()); err == nil {
			return later
		}
	}
	return window
}

// Refusal is the answer a scope's refusal of tokens gives: the refusing window
// that ends last, what it had spent and its ceiling. Windows that end together
// name the longer period.
//
// scope is the name the answer reports ("org" or "agent"), periods what
// [Tally.Refusing] found, and the tally the one rolled to w that it was found
// on. A slot a peer's clock already moved onto a later window is named as that
// window, read on w's clock.
//
// The choice turns on each window's END and, at a tie, on the period's length
// — never on the order periods arrive in, so a caller that listed them in any
// other order still gets the answer every backend gives. It is [outlasts], the
// rule [Outlasts] exports to every caller choosing among windows.
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
