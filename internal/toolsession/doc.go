// Package toolsession serves one phase's tools to one coding-CLI run, over MCP:
// the engine's end of `crewlet mcp-relay`.
//
// # Why the tools travel this way
//
// Every model call runs a vendor's coding CLI, and each of those CLIs is an MCP
// client in its headless mode. So a seat's tools reach its CLI as ONE MCP
// server, and the CLI calls them natively, in its own loop, the way it calls any
// tool — rather than as a catalogue pasted into the prompt and a JSON reply the
// engine has to find in the model's prose.
//
// # What this package is, and is not
//
// It is the transport: an MCP server kept in step with what a [Dispatcher]
// offers, and a unix socket per CLI run that the relay connects to. It decides
// nothing about a call. The dispatcher — the tool loop, which holds the phase —
// is where the seat's grant, the skill guard, the fence, the budget, the round
// cap, the terminator and the record are; a call reaches it exactly as the CLI
// made it, arguments that did not parse included.
//
// # The socket and who can reach it
//
// A session's socket sits in a directory this process created for itself
// ([NewHost]): fresh, random, mode 0700, checked rather than trusted. That
// directory is the whole of what admits a connection — the socket carries no
// token, and its path on the relay's command line is not a secret — so it
// separates the engine's user from every other user on the host. It does NOT
// separate one seat from another: every seat's CLI runs as the engine's user,
// and what keeps one seat's model off another's tools is each CLI's own tool
// confinement, which leaves it no shell (see docs/concepts/llm-backends.md).
//
// ONE SOCKET PER CLI RUN, retired when the run ends. A run that outlives its
// end — a CLI killed past its grace whose relay was in a process group of its
// own — finds its socket gone rather than the next run's calls.
//
// # No listener anywhere
//
// Nothing here binds a network port. A node may bind none at all (`api.port:
// 0`), and its seats still have their tools.
package toolsession
