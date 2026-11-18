package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/notify"
)

// The working indicator ("Agent SWE is thinking…") is raised here because it
// spans a whole turn, up before the turn assembles anything and down when it is
// over, and only [Engine.runTurn] and its resume twin own that lifetime: the
// turn loop sees rounds, a phase one conversation, a tool one call.
//
// Whether an indicator shows is the driver's (off / addressed / always), and
// [notify.Statuses] fans a raise across every chat surface, so no backend is
// named here; the engine supplies only WHICH turn, WHAT woke it, and WHEN it
// ended. Teardowns hand the driver the caller's own context, cancelled or not:
// the driver detaches and bounds each clear and waits out a post in flight
// ([notify.StatusSession.End]), so no call site here bounds it again.

// chatMetadataOf is the metadata of the chat message that woke a turn, or nil.
//
// The first notification's FLAT metadata wins, as in [threadOf]: a coalesced
// event's flat fields mirror its latest constituent, and a chat partition is
// thread-grained, so a burst never straddles two conversations.
//
// Nil for every non-chat trigger (a schedule tick, an A2A ask, a sandbox
// completion, a work-item assignment): their events stamp no transport, every
// driver refuses nil, and nobody is watching a composer for them.
func chatMetadataOf(evs []*events.Event) map[string]string {
	for _, n := range notificationsIn(evs) {
		// The transport stamp rather than a channel: a code host's and a
		// tracker's notifications carry metadata too, and none of it
		// addresses a conversation any chat backend can raise a status in.
		if n.Metadata[notify.TransportField] != "" {
			return n.Metadata
		}
	}
	return nil
}

// beginWorkingStatus raises the indicator for a turn, or reports a nil session
// whose methods are all no-ops.
//
// It opens on EXECUTE rather than the default pool because every turn's first
// phase is the executor's (the exception, a first turn's onboarding pass,
// announces itself via [RunnerInput.OnPhase]), so the phase seam's first report needs no second
// call. The `default` pool keeps its documented meaning: a phase with no pool
// of its own.
func (e *Engine) beginWorkingStatus(ctx context.Context, handle, turnID string,
	trigger []*events.Event,
) *notify.StatusSession {
	return e.Status().Begin(ctx, handle, turnID, phase.Execute.String(),
		chatMetadataOf(trigger))
}

// endWorkingStatus takes a turn's indicator down, or leaves it up for the
// work that is still going.
//
// THE KEEP-ALIVE RULE, stated once for both turn frames: an indicator survives
// a turn only while a detached executor this turn is coming back from is still
// working; every other ending clears. keepAlive is that FACT, from
// [stillWorking], never the intent [turn.Result.Suspended]: the run's row is
// written after the result and can fail, and an intent-driven indicator would
// outlive every failed persist for the life of the process.
//
// ctx is the turn's own, cancelled or not: the driver detaches and bounds the
// clear itself.
func endWorkingStatus(ctx context.Context, s *notify.StatusSession, keepAlive bool) {
	s.End(ctx, keepAlive)
}

// stillWorking reads [Engine.persistSuspension]'s answer as the one question
// the indicator asks: is anything going to come back?
//
// A row that landed is a run the completion poll resumes: working. A row that
// could not be written means the run was settled and reclaimed meanwhile: the
// indicator comes down. An unknown counts as working, as internal/coord reads
// an unreachable store: the write may have landed, and reading it as loss
// would drop the indicator on a store blip mid-run, where reading it as work
// leaves at worst one indicator up until the seat moves or the node stops.
func stillWorking(resumable bool, err error) bool {
	return resumable || err != nil
}

// releaseWorkingStatus drops one turn's hold because the agent has STOPPED and
// the turn is not coming back to say so.
//
// A detached run stops in several ways (parked on a question, destroyed with
// its box reclaimed, left holding a claim it could not give back), all one
// fact to the indicator, so the sandbox layer reports them through one seam,
// [sandbox.CoordinatorOptions.Stopped]: a seam per reason is one a wiring can
// forget to pass. The keep-alive rule's counterpart: without it the indicator
// says "is thinking…" to the one person who could answer.
//
// Idempotent, so the seam may over-report; it releases one turn's hold, not
// the seat's indicators ([notify.Statuses.Release]).
func (e *Engine) releaseWorkingStatus(ctx context.Context, handle, turnID string) {
	e.Status().Release(ctx, handle, turnID)
}

// resumeWorkingStatus is the indicator a RESUMED turn shows, and which of the
// two routes it came up on, tried in this order:
//
//   - A BOX'S COMPLETION: the suspension's hold is still up under this turn id,
//     so it is taken back rather than raised again; a parked run's row carries
//     no chat metadata to raise from.
//   - A PERSON'S ANSWER to a parked run: that hold was released at the park,
//     and the answering chat message carries the conversation to raise in.
//
// Nil where neither holds (the seat moved, the process restarted, or the
// trigger is not chat). rejoined is the route the caller's retry rule turns on;
// re-deriving it from the run's row would be a second answer that could drift.
func (e *Engine) resumeWorkingStatus(ctx context.Context, handle, turnID string,
	trigger *events.Event,
) (session *notify.StatusSession, rejoined bool) {
	if kept := e.Status().Rejoin(handle, turnID); kept != nil {
		return kept, true
	}
	return e.beginWorkingStatus(ctx, handle, turnID, []*events.Event{trigger}), false
}
