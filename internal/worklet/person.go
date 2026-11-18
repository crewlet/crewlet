package worklet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A person's record has three authorities, so three verbs, each carrying only
// what its authority covers, rather than one verb dropping fields it refuses:
//
//	INBOX     marks, snoozes, the seen-through position: the owner only, since
//	          another's "read" removes it from the one place you would look.
//	PINS      pinned views and favourites: the owner only.
//	PRIORITY  the ordered next-work list: the owner or their lead, whose write
//	          notifies (`prioritised`).
//
// Inbox and pins take a GESTURE resolved against the decide's snapshot, so a
// stale tab cannot erase marks made since; priorities stay whole, under ifMatch.

// PersonAuthority is what a caller may write on somebody else's record: a
// value rather than a bool on the writer, because the answer is about a pair
// (who asks, about whom). It states the decision, not its inputs:
// [authz.ClassOwnOrLead] decides owner, lead or admin for every surface; what
// stays here is which verb each answer unlocks, and the one invariant no
// caller may waive: a seat never writes a colleague's record.
type PersonAuthority struct {
	// Authorized reports a caller the authority table admitted for this
	// person's record: the owner, whoever leads them, or an admin.
	Authorized bool

	// Agent reports a caller acting AS A SEAT rather than as a human. This
	// package's own rule, about the gesture rather than authority: an agent
	// re-ordering a colleague's queue is a hand-off in disguise that bypasses
	// the guarded take and the reassignment budget, so a seat that leads the
	// person is admitted by the table and still refused here.
	Agent bool
}

// MarkInbox applies one inbox gesture to a person's own record
// ([Writer.ownRecord]; the handle as [iam.RecordOwner] names it), resolved in
// the decide by [inbox.Apply] so a refused round re-applies it to the record
// that won. A list left past [inbox.MaxEntries] is refused [inbox.ErrFull],
// never trimmed; a no-op publishes nothing; [inbox.CheckGesture]'s refusals
// are marked [ErrInvalid] with their sentence unchanged.
//
// SEEN, how far the asker read the tracker log, is this call's floor, awaited
// ([Writer.After]) since the deciding node may lag the one that showed the
// notice. Unreached in the write budget: [statelog.ReasonBehind], nothing
// appended. Zero waits for nothing; another log's position is [ErrInvalid].
// Never the node's session floor (estate.Session): it would refuse every request.
func (w *Writer) MarkInbox(ctx context.Context, opID, handle string, seen statelog.Position,
	gesture inbox.Gesture[Reason], authority PersonAuthority) (WriteResult, error) {

	if err := w.ownRecord(handle, "inbox", authority); err != nil {
		return WriteResult{}, err
	}
	if err := inbox.CheckGesture(gesture, trackerStream, Reasons, w.Now()); err != nil {
		if errors.Is(err, inbox.ErrInvalid) {
			return WriteResult{}, invalid("%s", inbox.Sentence(err))
		}
		return WriteResult{}, err
	}
	if !seen.IsZero() {
		if err := seen.On(trackerStream); err != nil {
			return WriteResult{}, invalid("seen: %v", err)
		}
		w = w.After(seen)
	}
	return w.writePerson(ctx, opID, handle, func(tx *sql.Tx, post *Person,
		at time.Time) error {

		positions, err := inboxPositions(ctx, tx, gesture)
		if err != nil {
			return err
		}
		return inbox.Apply(&post.Marks, gesture, positions, at)
	})
}

// SetChange is a gesture over one of a person's own sets: Set states the whole
// collection, Add and Remove a delta against what the decide reads, never both
// ([RelationIntent] says why). A screen sends the delta, since a whole list
// from an earlier read drops what another tab added; Set is for a caller that
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

// asked is how many entries the change itself would put in the collection,
// which a cap can refuse before the stored set is read.
func (c SetChange[T]) asked() int {
	if c.Set != nil {
		return len(*c.Set)
	}
	return len(c.Add)
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

// WritePins applies one pin gesture to a person's own record
// ([Writer.ownRecord]). The gesture is applied to what the decide reads, so a
// star from one tab cannot drop a pin made in another, and the caps hold
// against the set it would LEAVE, the set a strip renders: a small delta can
// still be the thirty-third pin.
func (w *Writer) WritePins(ctx context.Context, opID, handle string,
	gesture PinGesture, authority PersonAuthority) (WriteResult, error) {

	if err := w.ownRecord(handle, "pins", authority); err != nil {
		return WriteResult{}, err
	}
	named := handle
	gesture.Views = gesture.Views.mapped(strings.TrimSpace)
	switch {
	case gesture.Views.Empty() && gesture.Favorites.Empty():
		return WriteResult{}, invalid("this pin gesture for %s names "+
			"nothing to pin, unpin, star or unstar", named)
	case gesture.Views.both(), gesture.Favorites.both():
		return WriteResult{}, invalid("this pin gesture for %s states "+
			"a whole set and a change to it at once — a caller states one or "+
			"the other, because whichever won would discard the other", named)
	}
	// A gesture past the cap on its own is invalid, since no state of the
	// record lets it land; one that only pushes the record past it is the
	// record's state, refused below on the set it would leave.
	switch {
	case gesture.Views.asked() > MaxPinnedViews:
		return WriteResult{}, invalid("this gesture names %d views for %s "+
			"and a person pins at most %d", gesture.Views.asked(), named,
			MaxPinnedViews)
	case gesture.Favorites.asked() > MaxFavorites:
		return WriteResult{}, invalid("this gesture names %d things for %s "+
			"to star and a person stars at most %d", gesture.Favorites.asked(),
			named, MaxFavorites)
	}
	for _, view := range gesture.Views.values() {
		if strings.TrimSpace(view) == "" {
			return WriteResult{}, invalid("a pinned view of %s names "+
				"no view, and a pin with no view points at nothing", named)
		}
	}
	for _, favorite := range gesture.Favorites.values() {
		if favorite.Kind == "" || favorite.ID == "" {
			return WriteResult{}, invalid("a favourite of %s names "+
				"kind %q and id %q, and a star with neither points at nothing",
				named, favorite.Kind, favorite.ID)
		}
	}
	return w.writePerson(ctx, opID, handle, func(tx *sql.Tx, post *Person,
		_ time.Time) error {

		views, err := withoutDeletedViews(ctx, tx, gesture.Views.apply(post.PinnedViews))
		if err != nil {
			return err
		}
		favorites := gesture.Favorites.apply(post.Favorites)
		// The record's state: the gesture fits, the set it would leave does
		// not. (A strip where everything is first has no first.)
		switch {
		case len(views) > MaxPinnedViews:
			return refusedByState("that would make %d pinned views for %s, "+
				"and %d is the most. Unpin one first", len(views), named, MaxPinnedViews)
		case len(favorites) > MaxFavorites:
			return refusedByState("that would make %d starred things for %s, "+
				"and %d is the most. Unstar one first", len(favorites), named, MaxFavorites)
		}
		post.PinnedViews, post.Favorites = views, favorites
		return nil
	})
}

// withoutDeletedViews is a pin list less the views somebody deleted. A view's
// delete cannot unpin (each pin lives on its pinner's own record), so a
// deleted view stays pinned, drawn nowhere, until its person's pins next
// change; it is dropped here before the cap is counted, or it would hold one
// of the [MaxPinnedViews] places for good. A view with no row here is kept:
// this copy may not have caught up with it.
func withoutDeletedViews(ctx context.Context, tx *sql.Tx, views []string) ([]string, error) {
	if len(views) == 0 {
		return views, nil
	}
	ids := make([]any, len(views))
	for i, id := range views {
		ids[i] = id
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT id FROM tracker_views
		WHERE id IN (`+placeholders(len(ids))+`) AND deleted_at IS NOT NULL`, ids...)
	if err != nil {
		return nil, fmt.Errorf("worklet: read which pinned views were deleted: %w", err)
	}
	defer func() { _ = rows.Close() }()
	deleted := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("worklet: read which pinned views were deleted: %w", err)
		}
		deleted[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("worklet: read which pinned views were deleted: %w", err)
	}
	if len(deleted) == 0 {
		return views, nil
	}
	return slices.DeleteFunc(slices.Clone(views), func(id string) bool { return deleted[id] }), nil
}

// WritePriorities replaces a person's ordered list of what to do next: their
// own, or a lead's for somebody in their line. The caller resolves the lead
// relation (this package has no org chart); one that did not may write only
// its own, and a seat never a colleague's ([PersonAuthority]).
//
// A lead's write both stamps the record and wakes the person
// ([Writer.prioritisedWake]): a stamp is seen only by somebody who opens a
// screen, and a seat has none.
//
// The list stays whole, since an order is a statement about every entry, and
// ifMatch keeps it from being last-write-wins: a stale version is refused
// [ErrStaleVersion]. Nil writes unconditionally; zero means "never written".
func (w *Writer) WritePriorities(ctx context.Context, opID, handle string,
	priorities []string, ifMatch *uint64,
	authority PersonAuthority) (WriteResult, error) {

	priorities = cleanHandles(priorities)
	own := w.Actor == handle
	actor, named := w.Actor, handle
	switch {
	case !own && (!authority.Authorized || authority.Agent):
		return WriteResult{}, forbidden("%s is a seat, is not %s and does not "+
			"lead them, so it cannot set what %s does next — an agent "+
			"re-ordering a colleague's list is a hand-off in disguise, and it "+
			"bypasses the guarded take and the reassignment budget. A lead, a "+
			"human or an operator may",
			actor, named, named)
	case len(priorities) > MaxPriorities:
		return WriteResult{}, invalid("%s's priority list carries "+
			"%d items and the maximum is %d — a list longer than that is not "+
			"an order, it is the backlog again", named, len(priorities),
			MaxPriorities)
	}
	return w.writePersonNotifying(ctx, opID, handle, ChangePrioritised,
		func(tx *sql.Tx, post *Person, at time.Time) (*Notify, error) {
			// Compared inside the decide: a comparison before it would pass
			// a record that moved between the two reads.
			if ifMatch != nil && *ifMatch != post.Version {
				return nil, fmt.Errorf("worklet: %s's priority list is at "+
					"version %d and this reorder was made against %d — read "+
					"it again and decide from what it says now: %w",
					named, post.Version, *ifMatch, ErrStaleVersion)
			}
			post.Priorities = priorities
			// The stamp shows a lead's reorder on the screen the person
			// already looks at; their own write clears it, since taking the
			// queue back says they have seen it.
			post.PrioritiesSetBy, post.PrioritiesSetAt = "", time.Time{}
			if own {
				// Your own list wakes nobody: the one tracker write that
				// vetoes its own delivery per call.
				return nil, nil
			}
			// The actor writes AS the seat the directory binds them to
			// ([iam.ActorFor]), so the stamp names somebody in the chart,
			// never a credential's label.
			post.PrioritiesSetBy, post.PrioritiesSetAt = w.Actor, at
			// And the wake, for a seat with no screen: the one Addressed
			// wake of the four, which a seat takes up or says why it cannot.
			return w.prioritisedWake(ctx, tx, handle, priorities)
		})
}

// prioritisedWake is what a lead writing somebody's queue announces. It names
// the task at the top, since the list does not record what changed and the
// first entry answers "what do I do next". An empty list wakes nobody: it
// takes an instruction back, and there is no task to name.
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
		// A task this node has not applied yet: the list is still written,
		// but a card with an empty key and title is worse than silence.
		return nil, nil
	}
	// The excerpt names the task as a reader would open it: a duplicate key
	// named by the key alone sends the seat to the task that claimed it.
	// [Writer.decide] derives the snapshot's flag from the same snapshot, so
	// the two agree.
	collision, err := keyHeldByAnother(ctx, tx, top.Key, top.ID)
	if err != nil {
		return nil, fmt.Errorf("worklet: read whether key %s opens task %s: %w",
			top.Key, top.ID, err)
	}
	return &Notify{
		Kind: ChangePrioritised,
		Snapshot: Snapshot{
			Person: handle,
			Task:   top.ID,
			Key:    top.Key,
			Title:  top.Title,
			// The card renders project and status, so the seat need not
			// open the task to learn whether it is already done.
			Project:     top.Project,
			Status:      top.Status,
			StatusGroup: top.StatusGroup,
			Assignee:    top.Assignee,
			// As the stamp names them ([Writer.WritePriorities]).
			PrioritisedBy: w.Actor,
			Position:      1,
		},
		Excerpt: fmt.Sprintf("%s put %s at position 1 of your priorities",
			w.Actor,
			ItemNamed(top.ID, top.Key, collision)),
	}, nil
}

// ownRecord refuses a write on somebody else's half of a person record: the
// owner or the admin path (so a departed person's queue can be unstuck), never
// an agent, and not a lead, who re-orders work ([Writer.WritePriorities]) but
// has no call to mark somebody's mail read ([authz.ClassOwnRecord]). Both
// sides are handles: the writer's actor ([Writer.As]) and the record's.
func (w *Writer) ownRecord(handle, what string, authority PersonAuthority) error {
	switch {
	case w.Actor == handle:
		return nil
	case authority.Authorized && !authority.Agent:
		return nil
	}
	return forbidden("%s cannot write %s's %s — it is written on behalf of "+
		"the person whose it is, and somebody else's hand in it is the one "+
		"thing it must never allow",
		w.Actor, handle, what)
}

// recordPositionStatement is where one change sits in the log, read by
// [inboxPositions] and named for the plan test.
const recordPositionStatement = `SELECT log_seq FROM tracker_history WHERE id = ?`

// inboxPositions finds the log position of every notice a gesture names, from
// its history row (never swept) inside the decide. The lists are pruned
// against the seen-through position, so a caller-supplied position could lose
// a mark silently: a read mark at zero is pruned by the write that makes it.
//
// Packed `(generation << 40) | seq`, like every durable column here, so a mark
// from before a reanchor compares below one after it.
func inboxPositions(ctx context.Context, tx *sql.Tx,
	g inbox.Gesture[Reason]) (map[string]uint64, error) {

	ids := g.Records()
	out := make(map[string]uint64, len(ids))
	for _, id := range ids {
		var packed int64
		switch err := tx.QueryRowContext(ctx, recordPositionStatement, id).
			Scan(&packed); {
		case errors.Is(err, sql.ErrNoRows):
			return nil, invalid("record %s is not a change this "+
				"company recorded — a mark names the `record_id` a "+
				"`%s` notice carries", id, WorkInboxTool)
		case err != nil:
			return nil, fmt.Errorf("worklet: read where record %s sits: %w",
				id, err)
		}
		out[id] = uint64(packed)
	}
	return out, nil
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

// writePerson applies one part of a person's record, carrying the rest read
// from the stored row: the verbs write disjoint parts of one document, and one
// taking it whole from its caller would let an inbox write clear somebody's
// pins by omission. It notifies nobody; only a lead's priority write does,
// through [Writer.writePersonNotifying] directly.
func (w *Writer) writePerson(ctx context.Context, opID, handle string,
	apply func(*sql.Tx, *Person, time.Time) error) (WriteResult, error) {

	// Bookkeeping files under its own kind rather than the operation, so a
	// reader can name it.
	return w.writePersonNotifying(ctx, opID, handle, ChangePersonUpdated,
		func(tx *sql.Tx, post *Person, at time.Time) (*Notify, error) {
			return nil, apply(tx, post, at)
		})
}

// writePersonNotifying is the same write with a notification the apply
// decides, built inside the decide: what a lead's priority write announces
// (the task now at the top) is a property of the list after the apply, read
// in the same snapshot, not whichever task was first when the caller looked.
func (w *Writer) writePersonNotifying(ctx context.Context, opID, handle string,
	kind ChangeKind,
	apply func(*sql.Tx, *Person, time.Time) (*Notify, error)) (WriteResult, error) {

	if strings.TrimSpace(handle) == "" {
		return WriteResult{}, invalid("a person write names nobody")
	}
	subject := PersonSubject(handle)
	// The person family, the narrowest honest term: a person is in no
	// project and not in the workspace container.
	scope := ScopeSet{Subject: true}
	at := w.Now()
	return w.published(ctx, statelog.Request{
		Subject: wire(subject),
		Scope:   scope.Resolve(subject),
		OpID:    opID,
		Pattern: statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
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
				// A gesture that moves nothing publishes nothing. Every
				// apply builds fresh slices, which keeps this comparison
				// honest. Only on a record that exists: the first write
				// creates it, and from then on its defaults are the
				// stored ones, not the shipped ones.
				return statelog.Decision{Version: int64(post.Version)}, nil
			}
			post.UpdatedAt = at
			return w.decide(ctx, tx, stamp, subject, OpPatch, kind, scope, opID, post, notify, at)
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
