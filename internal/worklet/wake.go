package worklet

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/textcut"
)

// WHAT A CHANGE TELLS PEOPLE is decided at the WRITE and carried on the record.
//
// A wake carries the task's routing state COPIED at the change (assignee,
// watchers, project lead, previous assignee and status group) rather than a
// task id to look up: the node winning the feed's delivery may be behind on
// the task, and would route from a row that has moved on or block the feed
// until it caught up. The previous values matter most: routing turns on the
// TRANSITION ("entered or left a finished group" wakes a reporter and a
// parent's assignee, "reassigned away from" tells the one who lost it), which
// a row already holding the new value cannot reconstruct.

// Leads resolves the two fallbacks a wake may need. Both are carried, because
// which applies depends on whether the task has a routing unit at the moment
// of the change, which the receiving node cannot reconstruct.
type Leads interface {
	// ProjectLead is who hears about unassigned work in a project.
	ProjectLead(project string) string

	// UnitLead is who hears about work routed to a unit.
	UnitLead(unit string) string
}

// Wake is what a write says about itself, before it is published. The caller
// states the kind; the rest is computed from the task's two states.
type Wake struct {
	// Kind is what happened, as a recipient needs to be told it.
	Kind ChangeKind

	// Before is the task as it stood, and After as it will stand. Before is
	// the zero value on a create, so every delta there has no previous value.
	Before, After Task

	// Comment is the comment this change carried, if any.
	Comment *Comment

	// Mentions are the handles the text addressed, resolved by the caller:
	// resolved at READ time, a mention names whoever holds the role later.
	Mentions []string

	// Asks is an agent author's statement of whether the comment asks the
	// seats it names, and MentionedAgents those that are agents
	// ([Snapshot.CommentAsks], [Snapshot.MentionedAgents]). Nil for a person.
	Asks            *bool
	MentionedAgents []string

	// Excerpt overrides what a card shows. Empty derives one from the
	// comment, or from the body on a create.
	Excerpt string

	// AnswersDecision is the decision of the ask this comment answers
	// ([ResolvedThread.AnswersDecision]), so the card names the chosen
	// option by its LABEL. The comment names it only by id and the label
	// lives on another row, so the caller fills it.
	AnswersDecision *decision.Decision

	// Dependents and Thread are filled by the caller, like Mentions, because
	// each is about a DIFFERENT row, which the node routing the wake may
	// never have applied. The third such fact, the parent's assignee, the
	// write resolves in its decide's own snapshot ([Notify.restated]), so no
	// caller can forget it. A field nothing fills routes to nobody without
	// looking wrong: [Candidates] reads the three for `blocking`,
	// `parent_assignee`, `asked`, `answered` and `thread`, and drops an
	// empty handle.

	// Dependents names the tasks a MIRROR commit added to this blocker, each
	// with its key and assignee. Set only on the blocker's side of a
	// dependency and on the duty's repair of one: a new dependent is the
	// blocker's assignee's to weigh.
	Dependents []TaskParty

	// Thread is the comment conversation this change joins: who has
	// already spoken, who asked, and who is being answered. Empty on
	// every change that is not a comment.
	Thread ThreadParties
}

// ThreadParties is what a comment's routing needs to know about the
// conversation it lands in. The caller resolves it from the thread rows:
// [Candidates] is pure over the notification, and the routing node rarely
// holds the thread.
type ThreadParties struct {
	// Participants are the handles already in this thread: the parent
	// comment's author and every replier. The caller's query caps them at
	// [MaxThreadParticipants]; this package cuts longer input
	// deterministically, so two nodes reading one record agree.
	Participants []string

	// Asked is the handle THIS comment asks, which is a question somebody
	// now owes an answer to.
	Asked string

	// Owing are the handles an open ask in this thread is addressed to, and
	// Awaiting the authors of those asks, each capped like the participants.
	Owing    []string
	Awaiting []string

	// AnsweredAuthor is the author of the comment this one ANSWERS: the
	// answering comment's own author is the answerer, so routing off it
	// would wake the seat that replied and not the asker.
	AnsweredAuthor string
}

// Notify builds the record's routing snapshot, or nil when this change wakes
// nobody. Nil is the only way to say "quiet", so quiet is the record's shape
// rather than a flag a writer can forget; a quiet commit is still a full
// record, arbitrated and history-written like a loud one.
func (w Wake) Notify(leads Leads) *Notify {
	if !w.Kind.Valid() {
		return nil
	}
	// The caller's frame holds no catalogue ([TaskDeltas]), so custom values
	// go unnamed here; an update's decide restates the card with them
	// ([Notify.restated]).
	fields, omitted := cardFields(TaskDeltas(w.Before, w.After, nil))
	notify := &Notify{
		Kind:          w.Kind,
		Mentions:      w.Mentions,
		Fields:        fields,
		FieldsOmitted: omitted,
		Excerpt:       w.excerpt(),
		Snapshot:      w.snapshot(leads),
	}
	if w.Comment != nil {
		notify.CommentID = w.Comment.ID
	}
	notify.Answered = w.answered()
	return notify
}

// settled brings a create's wake (handed: the task as handed in) up to what
// the writer settled after the caller built it (filed: the task the record
// carries): normalised tags and default type ([Writer.CreateTask]), an ask's
// addressee as watcher ([checkCreateAsk]), the filed unit ([filedUnit]), the
// default assignee and its watch ([Writer.landsOn]), the starting status and
// priority. ONE completion once all are settled, or the default assignee and
// unit lead go unwoken and the card disagrees with the history row. When the
// writer moved anything, the card is recomputed whole from [TaskDeltas] over
// nothing and the filed task, as the applier's history row is. The snapshot
// changes only where the writer moved it: a unit the caller named keeps the
// lead its own chart resolved, a settled one takes [Writer.Leads]'s.
func (n *Notify) settled(handed, filed Task, leads Leads) {
	if n == nil {
		return
	}
	if len(TaskDeltas(handed, filed, nil)) > 0 {
		n.Fields, n.FieldsOmitted = cardFields(TaskDeltas(Task{}, filed, nil))
	}
	if filed.Assignee != handed.Assignee {
		n.Snapshot.Assignee = filed.Assignee
	}
	if !slices.Equal(filed.Watchers, handed.Watchers) || !slices.Equal(filed.Muted, handed.Muted) {
		n.Snapshot.Watchers = without(filed.Watchers, filed.Muted)
	}
	if filed.Status != handed.Status || filed.StatusGroup != handed.StatusGroup {
		n.Snapshot.Status, n.Snapshot.StatusGroup = filed.Status, filed.StatusGroup
	}
	if filed.RoutingUnit != handed.RoutingUnit && filed.RoutingUnit != "" && leads != nil {
		n.Snapshot.RoutingUnitLead = leads.UnitLead(filed.RoutingUnit)
	}
}

// keyed names, on a create's wake, the key the write minted. The caller builds
// the wake before the write takes the project's next counter value, and
// [Snapshot.Key] is the task's addressable name to everything the record wakes:
// the notice's `subject_key`, the woken seat's `item_key` and the prompt's
// "Read **<key>**" line. Without it a woken turn is charged to a bare uuid and
// its prompt has no key to read the task by.
func (n *Notify) keyed(key string) {
	if n == nil || key == "" {
		return
	}
	n.Snapshot.Key = key
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
		// Minus the muted, so the feed never has to subtract. The mute
		// itself travels in the mutation payload when watchers are touched.
		Watchers:        without(after.Watchers, after.Muted),
		Collaborators:   slices.Clone(after.Collaborators),
		PrevAssignee:    w.Before.Assignee,
		PrevStatusGroup: w.Before.StatusGroup,
	}
	if w.Comment != nil {
		// The author's kind decides whether the assignee is addressed: a
		// turn that answers rather than absorbs ([Notify.ownerAsked]).
		// Without it a person's question to an agent reads as activity.
		snapshot.CommentAuthorKind = w.Comment.AuthorKind
		snapshot.CommentAsks = w.Asks
		snapshot.MentionedAgents = slices.Clone(w.MentionedAgents)
	}
	if leads != nil {
		snapshot.ProjectLead = leads.ProjectLead(after.Project)
		if after.RoutingUnit != "" {
			snapshot.RoutingUnitLead = leads.UnitLead(after.RoutingUnit)
		}
	}
	if w.Kind != ChangeCreated {
		// AN UPDATE'S TRANSITIONS, whatever kind its record ends with,
		// and never a creation's, whose before is nothing.
		snapshot.moves(w.Before, after, leads)
	}
	if len(w.Dependents) > 0 {
		snapshot.Dependents = capParties(w.Dependents, MaxDependents)
	}
	if w.Comment != nil && w.Kind == ChangeCreated {
		// A TASK FILED AS A QUESTION ([Writer.CreateTaskAsking]) asks
		// somebody in the create itself, so the person asked is woken
		// under `asked` — which outranks `assignee`, so asking the
		// assignee reads as a question rather than as new work.
		snapshot.CommentAsk = w.Comment.Ask
	}
	if w.Comment != nil && w.Kind == ChangeComment {
		// The creation kind alone: a comment becomes a question when
		// WRITTEN ([Comment.Ask]) and an edit re-sends the whole comment,
		// so filling this on `comment_edited` would re-wake the asked
		// party for a typo fix, and the asker of a long-answered question.
		snapshot.CommentAsk = w.Thread.Asked
		snapshot.AnsweredAuthor = w.Thread.AnsweredAuthor
		snapshot.ThreadParticipants = capHandles(
			w.Thread.Participants, MaxThreadParticipants)
		// CAPPED AND DEDUPED BY THE READ ([Reader.Thread]); Validate refuses more.
		snapshot.ThreadOwing = slices.Clone(w.Thread.Owing)
		snapshot.ThreadAwaiting = slices.Clone(w.Thread.Awaiting)
	}
	return snapshot
}

// checklistAssignees is everybody whose checklist item this change added,
// removed or touched ([itemTouched]). By item ID, not position: a positional
// diff reports the whole list changed after one drag. Sorted and deduped,
// because nodes read the record rather than re-derive it, and map order would
// store two spellings of one notification.
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
		case itemTouched(previous, item):
			// Both owners: a reassigned item is news to who had it too.
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

// itemTouched reports whether an item changed in a way its assignee would care
// about: its text, its done flag, or who holds it. Named fields, not struct
// `!=`: `Parent` is a pointer, so two decodings of one unchanged item compare
// unequal, and `Order` and `Parent` are layout, so a drag would wake everybody
// on the list.
func itemTouched(before, after ChecklistItem) bool {
	return before.Name != after.Name || before.Done != after.Done ||
		before.Assignee != after.Assignee
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

// capHandles cuts a handle set to its cap, deduped, order preserved. Cut
// rather than refused, unlike a collection a caller states: these derive from
// rows, so an oversize set is a task that grew, and refusing would fail a
// comment because its thread has many voices. Nobody is silenced: a
// participant past the cap is still a watcher.
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

// moves fills in the transitions a write made from before to after: watchers
// dropped, the lead of a unit the task moved to, holders of checklist items
// touched. Each is filled only where its transition happened and emptied
// otherwise, so a filled field IS the transition, and [Candidates] reads it
// whatever kind the record ends with (a hand-off written with a status still
// drops a watcher). Never asked of a creation, whose before is nothing.
func (s *Snapshot) moves(before, after Task, leads Leads) {
	s.RemovedWatchers = nil
	if removed := without(before.Watchers, after.Watchers); len(removed) > 0 {
		// [Candidates] writes these to the INBOX rather than routing them:
		// a human learns that a lead removed her watch.
		s.RemovedWatchers = removed
	}
	s.RoutedTo = ""
	if after.RoutingUnit != before.RoutingUnit && after.RoutingUnit != "" && leads != nil {
		// The new unit's lead is an ORDINARY candidate, hence this field
		// beside RoutingUnitLead: a fallback survives only when no ordinary
		// candidate does, so on a task with an assignee, collaborator or
		// watcher the new lead would be dropped. Gated on the unit moving,
		// so a walk re-stamping a thousand tasks with the unit they are
		// already in does not wake one lead a thousand times.
		s.RoutingUnitLead = leads.UnitLead(after.RoutingUnit)
		s.RoutedTo = s.RoutingUnitLead
	}
	s.ChecklistAssignees = nil
	if lists := checklistAssignees(before, after); len(lists) > 0 {
		s.ChecklistAssignees = lists
	}
}

// restated is an update's wake as its decide publishes it: the caller's, with
// what the write moved restated from the decide's two states (before, this
// snapshot's row; after, the row this record leaves) rather than from the task
// the caller read outside it. The card is the update's history row, as a
// create's is ([Notify.settled]): [TaskDeltas] with the catalogue (declared),
// so a link resolved here, a custom value and a status's group are stated, and
// another writer's later change is not claimed; the router routes by these
// deltas ([Notify.moved]). Transitions only where they happened: assignee,
// status and group, [Snapshot.moves], and across the finished edge the
// parent's assignee (parent), read in the same snapshot. The rest is routing
// state the caller copied, a repair's deliberately narrow recipient among it.
func (n *Notify) restated(before, after Task, declared map[string]FieldDef,
	leads Leads, parent string) *Notify {

	if n == nil {
		return nil
	}
	out := *n
	out.Fields, out.FieldsOmitted = cardFields(TaskDeltas(before, after, declared))
	s := &out.Snapshot
	if before.Assignee != after.Assignee {
		s.PrevAssignee, s.Assignee = before.Assignee, after.Assignee
	}
	if before.Status != after.Status {
		s.Status, s.StatusGroup, s.PrevStatusGroup =
			after.Status, after.StatusGroup, before.StatusGroup
	}
	s.moves(before, after, leads)
	s.ParentAssignee = ""
	if crossesFinished(before, after) {
		s.ParentAssignee = parent
	}
	return &out
}

// crossesFinished reports a move of a task's status into or out of a finished
// group: the one transition a reporter and a parent's assignee are told of.
// [Notify.crossesFinished] reads the same transition off a record: its status
// delta beside the two groups its snapshot copies.
func crossesFinished(before, after Task) bool {
	return before.StatusGroup.Finished() != after.StatusGroup.Finished()
}

// TaskDeltas is what changed between two versions of a task.
//
// Exported and shared so the WRITER (the notification card) and the APPLIER
// (the history row, which every quiet commit writes and `status_entered_at`
// derives from) cannot disagree about what moved. It is the task's half of one
// rule; read `deltas.go`'s head, which states the bounds, the text forms and
// why a value is never a rendering, before adding a field.
//
// `declared`, the project's custom-field catalogue, is the one input the two
// frames do not share: the applier holds it, a writer's wake does not. Nil
// skips custom values rather than keying them by uuid. Every other field is
// computed identically, none held back from a card: [MaxDeltas] bounds it.
func TaskDeltas(before, after Task, declared map[string]FieldDef) map[string]Delta {
	moved := deltaSet{}
	add := moved.add
	add("title", before.Title, after.Title)
	add("status", string(before.Status), string(after.Status))
	add("assignee", before.Assignee, after.Assignee)
	add("priority", string(before.Priority), string(after.Priority))
	add("project", before.Project, after.Project)
	add("type", before.Type, after.Type)
	// The tags as a set ([deltaSet.set]): what this commit added and
	// removed, whole slugs. A re-ordering records nothing.
	moved.set("tags", before.Tags, after.Tags)
	// The schedule as the WHOLE INSTANT ([instantText]), never a day: a day
	// drops a move within one (09:00 to 17:00) and, truncated in UTC,
	// shifts an all-day date stored as the company's midnight. The applier
	// cannot use the company's zone: these rows are the state log's
	// identical copies, and live config differs between nodes at different
	// epochs. Rendering a day belongs to a surface, which knows the zone.
	add("due", instantText(before.DueAt), instantText(after.DueAt))
	// And whether that instant is a day, which it cannot say: making a
	// midnight due date all-day moves only this flag. [boolText], for
	// `archived`'s reason: both states are present on every task.
	add("due_all_day", boolText(before.DueAllDay), boolText(after.DueAllDay))
	add("start", instantText(before.StartAt), instantText(after.StartAt))
	// And why it waits: restating the date with another reason is a write
	// of its own, which would otherwise move nothing a reader could see.
	moved.prose("wait_reason", before.WaitReason, after.WaitReason)
	add("estimate", minutesText(before.EstimateMinutes), minutesText(after.EstimateMinutes))
	add("points", pointsText(before.Points), pointsText(after.Points))
	// The edges, one field per relation kind, each a set (what joined, what
	// left). Every dependency write is a task patch ([Writer.Depend]
	// publishes `Relate` on the dependent and `Depend` on the blocker), so
	// the applier holds both documents. A delta, not a [ChangeKind]: a kind
	// says what HAPPENED and a delta what MOVED, so `update_work_item` can
	// record a status move and an edge in ONE row ([Applier.writeHistory]
	// draws the same line). The other task by its ID, never its key
	// (`deltas.go`'s head). Kinds come from [RelationKinds], so a new kind
	// is recorded with no second edit.
	for _, kind := range RelationKinds {
		was, now := relationMembers(before.Relations, kind), relationMembers(after.Relations, kind)
		if kind == RelationEntry {
			// HOW MANY, NEVER WHICH: an entry's id is derived from its key,
			// and a history row is read by people who may not read the atlas.
			moved.count(string(kind), was, now)
			continue
		}
		moved.set(string(kind), was, now)
	}
	// The mirror, under the name [DependencyChange] and [Reason] use. Its
	// own field rather than a relation kind: not an authored edge, but the
	// copy a blocker carries so a close can name who it unblocks.
	moved.set("blocking", before.Dependents, after.Dependents)
	// The rest of what a writer can move, so no row carries its kind and
	// nothing it changed. Left out is what no patch moves: version, log
	// position and instants are the row's own columns, key and rank have
	// two producers and neither is a patch ([TaskPatch.Mint]), depth is
	// re-derived from the closure, and spend writes no history. A field
	// that gains a patch field belongs here.
	//
	// The people sets are sets: [settleWatch] rebuilds watchers by remove
	// and append, and callers restate a set in any order. `muted` is its
	// own field, not subtracted as [Wake.snapshot]'s routing list is: a
	// delta records the document, where "never watching" and "chose to
	// stop" differ.
	add("reporter", before.Reporter, after.Reporter)
	moved.set("watchers", before.Watchers, after.Watchers)
	moved.set("muted", before.Muted, after.Muted)
	moved.set("collaborators", before.Collaborators, after.Collaborators)
	// The parent by its ID, as every relation: the activity read resolves
	// it against this node's rows when it answers (`deltas.go`'s head).
	add("parent", parentText(before.Parent), parentText(after.Parent))
	// The routing unit, not the filed one: [Task.FiledUnit] is immutable,
	// so only a create moves it, where `routing_unit` already says it.
	add("routing_unit", before.RoutingUnit, after.RoutingUnit)
	// Both states present ([boolText]): every task has always had this one.
	add("archived", boolText(before.Archived), boolText(after.Archived))
	// And which root a cascade took this task with. A [Tombstone]'s other
	// members (`By`, `Kind`, `At`) are the row's own `actor`, `actor_kind`
	// and `created_at`; without `RemovedWith` a descendant's `removed` row
	// reads like a deliberate removal.
	add("removed_with", removedWithText(before.Removed), removedWithText(after.Removed))
	add("checklists", checklistText(before.Checklists), checklistText(after.Checklists))
	if before.Body != after.Body {
		// A marker, never the prose: a body is up to [MaxBody] (32 KiB),
		// `tracker_history` is never swept, and the text is on this row's
		// `document` column. [deltaSet.mark] rather than `add`: two bodies
		// of one length give equal markers and are still a change.
		moved.mark("body", bodyText(before.Body), bodyText(after.Body))
	}
	if from, to, changed := fieldsText(before.Fields, after.Fields, declared); changed {
		// By slug. [deltaSet.mark] because a value past [MaxDeltaElement]
		// is carried as its size, so two long values can render alike.
		moved.mark("fields", from, to)
	}
	return moved.done()
}

// parentText is a task's parent, and the empty string when it is a root, so
// "— → 0f3c…" reads like every other absent value on a delta.
func parentText(parent *string) string {
	if parent == nil {
		return ""
	}
	return *parent
}

// removedWithText is the root a cascade removed this task with, by its ID (a
// key is a fact about another task's row). Empty for a live task and for one
// removed on its own: neither went with anything. [removedWith], the task's own
// column, derives from this, so the two cannot name different roots.
func removedWithText(tomb *Tombstone) string {
	if tomb == nil || tomb.RemovedWith == nil {
		return ""
	}
	return *tomb.RemovedWith
}

// bodyText is how big a body is, and never a byte of it. Bytes, not runes: the
// unit [MaxBody] is declared in. An absent body is the empty string, drawn as
// an em dash, so "— → 1204 bytes" is a body written and "980 bytes → —" one
// cleared.
func bodyText(body string) string {
	if body == "" {
		return ""
	}
	return strconv.Itoa(len(body)) + " bytes"
}

// checklistText is a task's checklists as COUNTS PER NAMED LIST: the items (up
// to [MaxChecklistItemsTotal]) would copy the whole tree onto both sides of a
// log line nothing sweeps, and the detail is on this row's `document` column.
// A list added or removed appears on one side only, as in [paramsText]. In
// document order, unlike the people sets: somebody arranged it. Whole on both
// sides: at most [MaxChecklists] lists, each named in [MaxChecklistName] bytes.
func checklistText(lists []Checklist) string {
	if len(lists) == 0 {
		return ""
	}
	out := make([]string, 0, len(lists))
	for _, list := range lists {
		done := 0
		for _, item := range list.Items {
			if item.Done {
				done++
			}
		}
		out = append(out, fmt.Sprintf("%s: %d of %d done", checklistName(list),
			done, len(list.Items)))
	}
	return listText(out)
}

// checklistName is what a list is called, falling back to its id: the id is on
// this task's own document, so quoting it breaks none of a task key's rules,
// and ": 2 of 3 done" would attach to nothing.
func checklistName(list Checklist) string {
	if name := strings.TrimSpace(list.Name); name != "" {
		return name
	}
	return list.ID
}

// fieldsText is the custom-field values that MOVED, as `slug=value` on each
// side, and whether any moved at all. Only ids whose stored bytes differ
// appear, both sides of each, in [paramsText]'s shape: the whole map is up to
// [MaxFieldValues] × [MaxFieldValueBytes]. By slug, through `declared`; a nil
// catalogue is the writer's frame and yields nothing ([TaskDeltas]). A field
// the project does not declare is counted, not named: it has no slug
// (`fieldvalues.go`'s foreign rows), and dropping it would make a commit that
// moved only it read as moving nothing. `changed` comes from the bytes, not
// the text: values cut at [MaxDeltaElement] can render alike.
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
			from: proseText(fieldValueText(field, before[id]), MaxDeltaElement),
			to:   proseText(fieldValueText(field, after[id]), MaxDeltaElement),
		})
	}
	// By slug, so two nodes write one string, and both sides in that one
	// order, so a reader can line them up member for member.
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
// A choice is its option's SLUG: [coerceOption] stores the ID so a rename
// never re-points a value, and the declaration in hand turns it back into the
// word chosen. A relationship keeps its task ID: the other task is another
// ROW, which `deltas.go`'s head forbids quoting by key. A multi-valued field's
// members join whole with "/" (", " separates fields); the caller describes a
// value past [MaxDeltaElement] by its size ([proseText]), and a whole value is
// bounded where written ([MaxFieldValueBytes]).
func fieldValueText(field FieldDef, raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var members []json.RawMessage
	if err := json.Unmarshal(raw, &members); err == nil {
		out := make([]string, 0, len(members))
		for _, member := range members {
			out = append(out, fieldValueText(field, member))
		}
		return strings.Join(out, "/")
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		// A number, a boolean or a shape this build does not know: its own
		// JSON, as typed. Guessing a shape is how `"3"` and `3` stop comparing.
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

// relationMembers is one kind's counterparties, as the members of the set a
// delta records. Per kind, because the kinds are unrelated facts: adding a
// `page` link must not read as a change to what the task is `waiting_on`.
func relationMembers(relations []Relation, kind RelationKind) []string {
	others := make([]string, 0, len(relations))
	for _, relation := range relations {
		if relation.Kind == kind {
			others = append(others, relation.Other)
		}
	}
	return others
}

// excerpt is what a card shows, cut rune-safely to the display limit and
// MARKED, the marker counted against the cap. A card is all most readers see
// (inbox, history, activity feed; the wake prompt only where the excerpt was
// stated, [wakeBody]), so an unmarked cut reads as a comment that ENDED there.
// [textcut.Within] rather than Ellipsis because [Notify.Validate] refuses an
// excerpt above MaxExcerpt, which would fail every long comment's write.
//
// An answer that chose leads with the choice (`Chose “Ship Friday”: <body>`):
// the choice is the answer, and a card leading with the body could cut it off.
func (w Wake) excerpt() string {
	text := w.Excerpt
	if text == "" && w.Kind == ChangeCreated {
		// The body before the ask on a create: a task filed as a question
		// carries the question as its title, which the card already shows.
		text = w.After.Body
	}
	if text == "" && w.Comment != nil {
		text = w.Comment.Body
		if option := w.chosen(); option != nil {
			text = choiceExcerpt(option.Label, strings.TrimSpace(text))
		}
	}
	return excerptOf(text)
}

// excerptOf is the card excerpt a text is shown as — ONE RULE, because the
// wake prompt recognises an excerpt derived from a text by deriving it again
// (see [wakeBody]) and two spellings of the rule would stop recognising it.
func excerptOf(text string) string { return textcut.Within(strings.TrimSpace(text), MaxExcerpt) }

// chosen is the option this wake's comment chose, when its ask carried one.
func (w Wake) chosen() *decision.Option {
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
		return choicePrefix + label + "”"
	}
	return choicePrefix + label + "”: " + body
}

// choicePrefix opens the line [choiceExcerpt] sums a choosing answer up as.
const choicePrefix = "Chose “"

// ExcerptQuotesAChoice reports whether a change's excerpt is the line
// [choiceExcerpt] wrote for an answer that chose, which quotes the ASKER's
// option label, so the answerer is not its only writer. Read off the line
// itself, because its readers (activity feed, history, inbox) hold no payload;
// a remark opening the same way counts as quoting too, the safe direction.
func ExcerptQuotesAChoice(excerpt string) bool { return strings.HasPrefix(excerpt, choicePrefix) }

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

// The schedule values, as the TEXT a delta carries. An absent value is the
// empty string, drawn as an em dash, so "— → 16 Apr" reads like "— → jun".
//
// A zero is absent for an estimate and a size: [Task.EstimateMinutes] and
// [Task.Points] are plain numbers over NOT NULL columns defaulting to 0, which
// every reader already spends as "unsized"; "0m" would disagree. A pointer for
// "sized at nothing" is deliberately not wanted: nothing can act on the
// distinction, and it costs a nullable column, a migration and a nil check in
// every sum. Clearing still shows ("3" → ""); only 0 → 0 folds.

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

// instantText renders an optional instant for a delta or a wake's metadata,
// and an absent one as the empty string rather than a zero instant that renders
// as the year one. A pointer at the zero instant is absent too: the estate's
// wire carries it as nil, so a wake parsed on either side of it reads alike.
func instantText(at *time.Time) string {
	if at == nil || at.IsZero() {
		return ""
	}
	return at.UTC().Format(time.RFC3339)
}
