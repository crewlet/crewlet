package mcp

import (
	"bufio"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/textcut"
)

// maxStderrLine caps ONE retained stderr line. REASONED.
//
// A log line longer than 8 KiB is not a log line, and the tail exists to show
// a traceback, not to buffer a server's data dump. Without a cap this is an
// unbounded allocation driven by a third-party process: a server that writes a
// gigabyte with no newline is a gigabyte in the engine's heap. Over-long lines
// are truncated with a marker rather than dropped, so the fact that the server
// said something enormous survives.
//
// It bounds the CONTENT, not the field: [truncationMarker] is appended outside
// it, which is why the cut is taken with [textcut.Bytes] (no marker of its own)
// rather than [textcut.Ellipsis] — the latter would mark it twice.
//
// WHERE THE REST OF THE LINE IS: NOWHERE, and this comment says so rather than
// pointing at a column that does not exist. The bytes past the cap are read off
// the pipe and DROPPED. Nothing else holds a copy — a stdio server's stderr is a
// private pipe this relay owns end to end (see [stderrRelay]), so there is no
// second reader, no store row and no event payload carrying the whole of it.
//
// WINDOWING IS REJECTED ON ITS COST, NOT FOR WANT OF A ROUTE. The obvious
// escape is real and cheap in memory: [stderrRelay.pump] already emits one
// server_stderr debug event per line, so an over-long line could emit each
// maxStderrLine-sized window as its own numbered event and never hold more
// than 8 KiB — which would put the whole value in the log an operator already
// reaches for with -debug. What refuses it is the arithmetic on the input this
// bound exists for, which is the pathological one: a gigabyte with no newline
// is 131,072 debug events, each through slog's handler and whatever a
// deployment ships its log to. The recovery route would BE the outage, and it
// would fire hardest exactly when a server is already misbehaving. So the
// engine bounds and marks instead, and an operator who needs the untruncated
// output runs that server's own command in a terminal, where its stderr is
// nobody's heap.
const maxStderrLine = 8 << 10

// truncationMarker says a value was shortened, so a reader never mistakes the
// remainder for the whole. Two sites append it: a stderr line that hit
// maxStderrLine, and the logged slice of an error body that hit
// maxLoggedErrorBody (see boundedBody in http.go). Both bound their CONTENT and
// carry this marker outside that bound.
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
	// dropped counts the lines the tail WINDOW pushed out — a second cut,
	// taken on the collection where maxStderrLine's is taken on one line, and
	// it needs its own count for the same reason that one needs its marker.
	// See [tailLines] for where those lines went.
	dropped int
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

	br := bufio.NewReaderSize(s.r, stderrReadBuffer)
	for {
		line, truncated, err := readBoundedLine(br)
		// MARKED BEFORE the emptiness check, not inside it. The rune-safe cut
		// can legitimately keep NOTHING — a line whose whole first
		// maxStderrLine bytes are continuation bytes, which is a server
		// dumping binary down stderr — and a reader shown nothing cannot tell
		// that from a server that said nothing at all. So the marker stands
		// alone, the way textcut.Within's doc argues for a budget too small to
		// hold its own marker. An ordinary blank line is still dropped: it
		// carries no truncation, so this adds nothing to it.
		if truncated {
			line += truncationMarker
		}
		if line != "" {
			s.mu.Lock()
			s.tail = append(s.tail, line)
			if n := len(s.tail) - tailLines; n > 0 {
				// ASKED, NOT INFERRED. A window that silently forgets its
				// older lines hands a reader exactly tailLines of them, and
				// fifty lines from a server that wrote fifty and fifty from
				// one that wrote five thousand are the same slice — while
				// being opposite diagnoses. [stderrRelay.lines] returns this
				// count beside the window so the difference is visible.
				s.dropped += n
				// Copied down rather than resliced forward. s.tail[n:] leaves
				// the dropped strings reachable from the backing array's
				// unused prefix, so a window sized to be held for every
				// spawned server at once would hold up to twice its own lines
				// — 8 KiB each — until an append happened to reallocate. The
				// source and destination overlap, which append handles: it
				// copies, and there is room for the result by construction.
				s.tail = append(s.tail[:0], s.tail[n:]...)
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
//
// THERE IS EXACTLY ONE CUT, it goes through [textcut.Bytes], and it is taken
// over the ASSEMBLED prefix rather than over a chunk bufio just handed back.
// All three clauses are load-bearing, and a bare buf[:n] gets each of them
// wrong:
//
//   - A plain byte cut splits whatever rune straddles the cap. This line goes
//     straight to slog and into the crash tail a failed start reports
//     (client.reportStartFailure, Bridge.StderrTail), so a server printing a
//     path with an accent in it ends its retained line in half a rune, which
//     slog's JSON handler rewrites to U+FFFD and a console prints as a box.
//   - The straddling rune can begin in the PREVIOUS chunk. bufio hands back a
//     fragment whenever its own buffer fills without a newline, and that
//     boundary knows nothing about runes either — so a chunk-local cut walks
//     back inside the chunk and leaves the earlier half-rune sitting at the end
//     of buf, untouched.
//   - Cutting as the chunks arrive cannot see a line that fills the cap
//     EXACTLY and then continues: the cut branch never fires, and the
//     truncation is discovered a chunk later with the partial rune already
//     committed. Deciding once, at the end, has no such ordering to get right.
//
// [textcut.Bytes] and not [textcut.Ellipsis]: the marker is appended by the
// pump, outside the budget, and marking here as well would mark it twice.
func readBoundedLine(br *bufio.Reader) (line string, truncated bool, err error) {
	// ONE BYTE PAST THE CAP is read on purpose — the idiom httpx.ReadBody uses
	// — and it does both jobs here. It tells a line sitting EXACTLY on the cap
	// from one that overflows it, so an exact fit is returned whole and
	// unmarked; and it is everything textcut.Bytes needs, since that reads the
	// byte at the budget and walks BACK from it, never forward. So the probe
	// byte is both the overflow signal and the cut's own input, which is why
	// truncated is derived below rather than tracked.
	const probe = maxStderrLine + 1
	var buf []byte
	for {
		chunk, isPrefix, rerr := br.ReadLine()
		if n := probe - len(buf); n > 0 {
			if len(chunk) > n {
				// NOT the cut: every byte dropped here sits at or past the
				// probe, beyond anything the rune-safe cut below can look at.
				// The rest of an over-long line is discarded rather than
				// buffered, which is the whole point of the bound — without it
				// a server that writes a gigabyte with no newline writes it
				// into this process's heap.
				chunk = chunk[:n]
			}
			buf = append(buf, chunk...)
		}
		if rerr == nil && isPrefix {
			continue
		}
		if len(buf) > maxStderrLine {
			return textcut.Bytes(string(buf), maxStderrLine), true, rerr
		}
		return string(buf), false, rerr
	}
}

// lines returns the last words the server wrote, oldest first, and how many
// earlier lines the window dropped to keep them.
//
// TWO VALUES BECAUSE THE WINDOW IS A CUT. The slice alone reads as everything
// the server said, which is false for any server that wrote more than
// [tailLines] lines; returning the count beside it makes a caller that does not
// want it write the _ that says so, rather than not being asked. Where the
// dropped lines are is stated at [tailLines].
func (s *stderrRelay) lines() (tail []string, dropped int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.tail...), s.dropped
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
