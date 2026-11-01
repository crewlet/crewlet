package mcprelay

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// socket listens on a fresh unix socket and returns its path and listener.
//
// In a directory of its own under the system's temp root rather than the
// test's, which can be long enough to pass the 104-byte limit darwin puts on a
// socket path.
func socket(t *testing.T) (string, *net.UnixListener) {
	t.Helper()
	dir, err := os.MkdirTemp("", "relay") //nolint:usetesting // t.TempDir is named for the test, past a socket path's bound
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "s.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return path, ln
}

// THE ENGINE CLOSING ITS END ENDS THE RELAY, with what it sent delivered,
// whatever the harness is still doing with stdin — nothing it writes after that can
// reach a tool.
//
// The harness's stdin here never ends (a pipe nobody closes), so a relay that waited
// for it would hang the case.
//
// Mutation: wait on the stdin copy instead of the socket copy and the case never
// returns.
func TestTheEngineClosingTheSocketEndsTheRelay(t *testing.T) {
	t.Parallel()
	path, ln := socket(t)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = conn.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}` + "\n"))
		_ = conn.Close()
	}()
	stdin, keepOpen := io.Pipe()
	defer keepOpen.Close()

	var stdout, stderr bytes.Buffer
	if err := Run([]string{path}, stdin, &stdout, &stderr); err != nil {
		t.Fatalf("Run: %v (stderr %q)", err, stderr.String())
	}
	if got := stdout.String(); got != `{"jsonrpc":"2.0","id":1,"result":{}}`+"\n" {
		t.Errorf("stdout = %q, want the engine's line exactly", got)
	}
	if stderr.Len() != 0 {
		t.Errorf("the relay wrote to stderr on a clean run: %q", stderr.String())
	}
}

// THE HARNESS FINISHING HALF-CLOSES, so the answer to a call it already sent still
// reaches it: the engine reads the end of the stream, answers, and closes.
//
// Mutation: close the whole socket when stdin ends and the engine's answer is
// lost — stdout comes back empty.
func TestTheHarnessFinishingStillGetsTheAnswerToItsLastCall(t *testing.T) {
	t.Parallel()
	path, ln := socket(t)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// Everything the harness sent, up to its end, then the answer.
		got, _ := io.ReadAll(conn)
		_, _ = conn.Write([]byte("answer to " + strings.TrimSpace(string(got)) + "\n"))
	}()

	var stdout, stderr bytes.Buffer
	if err := Run([]string{path}, strings.NewReader("call-7\n"), &stdout, &stderr); err != nil {
		t.Fatalf("Run: %v (stderr %q)", err, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "answer to call-7" {
		t.Errorf("stdout = %q, want the answer to the harness's last call", got)
	}
}

// BYTES CROSS UNCHANGED BOTH WAYS, line by line, for as long as both ends are
// open: the relay is a pipe and has no idea what MCP is.
func TestTheRelayCopiesBothWays(t *testing.T) {
	t.Parallel()
	path, ln := socket(t)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		// An echo with a prefix, line by line, until the harness's end.
		lines := bufio.NewScanner(conn)
		for lines.Scan() {
			_, _ = conn.Write([]byte("engine:" + lines.Text() + "\n"))
		}
	}()

	var stdout, stderr bytes.Buffer
	in := strings.NewReader("one\ntwo\nthree\n")
	if err := Run([]string{path}, in, &stdout, &stderr); err != nil {
		t.Fatalf("Run: %v (stderr %q)", err, stderr.String())
	}
	if got, want := stdout.String(), "engine:one\nengine:two\nengine:three\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

// A SOCKET NOBODY HOLDS IS SAID ON STDERR, naming it, and nothing reaches
// stdout — which is the MCP stream, where any byte is a protocol error the harness
// would report as a broken server rather than as a missing one.
func TestAnUnreachableSocketIsNamedOnStderrAlone(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "gone.sock")
	var stdout, stderr bytes.Buffer
	if err := Run([]string{path}, strings.NewReader(""), &stdout, &stderr); err == nil {
		t.Fatal("the relay reached a socket nobody listens on")
	}
	if stdout.Len() != 0 {
		t.Errorf("the relay wrote to the MCP stream on failure: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), path) {
		t.Errorf("stderr does not name the socket: %q", stderr.String())
	}
}

// ITS ONLY INPUT IS ONE ABSOLUTE PATH. A relative one would be resolved against
// whatever working directory the harness started its server in, which is the
// call's own directory on some harnesses and the harness's elsewhere.
func TestTheRelayTakesExactlyOneAbsolutePath(t *testing.T) {
	t.Parallel()
	for name, args := range map[string][]string{
		"no argument":    nil,
		"a relative one": {"s.sock"},
		"two":            {"/a.sock", "/b.sock"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			if err := Run(args, strings.NewReader(""), &stdout, &stderr); err == nil {
				t.Fatal("the relay accepted the arguments")
			}
			if stdout.Len() != 0 {
				t.Errorf("the relay wrote to the MCP stream: %q", stdout.String())
			}
			if !strings.Contains(stderr.String(), "usage: crewlet "+Command) {
				t.Errorf("stderr does not give the usage: %q", stderr.String())
			}
		})
	}
}
