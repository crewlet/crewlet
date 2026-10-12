// Package mcp connects the engine to Model Context Protocol servers and hands
// their tools to a phase surface.
//
// It owns four things a caller must not have to reimplement:
//
//   - THE CHILD. A stdio server is a subprocess the engine forked. Its
//     environment, its stderr, its death, and the tree it may have spawned
//     underneath it are this package's problem, not the caller's.
//   - ITS LIVENESS. A server's session can end at any moment after it
//     started — a child crashes, a remote server forgets the session — and
//     a start can fail for a reason that passes. Every server a bridge runs
//     is supervised until it is stopped: the end is seen at the transport,
//     the server restarted on a schedule, and its tools refuse honestly
//     while it is down. See supervise.go.
//   - THE DEADLINES. An MCP server's characteristic failure is not an error,
//     it is a SILENCE. A server that launches and never completes the
//     handshake, or answers tools/list and then never returns from a call,
//     raises nothing at all: every error branch around it is dead code while
//     it hangs. Nothing else in the stdio path has a ceiling, and the engine
//     starts these on the seat-acquisition path — so one mute server does not
//     merely lose its own tools, it holds up every seat behind it for the life
//     of the process.
//   - THE CATALOGUE INDEX. Which tools a server contributes, and therefore
//     which names must come back out of the shared registry when that server
//     stops, restarts thinner, or fails to come back at all.
//
// Five decisions here are load-bearing, and each replaced a real incident:
//
//   - REGISTRATION HAPPENS ONLY ON SUCCESS. A server recorded before its tools
//     were discovered, whose discovery then failed, is a live subprocess with
//     no tools that answers "yes" to Has: a live config edit reads it as
//     healthy, the engine's own retry never fires, and the process sits there
//     until shutdown. A bridge that retries failed starts keeps the SPEC of
//     one, never a process or a tool, and Has still says no.
//   - THE ENGINE WAITS FOR ITS OWN CHILD. The SDK's command transport waits
//     for a child only inside its shutdown, so a server that died on its own
//     was neither reaped nor noticed: its tools stayed in every catalogue and
//     every call failed on a broken pipe until something restarted it — for
//     a shared server the next config apply, which on a quiet company is
//     never. The stdio transport is this package's own so that the one Wait
//     is made from the start, and a death is an event (stdio.go).
//   - THE DOOMED NAMES ARE CAPTURED BEFORE THE STOP. Stopping drops the
//     bridge's own index, so anything asking afterwards "what did that server
//     contribute?" gets an empty answer — and the names stay in the shared
//     registry for ever, advertising tools that dispatch into a stopped
//     client. Stop and Restart therefore RETURN what must be unregistered;
//     the caller cannot forget to ask, and cannot ask too late.
//   - ANNOTATIONS ARE TRI-STATE. "The server did not say" is not "the server
//     said no". See annotations.go — the SDK's own struct flattens two of the
//     four hints, and reading it naively inverts the sub-agent guard for every
//     under-annotated tool in the company.
//   - A PER-ROLE INSTANCE IS A SEPARATE CHILD. Two roles never share one
//     server process, because the credentials in its environment are one
//     seat's identity. See instance.go.
//
// The wire vocabulary (content blocks, hints, tool defs) is this package's
// own, not the SDK's. Nothing outside here should have to know which SDK the
// engine speaks MCP with, and the SDK's typed surface is lossy in one place
// that matters (again: annotations.go).
package mcp
