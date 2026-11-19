// Package toolsession serves one phase's tools to one harness run, over MCP:
// the engine's end of `crewlet mcp-relay`.
//
// # Why the tools travel this way
//
// Every model call runs a vendor's harness, and each of those harnesses is an MCP
// client in its headless mode. So a seat's tools reach its harness as ONE MCP
// server, and the harness calls them natively, in its own loop, the way it calls any
// tool — rather than as a catalogue pasted into the prompt and a JSON reply the
// engine has to find in the model's prose.
//
// # What this package is, and is not
//
// It is the transport: an MCP server kept in step with what a [Dispatcher]
// offers, and a unix socket per harness run that the relay connects to. It decides
// nothing about a call. The dispatcher — the tool loop, which holds the phase —
// is where the seat's grant, the skill guard, the fence, the budget, the round
// cap, the terminator and the record are; a call reaches it exactly as the harness
// made it, arguments that did not parse included.
//
// # The socket and who can reach it
//
// A session's socket sits in a directory this process created for itself
// ([NewHost]): fresh, random, mode 0700, checked rather than trusted. That
// directory is the whole of what admits a connection — the socket carries no
// token, and its path on the relay's command line is not a secret — so it
// separates the engine's user from every other user on the host. It does NOT
// separate one seat from another: every seat's harness runs as the engine's user,
// and what keeps one seat's model off another's tools is each harness's own tool
// confinement, which leaves it no shell (see docs/concepts/llm-backends.md).
//
// ONE SOCKET PER HARNESS RUN, retired when the run ends. A run that outlives its
// end — a harness killed past its grace whose relay was in a process group of its
// own — finds its socket gone rather than the next run's calls. The socket goes
// only after the calls running on it have returned.
//
// # What a dispatcher is handed of a call
//
// A call runs under the context of the run its dispatcher serves, never the MCP
// request's: a harness that cancels or drops a call must not cancel a tool
// half-way through, and the request's context is not on offer to be used by
// mistake. What a dispatcher gets of the request is [Call.Abandoned], [Call.Over]
// and [Call.Running].
//
// A harness counts as connected once it has LISTED the tools, not once it sent
// `notifications/initialized`, which a client on a newer protocol revision never
// sends. For a client that re-reads the list when told it changed (every harness
// whose profile re-lists, and every box), the answer to a call that changed it is
// held until the client has listed again, for two seconds at most
// ([Server.HoldAnswersForRelist]): answered at once, Gemini CLI, Copilot and
// OpenCode asked their model again first and offered an activated tool a request
// late.
//
// # A running call's answer, kept going where its transport needs it
//
// A server can keep the answer of every call its dispatcher runs going while
// the call's tool runs ([Server.KeepAnswersGoing]): a progress on the call's
// own token the moment the call starts running ([Call.Running]), another
// every cadence, none once its tool has returned — and a progress that can no
// longer be written tells the dispatcher nobody can read the answer
// ([Call.Abandoned]). The bridge turns it on for a box's harness, which dials
// it over streamable HTTP, where a call's response is silent until its first
// event and a harness gives up on one silent for too long
// (internal/api/mcpbridge). This package's own sessions do not: a harness
// reaches them over the relay's stdio, where no harness times a response's
// first byte, and the tool loop answers a call still running "still running"
// itself, before the harness's own tool timeout by a tenth of it and at least
// five seconds — the timeout every harness profile states
// (internal/agent/toolloop, internal/providers/llm/harness/harnessprofile).
//
// # An answer before the tool returns
//
// A dispatcher may answer a call while its tool runs on, and then finish and
// record the call itself: the tool loop near its harness's tool timeout, as
// above, and the bridge as the opening a call was made on ends. That answer,
// like the tool loop's, is not a failure: a harness hands its model a failed
// result as a call that failed, and a model makes a failed call again. The
// server writes such an answer as it writes any, after the beat is stopped and
// waited for. What the dispatcher may need besides is when that answer is out
// ([Call.Over]), since a session whose close has begun writes no answer: the
// bridge waits on it before it closes the sessions of the opening that ended.
//
// Over is the request's context's Done, which the SDK ends only once the
// answer's write has returned (go-sdk v1.8.0). That is the SDK's behaviour and
// not its promise, so TestACallsRequestIsOverOnlyOnceItsAnswerIsWritten holds
// it. The SDK ends it sooner only where it cancels the request first: on the
// harness's `notifications/cancelled` while the answer is still to come, and
// on the connection's failure. Then nobody reads the answer, so a wait on Over
// that lets go before its write loses nothing. Unlike [Call.Abandoned],
// nothing here widens it: a progress that finds the call's response gone, and
// the handler's own return, close Abandoned and leave Over to the transport.
//
// An answer may carry images ([Result.Images]), written after its text so a
// client reading none still reads it, and held to internal/mcp's CheckImages.
//
// # No listener anywhere
//
// Nothing here binds a network port. A node may bind none at all (`api.port:
// 0`), and its seats still have their tools.
package toolsession
