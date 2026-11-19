package engine

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A Worklet state notice about work the seat holds ([worklet.StateNotice],
// [worklet.Reason.Held]) whose relation ended starts no turn (ADR-0162): it
// asks nothing of the seat any more, however late it arrives, and its inbox
// row still tells it. Every such notice is moot once its item was finished,
// removed or handed to somebody else; a `blocking` one also once every
// dependent it names was finished, removed or no longer waits on the item; an
// `unblocked` one also once its item is blocked again. A flow notice, told to
// a lead, is moot once its item was finished or removed or has left the state
// it told of, judged only where the read can tell ([worklet.FlowLeft]). Words
// are never moot.

// dropStaleWakes is [Dispatcher.StaleWakes]: evs less the state notices whose
// relation ended, each logged.
func (e *Engine) dropStaleWakes(ctx context.Context, handle string, evs []*events.Event) []*events.Event {
	if e.router == nil {
		return evs
	}
	return staleWakes(ctx, e.router.Work(), handle, evs)
}

// staleWakes is [Engine.dropStaleWakes] over tasks.
func staleWakes(ctx context.Context, tasks taskReader, handle string, evs []*events.Event) []*events.Event {
	out := evs[:0:0]
	for _, ev := range evs {
		item, reason, why := staleWake(ctx, tasks, handle, ev)
		if why == "" {
			out = append(out, ev)
			continue
		}
		log.InfoContext(ctx, "work_wake_stale", "seat", handle, "event", ev.ID.String(),
			"item", item, "reason", string(reason), "why", why,
			"detail", "the item no longer asks anything of this seat; no turn runs for this wake")
	}
	return out
}

// staleWake names ev's item, its reason and why the wake is moot, or "" for a
// wake that is no state notice about held work or the item's flow, or one a
// read could not settle. Every read is floored at the wake's own change, so a
// node behind it never reads a fresh assignment as removed.
func staleWake(ctx context.Context, tasks taskReader, handle string,
	ev *events.Event) (item string, reason worklet.Reason, why string) {

	n, ok := events.DataAs[*types.ExternalNotification](ev)
	if !ok {
		return "", "", ""
	}
	reason, state := worklet.StateNotice(n)
	told, flow := worklet.WakeFlow(n.Metadata)
	flow = flow && reason == worklet.ReasonFlow
	if !state || (!reason.Held() && !flow) {
		return "", "", ""
	}
	item = n.Metadata[worklet.MetaTaskID]
	at, err := statelog.ParsePosition(notify.TriggerOf(ev))
	if item == "" || err != nil || at.IsZero() {
		return "", "", ""
	}
	fresh := statelog.FloorAt(statelog.DefaultReadLevel(statelog.SurfaceSeat), at)
	var want worklet.DetailWants
	if flow {
		// THE ITEM'S FLOW NOW, which only a read at an instant answers.
		want.Clock = &worklet.DayClock{Now: time.Now(), Zone: time.UTC}
	}
	detail, err := tasks.Task(ctx, item, want, fresh)
	switch why = taskEnded(detail, err); {
	case why != "":
		return item, reason, why
	case err != nil:
		return "", "", ""
	case flow:
		// A LEAD TOLD OF A COLLEAGUE'S ITEM: not reassigned away from it.
		if worklet.FlowLeft(told, detail.Flow, detail.FlowLive) {
			return item, reason, "it left the state it told of"
		}
		return "", "", ""
	case detail.Task.Assignee != handle:
		return item, reason, "reassigned"
	case reason == worklet.ReasonUnblocked && detail.Blocked:
		return item, reason, "blocked again"
	case reason == worklet.ReasonBlocking &&
		dependentsEnded(ctx, tasks, item, worklet.WakeDependents(n.Metadata), fresh):
		return item, reason, "its dependents ended"
	}
	return "", "", ""
}

// taskEnded is why a task a read answered with err is over, "removed" or
// "finished", or "" for one still open or not read.
func taskEnded(detail worklet.TaskDetail, err error) string {
	switch {
	case errors.Is(err, worklet.ErrNoTask):
		return "removed"
	case err != nil:
		return ""
	case detail.Task.Removed != nil:
		return "removed"
	case detail.Task.Status.Group().Finished():
		return "finished"
	}
	return ""
}

// dependentsEnded reports whether a `blocking` notice on blocker named
// dependents and every one was finished, removed or no longer waits on
// blocker. One a read could not settle keeps the notice.
func dependentsEnded(ctx context.Context, tasks taskReader, blocker string, dependents []string,
	fresh statelog.Freshness) bool {

	if len(dependents) == 0 {
		return false
	}
	for _, id := range dependents {
		detail, err := tasks.Task(ctx, id, worklet.DetailWants{}, fresh)
		if taskEnded(detail, err) != "" {
			continue
		}
		if err != nil || waitsOn(detail.Task, blocker) {
			return false
		}
	}
	return true
}

// waitsOn reports whether task's own edges still name blocker as one it waits
// on.
func waitsOn(task worklet.Task, blocker string) bool {
	return slices.ContainsFunc(task.Relations, func(r worklet.Relation) bool {
		return r.Kind == worklet.RelationWaitingOn && r.Other == blocker
	})
}
