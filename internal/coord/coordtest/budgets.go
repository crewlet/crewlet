package coordtest

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/period"
)

// ---- the token counters ------------------------------------------------- //

const testSeat = "agent:11111111-1111-1111-1111-111111111111"

// windows is the suite's current windows: the day, week and month of the
// harness's fixed instant, in UTC.
func (h *fleetHarness) windows() coord.Windows {
	return coord.WindowsAt(h.now(), time.UTC)
}

// day caps the day window alone, which is what every case that is about the
// counter rather than the calendar charges against: the protocol is the same
// in every window, and one window keeps the arithmetic readable.
func day(n int) coord.Caps { return coord.Caps{period.Day: n} }

func (h *fleetHarness) charge(seat string, tokens int, org, seatCaps coord.Caps) coord.Spend {
	h.t.Helper()
	return h.chargeAt(h.windows(), seat, tokens, org, seatCaps)
}

func (h *fleetHarness) chargeAt(w coord.Windows, seat string, tokens int, org, seatCaps coord.Caps) coord.Spend {
	h.t.Helper()
	got, err := h.f.Charge(h.ctx, coord.ChargeRequest{
		Seat: seat, Tokens: tokens, Windows: w, OrgCaps: org, SeatCaps: seatCaps,
	})
	if err != nil {
		h.t.Fatalf("Charge(%s, %d): %v", seat, tokens, err)
	}
	return got
}

// used is a scope's spend in the day window of the suite's instant.
func (h *fleetHarness) used(scope string) int {
	h.t.Helper()
	return h.usedAt(h.windows(), scope).In(period.Day).Used
}

func (h *fleetHarness) usedAt(w coord.Windows, scope string) coord.Usage {
	h.t.Helper()
	got, err := h.f.Used(h.ctx, scope, w)
	if err != nil {
		h.t.Fatalf("Used(%s): %v", scope, err)
	}
	if got.Scope != scope {
		h.t.Fatalf("Used(%s) answered for scope %q", scope, got.Scope)
	}
	return got
}

// spent is a scope's spend in each of the day, week and month of w.
func (h *fleetHarness) spent(w coord.Windows, scope string) [3]int {
	h.t.Helper()
	u := h.usedAt(w, scope)
	var out [3]int
	for i, p := range period.Periods {
		slot := u.In(p)
		if slot.Window.Label != w[i].Label {
			h.t.Fatalf("Used(%s) read the %s window %q, asked about %q",
				scope, p, slot.Window.Label, w[i].Label)
		}
		out[i] = slot.Used
	}
	return out
}

// usage is one scope's row of the listing at the suite's windows, and whether
// it is listed at all.
func (h *fleetHarness) usage(scope string) (coord.Usage, bool) {
	h.t.Helper()
	rows, err := h.f.Usage(h.ctx, h.windows())
	if err != nil {
		h.t.Fatalf("Usage: %v", err)
	}
	for _, row := range rows {
		if row.Scope == scope {
			return row, true
		}
	}
	return coord.Usage{}, false
}

// refusedAt is a scope's refusal stamp in one period's window, failing when it
// is absent or when it does not fall inside the interval the caller observed
// around the refusal.
//
// AN INTERVAL rather than an instant, because Charge takes no clock for the
// stamp: the backend stamps the wall clock, and the property worth pinning is
// that the stamp is the time of THIS refusal rather than zero, a placeholder,
// or a stale one.
func (h *fleetHarness) refusedAt(scope string, p period.Period, from, to time.Time) time.Time {
	h.t.Helper()
	row, listed := h.usage(scope)
	if !listed {
		h.t.Fatalf("%s is not listed at all, so its refusal was not recorded", scope)
	}
	stamp := row.In(p).RefusedAt
	if stamp.IsZero() {
		h.t.Fatalf("%s carries no %s refusal stamp: %+v", scope, p, row)
	}
	if stamp.Before(from.Add(-time.Second)) || stamp.After(to.Add(time.Second)) {
		h.t.Fatalf("%s %s refusal stamp %v is outside the refusal's own interval [%v, %v]",
			scope, p, stamp, from, to)
	}
	return stamp
}

// refusing reports which periods of a scope carry a refusal stamp.
func (h *fleetHarness) refusing(scope string) []period.Period {
	h.t.Helper()
	row, _ := h.usage(scope)
	var out []period.Period
	for _, p := range period.Periods {
		if !row.In(p).RefusedAt.IsZero() {
			out = append(out, p)
		}
	}
	return out
}

// AWindowRollsAtItsBoundary is ADR-0019's gate, and the one budget case a
// backend's own test names: a window's allowance comes back exactly when the
// window turns over — at local midnight, on Monday, on the 1st — and at
// nothing else, with no reset and on the company's own clock.
//
// Exported so the decision's named test runs it directly; [RunFleet] runs it
// against every backend as one of its budget cases.
func AWindowRollsAtItsBoundary(t *testing.T, f coord.Fleet) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), stallBudget)
	defer cancel()
	windowRollover(&fleetHarness{t: t, ctx: ctx, f: f})
}

// windowRollover walks one seat across every kind of boundary on a clock that
// is not UTC, through a spring-forward day.
func windowRollover(h *fleetHarness) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		h.t.Fatalf("load America/New_York: %v", err)
	}
	at := func(y int, m time.Month, d, hh, mm int) coord.Windows {
		return coord.WindowsAt(time.Date(y, m, d, hh, mm, 0, 0, ny), ny)
	}
	caps := coord.Caps{period.Day: 300, period.Week: 450, period.Month: 800}
	charge := func(w coord.Windows, tokens int) coord.Spend {
		h.t.Helper()
		return h.chargeAt(w, testSeat, tokens, caps, nil)
	}
	want := func(what string, w coord.Windows, dayUsed, weekUsed, monthUsed int) {
		h.t.Helper()
		if got := h.spent(w, coord.OrgScope); got != [3]int{dayUsed, weekUsed, monthUsed} {
			h.t.Fatalf("%s: org day/week/month = %v, want [%d %d %d]",
				what, got, dayUsed, weekUsed, monthUsed)
		}
	}
	refused := func(what string, got coord.Spend, p period.Period, w coord.Windows) {
		h.t.Helper()
		if got.OK || got.RefusedScope != "org" || got.RefusedPeriod != p {
			h.t.Fatalf("%s: %+v, want the org refusing in its %s window", what, got, p)
		}
		if i := slices.Index(period.Periods[:], p); got.RefusedWindow.Label != w[i].Label {
			h.t.Fatalf("%s: refused in window %q, want %q", what, got.RefusedWindow.Label, w[i].Label)
		}
	}

	// Saturday 7 March, half an hour before midnight in New York — 04:30 on
	// the 8th in UTC, so a counter cut on UTC would already be on Sunday.
	saturday := at(2026, time.March, 7, 23, 30)
	if got := charge(saturday, 300); !got.OK {
		h.t.Fatalf("Saturday's first charge was refused: %+v", got)
	}
	refused("Saturday's day is full", charge(saturday, 1), period.Day, saturday)
	want("Saturday", saturday, 300, 300, 300)

	// LOCAL MIDNIGHT, on the day the clocks spring forward: a 23-hour day.
	// The day's allowance is back, and nothing else moved — Sunday is in the
	// same ISO week and the same month.
	sunday := at(2026, time.March, 8, 0, 30)
	if got := charge(sunday, 150); !got.OK {
		h.t.Fatalf("Sunday's first charge was refused, so the day did not roll at "+
			"local midnight: %+v", got)
	}
	want("Sunday", sunday, 150, 450, 450)
	if stamps := h.refusingAt(sunday, coord.OrgScope); len(stamps) != 0 {
		h.t.Fatalf("Sunday reads as refusing in %v: Saturday's refusal belonged to a "+
			"window that is over", stamps)
	}
	// The week is full although the day is not: the refusal names the week.
	refused("Sunday's week is full", charge(sunday, 1), period.Week, sunday)

	// MONDAY: the day and the week roll together, the month does not.
	monday := at(2026, time.March, 9, 0, 0)
	if got := charge(monday, 300); !got.OK {
		h.t.Fatalf("Monday's first charge was refused, so the week did not roll: %+v", got)
	}
	want("Monday", monday, 300, 300, 750)

	// The last second of March: a fresh day and a fresh week (Monday the
	// 30th began it), and a month with 50 tokens left.
	lastOfMarch := coord.WindowsAt(time.Date(2026, time.March, 31, 23, 59, 59, 0, ny), ny)
	refused("March is nearly spent", charge(lastOfMarch, 100), period.Month, lastOfMarch)
	want("the 31st", lastOfMarch, 0, 0, 750)

	// THE 1ST: the day and the month roll, the week does not.
	april := at(2026, time.April, 1, 0, 0)
	if got := charge(april, 100); !got.OK {
		h.t.Fatalf("the 1st's first charge was refused, so the month did not roll: %+v", got)
	}
	want("the 1st", april, 100, 100, 100)
}

// refusingAt is which periods of a scope carry a refusal stamp, read against w.
func (h *fleetHarness) refusingAt(w coord.Windows, scope string) []period.Period {
	h.t.Helper()
	u := h.usedAt(w, scope)
	var out []period.Period
	for _, p := range period.Periods {
		if !u.In(p).RefusedAt.IsZero() {
			out = append(out, p)
		}
	}
	return out
}

var budgetCases = []fleetCase{{
	// The reason this moved off the node's own database. Four nodes on one
	// company each kept their own counter, so a ceiling of 500 000 was
	// silently four times that — and the config number was decoration.
	name: "one counter however many callers share it",
	fn: func(h *fleetHarness) {
		for i := range 4 {
			if got := h.charge(testSeat, 100, day(500), nil); !got.OK {
				h.t.Fatalf("charge %d was refused inside the cap: %+v", i+1, got)
			}
		}
		if got := h.charge(testSeat, 100, day(500), nil); !got.OK {
			h.t.Fatalf("the charge that exactly fills the cap was refused: %+v", got)
		}
		got := h.charge(testSeat, 1, day(500), nil)
		if got.OK {
			h.t.Fatal("a charge past the org cap was accepted")
		}
		if got.RefusedScope != "org" || got.RefusedLimit != 500 || got.RefusedUsed != 500 {
			h.t.Fatalf("refusal = %+v, want the org scope at 500 of its 500", got)
		}
		if h.used(coord.OrgScope) != 500 {
			h.t.Fatalf("org spend = %d, want the 500 that fit", h.used(coord.OrgScope))
		}
	},
}, {
	name: "a charge is counted in every window at once",
	fn: func(h *fleetHarness) {
		// Uncapped windows included: a ceiling added to the week later has
		// to find what the week already spent, not a counter that started
		// counting the moment somebody capped it.
		h.charge(testSeat, 70, day(1000), nil)
		h.charge(testSeat, 30, nil, nil)
		w := h.windows()
		for _, scope := range []string{coord.OrgScope, testSeat} {
			if got := h.spent(w, scope); got != [3]int{100, 100, 100} {
				h.t.Fatalf("%s day/week/month = %v, want 100 in each", scope, got)
			}
		}
	},
}, {
	name: "a scope that caps no window is unlimited",
	fn: func(h *fleetHarness) {
		// An absent period is the only uncapped one. Most companies set no
		// budget at all, and every one of their rounds comes through here.
		if got := h.charge(testSeat, 1_000_000, nil, nil); !got.OK {
			h.t.Fatalf("an uncapped charge was refused: %+v", got)
		}
		if h.used(coord.OrgScope) != 1_000_000 {
			h.t.Fatal("an uncapped charge was not counted")
		}
	},
}, {
	name: "a ceiling below one token is an error, never a refusal or an admission",
	fn: func(h *fleetHarness) {
		// 0 meant "unlimited" to the lifetime counter and means "nothing may
		// be spent" to anyone reading the word ceiling. A backend that read
		// it either way would decide, on its own, which company the caller
		// meant.
		for _, caps := range []coord.Caps{{period.Day: 0}, {period.Week: -5}, {"fortnight": 10}} {
			got, err := h.f.Charge(h.ctx, coord.ChargeRequest{
				Seat: testSeat, Tokens: 10, Windows: h.windows(), SeatCaps: caps,
			})
			if err == nil {
				h.t.Fatalf("caps %v were accepted: %+v", caps, got)
			}
		}
		if _, listed := h.usage(coord.OrgScope); listed {
			h.t.Fatal("a request refused as malformed still charged the org")
		}
	},
}, {
	name: "a charge that names no windows is an error, never a charge",
	fn: func(h *fleetHarness) {
		// A zero Window has no label, and a slot rolled to an empty label
		// reads as never charged: a caller that forgot to cut its windows
		// would hand every scope its whole allowance on every charge.
		for name, w := range map[string]coord.Windows{
			"none at all":   {},
			"out of order":  {h.windows()[2], h.windows()[1], h.windows()[0]},
			"a missing day": {{}, h.windows()[1], h.windows()[2]},
		} {
			if _, err := h.f.Charge(h.ctx, coord.ChargeRequest{
				Seat: testSeat, Tokens: 10, Windows: w, OrgCaps: day(100),
			}); err == nil {
				h.t.Fatalf("a charge with %s was accepted", name)
			}
			if _, err := h.f.PostCharge(h.ctx, testSeat, 10, w); err == nil {
				h.t.Fatalf("a post-charge with %s was accepted", name)
			}
		}
		if _, listed := h.usage(coord.OrgScope); listed {
			h.t.Fatal("a request refused as malformed still charged the org")
		}
	},
}, {
	name: "a refused seat leaves the org uncharged",
	fn: func(h *fleetHarness) {
		// The property a single SQL transaction used to give for free,
		// and the whole reason this contract cannot be two calls:
		// charging the company for a turn that never ran lets it
		// exhaust its budget on work it did not do. On a KV backend
		// there is no transaction, so the org bump made a moment ago
		// has to be UNWOUND by hand — this is the case that proves it.
		if got := h.charge(testSeat, 90, nil, day(100)); !got.OK {
			h.t.Fatalf("the first charge was refused: %+v", got)
		}
		before := h.spent(h.windows(), coord.OrgScope)
		got := h.charge(testSeat, 90, nil, day(100))
		if got.OK {
			h.t.Fatal("a charge past the seat cap was accepted")
		}
		if got.RefusedScope != "agent" {
			h.t.Fatalf("refusal = %+v, want the agent scope", got)
		}
		// Every window, the uncapped ones included: the unwind takes the
		// charge back from each window it was counted in.
		if after := h.spent(h.windows(), coord.OrgScope); after != before {
			h.t.Fatalf("org spend moved from %v to %v on a REFUSED turn", before, after)
		}
	},
}, {
	name: "a refused org leaves the seat uncharged",
	fn: func(h *fleetHarness) {
		// The other direction. Nothing to unwind here — the org is
		// charged first, so its refusal happens before the seat is
		// touched — which is exactly the property being pinned: a
		// backend that wrote the seat first would burn a seat's own
		// allowance on turns the company refused.
		if got := h.charge(testSeat, 90, day(100), day(1000)); !got.OK {
			h.t.Fatalf("the first charge was refused: %+v", got)
		}
		before := h.used(testSeat)
		got := h.charge(testSeat, 90, day(100), day(1000))
		if got.OK {
			h.t.Fatal("a charge past the org cap was accepted")
		}
		if got.RefusedScope != "org" {
			h.t.Fatalf("refusal = %+v, want the org scope", got)
		}
		if after := h.used(testSeat); after != before {
			h.t.Fatalf("seat spend moved from %d to %d on a REFUSED turn", before, after)
		}
	},
}, {
	name: "an exhausted company is reported before an exhausted seat",
	fn: func(h *fleetHarness) {
		// Both scopes are out of room. "The company is out" is the fact
		// that matters: raising this seat's ceiling against an
		// exhausted org changes nothing, and an operator sent to the
		// seat first finds that out the slow way.
		if got := h.charge(testSeat, 100, day(100), day(100)); !got.OK {
			h.t.Fatalf("the first charge was refused: %+v", got)
		}
		got := h.charge(testSeat, 50, day(100), day(100))
		if got.OK {
			h.t.Fatal("a charge past both caps was accepted")
		}
		if got.RefusedScope != "org" {
			h.t.Fatalf("refusal = %+v, want the org scope reported first", got)
		}
	},
}, {
	name: "an exhausted company is reported before a charge the seat cap can never hold",
	fn: func(h *fleetHarness) {
		// The same ordering rule, reached through the other door. A charge
		// larger than the seat's WHOLE cap is screened before anything is
		// written, and a screen that tested each scope's cap alone named
		// the seat while the company was out of room too: the org holds 90
		// of 100 and 50 more fits neither. An operator sent to raise the
		// seat's ceiling would raise it and still be refused.
		if got := h.charge(testSeat, 90, day(100), nil); !got.OK {
			h.t.Fatalf("the first charge was refused: %+v", got)
		}
		got := h.charge(testSeat, 50, day(100), day(40))
		if got.OK {
			h.t.Fatal("a charge past both caps was accepted")
		}
		if got.RefusedScope != "org" || got.RefusedUsed != 90 || got.RefusedLimit != 100 {
			h.t.Fatalf("refusal = %+v, want the org scope at 90 of 100", got)
		}
		if h.used(coord.OrgScope) != 90 || h.used(testSeat) != 90 {
			h.t.Fatal("a refused charge still moved a counter")
		}
	},
}, {
	name: "a charge the seat cap can never hold names the seat while the company has room",
	fn: func(h *fleetHarness) {
		// The counterpart, so the case above cannot be passed by naming
		// the org for every oversized charge.
		got := h.charge(testSeat, 50, day(100), day(40))
		if got.OK {
			h.t.Fatal("a charge larger than the seat's whole cap was accepted")
		}
		if got.RefusedScope != "agent" || got.RefusedLimit != 40 || got.RefusedPeriod != period.Day {
			h.t.Fatalf("refusal = %+v, want the agent scope's day window and its limit", got)
		}
		if h.used(coord.OrgScope) != 0 || h.used(testSeat) != 0 {
			h.t.Fatal("a refused charge still moved a counter")
		}
	},
}, {
	name: "a charge larger than the whole cap is refused before anything is written",
	fn: func(h *fleetHarness) {
		// The first-ever charge, against an empty counter. A backend
		// that only checked "existing + delta" on an UPDATE path would
		// accept this one, because there is nothing to update yet.
		got := h.charge(testSeat, 1_000_000, day(10), nil)
		if got.OK {
			h.t.Fatal("a charge larger than the entire cap was accepted")
		}
		if got.RefusedScope != "org" {
			h.t.Fatalf("refusal = %+v, want the org scope", got)
		}
		if h.used(coord.OrgScope) != 0 || h.used(testSeat) != 0 {
			h.t.Fatal("a refused charge still moved a counter")
		}
	},
}, {
	name: "a zero-token charge is neither an error nor a charge",
	fn: func(h *fleetHarness) {
		// A phase whose provider reported no usage still RAN. Refusing
		// it would stop a company over a backend that omits the field.
		if got := h.charge(testSeat, 0, day(10), day(10)); !got.OK {
			h.t.Fatalf("a zero-token charge was refused: %+v", got)
		}
		if _, listed := h.usage(coord.OrgScope); listed {
			h.t.Fatal("a zero-token charge created a counter")
		}
	},
}, {
	name: "two seats spend the org's allowance together",
	fn: func(h *fleetHarness) {
		// Per-SEAT counters are separate; the org's is not. A backend
		// that keyed the org counter per caller would let each seat
		// spend the whole company allowance.
		other := "agent:22222222-2222-2222-2222-222222222222"
		if got := h.charge(testSeat, 60, day(100), nil); !got.OK {
			h.t.Fatalf("the first seat was refused: %+v", got)
		}
		got := h.charge(other, 60, day(100), nil)
		if got.OK {
			h.t.Fatal("a second seat spent the org allowance the first had already spent")
		}
		if h.used(other) != 0 {
			h.t.Fatal("a refused seat was charged")
		}
	},
}, {
	name: "usage lists the org first, then the seats",
	fn: func(h *fleetHarness) {
		// The operator surface does not sort, and "org" does NOT sort
		// before "agent:…" alphabetically — so a backend that left the
		// order to its own collation would put the company's counter
		// in the middle of its seats.
		h.charge("agent:zzzz", 10, nil, nil)
		h.charge("agent:aaaa", 20, nil, nil)
		rows, err := h.f.Usage(h.ctx, h.windows())
		if err != nil {
			h.t.Fatalf("Usage: %v", err)
		}
		var scopes []string
		for _, row := range rows {
			scopes = append(scopes, row.Scope)
		}
		want := []string{coord.OrgScope, "agent:aaaa", "agent:zzzz"}
		if !slices.Equal(scopes, want) {
			h.t.Fatalf("scopes = %v, want %v", scopes, want)
		}
		for _, row := range rows {
			if row.Scope == coord.OrgScope && row.In(period.Month).Used != 30 {
				h.t.Fatalf("org used = %d, want both seats' spend", row.In(period.Month).Used)
			}
		}
	},
}, {
	name: "an unspent scope has spent nothing rather than erroring",
	fn: func(h *fleetHarness) {
		w := h.windows()
		if got := h.spent(w, "agent:never-ran"); got != [3]int{} {
			h.t.Fatalf("used = %v for a scope never charged, want nothing in any window", got)
		}
	},
}, {
	name: "a refusal is recorded on the scope that refused and on no other",
	fn: func(h *fleetHarness) {
		// "Exhausted" is a refusal, never used >= max: a refused charge
		// increments nothing, so a counter stalls short of its cap by the
		// size of the round that did not fit. The stamp is the only honest
		// record of the gate saying no, and it belongs to the scope that
		// said it.
		if got := h.charge(testSeat, 90, day(100), nil); !got.OK {
			h.t.Fatalf("the first charge was refused: %+v", got)
		}
		from := time.Now()
		if got := h.charge(testSeat, 50, day(100), nil); got.RefusedScope != "org" {
			h.t.Fatalf("refusal = %+v, want the org scope", got)
		}
		h.refusedAt(coord.OrgScope, period.Day, from, time.Now())
		if stamps := h.refusing(testSeat); len(stamps) != 0 {
			h.t.Fatalf("the seat carries a refusal the company made, in %v", stamps)
		}
		if h.used(coord.OrgScope) != 90 {
			h.t.Fatalf("org used = %d after a refusal, want the 90 it held", h.used(coord.OrgScope))
		}

		other := "agent:44444444-4444-4444-4444-444444444444"
		from = time.Now()
		if got := h.charge(other, 5, day(100), day(4)); got.RefusedScope != "agent" {
			h.t.Fatalf("refusal = %+v, want the agent scope", got)
		}
		h.refusedAt(other, period.Day, from, time.Now())
		if row, _ := h.usage(other); row.In(period.Day).Used != 0 || !row.UpdatedAt.IsZero() {
			// Listed, because a seat refused on its first charge has
			// refused one; but it has been charged nothing and never.
			h.t.Fatalf("a scope known only for its refusal reads %+v, want no spend and no charge time", row)
		}
	},
}, {
	name: "a refusal is recorded on the window that refused and on no other",
	fn: func(h *fleetHarness) {
		// "Out for the week" and "out for the day" send an operator to
		// different places — one waits hours, the other days — and a stamp
		// on every window would make each read like the other.
		caps := coord.Caps{period.Day: 1000, period.Week: 150, period.Month: 5000}
		if got := h.charge(testSeat, 100, caps, nil); !got.OK {
			h.t.Fatalf("the first charge was refused: %+v", got)
		}
		from := time.Now()
		got := h.charge(testSeat, 100, caps, nil)
		if got.OK || got.RefusedPeriod != period.Week || got.RefusedLimit != 150 || got.RefusedUsed != 100 {
			h.t.Fatalf("refusal = %+v, want the org's week at 100 of 150", got)
		}
		if want := h.windows()[1].Label; got.RefusedWindow.Label != want {
			h.t.Fatalf("refused in window %q, want the current week %q", got.RefusedWindow.Label, want)
		}
		h.refusedAt(coord.OrgScope, period.Week, from, time.Now())
		if stamps := h.refusing(coord.OrgScope); !slices.Equal(stamps, []period.Period{period.Week}) {
			h.t.Fatalf("the org is stamped in %v, want the week only", stamps)
		}
	},
}, {
	name: "a refusal names the refusing window that ends last",
	fn: func(h *fleetHarness) {
		// A scope refused by two windows can spend again only when BOTH have
		// turned over, so the answer names the one that ENDS LAST — decided
		// by its end, never by where its period sits in the list. Naming an
		// earlier one would send a caller waiting for a turnover that
		// changes nothing. Three instants pin it from both sides and at the
		// tie: mid-month the day, listed FIRST, ends before the month; on
		// Tuesday 31 March the month, listed LAST, ends before the week (on
		// Sunday 5 April); and on Sunday 31 May all three end at the same
		// midnight, where the longest period is named so that every backend
		// gives one answer. A backend that named the first refusing period,
		// or the last, fails one of the first two.
		//
		// In calendar order, because the org's counter is shared by all
		// three and a slot never rolls back.
		for _, tc := range []struct {
			what string
			at   time.Time
			caps coord.Caps
			want period.Period
		}{{
			what: "Tuesday 31 March, the week and the month full",
			at:   time.Date(2026, time.March, 31, 12, 0, 0, 0, time.UTC),
			caps: coord.Caps{period.Week: 100, period.Month: 100},
			want: period.Week,
		}, {
			what: "Sunday 31 May, all three full and ending together",
			at:   time.Date(2026, time.May, 31, 12, 0, 0, 0, time.UTC),
			caps: coord.Caps{period.Day: 100, period.Week: 100, period.Month: 100},
			want: period.Month,
		}, {
			what: "Wednesday 16 September, the day and the month full",
			at:   time.Date(2026, time.September, 16, 12, 0, 0, 0, time.UTC),
			caps: coord.Caps{period.Day: 100, period.Month: 100},
			want: period.Month,
		}} {
			w := coord.WindowsAt(tc.at, time.UTC)
			if got := h.chargeAt(w, testSeat, 100, tc.caps, nil); !got.OK {
				h.t.Fatalf("%s: the first charge was refused: %+v", tc.what, got)
			}
			got := h.chargeAt(w, testSeat, 1, tc.caps, nil)
			i := slices.Index(period.Periods[:], tc.want)
			if got.OK || got.RefusedScope != "org" || got.RefusedPeriod != tc.want ||
				got.RefusedWindow.Label != w[i].Label {
				h.t.Fatalf("%s: refusal = %+v, want the org's %s %q, the refusing window that ends last",
					tc.what, got, tc.want, w[i].Label)
			}
			if !got.RefusedWindow.End.Equal(w[i].End) || got.RefusedUsed != 100 || got.RefusedLimit != 100 {
				h.t.Fatalf("%s: refusal = %+v, want the %s ending %v at 100 of 100",
					tc.what, got, tc.want, w[i].End)
			}
			// And every full window was stamped, since every one refused —
			// naming one is not the same as recording only one.
			var capped []period.Period
			for _, p := range period.Periods {
				if _, ok := tc.caps[p]; ok {
					capped = append(capped, p)
				}
			}
			if stamps := h.refusingAt(w, coord.OrgScope); !slices.Equal(stamps, capped) {
				h.t.Fatalf("%s: the org is stamped in %v, want every full window %v", tc.what, stamps, capped)
			}
		}
	},
}, {
	name: "a window rolls at its boundary and at nothing else",
	fn:   windowRollover,
}, {
	name: "a slot never rolls back to an earlier window",
	fn: func(h *fleetHarness) {
		// Two nodes whose clocks straddle midnight. The one that is ahead
		// rolls the day; the one that is behind must count in the day the
		// counter is already on rather than roll it back — rolling back
		// would hand tomorrow its whole allowance again on every charge a
		// trailing clock makes.
		today := h.windows()
		tomorrow := coord.WindowsAt(today[0].End, time.UTC)
		if got := h.chargeAt(tomorrow, testSeat, 60, day(100), nil); !got.OK {
			h.t.Fatalf("the leading node's charge was refused: %+v", got)
		}
		if got := h.chargeAt(today, testSeat, 30, day(100), nil); !got.OK {
			h.t.Fatalf("the trailing node's charge was refused: %+v", got)
		}
		if got := h.usedAt(tomorrow, coord.OrgScope).In(period.Day).Used; got != 90 {
			h.t.Fatalf("tomorrow holds %d, want 90: the trailing charge rolled the day back "+
				"or went nowhere", got)
		}
		// Refused against the window the counter is on, and named as it.
		got := h.chargeAt(today, testSeat, 20, day(100), nil)
		if got.OK || got.RefusedWindow.Label != tomorrow[0].Label || got.RefusedUsed != 90 {
			h.t.Fatalf("refusal = %+v, want tomorrow %q at 90 of 100", got, tomorrow[0].Label)
		}
		// A read against the earlier day answers what that refusal was made
		// against — the later day, its spend and its refusal — and never the
		// earlier day unspent: a reader told there was room would hand a
		// turn headroom the gate refuses. That lasts a few seconds behind a
		// peer's clock, and up to a day after the company's zone moves west.
		read := h.usedAt(today, coord.OrgScope).In(period.Day)
		if read.Used != 90 || read.Window.Label != tomorrow[0].Label || read.RefusedAt.IsZero() {
			h.t.Fatalf("a read of the earlier day = %+v, want the later day %q at 90, refusing: "+
				"the counter holds the later day and the gate refuses against it", read, tomorrow[0].Label)
		}
	},
}, {
	name: "an admitted charge clears the refusals of both scopes it charged",
	fn: func(h *fleetHarness) {
		// A cap raised or a smaller round that fits: the scope has room
		// again, and a dashboard still saying "refusing charges" would send
		// an operator to fix what is already fixed.
		h.charge(testSeat, 95, day(100), nil)
		h.charge(testSeat, 50, day(100), nil) // org refuses
		h.charge(testSeat, 5, nil, day(4))    // seat refuses
		if len(h.refusing(coord.OrgScope)) == 0 {
			h.t.Fatal("setup: the org refusal was not recorded")
		}
		if len(h.refusing(testSeat)) == 0 {
			h.t.Fatal("setup: the seat refusal was not recorded")
		}
		if got := h.charge(testSeat, 5, day(100), day(200)); !got.OK {
			h.t.Fatalf("a charge that fits both caps was refused: %+v", got)
		}
		for _, scope := range []string{coord.OrgScope, testSeat} {
			if stamps := h.refusing(scope); len(stamps) != 0 {
				h.t.Fatalf("%s still reads as refusing in %v after an admitted charge", scope, stamps)
			}
		}
	},
}, {
	name: "a charge refused overall clears no refusal",
	fn: func(h *fleetHarness) {
		// The org would have had room for this charge and the SEAT refused
		// it. A backend that writes the org before testing the seat must
		// not let that write erase the company's own earlier refusal,
		// or whether the stamp survives depends on the order a
		// backend tests its scopes in.
		h.charge(testSeat, 90, day(100), nil)
		from := time.Now()
		h.charge(testSeat, 50, day(100), nil) // org refuses
		stamped := h.refusedAt(coord.OrgScope, period.Day, from, time.Now())

		got := h.charge(testSeat, 5, day(100), day(92)) // org has room, the seat does not
		if got.RefusedScope != "agent" {
			h.t.Fatalf("refusal = %+v, want the agent scope", got)
		}
		row, _ := h.usage(coord.OrgScope)
		if !row.In(period.Day).RefusedAt.Equal(stamped) {
			h.t.Fatalf("org refusal stamp = %v, want the %v it held: a charge refused "+
				"overall cleared it", row.In(period.Day).RefusedAt, stamped)
		}
		if row.In(period.Day).Used != 90 {
			h.t.Fatalf("org used = %d, want 90: the refused charge was not unwound", row.In(period.Day).Used)
		}
	},
}, {
	name: "a charge screened against a whole cap still records its refusal",
	fn: func(h *fleetHarness) {
		// The screen refuses before any counter is written. It is still
		// the gate saying no, and a backend that stamped only on the
		// compare-and-swap path would leave the largest refusals of all
		// invisible.
		from := time.Now()
		if got := h.charge(testSeat, 1_000_000, day(10), nil); got.RefusedScope != "org" {
			h.t.Fatalf("refusal = %+v, want the org scope", got)
		}
		h.refusedAt(coord.OrgScope, period.Day, from, time.Now())
		if h.used(coord.OrgScope) != 0 {
			h.t.Fatal("recording a refusal moved the counter")
		}
	},
}, {
	name: "concurrent charges add up",
	fn: func(h *fleetHarness) {
		// The property a compare-and-swap buys and a read-modify-write
		// does not. Without it N concurrent rounds count as one, and
		// the cap is whatever the last writer happened to see. Thirty-two
		// callers, so a backend's retry budget is exercised rather than
		// merely present.
		const callers = 32
		var wg sync.WaitGroup
		errs := make(chan error, callers)
		for range callers {
			wg.Go(func() {
				if _, err := h.f.Charge(h.ctx, coord.ChargeRequest{
					Seat: testSeat, Tokens: 10, Windows: h.windows(),
				}); err != nil {
					errs <- err
				}
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			h.t.Fatalf("a concurrent charge failed: %v", err)
		}
		if got := h.spent(h.windows(), coord.OrgScope); got != [3]int{callers * 10, callers * 10, callers * 10} {
			h.t.Fatalf("org spend = %v after %d concurrent charges of 10, want %d in every window",
				got, callers, callers*10)
		}
	},
}, {
	name: "concurrent charges never admit past a ceiling",
	fn: func(h *fleetHarness) {
		// The gate's half of the same property: a check and an increment
		// that were two steps would let every racing caller see room.
		const callers = 32
		var wg sync.WaitGroup
		for range callers {
			wg.Go(func() {
				_, _ = h.f.Charge(h.ctx, coord.ChargeRequest{
					Seat: testSeat, Tokens: 10, Windows: h.windows(), OrgCaps: day(100),
				})
			})
		}
		wg.Wait()
		if got := h.used(coord.OrgScope); got != 100 {
			h.t.Fatalf("org spend = %d after %d racing charges against a ceiling of 100, "+
				"want exactly 100", got, callers)
		}
	},
}, {
	name: "a post-charge records spend that overran both caps",
	fn: func(h *fleetHarness) {
		// The spend already happened (a detached coding run, collected
		// long after it started), so nothing can refuse it. Through the
		// gate it was recorded NOT AT ALL whenever it did not fit, which
		// is exactly when a cap binds, and the next round was admitted
		// against room the run had already used.
		if got := h.charge(testSeat, 90, day(100), day(100)); !got.OK {
			h.t.Fatalf("setup charge refused: %+v", got)
		}
		got, err := h.f.PostCharge(h.ctx, testSeat, 50, h.windows())
		if err != nil {
			h.t.Fatalf("PostCharge: %v", err)
		}
		if !got.OK || got.Org.In(period.Day).Used != 140 || got.Agent.In(period.Day).Used != 140 {
			h.t.Fatalf("post-charge = %+v, want OK at 140 on both counters", got)
		}
		if h.used(coord.OrgScope) != 140 || h.used(testSeat) != 140 {
			h.t.Fatal("the post-charge did not reach both counters")
		}
		if next := h.charge(testSeat, 1, day(100), day(100)); next.OK || next.RefusedScope != "org" {
			h.t.Fatalf("next charge = %+v, want the org to refuse against the recorded run", next)
		}
	},
}, {
	name: "a post-charge is counted in the windows it is collected in",
	fn: func(h *fleetHarness) {
		// A run launched yesterday and collected today spent today's
		// allowance as far as the store can know: it is told the windows
		// the spend was collected in and no other instant.
		today := h.windows()
		tomorrow := coord.WindowsAt(today[0].End, time.UTC)
		h.charge(testSeat, 90, day(100), nil)
		if _, err := h.f.PostCharge(h.ctx, testSeat, 50, tomorrow); err != nil {
			h.t.Fatalf("PostCharge: %v", err)
		}
		if got := h.usedAt(tomorrow, coord.OrgScope).In(period.Day).Used; got != 50 {
			h.t.Fatalf("tomorrow's day holds %d, want the 50 collected in it", got)
		}
	},
}, {
	name: "a post-charge leaves every refusal stamp as it was",
	fn: func(h *fleetHarness) {
		// It is not a decision about room, so it neither says the gate
		// turned a charge away nor that it had room for one: a refusing
		// company stays refusing, and a seat that never refused is not
		// stamped because a run took it past its cap.
		h.charge(testSeat, 90, day(100), nil)
		from := time.Now()
		h.charge(testSeat, 50, day(100), nil) // org refuses
		stamped := h.refusedAt(coord.OrgScope, period.Day, from, time.Now())

		if _, err := h.f.PostCharge(h.ctx, testSeat, 30, h.windows()); err != nil {
			h.t.Fatalf("PostCharge: %v", err)
		}
		if row, _ := h.usage(coord.OrgScope); !row.In(period.Day).RefusedAt.Equal(stamped) || row.In(period.Day).Used != 120 {
			h.t.Fatalf("org = %+v, want 120 used and the refusal stamped at %v kept", row, stamped)
		}
		if stamps := h.refusing(testSeat); len(stamps) != 0 {
			h.t.Fatalf("the seat reads as refusing in %v after a post-charge", stamps)
		}
	},
}, {
	name: "a post-charge of nothing records nothing, and one needs a seat",
	fn: func(h *fleetHarness) {
		if got, err := h.f.PostCharge(h.ctx, testSeat, 0, h.windows()); err != nil || !got.OK {
			h.t.Fatalf("PostCharge(0) = (%+v, %v), want OK", got, err)
		}
		if _, listed := h.usage(coord.OrgScope); listed {
			h.t.Fatal("a post-charge of zero created a counter")
		}
		if _, err := h.f.PostCharge(h.ctx, "", 5, h.windows()); err == nil {
			h.t.Fatal("a post-charge with no seat scope was accepted")
		}
		if _, listed := h.usage(coord.OrgScope); listed {
			h.t.Fatal("a post-charge with no seat scope still charged the org")
		}
	},
}, {
	name: "a post-charge of nothing answers nothing rather than reading the counters",
	fn: func(h *fleetHarness) {
		if _, err := h.f.PostCharge(h.ctx, testSeat, 40, h.windows()); err != nil {
			h.t.Fatalf("PostCharge: %v", err)
		}
		// NOT A READING OF TWO COUNTERS IT DID NOT CHANGE. The caller
		// compares this answer with its caps to decide whether to say the
		// run went over; filled from a live read, that comparison would
		// turn on spend this call had nothing to do with, and the two
		// backends would have to agree about a reading neither took.
		got, err := h.f.PostCharge(h.ctx, testSeat, 0, h.windows())
		if err != nil || !got.OK || got.Org.Scope != "" || got.Agent.Scope != "" {
			h.t.Fatalf("PostCharge(0) after a charge = (%+v, %v), want OK with "+
				"both counters empty", got, err)
		}
		// And it wrote nothing: the charge before it is still all there is.
		if row, listed := h.usage(coord.OrgScope); !listed || row.In(period.Day).Used != 40 {
			h.t.Fatalf("org usage = %+v (listed=%v), want the 40 the real charge "+
				"made and nothing from the charge of nothing", row, listed)
		}
	},
}, {
	name: "an org post-charge moves the company's counter and no seat's",
	fn: func(h *fleetHarness) {
		// A person's answer is spent by nobody's seat, so it reaches the
		// company's windows alone: the org counter a ceiling judges hears
		// about it, past the ceiling included, and no seat — nor any new
		// scope — is charged for work no seat did.
		if got := h.charge(testSeat, 90, day(100), day(100)); !got.OK {
			h.t.Fatalf("setup charge refused: %+v", got)
		}
		got, err := h.f.PostChargeOrg(h.ctx, 50, h.windows())
		if err != nil {
			h.t.Fatalf("PostChargeOrg: %v", err)
		}
		if got.Scope != coord.OrgScope || got.In(period.Day).Used != 140 {
			h.t.Fatalf("org post-charge = %+v, want the org's counter at 140", got)
		}
		if h.used(coord.OrgScope) != 140 || h.used(testSeat) != 90 {
			h.t.Fatalf("org = %d and seat = %d, want 140 and the seat's own 90",
				h.used(coord.OrgScope), h.used(testSeat))
		}
		rows, err := h.f.Usage(h.ctx, h.windows())
		if err != nil || len(rows) != 2 {
			h.t.Fatalf("Usage = (%+v, %v), want the org and the one seat and no scope "+
				"for the person", rows, err)
		}
		if next := h.charge(testSeat, 1, day(100), nil); next.OK || next.RefusedScope != "org" {
			h.t.Fatalf("next charge = %+v, want the org to refuse against the recorded answer", next)
		}
	},
}, {
	name: "an org post-charge keeps the refusal stamp, and one of nothing writes nothing",
	fn: func(h *fleetHarness) {
		if got, err := h.f.PostChargeOrg(h.ctx, 0, h.windows()); err != nil || got.Scope != "" {
			h.t.Fatalf("PostChargeOrg(0) = (%+v, %v), want an empty answer", got, err)
		}
		if _, listed := h.usage(coord.OrgScope); listed {
			h.t.Fatal("an org post-charge of zero created a counter")
		}
		if _, err := h.f.PostChargeOrg(h.ctx, 5, coord.Windows{}); err == nil {
			h.t.Fatal("an org post-charge with no windows was accepted")
		}
		if _, listed := h.usage(coord.OrgScope); listed {
			h.t.Fatal("an org post-charge with no windows still charged the org")
		}
		// Not a decision about room: a refusing company stays refusing.
		h.charge(testSeat, 90, day(100), nil)
		from := time.Now()
		h.charge(testSeat, 50, day(100), nil) // org refuses
		stamped := h.refusedAt(coord.OrgScope, period.Day, from, time.Now())
		if _, err := h.f.PostChargeOrg(h.ctx, 30, h.windows()); err != nil {
			h.t.Fatalf("PostChargeOrg: %v", err)
		}
		if row, _ := h.usage(coord.OrgScope); !row.In(period.Day).RefusedAt.Equal(stamped) ||
			row.In(period.Day).Used != 120 {
			h.t.Fatalf("org = %+v, want 120 used and the refusal stamped at %v kept", row, stamped)
		}
	},
}, {
	name: "retiring lifetime counters that are not there is not an error",
	fn: func(h *fleetHarness) {
		// The maintenance duty asks on every tick once the fleet is past
		// the protocol that windowed the counters; after the first tick
		// there is nothing left, and every later one must be a no-op
		// rather than a failure logged every fifteen minutes for ever.
		for range 2 {
			retired, err := h.f.RetireLifetimeCounters(h.ctx)
			if err != nil || retired {
				h.t.Fatalf("RetireLifetimeCounters = (%v, %v) on a store with none, "+
					"want (false, nil)", retired, err)
			}
		}
	},
}}
