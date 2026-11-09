package engine

import (
	"context"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/pickup"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/tracing"
)

// The work pickup's wiring (ADR-0112): every node that publishes runs one loop
// over the agent seats IT holds, and hands each idle seat its next ready item.
// What a pickup is, and when a seat is idle, is internal/pickup's; what this
// file adds is the facts only the holder has and the effects only it may take.

// pickupLoop is the running loop's handle and the picker it drives, whose
// settle remembers which seats the last pass found idle.
type pickupLoop struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}

	picker *pickup.Picker
}

// startPickups arms the loop on a node that publishes wakes. The company is
// read on every pass, so an apply that switches the pickup off or on needs no
// re-arm.
func (e *Engine) startPickups(ctx context.Context) {
	if !e.mode.Publishes() || e.node == nil || e.router == nil || e.backends == nil ||
		e.backends.Fleet == nil || e.backends.Queue == nil {
		return
	}
	e.pickup.mu.Lock()
	defer e.pickup.mu.Unlock()
	if e.pickup.cancel != nil {
		return
	}
	// DETACHED, for the reason the scheduler's loop is: stopPickups ends
	// it, so it lives as long as the engine rather than the first signal.
	loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	e.pickup.cancel, e.pickup.done = cancel, make(chan struct{})
	done := e.pickup.done
	go func() {
		defer close(done)
		ticker := time.NewTicker(pickup.Tick)
		defer ticker.Stop()
		for {
			select {
			case <-loopCtx.Done():
				return
			case <-ticker.C:
				e.pickupPass(loopCtx)
			}
		}
	}()
}

// stopPickups ends the loop and waits for a pass in flight, so no pickup is
// published into a queue the teardown is closing.
func (e *Engine) stopPickups() {
	e.pickup.mu.Lock()
	cancel, done := e.pickup.cancel, e.pickup.done
	e.pickup.cancel, e.pickup.done = nil, nil
	e.pickup.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}

// pickupPass decides every held agent seat's pickup once.
func (e *Engine) pickupPass(ctx context.Context) {
	p := e.picker()
	c := e.Company()
	// WORKLET'S OWN READY WORK: a company on a vendor tracker or none has
	// no ready work this engine can read.
	if c == nil || c.Org == nil || c.Models == nil ||
		!c.Config.Scheduling.WorkPickup.Runs() ||
		c.Config.TrackerBackendFor() != config.TrackerWorklet {
		p.Forget()
		// PARKED RUNS ARE STILL JUDGED EVERY PASS, which the pickup's own
		// idle check does where it runs: a settle once held back is retried.
		e.settleParkedRuns(ctx)
		return
	}
	var seats []pickup.Seat
	for _, seat := range e.node.Host().HeldSeats() {
		if role := c.Org.AgentSeatByHandle(seat.Handle); role != nil {
			seats = append(seats, pickup.Seat{ID: seat.ID, Handle: seat.Handle, Role: role.Name})
		}
	}
	p.Cooldown = c.Config.Scheduling.WorkPickup.Cooldown()
	for _, out := range p.Pass(ctx, seats) {
		e.notePickup(ctx, out)
	}
}

// picker is the engine's one [pickup.Picker], built on first use over the
// engine's own seams: this node's conditions, the estate router's ready-work
// read, the fleet's completion ledger and claims, and the node's wake queue.
func (e *Engine) picker() *pickup.Picker {
	e.pickup.mu.Lock()
	defer e.pickup.mu.Unlock()
	if e.pickup.picker == nil {
		e.pickup.picker = &pickup.Picker{
			Holder:    pickupHolder{e},
			Ready:     e.router.Work(),
			Ledger:    e.dispatch.Completions,
			Claims:    e.backends.Fleet,
			Publisher: e.wakes(),
			Trace:     tracing.TraceOf,
			Now:       e.now,
			Holds: func(handle string) bool {
				c := e.Company()
				return c != nil && c.Org != nil && c.Org.SeatByHandle(handle) != nil
			},
		}
	}
	return e.pickup.picker
}

// pickupHolder answers the conditions only the node holding a seat knows.
type pickupHolder struct{ e *Engine }

// Conditions reads one seat: the delivery screening's own answers, and the
// four only a pickup asks.
func (h pickupHolder) Conditions(ctx context.Context, seat pickup.Seat) pickup.Conditions {
	e := h.e
	screen := e.dispatch.Conditions(seat.Handle)
	c := pickup.Conditions{
		Owned:           screen.Owned,
		Paused:          screen.Paused,
		PauseUnknown:    screen.PauseUnknown,
		TurnEngineReady: screen.TurnEngineReady,
		Dispatching:     e.dispatch.Running(seat.Handle),
		ExecutorHolds:   screen.SeatHeldBySandbox,
		ExecutorAwaits:  screen.SandboxAwaitsAnswer,
		Parked:          e.inboxParkedHere(seat.ID),
		Shedding:        !screen.AdmitsTriggers,
		Draining:        e.shuttingDown.Load(),
	}
	awaits := c.ExecutorAwaits
	c.ExecutorAwaits = false
	if !pickup.IdleNow(c) {
		// THE CHEAP ANSWERS FIRST: a seat that is busy is not asked
		// about its budget, its provider or its inbox.
		c.ExecutorAwaits = awaits
		return c
	}
	if awaits {
		// A PARKED RUN HOLDS THE PICKUP ONLY WHILE ITS BOX WAITS PAUSED for
		// its answer; past its pause it still takes the answer when it comes.
		if c.ExecutorAwaits = e.parkedHoldsPickup(ctx, seat.Handle); c.ExecutorAwaits {
			return c
		}
	}
	// THE QUESTION A RELAUNCH ASKS BEFORE IT RUNS, asked without a
	// delivery: a budget window that would refuse the launch, a provider
	// park, or every model of the chain at a limit it reported.
	_, c.Refused = e.seatCannotRun(ctx, seat.Handle, sandbox.ResumeAnswer)
	waiting, err := e.backends.Queue.Waiting(ctx, topics.AgentInbox(seat.ID),
		topics.AgentInboxGroup(seat.ID))
	// AN INBOX THAT CANNOT BE READ IS NOT EMPTY: the pickup waits a pass
	// rather than queue work behind mail it cannot see.
	c.MailWaiting = err != nil || waiting.Total() > 0
	return c
}

// settleParkedRuns judges the parked runs of every held seat with a question
// open, settling those no build will resume.
func (e *Engine) settleParkedRuns(ctx context.Context) {
	rt := e.sandbox.Load()
	if rt == nil || e.node == nil {
		return
	}
	for _, seat := range e.node.Host().HeldSeats() {
		if _, awaits := rt.coordinator.SeatRuns(seat.Handle); !awaits {
			continue
		}
		if _, err := rt.coordinator.SettleParked(ctx, seat.Handle); err != nil {
			log.DebugContext(ctx, "work_pickup_parked_unread", "handle", seat.Handle, "error", err.Error())
		}
	}
}

// parkedHoldsPickup settles the seat's parked runs no build will resume and
// reports whether one still waits within its pause; a store that cannot say
// holds the pickup, as an inbox that cannot be read does.
func (e *Engine) parkedHoldsPickup(ctx context.Context, handle string) bool {
	rt := e.sandbox.Load()
	if rt == nil {
		return false
	}
	pausing, err := rt.coordinator.SettleParked(ctx, handle)
	if err != nil {
		log.DebugContext(ctx, "work_pickup_parked_unread", "handle", handle, "error", err.Error())
		return true
	}
	return pausing
}

// notePickup records what a pass decided for a seat it found idle: counted
// and logged, at info for a pickup published and at warn for a failure, and
// at debug for an idle seat handed nothing, which is the ordinary case. A seat
// that was not idle is logged at debug and not counted.
func (e *Engine) notePickup(ctx context.Context, out pickup.Outcome) {
	if !out.Settled() {
		log.DebugContext(ctx, "work_pickup_skipped", "handle", out.Seat.Handle,
			"reason", string(out.Skip))
		return
	}
	if out.Woken {
		e.metrics.Add(metrics.WorkPickups, 1, metrics.Attrs{"outcome": "woken"})
		log.InfoContext(ctx, "work_pickup_woken", "handle", out.Seat.Handle,
			"item", out.Item.Key, "status", string(out.Item.Status),
			"why", string(out.Item.Why), "moved", out.Item.Moved,
			"ready", out.Ready, "wake", out.Wake.String())
		return
	}
	e.metrics.Add(metrics.WorkPickups, 1, metrics.Attrs{"outcome": string(out.Skip)})
	if out.Err != nil {
		log.WarnContext(ctx, "work_pickup_skipped", "handle", out.Seat.Handle,
			"reason", string(out.Skip), "ready", out.Ready, "error", out.Err.Error())
		return
	}
	log.DebugContext(ctx, "work_pickup_skipped", "handle", out.Seat.Handle,
		"reason", string(out.Skip), "ready", out.Ready)
}
