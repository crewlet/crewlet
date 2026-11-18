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

// A Worklet wake about work the seat holds ([worklet.Reason.Held]: an
// assignment, a new dependent on its item, its item unblocked) whose item was
// finished, removed or handed to somebody else since starts no turn: it asks
// nothing of the seat any more, however late it arrives, and its inbox row
// still tells it. A comment is kept whatever its item became.

// dropStaleWakes is [Dispatcher.StaleWakes]: evs less the wakes about work
// the seat no longer holds, each logged.
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

// staleWake names ev's item, its reason and why the wake is stale, or "" for
// a wake about work the seat does not hold, a comment, or an unread item.
// Floored at the wake's own change, so a node behind it never reads a fresh
// assignment as removed.
func staleWake(ctx context.Context, tasks taskReader, handle string,
	ev *events.Event) (item string, reason worklet.Reason, why string) {

	n, ok := events.DataAs[*types.ExternalNotification](ev)
	if !ok || n == nil || n.NotificationSource != worklet.Source {
		return "", "", ""
	}
	reason = worklet.Reason(n.Metadata[worklet.MetaVia])
	if !reason.Held() || worklet.ChangeKind(n.SourceEventType).Conversation() {
		return "", "", ""
	}
	item = n.Metadata[worklet.MetaTaskID]
	at, err := statelog.ParsePosition(notify.TriggerOf(ev))
	if item == "" || err != nil || at.IsZero() {
		return "", "", ""
	}
	detail, err := tasks.Task(ctx, item, worklet.DetailWants{},
		statelog.FloorAt(statelog.DefaultReadLevel(statelog.SurfaceSeat), at))
	switch {
	case errors.Is(err, worklet.ErrNoTask):
		return item, reason, "removed"
	case err != nil:
		return "", "", ""
	case detail.Task.Removed != nil:
		return item, reason, "removed"
	case detail.Task.Status.Group().Finished():
		return item, reason, "finished"
	case detail.Task.Assignee != handle:
		return item, reason, "reassigned"
	}
	return "", "", ""
}
