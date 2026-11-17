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
// answer names and puts the answer on the control topic of the seat holding it.
//
// The desk runs on whichever node served the request; the record and the resume
// run on the seat's holder, the topic's only consumer ([Coordinator.OnAnswerGiven]).
// The topic is durable, so an answer accepted while the holder restarts waits.
// Never the seat's inbox: it is held while any of the seat's executors runs and
// worked oldest first, so a busy seat's answer would wait past the run's pause
// TTL ([Coordinator.AnswerByTurn]).
//
// It holds no coordinator: a node whose company runs no sandbox still serves the
// operator surface, and the run record is the fleet's, not this node's.
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

// Held is the active run whose row holds held call id ([PendingRun.HeldCall]),
// false where none does: the run a held call's approval answers
// (`POST /mcp-approvals/{id}`, ADR-0077). It scans every active run, as the
// board does, because nothing indexes a run by its held call's id.
func (d AnswerDesk) Held(ctx context.Context, id string) (PendingRun, bool, error) {
	runs, err := d.Pending.ListActive(ctx)
	if err != nil {
		return PendingRun{}, false, fmt.Errorf("sandbox: reading the runs for held call %s: %w", id, err)
	}
	for _, run := range runs {
		if run.HeldCall != nil && run.HeldCall.ID == id {
			return run, true, nil
		}
	}
	return PendingRun{}, false, nil
}

// Answerer is who gave the answer run has recorded, as its seal names them
// ([AnswerWords.By]), for a person told their own answer came second: empty
// where the run holds none, or its seal does not open on this node — under a
// key this node does not hold, or as an answer to no question of this run's.
// The answerer lives only inside the seal (answerseal.go), so a reader that
// cannot open it says "someone else" rather than guess.
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
// It returns the delivery's id, which the run's record names among the answers
// it took or declined, so the giver can tell which became of theirs.
//
// The run's own row decides the topic and stamps the seat ([types.ExecutorAnswerGiven]
// .Agent; control topics are addressed by agent id, ADR-0013); the consumer checks
// it against the row again, dropping one misdelivered ([Coordinator.AnswerByTurn]).
// The seat is the one thing taken from this read, since a run's seat never changes:
// the question stays the one the caller's person READ, never re-stamped with the
// run's current one, or an answer would be paired with a question its giver never saw.
func (d AnswerDesk) Deliver(ctx context.Context, given types.ExecutorAnswerGiven) (uuid.UUID, error) {
	if strings.TrimSpace(given.TurnID) == "" {
		return uuid.Nil, errors.New("sandbox: an answer names no run")
	}
	if given.LaunchID == "" {
		// The seat's holder would only spend it ([Coordinator.AnswerByTurn]).
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
		// It may have landed, so the id goes back with the error. Answering again
		// is harmless (the second is spent not_awaiting, as each names its
		// question), while "refused" would send the person elsewhere.
		return ev.ID, fmt.Errorf("sandbox: delivering the answer for run %s to seat %s (%s): %w",
			given.TurnID, given.AgentHandle, run.AgentID, err)
	}
	return ev.ID, nil
}
