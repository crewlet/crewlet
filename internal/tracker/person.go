package tracker

import (
	"context"
	"database/sql"
	"fmt"
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
// # Why every verb reads the stored row first
//
// The three write disjoint parts of one document, so each has to carry the
// other two through unchanged. Taking them from the caller would let an inbox
// write clear somebody's pins by omission — the ordinary shape of a lost
// update, and the one a whole-document object invites.

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

// WriteInbox replaces a person's inbox state.
//
// ONLY ON BEHALF OF THAT PERSON. The engine writes this as the seat bound to
// the human — see the file head — so the actor and the handle must be the
// same, and the refusal says so rather than silently writing nothing.
func (w *Writer) WriteInbox(ctx context.Context, opID, handle string,
	read, unread, snoozed []InboxEntry, reasons []Reason,
	seenThrough Position) (WriteResult, error) {

	if err := ownRecord(w.Actor, handle, "inbox"); err != nil {
		return WriteResult{}, err
	}
	if err := checkInbox(read, unread, snoozed, reasons, w.Now()); err != nil {
		return WriteResult{}, err
	}
	return w.writePerson(ctx, opID, handle, func(post *Person, at time.Time) error {
		// PRUNED AT OR BELOW THE SEEN-THROUGH POSITION, which is what
		// keeps this object small without a cap that discards: an entry
		// the person has read past is one no surface will ever render.
		post.Read = prunedEntries(read, seenThrough)
		post.Unread = prunedEntries(unread, seenThrough)
		post.Snoozed = prunedSnoozes(snoozed, at)
		post.PrimaryReasons = reasons
		post.SeenThrough = seenThrough
		post.Generation = seenThrough.Stream
		return nil
	})
}

// WritePins replaces a person's pinned views and favourites.
//
// ONLY ON BEHALF OF THAT PERSON: a pin somebody else can set is one that moves
// under them.
func (w *Writer) WritePins(ctx context.Context, opID, handle string,
	pinnedViews []string, favorites []Favorite) (WriteResult, error) {

	if err := ownRecord(w.Actor, handle, "pins"); err != nil {
		return WriteResult{}, err
	}
	pinnedViews = cleanHandles(pinnedViews)
	switch {
	case len(pinnedViews) > MaxPinnedViews:
		return WriteResult{}, fmt.Errorf("tracker: %s pins %d views and the "+
			"maximum is %d — a strip where everything is first has no first",
			handle, len(pinnedViews), MaxPinnedViews)
	case len(favorites) > MaxFavorites:
		return WriteResult{}, fmt.Errorf("tracker: %s stars %d things and the "+
			"maximum is %d", handle, len(favorites), MaxFavorites)
	}
	for _, favorite := range favorites {
		if favorite.Kind == "" || favorite.ID == "" {
			return WriteResult{}, fmt.Errorf("tracker: a favourite of %s names "+
				"kind %q and id %q, and a star with neither points at nothing",
				handle, favorite.Kind, favorite.ID)
		}
	}
	return w.writePerson(ctx, opID, handle, func(post *Person, _ time.Time) error {
		post.PinnedViews = pinnedViews
		post.Favorites = favorites
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
func (w *Writer) WritePriorities(ctx context.Context, opID, handle string,
	priorities []string, authority PersonAuthority) (WriteResult, error) {

	priorities = cleanHandles(priorities)
	own := w.Actor == handle
	switch {
	case !own && !authority.Lead && !authority.Person:
		return WriteResult{}, fmt.Errorf("tracker: %s is a seat, is not %s and "+
			"does not lead them, so it cannot set what %s does next — an agent "+
			"re-ordering a colleague's list is a hand-off in disguise, and it "+
			"bypasses the guarded take and the reassignment budget. A lead, a "+
			"human or an operator may: %w",
			w.Actor, handle, handle, statelog.ErrConflict)
	case len(priorities) > MaxPriorities:
		return WriteResult{}, fmt.Errorf("tracker: %s's priority list carries "+
			"%d items and the maximum is %d — a list longer than that is not "+
			"an order, it is the backlog again", handle, len(priorities),
			MaxPriorities)
	}
	return w.writePersonNotifying(ctx, opID, handle,
		func(tx *sql.Tx, post *Person, at time.Time) (*Notify, error) {
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
			return w.prioritisedWake(ctx, tx, handle, priorities, at)
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
	priorities []string, at time.Time) (*Notify, error) {

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
func ownRecord(actor, handle, what string) error {
	if actor == handle {
		return nil
	}
	return fmt.Errorf("tracker: %s cannot write %s's %s — it is written on "+
		"behalf of the person whose it is, and somebody else's hand in it is "+
		"the one thing it must never allow: %w",
		actor, handle, what, statelog.ErrConflict)
}

// checkInbox refuses an inbox nothing could render.
func checkInbox(read, unread, snoozed []InboxEntry, reasons []Reason,
	now time.Time) error {

	for _, part := range []struct {
		entries []InboxEntry
		what    string
	}{{read, "read"}, {unread, "unread"}, {snoozed, "snoozed"}} {
		if len(part.entries) > MaxInboxEntries {
			return fmt.Errorf("tracker: the %s list carries %d entries and the "+
				"maximum is %d — entries at or below the seen-through position "+
				"are pruned on every write, so a list this long is one nothing "+
				"has read past", part.what, len(part.entries), MaxInboxEntries)
		}
		for _, entry := range part.entries {
			if entry.RecordID == "" {
				return fmt.Errorf("tracker: a %s entry names no record",
					part.what)
			}
		}
	}
	// A SNOOZE PAST THE HORIZON IS A DELETE THAT DOES NOT SAY SO: the item
	// leaves the inbox and the person who snoozed it will not be at the
	// company when it returns. Refused naming the entry rather than
	// clamped, because a clamp would put it back on a date nobody chose.
	horizon := now.Add(MaxSnoozeAhead)
	for _, entry := range snoozed {
		if entry.Until != nil && entry.Until.After(horizon) {
			return fmt.Errorf("tracker: record %s is snoozed until %s, which is "+
				"more than %s away — a snooze that far out is a delete that "+
				"does not say so", entry.RecordID,
				entry.Until.Format(time.RFC3339), MaxSnoozeAhead)
		}
	}
	for _, reason := range reasons {
		if !slices.Contains(Reasons, reason) {
			return fmt.Errorf("tracker: %q is not a wake reason", reason)
		}
	}
	return nil
}

// prunedEntries drops what the person has already read past.
//
// AT OR BELOW, and the GENERATION decides first: a sequence from a dead
// sequence space compares as if it were current, which is exactly the failure
// [Position] is a triple for. An entry from another generation is KEPT rather
// than pruned, because the honest reading of "I cannot compare these" is not
// "you have seen it".
func prunedEntries(entries []InboxEntry, seen Position) []InboxEntry {
	if seen.Seq == 0 {
		return entries
	}
	out := make([]InboxEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.Position > seen.Seq {
			out = append(out, entry)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// prunedSnoozes drops a snooze whose time has come.
//
// AN EXPIRED SNOOZE IS NOT AN ENTRY, it is an item that belongs back in the
// unread list — and putting it there is the CALLER's, which is why this only
// removes. A prune that promoted would make one write mean two things.
func prunedSnoozes(entries []InboxEntry, now time.Time) []InboxEntry {
	out := make([]InboxEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.Until != nil && !entry.Until.After(now) {
			continue
		}
		out = append(out, entry)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// writePerson applies one part of a person's record, carrying the rest.
//
// EVERY VERB READS THE STORED ROW AND CARRIES THE REST. The three write
// disjoint parts of one document, and a verb that took the whole thing from
// its caller would let an inbox write clear somebody's pins by omission.
//
// NO NOTIFICATION ON ANY PATH: see [Writer.WritePriorities] for why a
// task-shaped wake cannot describe a person record.
func (w *Writer) writePerson(ctx context.Context, opID, handle string,
	apply func(*Person, time.Time) error) (WriteResult, error) {

	return w.writePersonNotifying(ctx, opID, handle,
		func(_ *sql.Tx, post *Person, at time.Time) (*Notify, error) {
			return nil, apply(post, at)
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
	apply func(*sql.Tx, *Person, time.Time) (*Notify, error)) (WriteResult, error) {

	if strings.TrimSpace(handle) == "" {
		return WriteResult{}, fmt.Errorf("tracker: a person write names nobody")
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
			post, _, err := readPerson(ctx, tx, handle)
			if err != nil {
				return statelog.Decision{}, err
			}
			post.V, post.Handle, post.UpdatedAt = DocumentVersion, handle, at
			notify, err := apply(tx, &post, at)
			if err != nil {
				return statelog.Decision{}, err
			}
			return w.decide(subject, OpPatch, scope, opID, post, notify, at)
		},
	})
}
