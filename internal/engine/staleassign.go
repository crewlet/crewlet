package engine

import (
	"context"
	"errors"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// An assignment wake whose Worklet item was finished, removed or handed to
// somebody else since starts no turn: it asks nothing of the seat any more,
// and its inbox row still tells it. A comment is kept whatever its item became.

// dropStaleAssignments is [Dispatcher.StaleAssignments]: evs less the
// assignment wakes whose item moved on, each logged.
func (e *Engine) dropStaleAssignments(ctx context.Context, handle string, evs []*events.Event) []*events.Event {
	if e.router == nil {
		return evs
	}
	return staleAssignments(ctx, e.router.Work(), handle, evs)
}

// staleAssignments is [Engine.dropStaleAssignments] over tasks.
func staleAssignments(ctx context.Context, tasks taskReader, handle string, evs []*events.Event) []*events.Event {
	out := evs[:0:0]
	for _, ev := range evs {
		item, why := staleAssignment(ctx, tasks, handle, ev)
		if why == "" {
			out = append(out, ev)
			continue
		}
		log.InfoContext(ctx, "assignment_wake_stale", "seat", handle, "event", ev.ID.String(),
			"item", item, "why", why,
			"detail", "the item no longer asks anything of this seat; no turn runs for this wake")
	}
	return out
}

// staleAssignment names ev's item and why its assignment wake is stale, or ""
// for no assignment, a comment, or an unread item. Floored at the wake's own
// change, so a node behind it never reads a fresh assignment as removed.
func staleAssignment(ctx context.Context, tasks taskReader, handle string, ev *events.Event) (item, why string) {
	n, ok := events.DataAs[*types.ExternalNotification](ev)
	if !ok || n == nil || n.NotificationSource != worklet.Source ||
		worklet.Reason(n.Metadata[worklet.MetaVia]) != worklet.ReasonAssignee ||
		worklet.ChangeKind(n.SourceEventType).Conversation() {
		return "", ""
	}
	item = n.Metadata[worklet.MetaTaskID]
	at, err := statelog.ParsePosition(notify.TriggerOf(ev))
	if item == "" || err != nil || at.IsZero() {
		return "", ""
	}
	detail, err := tasks.Task(ctx, item, worklet.DetailWants{},
		statelog.FloorAt(statelog.DefaultReadLevel(statelog.SurfaceSeat), at))
	switch {
	case errors.Is(err, worklet.ErrNoTask):
		return item, "removed"
	case err != nil:
		return "", ""
	case detail.Task.Removed != nil:
		return item, "removed"
	case detail.Task.Status.Group().Finished():
		return item, "finished"
	case detail.Task.Assignee != handle:
		return item, "reassigned"
	}
	return "", ""
}
