package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
)

// AnswerDesk is where an answer BY TURN is accepted: it reads the run the
// answer names, and puts the answer on the control topic of the seat that
// holds it.
//
// TWO HALVES ON TWO NODES. The desk runs on whichever node served the person's
// request; the record and the resume run on the node holding the seat, which
// is the only consumer of that seat's control topic
// ([Coordinator.OnAnswerGiven]). The topic is what joins them, because it is
// durable: an answer accepted while the holder restarts is still there when it
// is consumed.
//
// THE CONTROL TOPIC, NEVER THE SEAT'S INBOX. The answer is addressed to a run,
// like the completion that topic was made for, and the inbox is the seat's
// mail: held for as long as any of the seat's executors holds it, and worked
// oldest first after that. An answer put there waited behind every job the
// seat took on and every message it had not got to, and on a busy seat it
// waited until the run's pause TTL took its box (see
// [Coordinator.AnswerByTurn]).
//
// It holds no coordinator, deliberately — a node whose company runs no
// sandbox still serves the operator surface, and the run record it reads is
// the fleet's rather than this node's.
type AnswerDesk struct {
	Pending PendingStore
	Queue   Publisher

	// Seal opens the answer a run already has recorded, for who gave it
	// ([AnswerDesk.Answerer]); nil names nobody.
	Seal *AnswerSeal
}

// Run reads one run by its turn id, for the check an answer is made against.
func (d AnswerDesk) Run(ctx context.Context, turnID string) (PendingRun, bool, error) {
	return d.Pending.Get(ctx, turnID)
}

// Answerer is who gave the answer run has recorded, as its seal names them
// ([AnswerWords.By]), for a person told their own answer came second: empty
// where the run holds none, or its seal does not open on this node — under a
// key this node does not hold, or as an answer to no question of this run's.
//
// FROM THE SEAL, OR NOT AT ALL. The answerer lives only inside it
// (answerseal.go), so there is no copy in the clear to read in its place, and
// a reader that cannot open it says "someone else" rather than guess.
func (d AnswerDesk) Answerer(run PendingRun) string {
	return OpenedAnswerer(d.Seal, run)
}

// OpenedAnswerer is who gave run's recorded answer, as seal opens it, or empty
// where the run holds none, seal is nil or the answer does not open — see
// [AnswerDesk.Answerer]. Each reader that names an answerer asks it here, so
// what a refusal and a board say about one answer is the same.
func OpenedAnswerer(seal *AnswerSeal, run PendingRun) string {
	if seal == nil || run.Answer == nil {
		return ""
	}
	words, err := seal.Open(run)
	if err != nil {
		return ""
	}
	return words.By
}

// Deliver puts one answer on the control topic of the seat that holds the run
// it names, refusing one that names no run or no question
// ([types.ExecutorAnswerGiven.LaunchID]) before anything is read or published.
//
// THE RUN'S OWN ROW DECIDES THE TOPIC, read here by the turn the answer names,
// and the answer is stamped with that row's seat ([types.ExecutorAnswerGiven]
// .Agent): a control topic is addressed by the seat's agent id, derived from
// its handle (ADR-0013). The coordinator that consumes it compares the same
// id against the row again ([Coordinator.AnswerByTurn]), so an answer that
// somehow lands on another seat's topic is dropped rather than resumed there.
//
// THE QUESTION IS THE CALLER'S, never this read's: the caller stamps the
// question the person READ off its own check of the row, and the desk does
// not overwrite it with the one the run waits on now. Re-stamped here, an
// answer given to a question the run has since moved past would be paired with
// a question its giver never saw — the defect the question exists to stop. The
// seat is the one thing safe to take from this read, because a run's seat
// never changes.
//
// IT ANSWERS THE DELIVERY'S ID, which is the answer's own from here on: the
// run's record names the deliveries it was given in, and a question that lets
// an answer go names it among its declined ones, so a person who gave the
// answer can tell from the run's record whether it was taken or let go of.
//
// A PUBLISH THAT ERRORED MAY HAVE LANDED, and the caller is told so rather
// than told it failed: a second answer to the same question is harmless — the
// first to be consumed resumes the run and the other is announced
// not_awaiting, because each names the question it answers
// ([types.ExecutorAnswerGiven.LaunchID]) and the run is no longer waiting on it
// whatever it waits on by then — while telling a person their answer was
// refused when it is on its way would have them give it somewhere else. A row
// that could not be read is reported the same way, for the same reason:
// answering again is safe either way.
func (d AnswerDesk) Deliver(ctx context.Context, given types.ExecutorAnswerGiven) (uuid.UUID, error) {
	if strings.TrimSpace(given.TurnID) == "" {
		return uuid.Nil, errors.New("sandbox: an answer names no run")
	}
	if given.LaunchID == "" {
		// The node holding the seat would only spend it: an answer naming
		// no question is never resumed with ([Coordinator.AnswerByTurn]).
		return uuid.Nil, fmt.Errorf("sandbox: the answer for run %s names no question it answers", given.TurnID)
	}
	run, found, err := d.Pending.Get(ctx, given.TurnID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("sandbox: reading run %s to deliver its answer: %w", given.TurnID, err)
	}
	if !found {
		return uuid.Nil, fmt.Errorf("sandbox: no run is parked under turn %s to deliver an answer to",
			given.TurnID)
	}
	control := controlSubject(run.AgentID)
	if control == "" {
		return uuid.Nil, fmt.Errorf("sandbox: run %s names no seat id (%q) to deliver its answer to",
			given.TurnID, run.AgentID)
	}
	given.Agent = run.AgentID
	if given.AgentHandle == "" {
		given.AgentHandle = run.AgentHandle
	}
	ev := events.New(given, events.TraceContext{})
	ev.Source = types.OperatorSource
	if err := d.Queue.Publish(ctx, control, ev); err != nil {
		return ev.ID, fmt.Errorf("sandbox: delivering the answer for run %s to seat %s (%s): %w",
			given.TurnID, given.AgentHandle, run.AgentID, err)
	}
	return ev.ID, nil
}
