package engine

import (
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
)

// Turning one conversation's partition into one ask.
//
// [notify.Coalesce] renders the digest — the chronological list of what was
// said, the latest message in full, the per-integration supersede rules and the
// same-sender duplicate collapse. This file is the frame that decides WHEN it
// runs, what the merged event has to carry, and what happens when a partition
// cannot be merged at all.
//
// IT HAD NO CALLER. The coalescer, its rules and the constituent list were
// written, documented, schema'd into an event type and covered by their own
// suite, and nothing in the engine ever called them — the exact shape of the
// two config knobs that reached queue.BatchOptions through nothing (see
// [Engine.tuneBatching]), one layer up. What shipped was the BATCHING half:
// five comments on one issue did become one turn rather than five. What that
// turn was handed was the five enriched bodies concatenated — five copies of
// the third-party app's triage scaffolding, five "how to get full context" blocks
// pointing at progressively staler state, and five separate asks, which a
// model answers separately. The doc's headline promise, one notification for
// the remaining messages, was half true: one TURN, five NOTIFICATIONS in it.

// mergePartition merges a multi-event partition into the ONE event its turn is
// asked about, reporting false when it cannot.
//
// TWO KINDS OF PARTITION MERGE, and each has its own rule. A conversation's
// notifications become a digest of everything said ([mergeNotifications]); a
// schedule's waiting fires become its latest fire, standing for the rest
// ([mergeScheduledFires]). Anything else is a key-scheme bug, answered by the
// caller's per-event fallback.
func mergePartition(prompts notify.Prompts, evs []*events.Event) (*events.Event, bool) {
	if merged, ok := mergeScheduledFires(evs); ok {
		return merged, true
	}
	return mergeNotifications(prompts, evs)
}

// mergeScheduledFires turns fires of one schedule that reached the seat
// together into ONE run: the latest fire, told which earlier fires it stands
// for ([types.TaskAssigned.Missed]).
//
// # Why the latest and not a digest
//
// A fire carries no conversation to summarise: every fire of a schedule asks
// for the same work, and the work is done as of now whichever tick asked for
// it. What a seat needs from the earlier ones is only that they existed, so
// it does the duty once and does not read a late run as an anomaly.
//
// THE LATEST BY THE INSTANT IT CAME DUE, and the envelope's own instant only
// for a fire that carries none, so a fire republished by a park cannot pass
// for a later tick than it is.
//
// ITS TRACE IS THE LATEST FIRE'S, unlike a digest's, which takes its first
// constituent's: the run IS the latest fire, so the schedule's history links
// that fire to the turn and each earlier fire to its own superseded record
// ([Dispatcher.noteSuperseded]).
//
// Reports false for anything that is not two or more fires of one schedule.
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
// event carrying the digest.
//
// Reports false when the partition is not mergeable, which the caller answers
// by degrading to per-event dispatch rather than by losing an event. Two ways
// that happens, and both are a key-scheme bug rather than an ordinary case
// (see [inbox.Route]): a constituent that is not an external notification at
// all, and one whose typed body did not decode — an event from a build that
// renamed the payload, which reaches this build as an envelope with nothing
// in it.
//
// THE ENVELOPE IS THE LATEST CONSTITUENT'S, because the merged body ends with
// that message in full and every pointer a reply follows — a thread ts, an
// issue key, a "reply here" — is read off it. The two exceptions are the two
// facts a merge must not be allowed to launder, and each takes the same value
// the dispatcher derives for the turn itself: the trace comes from the FIRST
// constituent, matching [triggerTrace] and the event [DescribeTrigger] leads
// with, and the delegation depth is the DEEPEST, matching [delegationOf].
//
// The id is FRESH, which is why the work key is derived from the constituents
// before this runs: a digest minted on every drain would key differently on
// every redelivery and match nothing the completion ledger holds.
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
	// BOTH KEYS ride the merged envelope, because it REPLACES its
	// constituents: from here on this one event is the partition, and a
	// reader handed it must be able to ask either question. Every reader
	// downstream of the merge is such a reader — the dispatcher deriving the
	// turn's conversation from the trigger, and anything re-reading a stored
	// digest later.
	//
	// The stamp is NOT for the sandbox coordinator, which is what this
	// comment used to say: the answer match runs on screening.Events, before
	// the merge, and never sees this envelope.
	//
	// The constituents all carry the same pair by construction — the
	// partition key because that is what the broker grouped them on, the
	// identity because a source's partition key refines it — so taking both
	// through the dispatcher's own derivations keeps the "first non-empty"
	// rule in one place rather than two.
	notify.Stamp(out, partitionKeyOf(evs), conversationIdentityOf(evs))
	return out, true
}

// prompts is the third-party app registry the merge renders with.
//
// A FUNCTION on the dispatcher rather than a captured value, for the reason
// [Dispatcher.Conversation] is one: the dispatcher is built once and the
// registry belongs to an epoch a live apply replaces. A captured copy would
// keep merging with the integrations the process started with.
//
// An empty registry is a working answer rather than a missing one: every
// source falls back to [notify.Generic], whose DigestBody passes a body
// through unchanged — so a node with no integrations configured still merges,
// it just supersedes nothing.
func (d *Dispatcher) promptRegistry() notify.Prompts {
	if d.Prompts == nil {
		return notify.NewPrompts()
	}
	return d.Prompts()
}

// notifyPrompts is the live third-party app registry, or an empty one before the
// notification service has been started.
//
// ASKED OF THE SERVICE, never cached here. An apply calls [notify.Service.Replace]
// for every integration the new revision enables, and the service is where
// those land — so a copy taken when the service was built would go on merging
// with the third-party apps the PROCESS started with, and a company that added Jira on
// a later revision would silently lose that third-party app's supersede rules from
// every digest for the life of the node.
func (e *Engine) notifyPrompts() notify.Prompts {
	e.notify.mu.Lock()
	svc := e.notify.service
	e.notify.mu.Unlock()
	if svc == nil {
		return notify.NewPrompts()
	}
	return svc.Prompts()
}
