package node

import (
	"testing"
	"time"
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
		if outstanding, alarm := m.missing("ceo", ttl, start.Add(at)); alarm {
			t.Fatalf("a mailbox missing for %s of a %s lease raised an alarm; the next tick "+
				"retries it, and an alarm nobody can ignore is one everybody does", outstanding, ttl)
		}
	}

	if outstanding, alarm := m.missing("ceo", ttl, start.Add(ttl+time.Second)); !alarm {
		t.Fatalf("a mailbox missing for %s said nothing: past a lease its mail is being "+
			"dropped rather than retained, and nothing else in the engine reports that",
			outstanding)
	}

	// AND IT DOES NOT REPEAT ON EVERY TICK. The failure is not news; still
	// failing is, on the same interval and no other.
	if _, alarm := m.missing("ceo", ttl, start.Add(ttl+2*time.Second)); alarm {
		t.Fatal("a stranded mailbox re-alarmed inside its quiet window, so one stuck seat " +
			"can fill a log with its own retries")
	}
	if _, alarm := m.missing("ceo", ttl, start.Add(2*ttl+2*time.Second)); !alarm {
		t.Fatal("a mailbox still missing a lease after its first alarm said nothing again, " +
			"so the evidence rotates out of the log while the seat stays deaf")
	}

	// A mailbox that arrives clears the clock, so a seat that fails again
	// later gets the full grace rather than an alarm on its first tick.
	m.ensuredNow("ceo")
	if outstanding, alarm := m.missing("ceo", ttl, start.Add(3*ttl)); alarm || outstanding != 0 {
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

	m.adopt([]string{"ceo", "swe"}, map[string]struct{}{"ceo": {}, "swe": {}}, now)
	m.missing("swe", ttl, now)

	if missing, _ := m.diff([]string{"ceo"}, ttl, now); len(missing) != 0 {
		t.Fatalf("a company of one seat reported %v missing, want none", missing)
	}
	if _, held := m.ensured["swe"]; held {
		t.Fatal("a seat the company no longer has is still in the ensured set, so if it " +
			"returns after its mailbox was retired nothing will make it another one")
	}
	if _, held := m.missingSince["swe"]; held {
		t.Fatal("a removed seat still carries an outstanding-mailbox clock, which both grows " +
			"for ever and alarms about a seat nobody has")
	}

	// And on its return it is missing again, whatever this node remembered.
	if missing, _ := m.diff([]string{"ceo", "swe"}, ttl, now); len(missing) != 1 || missing[0] != "swe" {
		t.Fatalf("a returning seat reported %v missing, want [swe]", missing)
	}
}
