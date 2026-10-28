package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/tools/toolargs"
)

// ---- the one way a tool is called (ADR-0059) ------------------------------ //
//
// EVERY CALLER CALLS A TOOL THROUGH [Invoke]: a phase's surface, the operator
// surface (an operator's assistant over /operator/mcp and the work API's
// dispatch), and a replay of a run's submission. So a first-party call is
// held to its tool's schema whoever makes it, and a caller added later
// inherits the check by calling a tool at all: a test walks the tree and
// refuses a tool's Call or CallForTurn invoked anywhere else (a tool
// delegating to its own variant, and a wrapper forwarding to the tool it
// wraps, excepted). A seat's executor run is a phase's surface too: every call
// its CLI makes over the tool bridge (internal/api/mcpbridge) is admitted to
// the run's surface and comes here, with no check of the bridge's own.

// Invocation is one call of a tool, as its caller makes it.
type Invocation struct {
	// Name is the tool's wire name, and Tool the tool.
	Name string
	Tool Callable

	// Args are the call's arguments in the canonical form the caller's
	// entry made of them ([ArgsOf]), the form every step of the call reads.
	Args toolargs.Args

	// AsSeat calls the tool as a phase's surface does: a [SeatCallable]
	// one for Turn, any other through its Call. False calls every tool
	// through its Call, the MCP contract, as a caller with no turn does.
	AsSeat bool
	Turn   *turnctx.Turn

	// ThirdParty marks a tool a third-party MCP server serves, which holds
	// its own schema and is not checked here.
	ThirdParty bool

	// Where names the caller in a log line ("execute", "operator").
	Where string

	// Counter counts a call answered outside its schema
	// ([metrics.ToolArgsOutsideSchema]); nil counts nothing.
	Counter Counter
}

// ArgsOf is a call's arguments as its caller's entry hands them on, made
// ONCE, before anything else reads them ([toolargs]): a first-party tool's
// closed-set members spelt as its schema spells them, the one rule the check
// reads by, so a tool keeps no normalisation of its own; a third-party tool's
// as sent, since it holds its own schema.
func ArgsOf(tool Callable, thirdParty bool, raw map[string]any) toolargs.Args {
	if thirdParty || tool == nil {
		return toolargs.Of(nil, raw)
	}
	return toolargs.Of(tool.Parameters(), raw)
}

// Invoke calls a tool, holding a first-party call to the schema the tool
// advertises ([CheckArgs]): a value the tool would misread is refused before
// it runs (refused is true, and the tool was never called), and every other
// finding rides on the tool's own answer ([withFaults]). An error is the
// caller's context ending, as a tool's own call reports it.
func Invoke(ctx context.Context, in Invocation) (res Result, refused bool, err error) {
	var faults []ArgFault
	if !in.ThirdParty {
		faults = CheckArgs(in.Name, in.Tool.Parameters(), in.Args.Map())
	}
	if misread(faults) {
		// THE PERSON'S WORDS beside the model's (ADR-0060): a surface
		// answering a person answers this, never the sentence above,
		// which names arguments.
		return Result{Output: misreadAnswer(in.Name, faults), Failed: true,
			Cause: RefusalInvalid, PersonRefusal: caveat.ValueInWrongForm()}, true, nil
	}
	res, err = call(ctx, in)
	if err != nil {
		return res, false, err
	}
	return withFaults(in, res, faults), false, nil
}

// call is THE ONE PLACE a tool's own call is made, telling the tool who is
// calling when it asks.
//
// THE SEAT COMES FROM THE CALLER, never from the arguments. A tool that acts
// for a seat (asking a colleague, marking an onboarding step, writing to a
// diary) must not be able to be pointed at another one by a model that
// spelled a different handle in its arguments. A phase's surface is built per
// turn by the runner, which is the frame that knows.
//
// An OPTIONAL interface rather than a parameter on Callable, because Callable
// is the MCP tool contract as well: an MCP server has no notion of a turn, and
// widening the signature would put a turn-shaped argument on every bridged
// tool to be ignored. This way the two kinds of tool coexist under one
// registry, and the one that needs the fact asks for it.
func call(ctx context.Context, in Invocation) (Result, error) {
	if t, ok := in.Tool.(SeatCallable); ok && in.AsSeat {
		return t.CallForTurn(ctx, in.Turn, in.Args.Map())
	}
	return in.Tool.Call(ctx, in.Args.Map())
}

// withFaults is a tool's answer carrying the findings its call's arguments
// had: on a refusal of the arguments (a sentence), every one appended, so the
// next call answers them all; on a success, a [Result.Note] naming what was
// not read as sent, logged
// and counted ([metrics.ToolArgsOutsideSchema]) as the drift between the
// schema and the tool's reading. Any other answer is left as it was.
func withFaults(in Invocation, res Result, faults []ArgFault) Result {
	if len(faults) == 0 {
		return res
	}
	switch {
	case res.Failed && RefusalOf(res) == RefusalInvalid:
		res.Output += fmt.Sprintf("\n\nEvery argument outside %s's schema:\n%s", in.Name, faultLines(faults))
	case !res.Failed:
		// A NOTE, NEVER OUTPUT: a write's answer is the JSON receipt a
		// caller decodes ([Result.Note]).
		res.Note = fmt.Sprintf("%s ran, but these arguments are outside its schema and were not "+
			"read as sent; send them as described next time:\n%s", in.Name, faultLines(faults))
		paths := make([]string, len(faults))
		for i, f := range faults {
			paths[i] = f.Path
		}
		attrs := []any{"tool", in.Name, "paths", strings.Join(paths, ","), "where", in.Where}
		if in.Turn != nil {
			attrs = append(attrs, "turn", in.Turn.RunID)
		}
		contractLog.Info("tool_args_outside_schema", attrs...)
		argsOutsideSchema.add(in.Counter, in.Name)
	}
	return res
}

// Rendered is a result as a reader of text reads it: its output, and its note
// after it ([Result.Note]). The model's surface and an MCP transport render a
// result this way; a caller that decodes Output carries the note apart.
func Rendered(res Result) string {
	if res.Note == "" {
		return res.Output
	}
	return res.Output + "\n\nNote: " + res.Note
}
