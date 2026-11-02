package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// THE EXECUTOR'S SESSIONS, KEPT, AND EVERY TURN'S END RECORDED (ADR-0073), as
// the engine wires them into the sandbox runtime (internal/sandbox's
// session.go has the mechanism).
//
// # Through the router, wherever the seat runs
//
// A seat's box is wherever its seat is held, a seats-only node included, and
// only a data node publishes. So the coordinator records every capture and every
// end through the ESTATE ROUTER's writes ([Engine.executorLog]) — never a
// data node's own writer, which a seats-only node does not have — and they are
// the engine's alone: no package serving a person or a model reaches them
// (internal/estate's gate).
//
// # What the engine adds
//
// The company's word on keeping sessions, off the revision the FLEET runs
// ([executorKeep.Policy]); the keyring the secrets a launch handed its box are
// sealed under on its run's row, for its capture's scrub
// ([executorKeep.SealHanded]); the instant a turn's operations are minted at,
// the rule every operation id of a turn follows ([executorKeep.MintedAt]); the
// sealed spool and upload ([executorKeep.Seal], [executorKeep.Upload]); and, at
// every way a turn ends, the account of how it ended ([Engine.oweExecutorEnd])
// — the turn's decision, whether its reply reached the party waiting, who
// stopped it, what lost it.

// executorKeep is the engine's part of keeping a run's harness session
// ([sandbox.SessionKeep]).
type executorKeep struct{ engine *Engine }

var _ sandbox.SessionKeep = executorKeep{}

// Policy is the company's word on keeping sessions, off the revision the fleet's
// activation pointer names ([Engine.executorPolicy]). A node with no object
// store keeps nothing, whatever the company says: there is nowhere to keep a
// session, and a capture refused for it on every launch would say only that.
func (k executorKeep) Policy(ctx context.Context) (sandbox.SessionPolicy, error) {
	if k.engine.Objects() == nil {
		return sandbox.SessionPolicy{}, nil
	}
	return k.engine.executorPolicy(ctx)
}

// executorPolicy is `turn_engine.conversation_session`'s switch and per-seat
// budget, as the revision the fleet's activation pointer names states them
// ([Engine.fleetCompany]), or why this node cannot know them.
//
// THE FLEET'S REVISION, NEVER THIS NODE'S OWN, for [Engine.executorRetention]'s
// reason: the budget a capture carries decides which ended sessions every node
// releases, so a node serving a revision the fleet did not take must not decide
// it. A capture that cannot read it is refused, and its stamp says why.
func (e *Engine) executorPolicy(ctx context.Context) (sandbox.SessionPolicy, error) {
	company, err := e.fleetCompany(ctx, e.Company())
	if err != nil {
		return sandbox.SessionPolicy{}, err
	}
	if company.Config == nil {
		return sandbox.SessionPolicy{Keep: true, SeatBudget: config.DefaultSessionBytesPerSeat}, nil
	}
	session := company.Config.TurnEngine.ConversationSession
	return sandbox.SessionPolicy{Keep: session.KeepsSessions(), SeatBudget: session.SessionBudget()}, nil
}

// handedAAD is what the secrets a launch handed its box are sealed bound to:
// the turn whose row keeps them, so a seal copied onto another run's row does
// not open there, and its values scrub nothing but the session they were
// handed with.
func handedAAD(turnID string) string { return "sandbox/handed/" + turnID }

// SealHanded seals the secrets a launch handed its box for its run's row
// ([sandbox.PendingRun.Handed]) under the node's keyring — the coordination
// store is not encrypted — bound to the turn ([handedAAD]).
func (k executorKeep) SealHanded(turnID string, handed []byte) (string, error) {
	cipher := k.engine.Cipher()
	if cipher == nil {
		return "", errors.New("this node has no keyring to seal them under")
	}
	return cipher.Encrypt(string(handed), handedAAD(turnID))
}

// OpenHanded opens what [executorKeep.SealHanded] sealed, on whichever node
// captures the run's box.
func (k executorKeep) OpenHanded(turnID, sealed string) ([]byte, error) {
	cipher := k.engine.Cipher()
	if cipher == nil {
		return nil, errors.New("this node has no keyring to open them with")
	}
	plain, err := cipher.Decrypt(sealed, handedAAD(turnID))
	if err != nil {
		return nil, err
	}
	return []byte(plain), nil
}

// MintedAt is the instant a turn's derived operations are minted at: its
// work's start, or the instant the turn rebased onto where that start is past
// the operation ledger's horizon — [rebaseFor], the one rule every operation id
// a turn derives follows, judged at this attempt's clock.
func (k executorKeep) MintedAt(ctx context.Context, turnID, workKey string, workSince time.Time) (time.Time, error) {
	identity := builtin.Actor{TurnID: turnID, WorkKey: workKey, WorkSince: workSince}
	rebased, err := rebaseFor(ctx, k.engine.rebases(), identity, time.Now().UTC())
	if err != nil {
		return time.Time{}, err
	}
	identity.RebasedTo = rebased
	return identity.OperationSince(), nil
}

// Seal streams archive sealed under the fleet keyring into dst, under an
// object key minted now ([Engine.ExecutorSessions]).
func (k executorKeep) Seal(handle, turn string, archive io.Reader, dst io.Writer) (executordomain.SealedSpool, error) {
	store, err := k.sessionStore()
	if err != nil {
		return executordomain.SealedSpool{}, err
	}
	return store.Seal(handle, turn, archive, dst)
}

// Upload puts what [executorKeep.Seal] sealed into the object it was sealed for.
func (k executorKeep) Upload(ctx context.Context, spool executordomain.SealedSpool,
	sealed io.Reader) (executordomain.SealedObject, error) {

	store, err := k.sessionStore()
	if err != nil {
		return executordomain.SealedObject{}, err
	}
	return store.Upload(ctx, spool, sealed)
}

// sessionStore is the store a session's sealed bytes stream into, or why this
// node has none.
func (k executorKeep) sessionStore() (*executordomain.SessionStore, error) {
	store, err := k.engine.ExecutorSessions()
	if err == nil && store == nil {
		err = errors.New("this node has no object store to keep a session in")
	}
	return store, err
}

// oweExecutorEnd records how a turn that launched an executor ended: OWED on
// the turn's run row ([sandbox.Coordinator.OweEnd]), which the run's ending
// records on the executor log once every capture of the turn has settled and
// before it deletes the row — so the seat's mail waits on it, and the next turn
// on the conversation reads it. A turn whose row is gone — its launch abandoned
// under it — has nothing to owe it on, and its end is recorded now
// ([sandbox.Coordinator.EndTurn]), best effort and said where the log does not
// take it.
//
// DECIDED ONCE: a turn whose row owes an end already keeps that one.
func (e *Engine) oweExecutorEnd(ctx context.Context, run sandbox.PendingRun, ending executordomain.Ending) {
	rt := e.sandbox.Load()
	if rt == nil {
		return
	}
	ending.Failure = redact.Secrets(ending.Failure)
	owed, err := rt.coordinator.OweEnd(ctx, run.TurnID, ending)
	if err == nil && owed {
		return
	}
	if err != nil {
		log.WarnContext(ctx, "executor_turn_end_unowed", "turn_id", run.TurnID, "agent", run.AgentHandle,
			"error", err.Error(), "detail", "the turn's end could not be written on its run's record, so it is "+
				"recorded on the executor log now")
	}
	e.endTurnNow(ctx, runTurnEnd(run, ending))
}

// endTurnNow records a turn's end on the executor log at once
// ([sandbox.Coordinator.EndTurn]), for a turn with no run record left to owe it
// on — best effort, and said where the log does not take it.
func (e *Engine) endTurnNow(ctx context.Context, end sandbox.TurnEnd) {
	rt := e.sandbox.Load()
	if rt == nil {
		return
	}
	end.Ending.Failure = redact.Secrets(end.Ending.Failure)
	if err := rt.coordinator.EndTurn(context.WithoutCancel(ctx), end); err != nil {
		log.WarnContext(ctx, "executor_turn_end_left", "turn_id", end.TurnID, "agent", end.Handle,
			"outcome", string(end.Ending.Outcome), "error", err.Error(),
			"detail", "the turn has no run record left to owe its end on, and the executor log did not "+
				"take it; the retention duty ends a turn that captured a session as lost")
	}
}

// runRecordGone reports whether a turn's run record is gone, read through this
// node's sandbox runtime; false where it cannot be read, which says nothing
// about the record, so the turn is retried as it would have been.
func (e *Engine) runRecordGone(ctx context.Context, turnID string) bool {
	rt := e.sandbox.Load()
	if rt == nil || rt.pending == nil {
		return false
	}
	_, found, err := rt.pending.Get(ctx, turnID)
	return err == nil && !found
}

// runTurnEnd is the end of the turn run's row belongs to, ending as ending.
func runTurnEnd(run sandbox.PendingRun, ending executordomain.Ending) sandbox.TurnEnd {
	return sandbox.TurnEnd{Handle: run.AgentHandle, TurnID: run.TurnID,
		ConversationKey: run.ConversationKey, WorkKey: run.WorkKey, WorkSince: run.WorkSince,
		Ask: run.TaskDescription, Ending: ending}
}

// telTurnEnd is the end of the turn tel describes, ending as ending: for a
// dispatched turn whose executor's launch opened a run row that the launch
// then abandoned, so no row is left to owe it on.
func telTurnEnd(tel turnTelemetry, ending executordomain.Ending) sandbox.TurnEnd {
	return sandbox.TurnEnd{Handle: tel.handle, TurnID: tel.runID, ConversationKey: tel.convKey,
		WorkKey: tel.workKey, WorkSince: tel.workSince, Ask: tel.title, Ending: ending}
}

// endingOf is how a turn whose loop returned res ended, as its end records it:
// FAILED where a guard ended it or its decision was a failure, COMPLETED
// otherwise — and the turn's own decision and breach, never the reviewer's last
// word, which still reads `done` after the loop overrode it; whether its reply
// reached the party waiting, and on what; what the reviewer said landed; what
// stopped a blocked round. answered is the engine's own answer to a colleague's
// ask, where the turn owed one.
func endingOf(res turn.Result, reply turn.Reply, answered colleagueAnswer) executordomain.Ending {
	ending := executordomain.Ending{Outcome: executordomain.OutcomeCompleted, Decision: string(res.Decision),
		Delivered: res.Delivered, DeliveredTo: replySurface(reply)}
	if res.Breach != nil || res.Decision == phase.Failed {
		ending.Outcome = executordomain.OutcomeFailed
	}
	if res.Breach != nil {
		ending.Failure = breachAccount(res.Breach)
	}
	if res.LastReview != nil {
		ending.CompletedWork = res.LastReview.CompletedWork
	}
	if w := res.LastWork; w != nil && w.Outcome == turn.OutcomeBlocked {
		ending.BlockedOn = w.Evidence
	}
	answered.onto(&ending)
	return ending
}

// abandonedEnding is how a resumed turn that broke and was abandoned ended
// (turn.Abandon): failed, saying why — the turn's error redacted, since it can
// quote what a tool or a box said.
func abandonedEnding(res turn.Result, reply turn.Reply, answered colleagueAnswer, reason string,
	err error) executordomain.Ending {

	ending := endingOf(res, reply, answered)
	ending.Outcome = executordomain.OutcomeFailed
	ending.Failure = redact.Secrets(fmt.Sprintf("the resumed turn broke and was abandoned (%s): %v", reason, err))
	return ending
}

// breachAccount is the end's account of the guard that ended a turn, redacted:
// a guard's detail can be an error's or a panic's text, which quotes what
// tripped it.
func breachAccount(b *turn.Breach) string {
	account := "the turn stopped on the engine's " + string(b.Kind) + " guard"
	if detail := strings.TrimSpace(b.Detail); detail != "" {
		account += ": " + detail
	}
	return redact.Secrets(account)
}

// replySurface is where the party waiting on a turn waits, as its end names it:
// the surface a tool reply is owed on, `a2a` for the colleague the engine
// answers, nothing where nobody waits.
func replySurface(reply turn.Reply) string {
	switch reply.Kind {
	case turn.ReplyTool:
		return reply.Surface
	case turn.ReplyEngine:
		return replyA2A
	}
	return ""
}

// replyA2A is the surface a colleague's ask is answered on.
const replyA2A = "a2a"

// stoppedEnding is how a turn a person stopped ended: stopped, by whom and why
// — the reason in their own words, which the next turn is told fenced as theirs
// (the ending's field is declared somebody else's) — beside whatever the turn's
// loop had decided and delivered by then.
func stoppedEnding(res turn.Result, reply turn.Reply, stop error, answered colleagueAnswer) executordomain.Ending {
	ending := endingOf(res, reply, answered)
	ending.Outcome, ending.Failure = executordomain.OutcomeStopped, ""
	if pause, ok := stopOf(stop); ok {
		ending.StoppedBy, ending.StopReason = pause.By, pause.Reason
	}
	return ending
}

// runDelivered is whether the party waiting on run's turn has its answer, as
// the run's own record of the turn's bridged calls says — [turn.Answered], the
// rule a turn's loop answers it by, over every round's calls: the earlier
// launches' ([sandbox.PendingRun.PriorCalls]) and the current one's — for an
// ending no frame of the turn reaches: a loss, a stop. KNOWN is false where the
// record is incomplete — calls were elided from it — and what it kept shows no
// delivery: a definite "did not reply" read off half a record is the false
// account the end exists to prevent.
func (e *Engine) runDelivered(c *Company, run sandbox.PendingRun) (reply turn.Reply, delivered, known bool) {
	reply = turn.ParseReply(run.Reply)
	if !reply.Valid() {
		return reply, false, true
	}
	calls := make([]ledger.Call, 0, len(run.PriorCalls)+len(run.BridgeCalls))
	for _, name := range run.PriorCalls {
		calls = append(calls, ledger.Call{Name: name})
	}
	for _, call := range run.BridgeCalls {
		calls = append(calls, ledger.Call{Name: call.Name, Failed: call.Failed})
	}
	delivered = turn.Answered(calls, seatSurface(e.seatRegistry(c, run.AgentHandle)), reply)
	elided := run.PriorCallsElided || run.BridgeCallsElided > 0
	return reply, delivered, delivered || !elided
}

// seatSurface is the delivery surface a seat's calls are judged against, as its
// registry holds it: which of its tools reach somebody, and where.
func seatSurface(reg *tools.Registry) turn.Surface {
	if reg == nil {
		return turn.Surface{}
	}
	u := reg.Snapshot()
	return turn.Surface{Catalogue: u.Names(), Deliveries: u.Deliveries(), KnownReads: u.KnownReads(),
		KnownOpenWorld: u.KnownOpenWorld(), Discoverable: u.MCPNames()}
}

// runEnding is how a run's turn ended where the coordinator's ending, not the
// turn's loop, ended it: lost, or stopped by a person — whether its reply
// reached anybody read off the turn's own calls, and the engine's answer to a
// colleague's ask beside it.
func (e *Engine) runEnding(c *Company, run sandbox.PendingRun, answered colleagueAnswer) executordomain.Ending {
	reply, delivered, known := e.runDelivered(c, run)
	ending := executordomain.Ending{Delivered: delivered, DeliveredTo: replySurface(reply),
		DeliveryUnknown: !known}
	switch {
	case run.Stop != nil:
		ending.Outcome = executordomain.OutcomeStopped
		ending.StoppedBy, ending.StopReason = run.Stop.By, run.Stop.Reason
	default:
		ending.Outcome = executordomain.OutcomeLost
		if run.Ending != nil {
			ending.Failure = redact.Secrets(fmt.Sprintf("the executor's run was lost (%s): %s",
				run.Ending.Reason, run.Ending.Detail))
		}
	}
	answered.onto(&ending)
	return ending
}
