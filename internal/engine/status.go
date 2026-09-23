package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/notify"
)

// The working indicator, raised where a turn's lifetime is owned.
//
// # Why the engine, and only the engine, can raise it
//
// "Agent SWE is thinking…" spans a whole turn: it goes up before the turn
// assembles anything and comes down when the turn is over, whatever the turn
// concluded. That span belongs to exactly one frame — [Engine.runTurn] and its
// resume twin — and nothing below it has the same lifetime: the turn loop sees
// rounds, a phase sees one provider conversation, and a tool sees one call.
//
// # What this file deliberately does NOT decide
//
// Whether an indicator shows at all. The driver owns off / addressed / always
// and refuses a trigger whose transport is not its own, and [notify.Statuses]
// fans a raise across every chat surface this node runs — so there is no
// backend name anywhere in this package's status path, and a company that runs
// two chat surfaces needs no second wiring. What the engine contributes is the
// three facts only it holds: WHICH turn, WHAT woke it, and WHEN it ended.
//
// Nor how a teardown's requests are made. Every teardown here hands the
// driver the caller's OWN context, a cancelled one included: the driver
// detaches and bounds each clear itself and waits out the post in flight
// before it, because it is the only frame that knows a request is in flight
// at all (see [notify.StatusSession.End]). A copy of that rule at each call
// site here was a second answer to one question, and it bounded the wrong
// span.

// chatMetadataOf is the metadata of the chat message that woke a turn, or nil.
//
// FIRST WINS, over the FLAT metadata of each notification rather than over the
// constituents — the same reading [threadOf] takes, and for the same reason: a
// coalesced event's flat fields mirror its latest constituent, and a chat
// partition is thread-grained wherever a thread exists, so a coalesced burst
// can never straddle two conversations.
//
// A TURN WITH NO CHAT METADATA CORRECTLY RAISES NOTHING, and that is the whole
// of the non-chat story: a schedule tick, an A2A ask, a sandbox completion and
// a work-item assignment are woken by events that stamp no transport, so this
// answers nil and every driver refuses nil. There is nobody watching a
// composer for any of them — the person who would see an indicator is the
// person who typed into a chat thread, and for these triggers there is no such
// person and no such thread.
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
// The opening phase is EXECUTE rather than the default pool, and that is a
// request saved on every ordinary turn: every turn's first phase is the
// executor's — a first-ever turn's onboarding pass is the one exception, and it
// announces itself through [RunnerInput.OnPhase] like any other phase — so
// opening on it means the first thing the phase seam reports is a phase the
// indicator is already showing, which costs nothing rather than a second call
// to say what the first one said. The `default` pool keeps the meaning its
// config field documents: a phase added later with no pool of its own.
func (e *Engine) beginWorkingStatus(ctx context.Context, handle, turnID string,
	trigger []*events.Event,
) *notify.StatusSession {
	return e.Status().Begin(ctx, handle, turnID, phase.Execute.String(),
		chatMetadataOf(trigger))
}

// endWorkingStatus takes a turn's indicator down, or leaves it up for the
// work that is still going.
//
// THE KEEP-ALIVE RULE, written once because both turn frames need it and a
// second reading of it would be a second answer: an indicator survives a turn
// if and only if SOMETHING IS STILL WORKING behind it — a detached coding run
// this turn is actually coming back from. Every other ending clears: done,
// failed, skipped, a guard breach, a runner that could not be built.
//
// keepAlive is a FACT each frame establishes, never the turn's intent to
// suspend. [turn.Result.Suspended] says the executor asked to park; whether
// anything can ever resume it is the run's ROW, and the row is written after
// the result exists and can fail on its own — see [stillWorking]. Driven off
// the intent, an indicator outlived every failed persist for the life of the
// process: the turn never came back, and nothing but a seat handoff or a
// shutdown would have taken it down.
//
// ctx is the turn's own, cancelled or not: the driver detaches and bounds the
// clear itself.
func endWorkingStatus(ctx context.Context, s *notify.StatusSession, keepAlive bool) {
	s.End(ctx, keepAlive)
}

// stillWorking reads [Engine.persistSuspension]'s answer as the one question
// the indicator asks: is anything going to come back?
//
// THREE ANSWERS, TWO OF WHICH KEEP THE INDICATOR UP. A row that landed is a
// run the completion poll will resume, so the box is still working. A row that
// could not be written is a run that was settled and reclaimed while the turn
// was still here, so nothing is working and the indicator must come down —
// that is the whole of what this function exists to distinguish.
//
// AN UNKNOWN COUNTS AS WORKING, which is the same reading internal/coord takes
// of an unreachable store and for the same reason: the write that could not be
// confirmed may well have landed, and a run it moved to running is one the
// completion poll resumes. Read as loss, an indicator would come down on a
// two-second store blip in the middle of the longest thing an agent does,
// where reading it as work leaves at worst one indicator standing until this
// node hands the seat on or stops.
func stillWorking(resumable bool, err error) bool {
	return resumable || err != nil
}

// releaseWorkingStatus drops one turn's hold because the agent has STOPPED and
// the turn is not coming back to say so.
//
// ONE CALLER, ONE FACT, and the one is deliberate. The detached run a turn
// suspended into can stop in several ways — parked on a question and waiting
// for a person, destroyed with its box reclaimed, or left holding a claim its
// node could not give back — and they are different things to the RUN and the
// same one to the indicator, because in every one of them the frame that
// raised it has already returned and nothing below it holds the turn any more.
// So the sandbox layer reports them through a single seam
// ([sandbox.CoordinatorOptions.Stopped]) and this is what sits behind it: a
// second entry point per reason is a second thing a wiring can declare and
// never pass, which is exactly how three earlier rounds each left one way of
// stopping unreported.
//
// The counterpart of the keep-alive above, and the reason that rule needs one:
// a suspended turn's indicator is kept up because a box is working, and a stop
// is the moment the engine learns it no longer is. Nothing else can say so —
// the turn does not return, and on the park side a person can take days — so
// without this the indicator says "is thinking…" at the one person who could
// answer, for as long as this process lives.
//
// IDEMPOTENT BY CONSTRUCTION, which is what lets the seam over-report rather
// than decide: a hold this node does not have answers a nil session whose End
// is a no-op, so a stop reported for a turn that already ended its own hold —
// every ordinary collected run — costs one map lookup. See
// [notify.Statuses.Release].
//
// It is one turn's HOLD, not the seat's indicators: a second turn in the same
// thread keeps its own. See [notify.Statuses.Release].
func (e *Engine) releaseWorkingStatus(ctx context.Context, handle, turnID string) {
	e.Status().Release(ctx, handle, turnID)
}

// resumeWorkingStatus is the indicator a RESUMED turn shows, and which of the
// two routes it came up on.
//
// TWO SOURCES, tried in this order, because a resume arrives by two routes and
// only one of them has an indicator still standing:
//
//   - A BOX'S COMPLETION resumes the turn its suspension kept alive. The hold
//     is already up and heartbeating under the same turn id, so this takes it
//     back rather than raising a second one over it — and a resume has nothing
//     to raise FROM on that route: a parked run's row deliberately carries no
//     chat metadata, and the conversation keys it does carry are partitions
//     rather than addresses in a channel.
//   - A PERSON'S ANSWER resumes a run that parked on a question. That hold was
//     released at the park, because the agent had stopped; the answer is an
//     ordinary chat message, so the trigger that woke this resume carries the
//     conversation to raise in — and the person who just answered is the one
//     waiting on the composer.
//
// Nil where neither holds: a completion whose session is not on this node (the
// seat moved while the box ran, or this process restarted) raises nothing, and
// a trigger that is not a chat message has no conversation to raise in.
//
// REJOINED IS THE ROUTE, reported rather than re-derived, and it is what the
// caller's retry rule turns on: a resume that never reaches its turn reverts
// the coordinator's claim to the status it was claimed FROM, which is a live
// box on the first route and the same person's answer still pending on the
// second. A caller asking the run's row that question instead would be a
// second way of telling the routes apart, and the two would eventually
// disagree about which indicator is a lie.
func (e *Engine) resumeWorkingStatus(ctx context.Context, handle, turnID string,
	trigger *events.Event,
) (session *notify.StatusSession, rejoined bool) {
	if kept := e.Status().Rejoin(handle, turnID); kept != nil {
		return kept, true
	}
	return e.beginWorkingStatus(ctx, handle, turnID, []*events.Event{trigger}), false
}
