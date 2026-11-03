package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
)

// mainProbeEnv makes this test binary run main() itself, with the arguments it
// was started with — the one way to watch what main() does before anything a
// test harness installs.
const mainProbeEnv = "CREWLET_MAIN_PROBE"

// runMainProbe is the child: main(), and the exit main() did not take.
func runMainProbe() {
	main()
	os.Exit(0)
}

// probeHook runs this binary's main() as git starts the hook — `code hook
// proc-receive` — under env, and answers what it printed and its status.
func probeHook(t testing.TB, env ...string) (stdout, stderr string, status int) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "code", "hook", "proc-receive")
	// THE ENVIRONMENT AS GIT HANDS IT OVER: only what the test names, beside
	// the probe's switch and the stand-in's race options (a race-built child
	// otherwise sleeps a second at exit).
	cmd.Env = append([]string{mainProbeEnv + "=1",
		procgrouptest.GORACE + "=" + procgrouptest.StandInRaceOptions()}, env...)
	cmd.Stdin = strings.NewReader("")
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		status = exit.ExitCode()
	default:
		t.Fatalf("run the probe: %v", err)
	}
	return out.String(), errOut.String(), status
}

// THE HOOK IS main()'S FIRST STATEMENT. The engine's git starts this binary as
// `crewlet code hook proc-receive` inside a push, and its stdout is git's
// protocol and its stderr the pushing person's terminal — so nothing the
// CLI's setup prints may reach either, and a hand-run is told what it is.
//
// Mutation: dispatch the hook inside run, and the hand-run answers `unknown
// command` with exit 1; dispatch it after logging.Configure and log a line at
// boot, and the stderr assertions go red.
func TestTheHookIsMainsFirstStatement(t *testing.T) {
	t.Parallel()
	stdout, stderr, status := probeHook(t)
	if status != 2 || stdout != "" || stderr != "crewlet code hook: run by the engine's git, never by hand\n" {
		t.Fatalf("a hand-run hook exited %d, stdout %q, stderr %q; want 2, nothing, and the "+
			"hand-run sentence alone", status, stdout, stderr)
	}

	// THE RELAY'S SIDE, spelled as the engine speaks it: the handshake line
	// naming the token, then frames of a channel byte and a big-endian
	// length — stdout, stderr, and the exit status last.
	socket := filepath.Join(t.TempDir(), "h")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	handshake := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			handshake <- "accept: " + err.Error()
			return
		}
		defer func() { _ = conn.Close() }()
		line, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil {
			handshake <- "read: " + err.Error()
			return
		}
		handshake <- line
		for _, f := range []struct {
			ch      byte
			payload string
		}{{1, "0000"}, {2, "hi\n"}, {0, "\x03"}} {
			var head [5]byte
			head[0] = f.ch
			binary.BigEndian.PutUint32(head[1:], uint32(len(f.payload)))
			if _, err := conn.Write(append(head[:], f.payload...)); err != nil {
				return
			}
		}
		_, _ = io.Copy(io.Discard, conn)
	}()
	stdout, stderr, status = probeHook(t, gitproc.EnvHookSocket+"="+socket, gitproc.EnvHookToken+"=t")
	if got := <-handshake; got != "CREWLET-HOOK 1 t\n" {
		t.Fatalf("the relay read %q, want the handshake naming the token", got)
	}
	if status != 3 || stdout != "0000" || stderr != "hi\n" {
		t.Fatalf("the hook exited %d, stdout %q, stderr %q; want the relay's 3, 0000 and hi",
			status, stdout, stderr)
	}
}
