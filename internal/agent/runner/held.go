package runner

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/origin"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/textcut"
	"github.com/crewlet/crewlet/internal/tools"
)

// HeldCall is the call to a tool at Needs approval a parked run's question was
// about, answered ([Asked.Held], ADR-0077): what carrying the run on makes, at
// most once, before it relaunches the run, and tells the run about.
type HeldCall struct {
	// Tool is the tool as the run called it, Server its MCP server, and
	// Digest the definition the person approved
	// ([mcp.Definition.Digest]).
	Tool, Server, Digest string

	// Args are the call's arguments, opened from their seal: what runs. Left
	// sealed, and nil, for a call an earlier attempt marked
	// ([HeldCommit.Marked]), which nothing runs again.
	Args map[string]any

	// Choice is the person's answer: [sandbox.HeldOnce],
	// [sandbox.HeldAlways] or [sandbox.HeldDeny].
	Choice string

	// Approver is who answered, HeldID the held call's id, and Fixed a call
	// to an engine tool that asks before every call, whose denial its tool
	// records ([denialRecorder]).
	Approver, HeldID, Binding string
	Fixed                     bool

	// Commit is how the call is made at most once, on the run's row.
	Commit HeldCommit

	// Unopened is why the call's arguments could not be opened from their
	// seal, "" where they were: a call that is then never made, since what
	// would run is not what the person approved.
	Unopened string

	// outcome is what became of the call, set by [makeHeld].
	outcome heldOutcome
}

// HeldCommit marks a held call as being made and records what it returned, on
// its run's row, under the claim that carries the run on
// ([sandbox.PendingStore.MarkHeldCall]). The engine supplies it, sealing and
// opening the result under the fleet keyring.
type HeldCommit interface {
	// Marked is how an earlier attempt at the same answer left the call, as
	// the claim that carries the run on read its row, and false where no
	// attempt had marked it ([sandbox.HeldCall.Calling]): Made says whether
	// that attempt recorded what the call returned (Result), and Unread that
	// it did where what it recorded cannot be opened here.
	//
	// THE CLAIM'S OWN READ, which needs no other: nothing clears a mark but
	// the next job's opening, which ends the claim, so a call the claim read
	// as marked stays marked for as long as the claim holds the row. A call
	// it read as unmarked is the one [HeldCommit.Begin] marks, and Begin's
	// answer is the store's word on whether its mark was the first.
	Marked(ctx context.Context) (HeldPrior, bool)
	// Begin marks the call as being made. First is true where this mark is
	// the first, and the caller makes the call; false where some earlier
	// attempt at the same answer marked it, and Made says whether that
	// attempt recorded what the call returned (Result), and Unread that it did
	// where what it recorded cannot be opened here.
	Begin(ctx context.Context) (HeldPrior, error)
	// Record records what the call returned — result, as the run that
	// carries the work on is handed it — and the call itself as a call of the
	// run's round, with what the surface that made it answered (answered:
	// whether it failed and why, and who answered it)
	// ([sandbox.PendingStore.RecordHeldResult]): every reader of whether the
	// turn replied reads the round's calls, and a call made and recorded
	// nowhere among them was a reply that read as never sent. The result and
	// the call's entry land on the row in one write and the call reaches the
	// round's log after it; an error after the row's write leaves the call to
	// the next job's opening, which appends it ([sandbox.OwedCall]).
	//
	// WITHOUT ITS ARGUMENTS AND WITHOUT THEIR DIGEST: they ride the row
	// sealed on the held call (ADR-0077), and a digest of them in the clear
	// beside the tool's name would confirm a guess at what the seal hides.
	// A later segment's call log counts the call as one of its tool whose
	// arguments are not known (sandbox.BridgeCall.Withheld) — an over-count
	// of a tool no operation id reads, since no builtin is ever held.
	Record(ctx context.Context, result sandbox.HeldResult, answered toolloop.ToolResult) error
}

// HeldPrior is how a held call stood: as [HeldCommit.Begin] found it when it
// marked it, or as the claim read it ([HeldCommit.Marked]), where First is
// never set.
type HeldPrior struct {
	First  bool
	Made   bool
	Result sandbox.HeldResult

	// Unread is a call made whose result its attempt recorded, where what
	// the result says cannot be opened on the node carrying the run on — no
	// key it was sealed under is there (internal/engine's heldCommit.prior):
	// Made is set, and Result holds only whether the tool reported a failure,
	// off the call's entry recorded with it (sandbox.HeldCall.Call), never
	// its output.
	Unread bool
}

// outcome is what became of a call some earlier attempt at the same answer
// marked, as the run is told it: the result that attempt recorded — or, where
// it cannot be read here, that the call was made and whether it failed — or,
// where it recorded none, that the call may or may not have taken effect.
func (p HeldPrior) outcome() heldOutcome {
	switch {
	case p.Made && p.Unread:
		return heldOutcome{kind: heldUnread, result: p.Result}
	case p.Made:
		return heldOutcome{kind: heldMade, result: p.Result}
	}
	return heldOutcome{kind: heldUnknown}
}

// digested is a tool that names its definition, as an MCP server's tool does
// ([mcp.Definition.Digest]).
type digested interface{ Digest() string }

// denialRecorder is an engine tool that keeps its own record of a call a person
// denied (ssh_operate's command audit): told the call, its arguments and who
// denied it, before the run is told it was refused.
type denialRecorder interface {
	RecordDenied(ctx context.Context, turn *turnctx.Turn, args map[string]any) error
}

// heldOutcome is what became of a held call, as the run is told it.
type heldOutcome struct {
	kind   heldKind
	result sandbox.HeldResult
	// why is the reason a call a person allowed was not made.
	why string
}

type heldKind int

const (
	heldPending heldKind = iota
	heldMade
	heldDenied
	heldNotMade
	heldUnknown
	// heldUnread is a call made and its result recorded, which the node
	// carrying the run on cannot open ([HeldPrior.Unread]).
	heldUnread
)

// makeHeld makes the held call the person allowed, through the seat's own
// rebuilt surface, at most once, and records what became of it on held.
//
// # At most once
//
// A resume can be attempted more than once for one answer, and every attempt
// reaches here. So the call is marked on the run's row BEFORE it is made, and
// only the attempt whose mark is the first makes it; a later attempt hands the
// run the result the first recorded, or, where the first stopped before it
// recorded one, tells the run the call may or may not have taken effect. It is
// never made twice, which a person who allowed it once is owed.
//
// A LATER ATTEMPT ANSWERS FROM THE MARK FIRST ([HeldCommit.Marked]), before it
// asks anything below of the call. Those checks ask whether a call nobody has
// made may be made now, of a surface every attempt rebuilds from the live
// tools and skills, and an attempt that made the call can still be refused
// after it — at the launch, by the floor of the session it continues or by
// every member sitting out — and its answer attempted again hours on. Asked
// first, a skill made required in between, or a tool blocked or changed,
// told the run that the call it had made was not made: its result was lost,
// the run asked the person again, and their second approval made the call
// twice.
//
// # The call the person approved, or none
//
// The tool is resolved against the surface the run carries on with, never what
// it was when the call was held: a tool blocked since, or removed with its
// server, is no longer filed, and a tool whose definition changed since is
// not the one the person read (A12 holds such a tool at Needs approval). Either
// way the call is not made, and the run is told why and may call again, which
// asks afresh. Checked before the mark, so a call not made leaves nothing on
// the row that would keep a later attempt from making it.
//
// AND SO IS THE SURFACE'S SKILL GUARD ([tools.Surface.GuardRefusal]), as the
// rebuild armed it for the session that asked: the guard the call is made
// under is that one, and it need not be the guard that held the call. A skill
// made required, or its trigger widened, while the call waited covers it now;
// and a job whose box's word on its session never reached its record is
// rebuilt holding only its own loads ([Resume.parked]), not the skills its
// launch armed — the safe direction, which asks the run to load them again.
// Met only as the call ran, after the mark, that refusal was recorded as the
// call's result: the run told the call was made and failed, and the person's
// approval used up on a call that never ran. Met here, the call is not made,
// and the run is told why: it can load the skill and call again, which asks
// the person afresh.
//
// An error is the resume's to retry: the mark could not be written, or the
// turn's context ended while the call ran.
func makeHeld(ctx context.Context, surface *tools.Surface, held *HeldCall) (err error) {
	defer origin.Stamp(&err)
	ctx = tools.WithApproval(ctx, tools.Approval{By: held.Approver, HeldID: held.HeldID, Binding: held.Binding})
	if !sandbox.HeldMakes(held.Choice) {
		// A DENIAL RAN NOTHING, so a record that did not land is logged and
		// the run is still told it was refused.
		if denyErr := held.recordDenied(ctx, surface); denyErr != nil {
			log.ErrorContext(ctx, "held_call_denial_unrecorded", "tool", held.Tool, "held_call", held.HeldID,
				"error", denyErr.Error())
		}
		held.outcome = heldOutcome{kind: heldDenied}
		return nil
	}
	if prior, marked := held.Commit.Marked(ctx); marked {
		held.outcome = prior.outcome()
		return nil
	}
	if why := held.notMade(surface); why != "" {
		held.outcome = heldOutcome{kind: heldNotMade, why: why}
		return nil
	}
	prior, err := held.Commit.Begin(ctx)
	if err != nil {
		return fmt.Errorf("runner: marking the held call to %s as made: %w", held.Tool, err)
	}
	if !prior.First {
		held.outcome = prior.outcome()
		return nil
	}
	res, err := surface.RunApproved(ctx, held.Tool, held.Args)
	if err != nil {
		// THE TURN'S CONTEXT ENDED: the call may have reached its server. The
		// mark stays, so no attempt makes it again; the next one tells the
		// run it may or may not have taken effect.
		return fmt.Errorf("runner: making the held call to %s: %w", held.Tool, err)
	}
	result := sandbox.HeldResult{Output: textcut.Within(res.Output, sandbox.MaxHeldResultBytes),
		Failed: res.Failed}
	// AN OUTPUT NO RECORD KEEPS reaches this run alone: the row keeps its
	// size and digest, which a later attempt is told in its place.
	recorded := result
	if res.Unkept {
		recorded.Output = tools.UnkeptOutput(res.Output)
	}
	if err := held.Commit.Record(ctx, recorded, res); err != nil {
		// THE CALL IS MADE, and the run is told what it returned whatever
		// the record says: only a later attempt at the same answer, which
		// would then say the result is unknown, reads the record.
		log.WarnContext(ctx, "held_call_result_unrecorded", "tool", held.Tool,
			"server", held.Server, "error", err.Error(),
			"detail", "the call a person approved was made and its result handed to the run; "+
				"it could not be recorded on the run's row, so a later attempt at the same "+
				"answer would tell the run its result is unknown")
	}
	held.outcome = heldOutcome{kind: heldMade, result: result}
	return nil
}

// recordDenied tells a fixed call's tool that a person denied it, where the
// tool keeps such a record and the call's arguments opened; nothing for any
// other call.
func (h *HeldCall) recordDenied(ctx context.Context, surface *tools.Surface) error {
	if !h.Fixed || h.Args == nil {
		return nil
	}
	entry, known := surface.Universe().Lookup(h.Tool)
	if !known {
		return nil
	}
	recorder, ok := tools.Innermost(entry.Tool).(denialRecorder)
	if !ok {
		return nil
	}
	return recorder.RecordDenied(ctx, surface.Turn(), h.Args)
}

// notMade is why h is not to be made through surface, "" where it may be: its
// arguments did not open, its tool is no longer filed or not the definition the
// person approved, or the surface's guard would refuse it. Asked only of a call
// no attempt has marked ([makeHeld]).
func (h *HeldCall) notMade(surface *tools.Surface) string {
	if h.Unopened != "" {
		return h.Unopened
	}
	entry, known := surface.Universe().Lookup(h.Tool)
	if !known {
		return "the tool is no longer filed for this agent (it was blocked, or its server removed " +
			"or turned off, since the call was held)"
	}
	if tool, ok := tools.Innermost(entry.Tool).(digested); !ok || tool.Digest() != h.Digest {
		return "the tool's definition changed since the call was held, so it is not the tool the " +
			"person approved; call it again to ask about the tool as it is now"
	}
	if refused := surface.GuardRefusal(h.Tool); refused != "" {
		return "the seat's skill guard holds the tool back. " +
			strings.TrimSuffix(strings.TrimSpace(refused), ".") + "; calling it again asks the person afresh"
	}
	return ""
}

// what is what the person answered, as the run is told it, under fence token
// b: who (by, as [Asked.answerer] names them) and what they chose.
func (h *HeldCall) what(by string) string {
	switch h.Choice {
	case sandbox.HeldOnce:
		return by + " allowed it this once."
	case sandbox.HeldAlways:
		return by + " allowed it, and set the tool to run without asking from now on."
	}
	return by + " denied it."
}

// told is what became of the call, under fence token b: its result is the
// server's words, defused so nothing in them opens or closes a fence.
func (h *HeldCall) told(b provenance.Boundary) string {
	switch o := h.outcome; o.kind {
	case heldMade:
		said := "The call was made, and returned:"
		if o.result.Failed {
			said = "The call was made, and the tool reported a failure:"
		}
		return said + "\n\n" + provenance.Neutralise(b, o.result.Output)
	case heldDenied:
		return "The call was not made: it was refused (forbidden). Do not call the tool again " +
			"for this; carry on without it, and say in your report what it would have done."
	case heldNotMade:
		return "The call was not made: " + o.why + "."
	case heldUnknown:
		return "The call was started, and the node making it stopped before its result was " +
			"recorded, so it may or may not have taken effect. Check its effect before you call " +
			"the tool again."
	case heldUnread:
		// MADE, AND SAID SO: never as a call whose result is unknown, which
		// it is not — what it returned is on its record, and the round's log
		// lists it made — and with whether it failed, which that record says.
		if o.result.Failed {
			return "The call was made, and the tool reported a failure. What it said is recorded, but " +
				"cannot be read on the node carrying this run on."
		}
		return "The call was made, and the tool reported success, so it ALREADY RAN: do not call the " +
			"tool again for this. What it returned is recorded, but cannot be read on the node carrying " +
			"this run on."
	}
	return "The call was not made."
}

// called is the line naming the call, as the run is told it: who made it
// (`You`, `It`) and whose turn ended (`your`, `its`).
func (h *HeldCall) called(who, whose string) string {
	return fmt.Sprintf("%s called `%s` on the `%s` MCP server, which asks a person before each "+
		"call, and ended %s turn to wait.", who, h.Tool, h.Server, whose)
}

// heldContinuation is [Asked.Continuation] for a held call.
func (a Asked) heldContinuation(b provenance.Boundary) string {
	var w strings.Builder
	if a.Withdrawn {
		w.WriteString("## You withdrew your call\n\n")
		w.WriteString(a.Held.called("You", "your") + " " + a.withdrew(b) + "\n\n")
		w.WriteString("The call was not made.\n\n" + withdrawnTail)
		return w.String()
	}
	w.WriteString("## Your call was answered\n\n")
	w.WriteString(a.Held.called("You", "your") + " " + a.Held.what(a.heldBy(b)) + "\n\n")
	w.WriteString(a.heldSaid(b))
	w.WriteString(a.Held.told(b) + "\n\n")
	w.WriteString("Carry on with the task from where you stopped, and finish it as the task asks.")
	return w.String()
}

// heldReseed is [Asked.Reseed] for a held call.
func (a Asked) heldReseed(b provenance.Boundary) string {
	var w strings.Builder
	if a.Withdrawn {
		w.WriteString("## A run before you on this task called a tool that asks a person " +
			"first, and your seat withdrew the call\n\n")
		w.WriteString(a.Held.called("It", "its") + " " + a.withdrew(b) + "\n\n")
		w.WriteString("The call was not made.\n\n")
		w.WriteString(happened + "\n\n")
		w.WriteString(a.freshMachine("under your access as it stands now"))
		return w.String()
	}
	w.WriteString("## A run before you on this task called a tool that asks a person first\n\n")
	w.WriteString(a.Held.called("It", "its") + " " + a.Held.what(a.heldBy(b)) + "\n\n")
	w.WriteString(a.heldSaid(b))
	w.WriteString(a.Held.told(b) + "\n\n")
	w.WriteString(happened + "\n\n")
	w.WriteString(a.freshMachine("with this incorporated"))
	return w.String()
}

// heldSaid is what whoever answered a held call said with it, as its own
// paragraph, "" for an answer that said nothing beyond its choice: a delegate's
// reason for a denial, and the rule it decided under (ADR-0125).
func (a Asked) heldSaid(b provenance.Boundary) string {
	said := strings.TrimSpace(a.Answer)
	if said == "" {
		return ""
	}
	return "They said: " + provenance.Neutralise(b, said) + "\n\n"
}

// heldBy is who answered a held call, as the prompts name them.
func (a Asked) heldBy(b provenance.Boundary) string {
	if a.By != "" {
		return provenance.Neutralise(b, a.By)
	}
	return "A person"
}
