package tracker

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
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
//
// THE BOUNDARY IS THE POSITION ITSELF, and both lists meet it: the notice AT
// the seen-through position reads as read, so a read mark on it is pruned and
// an unread mark on it is an exception the list holds and counts. Pools that
// only straddled the position left both comparisons free to move by one —
// keeping unread marks strictly below it silently dropped every one on the
// notice somebody read through, and the whole package stayed green.
func TestEveryInboxListHoldsItsCeilingAndPrunesBeforeItCounts(t *testing.T) {
	t.Parallel()
	const stream = "CREWLET_TRACKER_LOG"
	seen := Position{Stream: stream, Generation: 1, Seq: 1_000}
	at := time.Date(2031, 4, 16, 9, 0, 0, 0, time.UTC)
	later := at.Add(24 * time.Hour)

	// THREE POOLS OF NOTICES around the seen-through position, so a list's
	// entries are exceptions it has to hold — read marks ABOVE the position,
	// unread marks AT OR BELOW it — and a mark on the other side is one the
	// position already answers. The pools touch it from both sides: the
	// first notice above sits one past it, and the one notice AT it is the
	// position itself.
	positions := map[string]uint64{}
	notice := func(side string, i int) string {
		id := fmt.Sprintf("%s-%03d", side, i)
		seq := map[string]uint64{
			"below": uint64(i + 1),
			"at":    seen.Seq,
			"above": seen.Seq + 1 + uint64(i),
		}[side]
		positions[id] = Position{Stream: stream, Generation: 1, Seq: seq}.packed()
		return id
	}
	onSeen := notice("at", 0)
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
			// AT AND BELOW THE POSITION, which already read as read —
			// the notice AT it included, which a list keeping marks at
			// or above the position would hold.
			moot: InboxGesture{Read: []string{onSeen, notice("below", 0)}},
		},
		{
			what: "unread",
			stored: func(p *Person) {
				// ONE OF THEM ON THE NOTICE AT THE POSITION — read
				// through and kept unread — which the list holds and
				// counts like every other exception.
				p.Unread = append(entries("below", MaxInboxEntries-2, nil),
					InboxEntry{RecordID: onSeen, Position: positions[onSeen]})
			},
			one: func(i int) InboxGesture {
				return InboxGesture{Unread: []string{notice("below", i)}}
			},
			// JUST ABOVE THE POSITION, which already reads as unread.
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
			// THE POSSESSIVE, because "read list" is also the tail
			// of "unread list".
			if !strings.Contains(err.Error(), "'s "+c.what+" list") ||
				!strings.Contains(err.Error(), "read_through") {
				t.Fatalf("the refusal does not name the %s list and "+
					"read_through: %v", c.what, err)
			}
		})
	}
}

// "READ THROUGH HERE, BUT KEEP THIS ONE UNREAD" IS ONE GESTURE THAT MEANS
// WHAT IT SAYS — the case [applyInboxGesture] orders its work for.
//
// Moving the position forward clears every unread exception, so the position
// has to move BEFORE the marks are laid, or the one the person asked to keep
// is cleared by the gesture that made it. And the mark it keeps is on the
// very notice read through — AT the new position, which reads as read and so
// is exactly the exception the unread list exists for. Nothing exercised
// this: a gesture keeping unread marks strictly below the position dropped
// it, and every tracker test passed.
func TestReadingThroughANoticeKeepsItUnreadInOneGesture(t *testing.T) {
	t.Parallel()
	const stream = "CREWLET_TRACKER_LOG"
	at := time.Date(2031, 4, 16, 9, 0, 0, 0, time.UTC)
	through := statelog.Position{Stream: stream, Generation: 1, Seq: 1_000}
	packed := func(seq uint64) uint64 {
		return Position{Stream: stream, Generation: 1, Seq: seq}.packed()
	}
	positions := map[string]uint64{
		"the-one-read-through": packed(through.Seq),
		"one-past-it":          packed(through.Seq + 1),
	}
	p := &Person{
		Handle: "ana", Generation: stream,
		SeenThrough: Position{Stream: stream, Generation: 1, Seq: 500},
		// AN OLDER EXCEPTION, which reading further through is the
		// gesture that clears.
		Unread: []InboxEntry{{RecordID: "an-older-one", Position: packed(10)}},
	}

	if err := applyInboxGesture(p, InboxGesture{
		ReadThrough: &through,
		Unread:      []string{"the-one-read-through", "one-past-it"},
	}, positions, at); err != nil {
		t.Fatalf("read through a notice and keep it unread: %v", err)
	}

	if want := (Position{Stream: stream, Generation: 1, Seq: through.Seq}); p.SeenThrough != want {
		t.Errorf("the position is %+v after reading through %+v", p.SeenThrough, want)
	}
	// THE MARK AT THE POSITION SURVIVES; the older exception is cleared by
	// the read-through, and the mark one past the position is pruned,
	// since everything above it already reads as unread.
	want := []InboxEntry{{RecordID: "the-one-read-through", Position: packed(through.Seq)}}
	if !slices.Equal(p.Unread, want) {
		t.Errorf("the unread list is %+v, want %+v — the notice read "+
			"through and kept unread in the same gesture", p.Unread, want)
	}
}
