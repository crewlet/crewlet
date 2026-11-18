package engine

import (
	"context"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/a2a"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// Whether a colleague's ask is still owed its answer: the idle sweep's question
// ([a2a.Owed]). A swept channel refuses the later reply ([a2a.ErrClosed]), and a
// parked run has no clock bound, so the sweep spares a channel whose seat holds
// the ask: on a run's record ([sandbox.PendingRun.ColleagueAsk]; whatever ends
// the turn answers from it, a2a.go), or in its inbox held behind a run
// (sandbox's seathold.go), by a person's pause (seatpause.go), on its budget
// (budgetpark.go) or by any park of the seat's holder, the models' limit
// (providerpark.go) included, asked of the holder (parkheld.go). The timeout
// (maintenance's ChannelIdleTimeout) covers only an ask none of them sees:
// before the launch writes the run's record, or a dead node's unacked delivery.
// A seat the running company lacks answers from its run records alone (its
// mailbox retired), as a node with no company does ([Engine.pauseOfHandle]).

// colleagueOwed is the engine's [a2a.Owed]. parks is whether a park of the
// node holding a seat holds its inbox ([inboxParks]); nil asks the engine's
// own ([Engine.heldParks]), built at each question, so the incarnation it asks
// as is the node's once it has one.
type colleagueOwed struct {
	engine *Engine
	runs   seatRunLister
	parks  inboxParks
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
	if parked {
		return true, nil
	}
	return o.heldByPark(ctx, ch)
}

// heldByPark reports whether a park of the node holding the seat ch asked holds
// the seat's inbox, asked of that node (parkheld.go). A seat the running
// company does not have is parked by nobody: no node dispatches its mail.
func (o colleagueOwed) heldByPark(ctx context.Context, ch a2a.Channel) (bool, error) {
	c := o.engine.Company()
	if c == nil || c.Org == nil {
		return false, nil
	}
	seat, ok := c.Org.AgentIDFor(c.Org.AgentSeatByHandle(ch.Target))
	if !ok {
		return false, nil
	}
	parks := o.parks
	if parks == nil {
		parks = o.engine.heldParks()
	}
	held, err := parks.parked(ctx, seat)
	if err != nil {
		return false, fmt.Errorf("engine: whether a park holds the inbox of %s, which channel %s asked: %w",
			ch.Target, ch.ID, err)
	}
	return held, nil
}

// holdsInbox reports whether a run keeps its seat's inbox held: one the engine
// is driving ([sandbox.Holding]), or one owed the resume of an answer a person
// gave it — the two conditions the coordinator holds a seat's inbox for
// (sandbox's seathold.go).
func holdsInbox(run sandbox.PendingRun) bool {
	return slices.Contains(sandbox.Holding, run.Status) || run.Status == sandbox.StatusAnswered
}

// budgetParked reports whether handle's seat's mail waits on its budget: the
// conditions the budget stage parks it on ([meter.parks]) at the static floor
// ([floorOf]), where a delivery no launch has been refused for yet parks. A
// seat with no counter, or nothing capped, is never parked.
//
// A recount of the shared counters, so any node answers alike, even before a
// restarted or moved seat's next delivery parks it. A park on a refused
// launch's own floor (its brief, its continued session's context) is known
// only to the node that parked it, so [colleagueOwed.heldByPark] asks that node.
func (e *Engine) budgetParked(ctx context.Context, c *Company, handle string) (bool, error) {
	m := e.meterFor(c, handle)
	if m == nil || !m.basis.capped() {
		return false, nil
	}
	_, refusing, starved, err := m.parks(ctx, launchFloor{static: m.basis.floor})
	if err != nil {
		return false, err
	}
	return refusing || starved, nil
}
