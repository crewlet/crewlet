package host

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/events/types"
)

// lockedBuffer is a log a case reads back while the relay writes it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// relayHost is a host with nothing but its relay running — what the hook's
// side meets — under faults, logging to log.
func relayHost(t *testing.T, faults codefault.Injector, log io.Writer) *Server {
	t.Helper()
	s := &Server{o: Options{Timing: Production, Faults: faults}, now: time.Now,
		log: slog.New(slog.NewJSONHandler(log, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	s.life, s.end = context.WithCancel(context.Background())
	r, err := listen(s)
	if err != nil {
		t.Fatal(err)
	}
	s.relay = r
	go r.serve(s.life)
	t.Cleanup(func() {
		s.end()
		r.shutdown(context.Background())
	})
	return s
}

// openSession registers a session on s's relay, as a receive does before it
// starts receive-pack.
func openSession(t *testing.T, s *Server) *session {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sess := newSession(ctx, cancel)
	s.relay.open(sess)
	return sess
}

// hookSaid is what a hook connection was told: its stderr and its exit
// status (-1 when the connection ended with none).
func hookSaid(t *testing.T, s *Server, handshake, rest string) (string, int) {
	t.Helper()
	conn, err := net.Dial("unix", s.relay.path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(conn, handshake+rest); err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	for {
		ch, payload, err := readFrame(conn)
		if err != nil {
			return stderr.String(), -1
		}
		switch ch {
		case frameStderr:
			stderr.Write(payload)
		case frameExit:
			return stderr.String(), int(payload[0])
		}
	}
}

// waitDone waits for a claimed session's connection to end.
func waitDone(t *testing.T, sess *session) {
	t.Helper()
	select {
	case <-sess.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the session's connection never ended")
	}
}

// THE HOOK TAKES NOTHING BUT THE PROTOCOL: a connection that names a live
// session and then writes anything but git's version block is a protocol
// violation; a token is spent by the first connection that names it; and a
// token the relay never issued is refused.
func TestTheHookTakesNothingButTheProtocol(t *testing.T) {
	var log lockedBuffer
	s := relayHost(t, nil, &log)
	sess := openSession(t, s)

	stderr, status := hookSaid(t, s, hookHandshake+sess.token+"\n", string(pktString("repo=other\n"))+flushPkt)
	if status != 1 || !strings.Contains(stderr, "did not speak version 1") {
		t.Errorf("a forged line where the version is due: exit %d, %q", status, stderr)
	}
	waitDone(t, sess)
	if got := sess.outcome().outcome; got != types.AuditFailed {
		t.Errorf("the session ended %q, want failed", got)
	}
	if !strings.Contains(log.String(), `"msg":"code_hook_protocol"`) {
		t.Errorf("the violation was not logged code_hook_protocol: %s", log.String())
	}

	for name, handshake := range map[string]string{
		"the same token again":    hookHandshake + sess.token + "\n",
		"a token never issued":    hookHandshake + strings.Repeat("ab", 32) + "\n",
		"a line that is no token": "GET / HTTP/1.1\n",
	} {
		stderr, status := hookSaid(t, s, handshake, "")
		if status != 1 || !strings.Contains(stderr, "this hook was not started by the engine") {
			t.Errorf("%s: exit %d, %q", name, status, stderr)
		}
	}
}

// panicking is an injector whose every hit panics: a bug in the session's
// code, reached on the relay's own goroutine.
type panicking struct{}

func (panicking) Hit(context.Context, codefault.Point, int64) error { panic("a bug in the session") }

// O14: A PANIC ON A RELAY CONNECTION IS THAT PUSH'S REFUSAL, never the
// process's end: the relay runs outside net/http's per-handler recovery, and
// what it reads is bytes a pusher controls. The hook is told, the session
// recorded failed, the stack logged — and the relay serves the next push.
func TestARelayPanicIsThatPushsRefusal(t *testing.T) {
	var log lockedBuffer
	s := relayHost(t, panicking{}, &log)
	sess := openSession(t, s)
	stderr, status := hookSaid(t, s, hookHandshake+sess.token+"\n", "")
	if status != 1 || !strings.Contains(stderr, "the engine failed while serving this push") {
		t.Errorf("a session that panicked: exit %d, %q", status, stderr)
	}
	waitDone(t, sess)
	if got := sess.outcome().outcome; got != types.AuditFailed {
		t.Errorf("the session ended %q, want failed", got)
	}
	if !strings.Contains(log.String(), `"msg":"code_hook_panic"`) || !strings.Contains(log.String(), "a bug in the session") {
		t.Errorf("the panic was not logged with its value: %s", log.String())
	}
	if _, status := hookSaid(t, s, hookHandshake+strings.Repeat("cd", 32)+"\n", ""); status != 1 {
		t.Errorf("the relay after a panic answered the next connection exit %d", status)
	}
}

// A socket path past what every platform's sockaddr holds is refused at
// Start, naming the variable that chose it.
func TestTheSocketPathFitsEveryPlatform(t *testing.T) {
	base := t.TempDir()
	long := filepath.Join(base, strings.Repeat("d", max(1, 120-len(base)-1)))
	if err := os.MkdirAll(long, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", long)
	s := &Server{o: Options{Timing: Production}, now: time.Now, log: slog.New(slog.DiscardHandler)}
	s.life, s.end = context.WithCancel(context.Background())
	defer s.end()
	err := s.Start(t.Context())
	if err == nil || !strings.Contains(err.Error(), "TMPDIR") {
		t.Errorf("Start under a %d-byte TMPDIR = %v, want a refusal naming TMPDIR", len(long), err)
	}
}

// readerOf is a bufio.Reader over raw, for the fuzz targets.
func readerOf(raw []byte) *bufio.Reader { return bufio.NewReader(bytes.NewReader(raw)) }
