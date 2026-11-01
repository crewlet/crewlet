package toolsession

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/crewlet/crewlet/internal/mcprelay"
)

// Host is this process's place for tool sessions: one directory only the
// engine's user can open, and the executable a harness is told to start as the
// relay into it.
//
// ONE PER PROCESS, made by whoever owns the process's lifetime (the engine at
// boot, a command for its own run) and closed by it, which removes the
// directory. A package-level directory created on first use would have no owner
// to remove it, and a test binary would leave one behind per run.
type Host struct {
	dir   string
	relay string
}

// socketName is the length of a session's socket name: 16 hex characters, 64
// random bits — enough that no two runs of a process collide, and short,
// because a socket path is bounded and the directory around it is not ours to
// choose (see [NewHost]).
const socketNameBytes = 8

// NewHost makes this process's session directory.
//
// # Created, then checked
//
// os.MkdirTemp creates a fresh directory under the system's temp root with a
// random name, atomically and with mode 0700 — so another local user cannot
// have made it first and swapped a socket of their own in. It is then looked at
// rather than trusted: a directory, not a link, owned by this process's user,
// and open to nobody else. That directory is the whole of what admits a
// connection — a session's socket carries no token — so a directory that is
// not exactly that is refused, never used.
//
// # And sized
//
// A unix socket's path is bounded (104 bytes on darwin, 108 on linux, the
// terminating NUL included) and the temp root is whatever TMPDIR says — on
// darwin a per-user directory of about fifty characters already. A path past
// the bound fails at every bind, in every seat, with an error naming no
// setting; so it is checked here, once, and refused naming TMPDIR.
func NewHost() (*Host, error) {
	relay, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("toolsession: find this executable, which every harness "+
			"is told to start as the relay to its seat's tools: %w", err)
	}
	dir, err := os.MkdirTemp("", "crewlet-mcp-")
	if err != nil {
		return nil, fmt.Errorf("toolsession: make the directory a seat's tool sockets go in "+
			"(under TMPDIR, %s): %w", os.TempDir(), err)
	}
	if err := checkDir(dir); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	longest := filepath.Join(dir, socketName())
	if len(longest)+1 > maxSocketPath {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("toolsession: a socket under TMPDIR (%s) would be %d bytes "+
			"long, past the %d this platform allows a unix socket's path — set TMPDIR to a "+
			"shorter directory for this process", os.TempDir(), len(longest)+1, maxSocketPath)
	}
	return &Host{dir: dir, relay: relay}, nil
}

// checkDir verifies the session directory is what admits a connection: a real
// directory, this user's, open to nobody else.
func checkDir(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("toolsession: %w", err)
	}
	if info.Mode()&fs.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("toolsession: %s is not a directory of its own", dir)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		return fmt.Errorf("toolsession: %s is mode %v, and a seat's tool sockets must be "+
			"reachable by this user alone (0700)", dir, perm)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Getuid() {
		return fmt.Errorf("toolsession: %s is not owned by this process's user", dir)
	}
	return nil
}

// Dir is the directory, for a test and a log line.
func (h *Host) Dir() string { return h.dir }

// Close removes the directory and every socket left in it. Safe to call more
// than once and on nil.
func (h *Host) Close() error {
	if h == nil {
		return nil
	}
	return os.RemoveAll(h.dir)
}

// Session is one harness run's connection to a dispatcher: a socket of its own in
// the host's directory, served for as long as the run lasts.
//
// ONE PER RUN, never one per phase, so a run that is over cannot reach the next
// one's calls: a harness killed past its grace may leave a relay behind (Codex
// starts its servers in a process group of their own, which a signal to the
// harness's group does not reach), and that relay's socket is gone.
type Session struct {
	path   string
	server *Server
	ln     *net.UnixListener
	ctx    context.Context

	mu       sync.Mutex
	sessions []*mcp.ServerSession
	retired  bool

	wg sync.WaitGroup
}

// Open starts serving d on a fresh socket.
//
// ctx is the connection's own, with its cancellation dropped: ending a session
// is [Session.Retire], never a cancellation. No call runs under it — d runs
// each under the context of the run it serves ([Dispatcher.Call]).
//
// name is how the harness lists the server, and title is shown in its handshake.
func (h *Host) Open(ctx context.Context, name, title string, d Dispatcher) (*Session, error) {
	path := filepath.Join(h.dir, socketName())
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("toolsession: listen on %s: %w", path, err)
	}
	// The listener removes its socket when it is closed.
	ln.SetUnlinkOnClose(true)
	s := &Session{
		path:   path,
		server: NewServer(name, title, d),
		ln:     ln,
		ctx:    context.WithoutCancel(ctx),
	}
	s.wg.Add(1)
	go s.accept()
	return s, nil
}

// Command is the stdio server a harness is told to start for this session: this
// executable as the relay, and the socket. Not a secret — see [NewHost].
func (h *Host) Command(s *Session) (string, []string) {
	return h.relay, []string{mcprelay.Command, s.path}
}

// Path is the session's socket, for a test and a log line.
func (s *Session) Path() string { return s.path }

// Server is the session's MCP server.
func (s *Session) Server() *Server { return s.server }

// Connected reports whether a harness reached this session far enough to list the
// seat's tools — see [Server.Listed].
func (s *Session) Connected() bool {
	select {
	case <-s.server.Listed():
		return true
	default:
		return false
	}
}

// accept serves every connection made to the socket until it is retired. One
// is the norm — the harness's relay — and more are served alike rather than
// refused, because a harness that restarts a failed server reconnects.
func (s *Session) accept() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			// Closed by Retire, which is the only way this loop ends; any
			// other accept error on a unix listener leaves it unusable too.
			return
		}
		ss, err := s.server.MCP().Connect(s.ctx, &mcp.IOTransport{Reader: conn, Writer: conn}, nil)
		if err != nil {
			_ = conn.Close()
			continue
		}
		s.mu.Lock()
		if s.retired {
			s.mu.Unlock()
			_ = ss.Close()
			continue
		}
		s.sessions = append(s.sessions, ss)
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			_ = ss.Wait()
			_ = conn.Close()
		}()
	}
}

// Retire ends the session: no new connection is accepted, every connection is
// closed once the calls running on it have returned, and the socket is gone.
// It returns when all of that has happened. Safe to call more than once.
//
// THE CALLS ARE WAITED FOR, not cut off: a call is a tool that may be writing,
// and the dispatcher runs it under the phase's context, which a harness going away
// does not cancel. What bounds the wait is each call's own deadline, which the
// dispatcher sets.
func (s *Session) Retire() {
	s.mu.Lock()
	if s.retired {
		s.mu.Unlock()
		s.wg.Wait()
		return
	}
	s.retired = true
	sessions := s.sessions
	s.sessions = nil
	s.mu.Unlock()

	_ = s.ln.Close()
	for _, ss := range sessions {
		_ = ss.Close()
	}
	s.wg.Wait()
	// The listener unlinks its socket on close; one it could not unlink is
	// removed here, so a retired session leaves nothing a relay could reach.
	// Already gone is the ordinary answer, and there is nobody to tell about
	// any other: the directory goes with the host.
	_ = os.Remove(s.path)
}

// socketName is a fresh socket's name.
func socketName() string {
	var b [socketNameBytes]byte
	// crypto/rand.Read never fails on the platforms this builds for.
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:]) + ".sock"
}
