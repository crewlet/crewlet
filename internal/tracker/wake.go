package tracker

import (
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/textcut"
)

// WHAT A CHANGE TELLS PEOPLE, decided at the WRITE and carried on the record.
//
// # Why the snapshot is built here and never at the wake
//
// The node that wins a change feed's delivery is rarely the node that runs the
// woken seat, and it may be behind on the very task the record is about. So a
// wake carries the task's routing state COPIED at the moment of the change —
// who was assigned, who was watching, who leads the project, what the previous
// assignee and status group were — rather than a task id somebody looks up.
// A node routing from its own row would route from a row that has moved on,
// or would have to block the feed until it caught up.
//
// The previous values are the sharpest case. Routing turns on the TRANSITION
// and not on the destination: "entered or left a finished group" is what wakes
// a reporter and a parent's assignee, and "was reassigned away from" is what
// tells the person who no longer has it. Neither is reconstructible later from
// a row that already holds the new value.

// Leads resolves the two fallbacks a wake may need.
//
// BOTH ARE CARRIED, because which one applies depends on whether the task has
// a routing unit at the moment of the change — a fact the receiving node
// cannot reconstruct afterwards.
type Leads interface {
	// ProjectLead is who hears about unassigned work in a project.
	ProjectLead(project string) string

	// UnitLead is who hears about work routed to a unit.
	UnitLead(unit string) string
}

// Wake is what a write says about itself, before it is published.
//
// A VALUE THE CALLER FILLS AND THIS PACKAGE COMPLETES. The caller knows what
// it did — it created, it commented, it reassigned — and everything else on a
// notification is derivable from the two states of the task, so the caller
// states the kind and the rest is computed rather than remembered.
type Wake struct {
	// Kind is what happened, as a recipient needs to be told it.
	Kind ChangeKind

	// Before is the task as it stood, and After as it will stand. Before
	// is the zero value on a create, which is what makes every delta on a
	// create a field with no previous value rather than an empty diff.
	Before, After Task

	// Comment is the comment this change carried, if any.
	Comment *Comment

	// Mentions are the handles the text addressed, resolved by the caller
	// — a mention resolved at READ time names whoever holds the role
	// later, which is a different person.
	Mentions []string

	// Excerpt overrides what a card shows. Empty derives one from the
	// comment, or from the body on a create.
	Excerpt string

	// Dependents, Parent and Thread are the three routing facts this
	// package cannot derive from the two states of ONE task, and they are
	// filled by the caller for the same reason [Wake.Mentions] is: every
	// one of them is about a DIFFERENT row, and resolving another row at
	// the wake would resolve it from a node that may never have applied
	// it.
	//
	// Each is also the only thing standing between a reason and silence.
	// [Candidates] reads `Snapshot.Dependents` for `blocking`,
	// `Snapshot.ParentAssignee` for `parent_assignee`, and the thread for
	// `asked`, `answered` and `thread` — and a field nothing fills routes
	// to nobody without ever looking wrong, because an empty handle is
	// dropped by the candidate builder's own guard.

	// Dependents names the tasks a MIRROR commit added to this blocker,
	// with each one's key and assignee. Set on the blocker's side of a
	// dependency and on the duty's repair of one, and on nothing else: a
	// new dependent is a fact for the blocker's assignee to weigh.
	Dependents []TaskParty

	// Parent is this task's parent as it stood, carried so a child
	// entering or leaving a finished group can tell the parent's
	// assignee. Nil on a root, and on every change that is not a status
	// move across that edge.
	Parent *TaskParty

	// Thread is the comment conversation this change joins: who has
	// already spoken, who asked, and who is being answered. Empty on
	// every change that is not a comment.
	Thread ThreadParties
}

// ThreadParties is what a comment's routing needs to know about the
// conversation it lands in.
//
// RESOLVED BY THE CALLER, from the thread rows, because [Candidates] is pure
// over the notification and the node that routes the wake is rarely the node
// that holds the thread.
type ThreadParties struct {
	// Participants are the handles already in this thread — the parent
	// comment's author and everyone who has replied. Capped at
	// [MaxThreadParticipants] by the caller's own query; this package
	// cuts what arrives longer, deterministically, so two nodes reading
	// one record agree.
	Participants []string

	// Asked is the handle THIS comment asks, which is a question somebody
	// now owes an answer to.
	Asked string

	// AnsweredAuthor is the author of the comment this one ANSWERS.
	//
	// The author of the answering comment is the ANSWERER, so routing off
	// it would wake the seat that just replied and leave the person who
	// asked unwoken. That is why this is a snapshot field and not a
	// lookup at the wake.
	AnsweredAuthor string
}

// Notify builds the record's routing snapshot, or nil when this change wakes
// nobody.
//
// NIL IS THE ONLY WAY TO SAY "QUIET", and it is a question about the record's
// own shape rather than a flag a writer can forget to set: a quiet commit is a
// full record, arbitrated exactly like a loud one, and writes its history row
// like every other.
func (w Wake) Notify(leads Leads) *Notify {
	if !w.Kind.Valid() {
		return nil
	}
	notify := &Notify{
		Kind:     w.Kind,
		Mentions: w.Mentions,
		Fields:   w.deltas(),
		Excerpt:  w.excerpt(),
		Snapshot: w.snapshot(leads),
	}
	if w.Comment != nil {
		notify.CommentID = w.Comment.ID
	}
	return notify
}

// snapshot copies the routing state at the moment of the change.
func (w Wake) snapshot(leads Leads) Snapshot {
	after := w.After
	snapshot := Snapshot{
		Key:         after.Key,
		Project:     after.Project,
		Title:       after.Title,
		Status:      after.Status,
		StatusGroup: after.StatusGroup,
		Assignee:    after.Assignee,
		Reporter:    after.Reporter,
		// THE SET MINUS THE MUTED, so the feed never has to subtract and
		// can never forget to. The mute itself travels in the mutation
		// payload whenever the watcher collection is touched.
		Watchers:        without(after.Watchers, after.Muted),
		Collaborators:   slices.Clone(after.Collaborators),
		PrevAssignee:    w.Before.Assignee,
		PrevStatusGroup: w.Before.StatusGroup,
	}
	if w.Comment != nil {
		// WHO WROTE IT DECIDES WHETHER THE ASSIGNEE IS ADDRESSED, which
		// is the difference between a turn that answers and one that
		// absorbs — see [Notify.assigneeAddressed]. It was never copied
		// here, so the field was the zero AuthorKind on every record
		// ever written and the comparison against `human`/`operator`
		// was false for every comment: a person asking an agent a
		// question in a comment produced an unaddressed wake, and the
		// agent read it as activity to note rather than a question to
		// answer.
		snapshot.CommentAuthorKind = w.Comment.AuthorKind
	}
	if w.Kind == ChangeWatchers {
		// THE HANDLES THIS COMMIT DROPPED, which only the two states
		// know. [Candidates] reads them on this kind alone and writes
		// them to the INBOX rather than routing them — a human learns
		// that a lead removed her watch — and with nothing filling the
		// field that never happened at all.
		snapshot.RemovedWatchers = without(w.Before.Watchers, after.Watchers)
	}
	if leads != nil {
		snapshot.ProjectLead = leads.ProjectLead(after.Project)
		if after.RoutingUnit != "" {
			snapshot.RoutingUnitLead = leads.UnitLead(after.RoutingUnit)
		}
	}
	if w.Kind == ChangeRouted && after.RoutingUnit != w.Before.RoutingUnit {
		// THE NEW UNIT'S LEAD IS AN ORDINARY CANDIDATE, never the
		// fallback — which is the whole reason this field exists beside
		// RoutingUnitLead rather than being read from it. A fallback
		// survives only when no ordinary candidate did, so on any task
		// that still has an assignee, a collaborator or a watcher the
		// new lead would be dropped in silence.
		//
		// GATED ON THE UNIT ACTUALLY MOVING. A `routed` commit that
		// re-asserts the unit it already had tells its lead nothing
		// they do not know, and a walk that re-stamps a thousand tasks
		// with the unit they are already in would otherwise wake one
		// person a thousand times.
		snapshot.RoutedTo = snapshot.RoutingUnitLead
	}
	if len(w.Dependents) > 0 {
		snapshot.Dependents = capParties(w.Dependents, MaxDependents)
	}
	if w.Parent != nil && w.Kind == ChangeStatus &&
		w.Before.StatusGroup.Finished() != after.StatusGroup.Finished() {
		// ONLY ACROSS THE FINISHED EDGE, which is the one transition
		// the parent's assignee is told about — and the gate is the
		// same predicate [Candidates] applies, computed from the same
		// two values, so a field filled here can never be one the
		// router will not read.
		snapshot.ParentAssignee = w.Parent.Assignee
	}
	if lists := checklistAssignees(w.Before, after); len(lists) > 0 {
		snapshot.ChecklistAssignees = lists
	}
	if w.Comment != nil && w.Kind == ChangeComment {
		// THE CREATION KIND ALONE. A comment becomes a question when it
		// is WRITTEN — see [Comment.Ask] — and an edit re-sends the whole
		// comment document, so a snapshot filled on `comment_edited`
		// would wake the asked party again for a typo fix, and re-wake
		// the person who asked a question that was answered days ago.
		snapshot.CommentAsk = w.Thread.Asked
		snapshot.AnsweredAuthor = w.Thread.AnsweredAuthor
		snapshot.ThreadParticipants = capHandles(
			w.Thread.Participants, MaxThreadParticipants)
	}
	return snapshot
}

// checklistAssignees is everybody whose checklist item this change touched.
//
// BY ITEM ID rather than by position, because a checklist is reordered and
// renamed constantly and a positional diff would report the whole list as
// changed the first time somebody dragged a line. An item counts as touched
// when it appeared, disappeared, or changed in any way its assignee would care
// about — its text, its done flag, its owner, or the subtask it was promoted
// into.
//
// The set is SORTED and deduped, because two nodes read the record rather than
// re-deriving it and a slice in map order would put two spellings of one
// notification in the store.
func checklistAssignees(before, after Task) []string {
	was := itemsByID(before)
	now := itemsByID(after)
	touched := map[string]bool{}
	mark := func(item ChecklistItem) {
		if item.Assignee != "" {
			touched[item.Assignee] = true
		}
	}
	for id, item := range now {
		previous, held := was[id]
		switch {
		case !held:
			mark(item)
		case previous != item:
			// BOTH OWNERS, because a reassigned item is news to the
			// person who had it as much as to the person who has it.
			mark(item)
			mark(previous)
		}
	}
	for id, item := range was {
		if _, held := now[id]; !held {
			mark(item)
		}
	}
	out := make([]string, 0, len(touched))
	for handle := range touched {
		out = append(out, handle)
	}
	sort.Strings(out)
	return capHandles(out, MaxChecklists)
}

// itemsByID flattens a task's checklists to their items.
func itemsByID(task Task) map[string]ChecklistItem {
	out := map[string]ChecklistItem{}
	for _, list := range task.Checklists {
		for _, item := range list.Items {
			if item.ID != "" {
				out[item.ID] = item
			}
		}
	}
	return out
}

// capHandles cuts a handle set to its cap, deduped, order preserved.
//
// CUT RATHER THAN REFUSED, unlike every collection a caller states: these are
// derived from rows, so a set over its cap is a task that grew rather than a
// writer that asked for too much — and refusing the write would fail somebody's
// comment because a thread has many voices. The cap bounds what rides on the
// record; nobody is silenced, because a participant past it is still a watcher.
func capHandles(in []string, cap int) []string {
	out := make([]string, 0, min(len(in), cap))
	seen := make(map[string]bool, len(in))
	for _, handle := range in {
		if handle == "" || seen[handle] {
			continue
		}
		seen[handle] = true
		if len(out) == cap {
			break
		}
		out = append(out, handle)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// capParties is the same cut over the two lists that name somebody else's
// task, keyed on the task rather than on the handle.
func capParties(in []TaskParty, cap int) []TaskParty {
	out := make([]TaskParty, 0, min(len(in), cap))
	seen := make(map[string]bool, len(in))
	for _, party := range in {
		if party.Task == "" || seen[party.Task] {
			continue
		}
		seen[party.Task] = true
		if len(out) == cap {
			break
		}
		out = append(out, party)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// deltas are the fields that moved, as TEXT.
//
// Text rather than the typed value, because a change record is read by a card,
// a person and a model, and all three want "todo → in_progress". The typed
// value is on the row for anything that needs it.
//
// CAPPED AT THE DISPLAY LIMIT and no lower: the cap governs what a card SHOWS
// and never what the mutation carries, and a writer that hit it should be
// trimming what it shows rather than what it recorded.
func (w Wake) deltas() map[string]Delta { return TaskDeltas(w.Before, w.After) }

// TaskDeltas is what changed between two versions of a task.
//
// EXPORTED AND SHARED, because two frames need the same answer and a second
// copy is how one stops matching the other: the WRITER computes it for the
// notification card, and the APPLIER computes it for the history row — which
// every quiet commit also writes, and which the entered stamp is derived
// from. While this was a method on [Wake] the applier had no way to reach it,
// so a quiet status change wrote a history row with NO deltas, and
// `status_entered_at` went on naming an older change than the task had
// actually last made.
//
// IT IS THE TASK'S HALF OF ONE RULE, and `deltas.go` is the other: every apply
// that writes a history row records what it moved, computed by the APPLIER
// from the two states it holds. Read that file's head before adding a field
// here — the bounds, the text forms and the reason a value is never a
// rendering are stated there once for every object.
func TaskDeltas(before, after Task) map[string]Delta {
	moved := deltaSet{}
	add := moved.add
	add("title", before.Title, after.Title)
	add("status", string(before.Status), string(after.Status))
	add("assignee", before.Assignee, after.Assignee)
	add("priority", string(before.Priority), string(after.Priority))
	add("project", before.Project, after.Project)
	add("type", before.Type, after.Type)
	// THE TAGS IN THE ORDER THEY ARE STORED, and BOUNDED — [listText]
	// rather than a bare join. A task may carry [MaxTagsPerTask] tags
	// whose slugs run to 64 characters each, which is 2.6 KiB on each side
	// of one delta in a table nothing ever sweeps; the join had no bound
	// at all. Not [sortedText], unlike an edge set: these are in the order
	// a writer stated them, and re-ordering them here would rewrite every
	// row this column has ever held.
	add("tags", listText(before.Tags), listText(after.Tags))
	// THE SCHEDULE MOVES TOO, and until a tool could set any of these
	// nothing here could observe it: a due date, an estimate and a size
	// had no producer in the tree, so their absence from this list was
	// invisible. With one, a re-estimate wrote a history row and a
	// notification card carrying NO deltas at all — which renders as the
	// bare kind, and reads as a change that lost what it changed.
	// THE WHOLE INSTANT, which is [instantText].
	//
	// A DAY IS NOT A DELTA. Rendered as a calendar day these compared
	// equal whenever a move stayed inside one, so pulling a due time from
	// 09:00 to 17:00 produced NO entry at all: the history row and the
	// notification card carried the change's kind and nothing it changed,
	// which is the exact failure the block above this one was added to
	// end. The day form was also wrong about WHICH day for any company
	// east or west of UTC — an all-day due date is stored as the
	// company's own midnight, so truncating it in UTC moved it a day —
	// and a delta written by the APPLIER cannot consult the company's
	// zone to fix that: these rows are the state log's N identical
	// copies, and text derived from live configuration would differ
	// between two nodes at different epochs. The instant is the value
	// that is both lossless and the same everywhere; rendering a day from
	// it belongs to a surface, which knows the zone.
	add("due", instantText(before.DueAt), instantText(after.DueAt))
	add("start", instantText(before.StartAt), instantText(after.StartAt))
	add("estimate", minutesText(before.EstimateMinutes), minutesText(after.EstimateMinutes))
	add("points", pointsText(before.Points), pointsText(after.Points))
	// AND THE EDGES, ONE FIELD PER RELATION KIND, because an edge change
	// is a change to this task and the row that recorded it said only
	// `relations`. Every dependency write is a task patch — a
	// [Writer.Depend] sequence publishes `Relate` on the dependent and
	// `Depend` on the blocker, and both land here — so the applier already
	// holds both documents and the one thing missing was the comparison.
	//
	// HERE RATHER THAN AS A KIND OF ITS OWN, which is the choice this
	// makes explicit: a [ChangeKind] says what HAPPENED and a delta says
	// what MOVED, and they are different facts about one commit — the same
	// separation [Applier.writeHistory] draws between the kind and
	// `notified`. Keeping them apart is what lets `update_work_item`
	// record a status move and an edge in ONE row, which a second kind
	// could not have: one record has one kind.
	//
	// THE OTHER TASK BY ITS ID, never by its key. See `deltas.go`'s head:
	// a key belongs to another task's row, a history row is written once
	// and repaired by nothing, and a node that had not applied that task
	// would store a different string for ever. Resolving it is the
	// surface's, exactly as rendering a day is.
	//
	// ONE FIELD PER KIND, derived from [RelationKinds] rather than listed,
	// so a fifth kind of edge is recorded with no second edit — the reason
	// that slice exists.
	for _, kind := range RelationKinds {
		add(string(kind), relationText(before.Relations, kind),
			relationText(after.Relations, kind))
	}
	// AND THE MIRROR, under the word this package already uses for it:
	// `blocking` is what [DependencyChange] calls the other direction and
	// what [Reason] calls the wake it produces. It is a field of its own
	// rather than a fifth relation kind because it is not an authored edge
	// — it is the copy the blocker carries so a close can name who it
	// unblocks.
	add("blocking", sortedText(before.Dependents), sortedText(after.Dependents))
	return moved.done()
}

// relationText is one kind's counterparties, as the text a delta carries.
//
// BY KIND rather than the whole set in one field, because the kinds are
// unrelated facts: `waiting_on` is a dependency somebody has to clear and
// `page` is a link to a wiki page, and folding them into one string would make
// adding a link read as a change to what the task is waiting for.
func relationText(relations []Relation, kind RelationKind) string {
	others := make([]string, 0, len(relations))
	for _, relation := range relations {
		if relation.Kind == kind {
			others = append(others, relation.Other)
		}
	}
	return sortedText(others)
}

// excerpt is what a card shows, cut rune-safely to the display limit.
//
// MARKED, and the marker counted against the cap. A card is the whole of what
// most recipients read — the wake prompt renders it under "What changed" and a
// digest coalesces on it — so a comment cut at exactly six hundred bytes and
// handed over unmarked reads as a comment that ENDED there, which is a
// different message. [textcut.Within] rather than Ellipsis because
// [Notify.Validate] REFUSES an excerpt above MaxExcerpt: a marker outside the
// budget would turn every long comment into a failed write.
func (w Wake) excerpt() string {
	text := w.Excerpt
	if text == "" && w.Comment != nil {
		text = w.Comment.Body
	}
	if text == "" && w.Kind == ChangeCreated {
		text = w.After.Body
	}
	return textcut.Within(strings.TrimSpace(text), MaxExcerpt)
}

// without is a minus b, order preserved.
func without(all, muted []string) []string {
	if len(muted) == 0 {
		return slices.Clone(all)
	}
	out := make([]string, 0, len(all))
	for _, handle := range all {
		if !slices.Contains(muted, handle) {
			out = append(out, handle)
		}
	}
	return out
}

// The three schedule values, as the TEXT a delta carries.
//
// AN ABSENT VALUE IS THE EMPTY STRING, which every renderer of a delta already
// draws as an em-dash — so "no due date → 16 Apr" reads the way "— → jun"
// already does for an assignee.
//
// AND A ZERO IS ABSENT for an estimate and a size, because in this package it
// is the only absence those two have. [Task.StartAt] and [Task.DueAt] are
// pointers and can be nil; [Task.EstimateMinutes] and [Task.Points] are a
// plain int and a plain float64 over NOT NULL columns defaulting to 0, and
// every reader of them already spends that zero as "unsized" — a workload sums
// it and a total adds it. Rendering 0 as "0m" here would put a number on the
// one surface that disagreed with both.
//
// The obvious alternative is to make "sized at nothing" its own fact by giving
// the two fields the pointer this repo's zero-value rule calls for. It is not
// wanted: nothing downstream can act on the distinction, and it would buy a
// nullable column, a migration and a nil check in every sum to separate two
// states that mean the same thing to a person reading a card. What a fold here
// would cost — a size being CLEARED reported as no change at all — is not what
// it costs: clearing 3 points renders "3" → "", which differs and is a delta
// like any other. Only 0 → 0 folds, and that is not a change.

// minutesText is the estimate in minutes, or empty when there is none.
func minutesText(minutes int) string {
	if minutes == 0 {
		return ""
	}
	return strconv.Itoa(minutes) + "m"
}

// pointsText is the size, trimmed of a trailing zero so a half point still
// prints as one: a scale with halves in it is a scale somebody chose.
func pointsText(points float64) string {
	if points == 0 {
		return ""
	}
	return strconv.FormatFloat(points, 'f', -1, 64)
}

// instantText renders an optional instant for a delta, and an absent one as
// the empty string.
//
// A DELTA IS TEXT, because it is read by a card and by a history row rather
// than compared — so an absent date is the empty string rather than a zero
// instant that renders as the year one.
func instantText(at *time.Time) string {
	if at == nil {
		return ""
	}
	return at.UTC().Format(time.RFC3339)
}
