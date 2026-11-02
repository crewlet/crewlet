package mcp

import "slices"

// CallOutcome is what became of one call to an MCP server's tool, as a count
// of the server's calls files it: every call in exactly one.
//
// ONE RULE, read in two places that must agree: the turn's tally of its calls
// (types.ToolTally, counted as the turn runs) and a server's recent calls,
// read back out of the phase records that made them. Written twice, a call
// the tally counted as unavailable could be listed as failed beside it.
//
// A NAMED STRING TYPE with [CallOutcome.Valid], so an outcome a newer build
// answers is a value rather than a panic.
type CallOutcome string

const (
	// CallAnswered is a call the server answered without a failure.
	CallAnswered CallOutcome = "answered"

	// CallFailed is a call the server answered with a failure of its own.
	CallFailed CallOutcome = "failed"

	// CallUnavailable is a call the server never saw because it was not
	// running, or whose session ended under the call: the engine's
	// [RefusalUnavailable].
	CallUnavailable CallOutcome = "unavailable"

	// CallRefused is a call the engine refused before any tool ran: the
	// tool was not offered where it was called, or a guard held it.
	CallRefused CallOutcome = "refused"
)

// CallOutcomes is every outcome, in the order a reader lists them.
var CallOutcomes = []CallOutcome{CallAnswered, CallFailed, CallUnavailable, CallRefused}

// Valid reports whether an outcome off the wire is one this build knows.
func (o CallOutcome) Valid() bool { return slices.Contains(CallOutcomes, o) }

// OutcomeOf files one call to a server's tool: origin is who answered it
// (empty when nothing did), refusal the class the engine gave a failure it
// classified, and failed whether the call failed.
//
// NOTHING ANSWERED IT is a refusal whatever its class, because the call never
// reached the server; a call the engine answered as unavailable never reached
// it either, but was the server's to serve; anything else that failed, the
// server answered.
func OutcomeOf(origin string, refusal Refusal, failed bool) CallOutcome {
	switch {
	case origin == "":
		return CallRefused
	case refusal == RefusalUnavailable:
		return CallUnavailable
	case failed:
		return CallFailed
	}
	return CallAnswered
}
