package tracker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A person's own state, and the three different authorities over it.
//
// One object, one subject, and THREE rules about who may write what — which is
// why this is three verbs rather than one. A single "write the person" verb
// would have to take the whole document and then decide, field by field,
// which parts of it the caller was allowed to have sent; the parts it refused
// would be silently dropped or the whole write refused for a field the caller
// did not mean to change. Three verbs each carry only what their authority
// covers, so there is nothing to drop.
//
//	INBOX     read, unread, snoozed, the seen-through position and the
//	          primary reasons. Written ONLY on behalf of the person whose
//	          inbox it is. Somebody else marking your work read is the one
//	          thing an inbox must never allow, because the item is then gone
//	          from the only place you would have looked for it.
//
//	PINS      pinned views and favourites. The same rule, and for the
//	          plainer reason: a pin is a person's own arrangement, and one
//	          somebody else can set is one that moves under them.
//
//	PRIORITY  the ordered list of what to do next. Your own, OR a LEAD's for
//	          somebody in their line — which is what a lead is for — and
//	          that write is the one that produces a "prioritised"
//	          notification, because being told what to do next by somebody
//	          else is news and re-ordering your own list is not.
//
// # WHOSE record it is, which is not who wrote it
//
// All three authorities are about a PERSON, and a person is not the same thing
// as the credential in their hand: a write made through somebody's own API
// token is attributed to the TOKEN with author kind `operator`, deliberately,
// because a tracker whose author field is chosen by the writer is not an audit
// trail (see internal/api/operator). So the subject these verbs key on is
// [Writer.Record] — the seat the credential is bound to — while the author on
// every record they publish stays [Writer.Actor]. Keyed on the actor instead,
// a founder marking their own inbox through their own assistant wrote a second
// person record named after their credential.
//
// # Why every verb reads the stored row first
//
// The three write disjoint parts of one document, so each has to carry the
// other two through unchanged. Taking them from the caller would let an inbox
// write clear somebody's pins by omission — the ordinary shape of a lost
// update, and the one a whole-document object invites.
//
// # And why two of them take a GESTURE rather than the part they write
//
// The same lost update happens INSIDE a part when the caller sends the whole
// of it. The inbox and the pins used to be written by replacing their lists
// with the caller's, so one "mark this read" from a tab opened an hour ago
// erased every mark and snooze made since, and the watermark with them. What
// a person says about their own inbox or strip is one move — this notice is
// done, put that one off, star this — so [Writer.MarkInbox] and
// [Writer.WritePins] take the move and resolve it against the record the
// DECIDE reads, which is the snapshot the broker arbitrates: a round that
// loses the race re-applies the move to the record that won, and two screens
// marking two notices both land. The priority list is the exception, because
// an order is a statement about every entry at once — it stays whole, and
// `ifMatch` refuses one made from a stale read instead.

// PersonAuthority is what a caller may write on somebody else's record.
//
// A VALUE RATHER THAN A BOOL on the writer, because "may I set this person's
// priorities" is a question about a PAIR — who is asking and about whom — and
// the answer for one pair says nothing about another.
type PersonAuthority struct {
	// Lead reports whether the actor leads the handle being written.
	// Resolved by the caller from the org chart, because this package has
	// no chart.
	Lead bool

	// Person reports an actor acting as a HUMAN or through a person's own
	// credential, rather than as a seat.
	//
	// # Why this is a second authority rather than a special case of Lead
	//
	// Because it is a different question, and the chart cannot answer it.
	// An operator's actor is an API TOKEN's name — it is not a handle in
	// the chart, so no ancestor walk can ever match it, and `Lead` is
	// false for every operator by construction.
	//
	// Without it the ONE shipped surface for this verb could not use it.
	// `set_priorities` is registered on the operator MCP alone; an
	// operator could therefore never satisfy `own || Lead`, so a founder
	// re-ordering an agent's queue through the only tool that exists got
	// "<token> is not <handle> and does not lead them" every time — while
	// omitting the handle silently wrote a PERSON RECORD FOR THE TOKEN.
	//
	// The rule this restores is the design's own: a person's priorities
	// are written by the owner, by lead-or-above, by a human, or by an
	// operator. A SEAT is the one party that may not, which is the whole
	// point — an agent re-ordering a colleague's list is a hand-off in
	// disguise, bypassing the guarded take and the reassignment budget.
	Person bool
}

// MaxSnoozeAhead bounds how far into the future an item may be snoozed.
//
// A YEAR. A snooze is "not now", and one past the horizon a person plans over
// is a delete that does not say so — the item leaves the inbox and the person
// who snoozed it will not be at the company when it returns.
const MaxSnoozeAhead = 365 * 24 * time.Hour

// ErrInboxFull reports an inbox gesture that would leave one of a person's
// lists past [MaxInboxEntries].
//
// ITS OWN SENTINEL, because the remedy is a different gesture rather than a
// smaller one: the lists hold only what the seen-through position does not
// already answer, so a full one is a person marking notices one at a time
// that `read_through` would mark in one move — and a caller told "invalid"
// would go looking for what is wrong with the notice.
var ErrInboxFull = errors.New("tracker: this inbox list is at its ceiling")

// InboxGesture is one move a person makes on their own inbox.
//
// # A GESTURE, never the lists
//
// The inbox used to be written by handing over all three lists and the
// watermark, each REPLACING what was stored — so a screen marking one notice
// read had to send everything it had last read, and anything it had not read
// was cleared: one `read` from a tab opened before a snooze erased the
// snooze, and a caller that sent only the notice it meant wiped every other
// mark and the position with it. What a person can say about their inbox is
// "this one is done", "put this one off", "I have read everything up to
// here" — so that is what the verb takes, and [Writer.MarkInbox] resolves it
// against the record the DECIDE reads, which is the one snapshot the broker
// arbitrates. Two screens marking two notices at once both land.
//
// A record id appears in at most ONE of the four lists: a notice both read
// and unread in one gesture is a contradiction nothing could order.
type InboxGesture struct {
	// Read marks notices read — the "Done" of an inbox. It also lifts a
	// snooze on the notice, because a notice somebody has dealt with is
	// not one to bring back.
	Read []string

	// Unread marks notices unread again, including one at or below the
	// seen-through position: that is the only case the mark changes
	// anything, since everything above the position is unread already.
	Unread []string

	// Snooze puts notices off until an instant, at most [MaxSnoozeAhead]
	// away. A notice already snoozed moves to the new instant.
	Snooze []Snooze

	// Unsnooze brings notices back now.
	Unsnooze []string

	// ReadThrough moves the seen-through position FORWARD: everything at
	// or below it reads as read. ADVANCE ONLY, and generation-aware — a
	// position at or behind the stored one changes nothing, so a stale
	// tab's "mark all read" cannot un-read what a newer one read. Nil
	// leaves the position alone.
	ReadThrough *statelog.Position

	// PrimaryReasons declares which wake reasons are the person's to act
	// on. Nil leaves the declaration alone; an EMPTY list takes the
	// shipped default back, see [DefaultPrimaryReasons].
	PrimaryReasons *[]Reason
}

// Snooze is one notice put off until an instant.
type Snooze struct {
	RecordID string
	Until    time.Time
}

// Empty reports a gesture that would move nothing.
func (g InboxGesture) Empty() bool {
	return len(g.Read) == 0 && len(g.Unread) == 0 && len(g.Snooze) == 0 &&
		len(g.Unsnooze) == 0 && g.ReadThrough == nil && g.PrimaryReasons == nil
}

// MarkInbox applies one inbox gesture to a person's own record.
//
// ONLY ON BEHALF OF THAT PERSON — see the file head — so the handle must be
// the one this writer IS, and the refusal says so rather than silently writing
// nothing. Which handle that is comes from [Writer.Record]: a person acting
// through their own credential is the seat it is bound to, not the token.
//
// RESOLVED INSIDE THE DECIDE: every notice named is looked up there for the
// position it sits at, and the gesture is applied to the lists that snapshot
// holds, so a round the broker refuses re-applies it to the record that won.
// A gesture that would leave a list past [MaxInboxEntries] is refused
// [ErrInboxFull] rather than trimmed, and one that changes nothing publishes
// nothing.
func (w *Writer) MarkInbox(ctx context.Context, opID, handle string,
	gesture InboxGesture) (WriteResult, error) {

	if err := w.ownRecord(handle, "inbox"); err != nil {
		return WriteResult{}, err
	}
	if err := checkInboxGesture(gesture, w.Now()); err != nil {
		return WriteResult{}, err
	}
	return w.writePerson(ctx, opID, handle, func(tx *sql.Tx, post *Person,
		at time.Time) error {

		positions, err := inboxPositions(ctx, tx, gesture)
		if err != nil {
			return err
		}
		return applyInboxGesture(post, gesture, positions, at)
	})
}

// SetChange is a gesture over one of a person's own sets.
//
// TWO SPELLINGS AND NEVER BOTH, for the reason [RelationIntent] gives: Set
// states the whole collection, Add and Remove a delta against whatever the
// decide reads. The delta is what a screen sends — "star this", "unpin that"
// — because a whole list it formed from an earlier read drops whatever
// another tab added in between. Set is kept for the caller that genuinely
// means the whole arrangement, such as a drag that reorders the strip.
type SetChange[T comparable] struct {
	// Set replaces the collection. Non-nil and empty clears it.
	Set *[]T

	// Add appends what is not already there, in order; Remove drops what
	// is. Remove runs first, so moving an entry to the end is one gesture.
	Add    []T
	Remove []T
}

// Empty reports a change that states nothing.
func (c SetChange[T]) Empty() bool {
	return c.Set == nil && len(c.Add) == 0 && len(c.Remove) == 0
}

// apply is the change over the current collection, as a fresh slice.
func (c SetChange[T]) apply(current []T) []T {
	if c.Set != nil {
		return nilIfEmpty(dedupe(*c.Set))
	}
	out := make([]T, 0, len(current)+len(c.Add))
	for _, v := range current {
		if !slices.Contains(c.Remove, v) {
			out = append(out, v)
		}
	}
	for _, v := range c.Add {
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return nilIfEmpty(out)
}

// mapped is the change with every value it names passed through f.
func (c SetChange[T]) mapped(f func(T) T) SetChange[T] {
	each := func(in []T) []T {
		if in == nil {
			return nil
		}
		out := make([]T, len(in))
		for i, v := range in {
			out[i] = f(v)
		}
		return out
	}
	out := SetChange[T]{Add: each(c.Add), Remove: each(c.Remove)}
	if c.Set != nil {
		set := each(*c.Set)
		if set == nil {
			set = []T{}
		}
		out.Set = &set
	}
	return out
}

// both reports a change that states a whole set and a delta at once.
func (c SetChange[T]) both() bool {
	return c.Set != nil && (len(c.Add) != 0 || len(c.Remove) != 0)
}

// values is every value the change names, for the per-value checks.
func (c SetChange[T]) values() []T {
	var out []T
	if c.Set != nil {
		out = append(out, *c.Set...)
	}
	return append(append(out, c.Add...), c.Remove...)
}

// PinGesture is one move on a person's pinned views and starred things.
type PinGesture struct {
	Views     SetChange[string]
	Favorites SetChange[Favorite]
}

// WritePins applies one pin gesture to a person's own record.
//
// ONLY ON BEHALF OF THAT PERSON: a pin somebody else can set is one that moves
// under them.
//
// # Resolved inside the decide, and capped on the RESULT
//
// Both lists used to be replaced whole, so starring a project from one tab
// dropped a view pinned from another since that tab last read — and the call
// that only meant to star something cleared every pin it did not restate.
// The gesture is applied to what the decide reads, and the caps are held
// against the set it would LEAVE, because that is the set a strip renders: a
// delta that is small on its own can still be the thirty-third pin.
func (w *Writer) WritePins(ctx context.Context, opID, handle string,
	gesture PinGesture) (WriteResult, error) {

	if err := w.ownRecord(handle, "pins"); err != nil {
		return WriteResult{}, err
	}
	gesture.Views = gesture.Views.mapped(strings.TrimSpace)
	switch {
	case gesture.Views.Empty() && gesture.Favorites.Empty():
		return WriteResult{}, invalid("tracker: this pin gesture for %s names "+
			"nothing to pin, unpin, star or unstar", handle)
	case gesture.Views.both(), gesture.Favorites.both():
		return WriteResult{}, invalid("tracker: this pin gesture for %s states "+
			"a whole set and a change to it at once — a caller states one or "+
			"the other, because whichever won would discard the other", handle)
	}
	for _, view := range gesture.Views.values() {
		if strings.TrimSpace(view) == "" {
			return WriteResult{}, invalid("tracker: a pinned view of %s names "+
				"no view, and a pin with no view points at nothing", handle)
		}
	}
	for _, favorite := range gesture.Favorites.values() {
		if favorite.Kind == "" || favorite.ID == "" {
			return WriteResult{}, invalid("tracker: a favourite of %s names "+
				"kind %q and id %q, and a star with neither points at nothing",
				handle, favorite.Kind, favorite.ID)
		}
	}
	return w.writePerson(ctx, opID, handle, func(_ *sql.Tx, post *Person,
		_ time.Time) error {

		views := gesture.Views.apply(post.PinnedViews)
		favorites := gesture.Favorites.apply(post.Favorites)
		switch {
		case len(views) > MaxPinnedViews:
			return invalid("tracker: %s would pin %d views and the maximum "+
				"is %d — a strip where everything is first has no first; "+
				"unpin one first", handle, len(views), MaxPinnedViews)
		case len(favorites) > MaxFavorites:
			return invalid("tracker: %s would star %d things and the maximum "+
				"is %d; unstar one first", handle, len(favorites), MaxFavorites)
		}
		post.PinnedViews, post.Favorites = views, favorites
		return nil
	})
}

// WritePriorities replaces a person's ordered list of what to do next.
//
// YOUR OWN, OR A LEAD'S FOR SOMEBODY IN THEIR LINE — the one authority here
// that reaches across people, because telling somebody what to do next is what
// a lead is for. That write is the one that produces a notification: being
// told what to do next by somebody else is news, and re-ordering your own list
// is not.
//
// # Why it is a wake AND a stamp, when it used to be only a stamp
//
// This verb used to argue that a notification "would not do it", because every
// Notify the domain carried was task-shaped and one attached to a person record
// "renders no card and reaches nobody". That was an accurate description of the
// code and a wrong conclusion from it: the parser dropped every non-task
// subject outright (see [ObjectKind.Routable]) and the prompt had no frame but
// a task's, so the wake was unreachable — while recipients.go already routed
// `prioritised` off Snapshot.Person, the reason was already in the enum and
// already Primary, and the spec already called this write "the one that
// produces a `prioritised` Notify".
//
// The stamp alone is only half an answer, and which half it is matters: a
// stamp is seen by somebody who OPENS A SCREEN, and the recipients of this
// gesture are seats, which have no screen. A lead re-ordering an agent's queue
// and getting silence could not tell whether it was read.
//
// The caller resolves the lead relation, because this package has no org
// chart; what it enforces is that a caller which did NOT resolve one may only
// write its own.
//
// # The whole list, conditioned on the one it replaces
//
// An order is the one part of a person's record a gesture cannot express as a
// delta: moving one entry up is a statement about every entry around it. So it
// stays a whole list, and IFMATCH is what keeps that from being last-write-wins:
// a reorder made from a screen that read version N is refused
// [ErrStaleVersion] once the record has moved past N, and the caller re-reads
// rather than restoring the order somebody else just replaced. Nil writes
// unconditionally, which is an assistant's "put these three first"; zero is a
// condition like any other — "nobody has written this person yet".
func (w *Writer) WritePriorities(ctx context.Context, opID, handle string,
	priorities []string, ifMatch *uint64,
	authority PersonAuthority) (WriteResult, error) {

	priorities = cleanHandles(priorities)
	// THE PERSON, NOT THE CREDENTIAL. A founder re-ordering their own
	// queue through their own assistant is writing their OWN list, so it
	// clears the stamp and wakes nobody — measured through the token it
	// read as somebody else setting their queue, and the founder was
	// notified that they had been given instructions by themselves.
	own := w.Record() == handle
	switch {
	case !own && !authority.Lead && !authority.Person:
		return WriteResult{}, fmt.Errorf("tracker: %s is a seat, is not %s and "+
			"does not lead them, so it cannot set what %s does next — an agent "+
			"re-ordering a colleague's list is a hand-off in disguise, and it "+
			"bypasses the guarded take and the reassignment budget. A lead, a "+
			"human or an operator may: %w",
			w.Actor, handle, handle, ErrForbidden)
	case len(priorities) > MaxPriorities:
		return WriteResult{}, invalid("tracker: %s's priority list carries "+
			"%d items and the maximum is %d — a list longer than that is not "+
			"an order, it is the backlog again", handle, len(priorities),
			MaxPriorities)
	}
	return w.writePersonNotifying(ctx, opID, handle, ChangePrioritised,
		func(tx *sql.Tx, post *Person, at time.Time) (*Notify, error) {
			// COMPARED INSIDE THE DECIDE, against the version this
			// snapshot read: a comparison made before it would pass a
			// record that moved between the two reads.
			if ifMatch != nil && *ifMatch != post.Version {
				return nil, fmt.Errorf("tracker: %s's priority list is at "+
					"version %d and this reorder was made against %d — read "+
					"it again and decide from what it says now: %w",
					handle, post.Version, *ifMatch, ErrStaleVersion)
			}
			post.Priorities = priorities
			// THE STAMP IS WHAT MAKES THIS AUTHORITY VISIBLE ON THE
			// SCREEN. A lead silently re-ordering somebody's queue is a
			// person who starts the day on work they did not choose and
			// cannot tell why — so the stamp sits on the thing they are
			// already looking at.
			//
			// AND THE PERSON'S OWN WRITE CLEARS IT, because taking your
			// queue back is the gesture that says you have seen it.
			post.PrioritiesSetBy, post.PrioritiesSetAt = "", time.Time{}
			if own {
				// YOUR OWN LIST WAKES NOBODY. Re-ordering your own
				// queue is not news to anybody, least of all to you,
				// and this is the one tracker write that vetoes its
				// own delivery per call.
				return nil, nil
			}
			post.PrioritiesSetBy, post.PrioritiesSetAt = w.Actor, at
			// AND THE WAKE, which is the other half of the same
			// authority: the stamp is seen by somebody who opens the
			// screen, and a SEAT has no screen. Being told what to do
			// next by somebody above you is an instruction, and the one
			// Addressed wake of the four — a seat takes it up or says
			// why it cannot.
			return w.prioritisedWake(ctx, tx, handle, priorities)
		})
}

// prioritisedWake is what a lead writing somebody's queue announces.
//
// IT NAMES THE TASK AT THE TOP, which is what makes this wake actionable
// rather than a notice that something moved: "your list changed" sends a seat
// to read the whole list and work out what is new, and the list does not
// record what was new. The first entry is the answer to "what do I do next",
// which is the only question the gesture was making.
//
// AN EMPTY LIST WAKES NOBODY. A lead clearing somebody's priorities is taking
// an instruction back rather than giving one, and there is no task to name.
func (w *Writer) prioritisedWake(ctx context.Context, tx *sql.Tx, handle string,
	priorities []string) (*Notify, error) {

	if len(priorities) == 0 {
		return nil, nil
	}
	top, held, err := readTask(ctx, tx, priorities[0])
	if err != nil {
		return nil, err
	}
	if !held {
		// A TASK THIS NODE HAS NOT APPLIED YET. The list is still
		// written — the priorities are the caller's to set and a wake is
		// not worth refusing a write for — but naming a task whose row
		// is not here would put an empty key and an empty title on the
		// card. Silence is the honest degradation.
		return nil, nil
	}
	return &Notify{
		Kind: ChangePrioritised,
		Snapshot: Snapshot{
			Person: handle,
			Task:   top.ID,
			Key:    top.Key,
			Title:  top.Title,
			// THE PROJECT AND STATUS TRAVEL because the card renders
			// them and the seat would otherwise open the task to learn
			// whether the thing it has just been told to do first is
			// already done.
			Project:       top.Project,
			Status:        top.Status,
			StatusGroup:   top.StatusGroup,
			Assignee:      top.Assignee,
			PrioritisedBy: w.Actor,
			Position:      1,
		},
		Excerpt: fmt.Sprintf("%s put %s at position 1 of your priorities",
			w.Actor, top.Key),
	}, nil
}

// ownRecord refuses a write on somebody else's half of a person record.
//
// AGAINST [Writer.Record] RATHER THAN THE ACTOR, so a person writing through
// their own credential passes: the record is keyed on who they ARE and the
// author still names the token they used. Measured the other way, a bound
// founder's assistant could only ever write the credential's own record and
// never the founder's.
//
// THE REFUSAL NAMES THE RECORD THIS WRITER MAY WRITE, not the actor, because
// those are now two different strings for exactly the caller this message is
// for — and a refusal naming a token would send a reader looking for a person
// by that name.
func (w *Writer) ownRecord(handle, what string) error {
	if w.Record() == handle {
		return nil
	}
	return fmt.Errorf("tracker: %s cannot write %s's %s — it is written on "+
		"behalf of the person whose it is, and somebody else's hand in it is "+
		"the one thing it must never allow: %w",
		w.Record(), handle, what, ErrForbidden)
}

// checkInboxGesture refuses a gesture no record could honour, before anything
// is read.
func checkInboxGesture(g InboxGesture, now time.Time) error {
	if g.Empty() {
		return invalid("tracker: this inbox gesture marks nothing — name " +
			"notices to read, unread, snooze or unsnooze, a position to read " +
			"through, or the primary reasons")
	}
	seen := map[string]string{}
	name := func(id, list string) error {
		if strings.TrimSpace(id) == "" {
			return invalid("tracker: a %s mark names no record", list)
		}
		if prior, twice := seen[id]; twice {
			return invalid("tracker: record %s is named by both %s and %s in "+
				"one gesture, and nothing could say which it meant", id, prior,
				list)
		}
		seen[id] = list
		return nil
	}
	for _, part := range []struct {
		ids  []string
		list string
	}{{g.Read, "read"}, {g.Unread, "unread"}, {g.Unsnooze, "unsnooze"}} {
		for _, id := range part.ids {
			if err := name(id, part.list); err != nil {
				return err
			}
		}
	}
	// A SNOOZE PAST THE HORIZON IS A DELETE THAT DOES NOT SAY SO: the item
	// leaves the inbox and the person who snoozed it will not be at the
	// company when it returns. Refused naming the entry rather than
	// clamped, because a clamp would put it back on a date nobody chose.
	// And one that is already due is not a snooze at all.
	horizon := now.Add(MaxSnoozeAhead)
	for _, snooze := range g.Snooze {
		if err := name(snooze.RecordID, "snooze"); err != nil {
			return err
		}
		switch {
		case !snooze.Until.After(now):
			return invalid("tracker: record %s is snoozed until %s, which has "+
				"already come — a snooze names an instant in the future",
				snooze.RecordID, snooze.Until.Format(time.RFC3339))
		case snooze.Until.After(horizon):
			return invalid("tracker: record %s is snoozed until %s, which is "+
				"more than %s away — a snooze that far out is a delete that "+
				"does not say so", snooze.RecordID,
				snooze.Until.Format(time.RFC3339), MaxSnoozeAhead)
		}
	}
	if len(seen) > MaxInboxEntries {
		// A GESTURE LONGER THAN A LIST CAN HOLD cannot land whatever the
		// record holds, so it is refused before the decide reads it.
		return fmt.Errorf("tracker: this gesture names %d notices and a list "+
			"holds at most %d — mark everything you have seen with "+
			"`read_through` instead: %w", len(seen), MaxInboxEntries,
			ErrInboxFull)
	}
	if at := g.ReadThrough; at != nil {
		if err := at.Valid(); err != nil {
			return invalid("tracker: %s is not a position to read through: %v",
				at, err)
		}
		if stream := (Domain{}).Stream().Name; at.Stream != stream {
			return invalid("tracker: %s is a position on %s, and an inbox is "+
				"read through a position on %s — the one `work_inbox` answers",
				at, at.Stream, stream)
		}
	}
	if g.PrimaryReasons != nil {
		for _, reason := range *g.PrimaryReasons {
			if !slices.Contains(Reasons, reason) {
				return invalid("tracker: %q is not a wake reason", reason)
			}
		}
	}
	return nil
}

// inboxPositions finds the log position of every notice a gesture names.
//
// FROM THE HISTORY ROW, INSIDE THE DECIDE. A mark has to carry the position
// its notice sits at, because the lists are pruned against the seen-through
// position — and a caller that supplied it could get it wrong in the one way
// that loses the mark silently: a read mark at position zero is pruned on the
// write that makes it. The history row is the record the notice points at and
// is never swept, so the position is a fact of the rows this snapshot holds
// rather than a claim the caller made.
//
// PACKED, `(generation << 40) | seq`, which is what every durable column in
// this domain stores: a mark from before a reanchor compares below one after
// it rather than as a small number in the same space.
func inboxPositions(ctx context.Context, tx *sql.Tx,
	g InboxGesture) (map[string]uint64, error) {

	ids := slices.Concat(g.Read, g.Unread, g.Unsnooze)
	for _, snooze := range g.Snooze {
		ids = append(ids, snooze.RecordID)
	}
	out := make(map[string]uint64, len(ids))
	for _, id := range ids {
		var packed int64
		switch err := tx.QueryRowContext(ctx,
			`SELECT log_seq FROM tracker_history WHERE id = ?`, id).
			Scan(&packed); {
		case errors.Is(err, sql.ErrNoRows):
			return nil, invalid("tracker: record %s is not a change this "+
				"company recorded — a mark names the `record_id` a "+
				"`work_inbox` notice carries", id)
		case err != nil:
			return nil, fmt.Errorf("tracker: read where record %s sits: %w",
				id, err)
		}
		out[id] = uint64(packed)
	}
	return out, nil
}

// applyInboxGesture moves a person's inbox by one gesture.
//
// PURE OVER VALUES — the record, the gesture, where each notice sits and the
// instant — so the rule is exercised without a database, and the decide is
// the only caller.
//
// # The two lists are overrides of the position, in opposite directions
//
// Everything at or below the seen-through position reads as read and
// everything above it as unread, so a list entry is only ever an EXCEPTION:
// `read` holds notices above the position somebody marked read out of order,
// and `unread` holds notices at or below it somebody marked unread again. An
// entry on the side the position already answers says nothing, and is dropped
// on every write — which is what keeps the record small without a cap that
// discards. Moving the position forward clears every `unread` exception,
// because "I have read everything up to here" is the gesture that says so.
//
// The ORDER: the position first, then the marks, so "read through here, but
// keep this one unread" is one gesture that means what it says.
func applyInboxGesture(p *Person, g InboxGesture,
	positions map[string]uint64, at time.Time) error {

	read := slices.Clone(p.Read)
	unread := slices.Clone(p.Unread)
	snoozed := slices.Clone(p.Snoozed)

	if g.ReadThrough != nil {
		next := Position{Stream: g.ReadThrough.Stream,
			Generation: g.ReadThrough.Generation, Seq: g.ReadThrough.Seq}
		if advances(p.SeenThrough, next) {
			p.SeenThrough = next
			// THE STREAM'S OWN IDENTITY rides beside the position — the
			// third belt [Person.Generation] describes.
			p.Generation = next.Stream
			unread = nil
		}
	}
	if g.PrimaryReasons != nil {
		p.PrimaryReasons = nilIfEmpty(slices.Clone(*g.PrimaryReasons))
	}

	entry := func(id string) InboxEntry {
		return InboxEntry{RecordID: id, Position: positions[id]}
	}
	for _, id := range g.Read {
		unread = withoutEntry(unread, id)
		snoozed = withoutEntry(snoozed, id)
		read = append(withoutEntry(read, id), entry(id))
	}
	for _, id := range g.Unread {
		read = withoutEntry(read, id)
		unread = append(withoutEntry(unread, id), entry(id))
	}
	for _, snooze := range g.Snooze {
		until := snooze.Until.UTC()
		mark := entry(snooze.RecordID)
		mark.Until = &until
		snoozed = append(withoutEntry(snoozed, snooze.RecordID), mark)
	}
	for _, id := range g.Unsnooze {
		snoozed = withoutEntry(snoozed, id)
	}

	seen := p.SeenThrough.packed()
	p.Read = keptEntries(read, func(e InboxEntry) bool { return seen == 0 || e.Position > seen })
	p.Unread = keptEntries(unread, func(e InboxEntry) bool { return seen != 0 && e.Position <= seen })
	// AN EXPIRED SNOOZE IS NOT AN ENTRY, it is an item that is back — and
	// nothing needs to move it, because a notice with no live snooze reads
	// by the position and the two lists like any other.
	p.Snoozed = keptEntries(snoozed, func(e InboxEntry) bool {
		return e.Until == nil || e.Until.After(at)
	})

	for _, part := range []struct {
		entries []InboxEntry
		what    string
	}{{p.Read, "read"}, {p.Unread, "unread"}, {p.Snoozed, "snoozed"}} {
		if len(part.entries) > MaxInboxEntries {
			return fmt.Errorf("tracker: %s's %s list would hold %d notices "+
				"and the maximum is %d — mark everything you have seen with "+
				"`read_through`, which moves the position the lists are kept "+
				"against: %w", p.Handle, part.what, len(part.entries),
				MaxInboxEntries, ErrInboxFull)
		}
	}
	return nil
}

// advances reports whether next is a position past the stored one.
//
// GENERATION FIRST, for the reason [statelog.Position.Before] states: a
// sequence from before a reanchor is below every sequence after it however
// large. A position on ANOTHER STREAM than the stored one replaces it, because
// the caller's was checked against the live stream and the stored one is
// therefore from a stream that no longer exists — comparing their numbers
// would be comparing two unrelated spaces.
func advances(stored, next Position) bool {
	if stored.Stream != next.Stream || stored.Seq == 0 {
		return true
	}
	return next.packed() > stored.packed()
}

// withoutEntry is the list with every entry for one record dropped, as a fresh
// slice.
func withoutEntry(entries []InboxEntry, id string) []InboxEntry {
	out := make([]InboxEntry, 0, len(entries))
	for _, e := range entries {
		if e.RecordID != id {
			out = append(out, e)
		}
	}
	return out
}

// keptEntries is the entries a rule keeps, nil when none are.
func keptEntries(entries []InboxEntry, keep func(InboxEntry) bool) []InboxEntry {
	out := make([]InboxEntry, 0, len(entries))
	for _, e := range entries {
		if keep(e) {
			out = append(out, e)
		}
	}
	return nilIfEmpty(out)
}

// dedupe drops repeats, preserving the first occurrence's order.
func dedupe[T comparable](in []T) []T {
	out := make([]T, 0, len(in))
	for _, v := range in {
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// nilIfEmpty is the stored form of an empty list: absent, so a record that
// holds nothing encodes exactly like one nobody wrote.
func nilIfEmpty[T any](in []T) []T {
	if len(in) == 0 {
		return nil
	}
	return in
}

// writePerson applies one part of a person's record, carrying the rest.
//
// EVERY VERB READS THE STORED ROW AND CARRIES THE REST. The three write
// disjoint parts of one document, and a verb that took the whole thing from
// its caller would let an inbox write clear somebody's pins by omission.
//
// NO NOTIFICATION: a mark, a snooze and a pin are the person's own
// bookkeeping and news to nobody — the one person write that wakes anybody is
// a lead's priority write, which is [Writer.writePersonNotifying] directly.
func (w *Writer) writePerson(ctx context.Context, opID, handle string,
	apply func(*sql.Tx, *Person, time.Time) error) (WriteResult, error) {

	// EVERY OTHER PERSON WRITE IS THE PERSON'S OWN BOOKKEEPING — a pin, an
	// inbox mark, a snooze — and it files under its own kind rather than
	// under the operation. The row is written either way; what changes is
	// whether a reader can name it.
	return w.writePersonNotifying(ctx, opID, handle, ChangePersonUpdated,
		func(tx *sql.Tx, post *Person, at time.Time) (*Notify, error) {
			return nil, apply(tx, post, at)
		})
}

// writePersonNotifying is the same write with a NOTIFICATION the apply decides.
//
// THE NOTIFY IS BUILT INSIDE THE DECIDE, which is not a convenience: the one
// person write that announces itself is a lead writing somebody else's
// priority list, and what it announces — the task now at the top, and its
// position — is a property of the list AFTER the apply, read from rows in the
// same snapshot. Built outside, it would name whichever task was first when
// the caller last looked.
func (w *Writer) writePersonNotifying(ctx context.Context, opID, handle string,
	kind ChangeKind,
	apply func(*sql.Tx, *Person, time.Time) (*Notify, error)) (WriteResult, error) {

	if strings.TrimSpace(handle) == "" {
		return WriteResult{}, invalid("tracker: a person write names nobody")
	}
	subject := PersonSubject(handle)
	// THE PERSON FAMILY, which is what a person subject resolves to
	// anyway: a person is not in a project and not in the workspace
	// container, so the family is the narrowest honest term.
	scope := ScopeSet{Subject: true}
	at := w.Now()
	return w.published(ctx, statelog.Request{
		Subject:  wire(subject),
		Scope:    scope.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx) (statelog.Decision, error) {
			post, held, err := readPerson(ctx, tx, handle)
			if err != nil {
				return statelog.Decision{}, err
			}
			post.V, post.Handle = DocumentVersion, handle
			before := post
			notify, err := apply(tx, &post, at)
			if err != nil {
				return statelog.Decision{}, err
			}
			if held && notify == nil && reflect.DeepEqual(before, post) {
				// A GESTURE THAT MOVES NOTHING PUBLISHES NOTHING: a
				// read-through behind the stored position, a notice
				// marked read twice, a retry of a write that landed.
				// Every apply builds fresh slices rather than editing
				// the ones it was handed, which is what makes the
				// comparison with the copy honest.
				//
				// ONLY ON A RECORD THAT EXISTS. The first write is what
				// creates a person, and the record's existence is itself
				// a fact a read turns on: a bound person's seat record,
				// once written, is the one answered instead of the one
				// their credential left.
				return statelog.Decision{Version: int64(post.Version)}, nil
			}
			post.UpdatedAt = at
			return w.decide(subject, OpPatch, kind, scope, opID, post, notify, at)
		},
	})
}

// cleanHandles trims, drops empties and deduplicates, preserving order.
func cleanHandles(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v = strings.TrimSpace(v); v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
