package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The seen-through mark (ADR-0120): a wake whose record a completed turn in
// its conversation already read past, and which obliges the seat to nothing,
// is absorbed rather than run.

// skipCause is why [Dispatcher.dropWorked] dropped a trigger.
type skipCause int

const (
	// skipWorked: a turn was woken for this very trigger.
	skipWorked skipCause = iota
	// skipInThread: a later turn's thread block showed the message waiting.
	skipInThread
	// skipAbsorbed: a completed turn read its conversation past it.
	skipAbsorbed
)

// absorbed reports whether ev is at or below its seat's mark for its
// conversation and asks the seat nothing; marks caches one read per conversation.
func (d *Dispatcher) absorbed(ctx context.Context, handle string, ev *events.Event,
	marks map[string]statelog.Position) bool {

	if d.SeenThrough == nil || obliges(ev) {
		return false
	}
	at, conversation := seenAt(ev)
	if at.IsZero() || conversation == "" {
		return false
	}
	mark, read := marks[conversation]
	if !read {
		mark = d.SeenThrough.Mark(ctx, handle, conversation)
		marks[conversation] = mark
	}
	return !mark.IsZero() && !ledgerstore.Past(at, mark)
}

// advanceSeen moves the seat's mark for a completed turn's conversation to the
// newest record it was woken for.
func (d *Dispatcher) advanceSeen(ctx context.Context, handle string, evs []*events.Event) {
	if d.SeenThrough == nil {
		return
	}
	conversation := notify.KeyOfAll(evs)
	var newest statelog.Position
	for _, ev := range evs {
		if at, _ := seenAt(ev); !at.IsZero() && ledgerstore.Past(at, newest) {
			newest = at
		}
	}
	if conversation == "" || newest.IsZero() {
		return
	}
	if err := d.SeenThrough.Advance(ctx, handle, conversation, newest); err != nil {
		log.WarnContext(ctx, "seen_through_not_recorded", "seat", handle, "error", err,
			"detail", "a redelivery this turn already read may run as a turn of its own")
	}
}

// seenAt is the log position of the record ev was derived from and its
// conversation's stamped key (the thread, never split by writer).
func seenAt(ev *events.Event) (statelog.Position, string) {
	raw := notify.TriggerOf(ev)
	if raw == "" {
		return statelog.Position{}, ""
	}
	at, err := statelog.ParsePosition(raw)
	if err != nil {
		return statelog.Position{}, ""
	}
	return at, notify.KeyOfAll([]*events.Event{ev})
}

// obliges reports whether ev asks the seat for anything, as its source routed
// it: addressed to it, owed on a chat surface, or a question put to the room.
// Only a notification can be absorbed.
func obliges(ev *events.Event) bool {
	n, ok := events.DataAs[*types.ExternalNotification](ev)
	if !ok || n == nil {
		return true
	}
	return n.Addressed || n.Owes != "" || n.Metadata[chatlet.MetaAsked] != ""
}

// skippedRecord is the record of a trigger not run, with the disposition the
// turn ledger counts it under where it has one.
func (d *Dispatcher) skippedRecord(handle string, ev *events.Event, disposition types.WakeDisposition,
	reason string) *events.Event {

	skipped := types.TurnTriggerSkipped{AgentHandle: handle, TriggerID: ev.ID.String(),
		TriggerType: ev.Type, Reason: reason, Disposition: disposition}
	if disposition != "" {
		skipped.Wake = types.WakeOf(types.DescribeTrigger(ev))
		if d.Identify != nil {
			_, skipped.Agent = d.Identify(handle)
		}
	}
	return events.New(skipped, triggerTrace([]*events.Event{ev}))
}
