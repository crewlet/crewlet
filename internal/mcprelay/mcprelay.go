// Package mcprelay is the stdio end of a seat's tool session: the process a
// harness starts as its one MCP server, `crewlet mcp-relay <socket>`, which
// copies the harness's stdin to the engine's per-invocation unix socket and the
// socket back to the harness's stdout, and does nothing else.
//
// # Why a relay, and not the session itself
//
// A harness reaches an MCP server in one of two ways every vendor supports: a
// command it starts and speaks to over stdio, or an HTTP URL. A URL needs a
// listener, and a node is allowed to bind none (`api.port: 0`, a seats node
// that accepts no connection of any kind), so the URL route would make the
// seat's tools depend on a setting that has nothing to do with them. A command
// is a process the harness owns, and the seat's tools are objects in the ENGINE's
// process — so the command has to be a relay into the engine, over something
// that is not a network listener: a unix socket in a directory only the engine's
// user can open (internal/toolsession).
//
// # It reads nothing but its argument
//
// Not the environment, not a config file, not a log setting. A harness decides the
// environment its MCP servers start in, and the measured harnesses disagree about
// it: some pass their whole environment through, some pass a short allowlist
// (HOME, PATH, TMPDIR), so anything the relay read from the environment would
// work under one harness and silently not under the next. The socket path on argv
// is the whole of what it needs, and it is not a secret: the directory around
// it is what admits a connection.
//
// And it writes nothing but the bytes it relays, plus one line on stderr when
// it cannot start: stdout IS the MCP stream, so a single stray byte there is a
// protocol error the harness reports as a broken server.
//
// # Dispatched on argv
//
// The binary's switch and [DispatchIfRelay] in a test binary standing in for the
// engine both key on argv, never on a variable: a dispatch keyed on the
// environment falls through to the whole test suite the first time a harness
// strips it. The relay ends when the engine closes the socket, and half-closes
// when the harness closes stdin so the answer to a call already sent arrives.
package mcprelay

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
)

// Command is the subcommand the relay runs as: `crewlet mcp-relay <socket>`.
//
// One name for the binary's dispatch, the argv a harness is handed, and the test
// binaries that stand in for the engine ([DispatchIfRelay]) — written out three
// times, a rename would leave a harness starting a command the binary no longer
// answers.
const Command = "mcp-relay"

// Run relays stdin to the socket at args[0] and the socket to stdout, until
// either side ends.
//
// The engine's end ends it: when it retires the invocation it closes the
// socket and the relay returns once the copy to stdout sees that, whatever the
// harness still does with stdin, since nothing sent after that reaches a tool.
//
// When the harness finishes first it closes the relay's stdin, which is
// half-closed on the socket so the engine answers what was in flight and
// closes its side, ending the copy and the relay. Closing both directions at
// the first EOF would drop the answer to a call already sent.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) != 1 || !filepath.IsAbs(args[0]) {
		fmt.Fprintf(stderr, "usage: crewlet %s <absolute path of the engine's tool socket>\n", Command)
		return errors.New("mcp-relay: expects exactly one argument, the absolute path of a socket")
	}
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: args[0], Net: "unix"})
	if err != nil {
		// THE ONE LINE THIS EVER WRITES, on stderr, which every harness keeps
		// apart from the MCP stream and most surface in their own log of a
		// server that failed to start: the socket it could not reach, and
		// that it is the engine's tool session that is missing.
		fmt.Fprintf(stderr, "crewlet %s: cannot reach the engine's tool session at %s: %v\n",
			Command, args[0], err)
		return fmt.Errorf("mcp-relay: %w", err)
	}
	defer func() { _ = conn.Close() }()

	go func() {
		// The harness finished: half-close, so the engine still answers what it
		// has (see above). Errors are not reported — a write to a socket the
		// engine already closed fails here, and that end is reported by the
		// copy below, which is the one the relay waits on.
		_, _ = io.Copy(conn, stdin)
		_ = conn.CloseWrite()
	}()
	if _, err := io.Copy(stdout, conn); err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("mcp-relay: relaying to the harness: %w", err)
	}
	return nil
}

// DispatchIfRelay runs this process as the relay when it was started as one —
// `<binary> mcp-relay <socket>` — and exits; otherwise it returns at once.
//
// For every test binary that stands in for the engine: the engine names its
// own executable as the relay ([os.Executable]), under `go test` the test
// binary, which without this would run the whole suite again, whose engines
// start harnesses that start relays. Dispatched on ARGV, never an environment
// variable: several harnesses pass only a short allowlist, and an
// environment-keyed dispatch would fall through to the suite when one stripped
// it. The binary's own dispatch is [Run] in its command switch.
func DispatchIfRelay() {
	if len(os.Args) < 2 || os.Args[1] != Command {
		return
	}
	if err := Run(os.Args[2:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}
