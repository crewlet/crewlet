package worklet

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/words"
)

// A removed seat's work is re-homed at its retirement (ADR-0162). An item whose
// assignee the chart no longer holds reads held/departed at once ([flowExpr]);
// the work moves only when the seat's mailbox is retired, a day after the
// removal (internal/maintenance), since a removal is often a mistake undone
// within the day. [Writer.RehomeDeparted] then writes each open item once, as
// the engine, the record's excerpt being the history note that names the seat.

// Departure is a seat the chart no longer holds, as its retirement hands it to
// [Writer.RehomeDeparted].
type Departure struct {
	// Handle is the seat's handle: the assignee its items carry.
	Handle string

	// Runs are the ids of the items a run of the seat is on, which the
	// retirement ends.
	Runs []string

	// At is the instant the retirement began. Each write's operation is
	// derived from it, so a write the retirement retries is one operation.
	At time.Time
}

// SideEffect is something outside an item's own fields that will come back to
// it: ADR-0162's live side effect, which sends an item in progress to its lead
// rather than back to `todo`.
type SideEffect string

const (
	// SideEffectDate is a wait with a date: a start still ahead, or one that
	// passed with nothing of the seat's on the item since.
	SideEffectDate SideEffect = "date"
	// SideEffectQuestion is the seat's own open question on the item.
	SideEffectQuestion SideEffect = "question"
	// SideEffectChange is an open pull request the item waits on: a blocker
	// that is not a work item.
	SideEffectChange SideEffect = "change"
	// SideEffectRun is a run of the seat on the item, which the retirement
	// ends.
	SideEffectRun SideEffect = "run"
)

// Rehomed is one item a departure re-homed.
type Rehomed struct {
	ID      string
	Key     string
	Project string

	// From is the status it had and Status the one it has now.
	From   Status
	Status Status

	// To is the lead who holds it now, empty for an item left unassigned.
	To string

	// Effects are the live side effects of an item in progress, in
	// [SideEffect]'s order; empty on any other item.
	Effects []SideEffect

	// Leaderless is an item a lead would have taken whose project and
	// routing unit nobody leads, so it went back to `todo`, unassigned.
	Leaderless bool
}

// departedWorkStatement is [Writer.RehomeDeparted]'s read of a seat's open
// items, named for the plan test: a seek of the assignee's queue, each item
// with the two live side effects its own row does not hold, read with the flow
// expression's own facts ([flowNothingSinceStart], [flowOwnAsk]).
func departedWorkStatement(now time.Time, handle string) (string, []any) {
	open := openGroups()
	return `SELECT t.document, t.version, t.scoped_through, t.rank,
		       CASE WHEN t.start_at > ? THEN 1 WHEN ` + flowNothingSinceStart + ` THEN 1 ELSE 0 END,
		       ` + flowOwnAsk + `
		FROM tracker_tasks t
		WHERE t.assignee = ? AND t.removed_at IS NULL
		  AND t.status_group IN (` + placeholders(len(open)) + `)`,
		append([]any{store.EncodeTime(now), handle}, open...)
}

// departedHoldersStatement is every holder of open work the flow reads as
// departed ([flowDeparted]): the seats [Writer.DepartedHolders] answers, a walk
// of the assignees' queue.
func departedHoldersStatement() (string, []any) {
	open := openGroups()
	return `SELECT DISTINCT t.assignee FROM tracker_tasks t
		WHERE t.removed_at IS NULL AND t.assignee <> ''
		  AND t.status_group IN (` + placeholders(len(open)) + `)
		  AND ` + flowDeparted, append(open, RosterSubject().ID)
}

// DepartedHolders is every handle that holds open work and that the chart, as
// this node last applied it, does not place, none before a chart was applied:
// how a retirement that knows a seat only by its id finds the handle its items
// carry.
func (w *Writer) DepartedHolders(ctx context.Context) ([]string, error) {
	statement, args := departedHoldersStatement()
	var out []string
	err := w.db.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, statement, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var handle string
			if err := rows.Scan(&handle); err != nil {
				return err
			}
			out = append(out, handle)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("worklet: read the departed holders of open work: %w", err)
	}
	return out, nil
}

// departedItem is one open item of a departed seat as the listing read it.
type departedItem struct {
	task     Task
	dated    bool
	question bool
}

// RehomeDeparted re-homes every open item d's seat still holds, one write per
// item, by ADR-0162's rule: an item in progress with a live side effect, or in
// review, goes to its lead (the project's, else the routing unit's); any other
// is unassigned, back in `todo`, and one neither leads is logged. An item left
// unassigned names no lead to fall back on: each lead is told once, on its last
// such item's record, which lists them all.
//
// ONLY THE ENGINE'S WRITER, as its history row says. Each item is written at the
// version it was read at: one changed since is left out and reported in the
// error, which the retirement retries.
func (w *Writer) RehomeDeparted(ctx context.Context, d Departure) ([]Rehomed, error) {
	switch {
	case w.ActorKind != AuthorSystem:
		return nil, invalid("a departed seat's work is re-homed by the engine, "+
			"and this writer acts as %q of kind %q", w.Actor, w.ActorKind)
	case d.Handle == "":
		return nil, invalid("a departure names no seat")
	case d.At.IsZero():
		return nil, invalid("the departure of %q names no instant its retirement began at", d.Handle)
	}
	now := w.Now()
	items, err := w.departedWork(ctx, d.Handle, now)
	if err != nil {
		return nil, err
	}
	plans := make([]rehomePlan, len(items))
	carrier := map[string]int{}
	for i, item := range items {
		leads := w.leadsOf(item.task, d.Handle)
		plans[i] = rehomePlan{item: item, moved: rehoming(item.task, item.effects(item.task, d.Runs), leads)}
		if plans[i].moved.To == "" {
			if plans[i].lead = cmp.Or(leads.project, leads.unit); plans[i].lead != "" {
				carrier[plans[i].lead] = i
			}
		}
	}
	var out []Rehomed
	var errs []error
	told := map[string][]string{}
	write := func(p rehomePlan, carries bool) {
		var batch []string
		if carries {
			batch = append(slices.Clone(told[p.lead]), cmp.Or(p.item.task.Key, p.item.task.ID))
		}
		moved, err := w.rehomeOne(ctx, d, p, now, batch)
		if err != nil {
			if len(batch) > 1 {
				log.WarnContext(ctx, "tracker_departed_work_untold", "handle", d.Handle,
					"lead", p.lead, "items", batch, "detail", "the record that tells the lead of "+
						"these unassigned items failed; the retry tells it of what it re-homes")
			}
			errs = append(errs, err)
			return
		}
		if moved.Leaderless {
			log.WarnContext(ctx, "tracker_departed_work_unled", "handle", d.Handle,
				"item", moved.Key, "project", moved.Project,
				"detail", "a removed seat's item would have gone to its lead, and nobody "+
					"leads its project or its routing unit; it is back in todo, unassigned")
		}
		if p.lead != "" {
			told[p.lead] = append(told[p.lead], cmp.Or(moved.Key, moved.ID))
		}
		out = append(out, moved)
	}
	// Each lead's carrier last, so its list names what the others wrote.
	for i, p := range plans {
		if p.lead == "" || carrier[p.lead] != i {
			write(p, false)
		}
	}
	for i, p := range plans {
		if p.lead != "" && carrier[p.lead] == i {
			write(p, true)
		}
	}
	return out, errors.Join(errs...)
}

// rehomePlan is one item of a departure, where [rehoming] sends it, and the
// lead told of it when it is left unassigned.
type rehomePlan struct {
	item  departedItem
	moved Rehomed
	lead  string
}

// departedWork reads the open items handle holds, each with its side effects.
func (w *Writer) departedWork(ctx context.Context, handle string, now time.Time) ([]departedItem, error) {
	statement, args := departedWorkStatement(now, handle)
	var out []departedItem
	err := w.db.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, statement, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var document []byte
			var version, scoped int64
			var rank string
			var item departedItem
			if err = rows.Scan(&document, &version, &scoped, &rank,
				&item.dated, &item.question); err != nil {
				return err
			}
			if item.task, err = storedTask(document, version, scoped, rank); err != nil {
				return err
			}
			out = append(out, item)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("worklet: read the open work of departed seat %q: %w", handle, err)
	}
	return out, nil
}

// rehomeOne writes one item where its plan sends it. Its record names no lead to
// fall back on; one that carries a batch tells its lead of every item listed.
func (w *Writer) rehomeOne(ctx context.Context, d Departure, p rehomePlan,
	now time.Time, batch []string) (Rehomed, error) {

	task, moved := p.item.task, p.moved
	after := task
	after.Assignee, after.Status, after.StatusGroup = moved.To, moved.Status, moved.Status.Group()
	patch := TaskPatch{Assignee: &moved.To}
	if moved.Status != task.Status {
		patch.Status = &moved.Status
	}
	note := departureNote(d.Handle, task, moved, now, w.zone())
	if len(batch) > 1 {
		note += " " + batchNote(d.Handle, batch)
	}
	notify := Wake{Kind: ChangeAssignee, Before: task, After: after, Excerpt: note}.Notify(nil)
	if batch != nil {
		notify.Snapshot.DepartureLead = p.lead
	}
	op := statelog.DeriveOpID(d.At, "rehome-"+task.ID, "crewlet.tracker.rehome",
		d.Handle, task.ID, strconv.FormatUint(task.Version, 10))
	result, err := w.UpdateTask(ctx, op, task.ID, task.Project, task.Version, patch,
		ChangeAssignee, notify)
	if err != nil {
		return Rehomed{}, fmt.Errorf("worklet: re-home %s of departed seat %q: %w",
			task.Key, d.Handle, err)
	}
	if result.Outcome == statelog.OutcomeUnknown {
		return Rehomed{}, fmt.Errorf("worklet: whether %s of departed seat %q was re-homed "+
			"is unknown (operation %s); the retirement reads it again", task.Key, d.Handle, op)
	}
	return moved, nil
}

// effects are an item's live side effects: its listing's two, the change it
// waits on that its own document holds open, and a run of the seat on it.
func (item departedItem) effects(task Task, runs []string) []SideEffect {
	var out []SideEffect
	if item.dated {
		out = append(out, SideEffectDate)
	}
	if item.question {
		out = append(out, SideEffectQuestion)
	}
	if slices.ContainsFunc(task.Relations, func(r Relation) bool {
		_, settled := task.Changes[r.Other]
		return r.Kind == RelationWaitingOn && IsChangeRef(r.Other) && !settled
	}) {
		out = append(out, SideEffectChange)
	}
	if slices.Contains(runs, task.ID) {
		out = append(out, SideEffectRun)
	}
	return out
}

// itemLeads are the two leads an item may go to, by the chart: its project's
// and its routing unit's.
type itemLeads struct{ project, unit string }

// leadsOf is task's two leads, never the departed seat itself, whom a chart
// read before the removal could still name.
func (w *Writer) leadsOf(task Task, handle string) itemLeads {
	var out itemLeads
	if w.Leads == nil {
		return out
	}
	if lead := w.Leads.ProjectLead(task.Project); lead != handle {
		out.project = lead
	}
	if task.RoutingUnit != "" {
		if lead := w.Leads.UnitLead(task.RoutingUnit); lead != handle {
			out.unit = lead
		}
	}
	return out
}

// rehoming is ADR-0162's rule for one open item of a departed seat.
func rehoming(task Task, effects []SideEffect, leads itemLeads) Rehomed {
	out := Rehomed{ID: task.ID, Key: task.Key, Project: task.Project,
		From: task.Status, Status: task.Status}
	toLead := false
	switch task.Status {
	case StatusInProgress:
		out.Effects = effects
		toLead = len(effects) > 0
		if !toLead {
			out.Status = StatusTodo
		}
	case StatusInReview:
		toLead = true
	}
	if !toLead {
		return out
	}
	out.To = cmp.Or(leads.project, leads.unit)
	if out.To == "" {
		out.Status, out.Leaderless = StatusTodo, true
	}
	return out
}

// departureNote is the history note a re-homing writes as its record's
// excerpt: the seat that left, where the item went and, for an item in
// progress its lead took, the side effects that sent it there.
func departureNote(handle string, task Task, moved Rehomed, now time.Time,
	zone *time.Location) string {

	left := handle + " left the chart."
	switch {
	case moved.Leaderless:
		return handle + " left the chart, and nobody leads its project or its unit. " +
			"It is back in todo, unassigned."
	case moved.To == "" && moved.From == StatusTodo:
		return left + " It is unassigned."
	case moved.To == "":
		return left + " It is back in todo, unassigned, for whoever takes it next."
	case moved.Status == StatusInReview:
		return left + " " + moved.To + " holds it now, to finish its review."
	}
	var because []words.Said
	for _, effect := range moved.Effects {
		because = append(because, effectWords(effect, handle, task, now, zone))
	}
	return left + " " + moved.To + " holds it now, still in progress, because " +
		words.And(because) + "."
}

// batchNote lists the items a departure left without an owner in one lead's
// charge, as many keys as fit the excerpt beside the item's own note.
func batchNote(handle string, keys []string) string {
	const room = MaxExcerpt / 2
	shown, used := keys, 0
	for i, key := range keys {
		if used += len(key) + 2; used > room {
			shown = append(slices.Clone(keys[:i]), fmt.Sprintf("%d more", len(keys)-i))
			break
		}
	}
	said := make([]words.Said, len(shown))
	for i, key := range shown {
		said[i] = words.Said(key)
	}
	return fmt.Sprintf("%d items %s held have no owner now: %s.", len(keys), handle, words.And(said))
}

// effectWords is how a history note says one side effect.
func effectWords(effect SideEffect, handle string, task Task, now time.Time,
	zone *time.Location) words.Said {

	switch effect {
	case SideEffectDate:
		if task.StartAt == nil {
			return words.Said("it waits for a date")
		}
		start := task.StartAt.In(zone).Format("2006-01-02 15:04 MST")
		if task.StartAt.After(now) {
			return words.Said("it waits until " + start)
		}
		return words.Said("its start, " + start + ", passed with nothing done since")
	case SideEffectQuestion:
		return words.Said(handle + "'s question on it is still open")
	case SideEffectChange:
		return words.Said("it waits on an open pull request")
	default:
		return words.Said(handle + "'s run on it was ended")
	}
}
