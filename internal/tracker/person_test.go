package tracker_test

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

func (r *roundTrip) person(handle string) tracker.PersonState {
	r.t.Helper()
	state, err := r.reader.Person(r.t.Context(), tracker.PersonQuery{
		Who: tracker.PartyOf(handle), Level: statelog.ReadStale,
	}, wednesday)
	if err != nil {
		r.t.Fatalf("Person(%q): %v", handle, err)
	}
	return state
}

// noticesFor routes n changes at somebody, made by bob so the person they
// reach is not their author, and answers the notices OLDEST FIRST.
func noticesFor(t *testing.T, r *roundTrip, handle string, n int) []tracker.InboxNotice {
	t.Helper()
	bob := r.writer.As("bob", tracker.AuthorHuman, tracker.Provenance{})
	for i := range n {
		routeAs(t, r, bob, "t-"+handle+"-"+itoa(i), "ENG-"+itoa(i+1), handle)
	}
	got := r.inbox(tracker.InboxQuery{Who: tracker.PartyOf(handle)})
	if len(got.Notices) != n {
		t.Fatalf("%s's inbox holds %d notices, want %d", handle, len(got.Notices), n)
	}
	slices.Reverse(got.Notices)
	return got.Notices
}

// positionOf is a notice's own position, as a read-through names it.
func positionOf(n tracker.InboxNotice) *statelog.Position {
	return &statelog.Position{Stream: n.LogStream, Generation: n.LogGeneration, Seq: n.LogSeq}
}

// marks is ana's inbox as three sets of record ids, read at an instant.
func (r *roundTrip) marks(handle string, at time.Time) (read, snoozed map[string]bool) {
	r.t.Helper()
	answer, err := r.reader.Inbox(r.t.Context(), tracker.InboxQuery{
		Who: tracker.PartyOf(handle), Snoozed: tracker.SnoozeInclude, Level: statelog.ReadStale,
	}, at)
	if err != nil {
		r.t.Fatalf("Inbox: %v", err)
	}
	read, snoozed = map[string]bool{}, map[string]bool{}
	for _, n := range answer.Notices {
		read[n.RecordID], snoozed[n.RecordID] = n.Read, n.Snoozed
	}
	return read, snoozed
}

// AN INBOX IS WRITTEN ONLY ON BEHALF OF THE PERSON WHOSE IT IS.
//
// Somebody else marking your work read is the one thing an inbox must never
// allow: the item is then gone from the only place you would have looked for
// it, and nothing anywhere says who removed it.
func TestNobodyElseWritesYourInboxOrYourPins(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	notices := noticesFor(t, r, "ana", 1)
	bob := r.writer.As("bob", tracker.AuthorHuman, tracker.Provenance{})

	for name, write := range map[string]func() error{
		"the inbox": func() error {
			_, err := bob.MarkInbox(t.Context(), "op-inbox", "ana",
				tracker.InboxGesture{Read: []string{notices[0].RecordID}})
			return err
		},
		"the pins": func() error {
			_, err := bob.WritePins(t.Context(), "op-pins", "ana",
				tracker.PinGesture{Views: tracker.SetChange[string]{Add: []string{"v-1"}}})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := write()
			if err == nil {
				t.Fatalf("bob wrote ana's %s", name)
			}
			if !errors.Is(err, tracker.ErrForbidden) {
				t.Fatalf("the refusal is %v, want an ErrForbidden a caller can "+
					"branch on — not a conflict, which invites a retry that "+
					"cannot land", err)
			}
		})
	}

	// AND HER OWN WRITES LAND, or the guard would be refusing everything.
	if _, err := r.writer.MarkInbox(t.Context(), "op-own-inbox", "ana",
		tracker.InboxGesture{Read: []string{notices[0].RecordID}}); err != nil {
		t.Fatalf("ana's own inbox write: %v", err)
	}
	r.drain()
	if got := r.person("ana"); len(got.Read) != 1 {
		t.Fatalf("ana's read list is %v, want the one notice she marked", got.Read)
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
		[]string{"t-1"}, nil, tracker.PersonAuthority{})
	if err == nil {
		t.Fatal("a colleague set somebody else's priorities")
	}
	if !errors.Is(err, tracker.ErrForbidden) {
		t.Fatalf("the refusal is %v, want an ErrForbidden", err)
	}

	if _, err := bob.WritePriorities(t.Context(), "op-lead", "ana",
		[]string{"t-1", "t-2"}, nil, tracker.PersonAuthority{Lead: true}); err != nil {
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
		[]string{"t-2"}, nil, tracker.PersonAuthority{}); err != nil {
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
	notices := noticesFor(t, r, "ana", 1)

	views := []string{"v-1", "v-2"}
	if _, err := r.writer.WritePins(t.Context(), "op-pins", "ana", tracker.PinGesture{
		Views:     tracker.SetChange[string]{Set: &views},
		Favorites: tracker.SetChange[tracker.Favorite]{Add: []tracker.Favorite{{Kind: "project", ID: "ENG"}}},
	}); err != nil {
		t.Fatalf("WritePins: %v", err)
	}
	r.drain()
	if _, err := r.writer.WritePriorities(t.Context(), "op-prio", "ana",
		[]string{"t-1"}, nil, tracker.PersonAuthority{}); err != nil {
		t.Fatalf("WritePriorities: %v", err)
	}
	r.drain()
	if _, err := r.writer.MarkInbox(t.Context(), "op-inbox", "ana",
		tracker.InboxGesture{Read: []string{notices[0].RecordID}}); err != nil {
		t.Fatalf("MarkInbox: %v", err)
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
	case len(got.Read) != 1:
		t.Fatalf("the read list is %v, want the one entry", got.Read)
	}
}

// ONE NOTICE MARKED READ IS ONE NOTICE MARKED READ.
//
// The verb this replaced took all three lists and the watermark and REPLACED
// each, so a screen that sent only the notice somebody pressed Done on erased
// every other mark they had made, every snooze, and the position they had read
// to — the inbox came back full of everything they had already dealt with.
func TestMarkingOneNoticeReadLeavesEveryOtherMarkAndTheWatermark(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	n := noticesFor(t, r, "ana", 4)
	later := wednesday.Add(24 * time.Hour)

	if _, err := r.writer.MarkInbox(t.Context(), "op-setup", "ana", tracker.InboxGesture{
		ReadThrough: positionOf(n[0]),
		Snooze:      []tracker.Snooze{{RecordID: n[2].RecordID, Until: later}},
		Read:        []string{n[3].RecordID},
	}); err != nil {
		t.Fatalf("set the inbox up: %v", err)
	}
	r.drain()
	before := r.person("ana")

	if _, err := r.writer.MarkInbox(t.Context(), "op-done", "ana",
		tracker.InboxGesture{Read: []string{n[1].RecordID}}); err != nil {
		t.Fatalf("mark one read: %v", err)
	}
	r.drain()

	after := r.person("ana")
	if after.SeenThrough != before.SeenThrough {
		t.Errorf("the watermark moved from %+v to %+v on a gesture that named "+
			"one notice", before.SeenThrough, after.SeenThrough)
	}
	read, snoozed := r.marks("ana", wednesday)
	for i, want := range []bool{true, true, false, true} {
		if read[n[i].RecordID] != want {
			t.Errorf("notice %d reads as read=%v, want %v", i, read[n[i].RecordID], want)
		}
	}
	if !snoozed[n[2].RecordID] {
		t.Error("the snooze made before was erased by marking another notice read")
	}
}

// TWO SCREENS MARKING TWO NOTICES AT ONCE BOTH LAND.
//
// The second gesture is decided against a record that does not yet hold the
// first: it is published while the first is still unapplied, so its first
// round reads the older record. The broker refuses that round, and the gesture
// is applied AGAIN to the record that won — which is only true because it is
// resolved inside the decide. A gesture resolved against a read taken before
// it publishes the stale lists on every round and the snooze is lost.
func TestAConcurrentSnoozeAndReadBothLand(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	n := noticesFor(t, r, "ana", 2)
	later := wednesday.Add(24 * time.Hour)

	if _, err := r.writer.MarkInbox(t.Context(), "op-snooze", "ana", tracker.InboxGesture{
		Snooze: []tracker.Snooze{{RecordID: n[0].RecordID, Until: later}},
	}); err != nil {
		t.Fatalf("snooze from one tab: %v", err)
	}
	// NOT DRAINED: the snooze is on the log and not in the rows, which is
	// the state a second tab's gesture meets.
	r.applyWhileWriting()
	if _, err := r.writer.MarkInbox(t.Context(), "op-read", "ana", tracker.InboxGesture{
		Read: []string{n[1].RecordID},
	}); err != nil {
		t.Fatalf("read from another tab: %v", err)
	}
	r.drain()

	read, snoozed := r.marks("ana", wednesday)
	if !snoozed[n[0].RecordID] {
		t.Error("the snooze from the first tab was lost to the second tab's read")
	}
	if !read[n[1].RecordID] {
		t.Error("the second tab's read did not land")
	}
}

// THE POSITION ONLY MOVES FORWARD, and forward is the generation first.
//
// "Mark all read" from a tab opened yesterday names an older position than the
// one a newer tab already read to; taken as given it would un-read everything
// in between. And a sequence from before a reanchor is below every sequence
// after it however large — which the stored row could not honour while it kept
// the bare sequence, so the generation is asserted after a real apply.
func TestReadThroughNeverMovesBackwards(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	n := noticesFor(t, r, "ana", 2)

	mark := func(op string, at *statelog.Position) {
		t.Helper()
		if _, err := r.writer.MarkInbox(t.Context(), op, "ana",
			tracker.InboxGesture{ReadThrough: at}); err != nil {
			t.Fatalf("read through %s: %v", at, err)
		}
		r.drain()
	}
	mark("op-newest", positionOf(n[1]))
	mark("op-older", positionOf(n[0]))
	if got := r.person("ana").SeenThrough; got.Seq != n[1].LogSeq {
		t.Fatalf("an older read-through moved the position back to %+v", got)
	}

	stream := tracker.Domain{}.Stream().Name
	mark("op-next-generation", &statelog.Position{Stream: stream, Generation: 1, Seq: 1})
	got := r.person("ana").SeenThrough
	if got.Generation != 1 || got.Seq != 1 {
		t.Fatalf("the position after a read-through into generation 1 is %+v — "+
			"a generation the row does not keep reads back as zero, and every "+
			"notice after the reanchor sits above it for ever", got)
	}
	mark("op-big-old-sequence", &statelog.Position{Stream: stream, Seq: n[1].LogSeq + 1_000_000})
	if got := r.person("ana").SeenThrough; got.Generation != 1 || got.Seq != 1 {
		t.Fatalf("a large sequence from generation 0 moved the position to %+v", got)
	}
}

// A GESTURE LONGER THAN A LIST CAN HOLD IS REFUSED inbox_full BEFORE ANYTHING
// IS READ — whatever the record holds it could not land — and the refusal
// names the gesture that fits.
func TestAGestureLongerThanAListIsRefusedInboxFull(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	ids := make([]string, 0, tracker.MaxInboxEntries+1)
	for i := range tracker.MaxInboxEntries + 1 {
		ids = append(ids, "r-"+itoa(i))
	}
	_, err := r.writer.MarkInbox(t.Context(), "op-full", "ana",
		tracker.InboxGesture{Read: ids})
	if !errors.Is(err, tracker.ErrInboxFull) {
		t.Fatalf("a gesture naming %d notices answered %v, want ErrInboxFull",
			len(ids), err)
	}
	if !strings.Contains(err.Error(), "read_through") {
		t.Fatalf("the refusal does not name the gesture that fits: %v", err)
	}
}

// allNoticesFor routes n changes at somebody, as [noticesFor] does, and pages
// the whole inbox back OLDEST FIRST — past the one page noticesFor reads.
func allNoticesFor(t *testing.T, r *roundTrip, handle string, n int) []tracker.InboxNotice {
	t.Helper()
	bob := r.writer.As("bob", tracker.AuthorHuman, tracker.Provenance{})
	for i := range n {
		routeAs(t, r, bob, "t-"+handle+"-"+itoa(i), "ENG-"+itoa(i+1), handle)
	}
	var all []tracker.InboxNotice
	for cursor := ""; ; {
		page := r.inbox(tracker.InboxQuery{Who: tracker.PartyOf(handle), Cursor: cursor})
		all = append(all, page.Notices...)
		if cursor = page.NextCursor; cursor == "" {
			break
		}
	}
	if len(all) != n {
		t.Fatalf("%s's inbox pages back %d notices, want %d", handle, len(all), n)
	}
	slices.Reverse(all)
	return all
}

// A LIST ALREADY AT ITS CAP REFUSES ONE MORE inbox_full, AGAINST THE RECORD
// THE DECIDE READS — never trimmed to make room, and never accepted past it.
//
// Every gesture here is small; what fills the list is the STORED record, so
// only the check the decide makes against the list the gesture would leave
// can see it. A refusal must leave the record exactly as it was.
func TestAListAtItsCeilingRefusesOneMoreInboxFull(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	// ONE NOTICE BELOW, which the position reads through, and a list's worth
	// plus one above it — so every read mark is an exception the list has
	// to hold rather than one the position already answers.
	n := allNoticesFor(t, r, "ana", tracker.MaxInboxEntries+2)
	mark := func(op string, g tracker.InboxGesture) error {
		t.Helper()
		_, err := r.writer.MarkInbox(t.Context(), op, "ana", g)
		r.drain()
		return err
	}
	if err := mark("op-through", tracker.InboxGesture{ReadThrough: positionOf(n[0])}); err != nil {
		t.Fatalf("read through the first notice: %v", err)
	}
	above := n[1:]
	ids := func(from, to int) []string {
		out := make([]string, 0, to-from)
		for _, notice := range above[from:to] {
			out = append(out, notice.RecordID)
		}
		return out
	}
	const batch = 64
	for from := 0; from < tracker.MaxInboxEntries; from += batch {
		if err := mark("op-read-"+itoa(from), tracker.InboxGesture{
			Read: ids(from, from+batch)}); err != nil {
			t.Fatalf("marking notices %d..%d read: %v", from, from+batch, err)
		}
	}
	full := r.person("ana")
	if len(full.Read) != tracker.MaxInboxEntries {
		t.Fatalf("the read list holds %d entries after marking %d, want it at "+
			"its ceiling", len(full.Read), tracker.MaxInboxEntries)
	}

	last := above[tracker.MaxInboxEntries].RecordID
	err := mark("op-read-one-more", tracker.InboxGesture{Read: []string{last}})
	if !errors.Is(err, tracker.ErrInboxFull) {
		t.Fatalf("one more read mark on a full list answered %v, want ErrInboxFull", err)
	}
	if !strings.Contains(err.Error(), "read_through") || !strings.Contains(err.Error(), "read list") {
		t.Fatalf("the refusal does not name the list and the gesture that fits: %v", err)
	}
	if got := r.person("ana"); got.Version != full.Version || len(got.Read) != len(full.Read) {
		t.Fatalf("a refused mark moved the record from version %d with %d read "+
			"to version %d with %d", full.Version, len(full.Read), got.Version, len(got.Read))
	}

	// THE SNOOZED LIST, which the position never prunes: the same ceiling
	// against the stored record.
	until := wednesday.Add(24 * time.Hour)
	for from := 0; from < tracker.MaxInboxEntries; from += batch {
		var snoozes []tracker.Snooze
		for _, id := range ids(from, from+batch) {
			snoozes = append(snoozes, tracker.Snooze{RecordID: id, Until: until})
		}
		if err := mark("op-snooze-"+itoa(from), tracker.InboxGesture{Snooze: snoozes}); err != nil {
			t.Fatalf("snoozing notices %d..%d: %v", from, from+batch, err)
		}
	}
	full = r.person("ana")
	if len(full.Snoozed) != tracker.MaxInboxEntries {
		t.Fatalf("the snoozed list holds %d entries, want it at its ceiling",
			len(full.Snoozed))
	}
	err = mark("op-snooze-one-more", tracker.InboxGesture{
		Snooze: []tracker.Snooze{{RecordID: last, Until: until}}})
	if !errors.Is(err, tracker.ErrInboxFull) || !strings.Contains(err.Error(), "snoozed list") {
		t.Fatalf("one more snooze on a full list answered %v, want ErrInboxFull "+
			"naming the snoozed list", err)
	}
	if got := r.person("ana"); got.Version != full.Version || len(got.Snoozed) != len(full.Snoozed) {
		t.Fatalf("a refused snooze moved the record from version %d to %d",
			full.Version, got.Version)
	}
}

// A PERSON ROW AN OLDER APPLIER WROTE IS RE-DERIVED, PACKED, from the rows the
// node already holds.
//
// The older applier stored `seen_through` as the bare sequence and every inbox
// entry at whatever position its caller sent. Left as they were, a position
// past a reanchor reads as generation zero — every notice in the live
// generation unread — and a read mark's stale position is pruned on the next
// write although its notice is still above the position, so it comes back
// unread. The re-derivation must restore both to what the current applier
// writes, and leave a row it already wrote untouched.
func TestAnOlderAppliersPersonRowIsRederivedPacked(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	n := noticesFor(t, r, "ana", 3)
	if _, err := r.writer.MarkInbox(t.Context(), "op-through", "ana",
		tracker.InboxGesture{ReadThrough: positionOf(n[0]), Read: []string{n[2].RecordID}}); err != nil {
		t.Fatalf("ana's marks: %v", err)
	}
	cy := r.writer.As("cy", tracker.AuthorHuman, tracker.Provenance{})
	stream := tracker.Domain{}.Stream().Name
	if _, err := cy.MarkInbox(t.Context(), "op-cy", "cy", tracker.InboxGesture{
		ReadThrough: &statelog.Position{Stream: stream, Generation: 1, Seq: 7}}); err != nil {
		t.Fatalf("cy's read-through into generation 1: %v", err)
	}
	r.drain()
	wantAna, wantCy := r.person("ana"), r.person("cy")
	if len(wantAna.Read) != 1 || wantAna.Read[0].Position == 0 {
		t.Fatalf("ana's read list is %+v, want the one mark at its position", wantAna.Read)
	}

	rederive := func() int {
		t.Helper()
		var rows int
		if err := r.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
			var err error
			rows, err = r.applier.Rederive(t.Context(), tx, statelog.ApplyOptions{})
			return err
		}); err != nil {
			t.Fatalf("re-derive: %v", err)
		}
		return rows
	}
	if rows := rederive(); rows != 0 {
		t.Fatalf("re-deriving rows this applier wrote rewrote %d of them", rows)
	}

	// THE PREDECESSOR'S ROWS: the bare sequence, and a caller's zero.
	if err := r.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(t.Context(),
			`UPDATE tracker_persons SET seen_through = 7 WHERE handle = 'cy'`); err != nil {
			return err
		}
		_, err := tx.ExecContext(t.Context(), `UPDATE tracker_persons SET read_json = ?
			WHERE handle = 'ana'`, `[{"record_id":"`+n[2].RecordID+`","position":0}]`)
		return err
	}); err != nil {
		t.Fatalf("plant the older applier's rows: %v", err)
	}
	if got := r.person("cy").SeenThrough; got.Generation != 0 {
		t.Fatalf("the planted row already reads generation %d", got.Generation)
	}
	if rows := rederive(); rows != 2 {
		t.Fatalf("the re-derivation wrote %d rows, want the two it had to", rows)
	}
	if got := r.person("cy").SeenThrough; got != wantCy.SeenThrough {
		t.Fatalf("cy's position re-derives to %+v, want %+v — a node upgrading "+
			"onto the older row would read every notice after the reanchor as "+
			"unread", got, wantCy.SeenThrough)
	}
	if got := r.person("ana").Read; !slices.Equal(got, wantAna.Read) {
		t.Fatalf("ana's read list re-derives to %+v, want %+v", got, wantAna.Read)
	}
	if tracker.DerivationVersion < 2 {
		t.Error("the person positions are re-derived under a rule set that " +
			"does not say so — a node already at the old version would never run it")
	}
}

// A STAR FROM ONE TAB DOES NOT DROP A PIN FROM ANOTHER.
//
// Both lists used to be replaced whole, so the call that only meant to star a
// project cleared every view it did not restate.
func TestAStarFromOneTabDoesNotDropAPinFromAnother(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for op, gesture := range map[string]tracker.PinGesture{
		"op-pin": {Views: tracker.SetChange[string]{Add: []string{"v-1"}}},
		"op-star": {Favorites: tracker.SetChange[tracker.Favorite]{
			Add: []tracker.Favorite{{Kind: "project", ID: "ENG"}},
		}},
		"op-pin-2": {Views: tracker.SetChange[string]{Add: []string{"v-2"}}},
	} {
		if _, err := r.writer.WritePins(t.Context(), op, "ana", gesture); err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		r.drain()
	}
	got := r.person("ana")
	if len(got.PinnedViews) != 2 || len(got.Favorites) != 1 {
		t.Fatalf("after two pins and a star the record holds views %v and "+
			"favourites %v", got.PinnedViews, got.Favorites)
	}

	// AND AN UNPIN TAKES ONE, leaving the rest.
	if _, err := r.writer.WritePins(t.Context(), "op-unpin", "ana", tracker.PinGesture{
		Views: tracker.SetChange[string]{Remove: []string{"v-1"}},
	}); err != nil {
		t.Fatalf("unpin: %v", err)
	}
	r.drain()
	if got := r.person("ana"); !slices.Equal(got.PinnedViews, []string{"v-2"}) ||
		len(got.Favorites) != 1 {
		t.Fatalf("after unpinning v-1 the record holds %v and %v",
			got.PinnedViews, got.Favorites)
	}

	// A WHOLE SET AND A CHANGE TO IT AT ONCE is refused: whichever won
	// would discard the other.
	set := []string{"v-3"}
	if _, err := r.writer.WritePins(t.Context(), "op-both", "ana", tracker.PinGesture{
		Views: tracker.SetChange[string]{Set: &set, Add: []string{"v-4"}},
	}); !errors.Is(err, tracker.ErrInvalid) {
		t.Fatalf("a set and a delta together answered %v", err)
	}
}

// A REORDER FROM A STALE SCREEN IS REFUSED, not applied over the newer list.
func TestAPriorityReorderFromAStaleScreenIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.WritePriorities(t.Context(), "op-first", "ana",
		[]string{"t-1", "t-2"}, nil, tracker.PersonAuthority{}); err != nil {
		t.Fatalf("the first list: %v", err)
	}
	r.drain()
	read := r.person("ana").Version

	if _, err := r.writer.WritePriorities(t.Context(), "op-reorder", "ana",
		[]string{"t-2", "t-1"}, &read, tracker.PersonAuthority{}); err != nil {
		t.Fatalf("a reorder against the version just read: %v", err)
	}
	r.drain()

	_, err := r.writer.WritePriorities(t.Context(), "op-stale", "ana",
		[]string{"t-1"}, &read, tracker.PersonAuthority{})
	if !errors.Is(err, tracker.ErrStaleVersion) {
		t.Fatalf("a reorder made against version %d answered %v, want "+
			"ErrStaleVersion", read, err)
	}
	r.drain()
	if got := r.person("ana").Priorities; !slices.Equal(got, []string{"t-2", "t-1"}) {
		t.Fatalf("the stale reorder replaced the list: %v", got)
	}
}

// AN UNREAD MARK BRINGS BACK A NOTICE THE POSITION COVERS.
//
// After "mark all read", every notice sits at or below the position, so the
// position alone answers read for all of them — and a reader that consulted
// only it and the read list made "mark unread" a mark nothing could see. And
// moving the position forward clears the mark again, because "I have read
// everything up to here" is the gesture that says so.
func TestAnUnreadMarkBringsBackANoticeThePositionCovers(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	n := noticesFor(t, r, "ana", 2)

	if _, err := r.writer.MarkInbox(t.Context(), "op-all", "ana",
		tracker.InboxGesture{ReadThrough: positionOf(n[1])}); err != nil {
		t.Fatalf("mark all read: %v", err)
	}
	r.drain()
	if _, err := r.writer.MarkInbox(t.Context(), "op-unread", "ana",
		tracker.InboxGesture{Unread: []string{n[0].RecordID}}); err != nil {
		t.Fatalf("mark one unread: %v", err)
	}
	r.drain()
	if read, _ := r.marks("ana", wednesday); read[n[0].RecordID] || !read[n[1].RecordID] {
		t.Fatalf("after marking the older notice unread the inbox reads %v", read)
	}

	// Moving the position to the same place is not moving it: nothing to
	// publish, and the mark stands.
	if _, err := r.writer.MarkInbox(t.Context(), "op-all-again", "ana",
		tracker.InboxGesture{ReadThrough: positionOf(n[1])}); err != nil {
		t.Fatalf("mark all read again: %v", err)
	}
	r.drain()
	if read, _ := r.marks("ana", wednesday); read[n[0].RecordID] {
		t.Fatal("a read-through that did not move the position cleared an unread mark")
	}

	routeAs(t, r, r.writer.As("bob", tracker.AuthorHuman, tracker.Provenance{}),
		"t-ana-next", "ENG-9", "ana")
	newest := r.inbox(tracker.InboxQuery{Who: tracker.PartyOf("ana")}).Notices[0]
	if _, err := r.writer.MarkInbox(t.Context(), "op-all-3", "ana",
		tracker.InboxGesture{ReadThrough: positionOf(newest)}); err != nil {
		t.Fatalf("mark all read past it: %v", err)
	}
	r.drain()
	if read, _ := r.marks("ana", wednesday); !read[n[0].RecordID] {
		t.Fatal("reading past an unread mark left it unread")
	}
}

// A MARK NAMES A RECORD THE COMPANY HOLDS, and its position is the record's own.
//
// The caller used to supply the position beside the id, and a mark sent with
// none — which is what a copied call did — was pruned on the write that made
// it. The position is now read from the history row inside the decide, so an
// id that names nothing is refused rather than stored.
func TestAMarkNamingNoRecordIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	_, err := r.writer.MarkInbox(t.Context(), "op-nothing", "ana",
		tracker.InboxGesture{Read: []string{"no-such-record"}})
	if !errors.Is(err, tracker.ErrInvalid) || !strings.Contains(err.Error(), "no-such-record") {
		t.Fatalf("a mark naming no record answered %v", err)
	}

	n := noticesFor(t, r, "ana", 1)
	// ONE ID IN TWO LISTS is a contradiction nothing could order.
	if _, err := r.writer.MarkInbox(t.Context(), "op-both", "ana", tracker.InboxGesture{
		Read: []string{n[0].RecordID}, Unread: []string{n[0].RecordID},
	}); !errors.Is(err, tracker.ErrInvalid) {
		t.Fatalf("one notice both read and unread answered %v", err)
	}
	// AND A READ-THROUGH ON ANOTHER STREAM is refused: an inbox is read
	// through the stream its notices are on.
	if _, err := r.writer.MarkInbox(t.Context(), "op-dead", "ana", tracker.InboxGesture{
		ReadThrough: &statelog.Position{Stream: "CREWLET_SOMETHING_ELSE", Seq: 4},
	}); !errors.Is(err, tracker.ErrInvalid) {
		t.Fatalf("a read-through on another stream answered %v", err)
	}
	// AND A GESTURE THAT SAYS NOTHING is refused rather than published.
	if _, err := r.writer.MarkInbox(t.Context(), "op-empty", "ana",
		tracker.InboxGesture{}); !errors.Is(err, tracker.ErrInvalid) {
		t.Fatalf("an empty gesture answered %v", err)
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
	n := noticesFor(t, r, "ana", 1)

	future := wednesday.Add(time.Hour)
	if _, err := r.writer.MarkInbox(t.Context(), "op-inbox", "ana", tracker.InboxGesture{
		Snooze: []tracker.Snooze{{RecordID: n[0].RecordID, Until: future}},
	}); err != nil {
		t.Fatalf("MarkInbox: %v", err)
	}
	r.drain()

	// THE READ'S OWN CLOCK DECIDES, so the same rows answer differently as
	// the hour passes — which is the whole point of a snooze.
	state, err := r.reader.Person(t.Context(), tracker.PersonQuery{
		Who: tracker.PartyOf("ana"), Level: statelog.ReadStale,
	}, wednesday.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("Person: %v", err)
	}
	if len(state.Due) != 1 || len(state.Snoozed) != 0 {
		t.Fatalf("snoozed=%v due=%v, want the entry reported due",
			state.Snoozed, state.Due)
	}
	// AND IT IS STILL IN THE SNOOZED LIST ON THE ROW, because nothing
	// promoted it: the person's next inbox write is what drops it.
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
		Who: tracker.PartyOf("ana"),
	}, wednesday); err == nil {
		t.Fatal("a person read with no read level was answered")
	}
}

// THE CAPS ARE REFUSED NAMING THE FIELD, never cut — and the pin caps are held
// against the set a gesture would LEAVE, since a small delta can still be the
// one past the cap.
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
	full := many(tracker.MaxPinnedViews)
	if _, err := r.writer.WritePins(t.Context(), "op-pins", "ana", tracker.PinGesture{
		Views: tracker.SetChange[string]{Set: &full},
	}); err != nil {
		t.Fatalf("a full strip was refused: %v", err)
	}
	r.drain()
	_, err := r.writer.WritePins(t.Context(), "op-one-more", "ana", tracker.PinGesture{
		Views: tracker.SetChange[string]{Add: []string{"one-more"}},
	})
	if err == nil || !strings.Contains(err.Error(), "pin") {
		t.Fatalf("a pin past the cap was accepted: %v", err)
	}
	_, err = r.writer.WritePriorities(t.Context(), "op-prio", "ana",
		many(tracker.MaxPriorities+1), nil, tracker.PersonAuthority{})
	if err == nil || !strings.Contains(err.Error(), "priority list") {
		t.Fatalf("an unbounded priority list was accepted: %v", err)
	}

	n := noticesFor(t, r, "ana", 1)
	// A SNOOZE PAST THE HORIZON is a delete that does not say so.
	_, err = r.writer.MarkInbox(t.Context(), "op-far", "ana", tracker.InboxGesture{
		Snooze: []tracker.Snooze{{RecordID: n[0].RecordID,
			Until: wednesday.Add(tracker.MaxSnoozeAhead + 24*time.Hour)}},
	})
	if err == nil || !strings.Contains(err.Error(), "delete that does not say so") {
		t.Fatalf("a snooze past the horizon was accepted: %v", err)
	}
	// AND ONE ALREADY DUE is not a snooze at all.
	_, err = r.writer.MarkInbox(t.Context(), "op-past", "ana", tracker.InboxGesture{
		Snooze: []tracker.Snooze{{RecordID: n[0].RecordID, Until: wednesday.Add(-time.Hour)}},
	})
	if !errors.Is(err, tracker.ErrInvalid) {
		t.Fatalf("a snooze into the past answered %v", err)
	}
	// AND ONE INSIDE IT LANDS, or the guard would refuse every snooze.
	if _, err := r.writer.MarkInbox(t.Context(), "op-near", "ana", tracker.InboxGesture{
		Snooze: []tracker.Snooze{{RecordID: n[0].RecordID, Until: wednesday.Add(24 * time.Hour)}},
	}); err != nil {
		t.Fatalf("an ordinary snooze was refused: %v", err)
	}

	// AND A STAR THAT POINTS AT NOTHING is refused too.
	_, err = r.writer.WritePins(t.Context(), "op-fav", "ana", tracker.PinGesture{
		Favorites: tracker.SetChange[tracker.Favorite]{Add: []tracker.Favorite{{Kind: "project"}}},
	})
	if err == nil || !strings.Contains(err.Error(), "points at nothing") {
		t.Fatalf("a favourite with no id was accepted: %v", err)
	}
}

// A ROW WRITTEN BEFORE THE GENERATION WAS STORED READS AS GENERATION ZERO,
// which is exactly what it was: the packed form of a zero generation is the
// bare sequence, so nothing already on disk changes meaning.
func TestABareSequenceRowReadsAsGenerationZero(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	stream := tracker.Domain{}.Stream().Name
	if err := r.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO tracker_persons
			(handle, seen_through, seen_through_stream, version)
			VALUES ('old', 42, ?, 1)`, stream)
		return err
	}); err != nil {
		t.Fatalf("plant a row: %v", err)
	}
	if got := r.person("old").SeenThrough; got.Generation != 0 || got.Seq != 42 {
		t.Fatalf("a bare-sequence row reads back as %+v", got)
	}
}
