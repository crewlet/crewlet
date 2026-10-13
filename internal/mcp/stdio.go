package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/procgroup"
)

// childProcess is the stdio half of a client: the process, the pipe its last
// words come out of, and the one record of how it ended.
type childProcess struct {
	cmd   *exec.Cmd
	relay *stderrRelay

	// exited is closed once the process has been waited for, and waitErr is
	// what that Wait returned — written before the close, so a reader that
	// saw the channel closed reads it without a lock. ownExit, written with
	// it, is whether the process ended before the engine closed its stdin:
	// an exit of its own rather than one the shutdown ladder caused.
	exited  chan struct{}
	waitErr error
	ownExit bool

	// stdinClosed is set by the shutdown ladder just before it closes the
	// child's stdin, which is what ownExit is judged against.
	stdinClosed atomic.Bool

	// ended is told the moment the session over this child can carry no
	// more calls: the process exited, or a write to its stdin failed; and
	// over reports whether that moment has come, by any sign the session
	// saw. Both set by the client before the transport starts anything, so
	// neither the reaper, a writer nor the ladder reads them under a lock.
	ended func(cause string)
	over  func() bool
}

// groupPID is the process group to signal, read at signal time.
//
// DERIVED rather than captured, because the one path where a package runner's
// grandchild is most likely to have survived is a server that never came up —
// and a field assigned only after a successful handshake is zero exactly
// there. procgroup.Kill(0) is refused by design and returns nil, so the reap
// logged success while signalling nothing at all.
//
// The group leader is the child itself: procgroup.Set puts it in its own
// group at fork, so its pid IS the group id. Zero when the process never
// started, which is the one case there is genuinely nothing to signal.
func (c *childProcess) groupPID() int {
	if c == nil || c.cmd == nil || c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
}

// reap is the ONE Wait this process makes, and it runs from the moment the
// child starts until the child ends.
//
// ONE, because a second Wait on the same process is an error, and the SDK's
// own command transport keeps that call to itself: it waits only inside its
// shutdown, so a server that DIED on its own was not reaped and not noticed
// until something stopped it — its entry stayed "running", its tools stayed
// in every catalogue, and every call failed on a broken pipe. Waiting here,
// from the start, is what turns the death into an event the bridge can act
// on (see supervise.go), and the shutdown ladder below waits on the same
// record rather than calling Wait again.
func (c *childProcess) reap() {
	err := c.cmd.Wait()
	c.waitErr = err
	c.ownExit = !c.stdinClosed.Load()
	close(c.exited)
	if c.ended != nil {
		c.ended("its process exited (" + exitStory(err) + ")")
	}
}

// hasExited reports whether the process has been waited for.
func (c *childProcess) hasExited() bool {
	select {
	case <-c.exited:
		return true
	default:
		return false
	}
}

// awaitExit waits up to d for the process to end, reporting whether it did
// and, when it did, what its Wait returned.
func (c *childProcess) awaitExit(d time.Duration) (bool, error) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-c.exited:
		return true, c.waitErr
	case <-t.C:
		return false, nil
	}
}

// exitStory says how the process ended, for a log line and a refusal: the
// status a well-behaved exit leaves, or the signal that ended it.
func exitStory(err error) string {
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return "exit status 0"
	case errors.As(err, &exitErr):
		return exitErr.Error()
	default:
		return err.Error()
	}
}

// newStdioTransport builds the child and the transport that will start it.
//
// It does NOT start anything: the SDK calls the transport's Connect inside its
// own, which is also what makes the startup deadline meaningful (spawning the
// process is inside the window, not before it).
func newStdioTransport(spec Spec, log *slog.Logger) (sdk.Transport, *childProcess, error) {
	relay, err := newStderrRelay(spec.Name, log)
	if err != nil {
		return nil, nil, fmt.Errorf("mcp: server %q: stderr pipe: %w", spec.Name, err)
	}

	// exec.Command, never exec.CommandContext. The context here carries the
	// STARTUP deadline, and CommandContext would kill the server the moment
	// that deadline passed — that is, kill every healthy server 120 seconds
	// after it came up. The child's lifetime is owned by Stop.
	cmd := exec.Command(spec.Command, spec.Args...) //nolint:gosec,noctx // the command IS the operator's config; the deliberate lack of a context is explained above
	cmd.Env = mergedEnv(spec, log)
	cmd.Stderr = relay.writer()
	procgroup.Set(cmd)

	child := &childProcess{cmd: cmd, relay: relay, exited: make(chan struct{})}
	return &stdioTransport{child: child}, child, nil
}

// stdioTransport starts a server and speaks newline-delimited JSON-RPC over
// its stdin and stdout.
//
// THE ENGINE'S OWN rather than the SDK's command transport, for one reason:
// the Wait. The SDK's transport waits for its child only inside Close, so
// nothing reaps or notices a child that dies on its own; and since a process
// may be waited for once, nobody else can wait for it either. This one waits
// from the start (childProcess.reap) and walks the same shutdown ladder on
// the result.
//
// BOTH PIPES ARE *os.File, made here, for the reason the stderr relay's write
// end is: handed anything else, exec builds a pipe and a copying goroutine of
// its own, and its Wait closes the parent's ends the moment the process exits
// — before the reader has drained what the server wrote last, which is the
// answer to the call it died after. With plain files, Wait closes nothing of
// ours, and the reader reads to the end of what was written.
type stdioTransport struct {
	child *childProcess
}

func (t *stdioTransport) Connect(ctx context.Context) (sdk.Connection, error) {
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		closeFiles(inR, inW)
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd := t.child.cmd
	cmd.Stdin, cmd.Stdout = inR, outW
	if err := cmd.Start(); err != nil {
		closeFiles(inR, inW, outR, outW)
		return nil, err
	}
	// The child holds its own copies now, so the parent's go at once. The
	// one that matters is stdout's WRITE end: while this process holds a
	// copy, the pipe never reaches EOF, and the reader would never see a
	// server that exited.
	closeFiles(inR, outW)
	go t.child.reap()

	return (&sdk.IOTransport{
		Reader: io.NopCloser(outR),
		Writer: &childInput{child: t.child, stdin: inW, stdout: outR},
	}).Connect(ctx)
}

// childInput is the write half of the connection, and its Close is the
// shutdown ladder the SDK's command transport walks: close stdin, wait, SIGTERM,
// wait, SIGKILL, wait — each rung shutdownGrace long, each wait on the reaper's
// record rather than a Wait of its own.
//
// The read half's Close is a no-op, as the SDK's is: the connection is ended
// by closing stdin, and stdout is closed only once the ladder is done, the
// moment exec would have closed it.
type childInput struct {
	child  *childProcess
	stdin  *os.File
	stdout *os.File
}

// Write sends to the child, and a write that fails ends the session: nothing
// is reading the other end of the pipe any more, so no later call can be
// heard either. Said HERE, before the failure reaches the call that made it,
// so that call's refusal already knows the server is gone rather than
// reporting a broken pipe — which is all the SDK would have to say.
func (w *childInput) Write(p []byte) (int, error) {
	n, err := w.stdin.Write(p)
	if err != nil && w.child.ended != nil {
		w.child.ended("writing to it failed (" + err.Error() + ")")
	}
	return n, err
}

func (w *childInput) Close() error {
	// LAST, whatever the ladder concluded: a reader still blocked on a pipe
	// a surviving grandchild holds is ended here rather than left for ever.
	defer func() { _ = w.stdout.Close() }()
	// A SESSION THAT HAS ALREADY ENDED — the SDK closes the connection
	// itself when its read fails — may be a process exiting on its own,
	// whose reaper is a scheduling delay behind the end of its output. It
	// gets [reapGrace] to be reaped before its stdin is closed, so that an
	// exit of its own is recorded as one (childProcess.ownExit) rather than
	// raced by the exit closing stdin causes.
	if w.child.over != nil && w.child.over() && !w.child.hasExited() {
		// Only the wait is wanted here: whether it exited, and how, the
		// first rung below reads again.
		_, _ = w.child.awaitExit(reapGrace)
	}
	w.child.stdinClosed.Store(true)
	if err := w.stdin.Close(); err != nil {
		return fmt.Errorf("closing stdin: %w", err)
	}
	if exited, err := w.child.awaitExit(shutdownGrace); exited {
		return err
	}
	// A failed SIGTERM moves straight to SIGKILL rather than waiting a rung
	// for a signal that was never delivered.
	if err := w.child.cmd.Process.Signal(syscall.SIGTERM); err == nil {
		if exited, err := w.child.awaitExit(shutdownGrace); exited {
			return err
		}
	}
	if err := w.child.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	if exited, err := w.child.awaitExit(shutdownGrace); exited {
		return err
	}
	return errors.New("unresponsive subprocess")
}

func closeFiles(files ...*os.File) {
	for _, f := range files {
		_ = f.Close()
	}
}

// mergedEnv is the child's environment: everything this process has, with the
// server's own declared variables layered on top.
//
// WHOLE-ENVIRONMENT INHERITANCE IS DELIBERATE. MCP servers routinely read
// undeclared conventional variables — PATH, HOME, the proxy variables, a
// vendor SDK's own key — so narrowing this to the declared set breaks servers
// that work today, in a way that surfaces as a server failing to start with an
// error from someone else's code.
//
// Note what it does NOT do: secret-store values are not poured in. spec.Env
// has already had its ${VAR} references resolved by the caller, so a server
// receives exactly the stored credentials its own config declares and no
// others. Injecting the whole store here would hand every seat's token to
// every subprocess in the company, which is strictly worse than the problem it
// would solve.
func mergedEnv(spec Spec, log *slog.Logger) []string {
	env := make(map[string]string, len(os.Environ())+len(spec.Env))
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	keys := make([]string, 0, len(spec.Env))
	var empty []string
	for k, v := range spec.Env {
		env[k] = v
		keys = append(keys, k)
		if v == "" {
			empty = append(empty, k)
		}
	}
	slices.Sort(keys)
	slices.Sort(empty)

	if len(empty) > 0 {
		// Almost always an unresolved ${VAR}: the server will come up, fail
		// to authenticate, and report something unrelated. Say it here, where
		// the variable name is still in hand.
		log.Warn("empty_env_vars", "server", spec.Name, "empty_keys", empty)
	}
	// Keys only, never values — this line exists to debug a missing variable,
	// and the value is the credential.
	log.Debug("custom_env_keys", "server", spec.Name, "keys", keys)

	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	// Sorted so a child's environment is reproducible across runs, which is
	// the difference between a comparable log line and a new one every boot.
	slices.Sort(out)
	return out
}
