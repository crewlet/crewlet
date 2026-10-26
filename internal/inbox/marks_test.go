package inbox_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// reason is a domain's reason, as these cases route under one.
type reason string

const stream = "CREWLET_TRACKER_LOG"

// EVERY ONE OF A PERSON'S THREE INBOX LISTS HOLDS [inbox.MaxEntries] AND
// REFUSES ONE MORE, AND AN ENTRY THE POSITION OR THE CLOCK ALREADY ANSWERS IS
// PRUNED RATHER THAN COUNTED.
//
// [inbox.Apply] is pure over values so this rule can be exercised without a
// database, and nothing exercised it that way: the tracker's end-to-end case
// fills the read and snoozed lists through hundreds of real writes and never
// reaches the unread list at all. So an off-by-one on any list, or a ceiling
// check that skipped the unread one, shipped with every test green. This holds
// all three to the same three facts — a list one short of the ceiling takes
// one more, a full list refuses the next with [inbox.ErrFull] naming the list,
// and a full list still takes a mark the position or the clock makes moot,
// because the prune runs before the count. The end-to-end case stays in the
// tracker: it is what proves the decide judges the STORED record and that a
// refusal publishes nothing.
//
// THE BOUNDARY IS THE POSITION ITSELF, and both lists meet it: the notice AT
// the seen-through position reads as read, so a read mark on it is pruned and
// an unread mark on it is an exception the list holds and counts. Pools that
// only straddled the position left both comparisons free to move by one —
// keeping unread marks strictly below it silently dropped every one on the
// notice somebody read through, and the whole package stayed green.
func TestEveryInboxListHoldsItsCeilingAndPrunesBeforeItCounts(t *testing.T) {
	t.Parallel()
	seen := inbox.Position{Stream: stream, Generation: 1, Seq: 1_000}
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
		positions[id] = inbox.Position{Stream: stream, Generation: 1, Seq: seq}.Packed()
		return id
	}
	onSeen := notice("at", 0)
	entries := func(side string, n int, until *time.Time) []inbox.Entry {
		out := make([]inbox.Entry, 0, n)
		for i := range n {
			id := notice(side, i)
			out = append(out, inbox.Entry{RecordID: id, Position: positions[id], Until: until})
		}
		return out
	}
	list := func(m *inbox.Marks[reason], what string) []inbox.Entry {
		return map[string][]inbox.Entry{
			"read": m.Read, "unread": m.Unread, "snoozed": m.Snoozed,
		}[what]
	}

	for _, c := range []struct {
		what string
		// stored is the list as the record holds it: one LIVE entry
		// short of its ceiling.
		stored func(*inbox.Marks[reason])
		// one is a gesture adding one entry the list must hold.
		one func(i int) inbox.Gesture[reason]
		// moot is a gesture adding one entry the list must NOT hold:
		// the position or the clock already answers it.
		moot inbox.Gesture[reason]
	}{
		{
			what: "read",
			stored: func(m *inbox.Marks[reason]) {
				m.Read = entries("above", inbox.MaxEntries-1, nil)
			},
			one: func(i int) inbox.Gesture[reason] {
				return inbox.Gesture[reason]{Read: []string{notice("above", i)}}
			},
			// AT AND BELOW THE POSITION, which already read as read —
			// the notice AT it included, which a list keeping marks at
			// or above the position would hold.
			moot: inbox.Gesture[reason]{Read: []string{onSeen, notice("below", 0)}},
		},
		{
			what: "unread",
			stored: func(m *inbox.Marks[reason]) {
				// ONE OF THEM ON THE NOTICE AT THE POSITION — read
				// through and kept unread — which the list holds and
				// counts like every other exception.
				m.Unread = append(entries("below", inbox.MaxEntries-2, nil),
					inbox.Entry{RecordID: onSeen, Position: positions[onSeen]})
			},
			one: func(i int) inbox.Gesture[reason] {
				return inbox.Gesture[reason]{Unread: []string{notice("below", i)}}
			},
			// JUST ABOVE THE POSITION, which already reads as unread.
			moot: inbox.Gesture[reason]{Unread: []string{notice("above", 0)}},
		},
		{
			what: "snoozed",
			stored: func(m *inbox.Marks[reason]) {
				m.Snoozed = entries("above", inbox.MaxEntries-1, &later)
				// ONE STORED SNOOZE HAS RUN OUT, so it is a notice
				// that is back rather than an entry — and the list it
				// leaves behind is one short of full.
				lapsed := at.Add(-time.Minute)
				m.Snoozed = append(m.Snoozed, inbox.Entry{
					RecordID: notice("below", 0), Position: positions[notice("below", 0)],
					Until: &lapsed,
				})
			},
			one: func(i int) inbox.Gesture[reason] {
				return inbox.Gesture[reason]{Snooze: []inbox.Snooze{
					{RecordID: notice("above", i), Until: later}}}
			},
			// A SNOOZE ALREADY DUE is a notice back in the inbox, not an
			// entry, whichever side of the position it sits.
			moot: inbox.Gesture[reason]{Snooze: []inbox.Snooze{
				{RecordID: notice("below", 1), Until: at}}},
		},
	} {
		t.Run(c.what, func(t *testing.T) {
			m := &inbox.Marks[reason]{Generation: stream, SeenThrough: seen}
			c.stored(m)

			// ONE SHORT OF THE CEILING TAKES ONE MORE.
			if err := inbox.Apply(m, c.one(inbox.MaxEntries-1), positions, at); err != nil {
				t.Fatalf("the %s list one short of its ceiling refused one more: %v",
					c.what, err)
			}
			if got := len(list(m, c.what)); got != inbox.MaxEntries {
				t.Fatalf("the %s list holds %d entries after filling it, want %d",
					c.what, got, inbox.MaxEntries)
			}
			filled := *m

			// A MOOT MARK ON A FULL LIST IS PRUNED, NOT COUNTED.
			moot := filled
			if err := inbox.Apply(&moot, c.moot, positions, at); err != nil {
				t.Fatalf("a full %s list refused a mark it does not hold: %v — the "+
					"prune has to run before the count", c.what, err)
			}
			if got := len(list(&moot, c.what)); got != inbox.MaxEntries {
				t.Fatalf("a moot mark left the full %s list at %d entries, want %d",
					c.what, got, inbox.MaxEntries)
			}

			// AND ONE PAST IT IS REFUSED, naming the list and the gesture
			// that fits.
			over := filled
			err := inbox.Apply(&over, c.one(inbox.MaxEntries), positions, at)
			if !errors.Is(err, inbox.ErrFull) {
				t.Fatalf("one more entry on a full %s list answered %v, want "+
					"ErrFull", c.what, err)
			}
			// THE POSSESSIVE, because "read list" is also the tail of
			// "unread list".
			if said := inbox.Sentence(err); !strings.Contains(said, "'s "+c.what+" list") ||
				!strings.Contains(said, "read_through") {
				t.Fatalf("the refusal does not name the %s list and "+
					"read_through: %v", c.what, err)
			}
		})
	}
}

// "READ THROUGH HERE, BUT KEEP THIS ONE UNREAD" IS ONE GESTURE THAT MEANS
// WHAT IT SAYS — the case [inbox.Apply] orders its work for.
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
	at := time.Date(2031, 4, 16, 9, 0, 0, 0, time.UTC)
	through := statelog.Position{Stream: stream, Generation: 1, Seq: 1_000}
	packed := func(seq uint64) uint64 {
		return inbox.Position{Stream: stream, Generation: 1, Seq: seq}.Packed()
	}
	positions := map[string]uint64{
		"the-one-read-through": packed(through.Seq),
		"one-past-it":          packed(through.Seq + 1),
	}
	m := &inbox.Marks[reason]{
		Generation:  stream,
		SeenThrough: inbox.Position{Stream: stream, Generation: 1, Seq: 500},
		// AN OLDER EXCEPTION, which reading further through is the
		// gesture that clears.
		Unread: []inbox.Entry{{RecordID: "an-older-one", Position: packed(10)}},
	}

	if err := inbox.Apply(m, inbox.Gesture[reason]{
		ReadThrough: &through,
		Unread:      []string{"the-one-read-through", "one-past-it"},
	}, positions, at); err != nil {
		t.Fatalf("read through a notice and keep it unread: %v", err)
	}

	if want := (inbox.Position{Stream: stream, Generation: 1, Seq: through.Seq}); m.SeenThrough != want {
		t.Errorf("the position is %+v after reading through %+v", m.SeenThrough, want)
	}
	// THE MARK AT THE POSITION SURVIVES; the older exception is cleared by
	// the read-through, and the mark one past the position is pruned,
	// since everything above it already reads as unread.
	want := []inbox.Entry{{RecordID: "the-one-read-through", Position: packed(through.Seq)}}
	if !slices.Equal(m.Unread, want) {
		t.Errorf("the unread list is %+v, want %+v — the notice read "+
			"through and kept unread in the same gesture", m.Unread, want)
	}
}

// A GESTURE NO RECORD COULD HONOUR IS REFUSED BEFORE ANYTHING IS READ, AND
// EVERY REFUSAL IS OF ITS CONTENT — in one of two classes a domain's writer
// tells apart.
//
// [inbox.ErrFull] is the gesture naming more notices than a list holds, whose
// remedy is `read_through`; [inbox.ErrInvalid] is every other, which a
// domain's writer classes as its OWN content refusal with the sentence
// unchanged — so the sentence must carry no package prefix, or a person would
// read "inbox:" in front of what to change. A refusal that slipped into the
// wrong class, or no class, would reach a person's surface as this node's
// failure ("did not land") rather than as what to change.
//
// Mutation: drop the stream check, the reason check or the horizon, and a
// case is accepted; build the full refusal as an invalid one, and it fails
// its class.
func TestAGestureNoRecordCouldHonourIsRefusedForWhatItSays(t *testing.T) {
	t.Parallel()
	now := time.Date(2031, 4, 16, 9, 0, 0, 0, time.UTC)
	routed := []reason{"mention", "assignee"}
	many := make([]string, inbox.MaxEntries+1)
	for i := range many {
		many[i] = fmt.Sprintf("r-%03d", i)
	}
	for _, c := range []struct {
		name    string
		gesture inbox.Gesture[reason]
		class   error
		says    string
	}{
		{"one that marks nothing", inbox.Gesture[reason]{}, inbox.ErrInvalid, "marks nothing"},
		{"a mark naming no record", inbox.Gesture[reason]{Read: []string{" "}},
			inbox.ErrInvalid, "names no record"},
		{"one notice both read and unread", inbox.Gesture[reason]{
			Read: []string{"r-1"}, Unread: []string{"r-1"}}, inbox.ErrInvalid, "both read and unread"},
		{"a snooze already due", inbox.Gesture[reason]{
			Snooze: []inbox.Snooze{{RecordID: "r-1", Until: now}}}, inbox.ErrInvalid, "already come"},
		{"a snooze past the horizon", inbox.Gesture[reason]{
			Snooze: []inbox.Snooze{{RecordID: "r-1",
				Until: now.Add(inbox.MaxSnoozeAhead + time.Hour)}}}, inbox.ErrInvalid, "does not say so"},
		{"more notices than a list holds", inbox.Gesture[reason]{Read: many},
			inbox.ErrFull, "read_through"},
		{"a read-through on another log", inbox.Gesture[reason]{
			ReadThrough: &statelog.Position{Stream: "CREWLET_SOMETHING_ELSE", Seq: 4}},
			inbox.ErrInvalid, "the log its notices are filed on"},
		{"a reason this domain does not route under", inbox.Gesture[reason]{
			PrimaryReasons: &[]reason{"mention", "gossip"}}, inbox.ErrInvalid, `"gossip"`},
	} {
		err := inbox.CheckGesture(c.gesture, stream, routed, now)
		if !errors.Is(err, c.class) {
			t.Errorf("%s answered %v, want %v", c.name, err, c.class)
			continue
		}
		said := inbox.Sentence(err)
		if !strings.Contains(said, c.says) || strings.HasPrefix(said, "inbox:") {
			t.Errorf("%s was refused saying %q, want the sentence alone, naming %q",
				c.name, said, c.says)
		}
	}
	// AND A GESTURE EVERY RECORD CAN TAKE passes, the declaration of the
	// split included.
	if err := inbox.CheckGesture(inbox.Gesture[reason]{
		Read:           []string{"r-1"},
		Snooze:         []inbox.Snooze{{RecordID: "r-2", Until: now.Add(time.Hour)}},
		ReadThrough:    &statelog.Position{Stream: stream, Seq: 4},
		PrimaryReasons: &[]reason{"mention"},
	}, stream, routed, now); err != nil {
		t.Errorf("a gesture every record can take was refused: %v", err)
	}
}
