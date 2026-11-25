package sandbox

import (
	"context"
	"errors"
	"sync"
	"time"
)

// detached is the context a teardown runs under: A CONTEXT OF ITS OWN, like
// [abandon] and [Manager.discard], since a teardown follows a failure
// (settleFailed, resumeAndSettle) or a drain-cancelled handler, so the inherited
// context is often dead, making the kill and store write no-ops: a box held with
// nothing to collect it, and Kill's wait for the process group is what stops
// removeBox racing the dying wrapper's writes. WithoutCancel, not Background, so warnings keep the turn's values, and
// through [detach] a teardown by the coordinator's stopped work still ends within
// the stop's grace.
func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	free, release := detach(ctx)
	bounded, cancel := context.WithTimeout(free, discardGrace)
	return bounded, func() {
		cancel()
		release()
	}
}

// detach is ctx with its values and NONE OF ITS CANCELLATION, the context a
// teardown, a failure path's record, or any call outliving its asking frame runs
// under ([detached], [Coordinator.detachedRecord], [detachedEnding],
// [Coordinator.settleContext]): the frame's context is often dead. EXCEPT THE STOP'S
// ONE DEADLINE, which ctx carries for the coordinator's own work ([stopBoundKey],
// [Coordinator.Stop]): such a call ends with that grace whenever it began, cut with
// [errStopGrace]. THE ONE DETACHMENT A STEP OF THE PACKAGE MAKES: context.WithoutCancel
// dropped the stop's deadline, and a stopped retry's teardowns held the stop a grace
// apiece. What keeps its own WithoutCancel is no such step: a box backend's rollback
// inside its call (a refreshed login's write-back, a failed mount proof's container),
// the taker lease's give-back after the stop's waits ([takerLease.release]), a
// waiter's row write with no lifetime ([Waiter.rowContext]).
func detach(ctx context.Context) (context.Context, context.CancelFunc) {
	free := context.WithoutCancel(ctx)
	last, ok := ctx.Value(stopBoundKey{}).(context.Context)
	if !ok {
		return free, func() {}
	}
	held, cut := context.WithCancelCause(free)
	if last.Err() != nil {
		cut(context.Cause(last))
		return held, func() { cut(context.Canceled) }
	}
	unhook := context.AfterFunc(last, func() { cut(context.Cause(last)) })
	return held, func() {
		unhook()
		cut(context.Canceled)
	}
}

// stopBoundKey is the key under which a coordinator's lifetime ([Coordinator.life])
// carries the context ending one teardown's grace after its stop
// ([Coordinator.lastCalls]), which every call its work makes once stopped is held
// to, detached or not ([detach]).
type stopBoundKey struct{}

// detachedRecord is the context a teardown's write of a run's row runs under: its
// own, for [detached]'s reason, within the teardown's grace plus the row's
// crossing time ([Coordinator.rowContext]). The grace alone is a box call's, and
// a megabyte row on a slow link outlasts it mid-crossing, leaving the claim
// unreleased, the box's release unwritten and the turn's end unowed
// (crewlet/crewlet#1076). Box calls keep [detached]; an ending's steps take a
// context each ([detachedEnding]).
func (c *Coordinator) detachedRecord(ctx context.Context) (context.Context, context.CancelFunc) {
	free, release := detach(ctx)
	record, cancel := c.rowContext(free, discardGrace)
	return record, func() {
		cancel()
		release()
	}
}

// detachedEnding is the context an ending makes ([Coordinator.endRecord]) runs
// under: ctx's values, none of its cancellation, NO DEADLINE OF ITS OWN. Each store
// call on the row has its own ([Coordinator.rowContext]: grace plus crossing, afresh
// per call), a box call [detached], other steps (a publish, the engine's account)
// what makes them ([Coordinator.retryOwed]). ONE BOUND FOR THE WHOLE ENDING gave the
// row its crossing once, yet an ending writes it several times (decision, let-go,
// cleared copies, owed end, delete) reading and sending it whole: at the slowest link
// a row at its bound takes four minutes a read and three a send, so the second write
// outlasted the bound and the ending was cut between two. An ending by the
// coordinator's stopped work keeps the stop's one deadline ([detach]).
func detachedEnding(ctx context.Context) (context.Context, context.CancelFunc) {
	return detach(ctx)
}

// rowContext is the context ONE store call on a run's row runs under, within ctx:
// its reason (a store's slow moment, an outlasted election, a teardown's grace) plus
// the row's crossing time at its bound ([RowBudget]). HELD TO ITS REASON ALONE ONCE
// THIS COORDINATOR STOPS ([Coordinator.Stop]): the stop waits for cut keeps and
// running retries, and a crossing in each write held a node's stop minutes past its
// orchestrator's grace (sized counting each write at its reason). A call by the
// coordinator's OWN work is also held to the stop's one grace wherever it began
// ([detach]). A write so cut is left undone as a keep is: a begun stamp is settled by
// a peer once this process's taker lease is gone, an ending or claim finished by the
// next holder.
func (c *Coordinator) rowContext(ctx context.Context, reason time.Duration) (context.Context, context.CancelFunc) {
	return c.stopHeld(ctx, reason, RowBudget(c.pending, reason))
}

// rowReadContext is [Coordinator.rowContext] for one READ of a run's row: its
// reason plus the store's reply's crossing alone ([RowReadBudget]), never a
// change's.
func (c *Coordinator) rowReadContext(ctx context.Context, reason time.Duration) (context.Context, context.CancelFunc) {
	return c.stopHeld(ctx, reason, RowReadBudget(c.pending, reason))
}

// stopHeld is the context of one store call on a run's row given budget within
// ctx, and held to reason alone once this coordinator stops
// ([Coordinator.rowContext]).
func (c *Coordinator) stopHeld(ctx context.Context, reason, budget time.Duration) (context.Context,
	context.CancelFunc) {

	row, cancel := context.WithTimeout(ctx, budget)
	held, cut := context.WithCancelCause(row)
	var mu sync.Mutex
	var grace *time.Timer
	ended := false
	stopping := context.AfterFunc(c.life, func() {
		mu.Lock()
		defer mu.Unlock()
		if !ended {
			grace = time.AfterFunc(reason, func() { cut(errStopGrace) })
		}
	})
	return held, func() {
		stopping()
		mu.Lock()
		ended = true
		if grace != nil {
			grace.Stop()
		}
		mu.Unlock()
		cut(context.Canceled)
		cancel()
	}
}

// errStopGrace is a store call on a run's row a stopping coordinator cut at its
// reason ([Coordinator.rowContext]).
var errStopGrace = errors.New("sandbox: the node stopped, and the write was held to its reason alone")
