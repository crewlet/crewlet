package mcp

import (
	"bufio"
	"log/slog"
	"os"
	"sync"
	"time"
)

// maxStderrLine caps ONE retained stderr line. REASONED.
//
// A log line longer than 8 KiB is not a log line, and the tail exists to show
// a traceback, not to buffer a server's data dump. Without a cap this is an
// unbounded allocation driven by a third-party process: a server that writes a
// gigabyte with no newline is a gigabyte in the engine's heap. Over-long lines
// are truncated with a marker rather than dropped, so the fact that the server
// said something enormous survives.
const maxStderrLine = 8 << 10

// truncationMarker is appended to a line that hit maxStderrLine.
const truncationMarker = " …[truncated]"

// stderrRelay owns one stdio server's stderr.
//
// Two jobs, and the second is the reason it exists at all:
//
//   - ATTRIBUTION. The child's stderr is a real OS pipe rather than the
//     engine's own, because anything the server (or the package runner in
//     front of it — uvx, npx) prints would otherwise splat unattributed into
//     the engine's console, interleaving foreign log formats with the
//     structured stream. With a dozen per-role servers that is unreadable.
//   - LAST WORDS. A server that fails to start usually explains itself on
//     stderr and nowhere else: a bad token, a missing binary, an import error.
//     The handshake failure the engine sees says only "did not connect". So a
//     bounded tail is kept and surfaced with the failure.
//
// The write end is a *os.File on purpose. Handing exec.Cmd any other io.Writer
// makes it create its own pipe AND a copying goroutine that Wait blocks on —
// so a grandchild holding the descriptor would wedge the child's reaping
// inside the SDK. With a file, exec hands the descriptor straight to the child
// and this type owns both ends.
type stderrRelay struct {
	server string
	log    *slog.Logger

	w *os.File // handed to the child; also held by the parent until closeWriter
	r *os.File // the pump reads this

	done chan struct{} // closed when the pump has returned

	closeWriteOnce sync.Once
	closeReadOnce  sync.Once

	mu   sync.Mutex
	tail []string
}

func newStderrRelay(server string, log *slog.Logger) (*stderrRelay, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	s := &stderrRelay{server: server, log: log, r: r, w: w, done: make(chan struct{})}
	go s.pump()
	return s, nil
}

// writer is the descriptor handed to the child process.
func (s *stderrRelay) writer() *os.File { return s.w }

// closeWriter drops the PARENT's copy of the write end.
//
// It must happen after the child has been forked (the descriptor is dup'd into
// it at that point) and it must happen at all: while the parent holds a write
// end open, the pipe never reaches EOF even after every process has died, and
// the pump would block for ever waiting for words nobody can write.
func (s *stderrRelay) closeWriter() {
	s.closeWriteOnce.Do(func() { _ = s.w.Close() })
}

// pump turns each line into a debug event and keeps it in the bounded tail.
func (s *stderrRelay) pump() {
	defer close(s.done)
	// Through the same once as forceClose: the pump and a forced teardown can
	// both reach the read end, and closing an *os.File twice is an error the
	// second caller has no way to distinguish from a real one.
	defer s.forceClose()

	br := bufio.NewReaderSize(s.r, 64<<10)
	for {
		line, truncated, err := readBoundedLine(br)
		if line != "" {
			if truncated {
				line += truncationMarker
			}
			s.mu.Lock()
			s.tail = append(s.tail, line)
			if len(s.tail) > tailLines {
				s.tail = s.tail[len(s.tail)-tailLines:]
			}
			s.mu.Unlock()
			s.log.Debug("server_stderr", "server", s.server, "line", line)
		}
		if err != nil {
			return
		}
	}
}

// readBoundedLine reads one line, keeping at most maxStderrLine bytes of it
// and discarding the rest of an over-long line rather than buffering it.
func readBoundedLine(br *bufio.Reader) (line string, truncated bool, err error) {
	var buf []byte
	for {
		chunk, isPrefix, rerr := br.ReadLine()
		if n := maxStderrLine - len(buf); n > 0 {
			if len(chunk) > n {
				chunk, truncated = chunk[:n], true
			}
			buf = append(buf, chunk...)
		} else if len(chunk) > 0 {
			truncated = true
		}
		if rerr != nil {
			return string(buf), truncated, rerr
		}
		if !isPrefix {
			return string(buf), truncated, nil
		}
	}
}

// lines returns the last words the server wrote, oldest first.
func (s *stderrRelay) lines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.tail...)
}

// drained waits up to d for the pump to reach EOF, and reports whether it did.
//
// Not draining is EVIDENCE, not a nuisance: it means some process still holds
// the write end this relay handed to the child, which is the only signal
// available here that a descendant outlived the server.
func (s *stderrRelay) drained(d time.Duration) bool {
	if d <= 0 {
		select {
		case <-s.done:
			return true
		default:
			return false
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-s.done:
		return true
	case <-t.C:
		return false
	}
}

// forceClose ends the pump unconditionally by closing the read end under it.
//
// The last resort, and the reason this type leaks nothing: a goroutine blocked
// on a descriptor a stuck grandchild holds would otherwise live as long as the
// engine, one per server that ever failed that way.
func (s *stderrRelay) forceClose() {
	s.closeReadOnce.Do(func() { _ = s.r.Close() })
}
