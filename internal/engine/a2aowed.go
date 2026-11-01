package engine

import (
	"context"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/a2a"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// WHETHER A COLLEAGUE'S ASK IS STILL OWED ITS ANSWER — the idle sweep's question
// ([a2a.Owed]).
//
// The sweep closes an agent-to-agent channel that has been quiet past its
// timeout, and a closed channel refuses the reply that arrives later
// ([a2a.ErrClosed]): the asker, whose turn ended when it asked, is never woken.
// That was safe while a turn was bounded by a clock the timeout could be sized
// to. It is not any more: every agent seat's turn parks on its executor's run,
// a run has no clock bound and can wait days on a person's question, and while
// one of a seat's runs works its inbox is held and every ask sent to it waits
// there. So the sweep asks this first, and spares the channel where the seat it
// asked still holds the ask somewhere it will take it up from:
//
//   - ON A RUN'S RECORD. The launch writes the ask the turn took up onto its
//     run's record ([sandbox.PendingRun.ColleagueAsk]), and whatever ends that
//     turn answers from there — its resume, a person's stop, or the run's loss
//     (a2a.go). For as long as the record exists, an answer is coming.
//   - IN ITS INBOX, HELD BEHIND A RUN. A seat busy with one of its runs takes no
//     other work (sandbox's seathold.go): the ask waits on the broker, in order,
//     until the run settles or parks on its own question.
//   - IN ITS INBOX, HELD BY A PERSON'S PAUSE, until somebody resumes the seat
//     (seatpause.go).
//   - IN ITS INBOX, PARKED ON ITS BUDGET, until the refusing window turns over
//     or a revision raises its ceiling (budgetpark.go).
//
// None of those has an end the timeout could be sized to, and each is a fact
// this engine can read on any node: the run records are the fleet's, every node
// watches the pauses, and the budget counters are shared. What is left to the
// timeout is an ask in flight nowhere any of them can see — a dispatch segment
// before its launch writes the run's record, a delivery a node that died held
// unacknowledged — which is what internal/maintenance's ChannelIdleTimeout is
// sized to.
//
// A SEAT THE RUNNING COMPANY DOES NOT HAVE answers from its run records alone:
// nobody can have paused it or parked it here, and its mailbox is retired with
// it, so an ask in it is answered by nothing. And a node that runs no company
// judges the same way, as the dispatcher's own pause reading does
// ([Engine.pauseOfHandle]).

// colleagueOwed is the engine's [a2a.Owed].
type colleagueOwed struct {
	engine *Engine
	runs   seatRunLister
}

// seatRunLister is the one read colleagueOwed makes of the fleet's executor
// records: a seat's unfinished runs, by the handle a channel names its target
// by. [sandbox.CoordStore] is the implementation.
type seatRunLister interface {
	ListActiveForSeat(ctx context.Context, handle string) ([]sandbox.PendingRun, error)
}

var _ a2a.Owed = colleagueOwed{}

// AnswerOwed reports whether the seat ch asked still holds its ask somewhere it
// will take it up from. Three-valued: an error is a read that could not be
// made, which the sweep spares the channel on.
func (o colleagueOwed) AnswerOwed(ctx context.Context, ch a2a.Channel) (bool, error) {
	runs, err := o.runs.ListActiveForSeat(ctx, ch.Target)
	if err != nil {
		return false, fmt.Errorf("engine: reading the executor records of %s, which channel %s asked: %w",
			ch.Target, ch.ID, err)
	}
	for _, run := range runs {
		if run.ColleagueAsk != nil && run.ColleagueAsk.ChannelID == ch.ID {
			return true, nil
		}
	}
	if slices.ContainsFunc(runs, holdsInbox) {
		return true, nil
	}
	paused, known := o.engine.pauseOfHandle(ch.Target)
	if !known {
		return false, fmt.Errorf("engine: whether %s, which channel %s asked, is paused: %w",
			ch.Target, ch.ID, errPausesUnread)
	}
	if paused {
		return true, nil
	}
	parked, err := o.engine.budgetParked(ctx, o.engine.Company(), ch.Target)
	if err != nil {
		return false, fmt.Errorf("engine: whether %s, which channel %s asked, is parked on its "+
			"budget: %w", ch.Target, ch.ID, err)
	}
	return parked, nil
}

// holdsInbox reports whether a run keeps its seat's inbox held: one the engine
// is driving ([sandbox.Holding]), or one owed the resume of an answer a person
// gave it — the two conditions the coordinator holds a seat's inbox for
// (sandbox's seathold.go).
func holdsInbox(run sandbox.PendingRun) bool {
	return slices.Contains(sandbox.Holding, run.Status) || run.Status == sandbox.StatusAnswered
}

// budgetParked reports whether handle's seat's mail waits on its budget — the
// conditions the budget stage parks it on ([meter.parks]). A seat with no
// counter, or nothing capped, is never parked.
func (e *Engine) budgetParked(ctx context.Context, c *Company, handle string) (bool, error) {
	m := e.meterFor(c, handle)
	if m == nil || !m.basis.capped() {
		return false, nil
	}
	_, refusing, starved, err := m.parks(ctx)
	if err != nil {
		return false, err
	}
	return refusing || starved, nil
}
