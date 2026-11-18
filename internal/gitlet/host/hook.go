package host

import (
	"context"
	"errors"
	"io"
	"net"
	"slices"

	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
)

// The hook's side of the relay: this binary, run by receive-pack as
// proc-receive in a push's scratch repository, relays its stdin to the engine
// and the engine's frames to its stdout and stderr. It decides nothing.

// hookArgv is the argv after the binary that starts the hook, written once so
// the spelling that starts it (HookCommand) and the one that recognises it
// (HookArgs) cannot drift; a second copy in cmd/crewlet would let a rename
// fail every push at proc-receive.
//
// No older spelling is recognised: cache.Open (step 4) rewrites
// hooks/proc-receive from HookCommand on every open, so an older binary's hook
// never outlives the restart, and a push in flight across it fails anyway.
var hookArgv = []string{"gitlet", "hook", "proc-receive"}

// HookCommand is the argv the cache's hooks/proc-receive execs for a binary at
// exe: [exe, gitlet, hook, proc-receive].
func HookCommand(exe string) []string { return append([]string{exe}, hookArgv...) }

// HookArgs reports whether args (os.Args[1:]) start the hook ([gitlet, hook,
// …]) and returns what follows, RunHook's args. main() and every test binary
// that pushes for real ask it first.
func HookArgs(args []string) (rest []string, ok bool) {
	prefix := hookArgv[:2]
	if len(args) < len(prefix) || !slices.Equal(args[:len(prefix)], prefix) {
		return nil, false
	}
	return slices.Clone(args[len(prefix):]), true
}

// The sentences the hook writes to the pushing client's stderr.
const (
	hookUsage       = "crewlet gitlet hook: run by the engine's git, never by hand"
	hookEngineGone  = "crewlet: the engine went away during this push; its outcome is unknown — fetch, then re-run git push"
	hookBadFrame    = "crewlet: the engine sent the hook a frame it cannot read"
	hookUnreachable = "crewlet: the engine's hook relay could not be reached; nothing was committed — re-run git push"
)

// RunHook is the proc-receive hook. args must be exactly ["proc-receive"] and
// gitproc.EnvHookSocket and EnvHookToken set, or it prints hookUsage and
// exits 2. It dials the socket within HookDialTimeout, sends its handshake,
// relays stdin, half-closing at EOF, and the engine's frames out: channel 1
// to stdout, 2 to stderr, 0 the exit status. A connection ending with no exit
// frame is an engine gone mid-push: outcome unknown, exit 1. It writes
// nothing else, since its stdout is git's protocol and its stderr the
// client's.
func RunHook(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	socket, token := getenv(gitproc.EnvHookSocket), getenv(gitproc.EnvHookToken)
	if !slices.Equal(args, hookArgv[2:]) || socket == "" || token == "" {
		_, _ = io.WriteString(stderr, hookUsage+"\n")
		return 2
	}
	// Only git, by killing the process, can end the hook, so the dial is
	// bounded by its timeout alone.
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
		// The half-close tells the engine git's stdin is done.
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
			// No exit frame: the engine went away mid-push.
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
