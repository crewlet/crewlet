package tracker

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// EVERY ONE OF A PERSON'S THREE INBOX LISTS HOLDS [MaxInboxEntries] AND
// REFUSES ONE MORE, AND AN ENTRY THE POSITION OR THE CLOCK ALREADY ANSWERS IS
// PRUNED RATHER THAN COUNTED.
//
// [applyInboxGesture] is pure over values so this rule can be exercised
// without a database, and nothing exercised it that way: the end-to-end case
// ([TestAListAtItsCeilingRefusesOneMoreInboxFull]) fills the read and snoozed
// lists through hundreds of real writes and never reaches the unread list at
// all. So an off-by-one on any list, or a ceiling check that skipped the
// unread one, shipped with every test green. This holds all three to the same
// three facts — a list one short of the ceiling takes one more, a full list
// refuses the next with [ErrInboxFull] naming the list, and a full list still
// takes a mark the position or the clock makes moot, because the prune runs
// before the count. The end-to-end case stays: it is what proves the decide
// judges the STORED record and that a refusal publishes nothing.
func TestEveryInboxListHoldsItsCeilingAndPrunesBeforeItCounts(t *testing.T) {
	t.Parallel()
	const stream = "CREWLET_TRACKER_LOG"
	seen := Position{Stream: stream, Generation: 1, Seq: 1_000}
	at := time.Date(2031, 4, 16, 9, 0, 0, 0, time.UTC)
	later := at.Add(24 * time.Hour)

	// TWO POOLS OF NOTICES, either side of the seen-through position, so a
	// list's entries are exceptions it has to hold — read marks ABOVE the
	// position, unread marks AT OR BELOW it — and a mark on the other side
	// is one the position already answers.
	positions := map[string]uint64{}
	notice := func(side string, i int) string {
		id := fmt.Sprintf("%s-%03d", side, i)
		seq := uint64(i + 1)
		if side == "above" {
			seq += seen.Seq
		}
		positions[id] = Position{Stream: stream, Generation: 1, Seq: seq}.packed()
		return id
	}
	entries := func(side string, n int, until *time.Time) []InboxEntry {
		out := make([]InboxEntry, 0, n)
		for i := range n {
			id := notice(side, i)
			out = append(out, InboxEntry{RecordID: id, Position: positions[id], Until: until})
		}
		return out
	}
	list := func(p *Person, what string) []InboxEntry {
		return map[string][]InboxEntry{
			"read": p.Read, "unread": p.Unread, "snoozed": p.Snoozed,
		}[what]
	}

	for _, c := range []struct {
		what string
		// stored is the list as the record holds it: one LIVE entry
		// short of its ceiling.
		stored func(*Person)
		// one is a gesture adding one entry the list must hold.
		one func(i int) InboxGesture
		// moot is a gesture adding one entry the list must NOT hold:
		// the position or the clock already answers it.
		moot InboxGesture
	}{
		{
			what:   "read",
			stored: func(p *Person) { p.Read = entries("above", MaxInboxEntries-1, nil) },
			one: func(i int) InboxGesture {
				return InboxGesture{Read: []string{notice("above", i)}}
			},
			// BELOW THE POSITION, which already reads as read.
			moot: InboxGesture{Read: []string{notice("below", 0)}},
		},
		{
			what:   "unread",
			stored: func(p *Person) { p.Unread = entries("below", MaxInboxEntries-1, nil) },
			one: func(i int) InboxGesture {
				return InboxGesture{Unread: []string{notice("below", i)}}
			},
			// ABOVE THE POSITION, which already reads as unread.
			moot: InboxGesture{Unread: []string{notice("above", 0)}},
		},
		{
			what: "snoozed",
			stored: func(p *Person) {
				p.Snoozed = entries("above", MaxInboxEntries-1, &later)
				// ONE STORED SNOOZE HAS RUN OUT, so it is an item
				// that is back rather than an entry — and the
				// list it leaves behind is one short of full.
				lapsed := at.Add(-time.Minute)
				p.Snoozed = append(p.Snoozed, InboxEntry{
					RecordID: notice("below", 0), Position: positions[notice("below", 0)],
					Until: &lapsed,
				})
			},
			one: func(i int) InboxGesture {
				return InboxGesture{Snooze: []Snooze{{RecordID: notice("above", i), Until: later}}}
			},
			// A SNOOZE ALREADY DUE is an item back in the inbox, not an
			// entry, whichever side of the position it sits.
			moot: InboxGesture{Snooze: []Snooze{{RecordID: notice("below", 1), Until: at}}},
		},
	} {
		t.Run(c.what, func(t *testing.T) {
			p := &Person{Handle: "ana", Generation: stream, SeenThrough: seen}
			c.stored(p)

			// ONE SHORT OF THE CEILING TAKES ONE MORE.
			if err := applyInboxGesture(p, c.one(MaxInboxEntries-1), positions, at); err != nil {
				t.Fatalf("the %s list one short of its ceiling refused one more: %v",
					c.what, err)
			}
			if got := len(list(p, c.what)); got != MaxInboxEntries {
				t.Fatalf("the %s list holds %d entries after filling it, want %d",
					c.what, got, MaxInboxEntries)
			}
			full := *p

			// A MOOT MARK ON A FULL LIST IS PRUNED, NOT COUNTED.
			moot := full
			if err := applyInboxGesture(&moot, c.moot, positions, at); err != nil {
				t.Fatalf("a full %s list refused a mark it does not hold: %v — the "+
					"prune has to run before the count", c.what, err)
			}
			if got := len(list(&moot, c.what)); got != MaxInboxEntries {
				t.Fatalf("a moot mark left the full %s list at %d entries, want %d",
					c.what, got, MaxInboxEntries)
			}

			// AND ONE PAST IT IS REFUSED, naming the list and the gesture
			// that fits.
			over := full
			err := applyInboxGesture(&over, c.one(MaxInboxEntries), positions, at)
			if !errors.Is(err, ErrInboxFull) {
				t.Fatalf("one more entry on a full %s list answered %v, want "+
					"ErrInboxFull", c.what, err)
			}
			if !strings.Contains(err.Error(), c.what+" list") ||
				!strings.Contains(err.Error(), "read_through") {
				t.Fatalf("the refusal does not name the %s list and "+
					"read_through: %v", c.what, err)
			}
		})
	}
}
