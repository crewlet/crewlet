// Package toolsessiontest is the contract suite every [toolsession.Dispatcher]
// runs: the gates a call is held to wherever the coding CLI that made it runs.
//
// # Why one suite
//
// Two dispatchers stand between a coding CLI and a seat's tools. The tool
// loop's serves a CLI on this host, over the unix socket `crewlet mcp-relay`
// reaches (internal/agent/toolloop); the bridge's serves a CLI in a sandbox
// box, over HTTP (internal/api/mcpbridge). A call is the same call from either
// place, so the gates it passes are the same gates — and two packages each
// testing their own copy of them is how one of the copies drifts while its
// own tests stay green. So the gates both hold live here, once, and each
// package runs this suite from its own test file against its own dispatcher.
//
// # What the suite covers, and what it leaves to each package
//
// The shared gates: the seat's fence before a call and after it; a tool that
// panics; a call that runs under the run's context and not the MCP request's;
// arguments that did not parse; a call after the run ended; each call's place
// in the run's call log, fixed when it arrives; an activation answered once the
// client has re-listed; a suspend the run cannot resume, refused; and the class
// every refusal carries on the record.
//
// A gate only one of them has BY DESIGN stays in that package's own tests: the
// tool loop's round cap, its terminator barrier, its pause for a person's note
// and its answers withheld from a CLI it is stopping; the bridge's credential,
// its route and the durable ledger a resume reads. A detached run's loop is its
// CLI's own, and its phase is settled when the run is collected.
//
// # Through a real session, as a CLI calls
//
// A [Factory] builds a dispatcher of its kind over the surface the suite
// describes ([Spec]) and serves it through a REAL session: the suite connects
// an MCP client to it the way a coding CLI's own client does — over the socket
// for the tool loop's, over the bridge URL for the box's — lists the tools,
// calls them, gives up on them and hangs up. A suite that called the
// dispatcher around its session would test a path no CLI takes: the session's
// server, its tool list and its held answers are on the way of every call.
//
// The one call made around the session is the one no session can deliver: a
// call already past the transport when the run ended ([Served.Dispatcher]).
//
// # What a case asserts once a run is ending
//
// The two dispatchers answer a run that is ending differently, on purpose. The
// tool loop STOPS its CLI, so it withholds every answer from then until the
// CLI is gone — a CLI that read one would ask its model again, a request it
// would be billed for — and the conversation carries the real results. A box's
// CLI cannot be stopped from here, so the bridge answers at once that the
// run's tools are closed. So a case never waits for the answer to a call made
// once the run's end was decided: it reads the dispatcher's record of the call
// ([Served.Recorded]) and what the tool did, ends the run, and only then
// collects whatever reached the client.
package toolsessiontest

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/toolsession"
)

// Factory opens one run of a dispatcher over the surface spec describes, and
// serves it through a real session. It registers the teardown of whatever it
// built on t; the suite ends the run itself ([Served.End]).
type Factory func(t *testing.T, spec Spec) *Served

// Spec is the run a factory serves: the surface's tools, the seat's fence and
// the run's own context.
type Spec struct {
	// Context is the run's own context — the one it was served or launched
	// under. A call runs under its values; it is not cancelled while a case
	// runs.
	Context context.Context

	// Tools is every tool the surface can reach, and Offered the names it
	// offers from the start. A tool not offered is offered once a call
	// activates it ([ToolCall.Activate]).
	Tools   []Tool
	Offered []string

	// Fence is the seat's fence, which the dispatcher asks before every call
	// and after it: nil while the run may act for the seat.
	Fence func() error
}

// Tool is one of the surface's tools, as the suite defines it. The factory
// registers it on the surface its dispatcher runs calls through.
type Tool struct {
	Name string

	// Run is what the tool does, and its output.
	Run func(ctx context.Context, call ToolCall) string

	// Suspends marks a tool whose work outlives its call, as a Dashboard
	// Assistant proposal's does: its result asks the run to park with the
	// call unanswered. The run a factory serves is one that cannot be
	// resumed — a tool loop's that may not suspend, a box's — so the
	// dispatcher refuses it.
	Suspends bool
}

// ToolCall is one call as the tool sees it.
type ToolCall struct {
	Args map[string]any

	// Ordinal is what the run's call log counts for this call: how many
	// calls to the same tool, admitted before it, asked for something else.
	// The count an operation id carries — and fixed when the call ARRIVED,
	// not when the calls before it returned.
	Ordinal int

	// Activate offers another of the surface's tools, as activate_tool does,
	// and reports whether it resolved.
	Activate func(name string) bool
}

// Served is one run, served, as the suite drives it.
type Served struct {
	// Dial is a new connection to the run's session, the way a coding CLI's
	// own MCP client reaches it — and hangUp cuts it the way a CLI that dies
	// does: the socket closed, the requests in flight aborted. Never the
	// client's own Close, which a dead CLI does not send and which waits for
	// the calls in flight to be answered.
	Dial func(t *testing.T) (transport mcp.Transport, hangUp func())

	// Dispatcher is what the run's session hands each call to, for a call
	// that was already past the transport when the run ended.
	Dispatcher toolsession.Dispatcher

	// End ends the run the way its owner does — the tool loop's CLI run is
	// over and its session released; the bridge closes the run — and
	// returns once it has ended. Called with every call it holds free to
	// return, and safe to call more than once.
	End func(t *testing.T)

	// Recorded is the dispatcher's own record of every call it has answered
	// on this run, in any order: the tool loop's record of its phase, the
	// bridge's ledger.
	Recorded func(t *testing.T) []Recorded

	// UnparsedArguments is the answer a call whose arguments did not parse
	// is handed — the one rule the tool loop owns and the bridge borrows,
	// read from the implementation rather than copied here.
	UnparsedArguments func(name, why string) string
}

// Recorded is one call on the dispatcher's record.
type Recorded struct {
	// Seq is the call's place in the order the run's calls reached the
	// dispatcher, from 1.
	Seq int

	Name   string
	Output string
	Failed bool

	// Class is the refusal's class, read out of the recorded cause: "" for
	// a call that succeeded.
	Class crewletmcp.Refusal
}

// Run runs every case of the contract against the dispatcher factory builds,
// each case on a run of its own.
func Run(t *testing.T, factory Factory) {
	t.Helper()
	cases := []struct {
		name string
		fn   func(*testing.T, Factory)
	}{
		{"AClosedFenceRefusesTheCallAndEveryOneAfter", testAClosedFenceRefusesTheCallAndEveryOneAfter},
		{"AFenceThatClosesDuringACallRefusesEveryCallAfterIt", testAFenceThatClosesDuringACallRefusesEveryCallAfterIt},
		{"AToolThatPanicsFailsAndEndsTheRun", testAToolThatPanicsFailsAndEndsTheRun},
		{"ACallRunsUnderTheRunsContextNotTheRequests", testACallRunsUnderTheRunsContextNotTheRequests},
		{"ArgumentsThatDoNotParseAreAnsweredNotRun", testArgumentsThatDoNotParseAreAnsweredNotRun},
		{"ACallAfterTheRunEndedIsRefused", testACallAfterTheRunEndedIsRefused},
		{"CallsCountInTheOrderTheyArrived", testCallsCountInTheOrderTheyArrived},
		{"AnActivationIsAnsweredOnceTheClientHasRelisted", testAnActivationIsAnsweredOnceTheClientHasRelisted},
		{"ASuspendTheRunCannotResumeIsRefused", testASuspendTheRunCannotResumeIsRefused},
		{"EveryRefusalCarriesItsClass", testEveryRefusalCarriesItsClass},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			c.fn(t, factory)
		})
	}
}
