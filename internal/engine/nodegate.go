package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE NODE GATE: eviction and readmission as ONE fleet gesture over every
// identity-claiming log. The trim counts nodes per log, so an eviction is a
// record on every such log and a readmission the inverse commit on each. It
// lives in the engine, the one place holding every domain's write authority,
// the positions register, the published floors and the presence leases.
//
// It is JUDGED ONCE, before anything is appended: judged per log, one gesture
// could leave a node evicted on one log and counted on another. Then every log
// is written at once ([NodeGate.write]), each answering its own outcome, under
// an operation id derived from the gesture's ([domainOpID]) and judged against
// the node's row ([statelog.GateStanding]): the same gesture again finishes a
// partial result, and a record undone since answers `superseded`.

// The gesture's budgets: what bounds each of its two phases, and so how long a
// node takes to answer one gesture at most ([GateAnswerBudget]). One budget
// per phase, because each waits on something different and one shared
// deadline lets the slower starve the other.
const (
	// GateJudgeBudget bounds the JUDGEMENT, everything before the first
	// record: the presence leases for an eviction; for a readmission the
	// positions register, the published floors and each identity log's
	// first surviving sequence on this node's copy ([stateLog.Readmissible]).
	//
	// TWENTY SECONDS: one election of the coordination store's group, which
	// may stall those reads once ([jsprovision.AskTerm], fifteen seconds),
	// and five for the reads themselves. A judgement cut short has written
	// nothing, and the same gesture again finishes it.
	GateJudgeBudget = 20 * time.Second

	// GateLogBudget bounds the LOGS, from the first record to the last log's
	// answer. They are written at once ([NodeGate.write]), so it waits on the
	// SLOWEST log rather than the sum, and a log added to the register costs
	// the gesture nothing.
	//
	// THIRTY SECONDS, from what one log waits on: an append through this
	// node's write authority, whose two waits (its applier reaching a peer's
	// record the decision must see, and its own append's resolution) are each
	// [statelog.DefaultResolveBudget], five seconds, behind at most one
	// election of the log's stream group ([jsprovision.AskTerm], fifteen),
	// plus five for the snapshot and margin. A log silent by then answers
	// `unknown`, and the same gesture finishes it from that log's ledger.
	GateLogBudget = 30 * time.Second

	// GateBudget bounds one gesture from its first record to its last answer.
	GateBudget = GateLogBudget

	// GateAnswerBudget is the longest a node takes to answer one gesture
	// request: its judgement, then the gesture, fifty seconds. A client waits
	// [GateClientWait], a little longer, so the node's own answer reaches the
	// operator before the client gives up.
	GateAnswerBudget = GateJudgeBudget + GateBudget

	// gateRoundTrip is what a client's wait adds to [GateAnswerBudget]: two
	// five-second resolutions back to back, the ten seconds every other verb
	// a client sends waits.
	gateRoundTrip = 2 * statelog.DefaultResolveBudget
)

// GateClientWait is how long a client waits for a gesture's answer, one
// minute: the node's own bound ([GateAnswerBudget]) plus the round trip. It is
// the one relation every client reads (`crewlet retention evict` and
// `readmit`; the dashboard's GATE_REQUEST_TIMEOUT_MS is held to it by a gate in
// internal/api), so the node's answer, every log's outcome and remedy, reaches
// the operator rather than a client timeout that knows none of it.
func GateClientWait() time.Duration { return GateAnswerBudget + gateRoundTrip }

// ErrInvalidGate is what a gate request that could not be carried out as given
// wraps: no node, a node id no node could run under, no operation id, or no
// operator. Nothing is judged and nothing is written.
var ErrInvalidGate = errors.New("engine: invalid gate request")

// GateRequest is one eviction or readmission, as the operator asked for it.
type GateRequest struct {
	// Node is the machine being evicted or taken back.
	Node string

	// OpID is the GESTURE's operation id, supplied by the caller so a retry
	// of a partial result is the same operation on every log.
	OpID string

	// By is the principal who ran it, recorded on every log's record as the
	// author, the kind of party and the credential they acted through
	// ([iam.ActorFor]), which `crewlet retention status` names beside an
	// eviction. The principal and not its name: a bare name cannot carry the
	// kind or the credential (it reads as a Tier A token of that name), and
	// the identity estate's writer judges admin on the party's OWN grants at
	// the record ([registration.NewGate]).
	By iam.Principal

	// Force evicts a node that still holds a live presence lease — see
	// [statelog.PermitEviction] — and one whose lease could not be read at
	// all. A readmission ignores it.
	Force bool
}

// valid refuses a request that could not be retried as the same operation.
// The node id is held to a node's own rule because it becomes a subject token
// on every identity log: anything else is a typo, or a wildcard the broker
// refuses. The operation id is held to a caller's grammar
// ([statelog.CheckCallerOpID]) here, not only at the route: every log's id is
// derived from it, and the publisher reads its mint instant to decide whether
// the ledger can vouch for a retry. One carrying none reads as minted at the
// epoch, so once any ledger is swept the gesture answers `unknown` on every
// log, every time, unpublished and without saying why.
func (r GateRequest) valid() error {
	opID := statelog.CheckCallerOpID(r.OpID)
	invalid := func(err error, format string, args ...any) error {
		return &statelog.Telling{Say: fmt.Sprintf(format, args...),
			Err: errors.Join(ErrInvalidGate, err)}
	}
	switch {
	case r.Node == "":
		return invalid(nil, "name the node")
	case !config.ValidNodeID(r.Node):
		return invalid(nil, "%q isn't a node id any node can run under. One "+
			"starts with a letter or a digit and holds only letters, digits, "+
			"'.', '_' or '-', at most 64 characters", r.Node)
	case r.OpID == "":
		return invalid(nil, "the gate on %s has no operation id, and a retry "+
			"under a fresh one would be a second gesture rather than the same "+
			"one finished", r.Node)
	case opID != nil:
		var refused *statelog.CallerOpIDError
		if errors.As(opID, &refused) {
			return invalid(opID, "the operation id %s; send back the op_id an "+
				"earlier answer returned, unchanged, or leave it out to start "+
				"afresh", refused.Fault)
		}
		return invalid(opID, "the gate on %s carries an operation id this "+
			"engine wouldn't have minted", r.Node)
	case iam.RecordOwner(r.By) == "":
		return invalid(nil, "the gate on %s names no operator, and every "+
			"log's record carries who ran it", r.Node)
	}
	return nil
}

// operator is who ran the gesture as the log lines name them: the author and
// the credential, the two columns an operator searches the trail by.
func (r GateRequest) operator() (name, credential string) {
	a := iam.ActorFor(r.By)
	return a.Name, a.OperatorID
}

// GateUnjudged is an eviction nobody could judge: the presence leases it is
// decided against could not be read, and the request did not force it.
//
// ITS OWN TYPE, NOT AN [*statelog.EvictionRefusal], because the remedy is the
// opposite one: a refusal names a node still reaching the fleet and says to
// stop it, and this names a coordination read that failed on THIS node — the
// target may well be gone, which is the case eviction exists for.
type GateUnjudged struct {
	Node string
	Err  error
}

func (e *GateUnjudged) Error() string {
	return fmt.Sprintf("engine: the eviction of %s cannot be judged, since a lease "+
		"listing nobody could read is not one that came back clear: %v", e.Node, e.Err)
}

func (e *GateUnjudged) Unwrap() error { return e.Err }

// Tell is the refusal as the operator who asked reads it ([statelog.Told]):
// what couldn't be judged, without the read that failed, which the log keeps.
func (e *GateUnjudged) Tell() string {
	return fmt.Sprintf("couldn't tell whether %s is still running, because this "+
		"node couldn't read the presence leases, so nothing was written", e.Node)
}

// Remedy is what the operator does about it: ask again once coordination
// answers, or force the eviction, which does not need the leases. Its detail
// names no surface's controls ([statelog.GateAction]): a coordination fault is
// when an absent node most needs evicting, so this often meets an operator in
// a browser, which has no flags.
func (e *GateUnjudged) Remedy() statelog.GateRemedy {
	return statelog.GateRemedy{
		Actions: []statelog.GateAction{statelog.GateRetrySameOp, statelog.GateForce},
		Detail: fmt.Sprintf("ask again once coordination answers, or, if you know "+
			"%s is gone, force the eviction, which doesn't need the leases", e.Node),
	}
}

// ReadmissionUnjudged is a readmission refused because something it is judged
// against could not be read: nothing was written anywhere. The judgement
// covers every log at once ([stateLog.Readmissible]), so one unreadable input
// refuses it whole: a register nobody could list is not one without the node,
// and an unread floor is not a low one. Its own type beside
// [*statelog.ReadmissionRefusal] because the remedy differs: that one says
// catch up, this one says the judgement could not be made.
type ReadmissionUnjudged struct {
	Node string

	// Log is the log whose bound could not be read, by its register key —
	// empty where what failed is the fleet's (the positions register, the
	// published floors).
	Log string

	// Behind is a log the fleet has re-anchored and this node hasn't applied
	// yet ([statelog.ReanchorNotApplied]): it read the bound, and the bound
	// is in a generation this node doesn't hold.
	Behind bool

	Err error
}

func (e *ReadmissionUnjudged) Error() string {
	if e.Log == "" {
		return fmt.Sprintf("engine: the readmission of %s cannot be judged: %v", e.Node, e.Err)
	}
	return fmt.Sprintf("engine: the readmission of %s cannot be judged on %s: %v",
		e.Node, e.Log, e.Err)
}

func (e *ReadmissionUnjudged) Unwrap() error { return e.Err }

// Tell is the refusal as the operator who asked reads it ([statelog.Told]):
// what couldn't be judged, naming the log where one was being read, without
// the read that failed, which the log keeps.
func (e *ReadmissionUnjudged) Tell() string {
	if e.Log == "" {
		return fmt.Sprintf("couldn't judge whether %s can be counted again, "+
			"because this node couldn't read the fleet's positions or floors, so "+
			"nothing was written", e.Node)
	}
	if e.Behind {
		return fmt.Sprintf("couldn't judge whether %s can be counted again on %s, "+
			"because the fleet has re-anchored that log and this node hasn't "+
			"applied it yet, so nothing was written", e.Node, e.Log)
	}
	return fmt.Sprintf("couldn't judge whether %s can be counted again on %s, "+
		"because this node couldn't read that log's bound, so nothing was "+
		"written", e.Node, e.Log)
}

// Remedy is what the operator does about it: ask again once the read answers,
// here or through another node, which reads the same inputs from its own
// connection and its own copy.
func (e *ReadmissionUnjudged) Remedy() statelog.GateRemedy {
	if e.Behind {
		return statelog.GateRemedy{
			Actions: []statelog.GateAction{statelog.GateRetrySameOp, statelog.GateOtherNode},
			Detail: fmt.Sprintf("this node hasn't applied the fleet's re-anchor of %s "+
				"yet, so it can't judge the readmission of %s there and wrote "+
				"nothing: ask a node that has applied it, or ask here again once "+
				"this node has", e.Log, e.Node),
		}
	}
	return statelog.GateRemedy{
		Actions: []statelog.GateAction{statelog.GateRetrySameOp, statelog.GateOtherNode},
		Detail: fmt.Sprintf("this node could not read what the readmission of %s is "+
			"judged against, and wrote nothing: ask again once it answers, or through "+
			"another node", e.Node),
	}
}

// GateResult is what one gesture did to every log it had to reach.
type GateResult struct {
	Node string
	OpID string

	// Domains is one entry per identity-claiming log, in the register's own
	// order — every one of them, whatever it answered.
	Domains []DomainGate
}

// Complete reports whether every log holds the gate record durably — applied
// on this node, or pending here and applied by every node as it reaches it.
// False is not a failure of the whole: the logs that answered hold their
// record, and a retry under the same [GateResult.OpID] writes only what is
// missing, where the log's answer says a retry can ([DomainGate.Retry]).
func (r GateResult) Complete() bool {
	for _, d := range r.Domains {
		if !d.done() {
			return false
		}
	}
	return len(r.Domains) > 0
}

// DomainGate is one log's answer to one gesture.
type DomainGate struct {
	// Domain names the log as the positions register keys it, and Stream as
	// the broker does.
	Domain string
	Stream string

	// OpID is the operation this log's record is published under, derived
	// from the gesture's.
	OpID string

	// Duplicates is the log's duplicate window: how long the broker holds an
	// operation id on the record it first landed, collapsing the same id
	// onto it. A record of this gesture's that landed and applies nowhere
	// ([statelog.Unavailable.Landed]) keeps the id spent that long, which
	// the remedy says.
	Duplicates time.Duration

	// Outcome is the write's own three-valued answer, and EMPTY when Err is
	// set: a write that answered with an error gave no outcome.
	Outcome  statelog.Outcome
	Position statelog.Position

	// Unvouched says an `unknown` Outcome came because this node's ledger
	// cannot vouch for the operation, minted before rows it may have lost
	// ([statelog.Result.Unvouched]): nothing was published, and the same
	// gesture here answers the same way every time, which decides the remedy.
	Unvouched bool

	// Err is why this log gave no outcome: a refusal naming its reason
	// (a [*statelog.Unavailable]), or a failure before the write could
	// answer. Whether a rerun can clear it is [DomainGate.Retry]'s.
	Err error
}

// done reports a log that holds the record durably.
func (d DomainGate) done() bool {
	return d.Err == nil &&
		(d.Outcome == statelog.OutcomeApplied || d.Outcome == statelog.OutcomePending)
}

// Retry reports whether running the same gesture again, under the same
// operation id, can finish this log: false for a finished log and for one
// whose refusal no retry clears (an evicted node, a log rebuilt under this
// node, a full log, an operation superseded since), which fails the same way
// for ever. The route and the command both render this one judgement, so they
// never advise two different things.
func (d DomainGate) Retry() bool { return d.Remedy().Offers(statelog.GateRetrySameOp) }

// Remedy is what the operator does about a log the gesture did not finish, or
// the zero remedy for one it did. Surfaces switch on the actions, and the
// detail names no surface's controls: the command line renders
// [statelog.GateNewGesture] as "without -op-id", the dashboard as starting
// afresh.
func (d DomainGate) Remedy() statelog.GateRemedy {
	if d.done() {
		return statelog.GateRemedy{}
	}
	retry := func(detail string, also ...statelog.GateAction) statelog.GateRemedy {
		return statelog.GateRemedy{
			Actions: append([]statelog.GateAction{statelog.GateRetrySameOp}, also...),
			Detail:  detail,
		}
	}
	only := func(action statelog.GateAction, detail string) statelog.GateRemedy {
		return statelog.GateRemedy{Actions: []statelog.GateAction{action}, Detail: detail}
	}
	if d.Err == nil && d.Unvouched {
		// UNKNOWN, AND NOT FOR THIS NODE TO SETTLE: the operation predates
		// rows this node's ledger may have lost (its sweep, or a donor's
		// inherited with an adopted snapshot), so it answers `unknown` here
		// every time. No retry_same_op: only a node whose ledger reaches
		// back that far can answer it, under the same id.
		return only(statelog.GateOtherNode, "this node cannot tell whether "+
			"the record landed: its operation ledger may have lost the record "+
			"of this operation, which was minted before the cutoff of the "+
			"ledger's thirty-day sweep, so the same gesture here answers the same way "+
			"every time. Run it through a node whose ledger reaches back that "+
			"far, under the same operation id")
	}
	if d.Err == nil {
		// UNKNOWN: the record may or may not be on the log; the log's own
		// ledger can tell, next time.
		return retry("its outcome is unknown: the same gesture under the same " +
			"operation id answers from this log's own ledger if the record landed, " +
			"and writes it if it did not")
	}
	var refusal *statelog.Unavailable
	if errors.As(d.Err, &refusal) {
		if refusal.CopyWriter != "" {
			// ANOTHER NODE'S COPY, whatever the gate: the reason is that
			// node's standing, and every case below is about this one's.
			return retry(d.anotherNodesCopy(refusal))
		}
		switch refusal.Reason {
		case statelog.ReasonEvicted:
			// THREE SHAPES, told apart by whose record the refusal names
			// ([statelog.Unavailable.Landed]), never by whether it names a
			// position, which may be another operation's record, perhaps a
			// healthy peer's.
			switch {
			case refusal.Landed():
				return only(statelog.GateOtherNode, "this node is evicted itself, and "+
					d.landedNowhere(refusal.Position, "a node the fleet still counts"))
			case !refusal.Position.IsZero():
				return only(statelog.GateOtherNode, d.heldAt(refusal.Position))
			}
			return only(statelog.GateOtherNode, "this node is evicted itself and "+
				"writes nothing to any log: run the gesture through a node the fleet "+
				"still counts, under the same operation id")
		case statelog.ReasonOvertaken:
			// A RECORD A RESTORED REANCHOR OVERTOOK: written from rows the
			// reanchor did not keep. Always one that landed, and this node
			// refuses the log until it re-keys to the new generation.
			return only(statelog.GateOtherNode, "a restored reanchor had overtaken "+
				"this node's rows when it wrote, and "+d.landedNowhere(refusal.Position,
				"a node on the log's current generation"))
		case statelog.ReasonAbandoned:
			return only(statelog.GateOtherNode, "this node wrote in a generation a "+
				"reanchor abandoned, and "+d.landedNowhere(refusal.Position,
				"a node the fleet still counts"))
		case statelog.ReasonWrongStream:
			return only(statelog.GateReanchor, fmt.Sprintf("the log under %s's name "+
				"is not the one this node's rows were derived from: re-anchor it "+
				"first, since nothing can be written to it until then, and then the "+
				"same gesture under the same operation id finishes it", d.Stream))
		case statelog.ReasonLogFull:
			// PAST THE GATE RESERVE: a gate record is admitted into room
			// kept above the ordinary writes' ceiling, so `log_full` here
			// means even that is spent, which no retry refills.
			return only(statelog.GateSetCapacity, fmt.Sprintf("%s is full to its "+
				"broker ceiling, past even the reserve kept there for gate records: "+
				"raise its ceiling, which is the only thing that makes room for it. "+
				"Then the same gesture under the same operation id finishes it, where "+
				"a fresh one would write every log that already holds the record "+
				"again", d.Stream))
		case statelog.ReasonSuperseded:
			return only(statelog.GateNewGesture, "a later gate record has undone "+
				"this operation's since: start a new gesture, under a fresh operation "+
				"id, if the node should change again")
		case statelog.ReasonOpReused:
			return only(statelog.GateNewGesture, "the operation id already names a "+
				"record on another object: start the gesture again under a fresh "+
				"operation id")
		case statelog.ReasonSkew:
			return only(statelog.GateRestore, "a store and a stream were restored "+
				"out of step, which no retry clears: restore them from one backup")
		case statelog.ReasonBehind:
			return retry("this node is catching up with the log, which clears on " +
				"its own: then the same gesture under the same operation id finishes it")
		case statelog.ReasonDeferred:
			return retry("this node holds a record it cannot decode: a node "+
				"running a newer build can finish the same gesture under the same "+
				"operation id", statelog.GateOtherNode)
		case statelog.ReasonFloorUnknown:
			return retry("the trim floor could not be read: the same gesture " +
				"under the same operation id finishes it once coordination answers")
		case statelog.ReasonEvictionUnknown:
			return retry("this node could not read its own eviction state, which " +
				"clears the moment it reads again: then the same gesture under the " +
				"same operation id finishes it")
		case statelog.ReasonBelowFloor:
			return retry("this node is adopting a peer's snapshot: the same "+
				"gesture under the same operation id finishes it once it has, here "+
				"or through another node", statelog.GateOtherNode)
		}
		// EVERY OTHER REASON, in the framework's two classes: one that
		// clears on this node by itself ([statelog.Reason.Retryable]) is
		// offered the retry, and each is advised in the REFUSAL'S OWN
		// WORDS, which say what clears it (the limit a too-large record
		// hit and what moves it, the broker's words on a refusal it named).
		own := ""
		if refusal.Detail != "" {
			own = "; " + refusal.Detail
		}
		if refusal.Reason.Retryable() {
			return retry("this refusal (" + string(refusal.Reason) + ") clears on " +
				"this node by itself" + own)
		}
		return statelog.GateRemedy{
			Detail: "no retry clears this refusal (" + string(refusal.Reason) + ")" + own,
		}
	}
	if errors.Is(d.Err, statelog.ErrConflict) {
		return retry("the node's gate subject kept changing under this write: the " +
			"same gesture under the same operation id finishes it")
	}
	return retry("the write failed before it could answer: the same gesture under " +
		"the same operation id finishes it")
}

// anotherNodesCopy is the remedy for a record of this gesture that ANOTHER
// node wrote, which this node's append was collapsed onto
// ([statelog.Unavailable.CopyWriter]) and a gate dropped: the id reached this
// node inside the duplicate window from a node that had written under it and
// was since evicted, or wrote in a generation a reanchor abandoned or
// overtook. Those three are exactly the gates that blame a writer
// ([statelog.Reason.BlamesWriter]); the fallback names a reason a later build
// adds. The gate is about the copy's writer: this node passed its own fences,
// so it is the node to finish the gesture here once the broker lets go of the
// id, and cannot apply twice since the copy in the way applies nowhere.
func (d DomainGate) anotherNodesCopy(refusal *statelog.Unavailable) string {
	why := fmt.Sprintf("a gate dropped it (%s)", refusal.Reason)
	switch refusal.Reason {
	case statelog.ReasonEvicted:
		why = "that node is evicted"
	case statelog.ReasonOvertaken:
		why = "that node wrote it from rows a restored reanchor had overtaken"
	case statelog.ReasonAbandoned:
		why = "that node wrote it in a generation a reanchor abandoned"
	}
	return fmt.Sprintf("the record of this gesture at %s on %s is node %s's copy, "+
		"which this node's own write was collapsed onto, and it applies nowhere "+
		"because %s, a fact about node %s and not about this one. The broker holds "+
		"its operation id for %s from when it landed. Once that has passed, the same "+
		"gesture under the same operation id finishes it here: before then the id "+
		"is collapsed onto that record and refused the same way, and a fresh id "+
		"would write every log that already holds the gesture's record again",
		refusal.Position, d.Stream, refusal.CopyWriter, why, refusal.CopyWriter, d.window())
}

// window names the log's duplicate window for a remedy — its length where the
// gate knows it.
func (d DomainGate) window() string {
	if d.Duplicates > 0 {
		return fmt.Sprintf("the log's duplicate window, %s,", d.Duplicates)
	}
	return "the log's duplicate window"
}

// landedNowhere is the remedy for a record of this gesture that landed at `at`
// on this log and applies nowhere: finish it through another node, named by
// who, under the SAME operation id, once the broker has let go of it. Within
// the log's duplicate window the same id from any node is collapsed onto that
// record and refused the same way. A fresh id would be a SECOND gesture: every
// log already holding this one's record written again, its gate re-dated, and
// the first id answering `superseded`. The record in the way applies nowhere,
// so the same id after the window cannot apply twice.
func (d DomainGate) landedNowhere(at statelog.Position, who string) string {
	return fmt.Sprintf("the record this gesture put on %s at %s applies nowhere: "+
		"the broker holds its operation id for %s from when it landed. Once that "+
		"has passed, run the gesture through %s under the same operation id: "+
		"before then the id is collapsed onto that record and refused the same "+
		"way, and a fresh id would write every log that already holds the "+
		"gesture's record again", d.Stream, at, d.window(), who)
}

// heldAt is the remedy for a gesture refused `evicted` at ANOTHER operation's
// record, the newest on its subject when this log's answer was lost: the
// refusal is about this node, and the record at `at` may be a healthy peer's.
// Whatever this node appended under the operation lies below `at` and applies
// nowhere, so the gesture is finished through another node under the same id,
// at once. If this node's record did land, that node answers naming this one
// as the copy's writer and when the id may go again
// ([DomainGate.anotherNodesCopy]); a fresh id is never the way round it
// ([DomainGate.landedNowhere]).
func (d DomainGate) heldAt(at statelog.Position) string {
	return fmt.Sprintf("this node is evicted itself: the newest record on %s, at %s, "+
		"is another operation's, and whatever this node appended under this one lies "+
		"below it and applies nowhere, if it landed at all. Run the gesture through a "+
		"node the fleet still counts, under the same operation id. If this node's "+
		"record did land, the broker holds the id on it for %s from then, and that "+
		"node answers so and says when to send it again", d.Stream, at, d.window())
}

// NodeGate is the gesture, over every identity-claiming log this node runs —
// on a data node, every one in the register.
type NodeGate struct {
	// logs is every identity-claiming log as the gate writes it, in the
	// register's order, each through this node's own write authority; built
	// once ([newNodeGate]).
	logs []gateLog

	// live lists the nodes holding a presence lease, which an eviction is
	// judged against, and readmissible is the state log's own judgement of
	// a readmission.
	live         func(ctx context.Context) ([]statelog.Presence, error)
	readmissible func(ctx context.Context, nodeID string) error

	// publishing refuses a gesture on a node whose mode appends nothing
	// ([ErrNotPublishing]) — asked FIRST, before anything is judged, since
	// no judgement could make the write allowed.
	publishing func(gesture string) error

	// budgets are the gesture's two budgets: zero is the package's own
	// ([GateJudgeBudget], [GateLogBudget]); a test sets shorter ones.
	budgets gateBudgets
}

// gateBudgets is one gesture's budget per phase; a zero one is the package's
// constant for that phase.
type gateBudgets struct {
	judge, logs time.Duration
}

// orDefault is b with every phase it leaves zero set to the package's budget.
func (b gateBudgets) orDefault() gateBudgets {
	if b.judge == 0 {
		b.judge = GateJudgeBudget
	}
	if b.logs == 0 {
		b.logs = GateLogBudget
	}
	return b
}

// gateLog is one identity-claiming log as the gate writes it.
type gateLog struct {
	domain string
	stream string

	// duplicates is the log's duplicate window, which a remedy for a record
	// that landed and applies nowhere names ([DomainGate.Duplicates]).
	duplicates time.Duration

	// write publishes the log's own gate record, or for a retried operation
	// whose record is already the gate in force answers where it landed
	// ([statelog.GateStanding]).
	write gateWrite
}

// liveLeases is the slice of the coordination backend the gate and the trim
// both judge presence from.
type liveLeases interface {
	ListLive(ctx context.Context, class coord.Class) ([]coord.Lease, error)
}

// livePresences is every DATA node holding a live presence lease: one reading
// for the trim and the gate, which ask the same question. The trim counts such
// a node at position zero until its row lands ([statelog.CountedSet]); the
// gate refuses to evict one. A node without `data` publishes no position, so
// the trim counting it at zero would never advance, and the gate has no copy
// of it to fence.
func livePresences(ctx context.Context, leases liveLeases) ([]statelog.Presence, error) {
	ids, err := dataNodes(ctx, leases)
	if err != nil {
		return nil, fmt.Errorf("list the live nodes: %w", err)
	}
	return presencesOf(ids), nil
}

// presencesOf is the live nodes ids names, as a counted set takes them
// ([statelog.CountedSet]).
func presencesOf(ids []string) []statelog.Presence {
	out := make([]statelog.Presence, 0, len(ids))
	for _, id := range ids {
		out = append(out, statelog.Presence{NodeID: id})
	}
	return out
}

// domainOpID is one log's operation id for a gesture: STABLE, so a retry is the
// same operation, and DISTINCT per log (each ledger answers for its own
// record), per sign (an id carried from an eviction to a readmission is a new
// operation) and per target node (evicting node-b under node-a's gesture id is
// neither answered from node-a's ledger nor collapsed onto its record).
// Spelled in the state log's grammar ([statelog.StepOpID]) so it carries the
// gesture's mint instant the ledger vouches by: any other shape reads as
// minted at zero and answers `unknown` once a ledger has swept. INJECTIVE,
// which a plain join is not (dotted gesture tails and node ids): each step is
// one dot-free segment read from the right, the node escaped by
// [statelog.OpNamePart], and the log, holding no colon, ended by the colon
// after it. A collision would be refused ([statelog.ReasonOpReused]) anyway.
func domainOpID(gesture string, readmit bool, domain, node string) string {
	verb := "evict"
	if readmit {
		verb = "readmit"
	}
	return statelog.StepOpID(gesture, verb, domain+":"+statelog.OpNamePart(node))
}

// newNodeGate builds the gesture over every identity-claiming log this node
// runs, in the register's own order, each through the domain's OWN gate
// writer from its register entry rather than a surface's: a company on other
// backends may have no tracker or page store, and an eviction must reach
// every log. A log that claims identity with no writer refuses the boot, and
// before anything is provisioned ([checkRegister] asks every such entry for
// its [registration.NewGate]): an eviction would otherwise lift every pin but
// that one. BUILT ONCE: a data node runs every log in the register from boot,
// and none starts or stops while it runs.
func newNodeGate(s *stateLog, leases liveLeases) (*NodeGate, error) {
	g := &NodeGate{
		live: func(ctx context.Context) ([]statelog.Presence, error) {
			return livePresences(ctx, leases)
		},
		readmissible: s.Readmissible,
		publishing:   s.appends,
	}
	for _, running := range s.running() {
		if !running.domain.ClaimsIdentity() {
			continue
		}
		gl, err := gateLogFor(s, running, running.publisher)
		if err != nil {
			return nil, err
		}
		g.logs = append(g.logs, gl)
	}
	if len(g.logs) == 0 {
		return nil, errors.New("engine: no registered domain claims identity, so " +
			"there is no log an eviction could be written to")
	}
	return g, nil
}

// gateLogFor is one identity-claiming log's writer for the gate, publishing
// through publisher — the domain's own write authority — and built by the
// domain's register entry, the one place every domain is declared.
func gateLogFor(s *stateLog, running *runningLog,
	publisher *statelog.Publisher) (gateLog, error) {

	name := running.domain.Name()
	entry, found := registrationFor(name)
	if !found || entry.NewGate == nil {
		return gateLog{}, fmt.Errorf("engine: domain %q claims identity and the node "+
			"gate has no writer for its log: an eviction would lift every "+
			"other log's pin and leave the node counted on this one", name)
	}
	write, err := entry.NewGate(s, running, publisher)
	if err != nil {
		return gateLog{}, fmt.Errorf("engine: the node gate's writer for %s: %w", name, err)
	}
	return gateLog{domain: name, stream: running.spec.Name,
		duplicates: running.spec.Duplicates, write: write}, nil
}

// Evict removes a node from every identity-claiming log's counted set.
//
// JUDGED BEFORE ANYTHING IS WRITTEN: a node holding a live presence lease is
// refused unless forced, and an unreadable lease listing refuses too
// ([*GateUnjudged]). Past the judgement the error is nil and every log's own
// answer is in the result. Force decides the unreadable listing as well: the
// listing is the judgement's only input, force says it does not decide this
// node, and a coordination fault is when an absent node most needs evicting.
// A forced eviction past an unreadable listing or a live lease is logged, as
// the one gesture no evidence cleared.
func (g *NodeGate) Evict(ctx context.Context, req GateRequest) (GateResult, error) {
	if err := req.valid(); err != nil {
		return GateResult{}, err
	}
	if err := g.publishing("an eviction"); err != nil {
		return GateResult{}, err
	}
	judge, cancel := context.WithTimeout(ctx, g.budgets.orDefault().judge)
	defer cancel()
	live, err := g.live(judge)
	switch {
	case err != nil && !req.Force:
		return GateResult{}, &GateUnjudged{Node: req.Node, Err: err}
	case err != nil:
		by, credential := req.operator()
		log.WarnContext(ctx, "retention_eviction_forced_unjudged",
			"node", req.Node, "by", by, "operator", credential, "op_id", req.OpID,
			"error", err.Error(),
			"detail", "the presence leases could not be read, so nothing "+
				"established that the node is gone; the operator forced it")
	default:
		if err := statelog.PermitEviction(req.Node, live, req.Force); err != nil {
			return GateResult{}, fmt.Errorf("engine: evict node %s: %w", req.Node, err)
		}
		if req.Force && slices.ContainsFunc(live, func(p statelog.Presence) bool {
			return p.NodeID == req.Node
		}) {
			by, credential := req.operator()
			log.WarnContext(ctx, "retention_eviction_forced_live",
				"node", req.Node, "by", by, "operator", credential, "op_id", req.OpID,
				"detail", "the node holds a live presence lease and the "+
					"operator forced its eviction past it")
		}
	}
	return g.write(judge, req, false), nil
}

// Readmit is the inverse commit on every identity-claiming log.
//
// JUDGED BEFORE ANYTHING IS WRITTEN, against every log at once: a node below a
// trim floor it would be counted against is refused with a
// [*statelog.ReadmissionRefusal] naming the log, its position and the floor,
// and nothing is appended anywhere — see [stateLog.Readmissible].
func (g *NodeGate) Readmit(ctx context.Context, req GateRequest) (GateResult, error) {
	if err := req.valid(); err != nil {
		return GateResult{}, err
	}
	if err := g.publishing("a readmission"); err != nil {
		return GateResult{}, err
	}
	judge, cancel := context.WithTimeout(ctx, g.budgets.orDefault().judge)
	defer cancel()
	if err := g.readmissible(judge, req.Node); err != nil {
		return GateResult{}, fmt.Errorf("engine: readmit node %s: %w", req.Node, err)
	}
	return g.write(judge, req, true), nil
}

// write publishes the gate record to every identity-claiming log at once (a
// log that refuses or cannot answer stops no other, since each gate is
// independent), UNDER ITS OWN BUDGET, NOT THE CALLER'S: a request abandoned during the
// judgement wrote nothing, but past it the gesture is half-done the moment it
// stops, and a caller going away is no request to leave a node evicted on one
// log and counted on another. The context's values travel, so each write keeps
// the trace and the operator it was asked under.
func (g *NodeGate) write(judge context.Context, req GateRequest, readmit bool) GateResult {
	budgets := g.budgets.orDefault()
	detached := context.WithoutCancel(judge)
	ctx, cancel := context.WithTimeout(detached, budgets.logs)
	defer cancel()
	out := GateResult{Node: req.Node, OpID: req.OpID, Domains: make([]DomainGate, len(g.logs))}
	var wg sync.WaitGroup
	for i, l := range g.logs {
		// EACH ENTRY ITS OWN GOROUTINE'S, so answers keep the logs' order.
		d := &out.Domains[i]
		*d = DomainGate{Domain: l.domain, Stream: l.stream, Duplicates: l.duplicates,
			OpID: domainOpID(req.OpID, readmit, l.domain, req.Node)}
		wg.Go(func() {
			res, err := l.write(ctx, req.By, d.OpID, req.Node, readmit)
			if err != nil {
				d.Err = err
				return
			}
			d.Outcome, d.Position = res.Outcome, res.Position
			d.Unvouched = res.Outcome == statelog.OutcomeUnknown && res.Unvouched
		})
	}
	wg.Wait()
	return out
}
