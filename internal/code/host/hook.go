package host

import (
	"context"
	"errors"
	"io"
	"net"
	"slices"

	"github.com/crewlet/crewlet/internal/code/gitproc"
)

// THE HOOK'S SIDE OF THE RELAY: the engine's own binary, started by
// receive-pack as proc-receive in a push's scratch repository, relaying its
// stdin to the engine and the engine's frames to its stdout and stderr. It
// decides nothing: everything a push is checked against is the engine's.

// hookArgv is the argv after the binary that starts the hook — `code hook
// proc-receive` — written ONCE, so the spelling that STARTS the hook
// (HookCommand) and the spelling that RECOGNISES it (HookArgs) cannot drift:
// a second copy in cmd/crewlet is how an upgrade renames one and every push
// fails `proc-receive`.
var hookArgv = []string{"code", "hook", "proc-receive"}

// HookCommand is the argv the cache's hooks/proc-receive execs for a binary at
// exe: [exe, code, hook, proc-receive].
func HookCommand(exe string) []string { return append([]string{exe}, hookArgv...) }

// HookArgs reports whether args — a process's os.Args[1:] — start the hook
// ([code, hook, …]), and returns what follows (RunHook's args). main() and
// every test binary that pushes for real ask it first.
func HookArgs(args []string) (rest []string, ok bool) {
	prefix := hookArgv[:2]
	if len(args) < len(prefix) || !slices.Equal(args[:len(prefix)], prefix) {
		return nil, false
	}
	return slices.Clone(args[len(prefix):]), true
}

// The sentences the hook writes to the pushing client's stderr.
const (
	hookUsage       = "crewlet code hook: run by the engine's git, never by hand"
	hookEngineGone  = "crewlet: the engine went away during this push; its outcome is unknown — fetch, then re-run git push"
	hookBadFrame    = "crewlet: the engine sent the hook a frame it cannot read"
	hookUnreachable = "crewlet: the engine's hook relay could not be reached; nothing was committed — re-run git push"
)

// RunHook is the proc-receive hook. args must be exactly ["proc-receive"] and
// both of the relay's variables set — getenv(gitproc.EnvHookSocket),
// getenv(gitproc.EnvHookToken) — or it prints the hand-run sentence and exits
// 2. It dials the socket within HookDialTimeout, writes its handshake, copies
// stdin to the connection until EOF and half-closes it, and writes the
// engine's frames out: channel 1 to stdout, 2 to stderr, and 0 the exit
// status it returns. A connection that ends with no exit frame is an engine
// that went away: its outcome is unknown, exit 1. It writes nothing else
// anywhere — its stdout is git's protocol and its stderr the client's.
func RunHook(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	socket, token := getenv(gitproc.EnvHookSocket), getenv(gitproc.EnvHookToken)
	if !slices.Equal(args, hookArgv[2:]) || socket == "" || token == "" {
		_, _ = io.WriteString(stderr, hookUsage+"\n")
		return 2
	}
	// THE HOOK'S OWN LIFETIME is the process's: nothing above it can end it
	// but git, which kills it, so the dial is bounded by its timeout alone.
	conn, err := (&net.Dialer{Timeout: HookDialTimeout}).DialContext(context.Background(), "unix", socket)
	if err != nil {
		_, _ = io.WriteString(stderr, hookUnreachable+"\n")
		return 1
	}
	defer func() { _ = conn.Close() }()
	if _, err := io.WriteString(conn, hookHandshake+token+"\n"); err != nil {
		_, _ = io.WriteString(stderr, hookEngineGone+"\n")
		return 1
	}
	go func() {
		// THE HOOK'S STDIN IS GIT'S, relayed until git closes it; the
		// half-close tells the engine nothing more is coming.
		_, _ = io.Copy(conn, stdin)
		if uc, ok := conn.(*net.UnixConn); ok {
			_ = uc.CloseWrite()
		}
	}()
	for {
		ch, payload, err := readFrame(conn)
		switch {
		case errors.Is(err, errFrame):
			_, _ = io.WriteString(stderr, hookBadFrame+"\n")
			return 1
		case err != nil:
			// THE CONNECTION ENDED WITH NO EXIT FRAME — closed, reset or
			// cut — which is an engine that went away mid-push.
			_, _ = io.WriteString(stderr, hookEngineGone+"\n")
			return 1
		}
		switch ch {
		case frameExit:
			return int(payload[0])
		case frameStdout:
			if _, err := stdout.Write(payload); err != nil {
				return 1
			}
		case frameStderr:
			_, _ = stderr.Write(payload)
		}
	}
}
