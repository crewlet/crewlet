package engine

import (
	"context"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// Below a seat's cap its inbox flows while its runs work (ADR-0169). A delivery
// of a conversation one of them works, or related to one by the router's rule
// or as a triage candidate (wakerouter.go, waketriage.go), is routed to that
// run as the held inbox's router routes it, under the same memo and lock
// ([seatRoute]), and then WAITS ASIDE on the inbox (queue.OutcomeAside) under
// the conversation or the run's key ([sandbox.RunKey]), until the run leaves
// and the coordinator readmits the key: a joined wake the run read is then
// dropped as worked, an unread one runs as its own turn. A delivery related to
// none starts a turn of its own beside them, with no model call. At the cap
// the inbox is held, as before.

// besideAnswer is what a delivery below the seat's cap waits on: the key it
// waits aside under, the run it waits behind (empty for a served conversation
// whose run the listing did not name), the rule its count names, and whether
// that conversation's run is parked waiting on another run (ADR-0176), which
// its writer is told. The zero answer starts a turn beside the seat's runs.
type besideAnswer struct {
	Key, Run string
	Rule     routeRule
	Waits    bool
}

// waitBeside is the dispatcher's step past the answer offer: what [Dispatcher.Beside]
// answers of evs, carried out. A delivery waiting aside whose every wake the
// routing settled (a lead's steer filed, noise absorbed) is acknowledged as the
// ledger says; one past what the inbox keeps aside holds the inbox and defers.
func (d *Dispatcher) waitBeside(ctx context.Context, handle string, evs []*events.Event,
	held *holding) (queue.Result, bool) {

	if d.Beside == nil {
		return queue.Result{}, false
	}
	at, err := d.Beside(ctx, handle, evs)
	if err != nil {
		log.WarnContext(ctx, "seat_wake_aside_unread", "seat", handle, "error", err.Error(),
			"detail", "the delivery is deferred rather than run beside a run that may be doing its work")
		if d.NoteDeferred != nil {
			d.NoteDeferred(handle)
		}
		return queue.Defer(queue.DeferExecutorHolds, fmt.Sprintf("engine: %s: whether the seat's runs "+
			"work this delivery could not be read: %s", handle, err)), true
	}
	if at.Key == "" {
		return queue.Result{}, false
	}
	if surviving, skipped := d.dropWorked(ctx, handle, evs); len(surviving) == 0 {
		held.events = nil
		d.noteSkipped(ctx, handle, evs, surviving, skipped)
		return queue.Ack(), true
	}
	if room, known := queue.AsideRoom(ctx); known && room < len(evs) {
		if d.HoldAside != nil {
			d.HoldAside(ctx, handle)
		}
		if d.NoteDeferred != nil {
			d.NoteDeferred(handle)
		}
		return queue.Defer(queue.DeferExecutorHolds,
			"the seat's inbox holds as many deliveries aside as it keeps"), true
	}
	if d.SetAside != nil {
		d.SetAside(ctx, handle, at, evs)
	}
	return queue.Aside(at.Key, "it waits for the seat's run it relates to"), true
}

// beside is [Dispatcher.Beside]: below the cap, with a run of the seat holding
// it, whether evs wait on one of its runs, each wake routed to the run it
// relates to on the way ([Engine.routeWake]); the key a run names is checked
// still held last, since the listing may trail the coordinator.
func (e *Engine) beside(ctx context.Context, handle string, evs []*events.Event) (besideAnswer, error) {
	rt := e.sandbox.Load()
	if rt == nil {
		return besideAnswer{}, nil
	}
	// A RUN PARKED ON ANOTHER RUN keeps its conversation busy too (ADR-0176).
	if count := rt.coordinator.SeatCount(handle); count.Holding == 0 && count.Waiting == 0 {
		return besideAnswer{}, nil
	}
	conversation := conversationIdentityOf(evs)
	busy, err := rt.coordinator.ConversationBusy(ctx, handle, conversation)
	if err != nil {
		return besideAnswer{}, err
	}
	var runs, listed []sandbox.PendingRun
	if e.runSteers != nil {
		if listed, err = e.runSteers.store.ListActiveForSeat(ctx, handle); err != nil {
			return besideAnswer{}, fmt.Errorf("engine: reading %s's runs: %w", handle, err)
		}
		runs = routableRuns(listed)
	}
	var at besideAnswer
	if busy {
		at = besideAnswer{Key: conversation, Run: runWorking(runs, conversation), Rule: ruleConversation,
			Waits: waitingIn(listed, conversation)}
	}
	if len(runs) > 0 {
		wrote, ok := e.writtenOf(ctx, handle, runs)
		if !ok {
			return besideAnswer{}, fmt.Errorf("engine: reading what %s's runs wrote", handle)
		}
		route := e.seatRoute(handle)
		route.mu.Lock()
		e.expireFoldIns(ctx, handle, runs, route.memo)
		route.memo.prune(runs)
		read := &routeRead{wrote: wrote}
		for _, ev := range evs {
			run, related := e.relatedRun(ctx, handle, runs, ev, wrote)
			if !related {
				continue
			}
			e.routeWake(ctx, handle, runs, ev, nil, route.memo, read)
			if at.Key == "" {
				at = besideAnswer{Key: sandbox.RunKey(run.TurnID), Run: run.TurnID, Rule: ruleRun}
			}
		}
		e.fileSteers(ctx, handle, read, route.memo)
		route.mu.Unlock()
	}
	if at.Key != "" && !busy {
		held, err := rt.coordinator.KeyBusy(ctx, handle, at.Key)
		if err != nil {
			return besideAnswer{}, err
		}
		if !held {
			at = besideAnswer{}
		}
	}
	if at.Key == "" {
		e.noteStarted(ctx, handle, evs)
	}
	return at, nil
}

// relatedRun is the first of runs ev relates to: the run its requester's rule
// joins it to ([routeOf]), or one it is a triage candidate of ([candidate]),
// wrote being what each run created or wrote, by turn ([Engine.writtenOf]).
func (e *Engine) relatedRun(ctx context.Context, handle string, runs []sandbox.PendingRun,
	ev *events.Event, wrote map[string][]types.WrittenRecord) (sandbox.PendingRun, bool) {

	o := e.originOf(handle, ev)
	for _, run := range runs {
		about := wroteAbout(wrote[run.TurnID], o)
		rule, stay := routeOf(run, ev, o, false, about)
		if rule != ruleNone || candidate(stay, e.relationsOf(ctx, handle, run, o, about)) {
			return run, true
		}
	}
	return sandbox.PendingRun{}, false
}

// waitingIn reports whether a run of runs parked on what the engine answers (a
// run it waits for, or its own calls) works
// conversation, its own: one that keeps it busy while it waits (ADR-0176).
func waitingIn(runs []sandbox.PendingRun, conversation string) bool {
	return slices.ContainsFunc(runs, func(run sandbox.PendingRun) bool {
		return run.EngineAnswers() && run.ConversationKey == conversation &&
			!slices.Contains(sandbox.Holding, run.Status)
	})
}

// runWorking is the turn of the run of runs whose own conversation is
// conversation, empty where none names it (a run that served it).
func runWorking(runs []sandbox.PendingRun, conversation string) string {
	for _, run := range runs {
		if run.ConversationKey == conversation {
			return run.TurnID
		}
	}
	return ""
}

// noteStarted counts evs as starting a turn beside the seat's runs
// ([routeStarted]).
func (e *Engine) noteStarted(ctx context.Context, handle string, evs []*events.Event) {
	e.metrics.Add(metrics.SeatWakesRouted, uint64(len(evs)),
		metrics.Attrs{"outcome": string(routeStarted), "rule": string(ruleNone)})
	log.DebugContext(ctx, "seat_wake_started", "seat", handle, "wakes", len(evs),
		"detail", "related to none of the seat's running runs, it starts a turn of its own beside them")
}

// setAside is [Dispatcher.SetAside]: evs counted and said waiting aside under
// at, and each person among their writers told why, once ([Engine.tellAside]).
func (e *Engine) setAside(ctx context.Context, handle string, at besideAnswer, evs []*events.Event) {
	e.metrics.Add(metrics.SeatWakesRouted, uint64(len(evs)),
		metrics.Attrs{"outcome": string(routeWaited), "rule": string(at.Rule)})
	log.InfoContext(ctx, "seat_wake_aside", "seat", handle, "key", at.Key, "turn_id", at.Run,
		"wake_ids", wakeIDsOf(evs))
	why := runsAfterLine
	switch {
	case at.Waits:
		why = asideWaitLine
	case at.Rule == ruleConversation:
		why = asideLine
	}
	for _, ev := range evs {
		e.tellAside(ctx, handle, ev, why)
	}
}

// asideLine is why a wake of a conversation another run works waits, as its
// writer is told it beside the seat's name.
const asideLine = "is working on this conversation in another run; this runs after it"

// asideWaitLine is why a wake of a conversation whose run waits on another of
// the seat's runs waits (ADR-0176), as its writer is told it.
const asideWaitLine = "is waiting for its run here to finish the work it waits on; this runs after it"

// tellAside tells the person who wrote ev, waiting aside on handle's inbox, why
// it waits, on the conversation's queued line ([Engine.tellWaiting]'s memory):
// once, and never over a reason the routing told first (a fold-in's approver,
// a refusal), which says more of the same wait.
func (e *Engine) tellAside(ctx context.Context, handle string, ev *events.Event, why string) {
	if ev == nil {
		return
	}
	o := e.originOf(handle, ev)
	e.explainWait(ctx, handle, ev, ev.ID.String(), o.login == "" && e.seatIsHuman(o.requester), why,
		e.waitsTold.first)
}
