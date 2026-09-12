package tracker

import (
	"slices"
	"sort"
	"strings"

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
// every quiet commit also writes, and which the status spans are rebuilt
// from. While this was a method on [Wake] the applier had no way to reach it,
// so a quiet status change wrote a history row with NO deltas and produced NO
// span, and every report derived from the spans — burndown, cumulative flow,
// cycle time, a sprint's `done` and therefore velocity — silently omitted it.
func TaskDeltas(before, after Task) map[string]Delta {
	moved := map[string]Delta{}
	add := func(field, from, to string) {
		if from != to {
			moved[field] = Delta{From: from, To: to}
		}
	}
	add("title", before.Title, after.Title)
	add("status", string(before.Status), string(after.Status))
	add("assignee", before.Assignee, after.Assignee)
	add("priority", string(before.Priority), string(after.Priority))
	add("project", before.Project, after.Project)
	add("type", before.Type, after.Type)
	add("tags", strings.Join(before.Tags, ", "), strings.Join(after.Tags, ", "))
	if len(moved) == 0 {
		return nil
	}
	if len(moved) > MaxDeltas {
		// DETERMINISTICALLY, by field name: a card that showed a
		// different thirty-two on two nodes would be one screen
		// disagreeing with another about what changed.
		names := make([]string, 0, len(moved))
		for name := range moved {
			names = append(names, name)
		}
		sort.Strings(names)
		trimmed := make(map[string]Delta, MaxDeltas)
		for _, name := range names[:MaxDeltas] {
			trimmed[name] = moved[name]
		}
		return trimmed
	}
	return moved
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
