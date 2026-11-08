package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
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

// executorLogWrites is the executor log's writes the engine records a run's
// captures and its turn's end through: the estate router's
// (estate.ExecutorWrites), which a seats-only node reaches through a data node.
type executorLogWrites interface {
	Begin(ctx context.Context, c executordomain.Call, n executordomain.NewBegin) (executordomain.BeginResult, error)
	Capture(ctx context.Context, c executordomain.Call,
		n executordomain.NewCapture) (executordomain.CaptureResult, error)
	End(ctx context.Context, c executordomain.Call, n executordomain.NewEnd) (statelog.Result, error)
}

// executorRecords is the sandbox runtime's writes of the executor log
// ([sandbox.SessionWrites]), stated as the log's records and answered in the
// sandbox's words: the sandbox links no state log (its dependency gate), so the
// author, the operation a record's place derives and the reading of the log's
// three-valued answer are the engine's, here.
type executorRecords struct{ log executorLogWrites }

var _ sandbox.SessionWrites = executorRecords{}

// executorRecordsOf is the sandbox's writes over the router's, or nil on an
// engine built without them, whose runtime then records nothing.
func executorRecordsOf(log executorLogWrites) sandbox.SessionWrites {
	if log == nil {
		return nil
	}
	return executorRecords{log: log}
}

// sandboxAuthor is who every executor record the sandbox runtime makes names:
// the engine's sandbox runtime, which no person and no model drives.
var sandboxAuthor = iam.Actor{Name: "sandbox", Kind: iam.ActorSystem}

// Capture records one capture under the operation its place derives
// ([executordomain.CaptureOpID]): the turn, the launch it was taken from and
// its ordinal there, at the instant the turn's operations are minted at — so
// every attempt at it is one operation, however often the sandbox asks.
func (r executorRecords) Capture(ctx context.Context, c sandbox.CaptureRecord) (sandbox.LogAnswer, error) {
	stamp := c.Stamp
	call := executordomain.Call{By: sandboxAuthor,
		OpID: executordomain.CaptureOpID(stamp.Minted, c.Handle, c.Turn, stamp.Launch, stamp.CaptureSeq)}
	n := executordomain.NewCapture{
		Handle: c.Handle, Turn: c.Turn, ConversationKey: c.ConversationKey, WorkKey: c.WorkKey,
		Harness: stamp.Harness, HarnessVersion: stamp.HarnessVersion, SessionID: stamp.SessionID,
		Object: c.Object, ContentKey: stamp.ContentKey, ArchiveFormat: stamp.ArchiveFormat,
		LaunchSeq: stamp.LaunchSeq, CaptureSeq: stamp.CaptureSeq,
		Home: stamp.Home, Workspace: stamp.Workspace, SeatBudget: stamp.SeatBudget,
		// WHAT ONLY THE RUN'S ROW KNEW OF THE SESSION, carried onto the log's
		// row, which outlives it: a launch continuing the session after its
		// turn ended has no run record to read them from.
		ContextTokens: stamp.ContextTokens, Interrupted: stamp.Interrupted,
	}
	if c.ReadThrough > 0 {
		// THE TURN'S FIRST READ, as its first launch froze it: the log
		// keeps it where no begin of the turn landed first.
		n.ReadThrough = statelog.Unpack(executordomain.Domain{}.Stream().Name, c.ReadThrough)
		n.Absorbs = c.Absorbs
	}
	res, err := r.log.Capture(ctx, call, n)
	switch {
	case err == nil && res.Outcome == statelog.OutcomeApplied && res.Recorded:
		return sandbox.LogLanded, nil
	case err == nil && res.Outcome == statelog.OutcomeApplied:
		return sandbox.LogPassedOver, nil
	case refusedCapture(err):
		return sandbox.LogRefused, err
	}
	return sandbox.LogUnsettled, unsettledRecord(res.Outcome, err)
}

// Begin records a turn's first read of its conversation under the one
// operation the turn derives ([executordomain.BeginOpID]), and answers whether
// the turn's row, read back, holds the base the read froze
// ([executordomain.BeginResult]). APPLIED is LANDED whether this attempt wrote
// it or an earlier one, or a capture that landed first, recorded the turn's
// first read: either way the row read back says what it holds. The log's
// definite refusals are a capture's ([refusedCapture]).
func (r executorRecords) Begin(ctx context.Context, b sandbox.BeginRecord) (sandbox.BeginAnswer, error) {
	call := executordomain.Call{By: sandboxAuthor, OpID: executordomain.BeginOpID(b.Minted, b.Handle, b.Turn)}
	res, err := r.log.Begin(ctx, call, executordomain.NewBegin{Handle: b.Handle, Turn: b.Turn,
		ConversationKey: b.ConversationKey, WorkKey: b.WorkKey,
		ReadThrough: statelog.Unpack(executordomain.Domain{}.Stream().Name, b.ReadThrough),
		Absorbs:     b.Absorbs, BaseTurn: b.BaseTurn, BaseObject: b.BaseObject})
	switch {
	case err == nil && res.Outcome == statelog.OutcomeApplied:
		return sandbox.BeginAnswer{Log: sandbox.LogLanded, Held: res.Held, Why: res.Why}, nil
	case refusedCapture(err):
		return sandbox.BeginAnswer{Log: sandbox.LogRefused}, err
	}
	return sandbox.BeginAnswer{Log: sandbox.LogUnsettled}, unsettledRecord(res.Outcome, err)
}

// refusedCapture reports whether err is the log's definite refusal of a capture:
// the turn ended or removed, a newer capture recorded, an object too old to
// name, a seat budget the live turns fill, or a record it refuses outright.
// Anything else — a node that did not answer, none that serves, a conversation
// that kept changing — may land if asked again under the same operation.
func refusedCapture(err error) bool {
	for _, refusal := range []error{executordomain.ErrTurnEnded, executordomain.ErrStaleCapture,
		executordomain.ErrUploadStale, executordomain.ErrOverBudget, executordomain.ErrInvalid,
		executorkeep.ErrSessionTooLarge, executordomain.ErrNotFound} {
		if errors.Is(err, refusal) {
			return true
		}
	}
	return false
}

// End records how a turn ended under the one operation every path that ends
// the turn derives ([executordomain.EndOpID]). An end the log refuses as
// already made is LANDED: two frames that both end a turn derive the same
// operation, and the second is refused because the first was taken.
func (r executorRecords) End(ctx context.Context, e sandbox.EndRecord) (sandbox.LogAnswer, error) {
	call := executordomain.Call{By: sandboxAuthor, OpID: executordomain.EndOpID(e.Minted, e.Handle, e.Turn)}
	res, err := r.log.End(ctx, call, executordomain.NewEnd{Handle: e.Handle, Turn: e.Turn,
		ConversationKey: e.ConversationKey, WorkKey: e.WorkKey, Ask: e.Ask, Ending: e.Ending})
	switch {
	case err == nil && res.Outcome == statelog.OutcomeApplied:
		return sandbox.LogLanded, nil
	case errors.Is(err, executordomain.ErrTurnEnded):
		return sandbox.LogLanded, nil
	case errors.Is(err, executordomain.ErrInvalid):
		return sandbox.LogRefused, err
	}
	return sandbox.LogUnsettled, unsettledRecord(res.Outcome, err)
}

// unsettledRecord is what an unsettled answer says: the log's outcome, and the
// error it came with where there was one.
func unsettledRecord(outcome statelog.Outcome, err error) error {
	if err == nil {
		return fmt.Errorf("outcome %s", outcome)
	}
	return fmt.Errorf("outcome %s: %w", outcome, err)
}

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

// Keeps is whether the revision this node runs keeps sessions, read off the
// company it holds with no round trip ([sandbox.SessionKeep.Keeps]): the hint
// that spares a launch its sealing, and a loss its read of another host's box,
// where nothing would be kept. A node with no object store keeps nothing, as
// [executorKeep.Policy] says; the take itself asks Policy, the fleet's word.
func (k executorKeep) Keeps() bool {
	if k.engine.Objects() == nil {
		return false
	}
	c := k.engine.Company()
	if c == nil || c.Config == nil {
		return true
	}
	return c.Config.TurnEngine.ConversationSession.KeepsSessions()
}

// executorPolicy is `turn_engine.conversation_session`'s switch and per-seat
// budget, as the revision the fleet's activation pointer names states them
// ([Engine.servedFleetRevision]), or why this node cannot know them.
//
// THE FLEET'S REVISION, NEVER THIS NODE'S OWN, for [Engine.executorRetention]'s
// reason: the budget a capture carries decides which ended sessions every node
// releases, so a node serving a revision the fleet did not take must not decide
// it. A capture that cannot read it is refused, and its stamp says why.
func (e *Engine) executorPolicy(ctx context.Context) (sandbox.SessionPolicy, error) {
	company, err := e.servedFleetRevision(ctx)
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
// ([sandbox.PendingStore.Handed]) under the node's keyring — the coordination
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
	return k.engine.turnMintedAt(ctx, turnID, workKey, workSince)
}

// Seal streams archive sealed under the fleet keyring into dst, under an
// object key minted now ([Engine.ExecutorSessions]).
func (k executorKeep) Seal(handle, turn string, archive io.Reader, dst io.Writer) (executorkeep.SealedSpool, error) {
	store, err := k.engine.executorSessionStore()
	if err != nil {
		return executorkeep.SealedSpool{}, err
	}
	return store.Seal(handle, turn, archive, dst)
}

// Upload puts what [executorKeep.Seal] sealed into the object it was sealed for.
func (k executorKeep) Upload(ctx context.Context, spool executorkeep.SealedSpool,
	sealed io.Reader) (executorkeep.SealedObject, error) {

	store, err := k.engine.executorSessionStore()
	if err != nil {
		return executorkeep.SealedObject{}, err
	}
	return store.Upload(ctx, spool, sealed)
}

// executorSessionStore is the store a session's sealed bytes stream into and
// are fetched back from, or why this node has none: the ENGINE'S, one store
// whether a launch keeps a session ([executorKeep]) or a relaunch continues one
// ([executorRestore]).
func (e *Engine) executorSessionStore() (*executordomain.SessionStore, error) {
	store, err := e.ExecutorSessions()
	if err == nil && store == nil {
		err = errors.New("this node has no object store to keep a session in")
	}
	return store, err
}

// oweExecutorEnd records how a turn that launched an executor ended: OWED on
// the turn's run row ([sandbox.Coordinator.OweEnd]), which the run's ending
// records on the executor log once every capture of the turn has settled and
// before it deletes the row — so the seat's mail waits on it, and the turn's
// row on the log holds it beside the session it kept. Owed under a context of
// the coordinator's own, so a frame a drain is cancelling still owes it. A turn
// whose row is gone has nothing to owe it on, and its end is recorded now
// ([sandbox.Coordinator.EndTurn]) — owed after all where the row turns out to
// be there — best effort and said where the log does not take it.
//
// DECIDED ONCE: a turn whose row owes an end already keeps that one.
func (e *Engine) oweExecutorEnd(ctx context.Context, run sandbox.PendingRun, ending executorkeep.Ending) {
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

// endTurnNow records a turn's end for a turn with no run record left to owe it
// on ([sandbox.Coordinator.EndTurn], which owes it on the record where one is
// there after all) — best effort, and said where the log does not take it.
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

// runEnded reports whether a turn's run is over: its record gone, or its
// ending decided — a relaunch abandoned under the turn's frame, whose record
// waits for that frame to owe the turn's end ([sandbox.Coordinator.OweEnd]).
// Read through this node's sandbox runtime; false where it cannot be read,
// which says nothing about the run, so the turn is retried as it would have
// been.
func (e *Engine) runEnded(ctx context.Context, turnID string) bool {
	rt := e.sandbox.Load()
	if rt == nil || rt.pending == nil {
		return false
	}
	run, found, err := rt.pending.Get(ctx, turnID)
	return err == nil && (!found || run.Ending != nil)
}

// runTurnEnd is the end of the turn run's row belongs to, ending as ending.
func runTurnEnd(run sandbox.PendingRun, ending executorkeep.Ending) sandbox.TurnEnd {
	return sandbox.TurnEnd{Handle: run.AgentHandle, TurnID: run.TurnID,
		ConversationKey: run.ConversationKey, WorkKey: run.WorkKey, WorkSince: run.WorkSince,
		Ask: run.TaskDescription, Ending: ending}
}

// telTurnEnd is the end of the turn tel describes, ending as ending: for a
// dispatched turn whose executor's launch opened a run row that the launch
// then abandoned, so no row is left to owe it on.
func telTurnEnd(tel turnTelemetry, ending executorkeep.Ending) sandbox.TurnEnd {
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
func endingOf(res turn.Result, reply turn.Reply, answered colleagueAnswer) executorkeep.Ending {
	ending := executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted, Decision: string(res.Decision),
		Delivered: res.Delivered, DeliveredTo: replySurface(reply)}
	if res.Breach != nil || res.Decision == phase.Failed {
		ending.Outcome = executorkeep.OutcomeFailed
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
	err error) executorkeep.Ending {

	ending := endingOf(res, reply, answered)
	ending.Outcome = executorkeep.OutcomeFailed
	ending.Failure = redact.Secrets(fmt.Sprintf("the resumed turn broke and was abandoned (%s): %v", reason, err))
	return ending
}

// launchAbandonedEnding is how a dispatched turn whose first launch was
// abandoned after its run's record was written ended: failed, saying why — the
// launch's error redacted, since it can quote what a box or a store said —
// beside whatever its loop had decided and answered by then.
func launchAbandonedEnding(res turn.Result, reply turn.Reply, answered colleagueAnswer,
	err error) executorkeep.Ending {

	ending := endingOf(res, reply, answered)
	ending.Outcome = executorkeep.OutcomeFailed
	ending.Failure = redact.Secrets(fmt.Sprintf("the turn's executor launch was abandoned after its run was "+
		"recorded: %v", err))
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
// — the reason in their own words, kept as theirs, fenced for any model that
// reads it (the ending's field is declared somebody else's) — beside whatever
// the turn's loop had decided and delivered by then.
func stoppedEnding(res turn.Result, reply turn.Reply, stop error, answered colleagueAnswer) executorkeep.Ending {
	ending := endingOf(res, reply, answered)
	ending.Outcome, ending.Failure = executorkeep.OutcomeStopped, ""
	if pause, ok := stopOf(stop); ok {
		ending.StoppedBy, ending.StopReason = pause.By, pause.Reason
	}
	return ending
}

// runDelivered is whether the party waiting on run's turn has its answer, as
// the run's own log of the turn's bridged calls says, read by the run's ending
// ([sandbox.RunCalls]) — [turn.Answered], the rule a turn's loop answers it
// by, over every round's calls: the earlier rounds' ([sandbox.BridgeLog.PriorCalls])
// and the current one's, the calls its log kept and the identities of the ones
// it does not list ([sandbox.BridgeLog.Unlisted]) — for an ending no frame of
// the turn reaches: a loss, a stop. KNOWN is false where the log could not be
// read, or is incomplete — calls not accounted for by name, on a log a build
// without identities elided or past what its tally of identities holds
// ([sandbox.BridgeLog.ElisionKnown]) — and what it kept shows no delivery: a
// definite "did not reply" read off no log, or half of one, is the false
// account the end exists to prevent.
func (e *Engine) runDelivered(c *Company, run sandbox.PendingRun, bridged sandbox.RunCalls) (reply turn.Reply,
	delivered, known bool) {

	reply = turn.ParseReply(run.Reply)
	if !reply.Valid() {
		return reply, false, true
	}
	if bridged.Unread != nil {
		return reply, false, false
	}
	logged := bridged.Log
	dropped := logged.Unlisted()
	calls := make([]ledger.Call, 0, len(logged.PriorCalls)+len(logged.Calls)+len(dropped))
	for _, name := range logged.PriorCalls {
		calls = append(calls, ledger.Call{Name: name})
	}
	for _, call := range logged.Calls {
		calls = append(calls, ledger.Call{Name: call.Name, Failed: call.Failed})
	}
	calls = append(calls, ledger.Called(identitiesOf(dropped))...)
	delivered = turn.Answered(calls, deliverySurface(e.seatRegistry(c, run.AgentHandle)), reply)
	incomplete := logged.PriorCallsElided || !logged.ElisionKnown()
	return reply, delivered, delivered || !incomplete
}

// deliverySurface is the delivery surface a seat's calls are judged against, as
// its registry holds it: which of its tools reach somebody, and where.
func deliverySurface(reg *tools.Registry) turn.Surface {
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
func (e *Engine) runEnding(c *Company, run sandbox.PendingRun, bridged sandbox.RunCalls,
	answered colleagueAnswer) executorkeep.Ending {

	reply, delivered, known := e.runDelivered(c, run, bridged)
	ending := executorkeep.Ending{Delivered: delivered, DeliveredTo: replySurface(reply),
		DeliveryUnknown: !known}
	switch {
	case run.Stop != nil:
		ending.Outcome = executorkeep.OutcomeStopped
		ending.StoppedBy, ending.StopReason = run.Stop.By, run.Stop.Reason
	default:
		ending.Outcome = executorkeep.OutcomeLost
		if run.Ending != nil {
			ending.Failure = redact.Secrets(fmt.Sprintf("the executor's run was lost (%s): %s",
				run.Ending.Reason, run.Ending.Detail))
		}
	}
	answered.onto(&ending)
	return ending
}
