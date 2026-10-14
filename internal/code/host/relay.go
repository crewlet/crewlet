package host

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// THE HOOK RELAY, the engine's side: a unix socket in a private directory,
// where every push's proc-receive hook — the engine's own binary, started by
// receive-pack — names its session by a one-time token and is then served the
// push's verdict.
//
// THE RELAY CANNOT END THE PROCESS. Its accept loop and its connections run
// outside net/http's per-handler recovery, and what they read — the handshake,
// git's proc-receive protocol — is bytes a pusher controls; so each
// connection's goroutine turns a panic into that push's refusal: an error to
// its hook, the stack logged once, the engine still running.

// hookHandshake is the line a hook connection opens with, before its token.
const hookHandshake = "CREWLET-HOOK 1 "

// The frame channels, engine to hook: the hook's stdout (git's proc-receive
// protocol), its stderr (the pushing client sees it as `remote: …`), and its
// exit status (one byte, the last frame).
const (
	frameExit   byte = 0
	frameStdout byte = 1
	frameStderr byte = 2
)

// frameHeader is a frame's channel byte and its payload's uint32 big-endian
// length.
const frameHeader = 5

// relay is one host's hook socket and the sessions waiting on it.
type relay struct {
	s    *Server
	ln   *net.UnixListener
	dir  string
	path string

	mu       sync.Mutex
	sessions map[string]*session // by token, until consumed or closed
	conns    sync.WaitGroup
}

// listen makes the relay's private directory and its socket. The socket's path
// must fit every platform's sockaddr — TMPDIR names where it is made, so a
// path past maxSocketPath is refused naming it.
func listen(s *Server) (*relay, error) {
	dir, err := os.MkdirTemp("", "crewlet-git-")
	if err != nil {
		return nil, fmt.Errorf("host: make the hook relay's directory: %w (TMPDIR must be a writable "+
			"directory)", err)
	}
	path := filepath.Join(dir, "h")
	if len(path) > maxSocketPath {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("host: the hook relay's socket path %s is over %d bytes (unix sockets take "+
			"104 on darwin, 108 on Linux): set TMPDIR to a shorter directory", path, maxSocketPath)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("host: listen on the hook relay's socket %s: %w", path, err)
	}
	return &relay{s: s, ln: ln, dir: dir, path: path, sessions: map[string]*session{}}, nil
}

// serve accepts hook connections until the listener closes.
func (r *relay) serve(life context.Context) {
	for {
		conn, err := r.ln.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			r.s.log.Warn("code_hook_accept_failed", "error", err)
			select {
			case <-life.Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
			continue
		}
		r.conns.Add(1)
		go r.handle(conn)
	}
}

// open registers a session under a fresh token: 32 bytes from crypto/rand,
// so a token is never guessed and never reused.
func (r *relay) open(sess *session) {
	var raw [32]byte
	_, _ = rand.Read(raw[:])
	sess.token = hex.EncodeToString(raw[:])
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessions[sess.token] = sess
}

// close unregisters a session whatever became of it — a token never presented
// is gone with its request — and, if its hook claimed it, ends the session's
// work and waits for its goroutine, so nothing outlives the request; it
// reports whether a hook claimed it.
//
// One lock decides it against claim: either close removes the token first and
// no connection can claim it after, or a connection claimed it first and close
// waits for that connection's session to end.
func (r *relay) close(sess *session) bool {
	r.mu.Lock()
	delete(r.sessions, sess.token)
	claimed := sess.claimed
	r.mu.Unlock()
	sess.cancel()
	if claimed {
		<-sess.done
	}
	return claimed
}

// claim consumes a session's token: a second connection naming it finds
// nothing.
func (r *relay) claim(token string) *session {
	r.mu.Lock()
	defer r.mu.Unlock()
	sess := r.sessions[token]
	if sess != nil {
		delete(r.sessions, token)
		sess.claimed = true
	}
	return sess
}

// shutdown closes the listener, waits for the connections (or ctx), and
// removes the directory.
func (r *relay) shutdown(ctx context.Context) {
	_ = r.ln.Close()
	done := make(chan struct{})
	go func() {
		r.conns.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
	_ = os.RemoveAll(r.dir)
}

// handle serves one hook connection: its handshake, then its session.
func (r *relay) handle(conn *net.UnixConn) {
	defer r.conns.Done()
	defer func() { _ = conn.Close() }()
	fw := &frameWriter{w: conn}
	br := bufio.NewReaderSize(conn, 64<<10)
	_ = conn.SetReadDeadline(time.Now().Add(r.s.o.Timing.Handshake))
	token, err := readHandshake(br)
	if err != nil {
		r.refuse(fw, "this hook was not started by the engine", err)
		return
	}
	sess := r.claim(token)
	if sess == nil {
		r.refuse(fw, "this hook was not started by the engine", errors.New("unknown or spent token"))
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	// DONE CLOSES LAST, after the recovery below has recorded the push as
	// failed: the request waits on it for the session's result.
	defer close(sess.done)
	defer func() {
		if p := recover(); p != nil {
			r.s.log.Error("code_hook_panic", "repo", sess.repo.Name, "push_id", sess.pushID,
				"panic", fmt.Sprint(p), "stack", string(debug.Stack()))
			sess.fail("the engine failed while serving this push's hook")
			_ = fw.write(frameStderr, []byte("crewlet: the engine failed while serving this push; nothing "+
				"was committed by this hook — re-run git push\n"))
			_ = fw.write(frameExit, []byte{1})
		}
	}()
	r.s.serveSession(sess, br, fw)
}

// refuse ends a connection that is not a session's hook: a sentence on its
// stderr, exit 1, and a warning.
func (r *relay) refuse(fw *frameWriter, words string, cause error) {
	r.s.log.Warn("code_hook_refused", "reason", words, "error", cause)
	_ = fw.write(frameStderr, []byte("crewlet: "+words+"\n"))
	_ = fw.write(frameExit, []byte{1})
}

// errHandshake is a connection's first line that is not a hook's.
var errHandshake = errors.New("host: not a hook's handshake")

// readHandshake reads `CREWLET-HOOK 1 <token>\n`, at most maxHandshake bytes,
// and answers the token: 64 lowercase hex digits.
func readHandshake(br *bufio.Reader) (string, error) {
	var line []byte
	for len(line) <= maxHandshake {
		b, err := br.ReadByte()
		if err != nil {
			return "", fmt.Errorf("%w: %w", errHandshake, err)
		}
		if b == '\n' {
			token, ok := strings.CutPrefix(string(line), hookHandshake)
			if !ok || len(token) != 64 || strings.Trim(token, "0123456789abcdef") != "" {
				return "", fmt.Errorf("%w: %q", errHandshake, line)
			}
			return token, nil
		}
		line = append(line, b)
	}
	return "", fmt.Errorf("%w: a first line past %d bytes", errHandshake, maxHandshake)
}

// frameWriter serialises one connection's frames — the session's and the
// keepalive's — under a mutex.
type frameWriter struct {
	mu sync.Mutex
	w  io.Writer
}

// write sends payload on ch, in frames of at most hookFrameMax bytes.
func (f *frameWriter) write(ch byte, payload []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for first := true; first || len(payload) > 0; first = false {
		n := min(len(payload), hookFrameMax)
		var head [frameHeader]byte
		head[0] = ch
		binary.BigEndian.PutUint32(head[1:], uint32(n))
		if _, err := f.w.Write(append(head[:], payload[:n]...)); err != nil {
			return err
		}
		payload = payload[n:]
	}
	return nil
}

// errFrame is bytes that are not a relay frame.
var errFrame = errors.New("host: not a relay frame")

// readFrame reads one frame: a channel this relay sends on and a payload of at
// most hookFrameMax bytes, the exit frame's exactly one. io.EOF only before a
// frame's first byte.
func readFrame(r io.Reader) (ch byte, payload []byte, err error) {
	var head [frameHeader]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return 0, nil, err
	}
	ch = head[0]
	n := binary.BigEndian.Uint32(head[1:])
	switch {
	case ch != frameExit && ch != frameStdout && ch != frameStderr:
		return 0, nil, fmt.Errorf("%w: channel %d", errFrame, ch)
	case n > hookFrameMax:
		return 0, nil, fmt.Errorf("%w: %d bytes, past %d", errFrame, n, hookFrameMax)
	case ch == frameExit && n != 1:
		return 0, nil, fmt.Errorf("%w: an exit frame of %d bytes", errFrame, n)
	}
	payload = make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return 0, nil, err
	}
	return ch, payload, nil
}
