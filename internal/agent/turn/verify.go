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

// Reply says who is waiting for this turn, how they get an answer, and WHERE
// they are waiting for it.
//
// Derived at dispatch from the trigger's own type, never from anything the
// model says: intent was the half of the old gate a model could get wrong, and
// this is the half it cannot.
//
// A VALUE RATHER THAN AN ENUM, because the kind alone answers the wrong
// question. "Somebody is waiting on a tool" is satisfied by any tool that
// reaches anybody, so a seat woken by a Mattermost DM discharged it with a
// work-item write: the founder was asked to wait, the tracker got a row, and
// the turn closed `done` having answered nobody who was listening. The two
// facts travel together or the gate compares one of them against nothing.
type Reply struct {
	// Kind is the shape of the obligation.
	Kind ReplyKind

	// Surface is where the asker is waiting, in the same vocabulary a
	// delivering tool reports — an MCP server's name for a vendor, or one
	// of the engine's own surfaces.
	//
	// EMPTY IS A REAL ANSWER and not a missing one: "somebody is waiting
	// and the engine cannot say where". An assignment is that case — the
	// answer may legitimately land on the tracker, in chat, or on the
	// ticket — and so is any trigger type that names no source. The gate
	// falls back to "any delivery counts" there, because narrowing on a
	// surface nobody chose would refuse correct turns.
	Surface string
}

// ReplyKind is the shape of a turn's delivery obligation.
type ReplyKind string

const (
	// ReplyUnset is the zero value, and it is NOT a setting. [Run] refuses
	// it, and that refusal is the point of naming it.
	//
	// Each of the three real values has a different consequence, and none
	// of them is a safe reading of "the caller did not say": permissive
	// exempts a turn that owes somebody an answer, strict loops every
	// unaddressed turn to exhaustion. A field whose zero value cannot be
	// either is one the type has to refuse.
	//
	// It was read as the permissive one for as long as this was an
	// unnamed "". The ordinary dispatch path built its [Input] without a
	// Reply — the value was computed in the same frame and handed to the
	// runner, just never to the loop — so every gate that turns on it,
	// [Check]'s two corrections and [OverrideDone], was inert in
	// production while this package's own suite exercised all of them.
	// The seat that found it filed a work item, told nobody, and closed
	// the turn `done`.
	ReplyUnset ReplyKind = ""

	// ReplyNone — nobody asked. A schedule fired, a broadcast mentioned
	// the seat in passing, an internal event woke it. Such a turn may end
	// having done nothing at all, which is what makes triage cheap.
	ReplyNone ReplyKind = "none"

	// ReplyTool — somebody is waiting on a surface the seat reaches with
	// its own tools: a chat mention, an issue comment, an assignment. The
	// answer only exists if a tool put it there.
	ReplyTool ReplyKind = "tool"

	// ReplyEngine — a colleague asked over A2A, and the engine itself
	// returns the turn's artifact on the channel the ask opened. The seat
	// delivers by ANSWERING; there is no tool for it to call, and demanding
	// one would loop every colleague exchange to exhaustion.
	ReplyEngine ReplyKind = "engine"
)

// NoReply is the obligation of a turn nobody asked for.
func NoReply() Reply { return Reply{Kind: ReplyNone} }

// EngineReply is the obligation of an A2A ask, which the engine answers itself.
func EngineReply() Reply { return Reply{Kind: ReplyEngine} }

// ToolReply is the obligation of a turn somebody is waiting on, optionally
// naming the surface they are waiting on. Pass "" when the trigger does not
// say — see [Reply.Surface].
func ToolReply(surface string) Reply { return Reply{Kind: ReplyTool, Surface: surface} }

// Valid reports whether this is one of the three real answers.
//
// [ReplyUnset] is not one, deliberately — see its own doc. An unrecognised
// value off the wire is not one either: a pending sandbox run carries its
// Reply as a stored string, and a build that reads a value it does not know
// must say so rather than fall through to whichever branch happens to be
// last.
func (r Reply) Valid() bool {
	return r.Kind == ReplyNone || r.Kind == ReplyTool || r.Kind == ReplyEngine
}

// String encodes the obligation for the one place it is stored rather than
// passed: a suspended sandbox run's row, which outlives the process that
// parked it. `tool:mattermost`, or the bare kind when no surface is named.
func (r Reply) String() string {
	if r.Surface == "" {
		return string(r.Kind)
	}
	return string(r.Kind) + ":" + r.Surface
}

// ParseReply decodes what [Reply.String] wrote. An unrecognised kind comes
// back as [ReplyUnset] rather than an error, which [Run] then refuses by the
// same rule as any other invalid input — one refusal, in one place.
func ParseReply(s string) Reply {
	kind, surface, _ := strings.Cut(s, ":")
	return Reply{Kind: ReplyKind(kind), Surface: surface}
}

// Awaited reports whether anyone is waiting for this turn's answer.
func (r Reply) Awaited() bool { return r.Kind == ReplyTool || r.Kind == ReplyEngine }

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
// ONE MEMBERSHIP TEST, against a map the registry computed — see
// [tools.Registry.Deliveries]. Two kinds of tool are in it: an MCP-served
// tool that its own annotations do not POSITIVELY call a read (the
// fail-closed direction, since treating unannotated as read would exempt
// every tool a server forgot to annotate), and a first-party tool the engine
// registered as one that reaches somebody.
//
// WHETHER, NOT WHERE. This is the question [Acted] and the ledger need — did
// anything irreversible happen — and it is deliberately NOT the one the
// delivery gate asks. [DeliveredTo] is that one, and it reads the same map's
// VALUES rather than its keys.
//
// The rule this replaces was an ORIGIN test — server-backed and not a known
// read — and it was right for as long as every shared surface belonged to
// somebody else. It stopped being right when the tracker moved in-process:
// commenting on a native work item reaches the person who asked, and a gate
// keyed on origin judged that turn to have answered nobody and looped it to
// failure. What has NOT changed is that a diary write still does not deliver:
// the engine declares which of its own tools reach anybody, one at a time.
func Deliverable(name string, s Surface) bool {
	_, ok := s.Deliveries[name]
	return ok
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

// DeliveredTo reports whether this turn's record shows a call that reached the
// party who is waiting, ON THE SURFACE THEY ARE WAITING ON.
//
// THE QUESTION [Delivered] SHOULD HAVE BEEN ASKING. A flat "did any tool that
// reaches anybody run" was right for as long as a seat held tools for one
// shared surface and no more. It stopped being right when the tracker moved
// in-process and every seat gained deliverable builtins: a turn woken by a
// Mattermost DM then discharged its obligation by filing a work item, and the
// founder — who had been asked to wait for a clarification — was never told
// the task existed. Every layer passed. Nobody was answered.
//
// NARROWED ONLY WHERE IT CAN BE, and the two fallbacks are the design rather
// than leniency:
//
//   - An obligation with NO surface is one the engine could not place. An
//     assignment is the honest example: the answer may land on the tracker,
//     in the thread, or on the ticket, and the engine has no basis to pick.
//     Narrowing on a surface nobody chose would refuse correct turns.
//   - A surface THIS SEAT CANNOT REACH leaves nothing to enforce. If no tool
//     on the surface delivers there, the seat could not have answered there
//     however many rounds it spent, and holding the turn open until it did
//     would loop it to exhaustion over an operator's missing integration.
//
// In both, the flat question is the answer, which is today's behaviour — so
// this is strictly stricter than what it replaces and never stricter than the
// seat can satisfy.
func DeliveredTo(calls []ledger.Call, s Surface, reply Reply) bool {
	want := reply.Surface
	if want == "" || !s.Reaches(want) {
		return Delivered(calls, s)
	}
	for _, c := range calls {
		if !c.Failed && s.Deliveries[c.Name] == want {
			return true
		}
	}
	return false
}

// Answered reports whether the party waiting on this turn has its answer.
//
// THREE OBLIGATIONS, THREE ANSWERS, and only one of them is a question about
// tool calls at all:
//
//   - [ReplyTool] — somebody is waiting on a surface the seat reaches with its
//     own tools, so the record has to show a call that reached THEM. This is
//     the only case [DeliveredTo] can speak to.
//   - [ReplyEngine] — the engine returns the turn's artifact on the channel
//     the ask opened. It is delivered by construction and no tool call will
//     ever show it, so reading the record would report every colleague
//     exchange as unanswered.
//   - [ReplyNone] — nobody asked, so there is no answer anybody is missing. A
//     turn that ends in prose has not failed to reply; it had nobody to reply
//     to, which is the whole reason triage is cheap.
//
// It exists for the conversation ledger, which has to tell "the seat said
// this" from "the seat concluded this and nobody heard it". Asking the tool
// record on all three filed an A2A answer and an unprompted turn's own notes
// as things that never reached anybody — which is the same false record this
// was written to stop, pointed the other way.
func Answered(calls []ledger.Call, s Surface, reply Reply) bool {
	if reply.Kind != ReplyTool {
		return true
	}
	return DeliveredTo(calls, s, reply)
}

// DeliverersFor names the tools on this surface that reach the waiting party,
// so a refusal can say which ones would actually have counted.
//
// The refusal this feeds is read by a model that has just cited the wrong
// tool, and the list is the whole instruction: naming every deliverable
// including the ones on other surfaces is how it cited create_work_item at a
// founder waiting in a chat thread.
func DeliverersFor(s Surface, reply Reply) []string {
	out := make([]string, 0, len(s.Deliveries))
	for name, surface := range s.Deliveries {
		if reply.Surface == "" || !s.Reaches(reply.Surface) || surface == reply.Surface {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// Acted reports whether this round's record PROVES the turn already reached
// outside the engine — that re-running it would repeat something it cannot
// take back.
//
// A DIFFERENT QUESTION FROM [Delivered], and the two must not be merged.
// Delivered asks whether an answer reached the person who is waiting, so it is
// MCP-only on purpose: a first-party builtin "never counts however much it
// writes". This asks whether anything irreversible happened at all, so a
// builtin that wakes a colleague or starts a billed box counts exactly as much
// as a Jira comment does.
//
// PROOF, NOT SUSPICION, and that asymmetry is the whole design. Its caller
// spends a trigger on a true answer — a redelivery that would have re-run this
// turn is given up instead — so a false positive discards work that never left
// the process, while a false negative merely leaves today's behaviour in place.
// Both halves are therefore positive: an MCP-backed call not proven read-only
// (the rule [Deliverable] has survived two incidents on), or a tool whose own
// annotations say open-world. A tool nobody classified proves nothing and is
// not counted.
//
// SUCCESSFUL calls only, like Delivered: a post that failed did not post.
func Acted(calls []ledger.Call, s Surface) bool {
	for _, c := range calls {
		if c.Failed {
			continue
		}
		if Deliverable(c.Name, s) || slices.Contains(s.KnownOpenWorld, c.Name) {
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
	// TWO PREDICATES, TWO QUESTIONS, and they are deliberately not one
	// variable. `acted` asks whether anything at all reached outside the
	// engine, which is what makes ending a turn as "nobody was asking"
	// wrong; `delivered` asks whether the party WAITING was reached, which
	// is what makes a claim of delivery wrong. A turn can satisfy the first
	// and fail the second — it files a ticket while a founder waits in chat
	// — and collapsing them lets that turn pass one check under the other's
	// name.
	acted := Delivered(w.Calls, s)
	delivered := DeliveredTo(w.Calls, s, reply)

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
	if w.Outcome == OutcomeDelivered && reply.Kind == ReplyTool && !delivered {
		// NAMED WHERE IT IS KNOWN, exactly as [OverrideDone] and the
		// submission check do. A turn that DID write somewhere is told "no
		// tool was called", reads that as false against its own record, and
		// argues with the correction instead of acting on it.
		if on := reply.Surface; on != "" && s.Reaches(on) && acted {
			return Verdict{Correction: "You reported the work as delivered, and you " +
				"did act — but nothing was delivered on " + on + ", which is where " +
				"this was asked. Filing the work somewhere else does not tell the " +
				"person waiting. Call the tool that delivers on " + on + " — " +
				"discovering it with `list_mcp_server_tools` and `activate_tool` if " +
				"you do not have it yet — or report honestly what is blocking you."}
		}
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
	if reply.Kind != ReplyTool || DeliveredTo(w.Calls, s, reply) {
		return false, ""
	}
	if on := reply.Surface; on != "" && s.Reaches(on) {
		// NAMED, because this is the case the flat check used to pass. The
		// turn DID reach somebody — just not the person waiting — so a
		// correction saying "no tool was called" would read as false to a
		// model looking at its own successful write, and it would submit
		// the same citation again.
		return true, "This turn reached somebody, but not the person waiting for it: " +
			"nothing was delivered on " + on + ", which is where this was asked. " +
			"Call the tool that delivers there — `list_mcp_server_tools` and " +
			"`activate_tool` will find it — before reporting the work delivered."
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
