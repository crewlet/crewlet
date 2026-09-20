package engine

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
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

// statusClearTimeout bounds taking a released seat's indicators down.
//
// Five seconds, the same value and the same arithmetic as [memoryFlushTimeout]
// beside it on the same path: long enough for a chat instance under load to
// answer, short enough that a drain of a dozen seats against one that has
// stopped answering finishes in seconds rather than waiting out a client
// timeout per seat. What expires with it is one indicator left standing until
// the backend's own expiry lapses it — about two minutes on the surface that
// renders text, seconds on the one that does not.
const statusClearTimeout = 5 * time.Second

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
// detached run that is still working.
//
// THE KEEP-ALIVE RULE, written once because both turn frames need it and a
// second reading of it would be a second answer: an indicator survives a turn
// if and only if that turn SUSPENDED into a detached coding run. The box is
// still running, the same turn resumes when it reports, and the person who
// asked is still waiting — so taking it down would say the agent had stopped
// in the middle of the longest thing it does. Every other ending clears: done,
// failed, skipped, a guard breach, a runner that could not be built.
//
// Off [turn.Result.Suspended], which is the SAME field [Engine.persistSuspension]
// is driven by, so the indicator and the parked row can never disagree about
// whether a turn is coming back. A zero result — a turn that never reached the
// loop — is not suspended, which is what makes ending on the error paths
// correct rather than merely safe.
//
// The context is DETACHED because this is a teardown: the ending it is
// reporting is often the cancellation itself (a shed seat, a drained node, a
// turn that ran out of time), and a clear on a dead context does nothing at
// all — which leaves an indicator claiming the agent is still working.
func endWorkingStatus(ctx context.Context, s *notify.StatusSession, res turn.Result) {
	s.End(context.WithoutCancel(ctx), res.Suspended)
}
