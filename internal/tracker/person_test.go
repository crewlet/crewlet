package tracker_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

func (r *roundTrip) person(handle string) tracker.PersonState {
	r.t.Helper()
	state, err := r.reader.Person(r.t.Context(), tracker.PersonQuery{
		Handle: handle, Level: statelog.ReadStale,
	}, wednesday)
	if err != nil {
		r.t.Fatalf("Person(%q): %v", handle, err)
	}
	return state
}

// AN INBOX IS WRITTEN ONLY ON BEHALF OF THE PERSON WHOSE IT IS.
//
// Somebody else marking your work read is the one thing an inbox must never
// allow: the item is then gone from the only place you would have looked for
// it, and nothing anywhere says who removed it.
func TestNobodyElseWritesYourInboxOrYourPins(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	bob := r.writer.As("bob", tracker.AuthorHuman, tracker.Provenance{})

	for name, write := range map[string]func() error{
		"the inbox": func() error {
			_, err := bob.WriteInbox(t.Context(), "op-inbox", "ana", nil, nil,
				nil, nil, tracker.Position{})
			return err
		},
		"the pins": func() error {
			_, err := bob.WritePins(t.Context(), "op-pins", "ana",
				[]string{"v-1"}, nil)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := write()
			if err == nil {
				t.Fatalf("bob wrote ana's %s", name)
			}
			if !errors.Is(err, statelog.ErrConflict) {
				t.Fatalf("the refusal is %v, want an ErrConflict a caller can "+
					"branch on", err)
			}
		})
	}

	// AND HER OWN WRITES LAND, or the guard would be refusing everything.
	if _, err := r.writer.WriteInbox(t.Context(), "op-own-inbox", "ana",
		nil, []tracker.InboxEntry{{RecordID: "rec-1", Position: 9}}, nil,
		[]tracker.Reason{tracker.ReasonAssignee},
		tracker.Position{Stream: "CREWLET_TRACKER_LOG", Seq: 5}); err != nil {
		t.Fatalf("ana's own inbox write: %v", err)
	}
	r.drain()
	if got := r.person("ana"); len(got.Unread) != 1 {
		t.Fatalf("ana's unread is %v, want the one entry she wrote", got.Unread)
	}
}

// A LEAD MAY SET WHAT SOMEBODY IN THEIR LINE DOES NEXT, and the write SAYS SO.
//
// A lead silently re-ordering somebody's queue is a person who starts the day
// on work they did not choose and cannot tell why.
func TestALeadsPriorityWriteSaysWhoSetIt(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	bob := r.writer.As("bob", tracker.AuthorHuman, tracker.Provenance{})

	// WITHOUT THE AUTHORITY IT IS REFUSED, which is what stops "a lead
	// may" from meaning "anybody may".
	_, err := bob.WritePriorities(t.Context(), "op-nope", "ana",
		[]string{"t-1"}, tracker.PersonAuthority{})
	if err == nil {
		t.Fatal("a colleague set somebody else's priorities")
	}
	if !errors.Is(err, statelog.ErrConflict) {
		t.Fatalf("the refusal is %v, want an ErrConflict", err)
	}

	if _, err := bob.WritePriorities(t.Context(), "op-lead", "ana",
		[]string{"t-1", "t-2"}, tracker.PersonAuthority{Lead: true}); err != nil {
		t.Fatalf("bob leads ana and was refused: %v", err)
	}
	r.drain()

	got := r.person("ana")
	if len(got.Priorities) != 2 {
		t.Fatalf("ana's priorities are %v, want the two bob set", got.Priorities)
	}
	if got.PrioritiesSetBy != "bob" {
		t.Fatalf("the list says it was set by %q, want bob — a queue somebody "+
			"else chose and nothing says so is the failure this stamp exists "+
			"for", got.PrioritiesSetBy)
	}
	if got.PrioritiesSetAt.IsZero() {
		t.Fatal("the stamp carries no time, so a list set six weeks ago reads " +
			"exactly like one set this morning")
	}

	// AND HER OWN NEXT WRITE CLEARS IT, because taking your queue back is
	// the gesture that says you have seen it.
	if _, err := r.writer.WritePriorities(t.Context(), "op-own", "ana",
		[]string{"t-2"}, tracker.PersonAuthority{}); err != nil {
		t.Fatalf("ana's own write: %v", err)
	}
	r.drain()
	if got := r.person("ana"); got.PrioritiesSetBy != "" {
		t.Fatalf("the stamp still says %q after ana took her own list back",
			got.PrioritiesSetBy)
	}
}

// EACH VERB CARRIES THE OTHER PARTS THROUGH UNCHANGED.
//
// The three write disjoint parts of one document, so a verb that took the
// whole thing from its caller would let an inbox write clear somebody's pins
// by omission — the ordinary shape of a lost update, and the one a
// whole-document object invites.
func TestOnePartOfAPersonDoesNotClearTheOthers(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	if _, err := r.writer.WritePins(t.Context(), "op-pins", "ana",
		[]string{"v-1", "v-2"},
		[]tracker.Favorite{{Kind: "project", ID: "ENG"}}); err != nil {
		t.Fatalf("WritePins: %v", err)
	}
	r.drain()
	if _, err := r.writer.WritePriorities(t.Context(), "op-prio", "ana",
		[]string{"t-1"}, tracker.PersonAuthority{}); err != nil {
		t.Fatalf("WritePriorities: %v", err)
	}
	r.drain()
	if _, err := r.writer.WriteInbox(t.Context(), "op-inbox", "ana", nil,
		[]tracker.InboxEntry{{RecordID: "rec-1", Position: 9}}, nil, nil,
		tracker.Position{Stream: "CREWLET_TRACKER_LOG", Seq: 3}); err != nil {
		t.Fatalf("WriteInbox: %v", err)
	}
	r.drain()

	got := r.person("ana")
	switch {
	case len(got.PinnedViews) != 2:
		t.Fatalf("the pins are %v after two later writes, want both",
			got.PinnedViews)
	case len(got.Favorites) != 1:
		t.Fatalf("the favourites are %v, want the one she starred", got.Favorites)
	case len(got.Priorities) != 1:
		t.Fatalf("the priorities are %v, want the one she set", got.Priorities)
	case len(got.Unread) != 1:
		t.Fatalf("the unread is %v, want the one entry", got.Unread)
	}
}

// AN INBOX ENTRY THE PERSON HAS READ PAST IS PRUNED ON THE WRITE.
//
// That is what keeps this object small without a cap that DISCARDS: an entry
// at or below the seen-through position is one no surface will ever render.
func TestAnInboxPrunesWhatWasReadPast(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	entries := []tracker.InboxEntry{
		{RecordID: "old", Position: 2},
		{RecordID: "edge", Position: 5},
		{RecordID: "new", Position: 9},
	}
	if _, err := r.writer.WriteInbox(t.Context(), "op-inbox", "ana", nil,
		entries, nil, nil,
		tracker.Position{Stream: "CREWLET_TRACKER_LOG", Seq: 5}); err != nil {
		t.Fatalf("WriteInbox: %v", err)
	}
	r.drain()

	got := r.person("ana")
	if len(got.Unread) != 1 || got.Unread[0].RecordID != "new" {
		t.Fatalf("the unread is %v, want only what is above the seen-through "+
			"position", got.Unread)
	}
}

// A DUE SNOOZE IS REPORTED, NEVER PROMOTED.
//
// Putting one back in the unread list is a WRITE, and a read that performed
// one would change the fleet's state from a path with no operation id, no
// arbitration and no record.
func TestADueSnoozeIsReportedRatherThanPromoted(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	future := wednesday.Add(time.Hour)
	if _, err := r.writer.WriteInbox(t.Context(), "op-inbox", "ana", nil, nil,
		[]tracker.InboxEntry{
			{RecordID: "later", Position: 9, Until: &future},
		}, nil, tracker.Position{Stream: "CREWLET_TRACKER_LOG", Seq: 1}); err != nil {
		t.Fatalf("WriteInbox: %v", err)
	}
	r.drain()

	// THE READ'S OWN CLOCK DECIDES, so the same rows answer differently as
	// the hour passes — which is the whole point of a snooze.
	state, err := r.reader.Person(t.Context(), tracker.PersonQuery{
		Handle: "ana", Level: statelog.ReadStale,
	}, wednesday.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("Person: %v", err)
	}
	if len(state.Due) != 1 || len(state.Snoozed) != 0 {
		t.Fatalf("snoozed=%v due=%v, want the entry reported due",
			state.Snoozed, state.Due)
	}
	// AND IT IS STILL IN THE SNOOZED LIST ON THE ROW, because nothing
	// promoted it: the person's next inbox write is what moves it.
	if got := r.person("ana"); len(got.Snoozed) != 1 {
		t.Fatalf("the stored snooze is %v, want the read to have changed "+
			"nothing", got.Snoozed)
	}
}

// A PERSON NOBODY HAS WRITTEN IS AN EMPTY PERSON, not a missing one.
//
// Every human starts with no inbox, no pins and no priorities, and there is no
// gesture that creates the record — the first write does.
func TestAPersonNobodyHasWrittenReadsEmpty(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	got := r.person("nobody")
	switch {
	case got.Held:
		t.Fatal("a person nobody has written reports as held")
	case len(got.Unread) != 0 || len(got.PinnedViews) != 0:
		t.Fatalf("an unwritten person carries state: %+v", got)
	case got.Handle != "nobody":
		t.Fatalf("the answer is about %q", got.Handle)
	}

	// AND THE READ REFUSES WHAT IT CANNOT ANSWER, rather than reporting an
	// empty person for a question nobody asked.
	if _, err := r.reader.Person(t.Context(), tracker.PersonQuery{
		Level: statelog.ReadStale,
	}, wednesday); err == nil {
		t.Fatal("a person read naming nobody was answered")
	}
	if _, err := r.reader.Person(t.Context(), tracker.PersonQuery{
		Handle: "ana",
	}, wednesday); err == nil {
		t.Fatal("a person read with no read level was answered")
	}
}

// THE CAPS ARE REFUSED NAMING THE FIELD, never cut.
func TestAPersonsListsAreBounded(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	many := func(n int) []string {
		out := make([]string, 0, n)
		for i := range n {
			out = append(out, "x-"+itoa(i))
		}
		return out
	}
	_, err := r.writer.WritePins(t.Context(), "op-pins", "ana",
		many(tracker.MaxPinnedViews+1), nil)
	if err == nil || !strings.Contains(err.Error(), "pins") {
		t.Fatalf("an unbounded pin list was accepted: %v", err)
	}
	_, err = r.writer.WritePriorities(t.Context(), "op-prio", "ana",
		many(tracker.MaxPriorities+1), tracker.PersonAuthority{})
	if err == nil || !strings.Contains(err.Error(), "priority list") {
		t.Fatalf("an unbounded priority list was accepted: %v", err)
	}
	entries := make([]tracker.InboxEntry, 0, tracker.MaxInboxEntries+1)
	for i := range tracker.MaxInboxEntries + 1 {
		entries = append(entries, tracker.InboxEntry{
			RecordID: "r-" + itoa(i), Position: uint64(i + 1),
		})
	}
	_, err = r.writer.WriteInbox(t.Context(), "op-inbox", "ana", nil, entries,
		nil, nil, tracker.Position{})
	if err == nil || !strings.Contains(err.Error(), "unread list") {
		t.Fatalf("an unbounded inbox was accepted: %v", err)
	}
	// A SNOOZE PAST THE HORIZON is a delete that does not say so.
	far := wednesday.Add(tracker.MaxSnoozeAhead + 24*time.Hour)
	_, err = r.writer.WriteInbox(t.Context(), "op-far", "ana", nil, nil,
		[]tracker.InboxEntry{{RecordID: "r-1", Position: 1, Until: &far}},
		nil, tracker.Position{})
	if err == nil || !strings.Contains(err.Error(), "delete that does not say so") {
		t.Fatalf("a snooze past the horizon was accepted: %v", err)
	}
	// AND ONE INSIDE IT LANDS, or the guard would refuse every snooze.
	near := wednesday.Add(24 * time.Hour)
	if _, err := r.writer.WriteInbox(t.Context(), "op-near", "ana", nil, nil,
		[]tracker.InboxEntry{{RecordID: "r-1", Position: 1, Until: &near}},
		nil, tracker.Position{}); err != nil {
		t.Fatalf("an ordinary snooze was refused: %v", err)
	}

	// AND A STAR THAT POINTS AT NOTHING is refused too.
	_, err = r.writer.WritePins(t.Context(), "op-fav", "ana", nil,
		[]tracker.Favorite{{Kind: "project"}})
	if err == nil || !strings.Contains(err.Error(), "points at nothing") {
		t.Fatalf("a favourite with no id was accepted: %v", err)
	}
}
