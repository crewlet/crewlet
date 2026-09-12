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
	return snapshot
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
