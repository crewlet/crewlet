package turn

import (
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/words"
)

// Whether a turn actually delivered, judged from the engine's own record.
//
// Nothing is predicted. The verdict rests on two facts the model does not
// control: WHO IS WAITING, which the engine derives from the trigger before the
// turn starts, and WHAT ACTUALLY RAN, which the tool loop recorded as it
// happened. Reconciling a named list of expected tools against what ran failed
// both ways: too strict posted a message twice, too loose let "reply hi"
// complete having called no chat tool.

// Reply says who is waiting for this turn, how they get an answer, and WHERE
// they are waiting for it.
//
// Derived at dispatch from the trigger's own type, never from anything the
// model says.
//
// A VALUE RATHER THAN AN ENUM, because the kind alone answers the wrong
// question: any tool that reaches anybody satisfies "somebody is waiting on a
// tool", so a seat woken by a Mattermost DM discharged it with a work-item
// write and closed `done` having answered nobody listening. Kind and surface
// travel together or the gate compares one against nothing.
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

	// Item is the work item a work pickup handed this turn, by id, and
	// empty for every other trigger. Its answer is delivered only by a
	// typed answer on that item, or by the item leaving the seat, which the
	// engine reads off its own record ([Work.Answer]): a comment alone
	// records nothing the engine can act on (ADR-0122).
	Item string
}

// ReplyKind is the shape of a turn's delivery obligation.
type ReplyKind string

const (
	// ReplyUnset is the zero value, and it is NOT a setting. [Run] refuses
	// it, and that refusal is the point of naming it.
	//
	// None of the three real values is a safe reading of "the caller did not
	// say": permissive exempts a turn that owes somebody an answer, strict
	// loops every unaddressed turn to exhaustion. A zero value that cannot be
	// either is one the type has to refuse.
	//
	// Read as permissive, it left every gate that turns on it ([Check]'s two
	// corrections, [OverrideDone]) inert in production while this package's
	// suite exercised all of them.
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
// value off the wire is not one either: a pending run carries its
// Reply as a stored string, and a build that reads a value it does not know
// must say so rather than fall through to whichever branch happens to be
// last. Nor is an item on anything but a tool obligation: a pickup's answer
// is a tool's write.
func (r Reply) Valid() bool {
	if r.Item != "" && r.Kind != ReplyTool {
		return false
	}
	return r.Kind == ReplyNone || r.Kind == ReplyTool || r.Kind == ReplyEngine
}

// itemMark separates a pickup's item from the rest of a stored obligation. An
// older build reads `tool@<id>` as a kind it does not know and refuses the
// resume, so the completion goes back for a build that holds the turn to it.
const itemMark = "@"

// String encodes the obligation for the one place it is stored rather than
// passed: a parked run's row, which outlives the process that
// parked it. `tool:mattermost`, the bare kind when no surface is named, and
// `tool@<item>` for a pickup's.
func (r Reply) String() string {
	out := string(r.Kind)
	if r.Surface != "" {
		out += ":" + r.Surface
	}
	if r.Item != "" {
		out += itemMark + r.Item
	}
	return out
}

// ParseReply decodes what [Reply.String] wrote. An unrecognised kind comes
// back as [ReplyUnset] rather than an error, which [Run] then refuses by the
// same rule as any other invalid input — one refusal, in one place.
func ParseReply(s string) Reply {
	s, item, _ := strings.Cut(s, itemMark)
	kind, surface, _ := strings.Cut(s, ":")
	return Reply{Kind: ReplyKind(kind), Surface: surface, Item: item}
}

// PickupUntyped is the correction of a pickup's turn that has put nothing typed
// on its item: what [Check], [OverrideDone] and the submission check all say,
// naming the five typed answers (ADR-0122).
const PickupUntyped = "this turn answers a work pickup, and nothing typed is on the item " +
	"you were handed yet: a comment alone, or words in your report, records nothing the " +
	"engine can act on, so the item would sit with you unanswered. Answer it with one of " +
	"these five, on the item itself:\n" + types.PickupAnswers + "\nThen report what you did"

// UntypedPickup reports a pickup's turn whose record shows no typed answer on
// its item while the item is still the seat's: the one condition the three
// delivery checks refuse a pickup on. An item that left the seat mid-turn
// (removed, finished or reassigned) owes it nothing ([types.PickupAnswer]).
func UntypedPickup(w Work, reply Reply) bool { return reply.Item != "" && !w.Answer.Accepted() }

// Awaited reports whether anyone is waiting for this turn's answer.
func (r Reply) Awaited() bool { return r.Kind == ReplyTool || r.Kind == ReplyEngine }

// Outcome is what the executor says it did.
type Outcome string

const (
	// OutcomeDelivered — the work is done and, where a tool was the way to
	// deliver it, the tool ran.
	OutcomeDelivered Outcome = "delivered"

	// OutcomeNoAction — nobody was asking this seat to do anything, and it
	// changed nothing outside the engine. Nothing is posted.
	//
	// NOT the way to decline: a seat that was asked and says no must say so
	// where it was asked, or the requester waits in silence.
	//
	// Nor the way to end a turn that ACTED outside the engine ([Acted]): that
	// reports delivered. The live submission refuses its no_action and
	// [Check] sends back any that reaches it; it would leave no episode and
	// nothing reviewed.
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
// ONE MEMBERSHIP TEST, against [tools.Registry.Deliveries]: an MCP tool its own
// annotations do not POSITIVELY call a read (fail closed), and a first-party
// tool the engine registered as reaching somebody (a diary write does not;
// origin is no test, since an in-process tracker comment reaches the asker).
//
// WHETHER, NOT WHERE: [Acted]'s and the ledger's question. The delivery gate's
// is [DeliveredTo], which reads the same map's VALUES, not its keys.
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

// KnownDelivered reports whether this record shows a delivery the engine
// KNOWS of: a successful call that delivers ([Deliverable]) other than by
// presumption alone ([Surface.PresumedWrites]).
//
// FOR SAYING WHERE A TURN WROTE, NEVER FOR JUDGING WHETHER IT DELIVERED. The
// delivery check counts a presumed write, and must; but a correction that
// tells a turn it filed its work somewhere else, or reached somebody, says so
// only of a write it knows of. Told so after it only searched through a tool
// that no hint marks as a read or as a write, a model goes looking for a
// write it never made and argues with the correction.
func KnownDelivered(calls []ledger.Call, s Surface) bool {
	for _, c := range calls {
		if !c.Failed && Deliverable(c.Name, s) && !slices.Contains(s.PresumedWrites, c.Name) {
			return true
		}
	}
	return false
}

// DeliveredTo reports whether this turn's record shows a call that reached the
// party who is waiting, ON THE SURFACE THEY ARE WAITING ON.
//
// The flat [Delivered] let a Mattermost DM's turn discharge its obligation by
// filing a work item. NARROWED ONLY WHERE IT CAN BE; both fallbacks use the
// flat question, so this is never stricter than the seat can satisfy:
//
//   - NO surface: an assignment may be answered on the tracker, thread or
//     ticket, and narrowing on a surface nobody chose refuses correct turns.
//   - A surface THIS SEAT CANNOT REACH: no tool delivers there, and holding the
//     turn open would loop it to exhaustion over a missing integration.
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
// Only [ReplyTool] is a question about tool calls ([DeliveredTo]).
// [ReplyEngine] is delivered by construction (the record carries whether the
// engine's reply went, which this cannot see) and [ReplyNone] has no answer
// missing; asking the tool record on all three filed them as never delivered.
// It feeds the executor log's ending: [Result.Delivered] where the loop ended
// the turn, this over bridged calls where a loss or stop left no frame.
func Answered(calls []ledger.Call, s Surface, reply Reply) bool {
	if reply.Kind != ReplyTool {
		return true
	}
	return DeliveredTo(calls, s, reply)
}

// Record is what every delivery check reads: every call this turn's CLOSED
// rounds made, then the current round's.
//
// THE OBLIGATION IS THE TURN'S, NOT THE ROUND'S: the prior-work ledger tells
// later rounds an earlier write ALREADY RAN, so a check reading the round
// alone refused a reply already posted and sent the turn to post it again.
// The runner's citation check, [Check] and [OverrideDone] all read this: what
// one refuses another must not accept. [Result.Delivered] deliberately does not.
//
// It includes the calls a record did not keep, by name and outcome
// ([ledger.Called]): elided middles, a reply posted mid-run reached its person.
func Record(earlier []ledger.Iteration, current []ledger.Call) []ledger.Call {
	n := len(current)
	for _, it := range earlier {
		n += len(it.Calls) + len(it.Elided)
	}
	out := make([]ledger.Call, 0, n)
	for _, it := range earlier {
		out = append(out, it.Calls...)
		out = append(out, ledger.Called(it.Elided)...)
	}
	return append(out, current...)
}

// Record is [Record] for this round: the closed rounds, then every call this
// round made ([Work.Called]).
func (w Work) Record(earlier []ledger.Iteration) []ledger.Call {
	return Record(earlier, w.Called())
}

// Called is every call this round made as a delivery check reads it: the
// calls its record kept, then its elided middle by name and outcome
// ([ledger.Called]).
func (w Work) Called() []ledger.Call {
	return append(slices.Clip(w.Calls), ledger.Called(w.Elided)...)
}

// Remedy is the sentence a delivery refusal ends with: how to deliver where
// the waiting party actually is.
//
// IT NAMES THE TOOLS, not a discovery mechanism: a vendor's tools sit behind
// discovery, but Worklet and Doclet are answered by first-party tools the
// executor already holds, which no discovery call lists. Sent to discovery, a
// seat woken by a work-item comment posted in a chat channel that could never
// satisfy the check.
//
// Where the asker's surface cannot be named ([DeliveredTo]'s two fallbacks)
// any delivery counts, and the general instruction is the honest one.
func Remedy(s Surface, reply Reply) string {
	on := reply.Surface
	if on == "" || !s.Reaches(on) {
		// DISCOVERY ONLY WHERE THE PHASE HAS IT: an executor holds every
		// tool it can reach, and was sent to two tools it does not hold.
		if len(s.Discoverable) == 0 {
			return "Call the tool that delivers on the surface the request came from."
		}
		return "Call the tool that delivers on the surface the request came from — " +
			"`" + toolname.ListMCPServerTools + "` and `" + toolname.Activate +
			"` will find one you do not have yet."
	}
	var held []string
	discoverable := false
	for name, surface := range s.Deliveries {
		switch {
		case surface != on:
		case slices.Contains(s.Discoverable, name):
			discoverable = true
		default:
			held = append(held, name)
		}
	}
	slices.Sort(held)
	// THE SERVER IS THE SURFACE: an MCP tool reaches the surface named after
	// its own server, so `on` is also the server to look in.
	const discover = "`" + toolname.ListMCPServerTools + "` lists and `" +
		toolname.Activate + "` promotes"
	switch {
	case len(held) > 0 && discoverable:
		return "Deliver on `" + on + "` with one of the tools already on your tool " +
			"list — no discovery needed: " + strings.Join(held, ", ") + " — or with " +
			"one from the `" + on + "` MCP server, which " + discover + "."
	case len(held) > 0:
		return "Deliver on `" + on + "` with one of the tools already on your tool " +
			"list — no discovery needed: " + strings.Join(held, ", ") + "."
	default:
		return "Deliver on `" + on + "` with a tool from the `" + on + "` MCP " +
			"server, which " + discover + "."
	}
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

// Acted reports whether this record shows the turn already reached outside
// the engine: re-running it would repeat something irreversible. SUCCESSFUL
// calls only. Read by [Abandon], [Check] and the live submission, which refuse
// a `no_action` it answers true for ([ActedAccount]). Not [Delivered], which
// counts only tools carrying an answer: a turn that asked a colleague ended
// skipped, with nothing reviewed.
//
// TWO HALVES, FAILING IN OPPOSITE DIRECTIONS: a false positive gives up a
// trigger a redelivery would have run; a false negative leaves today's
// behaviour. So this half counts PROOF alone (open-world, not read-only; a web
// search is no act), and [Deliverable]'s FAILS CLOSED ([Surface.PresumedWrites]).
func Acted(calls []ledger.Call, s Surface) bool {
	return slices.ContainsFunc(calls, func(c ledger.Call) bool { return actsOutside(c, s) })
}

// actsOutside is [Acted]'s rule for one call, which [ActedWith] names by.
func actsOutside(c ledger.Call, s Surface) bool {
	switch {
	case c.Failed:
		return false
	case Deliverable(c.Name, s):
		return true
	default:
		return slices.Contains(s.KnownOpenWorld, c.Name) && !slices.Contains(s.KnownReads, c.Name)
	}
}

// ActedWith says which tools this record acted with (the calls [Acted] counts):
// each once, in name order, the first [actedNamed] by name and then how many
// more ([words.AndMore]). Empty exactly where Acted is false.
//
// Corrections that find the turn acted but delivered nothing the engine knows
// of ([KnownDelivered]) name tools through this and say only that it called
// them, as true of a presumed write ([Surface.PresumedWrites]) as of an ask.
// The `no_action` one uses [ActedAccount].
//
// A SENTENCE THAT SAYS NOTHING WAS DONE MUST BE TRUE: "no tool that acts
// outside the engine was called", said to a turn looking at its own
// successful call, is one the model argues with.
func ActedWith(calls []ledger.Call, s Surface) string {
	names := actedNames(calls, s)
	if len(names) == 0 {
		return ""
	}
	return words.AndMore(names, actedNamed)
}

// ActedAccount says what this record did outside the engine, as a `no_action`
// it bars is told: the tools [ActedWith] names, with those that no hint marks
// as a read or as a write, from their servers or from an operator's override
// ([Surface.PresumedWrites]), named apart, as calls the engine cannot tell
// from a write; one marked as a write, `read_only: false` or `destructive:
// true`, is named as an act. Empty exactly where [Acted] is false.
//
// A PRESUMED WRITE IS NOT CALLED AN ACT. The delivery check counts a call to
// such a tool, and must: read as a read, it would exempt most of a fresh
// server from the fence. But a turn that only searched through one, told it
// "already acted outside the engine", reads that as false against its own
// call and argues with the correction, so it is told why the call counts.
func ActedAccount(calls []ledger.Call, s Surface) string {
	var proven, presumed []string
	for _, name := range actedNames(calls, s) {
		if slices.Contains(s.PresumedWrites, name) {
			presumed = append(presumed, name)
		} else {
			proven = append(proven, name)
		}
	}
	account := ""
	if len(proven) > 0 {
		account = "this turn already acted outside the engine — it called " +
			words.AndMore(proven, actedNamed)
	}
	if len(presumed) == 0 {
		return account
	}
	unmarked := "which its server does not mark read-only, so the engine cannot tell it from a write"
	if len(presumed) > 1 {
		unmarked = "which their servers do not mark read-only, so the engine cannot tell them from writes"
	}
	called := words.AndMore(presumed, actedNamed) + ", " + unmarked
	if account == "" {
		return "this turn called " + called
	}
	return account + " — and it called " + called
}

// actedNames is each tool this record acted with ([actsOutside]), once, in
// name order.
func actedNames(calls []ledger.Call, s Surface) []string {
	var names []string
	for _, c := range calls {
		if actsOutside(c, s) && !slices.Contains(names, c.Name) {
			names = append(names, c.Name)
		}
	}
	slices.Sort(names)
	return names
}

// actedNamed is how many tools [ActedWith] names before it says how many more,
// and [ActedAccount] in each of its two lists.
//
// THE NAMES POINT, THEY DO NOT RECORD. A correction is carried as review notes
// into every later round's brief, so its length must not grow with the run, and
// every call it names is already in the model's transcript and the prior-work
// ledger. Five keep the clause to a line at a usual tool name length; a turn
// that acted through more is told how many.
//
// CAPPED, UNLIKE [Remedy]'s list: Remedy's names are an instruction to pick
// from, where a cap could hide the one tool that delivers; these remind the
// model of calls it already holds.
const actedNamed = 5

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
//
// THE RECORD IS THE TURN'S — earlier holds the rounds already closed — and not
// this round's alone; see [Record]. A round that only re-read the item after
// an earlier one answered has delivered exactly what the turn owed, and whether
// it ALSO owed a further delivery (a correction the first answer needs) is a
// judgement about content, which is the reviewer's.
func Check(w Work, earlier []ledger.Iteration, reply Reply, s Surface) Verdict {
	record := w.Record(earlier)
	// TWO PREDICATES, TWO QUESTIONS, deliberately not one variable. `acted`
	// ([Acted], also what decides a broken turn's redelivery) asks whether
	// anything reached outside the engine, which makes "nobody was asking"
	// wrong; `delivered` asks whether the party WAITING was reached, which
	// makes a claim of delivery wrong. A turn can satisfy the first and fail
	// the second (a ticket filed while a founder waits in chat); collapsing
	// them lets it pass one check under the other's name.
	acted := Acted(record, s)
	delivered := DeliveredTo(record, s, reply)

	// A rescue never takes any fast path. The engine wrote the outcome,
	// so there is nothing here anybody committed to.
	if w.Rescued {
		return Verdict{}
	}

	// A PICKUP IS ANSWERED ONLY ON ITS ITEM, typed: a delivery or a block
	// reported over words alone is sent back naming the five answers. A
	// no_action is refused below, as on every turn somebody waits on.
	if (w.Outcome == OutcomeDelivered || w.Outcome == OutcomeBlocked) && UntypedPickup(w, reply) {
		return Verdict{Correction: PickupUntyped}
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
			// Something already reached the outside world, or may have
			// through a call the engine cannot tell from a write. Ending the
			// turn as "nobody was asking" would file a side effect nobody
			// reviewed, and leave the next turn on this thread with no
			// record that it happened — and NAMED, as every correction that
			// turns on an act names what the turn acted with: here any act,
			// a delivery included, and a presumed write named as one
			// ([ActedAccount]).
			return Verdict{Correction: "You reported no_action, but " + ActedAccount(record, s) +
				". Report what you actually did instead."}
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
		on := reply.Surface
		named := on != "" && s.Reaches(on)
		switch {
		case named && KnownDelivered(record, s):
			// NAMED WHERE IT IS KNOWN, exactly as [OverrideDone] and the
			// submission check do. A turn that DID write somewhere is told
			// "no tool was called", reads that as false against its own
			// record, and argues with the correction instead of acting on
			// it. Keyed on a delivery elsewhere the engine KNOWS of, never
			// on an act or a presumed write: "filing the work somewhere
			// else" said to a turn that only asked a colleague, or only
			// searched through a tool that no hint marks as a read or as a
			// write, is false too.
			return Verdict{Correction: "You reported the work as delivered, and you " +
				"did act — but nothing was delivered on `" + on + "`, which is where " +
				"this was asked. Filing the work somewhere else does not tell the " +
				"person waiting. " + Remedy(s, reply) + " If you cannot, report " +
				"honestly what is blocking you."}
		case acted:
			// ACTED, AND DELIVERED NOTHING THE ENGINE KNOWS OF: an ask, a
			// reaction, a publish, or a call it only presumes wrote,
			// elsewhere. Told "no tool that acts outside the engine was
			// called", the model reads that as false against its own record;
			// told it "did act" after only a search, it does the same. So it
			// is told what it called, which is true of both.
			where := ""
			if named {
				where = " on `" + on + "`, which is where this was asked"
			}
			return Verdict{Correction: "You reported the work as delivered, and you called " +
				ActedWith(record, s) + ", but none of it delivered anything to the person waiting" +
				where + ". " + Remedy(s, reply) + " If you cannot, report honestly what is " +
				"blocking you."}
		}
		return Verdict{Correction: "You reported the work as delivered, but no tool " +
			"that acts outside the engine was called in this turn: writing about an " +
			"action does not perform it. " + Remedy(s, reply) + " If you cannot, " +
			"report honestly what is blocking you."}
	}
	return Verdict{}
}

// OverrideDone reports whether a reviewer's `done` has to be overturned.
//
// THE LAST LAYER: Check reads the executor's claim before the reviewer looks,
// this reads the reviewer's verdict against the same record. A reviewer judging
// the produced TEXT says done though nothing put the answer anywhere a person
// can see.
//
// It fires only when somebody is waiting for a TOOL to have delivered (a
// research turn may end in prose; the engine returns an A2A artifact itself).
// Judged on the TURN's record, as [Check] is. A turn that called something
// that acts without delivering has it named ([ActedWith]), and "reached
// somebody" is said only of a delivery known ([KnownDelivered]).
func OverrideDone(w Work, earlier []ledger.Iteration, reply Reply, s Surface,
) (override bool, correction string) {
	record := w.Record(earlier)
	if UntypedPickup(w, reply) {
		return true, PickupUntyped
	}
	if reply.Kind != ReplyTool || DeliveredTo(record, s, reply) {
		return false, ""
	}
	on := reply.Surface
	named := on != "" && s.Reaches(on)
	acted := ActedWith(record, s)
	switch {
	case named && KnownDelivered(record, s):
		// NAMED, because this is the case the flat check used to pass. The
		// turn DID reach somebody — just not the person waiting — so a
		// correction saying "no tool was called" would read as false to a
		// model looking at its own successful write, and it would submit
		// the same citation again.
		return true, "This turn reached somebody, but not the person waiting for it: " +
			"nothing was delivered on `" + on + "`, which is where this was asked. " +
			Remedy(s, reply)
	case named && acted != "":
		// NAMED WHAT IT DID CALL: an ask, a reaction or a publish reached
		// somebody, just nobody waiting there — or a call the engine only
		// presumes wrote did, if it wrote at all, which is why this says
		// only that the turn called it.
		return true, "Nothing in this turn reached the person waiting for it: it called " + acted +
			", but nothing was delivered on `" + on + "`, which is where this was asked. " +
			Remedy(s, reply)
	case named:
		// AND ONLY THEN. Saying "this turn reached somebody" to a turn that
		// reached nobody is the same false statement pointed the other way,
		// and a model told it has written somewhere goes looking for where.
		return true, "Nothing in this turn reached the person waiting for it: " +
			"nothing was delivered on `" + on + "`, which is where this was asked. " +
			Remedy(s, reply)
	case acted != "":
		return true, "This turn produced an answer as text, and it called " + acted + ", but " +
			"none of that delivers an answer: the requester will never see it. " + Remedy(s, reply)
	default:
		return true, "This turn produced an answer as text, but no tool that acts " +
			"outside the engine was called: the requester will never see it. " +
			Remedy(s, reply)
	}
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
