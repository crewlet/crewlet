package engine

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/period"
)

// woke reports whether the latch has a wake pending, taking it.
func woke(l *refusalLatch) bool {
	select {
	case <-l.woken():
		return true
	default:
		return false
	}
}

// A WINDOW'S FIRST REFUSAL WAKES THE LIVE METERS, AND ONLY ITS FIRST.
//
// The first refusal is the moment a header has to change — the company, or a
// seat, has stopped — and every refusal after it in the same window changes
// nothing a header draws, so it rides the reporter's tick rather than putting
// one frame per refused round on the fleet's stream. "First" is as the counter
// counts it: an admitted charge clears the scope's stamps, so a refusal after
// one is a first again, and a window that is no longer current is forgotten.
func TestAWindowsFirstRefusalWakesTheMetersAndOnlyItsFirst(t *testing.T) {
	t.Parallel()
	l := newRefusalLatch()
	seat := coord.AgentScope("x")

	l.refused(coord.OrgScope, period.Day, "2026-09-23")
	if !woke(l) {
		t.Fatal("a window's first refusal did not wake the meters")
	}
	l.refused(coord.OrgScope, period.Day, "2026-09-23")
	if woke(l) {
		t.Fatal("a repeat refusal in the same window woke the meters again")
	}
	for _, other := range []refusedWindow{
		{scope: seat, period: period.Day, label: "2026-09-23"},
		{scope: coord.OrgScope, period: period.Week, label: "2026-W39"},
		{scope: coord.OrgScope, period: period.Day, label: "2026-09-24"},
	} {
		l.refused(other.scope, other.period, other.label)
		if !woke(l) {
			t.Errorf("the first refusal of %+v did not wake the meters", other)
		}
	}

	// AN ADMITTED CHARGE CLEARS THE STAMPS of the scopes it charged.
	l.admitted(coord.OrgScope, seat)
	l.refused(coord.OrgScope, period.Day, "2026-09-23")
	if !woke(l) {
		t.Error("a refusal after an admitted charge cleared the window did not wake the meters")
	}

	// AND A WINDOW THAT IS NO LONGER CURRENT IS FORGOTTEN.
	l.refused(coord.OrgScope, period.Month, "2026-09")
	woke(l)
	l.keep(coord.WindowsAt(time.Date(2026, time.October, 2, 12, 0, 0, 0, time.UTC), time.UTC))
	l.mu.Lock()
	held := len(l.seen)
	l.mu.Unlock()
	if held != 0 {
		t.Errorf("the latch holds %d window(s) after the month turned over, want none", held)
	}
}

// EVERY REFUSAL THE COUNTER ANSWERS REACHES THE LATCH.
//
// The counter's answer is the one point every refusal passes, so the gate's
// counter is where the meters are told: a charge refused names its window, an
// admitted one clears both scopes it charged, and a refusal recorded without a
// charge names every window it stamped.
func TestEveryRefusalTheCounterAnswersReachesTheLatch(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.September, 23, 12, 0, 0, 0, time.UTC)
	windows := coord.WindowsAt(now, time.UTC)
	seat := coord.AgentScope("x")
	caps := coord.Caps{period.Day: 100}
	l := newRefusalLatch()
	counter := noticedCounter{budgetCounter: coordmem.NewFleet(), refusals: l}
	charge := func(tokens int, caps coord.Caps) coord.Spend {
		t.Helper()
		got, err := counter.Charge(t.Context(), coord.ChargeRequest{
			Seat: seat, Tokens: tokens, Windows: windows, OrgCaps: caps,
		})
		if err != nil {
			t.Fatalf("Charge: %v", err)
		}
		return got
	}

	if got := charge(40, caps); !got.OK || woke(l) {
		t.Fatalf("an admitted charge (%+v) woke the meters", got)
	}
	if got := charge(80, caps); got.OK || !woke(l) {
		t.Fatalf("a refused charge (%+v) did not wake the meters", got)
	}
	if got := charge(1, caps); got.OK || woke(l) {
		t.Fatalf("a second refusal in the window (%+v) woke the meters again", got)
	}
	// A CEILING RAISED: the charge it admits clears the window's stamp, so
	// the next refusal there is a first again.
	raised := coord.Caps{period.Day: 1000}
	if got := charge(1, raised); !got.OK || woke(l) {
		t.Fatalf("a charge under the raised ceiling (%+v) was refused or woke the meters", got)
	}
	if got := charge(1000, raised); got.OK || !woke(l) {
		t.Fatalf("the first refusal after an admitted charge (%+v) did not wake the meters", got)
	}

	// A SEAT'S REFUSAL IS ITS OWN WINDOW: two seats each stopped on their own
	// day are two firsts, not one window refused twice.
	for _, s := range []string{coord.AgentScope("a"), coord.AgentScope("b")} {
		got, err := counter.Charge(t.Context(), coord.ChargeRequest{
			Seat: s, Tokens: 20, Windows: windows, SeatCaps: coord.Caps{period.Day: 10},
		})
		if err != nil {
			t.Fatalf("Charge: %v", err)
		}
		if got.OK || !woke(l) {
			t.Errorf("%s's first refusal of its own day (%+v) did not wake the meters", s, got)
		}
	}

	// A REFUSAL WITH NO CHARGE, on a seat whose own day the gate finds full.
	seatCaps := coord.Caps{period.Day: 10}
	if _, err := counter.budgetCounter.PostCharge(t.Context(), coord.AgentScope("y"), 50, windows); err != nil {
		t.Fatalf("PostCharge: %v", err)
	}
	if _, err := counter.Refuse(t.Context(), coord.AgentScope("y"), seatCaps, windows); err != nil {
		t.Fatalf("Refuse: %v", err)
	}
	if !woke(l) {
		t.Fatal("a refusal recorded without a charge did not wake the meters")
	}
}

// A TURN'S METER AND A PERSON'S ANSWER CHARGE THROUGH THE COUNTER THAT TELLS.
//
// The two places a gate is built — every turn's meter, and the company's
// answer budget — each take this node's counter, and either one built over the
// bare fleet store would refuse with the header none the wiser.
func TestTheGatesChargeThroughTheCounterThatTellsTheMeters(t *testing.T) {
	t.Parallel()
	lead := &org.Role{Name: "Lead"}
	c := meteredCompany(config.TokenBudget{Day: ceiling(100)}, lead)
	e := &Engine{backends: &Backends{Fleet: coordmem.NewFleet()},
		clock: fixedClock(budgetNow), refusals: newRefusalLatch()}
	e.epoch.current.Store(c)

	if got, err := e.meterFor(c, lead.Handle()).Spend(t.Context(), 150); err != nil || got.OK {
		t.Fatalf("Spend = (%+v, %v), want the company's day to refuse it", got, err)
	}
	if !woke(e.refusals) {
		t.Fatal("a turn's refused round did not reach the live meters")
	}
	e.refusals.admitted(coord.OrgScope)
	if _, refused, err := AnswerBudget(e).Refusing(t.Context()); err != nil || !refused {
		t.Fatalf("Refusing = (%v, %v), want the full day to refuse the question", refused, err)
	}
	if !woke(e.refusals) {
		t.Error("a person's refused question did not reach the live meters")
	}
}

// THE METERS PUBLISH A WINDOW'S FIRST REFUSAL AT ONCE.
//
// The reporter publishes at start and then on its tick — an hour away here —
// so a frame inside the case can only be the one a refusal woke.
func TestTheMetersPublishAWindowsFirstRefusalAtOnce(t *testing.T) {
	t.Parallel()
	e := &Engine{refusals: newRefusalLatch()}
	r := &budgetReporter{engine: e}
	published := make(chan struct{}, 8)
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.loop(ctx, time.Hour, func(context.Context) { published <- struct{}{} })
	}()
	t.Cleanup(func() { stop(); <-done })
	await := func(what string) {
		t.Helper()
		select {
		case <-published:
		case <-time.After(10 * time.Second):
			t.Fatalf("waited 10s for %s", what)
		}
	}
	await("the frame at start")
	e.refusals.refused(coord.OrgScope, period.Day, "2026-09-23")
	await("the frame the first refusal asked for")
	e.refusals.refused(coord.OrgScope, period.Day, "2026-09-23")
	select {
	case <-published:
		t.Fatal("a repeat refusal in the window published a frame")
	case <-time.After(300 * time.Millisecond):
	}
}
