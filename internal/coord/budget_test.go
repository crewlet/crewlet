package coord_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/period"
)

// The arithmetic both backends share, exercised as values. The contract suite
// proves each backend runs it; these prove what it says, including the cases
// no charge through a backend can arrange on demand.

func windowsOn(y int, m time.Month, d int) coord.Windows {
	return coord.WindowsAt(time.Date(y, m, d, 12, 0, 0, 0, time.UTC), time.UTC)
}

// A SLOT NEVER ROLLS BACK. A node whose clock trails a peer's across a
// boundary must count in the window the counter is already on; rolling the
// slot back would hand that window its whole allowance again on every charge a
// trailing clock makes.
func TestARollMovesASlotForwardOnly(t *testing.T) {
	t.Parallel()
	today, tomorrow := windowsOn(2026, time.March, 14), windowsOn(2026, time.March, 15)
	tally := coord.Tally{}.Roll(tomorrow).Add(60, time.Now())

	back := tally.Roll(today)
	if back.Slots[0] != tally.Slots[0] {
		t.Fatalf("rolled to an earlier day: %+v, want the later day's slot kept %+v",
			back.Slots[0], tally.Slots[0])
	}
	next := tally.Roll(windowsOn(2026, time.March, 16))
	if next.Slots[0].Used != 0 || next.Slots[0].Label != "2026-03-16" {
		t.Fatalf("a later day did not roll the slot: %+v", next.Slots[0])
	}
	// The week (Mon 16 March begins W12) rolled too; the month did not.
	if next.Slots[1].Used != 0 || next.Slots[2].Used != 60 {
		t.Fatalf("week/month after the roll = %d/%d, want 0/60", next.Slots[1].Used, next.Slots[2].Used)
	}
}

// A ROLL CLEARS THE WINDOW'S REFUSAL WITH ITS SPEND, and only the window that
// turned over: a refusal of yesterday is not a refusal of today, and a
// refusal of this month still is.
func TestARollClearsOnlyTheRefusalsOfWindowsThatTurnedOver(t *testing.T) {
	t.Parallel()
	today := windowsOn(2026, time.March, 14)
	stamp := time.Date(2026, time.March, 14, 12, 0, 0, 0, time.UTC)
	tally := coord.Tally{}.Roll(today).Stamp([]period.Period{period.Day, period.Month},
		coord.Tally{}.Roll(today), stamp)

	rolled := tally.Roll(windowsOn(2026, time.March, 15))
	if !rolled.Slots[0].RefusedAt.IsZero() {
		t.Error("yesterday's refusal survived into today")
	}
	if !rolled.Slots[2].RefusedAt.Equal(stamp) {
		t.Error("the month's refusal was cleared by the day turning over")
	}
}

// AN UNDO TAKES A CHARGE BACK FROM THE WINDOWS IT WAS COUNTED IN. A slot that
// rolled on between the charge and its unwind holds another window's spend,
// and taking the round off it would hand that window credit.
func TestAnUndoLeavesASlotThatRolledOnAlone(t *testing.T) {
	t.Parallel()
	charged := coord.Tally{}.Roll(windowsOn(2026, time.March, 14)).Add(40, time.Now())
	moved := charged.Roll(windowsOn(2026, time.March, 15)).Add(25, time.Now())

	undone := moved.Undo(40, charged, time.Now())
	if undone.Slots[0].Used != 25 {
		t.Errorf("the new day = %d, want the 25 charged in it", undone.Slots[0].Used)
	}
	if undone.Slots[2].Used != 25 {
		t.Errorf("the month = %d, want 65 less the 40 undone", undone.Slots[2].Used)
	}
	if floor := charged.Undo(100, charged, time.Now()); floor.Slots[0].Used != 0 {
		t.Errorf("an undo larger than the spend left %d, want a floor at zero", floor.Slots[0].Used)
	}
}

// A CLEAR TAKES ONLY THE STAMPS IT SAW. A refusal stamped after an admitted
// charge read the counter is still true, and clearing it would hide a scope
// that is refusing now.
func TestAClearLeavesANewerRefusalStanding(t *testing.T) {
	t.Parallel()
	w := windowsOn(2026, time.March, 14)
	day := []period.Period{period.Day}
	first := time.Date(2026, time.March, 14, 12, 0, 0, 0, time.UTC)
	seen := coord.Tally{}.Roll(w).Stamp(day, coord.Tally{}.Roll(w), first)
	newer := seen.Stamp(day, seen, first.Add(time.Second))

	if got := newer.Clear(seen); !got.Slots[0].RefusedAt.Equal(first.Add(time.Second)) {
		t.Fatalf("a clear of a stale stamp erased the newer refusal: %+v", got.Slots[0])
	}
	if got := seen.Clear(seen); !got.Slots[0].RefusedAt.IsZero() {
		t.Fatalf("a clear of the stamp it saw left it: %+v", got.Slots[0])
	}
}

// A REFUSAL NAMES THE REFUSING WINDOW THAT ENDS LAST — by its end, not by its
// period. On Tuesday 31 March the month ends at midnight and the week on
// Sunday, so a seat refused by both waits for the week; windows that end
// together name the longer period.
func TestARefusalNamesTheWindowThatEndsLast(t *testing.T) {
	t.Parallel()
	w := windowsOn(2026, time.March, 31)
	caps := coord.Caps{period.Day: 500, period.Week: 100, period.Month: 100}
	tally := coord.Tally{}.Roll(w).Add(100, time.Now())

	refusing := tally.Refusing(1, caps)
	if len(refusing) != 2 {
		t.Fatalf("refusing = %v, want the week and the month", refusing)
	}
	got := tally.Refusal("org", refusing, caps, w)
	if got.RefusedPeriod != period.Week || got.RefusedWindow.Label != "2026-W14" ||
		got.RefusedUsed != 100 || got.RefusedLimit != 100 || got.RefusedScope != "org" {
		t.Fatalf("refusal = %+v, want the org's week 2026-W14 at 100 of 100", got)
	}

	// A day that ends before a month: the month is named.
	mid := windowsOn(2026, time.March, 14)
	full := coord.Tally{}.Roll(mid).Add(100, time.Now())
	both := coord.Caps{period.Day: 100, period.Month: 100}
	if got := full.Refusal("org", full.Refusing(1, both), both, mid); got.RefusedPeriod != period.Month {
		t.Fatalf("refusal = %+v, want the month, which ends after the day", got)
	}

	// Sunday 31 May: the day, the week and the month all end at the same
	// midnight, and the longer period is named — in whatever order the
	// refusing periods arrive, since the tie is broken on the period and
	// not on the list.
	sunday := windowsOn(2026, time.May, 31)
	all := coord.Caps{period.Day: 100, period.Week: 100, period.Month: 100}
	spent := coord.Tally{}.Roll(sunday).Add(100, time.Now())
	for _, order := range [][]period.Period{
		{period.Day, period.Week, period.Month},
		{period.Month, period.Week, period.Day},
		{period.Week, period.Month, period.Day},
	} {
		if got := spent.Refusal("org", order, all, sunday); got.RefusedPeriod != period.Month ||
			got.RefusedWindow.Label != "2026-05" {
			t.Fatalf("refusing %v: refusal = %+v, want the month 2026-05, the longest of three "+
				"windows ending together", order, got)
		}
	}
}

// THE BINDING WINDOW is the one a single-figure reader shows: a refusing window
// first, else the one with the least room left.
func TestTheBindingWindowIsTheRefusingOneElseTheTightest(t *testing.T) {
	t.Parallel()
	w := windowsOn(2026, time.March, 14)
	caps := coord.Caps{period.Day: 3000, period.Month: 10000}

	u := coord.Tally{}.Roll(w).Add(2900, time.Now()).Usage("org", w)
	if slot, ceiling, ok := u.Binding(caps); !ok || slot.Window.Period != period.Day || ceiling != 3000 {
		t.Fatalf("binding = %+v of %d (%v), want the day, 100 from its ceiling", slot, ceiling, ok)
	}

	// The month has 150 left and the day 100 — but the MONTH refused: a
	// refusal is the window the scope is waiting on, whatever the arithmetic
	// of the others says.
	tally := coord.Tally{}.Roll(w).Add(2900, time.Now())
	tally.Slots[2].Used = 9850
	tally = tally.Stamp([]period.Period{period.Month}, tally, time.Now())
	if slot, ceiling, ok := tally.Usage("org", w).Binding(caps); !ok || slot.Window.Period != period.Month || ceiling != 10000 {
		t.Fatalf("binding = %+v of %d (%v), want the refusing month", slot, ceiling, ok)
	}

	// Two windows refusing: the day with nothing left, the month with 150.
	// Once the gate has said no in both, room left is not the question —
	// neither admits anything until it turns over — so the binding window
	// is the one the scope waits out, the month, and not the day that has
	// less room. See TestTheBindingWindowIsTheWindowTheRefusalNamed.
	both := coord.Tally{}.Roll(w).Add(3000, time.Now())
	both.Slots[2].Used = 9850
	both = both.Stamp([]period.Period{period.Day, period.Month}, both, time.Now())
	if slot, ceiling, ok := both.Usage("org", w).Binding(caps); !ok || slot.Window.Period != period.Month || ceiling != 10000 {
		t.Fatalf("binding = %+v of %d (%v), want the refusing month, which ends after the refusing day",
			slot, ceiling, ok)
	}

	// Nothing capped binds nothing, and the one figure left to state is the
	// month: the widest spend the counter keeps.
	if slot, ceiling, ok := u.Binding(nil); ok || slot.Window.Period != period.Month ||
		slot.Used != 2900 || ceiling != 0 {
		t.Fatalf("binding with no caps = %+v of %d (%v), want the month, uncapped", slot, ceiling, ok)
	}
}

// THE BINDING WINDOW IS THE WINDOW THE REFUSAL NAMED. A charge refused by
// several windows stamps every one of them and names the one that ends last;
// a reader stating one figure per scope — the budgets answer, the live meter,
// `crewlet budgets` — states the binding window of those same stamps. Were the
// two chosen by different rules, the refusal event would say "out for the
// month" while every screen said "out for the day". Three instants cover it
// from both sides of the period order and at the tie: mid-month the month
// outlasts the day, on Tuesday 31 March the week outlasts the month, and on
// Sunday 31 May all three end together.
func TestTheBindingWindowIsTheWindowTheRefusalNamed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		what string
		w    coord.Windows
		caps coord.Caps
		used [3]int
	}{{
		// The day has nothing left and the month 50: least room left
		// would name the day.
		what: "mid-month, the day at 0 left and the month at 50",
		w:    windowsOn(2026, time.March, 14),
		caps: coord.Caps{period.Day: 100, period.Month: 150},
		used: [3]int{100, 100, 100},
	}, {
		// The month has less room than the week, and ends first.
		what: "Tuesday 31 March, the week and the month",
		w:    windowsOn(2026, time.March, 31),
		caps: coord.Caps{period.Week: 200, period.Month: 120},
		used: [3]int{100, 150, 100},
	}, {
		what: "Sunday 31 May, all three ending together",
		w:    windowsOn(2026, time.May, 31),
		caps: coord.Caps{period.Day: 100, period.Week: 150, period.Month: 200},
		used: [3]int{100, 100, 100},
	}} {
		tally := coord.Tally{}.Roll(tc.w)
		for i, used := range tc.used {
			tally.Slots[i].Used = used
		}
		const charge = 120
		refusing := tally.Refusing(charge, tc.caps)
		if len(refusing) < 2 {
			t.Fatalf("%s: refusing = %v, want at least two windows for the choice to matter",
				tc.what, refusing)
		}
		named := tally.Refusal("org", refusing, tc.caps, tc.w)
		stamped := tally.Stamp(refusing, tally, time.Now())
		slot, ceiling, ok := stamped.Usage("org", tc.w).Binding(tc.caps)
		if !ok || slot.Window.Period != named.RefusedPeriod || slot.Window.Label != named.RefusedWindow.Label ||
			slot.Used != named.RefusedUsed || ceiling != named.RefusedLimit {
			t.Fatalf("%s: binding = %s %q at %d of %d, the refusal named %s %q at %d of %d",
				tc.what, slot.Window.Period, slot.Window.Label, slot.Used, ceiling,
				named.RefusedPeriod, named.RefusedWindow.Label, named.RefusedUsed, named.RefusedLimit)
		}
	}
}

// A READ AGAINST A LATER WINDOW IS THAT WINDOW UNSPENT, and names the window
// it was asked about, so a reader never shows a figure under a label it does
// not belong to — which is what the charge that rolls the slot will make it.
func TestAReadAgainstALaterWindowIsThatWindowUnspent(t *testing.T) {
	t.Parallel()
	today, tomorrow := windowsOn(2026, time.March, 14), windowsOn(2026, time.March, 15)
	u := coord.Tally{}.Roll(today).Add(70, time.Now()).Usage("org", tomorrow)
	if day := u.In(period.Day); day.Used != 0 || day.Window.Label != "2026-03-15" {
		t.Fatalf("tomorrow's day read %+v, want it unspent under its own label", day)
	}
	if month := u.In(period.Month); month.Used != 70 {
		t.Fatalf("the month read %d, want the 70 it holds", month.Used)
	}
	if empty := coord.Unspent("agent:x", today); empty.Scope != "agent:x" || empty.In(period.Week).Window.Label != "2026-W11" {
		t.Fatalf("Unspent = %+v, want the scope and the week it was read against", empty)
	}
}

// A READ BEHIND A SLOT THAT LEADS IT IS THE LATER WINDOW, WITH ITS SPEND. A
// slot never rolls back, so a charge against the earlier window is refused
// against the later window's spend; a read answering the earlier window
// unspent would hand every reader — the headroom a turn is given, the learning
// gate, the budgets answer — room the gate will not admit. That lasts a few
// seconds behind a peer's clock and up to a day after the company's zone moves
// west.
func TestAReadBehindALaterSlotIsThatSlot(t *testing.T) {
	t.Parallel()
	today, tomorrow := windowsOn(2026, time.March, 14), windowsOn(2026, time.March, 15)
	caps := coord.Caps{period.Day: 100}
	stamp := time.Date(2026, time.March, 15, 1, 0, 0, 0, time.UTC)
	tally := coord.Tally{}.Roll(tomorrow).Add(90, time.Now())
	tally = tally.Stamp([]period.Period{period.Day}, tally, stamp)

	day := tally.Usage("org", today).In(period.Day)
	if day.Used != 90 || day.Window.Label != "2026-03-15" || !day.Window.Start.Equal(tomorrow[0].Start) ||
		!day.RefusedAt.Equal(stamp) {
		t.Fatalf("a read of the 14th behind a slot on the 15th = %+v, want the 15th at 90, refusing", day)
	}
	// And it is what the gate decides with: the charge the read left room
	// for is refused, naming the same window at the same spend.
	rolled := tally.Roll(today)
	refusing := rolled.Refusing(20, caps)
	named := rolled.Refusal("org", refusing, caps, today)
	if len(refusing) != 1 || named.RefusedWindow.Label != day.Window.Label || named.RefusedUsed != day.Used {
		t.Fatalf("the gate refused %v naming %+v, the read said %+v", refusing, named, day)
	}
}

// A REQUEST THAT CANNOT BE COUNTED IS REFUSED BY NAME: no windows, windows out
// of order, and a ceiling of 0 — which once meant unlimited here and means
// nothing may be spent to anybody reading the word.
func TestAnUncountableRequestIsRefusedByName(t *testing.T) {
	t.Parallel()
	w := windowsOn(2026, time.March, 14)
	for name, req := range map[string]coord.ChargeRequest{
		"no seat":         {Tokens: 1, Windows: w},
		"no windows":      {Seat: "agent:x", Tokens: 1},
		"reversed":        {Seat: "agent:x", Tokens: 1, Windows: coord.Windows{w[2], w[1], w[0]}},
		"a zero ceiling":  {Seat: "agent:x", Tokens: 1, Windows: w, OrgCaps: coord.Caps{period.Day: 0}},
		"not a period":    {Seat: "agent:x", Tokens: 1, Windows: w, SeatCaps: coord.Caps{"fortnight": 5}},
		"a negative ceil": {Seat: "agent:x", Tokens: 1, Windows: w, SeatCaps: coord.Caps{period.Week: -1}},
	} {
		if err := req.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := (coord.ChargeRequest{Seat: "agent:x", Tokens: 1, Windows: w,
		OrgCaps: coord.Caps{period.Month: 1}}).Validate(); err != nil {
		t.Errorf("a well-formed request was refused: %v", err)
	}
}

// THE PROTOCOL THAT WINDOWED THE COUNTERS IS ONE THIS BUILD SPEAKS. The
// lifetime counters are retired once no lease is held below it, so a build
// whose own protocol were lower would retire them under its own feet.
func TestTheWindowedCountersProtocolIsNotAheadOfThisBuild(t *testing.T) {
	t.Parallel()
	if coord.WindowedCountersProtocol > coord.ProtocolVersion {
		t.Fatalf("WindowedCountersProtocol %d is ahead of ProtocolVersion %d",
			coord.WindowedCountersProtocol, coord.ProtocolVersion)
	}
}
