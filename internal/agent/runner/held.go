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

	// outcome is what became of the call, and kept its result as the run's row
	// sealed it, where one was recorded ([HeldCommit.Record], [HeldPrior.Kept]),
	// both set by [makeHeld]:
	// the answer's lead keeps it for a later brief to tell again ([Asked.lead]).
	outcome heldOutcome
	kept    *sandbox.HeldKept
}

// HeldCommit marks a held call as being made and records what it returned, on
// its run's row, under the claim that carries the run on
// ([sandbox.PendingStore.MarkHeldCall]). The engine supplies it, sealing and
// opening the result under the fleet keyring.
type HeldCommit interface {
	// Marked is how an earlier attempt at the same answer left the call, as
	// the claim carrying the run on read its row, false where none marked it
	// ([sandbox.HeldCall.Calling]); Made and Unread as for Begin.
	//
	// THE CLAIM'S OWN READ needs no other: only the next job's opening clears
	// a mark, and it ends the claim, so a call read as marked stays marked
	// while the claim holds the row. One read as unmarked is the one
	// [HeldCommit.Begin] marks, whose answer is the store's word on whether
	// its mark was the first.
	Marked(ctx context.Context) (HeldPrior, bool)
	// Begin marks the call as being made. First is true where this mark is
	// the first, and the caller makes the call; otherwise Made says whether
	// the earlier attempt recorded what the call returned (Result), and Unread
	// that it did where that record cannot be opened here.
	Begin(ctx context.Context) (HeldPrior, error)
	// Record records result, as the run is handed it, and the call as one of
	// the run's round with the surface's answer
	// ([sandbox.PendingStore.RecordHeldResult]): a call missing from the
	// round's calls reads as a reply never sent. Both land on the row in one
	// write, the round's log after; an error past that write leaves the call to
	// the next job's opening ([sandbox.OwedCall]). It answers the result as
	// sealed ([sandbox.HeldKept]), error or not, zero for none: the answer's
	// lead keeps it, as no record holds it once the next job opens.
	// NO ARGUMENTS OR THEIR DIGEST: they ride the row sealed (ADR-0077), and a
	// clear digest would confirm a guess at the seal. Call logs count it
	// withheld (sandbox.BridgeCall.Withheld), an over-count no operation id
	// reads, since no builtin is ever held.
	Record(ctx context.Context, result sandbox.HeldResult, answered toolloop.ToolResult) (sandbox.HeldKept, error)
}

// HeldPrior is how a held call stood: as [HeldCommit.Begin] found it when it
// marked it, or as the claim read it ([HeldCommit.Marked]), where First is
// never set.
type HeldPrior struct {
	First  bool
	Made   bool
	Result sandbox.HeldResult

	// Unread is a call made and its result recorded, where no key it was
	// sealed under is on the node carrying the run on (internal/engine's
	// heldCommit.prior): Made is set, and Result holds only whether the tool
	// failed, off the call's entry (sandbox.HeldCall.Call), never its output.
	Unread bool

	// Kept is the result that attempt recorded, as the row sealed it
	// ([sandbox.HeldCall.Kept]), an unread one included, for the answer's lead
	// to keep so a later node holding its key can open it. Nil for none.
	Kept *sandbox.HeldKept
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

// makeHeld makes the held call the person allowed, through the seat's rebuilt
// surface, at most once, and records what became of it on held. An error is
// the resume's to retry: the mark was not written, or the context ended.
//
// AT MOST ONCE: every attempt at one answer reaches here, so the call is
// marked on the run's row BEFORE it is made, and only the first mark makes it.
// A later attempt answers from the mark FIRST ([HeldCommit.Marked]): asked
// hours on of a rebuilt surface, [HeldCall.notMade] could call a made call
// unmade, and the person's second approval would make it twice. Its checks
// come before the mark, so a call they stop leaves nothing on the row and the
// run may call again, asking afresh; met after it, a guard's refusal would
// use up the approval on a call that never ran.
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
		held.outcome, held.kept = prior.outcome(), prior.Kept
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
		held.outcome, held.kept = prior.outcome(), prior.Kept
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
	kept, err := held.Commit.Record(ctx, recorded, res)
	if kept.Result != "" {
		held.kept = &kept
	}
	if err != nil {
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
// arguments did not open, its tool is no longer filed (blocked, or its server
// removed) or not the definition the person approved (A12 holds a changed tool
// at Needs approval), or the surface's skill guard would refuse it. That guard
// is the rebuilt one, not the one that held the call: a skill made required
// since covers it, and a job rebuilt holding only its own loads
// ([Resume.parked]) asks the run to load them again, the safe direction.
// Asked only of a call no attempt has marked ([makeHeld]).
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
		// MADE, AND SAID SO, with whether it failed: its result is on record
		// and the round's log lists it made, so it is never "unknown".
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
