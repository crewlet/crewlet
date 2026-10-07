package coordtest

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
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
	//
	// Every refused round below is COUNTED in all three windows, as every
	// charge is (coord.Budgets.Charge): the figures carry each one, and the
	// last step is where that shows — the week the 1st shares with the 31st
	// still holds the round March refused.
	saturday := at(2026, time.March, 7, 23, 30)
	if got := charge(saturday, 300); !got.OK {
		h.t.Fatalf("Saturday's first charge was refused: %+v", got)
	}
	refused("Saturday's day is full", charge(saturday, 1), period.Day, saturday)
	want("Saturday", saturday, 301, 301, 301)

	// LOCAL MIDNIGHT, on the day the clocks spring forward: a 23-hour day.
	// The day's allowance is back, and nothing else moved — Sunday is in the
	// same ISO week and the same month.
	sunday := at(2026, time.March, 8, 0, 30)
	if got := charge(sunday, 149); !got.OK {
		h.t.Fatalf("Sunday's first charge was refused, so the day did not roll at "+
			"local midnight: %+v", got)
	}
	want("Sunday", sunday, 149, 450, 450)
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
	want("Monday", monday, 300, 300, 751)

	// The last second of March: a fresh day and a fresh week (Monday the
	// 30th began it), and a month with 49 tokens left.
	lastOfMarch := coord.WindowsAt(time.Date(2026, time.March, 31, 23, 59, 59, 0, ny), ny)
	refused("March is nearly spent", charge(lastOfMarch, 100), period.Month, lastOfMarch)
	want("the 31st", lastOfMarch, 100, 100, 851)

	// THE 1ST: the day and the month roll, the week does not — so it still
	// holds the round March refused on the 31st.
	april := at(2026, time.April, 1, 0, 0)
	if got := charge(april, 100); !got.OK {
		h.t.Fatalf("the 1st's first charge was refused, so the month did not roll: %+v", got)
	}
	want("the 1st", april, 100, 200, 100)
}

// smallerAfterRefused fills a 100-token day to 98 under the caps given, has a
// 3-token round refused by scope, and then charges a 1-token round — which fits
// the room the counter showed before the refused round was counted, and must
// be refused against the room that round actually left.
func smallerAfterRefused(h *fleetHarness, scope string, org, seatCaps coord.Caps) {
	h.t.Helper()
	if got := h.charge(testSeat, 98, org, seatCaps); !got.OK {
		h.t.Fatalf("the first charge was refused: %+v", got)
	}
	if got := h.charge(testSeat, 3, org, seatCaps); got.OK || got.RefusedScope != scope ||
		got.RefusedUsed != 101 {
		h.t.Fatalf("the round past the ceiling = %+v, want the %s scope refusing at 101 of 100",
			got, scope)
	}
	if got := h.charge(testSeat, 1, org, seatCaps); got.OK || got.RefusedScope != scope ||
		got.RefusedUsed != 102 {
		h.t.Fatalf("a smaller round after the refusal = %+v, want the %s scope refusing at "+
			"102 of 100: the refused round was paid for, and left no room", got, scope)
	}
	for _, s := range []string{coord.OrgScope, testSeat} {
		if got := h.used(s); got != 102 {
			h.t.Fatalf("%s used = %d, want all 102 tokens the three rounds spent", s, got)
		}
	}
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

// answered fails unless a charge's answer carries both counters read against
// the suite's windows, the company's at org and the seat's at seat in every
// window.
func (h *fleetHarness) answered(got coord.Spend, org, seat int) {
	h.t.Helper()
	w := h.windows()
	for _, c := range []struct {
		counter coord.Usage
		scope   string
		want    int
	}{{got.Org, coord.OrgScope, org}, {got.Agent, testSeat, seat}} {
		if c.counter.Scope != c.scope {
			h.t.Fatalf("the answer %+v carries %q where the %s counter belongs",
				got, c.counter.Scope, c.scope)
		}
		for i, p := range period.Periods {
			slot := c.counter.In(p)
			if slot.Window.Label != w[i].Label || slot.Used != c.want {
				h.t.Fatalf("the answer's %s %s window = %q at %d, want %q at %d: %+v",
					c.scope, p, slot.Window.Label, slot.Used, w[i].Label, c.want, got)
			}
		}
	}
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
		// The refused round is counted, and the refusal states the counter
		// as it left it: 501 of 500, the figure every later reader sees.
		if got.RefusedScope != "org" || got.RefusedLimit != 500 || got.RefusedUsed != 501 {
			h.t.Fatalf("refusal = %+v, want the org scope at 501 of its 500", got)
		}
		if h.used(coord.OrgScope) != 501 {
			h.t.Fatalf("org spend = %d, want the 500 that fit and the 1 that was refused",
				h.used(coord.OrgScope))
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
	name: "a round the seat refuses is counted on the company too",
	fn: func(h *fleetHarness) {
		// A round is charged once its reply is in, so a refused round has
		// already been billed — to the company as much as to the seat. The
		// counter that took the company's half BACK on a seat's refusal
		// read a round short of what the company had paid for, and handed
		// its other seats room that round had already used.
		if got := h.charge(testSeat, 90, nil, day(100)); !got.OK {
			h.t.Fatalf("the first charge was refused: %+v", got)
		}
		got := h.charge(testSeat, 90, nil, day(100))
		if got.OK {
			h.t.Fatal("a charge past the seat cap was accepted")
		}
		if got.RefusedScope != "agent" || got.RefusedUsed != 180 || got.RefusedLimit != 100 {
			h.t.Fatalf("refusal = %+v, want the agent scope at 180 of its 100", got)
		}
		// Every window, the uncapped ones included.
		w := h.windows()
		for _, scope := range []string{coord.OrgScope, testSeat} {
			if got := h.spent(w, scope); got != [3]int{180, 180, 180} {
				h.t.Fatalf("%s day/week/month = %v after a refused round, want 180 in each: "+
					"the round the seat refused was paid for all the same", scope, got)
			}
		}
	},
}, {
	name: "a round the company refuses is counted on the seat too",
	fn: func(h *fleetHarness) {
		// The other direction: the company is judged first, and its
		// refusal settles the answer — but the round was the seat's, and a
		// seat whose counter never heard of it would read as having room
		// the company's next window will hand it.
		if got := h.charge(testSeat, 90, day(100), day(1000)); !got.OK {
			h.t.Fatalf("the first charge was refused: %+v", got)
		}
		got := h.charge(testSeat, 90, day(100), day(1000))
		if got.OK {
			h.t.Fatal("a charge past the org cap was accepted")
		}
		if got.RefusedScope != "org" || got.RefusedUsed != 180 {
			h.t.Fatalf("refusal = %+v, want the org scope at 180", got)
		}
		w := h.windows()
		for _, scope := range []string{coord.OrgScope, testSeat} {
			if got := h.spent(w, scope); got != [3]int{180, 180, 180} {
				h.t.Fatalf("%s day/week/month = %v after a refused round, want 180 in each",
					scope, got)
			}
		}
	},
}, {
	name: "a refusal answers both counters as the round left them",
	fn: func(h *fleetHarness) {
		// A refusal names ONE window of ONE scope, and the round it
		// records can fill another. Here the seat refuses a round that
		// leaves the company's day exactly at its ceiling, so the next
		// charge is refused by the COMPANY, which is judged first — and a
		// caller that kept only the refusal it was answered (the engine's
		// turn meter keeps what an answer makes certain) named the seat
		// for a refusal the company makes. So a refusal answers both
		// counters as the charge left them, exactly as an admission does.
		if got := h.charge(testSeat, 40, day(100), day(50)); !got.OK {
			h.t.Fatalf("the first charge was refused: %+v", got)
		}
		got := h.charge(testSeat, 60, day(100), day(50))
		if got.OK || got.RefusedScope != "agent" || got.RefusedUsed != 100 || got.RefusedLimit != 50 {
			h.t.Fatalf("refusal = %+v, want the seat at 100 of its 50", got)
		}
		h.answered(got, 100, 100)

		next := h.charge(testSeat, 1, day(100), day(50))
		if next.OK || next.RefusedScope != "org" || next.RefusedUsed != 101 {
			h.t.Fatalf("the next charge = %+v, want the company refusing at 101: the seat's "+
				"refusal left the company's day with no room", next)
		}
		// And the company's refusal carries the seat it counted the round
		// on, with no verdict of its own.
		h.answered(next, 101, 101)
	},
}, {
	name: "a round the company refused leaves no room for a smaller one",
	fn: func(h *fleetHarness) {
		// The room is judged on what the counter holds, so a counter that
		// dropped the refused round still showed the room that round had
		// spent: 98 of 100, a 3-token round refused, and then a 1-token
		// round ADMITTED on top of the 3 already paid for. Counted, the
		// refused round takes the window past its ceiling and every charge
		// after it is refused.
		smallerAfterRefused(h, "org", day(100), nil)
	},
}, {
	name: "a round the seat refused leaves no room for a smaller one",
	fn: func(h *fleetHarness) {
		// The same rule on the seat's own ceiling, which a backend judges
		// in a write of its own after the company's.
		smallerAfterRefused(h, "agent", nil, day(100))
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
		// The same ordering rule, reached through the other door: a charge
		// larger than the seat's WHOLE cap, while the company is out of
		// room too — the org holds 90 of 100 and 50 more fits neither. A
		// backend that judged each scope's cap alone named the seat, and an
		// operator sent to raise the seat's ceiling would raise it and
		// still be refused.
		if got := h.charge(testSeat, 90, day(100), nil); !got.OK {
			h.t.Fatalf("the first charge was refused: %+v", got)
		}
		got := h.charge(testSeat, 50, day(100), day(40))
		if got.OK {
			h.t.Fatal("a charge past both caps was accepted")
		}
		if got.RefusedScope != "org" || got.RefusedUsed != 140 || got.RefusedLimit != 100 {
			h.t.Fatalf("refusal = %+v, want the org scope at 140 of 100", got)
		}
		if h.used(coord.OrgScope) != 140 || h.used(testSeat) != 140 {
			h.t.Fatal("the refused round is missing from a counter it was spent against")
		}
		// The company refused, so the seat has no verdict of its own: its
		// own cap could not hold the round either, but the refusal is the
		// company's and it is stamped there alone.
		if stamps := h.refusing(testSeat); len(stamps) != 0 {
			h.t.Fatalf("the seat carries a refusal the company made, in %v", stamps)
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
		if got.RefusedScope != "agent" || got.RefusedLimit != 40 || got.RefusedPeriod != period.Day ||
			got.RefusedUsed != 50 {
			h.t.Fatalf("refusal = %+v, want the agent scope's day window at 50 of its 40", got)
		}
		if h.used(coord.OrgScope) != 50 || h.used(testSeat) != 50 {
			h.t.Fatal("the refused round is missing from a counter it was spent against")
		}
	},
}, {
	name: "a charge larger than the whole cap is refused on a counter that never charged",
	fn: func(h *fleetHarness) {
		// The first-ever charge, against an empty counter. A backend
		// that only checked "existing + delta" on an UPDATE path would
		// accept this one, because there is nothing to update yet. It is
		// still the gate saying no, so it is stamped like any refusal —
		// a backend that stamped only on one path would leave the largest
		// refusals of all invisible — and it is counted like any round.
		from := time.Now()
		got := h.charge(testSeat, 1_000_000, day(10), nil)
		if got.OK {
			h.t.Fatal("a charge larger than the entire cap was accepted")
		}
		if got.RefusedScope != "org" {
			h.t.Fatalf("refusal = %+v, want the org scope", got)
		}
		h.refusedAt(coord.OrgScope, period.Day, from, time.Now())
		if h.used(coord.OrgScope) != 1_000_000 || h.used(testSeat) != 1_000_000 {
			h.t.Fatal("the refused round is missing from a counter it was spent against")
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
		// Each seat's counter carries its own rounds and nobody else's;
		// the company's carries both.
		if h.used(other) != 60 || h.used(testSeat) != 60 || h.used(coord.OrgScope) != 120 {
			h.t.Fatalf("seats = %d and %d, org = %d, want 60 each and the company 120",
				h.used(testSeat), h.used(other), h.used(coord.OrgScope))
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
		// The stamp is the record of the gate saying no, and WHEN, and it
		// belongs to the scope that said it. The round it refused is
		// counted on both scopes; the stamp is on one.
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
		if h.used(coord.OrgScope) != 140 {
			h.t.Fatalf("org used = %d after a refusal, want the 90 it held and the 50 refused",
				h.used(coord.OrgScope))
		}

		other := "agent:44444444-4444-4444-4444-444444444444"
		from = time.Now()
		// The company's own ceiling left out: it is past it, and would
		// refuse first.
		if got := h.charge(other, 5, nil, day(4)); got.RefusedScope != "agent" {
			h.t.Fatalf("refusal = %+v, want the agent scope", got)
		}
		h.refusedAt(other, period.Day, from, time.Now())
		if row, _ := h.usage(other); row.In(period.Day).Used != 5 || row.UpdatedAt.IsZero() {
			// A seat refused on its first charge still spent that
			// round, so it reads the round and the time it was counted.
			h.t.Fatalf("a scope refused on its first charge reads %+v, want its 5 tokens and "+
				"a charge time", row)
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
		if got.OK || got.RefusedPeriod != period.Week || got.RefusedLimit != 150 || got.RefusedUsed != 200 {
			h.t.Fatalf("refusal = %+v, want the org's week at 200 of 150, the refused round counted", got)
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
			if !got.RefusedWindow.End.Equal(w[i].End) || got.RefusedUsed != 101 || got.RefusedLimit != 100 {
				h.t.Fatalf("%s: refusal = %+v, want the %s ending %v at 101 of 100",
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
		// Refused against the window the counter is on, named as it, and
		// counted there.
		got := h.chargeAt(today, testSeat, 20, day(100), nil)
		if got.OK || got.RefusedWindow.Label != tomorrow[0].Label || got.RefusedUsed != 110 {
			h.t.Fatalf("refusal = %+v, want tomorrow %q at 110 of 100", got, tomorrow[0].Label)
		}
		// A read against the earlier day answers what that refusal was made
		// against — the later day, its spend and its refusal — and never the
		// earlier day unspent: a reader told there was room would hand a
		// turn headroom the gate refuses. That lasts a few seconds behind a
		// peer's clock, and up to a day after the company's zone moves west.
		read := h.usedAt(today, coord.OrgScope).In(period.Day)
		if read.Used != 110 || read.Window.Label != tomorrow[0].Label || read.RefusedAt.IsZero() {
			h.t.Fatalf("a read of the earlier day = %+v, want the later day %q at 110, refusing: "+
				"the counter holds the later day and the gate refuses against it", read, tomorrow[0].Label)
		}
	},
}, {
	name: "an admitted charge clears the refusals of both scopes it charged",
	fn: func(h *fleetHarness) {
		// A cap raised: the scope has room again, and a dashboard still
		// saying "refusing charges" would send an operator to fix what is
		// already fixed. (A smaller round is no way back: the round that was
		// refused is counted, so it left the window past its ceiling.)
		h.charge(testSeat, 95, day(100), nil)
		h.charge(testSeat, 50, day(100), nil) // org refuses, at 145
		h.charge(testSeat, 5, nil, day(4))    // seat refuses, at 150
		if len(h.refusing(coord.OrgScope)) == 0 {
			h.t.Fatal("setup: the org refusal was not recorded")
		}
		if len(h.refusing(testSeat)) == 0 {
			h.t.Fatal("setup: the seat refusal was not recorded")
		}
		if got := h.charge(testSeat, 5, day(1000), day(1000)); !got.OK {
			h.t.Fatalf("a charge that fits both raised caps was refused: %+v", got)
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
		h.charge(testSeat, 50, day(100), nil) // org refuses, at 140
		stamped := h.refusedAt(coord.OrgScope, period.Day, from, time.Now())

		// The company's ceiling raised, so it has room; the seat's does not.
		got := h.charge(testSeat, 5, day(1000), day(142))
		if got.RefusedScope != "agent" {
			h.t.Fatalf("refusal = %+v, want the agent scope", got)
		}
		row, _ := h.usage(coord.OrgScope)
		if !row.In(period.Day).RefusedAt.Equal(stamped) {
			h.t.Fatalf("org refusal stamp = %v, want the %v it held: a charge refused "+
				"overall cleared it", row.In(period.Day).RefusedAt, stamped)
		}
		if row.In(period.Day).Used != 145 {
			h.t.Fatalf("org used = %d, want 145: the round the seat refused is the "+
				"company's spend too", row.In(period.Day).Used)
		}
	},
}, {
	name: "a company refusal leaves the seat's own refusal standing",
	fn: func(h *fleetHarness) {
		// The other direction of the case above, and the one a backend
		// reaches by WRITING the seat: a round the company refused is
		// counted on the seat with no verdict of its own, so that write
		// carries the seat's stamps through. A backend that cleared them
		// there — or judged the seat after all — would erase a seat
		// refusal that is still true, and the seat's surfaces would stop
		// saying since when it has been refusing.
		from := time.Now()
		if got := h.charge(testSeat, 50, nil, day(40)); got.RefusedScope != "agent" {
			h.t.Fatalf("setup refusal = %+v, want the agent scope", got)
		}
		stamped := h.refusedAt(testSeat, period.Day, from, time.Now())
		// A distinct instant, so a backend that stamped the seat again
		// cannot pass by landing in the same clock tick.
		time.Sleep(2 * time.Millisecond)

		// The company's ceiling tightened below what it holds: the
		// company refuses, and the seat's own ceiling is not asked.
		if got := h.charge(testSeat, 10, day(55), day(1000)); got.RefusedScope != "org" {
			h.t.Fatalf("refusal = %+v, want the org scope", got)
		}
		row, _ := h.usage(testSeat)
		if got := row.In(period.Day).RefusedAt; !got.Equal(stamped) {
			h.t.Fatalf("seat refusal stamp = %v, want the %v it held: a round the "+
				"company refused rewrote the seat's own refusal", got, stamped)
		}
		if h.used(coord.OrgScope) != 60 || h.used(testSeat) != 60 {
			h.t.Fatalf("org = %d and seat = %d, want both rounds on both counters",
				h.used(coord.OrgScope), h.used(testSeat))
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
		// that were two steps would let every racing caller see room. What
		// is ADMITTED is what the ceiling holds, exactly; what is COUNTED is
		// every round, because every one of them was spent.
		const callers = 32
		var (
			wg       sync.WaitGroup
			admitted atomic.Int32
		)
		errs := make(chan error, callers)
		for range callers {
			wg.Go(func() {
				got, err := h.f.Charge(h.ctx, coord.ChargeRequest{
					Seat: testSeat, Tokens: 10, Windows: h.windows(), OrgCaps: day(100),
				})
				switch {
				case err != nil:
					errs <- err
				case got.OK:
					admitted.Add(1)
				}
			})
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			h.t.Fatalf("a racing charge failed: %v", err)
		}
		if got := admitted.Load(); got != 10 {
			h.t.Fatalf("%d of %d racing charges of 10 were admitted against a ceiling of 100, "+
				"want exactly 10", got, callers)
		}
		if got := h.used(coord.OrgScope); got != callers*10 {
			h.t.Fatalf("org spend = %d after %d racing charges of 10, want all %d: a refused "+
				"round is spent like an admitted one", got, callers, callers*10)
		}
	},
}, {
	name: "a post-charge records spend that overran both caps",
	fn: func(h *fleetHarness) {
		// The spend already happened (a detached coding run, collected
		// long after it started) and nothing waits on a verdict about it,
		// so nothing refuses it: it is recorded whole, past both caps, and
		// the next round is refused against what the run used.
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
		if row, _ := h.usage(coord.OrgScope); !row.In(period.Day).RefusedAt.Equal(stamped) || row.In(period.Day).Used != 170 {
			h.t.Fatalf("org = %+v, want 170 used and the refusal stamped at %v kept", row, stamped)
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
			row.In(period.Day).Used != 170 {
			h.t.Fatalf("org = %+v, want 170 used and the refusal stamped at %v kept", row, stamped)
		}
	},
}, {
	name: "a seat post-charge moves that seat's counter and not the company's",
	fn: func(h *fleetHarness) {
		// It finishes a post-charge whose company half already landed, so
		// counting the company here would count it twice. Past the seat's
		// ceiling included: the spend happened, and the next round is
		// refused against it.
		if got := h.charge(testSeat, 90, day(1000), day(100)); !got.OK {
			h.t.Fatalf("setup charge refused: %+v", got)
		}
		got, err := h.f.PostChargeSeat(h.ctx, testSeat, 50, h.windows())
		if err != nil {
			h.t.Fatalf("PostChargeSeat: %v", err)
		}
		if got.Scope != testSeat || got.In(period.Day).Used != 140 {
			h.t.Fatalf("seat post-charge = %+v, want the seat's counter at 140", got)
		}
		if h.used(coord.OrgScope) != 90 || h.used(testSeat) != 140 {
			h.t.Fatalf("org = %d and seat = %d, want the company's own 90 and 140",
				h.used(coord.OrgScope), h.used(testSeat))
		}
		if next := h.charge(testSeat, 1, day(1000), day(100)); next.OK || next.RefusedScope != "agent" {
			h.t.Fatalf("next charge = %+v, want the seat to refuse against the recorded spend", next)
		}
	},
}, {
	name: "a seat post-charge keeps the refusal stamp, needs a seat, and one of nothing writes nothing",
	fn: func(h *fleetHarness) {
		if got, err := h.f.PostChargeSeat(h.ctx, testSeat, 0, h.windows()); err != nil || got.Scope != "" {
			h.t.Fatalf("PostChargeSeat(0) = (%+v, %v), want an empty answer", got, err)
		}
		if _, listed := h.usage(testSeat); listed {
			h.t.Fatal("a seat post-charge of zero created a counter")
		}
		if _, err := h.f.PostChargeSeat(h.ctx, "", 5, h.windows()); err == nil {
			h.t.Fatal("a seat post-charge with no seat scope was accepted")
		}
		if _, err := h.f.PostChargeSeat(h.ctx, testSeat, 5, coord.Windows{}); err == nil {
			h.t.Fatal("a seat post-charge with no windows was accepted")
		}
		if _, listed := h.usage(testSeat); listed {
			h.t.Fatal("a refused seat post-charge still charged the seat")
		}
		// Not a decision about room: a refusing seat stays refusing.
		from := time.Now()
		h.charge(testSeat, 50, nil, day(40)) // the seat refuses
		stamped := h.refusedAt(testSeat, period.Day, from, time.Now())
		if _, err := h.f.PostChargeSeat(h.ctx, testSeat, 30, h.windows()); err != nil {
			h.t.Fatalf("PostChargeSeat: %v", err)
		}
		if row, _ := h.usage(testSeat); !row.In(period.Day).RefusedAt.Equal(stamped) ||
			row.In(period.Day).Used != 80 {
			h.t.Fatalf("seat = %+v, want 80 used and the refusal stamped at %v kept", row, stamped)
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
