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
// Six decisions here are load-bearing, and each replaced a real incident:
//
//   - A STDIO CHILD GETS AN ALLOWLIST, NEVER THE ENGINE'S ENVIRONMENT. Its
//     environment is [hostbox]'s allowlist, the engine user's HOME, TMPDIR
//     and XDG directories ([hostbox.HostUserEnv], because a server started
//     through `npx` or `uvx` keeps its package cache there), and what its
//     `env:` and the seat's `mcp_env` declare, the declaration winning — and,
//     for a server whose command is `docker` or `podman`, the runtime's own
//     settings besides ([hostbox.ContainerRuntime]), because that server IS
//     the runtime's CLI. It is never os.Environ(). That was the obvious shape and it
//     was this package's for a long time, on the reasoning that servers read
//     undeclared conventional variables and narrowing the set would break
//     them — and it handed every tool server the company declared the
//     engine's keyring (which signs every session cookie and state-log
//     record) and every Tier A token value: to a process pulled off a
//     package registry that logs its
//     environment on a crash, forwards it to its own children and reports
//     it in telemetry. A server that needs anything else says so in `env:`,
//     where a `${VAR}` passes the host's value on by name, and a key a host may
//     leave unset is listed in the server's `optional_keys`. IT IS NOT
//     ISOLATION, and nothing here claims it is: the child runs as the
//     engine's user and can read what that user can, the Tier A file and the
//     engine's own /proc/<pid>/environ among it. What the allowlist removes
//     is the engine HANDING its secrets to code that never asked for them;
//     keeping a hostile server away from them takes a different user or a
//     container. See mergedEnv.
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
// # Replacing a server
//
// Only what the process runs from restarts it ([Spec.sameProcess]); a tool's
// prefix, level or hints refile it in place ([Bridge.Refile]). A restart
// ([Bridge.Restart]) stops the old process before starting the new, so a seat's
// child never runs twice, but drops no call: those in flight finish first, and
// one made meanwhile waits and is made on the replacement if it lists the tool
// unchanged, else refused unsent (replace.go). [Tool.CallBound] counts the wait.
//
// # Levels, digests and tests
//
// A tool's level is decided where it is filed ([Spec.Levels], by the server's
// own name): a blocked tool is never built as a [Tool], only kept as a
// definition ([Bridge.BlockedTools]), so every reader of a seat's surface
// agrees it is absent; an approve tool is filed and never called through a
// catalogue until a person answers (ADR-0077). [RiskOf] is the one reading of
// hints as a risk, and an undeclared tool is destructive. Each running session
// is listed again on the server's `ttlMs`, held between [RelistFloor] and
// [RelistCeiling], and a tool whose definition moved is held for a person
// ([Tool.Unreviewed], ADR-0078). The digest hashes only the server's own
// definition, so a company's edit never reads as the server changing a tool.
//
// A failure's class ([FailureClass]) is decided where the facts are, never
// parsed back from a sentence. A restart follows one schedule ([RestartWait])
// whoever retries, and a per-role child starts only where its owner says so
// ([Supervision.MayStart]), since the child is a seat's identity. [Test] and
// [CheckAddress] never call a tool, and a remote connection follows a redirect
// within its configured origin alone, sending its headers there alone.
//
// The wire vocabulary (content blocks, hints, tool defs) is this package's
// own, not the SDK's. Nothing outside here should have to know which SDK the
// engine speaks MCP with, and the SDK's typed surface is lossy in one place
// that matters (again: annotations.go).
//
// A first-party tool may answer with images ([Result.Images], [CheckImages]);
// an image cannot be fenced (ADR-0040), so its text says whose it is. A record
// keeps [ImageRecords] (type and size), never the bytes; a third-party server's
// image stays a one-line descriptor.
package mcp
