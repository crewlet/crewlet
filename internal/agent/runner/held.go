package runner

import (
	"context"
	"fmt"
	"strings"

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

	// Args are the call's arguments, opened from their seal: what runs.
	Args map[string]any

	// Choice is the person's answer: [sandbox.HeldOnce],
	// [sandbox.HeldAlways] or [sandbox.HeldDeny].
	Choice string

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
	// Begin marks the call as being made. First is true where this mark is
	// the first, and the caller makes the call; false where some earlier
	// attempt at the same answer marked it, and Made says whether that
	// attempt recorded what the call returned (Result).
	Begin(ctx context.Context) (HeldPrior, error)
	// Record records what the call returned.
	Record(ctx context.Context, result sandbox.HeldResult) error
}

// HeldPrior is how a held call stood when [HeldCommit.Begin] marked it.
type HeldPrior struct {
	First  bool
	Made   bool
	Result sandbox.HeldResult
}

// digested is a tool that names its definition, as an MCP server's tool does
// ([mcp.Definition.Digest]).
type digested interface{ Digest() string }

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
// An error is the resume's to retry: the mark could not be written, or the
// turn's context ended while the call ran.
func makeHeld(ctx context.Context, surface *tools.Surface, held *HeldCall) error {
	if !sandbox.HeldMakes(held.Choice) {
		held.outcome = heldOutcome{kind: heldDenied}
		return nil
	}
	if held.Unopened != "" {
		held.outcome = heldOutcome{kind: heldNotMade, why: held.Unopened}
		return nil
	}
	entry, known := surface.Universe().Lookup(held.Tool)
	if !known {
		held.outcome = heldOutcome{kind: heldNotMade, why: "the tool is no longer filed for this " +
			"agent (it was blocked, or its server removed or turned off, since the call was held)"}
		return nil
	}
	if tool, ok := entry.Tool.(digested); !ok || tool.Digest() != held.Digest {
		held.outcome = heldOutcome{kind: heldNotMade, why: "the tool's definition changed since " +
			"the call was held, so it is not the tool the person approved; call it again to ask " +
			"about the tool as it is now"}
		return nil
	}
	prior, err := held.Commit.Begin(ctx)
	if err != nil {
		return fmt.Errorf("runner: marking the held call to %s as made: %w", held.Tool, err)
	}
	switch {
	case prior.Made:
		held.outcome = heldOutcome{kind: heldMade, result: prior.Result}
		return nil
	case !prior.First:
		held.outcome = heldOutcome{kind: heldUnknown}
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
	if err := held.Commit.Record(ctx, result); err != nil {
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
	w.WriteString("## Your call was answered\n\n")
	w.WriteString(a.Held.called("You", "your") + " " + a.Held.what(a.heldBy(b)) + "\n\n")
	w.WriteString(a.Held.told(b) + "\n\n")
	w.WriteString("Carry on with the task from where you stopped, and finish it as the task asks.")
	return w.String()
}

// heldReseed is [Asked.Reseed] for a held call.
func (a Asked) heldReseed(b provenance.Boundary) string {
	var w strings.Builder
	w.WriteString("## A run before you on this task called a tool that asks a person first\n\n")
	w.WriteString(a.Held.called("It", "its") + " " + a.Held.what(a.heldBy(b)) + "\n\n")
	w.WriteString(a.Held.told(b) + "\n\n")
	w.WriteString(a.freshMachine("with this incorporated"))
	return w.String()
}

// heldBy is who answered a held call, as the prompts name them.
func (a Asked) heldBy(b provenance.Boundary) string {
	if a.By != "" {
		return provenance.Neutralise(b, a.By)
	}
	return "A person"
}
