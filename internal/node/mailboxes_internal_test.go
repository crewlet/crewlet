package node

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/seat/placement"
)

// THE ALARM FIRES AT A THRESHOLD SOMEBODY ELSE ALREADY CHOSE, and it is the
// interval rather than the event that decides.
//
// A create that fails on one tick is retried on the next, nine times inside a
// shipped lease, so alarming on the first failure would put twelve lines a
// minute in the log for a condition that clears itself — and an operator who
// learns to ignore those has lost the one signal that a seat is silently
// dropping every event addressed to it. What is worth saying is that a seat
// has been without a mailbox for as long as the fleet already tolerates a
// seat being unserved.
//
// Tested on the value rather than through a log, because the arithmetic is
// the whole of it: a threshold exercised only by reading lines out of a
// logger is a threshold nobody re-measures.
func TestAMissingMailboxIsAlarmedOnlyOnceItOutlivesALease(t *testing.T) {
	const ttl = time.Minute
	m := newMailboxes()
	start := time.Now()

	// THE CONTROL. Inside the lease the retries are silent, however many
	// of them there are — this is the healthy shape of a broker blip.
	for _, at := range []time.Duration{0, ttl / 4, ttl / 2, ttl} {
		if outstanding, alarm := m.missing(testSeat("ceo").ID, ttl, start.Add(at)); alarm {
			t.Fatalf("a mailbox missing for %s of a %s lease raised an alarm; the next tick "+
				"retries it, and an alarm nobody can ignore is one everybody does", outstanding, ttl)
		}
	}

	if outstanding, alarm := m.missing(testSeat("ceo").ID, ttl, start.Add(ttl+time.Second)); !alarm {
		t.Fatalf("a mailbox missing for %s said nothing: past a lease its mail is being "+
			"dropped rather than retained, and nothing else in the engine reports that",
			outstanding)
	}

	// AND IT DOES NOT REPEAT ON EVERY TICK. The failure is not news; still
	// failing is, on the same interval and no other.
	if _, alarm := m.missing(testSeat("ceo").ID, ttl, start.Add(ttl+2*time.Second)); alarm {
		t.Fatal("a stranded mailbox re-alarmed inside its quiet window, so one stuck seat " +
			"can fill a log with its own retries")
	}
	if _, alarm := m.missing(testSeat("ceo").ID, ttl, start.Add(2*ttl+2*time.Second)); !alarm {
		t.Fatal("a mailbox still missing a lease after its first alarm said nothing again, " +
			"so the evidence rotates out of the log while the seat stays deaf")
	}

	// A mailbox that arrives clears the clock, so a seat that fails again
	// later gets the full grace rather than an alarm on its first tick.
	m.ensuredNow(testSeat("ceo").ID)
	if outstanding, alarm := m.missing(testSeat("ceo").ID, ttl, start.Add(3*ttl)); alarm || outstanding != 0 {
		t.Fatalf("after the mailbox was made, a later failure reported (%s, %v), want (0s, false): "+
			"the clock measures how long THIS seat has been without a mailbox", outstanding, alarm)
	}
}

// A SEAT THAT LEAVES THE COMPANY LEAVES THE SET, and that is a correctness
// rule rather than a memory one.
//
// A removed seat's mailbox is retired 24 hours later by the maintenance duty,
// so a handle that comes back — a role re-added, a rename undone — may have no
// mailbox at all. A node that remembered ensuring it would never make it
// another one, and the returning seat would drop every event addressed to it
// while every screen showed it healthy. The bounded maps are the side effect.
func TestASeatThatLeavesTheCompanyLeavesTheSet(t *testing.T) {
	const ttl = time.Minute
	m := newMailboxes()
	now := time.Now()

	m.adopt(testSeats("ceo", "swe"),
		map[uuid.UUID]struct{}{testSeat("ceo").ID: {}, testSeat("swe").ID: {}}, now)
	m.missing(testSeat("swe").ID, ttl, now)

	if missing, _ := m.diff(testSeats("ceo"), ttl, now); len(missing) != 0 {
		t.Fatalf("a company of one seat reported %v missing, want none", missing)
	}
	if _, held := m.ensured[testSeat("swe").ID]; held {
		t.Fatal("a seat the company no longer has is still in the ensured set, so if it " +
			"returns after its mailbox was retired nothing will make it another one")
	}
	if _, held := m.missingSince[testSeat("swe").ID]; held {
		t.Fatal("a removed seat still carries an outstanding-mailbox clock, which both grows " +
			"for ever and alarms about a seat nobody has")
	}

	// And on its return it is missing again, whatever this node remembered.
	if missing, _ := m.diff(testSeats("ceo", "swe"), ttl, now); len(missing) != 1 ||
		missing[0].Handle != "swe" {

		t.Fatalf("a returning seat reported %v missing, want [swe]", missing)
	}
}

// testSeat is the seat a case calls handle: the id every durable name for it
// is built from, and the handle a log line reads.
//
// Derived rather than random, so a case can write the same seat twice and get
// the same id — which is what the ensured set is keyed on.
func testSeat(handle string) placement.Seat {
	return placement.Seat{
		ID: uuid.NewSHA1(uuid.MustParse("6f1c2a4e-9d3b-4a71-8f5e-2b0c7d81a940"),
			[]byte(handle)),
		Handle: handle,
	}
}

// testSeats is [testSeat] over a company's worth of them.
func testSeats(handles ...string) []placement.Seat {
	out := make([]placement.Seat, 0, len(handles))
	for _, handle := range handles {
		out = append(out, testSeat(handle))
	}
	return out
}
