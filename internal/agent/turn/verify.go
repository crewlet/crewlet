package turn

import (
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/ledger"
)

// Whether a turn actually delivered, judged from the engine's own record.
//
// The question this file answers used to be asked of a PREDICTION: the planner
// named the tools it expected to call, and the engine reconciled that list
// against what ran. Both of the recorded failures came from that
// reconciliation. Too strict and a real delivery through a tool the executor
// discovered read as undelivered, the override fired, and the seat posted the
// same message twice. Too loose and a turn whose named delivery tool was a
// wrong guess -- which then called nothing but a builtin -- read as delivered,
// so "reply hi" produced text, never called the chat tool, and completed
// silently having acted on nothing.
//
// With one loop there is no prediction to reconcile. What replaces it is two
// facts the model does not control: WHO IS WAITING, which the engine derives
// from the trigger before the turn starts, and WHAT ACTUALLY RAN, which the
// tool loop recorded as it happened.

// Reply says who is waiting for this turn, and how they get an answer.
//
// Derived at dispatch from the trigger's own type, never from anything the
// model says: intent was the half of the old gate a model could get wrong, and
// this is the half it cannot.
type Reply string

const (
	// ReplyNone — nobody asked. A schedule fired, a broadcast mentioned
	// the seat in passing, an internal event woke it. Such a turn may end
	// having done nothing at all, which is what makes triage cheap.
	ReplyNone Reply = "none"

	// ReplyTool — somebody is waiting on a surface the seat reaches with
	// its own tools: a chat mention, an issue comment, an assignment. The
	// answer only exists if a tool put it there.
	ReplyTool Reply = "tool"

	// ReplyEngine — a colleague asked over A2A, and the engine itself
	// returns the turn's artifact on the channel the ask opened. The seat
	// delivers by ANSWERING; there is no tool for it to call, and demanding
	// one would loop every colleague exchange to exhaustion.
	ReplyEngine Reply = "engine"
)

// Awaited reports whether anyone is waiting for this turn's answer.
func (r Reply) Awaited() bool { return r == ReplyTool || r == ReplyEngine }

// Outcome is what the executor says it did.
type Outcome string

const (
	// OutcomeDelivered — the work is done and, where a tool was the way to
	// deliver it, the tool ran.
	OutcomeDelivered Outcome = "delivered"

	// OutcomeNoAction — nobody was asking this seat to do anything. The
	// turn ends and nothing is posted.
	//
	// Deliberately NOT the way to decline. A seat that was mentioned,
	// assigned or asked and is saying no must say so where it was asked,
	// so the requester learns the message landed rather than waiting in
	// silence for an answer that is never coming.
	OutcomeNoAction Outcome = "no_action"

	// OutcomeBlocked — the work cannot proceed and the executor says why.
	// It still reaches the reviewer, which decides whether being blocked
	// is the honest end of this turn or a round that gave up early.
	OutcomeBlocked Outcome = "blocked"

	// OutcomeIncomplete is ENGINE-SYNTHESISED when the executor never
	// submitted anything at all.
	//
	// Its own value rather than a defaulted `delivered`, because the two
	// must stay distinguishable: an engine-written word carries none of
	// the model's commitment, and every rescue path in this loop turns on
	// telling "the executor decided this" from "nothing decided anything".
	OutcomeIncomplete Outcome = "incomplete"
)

// Deliverable reports whether calling this tool could deliver something to a
// surface outside the engine.
//
// SERVER-BACKED AND NOT A KNOWN READ, which is the rule the phantom-era gate
// fell back on and the one that survived both incidents. A delivery to a
// shared surface only ever comes from an MCP server, so a first-party builtin
// never counts however much it writes: reflect_and_persist records a thought,
// use_skill loads a page, and neither is an answer anybody is waiting for.
//
// "Not a known read" is POSITIVE: a tool is exempt only when its own
// annotations say it is read-only. An unannotated tool counts as a possible
// delivery, which is the fail-closed direction — the alternative exempts every
// tool a server forgot to annotate.
func Deliverable(name string, s Surface) bool {
	return slices.Contains(s.MCPTools, name) && !slices.Contains(s.KnownReads, name)
}

// Delivered reports whether anything in this turn's record could have reached
// somebody outside the engine.
//
// SUCCESSFUL calls only. A failed post did not post, and counting it would
// close the gate on exactly the turn that needs to iterate.
func Delivered(calls []ledger.Call, s Surface) bool {
	for _, c := range calls {
		if !c.Failed && Deliverable(c.Name, s) {
			return true
		}
	}
	return false
}

// Verdict is what the engine concludes about a round before the reviewer sees
// it.
type Verdict struct {
	// Skip ends the turn silently: nobody asked, and nothing external
	// happened.
	Skip bool

	// Correction is an engine instruction for the next round. Non-empty
	// means this round loops back without spending a review call.
	Correction string
}

// Check judges the executor's own account of a round against the record.
//
// BEFORE the reviewer, and cheaply: two of the three answers cost no model
// call at all. What it cannot do is judge whether the work was any GOOD —
// that is the reviewer's, and everything this returns without a correction
// goes there.
func Check(w Work, reply Reply, s Surface) Verdict {
	acted := Delivered(w.Calls, s)

	// A rescue never takes any fast path. The engine wrote the outcome,
	// so there is nothing here anybody committed to.
	if w.Rescued {
		return Verdict{}
	}

	if w.Outcome == OutcomeNoAction {
		switch {
		case reply.Awaited():
			// The one class the engine can prove: somebody asked. A turn
			// that answers silence to a direct request looks to the
			// requester exactly like a message that was lost.
			return Verdict{Correction: "You reported no_action, but this turn was asked " +
				"for directly and somebody is waiting on it. If you are declining, say " +
				"so where you were asked — a brief reply is a delivery. If you are not, " +
				"do the work and report what you did."}
		case acted:
			// Something already reached the outside world. Ending the turn
			// as "nobody was asking" would file a side effect nobody
			// reviewed, and leave the next turn on this thread with no
			// record that it happened.
			return Verdict{Correction: "You reported no_action, but this turn already " +
				"called a tool that acts outside the engine. Report what you actually " +
				"did instead."}
		default:
			return Verdict{Skip: true}
		}
	}

	// A claim of delivery is checked against the record only where a TOOL
	// was the way to deliver — the same condition [citations] and
	// [OverrideDone] use, and they must agree or the engine refuses at one
	// layer what it accepted at another.
	//
	// An unaddressed turn owes nobody a posted answer: a scheduled review
	// that read a dashboard and found nothing wrong has genuinely delivered,
	// and demanding a call there loops every research turn to exhaustion.
	// An A2A ask is answered by the engine itself, so there is no call to
	// look for.
	if w.Outcome == OutcomeDelivered && reply == ReplyTool && !acted {
		return Verdict{Correction: "You reported the work as delivered, but no tool " +
			"that acts outside the engine was called in this turn: writing about an " +
			"action does not perform it. Call the tool that actually delivers — " +
			"discovering it with `list_mcp_server_tools` and `activate_tool` if you " +
			"do not have it yet — or report honestly what is blocking you."}
	}
	return Verdict{}
}

// OverrideDone reports whether a reviewer's `done` has to be overturned.
//
// THE LAST LAYER, and it stays after [Check] because the two see different
// things: Check reads the executor's own claim before the reviewer looks at
// anything, while this reads the reviewer's verdict against the same record.
// The failure it exists for is the recorded one — the reviewer's model judges
// the produced TEXT, finds a good answer in it, and says done even though
// nothing put that answer anywhere a person can see.
//
// It fires only when somebody is waiting for a TOOL to have delivered.
// Nobody waiting means a research turn that legitimately ends in prose;
// waiting on the engine means the artifact reaches them either way.
func OverrideDone(w Work, reply Reply, s Surface) (override bool, correction string) {
	if reply != ReplyTool || Delivered(w.Calls, s) {
		return false, ""
	}
	return true, "This turn produced an answer as text, but no tool that acts outside " +
		"the engine was called: the requester will never see it. Call the tool that " +
		"delivers on the surface the request came from."
}

// AppendCorrection joins the reviewer's own notes to an engine correction.
//
// The engine's correction goes LAST because it is the one the next round must
// act on: on the override path the reviewer said done and wrote no correction
// of its own, so on that path this is the only instruction there is.
func AppendCorrection(notes, correction string) string {
	switch {
	case correction == "":
		return notes
	case strings.TrimSpace(notes) == "":
		return correction
	default:
		return notes + "\n\n" + correction
	}
}
