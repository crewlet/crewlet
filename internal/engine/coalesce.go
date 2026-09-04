package engine

import (
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
)

// Turning one conversation's partition into one ask.
//
// [notify.Coalesce] renders the digest — the chronological list of what was
// said, the latest message in full, the per-vendor supersede rules and the
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
// the vendor's triage scaffolding, five "how to get full context" blocks
// pointing at progressively staler state, and five separate asks, which a
// model answers separately. The doc's headline promise, one notification for
// the remaining messages, was half true: one TURN, five NOTIFICATIONS in it.

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
	// The conversation key rides the envelope for every reader downstream of
	// the partition — the sandbox coordinator matching a person's answer back
	// to the question that asked it, most of all. The constituents all carry
	// the same one by construction; taking it through [conversationKeyOf]
	// keeps the "first non-empty" rule in one place.
	notify.Stamp(out, conversationKeyOf(evs))
	return out, true
}

// prompts is the vendor registry the merge renders with.
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

// notifyPrompts is the live vendor registry, or an empty one before the
// notification service has been started.
//
// ASKED OF THE SERVICE, never cached here. An apply calls [notify.Service.Replace]
// for every integration the new revision enables, and the service is where
// those land — so a copy taken when the service was built would go on merging
// with the vendors the PROCESS started with, and a company that added Jira on
// a later revision would silently lose that vendor's supersede rules from
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
