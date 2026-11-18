package engine

import (
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
)

// Turning one conversation's partition into one ask. [notify.Coalesce]
// renders the digest (the chronological list, the latest message in full, the
// per-integration supersede rules, the same-sender duplicate collapse); this
// file decides when it runs, what the merged event carries, and what happens
// when a partition cannot be merged. Batching alone would hand one turn every
// enriched body concatenated, as separate asks a model answers separately.

// mergePartition merges a multi-event partition into the ONE event its turn is
// asked about, reporting false when it cannot. A conversation's notifications
// become a digest ([mergeNotifications]); a schedule's waiting fires become its
// latest fire ([mergeScheduledFires]). Anything else is a key-scheme bug,
// answered by the caller's per-event fallback.
func mergePartition(prompts notify.Prompts, evs []*events.Event) (*events.Event, bool) {
	if merged, ok := mergeScheduledFires(evs); ok {
		return merged, true
	}
	return mergeNotifications(prompts, evs)
}

// mergeScheduledFires turns fires of one schedule that reached the seat
// together into ONE run: the latest fire, told which earlier fires it stands
// for ([types.TaskAssigned.Missed]), not a digest: every fire asks for the
// same work, done as of now, so the seat need only know the others existed.
// Reports false for anything that is not two or more fires of one schedule.
//
// Every fire carries the schedule's one conversation as both keys (notify's
// ScheduleConversation), so the merged fire is stamped alike. The latest is by
// [dueAt], so a fire republished by a park cannot pass for a later tick. Its
// trace is the latest fire's, not the first constituent's as a digest's: the
// run IS that fire, and each earlier one links to its superseded record
// ([Dispatcher.noteSuperseded]).
func mergeScheduledFires(evs []*events.Event) (*events.Event, bool) {
	if len(evs) < 2 {
		return nil, false
	}
	fires := make([]*types.TaskAssigned, len(evs))
	latest := -1
	for i, ev := range evs {
		fire, ok := events.DataAs[*types.TaskAssigned](ev)
		if !ok || fire == nil || fire.Schedule == "" {
			return nil, false
		}
		if i > 0 && fire.Schedule != fires[0].Schedule {
			return nil, false
		}
		fires[i] = fire
		if latest < 0 || dueAt(ev, fire).After(dueAt(evs[latest], fires[latest])) {
			latest = i
		}
	}
	kept := *fires[latest]
	kept.Missed = nil
	for i, fire := range fires {
		if i == latest {
			continue
		}
		kept.Missed = append(kept.Missed, types.MissedFire{
			TaskID: fire.TaskID, ScheduledAt: dueAt(evs[i], fire),
		})
	}
	slices.SortStableFunc(kept.Missed, func(a, b types.MissedFire) int {
		return a.ScheduledAt.Compare(b.ScheduledAt)
	})
	src := evs[latest]
	out := events.New(kept, events.TraceContext{
		TraceID: src.TraceID, SpanID: src.SpanID, ParentSpanID: src.ParentSpanID,
	})
	out.Timestamp = src.Timestamp
	out.Source = src.Source
	out.ParentTurnID = src.ParentTurnID
	out.DelegationDepth, out.DelegationChain = delegationOf(evs)
	notify.Stamp(out, partitionKeyOf(evs), conversationIdentityOf(evs))
	return out, true
}

// dueAt is when a fire came due: the instant the scheduler stamped on it, or
// the envelope's own for a fire that carries none.
func dueAt(ev *events.Event, fire *types.TaskAssigned) time.Time {
	if !fire.ScheduledAt.IsZero() {
		return fire.ScheduledAt.UTC()
	}
	return ev.Timestamp.UTC()
}

// mergeNotifications merges a partition of external notifications into ONE
// event carrying the digest. It reports false when a constituent is not an
// external notification or its body did not decode (a build that renamed the
// payload): both a key-scheme bug ([inbox.Route]), which the caller answers
// with per-event dispatch rather than by losing an event.
//
// The envelope is the LATEST constituent's: the body ends with that message in
// full, and every reply pointer is read off it. The two facts a merge must not
// launder match the dispatcher's: the trace is the FIRST constituent's
// ([triggerTrace], [DescribeTrigger]), the delegation depth the DEEPEST
// ([delegationOf]). The id is FRESH, so the work key is derived from the
// constituents first, or a redelivery would match nothing the ledger holds.
func mergeNotifications(prompts notify.Prompts, evs []*events.Event) (*events.Event, bool) {
	if len(evs) < 2 {
		return nil, false
	}
	notes := make([]types.ExternalNotification, 0, len(evs))
	at := make([]time.Time, 0, len(evs))
	var latest *events.Event
	for _, ev := range evs {
		n, ok := events.DataAs[*types.ExternalNotification](ev)
		if !ok || n == nil {
			return nil, false
		}
		notes = append(notes, *n)
		at = append(at, ev.Timestamp)
		if latest == nil || ev.Timestamp.After(latest.Timestamp) {
			latest = ev
		}
	}
	merged, ok := notify.Coalesce(prompts, notes, at)
	if !ok {
		return nil, false
	}

	out := events.New(merged, triggerTrace(evs))
	out.Timestamp = latest.Timestamp
	out.Source = latest.Source
	out.ParentTurnID = latest.ParentTurnID
	out.DelegationDepth, out.DelegationChain = delegationOf(evs)
	// BOTH KEYS ride the merged envelope because it REPLACES its constituents:
	// every reader downstream (the dispatcher deriving the turn's conversation,
	// anything re-reading a stored digest) must be able to ask either. Not for
	// the sandbox coordinator, whose answer match runs on screening.Events
	// before the merge. The constituents share the pair by construction, so the
	// dispatcher's own derivations keep "first non-empty" in one place.
	notify.Stamp(out, partitionKeyOf(evs), conversationIdentityOf(evs))
	return out, true
}

// promptRegistry is the third-party app registry the merge renders with: a
// FUNCTION, not a captured value, since the dispatcher is built once and the
// registry belongs to an epoch a live apply replaces. An empty registry still
// merges: every source falls back to [notify.Generic], whose DigestBody passes
// a body through unchanged, so it supersedes nothing.
func (d *Dispatcher) promptRegistry() notify.Prompts {
	if d.Prompts == nil {
		return notify.NewPrompts()
	}
	return d.Prompts()
}

// notifyPrompts is the live third-party app registry, or an empty one before
// the notification service has started. Asked of the service, never cached: an
// apply calls [notify.Service.Replace] for every integration the new revision
// enables, so a copy would keep the process's first set and drop a later-added
// integration's supersede rules from every digest.
func (e *Engine) notifyPrompts() notify.Prompts {
	e.notify.mu.Lock()
	svc := e.notify.service
	e.notify.mu.Unlock()
	if svc == nil {
		return notify.NewPrompts()
	}
	return svc.Prompts()
}
