package tracker

import (
	"bytes"
	"encoding/json"
	"fmt"
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

	// AnswersDecision is the decision of the ask this comment answers —
	// [ResolvedThread.AnswersDecision] — carried so the card can say which
	// option the answer chose by its LABEL. The comment names the option
	// by id, and an id alone tells the asker nothing it can read at a
	// glance; the label lives on a different row, which is why the caller
	// fills it rather than this package resolving it.
	AnswersDecision *Decision

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
	notify.Answered = w.answered()
	return notify
}

// answered is what this wake's answer tells the asker about the decision it
// closed, or nil when it answers none.
func (w Wake) answered() *AnsweredDecision {
	if w.Kind != ChangeComment || w.Comment == nil || w.AnswersDecision == nil ||
		w.Comment.Answers == nil || *w.Comment.Answers == "" {
		return nil
	}
	out := &AnsweredDecision{
		Question: w.AnswersDecision.Question,
		Choice:   w.chosen(),
	}
	if inform := w.AnswersDecision.Inform; inform != nil {
		copied := *inform
		out.Inform = &copied
	}
	return out
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
	if w.Comment != nil && w.Kind == ChangeCreated {
		// A TASK FILED AS A QUESTION ([Writer.CreateTaskAsking]) asks
		// somebody in the create itself, so the person asked is woken
		// under `asked` — which outranks `assignee`, so asking the
		// assignee reads as a question rather than as new work.
		snapshot.CommentAsk = w.Comment.Ask
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
// THE WRITER HOLDS NO CATALOGUE, which is why the argument is nil here and
// why a custom-field move is HISTORY-ONLY. See [TaskDeltas].
func (w Wake) deltas() map[string]Delta { return TaskDeltas(w.Before, w.After, nil) }

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
//
// # `declared` is the ONE thing the two frames do not share
//
// A task's custom-field values are keyed by field ID, so naming them takes the
// project's catalogue — which the APPLIER holds (it reads the same
// declarations to write the value rows) and the WRITER does not: a wake is
// built by the tool from the snapshot it read, outside the write's own
// transaction. Nil therefore means "no catalogue", and the comparison is
// SKIPPED rather than keyed by uuid, because a delta reading
// `0f3c…=3 → 0f3c…=5` is worse on every surface than no delta at all.
//
// That is the whole of the split, and it is deliberately the whole: every
// OTHER field here is computed identically in both frames, because this
// function was exported precisely so two frames could not disagree about what
// moved. A field held back from the notification "because a card is one line"
// would be that disagreement re-introduced by hand — and what a card shows is
// already bounded, by [MaxDeltas].
func TaskDeltas(before, after Task, declared map[string]FieldDef) map[string]Delta {
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
	// AND WHETHER THAT INSTANT IS A DAY, which is the one thing about a due
	// date the instant cannot say. An all-day date is stored as the
	// company's own midnight, so making a midnight due date all-day — or
	// giving an all-day one a time of 00:00 — moves the flag and leaves the
	// instant exactly where it was: without this the commonest way to reach
	// that flag is a row saying the schedule changed and naming nothing.
	// [boolText] rather than an absence, for `archived`'s reason: both
	// states are present on every task.
	add("due_all_day", boolText(before.DueAllDay), boolText(after.DueAllDay))
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
	// AND THE REST OF WHAT A WRITER CAN MOVE, which recorded nothing at
	// all. Every field below had a producer and no comparison, so the
	// commit that changed it wrote a history row and a card carrying its
	// KIND and nothing it changed — a `watchers` row that did not say who,
	// an `archived` row that did not say which way, a `reparented` row that
	// named neither parent. The kind is what HAPPENED and a delta is what
	// MOVED, and a row with only the first is the exact failure the edge
	// block above was added to end.
	//
	// WHAT IS STILL LEFT OUT IS WHAT A WRITER CANNOT MOVE, and it is left
	// out because a delta describes a WRITE: a version, a log position and
	// the instants are the row's own columns, a key and a rank have exactly
	// two producers and neither is a patch ([TaskPatch.Mint] says so), a
	// depth is a hint the applier re-derives from the closure, and a turn's
	// spend writes no history row at all. A field that gains a patch field
	// later belongs in the list below, for the reason the schedule block
	// above gives: an absence nothing can produce is an absence nobody
	// notices.
	//
	// THE THREE PEOPLE SETS ARE SETS, so they take [sortedText] like the
	// edges and unlike `tags`: nothing orders them, [settleWatch] rebuilds
	// the watcher list by removing a handle and appending it, and a caller
	// may state a whole set it read in any order — so a delta over document
	// order would record a change on every re-statement that moved nobody.
	//
	// AND `muted` IS ITS OWN FIELD RATHER THAN SUBTRACTED FROM `watchers`.
	// [Wake.snapshot] subtracts because it is building a ROUTING list; a
	// delta records what the DOCUMENT holds, and folding the two would make
	// "was never watching" and "chose to stop" the same row — which is the
	// distinction those two fields exist to keep.
	add("reporter", before.Reporter, after.Reporter)
	add("watchers", sortedText(before.Watchers), sortedText(after.Watchers))
	add("muted", sortedText(before.Muted), sortedText(after.Muted))
	add("collaborators",
		sortedText(before.Collaborators), sortedText(after.Collaborators))
	// THE PARENT BY ITS ID, for the reason every relation is by one: a key
	// is a fact about another task's row and this row is repaired by
	// nothing. The activity read resolves it against the rows this node
	// holds at the instant it answers — see `deltas.go`'s head.
	add("parent", scalarText(parentText(before.Parent)),
		scalarText(parentText(after.Parent)))
	// THE ROUTING UNIT AND NOT THE FILED ONE. [Task.FiledUnit] is
	// immutable, so the only commit that could ever move it is the create,
	// where it says exactly what `routing_unit` already says — one more key
	// on every new task's row, carrying nothing a reader did not have.
	add("routing_unit",
		scalarText(before.RoutingUnit), scalarText(after.RoutingUnit))
	// BOTH STATES PRESENT, which is [boolText]'s rule: "archived: — → true"
	// would read as a field that had no value before, and every task has
	// always had this one.
	add("archived", boolText(before.Archived), boolText(after.Archived))
	// AND WHICH ROOT A CASCADE TOOK THIS TASK WITH — the one thing a
	// tombstone holds that the history row does not already carry.
	//
	// The other three members of a [Tombstone] are `By`, `Kind` and `At`,
	// and those ARE the row's own `actor`, `actor_kind` and `created_at`;
	// recording them would be one commit's facts written twice, in two
	// columns a later reader can find disagreeing. `RemovedWith` has no
	// second home, and without it a descendant's `removed` row and a task
	// somebody removed on purpose are the same three words — "removed by
	// ada" — meaning two different things, with nothing on either row to
	// tell them apart.
	add("removed_with", scalarText(removedWithText(before.Removed)),
		scalarText(removedWithText(after.Removed)))
	add("checklists", checklistText(before.Checklists), checklistText(after.Checklists))
	if before.Body != after.Body {
		// A MARKER AND NEVER THE PROSE. A body is [MaxBody] — 32 KiB —
		// and `tracker_history` is never swept, so carrying both sides
		// would put 64 KiB on one log line on every node for the life
		// of the company; the mutation itself is already on this row's
		// `document` column for anybody who needs the text.
		// [deltaSet.mark] rather than `add` because the two markers read
		// the same when an edit replaced one paragraph with another of
		// the same length, and that is a change.
		moved.mark("body", bodyText(before.Body), bodyText(after.Body))
	}
	if from, to, changed := fieldsText(before.Fields, after.Fields, declared); changed {
		// THE CUSTOM VALUES, BY SLUG. [deltaSet.mark] for the second of
		// that method's two reasons: each value is cut to
		// [MaxDeltaElement], so two long values differing past the cut
		// render the same although the field moved.
		moved.mark("fields", from, to)
	}
	return moved.done()
}

// parentText is a task's parent, and the empty string when it is a root.
//
// THE EMPTY STRING RATHER THAN A WORD, so "— → 0f3c…" reads the way every
// other absent value on a delta already does.
func parentText(parent *string) string {
	if parent == nil {
		return ""
	}
	return *parent
}

// removedWithText is the root a cascade removed this task with, by its ID.
//
// BY ID for the reason the parent and every relation are: a key is a fact
// about another task's row. Empty both for a live task and for one removed on
// its own, which is the honest fold — neither went with anything.
//
// [removedWith], which binds the same fact into the task's own column, is
// derived FROM this one, so the column and the delta cannot name two different
// roots.
func removedWithText(tomb *Tombstone) string {
	if tomb == nil || tomb.RemovedWith == nil {
		return ""
	}
	return *tomb.RemovedWith
}

// bodyText is how big a body is, and never a byte of it.
//
// BYTES RATHER THAN RUNES, because this is a statement about what the write
// COST — the figure [MaxBody] is declared in, and the one a reader comparing
// it against that cap needs. An absent body is the empty string, which every
// renderer of a delta already draws as an em dash, so "— → 1204 bytes" is a
// description written and "980 bytes → —" is one cleared: the two states the
// marker has to tell apart, in the spelling this package uses for absence
// everywhere else.
func bodyText(body string) string {
	if body == "" {
		return ""
	}
	return strconv.Itoa(len(body)) + " bytes"
}

// checklistText is a task's checklists as COUNTS PER NAMED LIST.
//
// COUNTS BECAUSE THE ITEMS DO NOT FIT: a task carries up to [MaxChecklists]
// lists holding [MaxChecklistItemsTotal] items between them, each with its own
// text, so listing them would put a copy of the whole tree on both sides of
// one log line in a table nothing sweeps. "3 of 5 done" is what a reader of a
// change wants from a checklist, and the item-level detail is on this row's
// `document` column for anybody who needs it.
//
// A LIST THAT ARRIVED OR LEFT IS NAMED BY BEING THERE — each side carries the
// lists that state held, so an addition appears on the `to` side alone and a
// removal on the `from` side alone, which is [paramsText]'s shape for the same
// reason.
//
// PROMOTIONS ARE COUNTED SEPARATELY, and that is not decoration: promoting an
// item to a subtask is a `checklist` commit ([markPromoted]) that moves
// neither the done count nor the total, so counts alone would have left the
// one checklist gesture this build actually has recording nothing.
//
// IN DOCUMENT ORDER, unlike the people sets above and like a person's
// `priorities`: a checklist collection renders in the order it is stored, so
// that order is something somebody arranged rather than an artefact of how a
// set was assembled.
func checklistText(lists []Checklist) string {
	if len(lists) == 0 {
		return ""
	}
	out := make([]string, 0, len(lists))
	for _, list := range lists {
		done, promoted := 0, 0
		for _, item := range list.Items {
			if item.Done {
				done++
			}
			if item.PromotedTo != nil {
				promoted++
			}
		}
		entry := fmt.Sprintf("%s: %d of %d done", checklistName(list), done,
			len(list.Items))
		if promoted > 0 {
			entry += fmt.Sprintf(" (%d promoted)", promoted)
		}
		out = append(out, entry)
	}
	return listText(out)
}

// checklistName is what a list is called, falling back to its id.
//
// THE ID IS ON THIS TASK'S OWN DOCUMENT, so quoting it breaks none of the
// rules a task KEY would: it is not a fact about another row. A nameless list
// rendering as ": 2 of 3 done" would be a line nobody can attach to anything.
func checklistName(list Checklist) string {
	if name := strings.TrimSpace(list.Name); name != "" {
		return name
	}
	return list.ID
}

// fieldsText is the custom-field values that MOVED, as `slug=value` on each
// side, and whether any moved at all.
//
// [paramsText]'S SHAPE, for [paramsText]'s reason: a task may carry
// [MaxFieldValues] values of up to [MaxFieldValueBytes] each, so carrying the
// whole map twice would put hundreds of kilobytes into a log line to show that
// one number changed. Only the ids whose stored bytes differ appear, and both
// sides of each — so an addition, a clearing and a re-pointing are three
// different pictures.
//
// BY SLUG, WHICH TAKES THE CATALOGUE. A value is keyed by field ID precisely
// so a rename never re-points it, and an id is a uuid: `declared` is what
// turns it back into the word a person typed. A nil catalogue is the WRITER's
// frame and yields nothing at all — see [TaskDeltas].
//
// A FIELD THIS PROJECT DOES NOT DECLARE IS COUNTED RATHER THAN NAMED. There is
// no slug to name it with, its value is already outside every filter (see
// `fieldvalues.go`'s foreign rows), and dropping it in silence would make a
// commit that moved only such a value read as a commit that moved nothing.
//
// THE `changed` FLAG IS THE VALUES' OWN ANSWER, not the text's: the members
// are cut at [MaxDeltaElement], so two long values differing past the cut
// render the same and only the comparison above can tell them apart.
func fieldsText(before, after map[string]json.RawMessage,
	declared map[string]FieldDef) (string, string, bool) {

	if declared == nil {
		return "", "", false
	}
	ids := make([]string, 0, len(before)+len(after))
	for id, was := range before {
		if !bytes.Equal(after[id], was) {
			ids = append(ids, id)
		}
	}
	for id, now := range after {
		if _, had := before[id]; !had && len(now) > 0 {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return "", "", false
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)

	type move struct{ slug, from, to string }
	named := make([]move, 0, len(ids))
	var wasUndeclared, isUndeclared int
	for _, id := range ids {
		field, known := declared[id]
		if !known {
			if len(before[id]) > 0 {
				wasUndeclared++
			}
			if len(after[id]) > 0 {
				isUndeclared++
			}
			continue
		}
		named = append(named, move{
			slug: field.Slug,
			from: fieldValueText(field, before[id]),
			to:   fieldValueText(field, after[id]),
		})
	}
	// ORDERED BY SLUG, because a map has no order and two nodes must write
	// one string — and the two sides keep that one order, so a reader can
	// line them up member for member.
	sort.Slice(named, func(i, j int) bool { return named[i].slug < named[j].slug })
	from := make([]string, 0, len(named)+1)
	to := make([]string, 0, len(named)+1)
	for _, m := range named {
		if m.from != "" {
			from = append(from, m.slug+"="+m.from)
		}
		if m.to != "" {
			to = append(to, m.slug+"="+m.to)
		}
	}
	if wasUndeclared > 0 {
		from = append(from, countText(wasUndeclared)+" undeclared")
	}
	if isUndeclared > 0 {
		to = append(to, countText(isUndeclared)+" undeclared")
	}
	return listText(from), listText(to), true
}

// fieldValueText is one stored custom-field value as the text a delta carries.
//
// THE OPTION'S SLUG FOR A CHOICE, which is the same decision `fields` itself
// takes one level up: [coerceOption] stores the option's ID so a rename never
// re-points a stored value, and the declaration in hand is what turns it back
// into the word somebody chose. Left unresolved, the commonest custom field
// there is would put a uuid on both sides of every change to it.
//
// A RELATIONSHIP KEEPS ITS TASK ID, which is the opposite answer for the
// opposite reason: the option list is on the declaration this frame holds,
// while the other task is another ROW, which `deltas.go`'s head forbids
// quoting by key.
//
// EACH MEMBER OF A MULTI-VALUED FIELD IS BOUNDED AS IT IS BUILT. A field may
// carry [MaxFieldValueSeq] members of up to [MaxTextareaBytes] each, and the
// join would otherwise assemble megabytes to be cut to [MaxDeltaElement] by
// the caller. "/" separates them because ", " already separates one field from
// the next.
func fieldValueText(field FieldDef, raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var members []json.RawMessage
	if err := json.Unmarshal(raw, &members); err == nil {
		out := make([]string, 0, len(members))
		for _, member := range members {
			out = append(out, textcut.Within(
				fieldValueText(field, member), MaxDeltaElement))
		}
		return strings.Join(out, "/")
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		// A NUMBER, A BOOLEAN OR SOMETHING THIS BUILD DOES NOT KNOW: its
		// own JSON, which is what the document holds and what somebody
		// typed. Guessing a shape for it is how `"3"` and `3` stop
		// comparing.
		return string(raw)
	}
	if field.Type == FieldDropdown || field.Type == FieldLabels {
		for _, option := range field.Config.Options {
			if option.ID == text {
				return option.Slug
			}
		}
	}
	return text
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
//
// AN ANSWER THAT CHOSE LEADS WITH THE CHOICE — `Chose “Ship Friday”: <body>` —
// because the choice is the answer and the body is its reason: cut at the cap,
// a card that led with the body could lose the one thing the asker was waiting
// for.
func (w Wake) excerpt() string {
	text := w.Excerpt
	if text == "" && w.Kind == ChangeCreated {
		// THE BODY BEFORE THE ASK on a create: a task filed as a
		// question carries its question as the title — which the card
		// already shows — and its detail as the body.
		text = w.After.Body
	}
	if text == "" && w.Comment != nil {
		text = w.Comment.Body
		if option := w.chosen(); option != nil {
			text = choiceExcerpt(option.Label, strings.TrimSpace(text))
		}
	}
	return textcut.Within(strings.TrimSpace(text), MaxExcerpt)
}

// chosen is the option this wake's comment chose, when its ask carried one.
func (w Wake) chosen() *DecisionOption {
	if w.Comment == nil || w.Comment.Choice == "" || w.AnswersDecision == nil {
		return nil
	}
	option, ok := w.AnswersDecision.Option(w.Comment.Choice)
	if !ok {
		return nil
	}
	return &option
}

// choiceExcerpt is the card line an answer that chose is summed up as.
func choiceExcerpt(label, body string) string {
	if body == "" {
		return "Chose “" + label + "”"
	}
	return "Chose “" + label + "”: " + body
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
