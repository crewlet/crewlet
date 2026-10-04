package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// AnswerDesk is where an answer BY TURN is accepted: it reads the run the
// answer names, and puts the answer on the inbox of the seat that holds it.
//
// TWO HALVES ON TWO NODES. The desk runs on whichever node served the person's
// request; the resume runs on the node holding the seat, which is the only
// consumer of that seat's inbox ([Coordinator.AnswerByTurn]). The inbox is
// what joins them, because it is durable: an answer accepted while the holder
// restarts, or while the seat is paused, is still there when it is consumed.
//
// It holds no coordinator, deliberately — a node whose company runs no
// sandbox still serves the operator surface, and the run record it reads is
// the fleet's rather than this node's.
type AnswerDesk struct {
	Pending PendingStore
	Queue   Publisher
}

// Run reads one run by its turn id, for the check an answer is made against.
func (d AnswerDesk) Run(ctx context.Context, turnID string) (PendingRun, bool, error) {
	return d.Pending.Get(ctx, turnID)
}

// Deliver puts one answer on the inbox of the seat that holds the run it
// names.
//
// THE RUN'S OWN ROW DECIDES THE INBOX, read here by the turn the answer names,
// and the answer is stamped with that row's seat ([types.SandboxAnswerGiven]
// .Agent): an inbox is addressed by the seat's agent id, derived from its
// handle (ADR-0013). The dispatcher that consumes it compares the same ID against
// the row again ([Coordinator.AnswerByTurn]), so an answer that somehow lands
// on another seat's inbox is dropped rather than resumed there.
//
// A PUBLISH THAT ERRORED MAY HAVE LANDED, and the caller is told so rather
// than told it failed: a second answer to the same run is harmless — the first
// to be consumed resumes it and the other is announced not_awaiting — while
// telling a person their answer was refused when it is on its way would have
// them give it somewhere else. A row that could not be read is reported the
// same way, for the same reason: answering again is safe either way.
func (d AnswerDesk) Deliver(ctx context.Context, given types.SandboxAnswerGiven) error {
	if strings.TrimSpace(given.TurnID) == "" {
		return errors.New("sandbox: an answer names no run")
	}
	run, found, err := d.Pending.Get(ctx, given.TurnID)
	if err != nil {
		return fmt.Errorf("sandbox: reading run %s to deliver its answer: %w", given.TurnID, err)
	}
	if !found {
		return fmt.Errorf("sandbox: no run is parked under turn %s to deliver an answer to",
			given.TurnID)
	}
	seat, err := uuid.Parse(run.AgentID)
	if err != nil || seat == uuid.Nil {
		return fmt.Errorf("sandbox: run %s names no seat id (%q) to deliver its answer to",
			given.TurnID, run.AgentID)
	}
	given.Agent = seat.String()
	if given.AgentHandle == "" {
		given.AgentHandle = run.AgentHandle
	}
	ev := events.New(given, events.TraceContext{})
	ev.Source = types.OperatorSource
	if err := d.Queue.Publish(ctx, topics.AgentInbox(seat), ev); err != nil {
		return fmt.Errorf("sandbox: delivering the answer for run %s to seat %s (%s): %w",
			given.TurnID, given.AgentHandle, seat, err)
	}
	return nil
}
