package builtin

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/delegated"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/kept"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/textcut"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/words"
)

// CallHold records a call an executor's run makes to a tool at Needs approval
// as the question its job parks on, in the shape a run's own ask is recorded
// (ADR-0068, ADR-0077): the [tools.Holder] an executor's surface is built with,
// and no other.
//
// # Why the run parks rather than waits
//
// A person may take minutes or days to answer, and a headless harness cannot
// wait inside one tool call for that long, nor should the seat be held while
// it does. So the call is recorded on the run's row (the tool, its server, its
// definition's digest, the work item and the arguments sealed under the fleet
// keyring), the harness is told to end its turn, and the collection parks the
// run on the question with the seat freed, exactly as it parks on
// `ask_and_wait`. The answer, by `POST /mcp-approvals/{id}`, carries the run
// on with the call made (`once`, `always`) or refused (`deny`).
//
// # What the question says, and what it does not
//
// Who called which tool on which server, and why it waits. NEVER THE
// ARGUMENTS: the question, its context and its decision are what the Inbox
// shows the run's audience and what the answered record keeps, and the
// arguments are the run's own business, often a credential's worth. They ride
// the row sealed, and are opened only for a person who may approve the call
// (internal/api/queries) and for the resume that makes it.
type CallHold struct {
	desk   RunAskDesk
	seal   *sandbox.CallSeal
	acting ActedDecisions
	rules  DelegatedRules
	now    func() time.Time
}

// DelegatedRules is the live delegated-decisions rule for an act: its
// delegates and 1-based number, false where none delegates it (ADR-0125).
type DelegatedRules interface {
	Rule(kind delegated.Kind) (delegates []string, rule, sampleOneIn int, ok bool)
}

// ActedDecisions reads the decision answers gave a seat: the newest question
// the seat asked that a person answered among them, nil for none.
type ActedDecisions interface {
	DecisionAnswered(ctx context.Context, answers []string, asker string) (*sandbox.HeldAct, error)
}

var _ tools.Holder = (*CallHold)(nil)

// NewCallHold is the holder over desk (where the question is recorded) and
// seal (what the arguments are sealed under), or nil without either: a
// surface with no holder refuses every call to a tool at Needs approval,
// which is the safe reading of a node that cannot record one.
// A nil acting names no decision on any held call, and nil rules delegate
// nothing.
func NewCallHold(desk RunAskDesk, seal *sandbox.CallSeal, acting ActedDecisions, rules DelegatedRules) *CallHold {
	if desk == nil || seal == nil {
		return nil
	}
	return &CallHold{desk: desk, seal: seal, acting: acting, rules: rules, now: time.Now}
}

// delegation is how a held call of act is put to delegates before any
// person, nil where no rule delegates it: the rule's number and seats as they
// stand now, and the digest of the arguments as its delegates will read them.
func (h *CallHold) delegation(act delegated.Kind, args map[string]any) *sandbox.HeldDelegation {
	if h.rules == nil {
		return nil
	}
	delegates, rule, _, ok := h.rules.Rule(act)
	if !ok || len(delegates) == 0 {
		return nil
	}
	// AS THE SEAL GIVES THEM BACK, which is what a delegate's digest is of.
	var opened map[string]any
	data, err := json.Marshal(args)
	if err == nil {
		err = json.Unmarshal(data, &opened)
	}
	digest, derr := sandbox.HeldArgumentsDigest(opened)
	if err != nil || derr != nil {
		return nil
	}
	return &sandbox.HeldDelegation{Rule: rule, Delegates: slices.Clone(delegates), Digest: digest}
}

// actedOn is the decision turn's call carries out: a person's answer to the
// seat's question whose delivery woke the turn. Nil for a turn no such answer
// woke, or a read that failed: the call is still held, naming no decision.
func (h *CallHold) actedOn(ctx context.Context, turn *turnctx.Turn) *sandbox.HeldAct {
	if h.acting == nil || turn == nil || turn.Handle() == "" || len(turn.Answered) == 0 {
		return nil
	}
	act, err := h.acting.DecisionAnswered(ctx, turn.Answered, turn.Handle())
	if err != nil {
		log.WarnContext(ctx, "held_call_decision_unread", "seat", turn.Handle(), "error", err.Error(),
			"detail", "the call is held naming no decision")
		return nil
	}
	return act
}

// actingWords is what a held call's context says of the decision it carries
// out, "" for none: an approver reads that the call is a step of an answer
// already given, not a new question.
func actingWords(act *sandbox.HeldAct) string {
	if act == nil {
		return ""
	}
	item := cmp.Or(act.Key, act.Item)
	question := textcut.Within(act.Question, heldActQuestionBytes)
	if act.Choice == "" {
		return fmt.Sprintf(" It carries out a decision on %s: %s answered %q in words.", item,
			act.AnsweredBy, question)
	}
	return fmt.Sprintf(" It carries out a decision on %s: %s answered %q with %q.", item, act.AnsweredBy,
		question, textcut.Within(act.Choice, heldActQuestionBytes))
}

// heldActQuestionBytes bounds the question and the choice a held call's
// context quotes, a change line's bound (capability.MaxLineBytes).
const heldActQuestionBytes = 300

// Hold records call as the question the run's job parks on. See
// [tools.Holder].
func (h *CallHold) Hold(ctx context.Context, turn *turnctx.Turn, call tools.HeldCall) (tools.Result, bool) {
	runID, launch := "", turnctx.JobOf(ctx)
	if turn != nil {
		runID = turn.RunID
	}
	if runID == "" || launch == "" {
		// NOT AN EXECUTOR'S CALL: nothing names the job to park. The holder
		// is installed on the executor's surface alone, so this is a wiring
		// fault, said as one.
		return refusedBy(tools.RefusalUnavailable, waitless(), call.Name+" was not called: it "+
			"needs a person's approval before each call, and this call was not made by a run "+
			"that can wait for one."), false
	}
	if call.Entry.ApprovalFixed {
		return h.holdFixed(ctx, turn, runID, launch, call)
	}
	tool, ok := call.Entry.Tool.(heldTool)
	if !ok {
		return faulted(ctx, call.Name, fmt.Errorf("builtin: a call held for approval names %q, "+
			"which no MCP server serves", call.Name), call.Name+" was not called: the engine "+
			"could not tell which server serves it, so nothing was asked"), false
	}
	held := sandbox.HeldCall{
		ID: uuid.Must(uuid.NewV7()).String(), LaunchID: launch,
		Server: tool.Server(), Tool: call.Name, Raw: tool.RawName(), Digest: tool.Digest(),
		HeldAt: h.now().UTC(), Acting: h.actedOn(ctx, turn), Act: string(delegated.KindOther),
	}
	if res, ok := h.sealArgs(ctx, runID, &held, call); !ok {
		return res, false
	}
	seat := turn.Handle()
	question := fmt.Sprintf("May %s call %s on the %s MCP server?", seat, call.Name, held.Server)
	why := "that asks a person before each call"
	if call.Changed {
		// WHY IT ASKS, where the level does not say so: Always allow
		// accepts the definition the call was made under, which is the
		// changed one (internal/api/mcpapi).
		why = "whose definition changed since it was allowed (or which the server added since " +
			"its first listing), so it asks a person before each call until somebody accepts the " +
			"change on the server's page; Always allow accepts the definition this call was made " +
			"under"
	}
	return h.record(ctx, runID, launch, call.Name, sandbox.Ask{
		Question: question,
		Context: fmt.Sprintf("%s's run called `%s`, a tool of the MCP server `%s` %s. The call "+
			"has not been made: it waits for this answer, and the run is parked until then. Its "+
			"arguments are shown to whoever may approve it.",
			seat, call.Name, held.Server, why) + actingWords(held.Acting),
		Decision: sandbox.HeldDecision(question, heldEvidence(turn)),
		To:       sandbox.AskToManager,
		Held:     &held,
	})
}

// holdFixed holds a call to an engine tool that asks before every call: its
// question and who may answer are the tool's own ([fixedTool]), and it is
// answered once or deny alone.
func (h *CallHold) holdFixed(ctx context.Context, turn *turnctx.Turn, runID, launch string,
	call tools.HeldCall) (tools.Result, bool) {
	tool, ok := tools.Innermost(call.Entry.Tool).(fixedTool)
	if !ok {
		return faulted(ctx, call.Name, fmt.Errorf("builtin: %q asks before every call and says "+
			"nothing a person can be asked", call.Name), call.Name+" was not called: the engine "+
			"could not word its question, so nothing was asked"), false
	}
	// REFUSED BEFORE ANYBODY IS ASKED: a call its tool would refuse once
	// approved is never put to a person.
	if refusal := tool.BeforeHold(ctx, turn, call.Args); refusal != nil {
		return *refusal, false
	}
	acting := h.actedOn(ctx, turn)
	// APPROVED ALREADY, through the decision this turn carries out: made
	// now, and never held (ADR-0125).
	if decided, ok := tool.(decidedTool); ok {
		if res, ran := decided.RunDecided(ctx, turn, call.Args, acting); ran {
			return res, false
		}
	}
	held := sandbox.HeldCall{
		ID: uuid.Must(uuid.NewV7()).String(), LaunchID: launch,
		Server: config.ToolServerName, Tool: call.Name, Raw: call.Name, Digest: tool.Digest(),
		HeldAt: h.now().UTC(), Fixed: true, Approvers: tool.Approvers(), Binding: tool.Binding(turn, call.Args),
		Acting: acting, Act: string(tool.Act()), Delegation: h.delegation(tool.Act(), call.Args),
	}
	if res, ok := h.sealArgs(ctx, runID, &held, call); !ok {
		return res, false
	}
	question, context := tool.HeldWords(turn, call.Args)
	return h.record(ctx, runID, launch, call.Name, sandbox.Ask{
		Question: question, Context: context + actingWords(held.Acting),
		Decision: sandbox.HeldFixedDecision(question, heldEvidence(turn)),
		To:       sandbox.AskToManager,
		Held:     &held,
	})
}

// sealArgs seals call's arguments onto held, refusing a call whose sealed
// arguments are past what a run's row carries.
func (h *CallHold) sealArgs(ctx context.Context, runID string, held *sandbox.HeldCall,
	call tools.HeldCall) (tools.Result, bool) {
	sealed, err := h.seal.SealArguments(runID, *held, call.Args)
	if err != nil {
		return nodeFailure(ctx, call.Name, err, call.Name+" was not called, and nothing was "+
			"asked: its arguments could not be sealed", "Call it again."), false
	}
	if sandbox.HeldArgsFit(sealed) != nil {
		// PAST WHAT THE RUN'S ROW CARRIES OF A HELD CALL (sandbox's
		// MaxHeldArgsBytes): a call held for approval never reaches its
		// server, so nothing else bounds its arguments, and the row that
		// carries them is one broker message. Refused while the run can
		// still act on it, with the size named — never recorded and refused
		// at the broker, which no call made again would get past.
		return refused(tools.RefusalInvalid, fmt.Sprintf("%s was not called, and nothing was asked: "+
			"a call that waits for a person's approval may carry at most %d KiB of arguments, "+
			"sealed, and this one carries %d KiB. Split it into smaller calls, or shorten what it "+
			"carries, and call again.", call.Name, sandbox.MaxHeldArgsBytes>>10,
			sealedKiB(sealed))), false
	}
	held.Arguments = sealed
	return tools.Result{}, true
}

// record records ask as the question the run's job parks on, and says what the
// run is told.
func (h *CallHold) record(ctx context.Context, runID, launch, name string, ask sandbox.Ask) (tools.Result, bool) {
	outcome, err := h.desk.RecordAsk(ctx, runID, launch, ask)
	switch {
	case err != nil && errors.Is(err, decision.ErrInvalid):
		// THE ENGINE'S OWN ASK REFUSED, which nothing the run did can fix:
		// a fault, said as one.
		return faulted(ctx, name, err, name+" was not called, and nothing was asked: "+
			"the engine could not record the question"), false
	case err != nil:
		return nodeFailure(ctx, name, err, name+" was not called, and nothing was "+
			"asked: the question could not be recorded", "Call it again."), false
	case outcome == sandbox.AskAlreadyAsked:
		return refused(tools.RefusalNotRunning, name+" was not called: this run is already "+
			"waiting on a person, for a question or another call, and one answer carries it on. "+
			"END YOUR TURN NOW, and call it again once the run carries on."), false
	case outcome != sandbox.AskRecorded:
		return refused(tools.RefusalNotRunning, name+" was not called: this run is no "+
			"longer running, so nothing can wait on a person here. End your turn."), false
	}
	who := "a person who may approve it"
	if ask.Held != nil && ask.Held.Delegation != nil {
		who = "the company's delegate for it (" + words.And(ask.Held.Delegation.Delegates) + "), who " +
			"may allow it, deny it or put it to a person"
	}
	return tools.Result{Output: name + " was NOT called: it needs approval " +
		"before each call, and the call has been put to " + who + ". END YOUR " +
		"TURN NOW: commit and push your work in progress first, then stop working. Do not call `" +
		toolname.SubmitWork + "`, and do not call " + name + " again. When they answer, " +
		"this session continues and is told whether the call was made and what it returned; " +
		"if this machine is gone by then, a fresh run is told the call, the answer and the " +
		"result."}, true
}

// sealedKiB is what sealed arguments weigh on a run's row, in whole KiB rounded
// up, as a refusal names them.
func sealedKiB(sealed string) int { return (kept.Quoted(sealed) + 1<<10 - 1) >> 10 }

// fixedTool is an engine tool that asks before every call
// ([tools.Entry.ApprovalFixed]): the refusal of a call it would never make,
// its definition's digest, which the resume holds the call to, the question
// and context a person reads, never the call's credentials, and who may
// answer ([sandbox.HeldCall.Approvers]). The registry asks before every call
// to any builtin that is one ([Register]).
type fixedTool interface {
	BeforeHold(ctx context.Context, turn *turnctx.Turn, args map[string]any) *tools.Result
	Digest() string
	// Binding is a digest of what the call is bound to beyond its arguments,
	// as the person is shown it; the resume holds the call to it.
	Binding(turn *turnctx.Turn, args map[string]any) string
	HeldWords(turn *turnctx.Turn, args map[string]any) (question, context string)
	Approvers() string
	// Act is what the call is for (ADR-0125).
	Act() delegated.Kind
}

// decidedTool is a fixed tool whose call a person may approve before it is
// made, through a decision the engine composed: it makes the call now where
// the decision the turn carries out approved exactly it (ADR-0125).
type decidedTool interface {
	RunDecided(ctx context.Context, turn *turnctx.Turn, args map[string]any, act *sandbox.HeldAct) (tools.Result, bool)
}

// heldTool is an MCP server's tool as a held call names it: its server, the
// server's own name for it and its definition (internal/mcp's Tool).
type heldTool interface {
	Server() string
	RawName() string
	Digest() string
}

// heldEvidence is what a held call cites: the work item the run is on, where it
// is a Crewlet Works task (the one kind of item a task reference names).
func heldEvidence(turn *turnctx.Turn) []decision.Evidence {
	if turn == nil || turn.WorkItem == nil || turn.WorkItem.Backend != types.WorkWorklet ||
		turn.WorkItem.ID == "" {
		return nil
	}
	label := turn.WorkItem.Key
	if len(label) > decision.MaxEvidenceLabel {
		label = ""
	}
	return []decision.Evidence{{Kind: decision.EvidenceTask, Ref: turn.WorkItem.ID, Label: label}}
}
