package executor

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// failureReadPiece is how much of the error stream one read hands its
// redaction: redact's own piece (pieceBytes, 32 KiB), so the clock is read
// once a piece and a box that trickles is stopped within one.
const failureReadPiece = 32 << 10

// failureTail is what a run's failure and transcript show of its error stream:
// the end of the WHOLE stream's redaction, read from its first byte through one
// [redact.Stream], at most a bound of whole lines ([sandbox.KeepEnd]); or, where
// that redaction did not end within [sandbox.FailureReadWait] or the stream
// declined the text ([redact.ErrDecidedEarly]), none of it, and why.
type failureTail struct {
	// shown is the end; before how many redacted bytes came before it.
	shown  string
	before int64
	// size is the stream's size at the collection's open.
	size int64
	// withheld is why none is shown where the read could not show any.
	withheld string
}

// note is the line that stands for a stream of which nothing is shown, its
// size and why: withheld, or an end of blank lines alone after text it leaves
// out. "" where something is shown, or the stream is blank and whole.
func (f failureTail) note() string {
	why := f.withheld
	if why == "" && f.before > 0 && strings.TrimSpace(f.shown) == "" {
		why = "the end a run's failure shows of it holds only blank lines"
	}
	if why == "" {
		return ""
	}
	return fmt.Sprintf("(the error stream, %s, is not shown: %s)", humanSize(f.size), why)
}

// transcript is the tail as a transcript with no events shows it: the end,
// after the note naming the start it leaves out, or the note alone.
func (f failureTail) transcript() string {
	shown := strings.TrimSpace(f.shown)
	switch {
	case f.withheld != "" || shown == "":
		return f.note()
	case f.before > 0:
		return unreadNote(f.before, false) + "\n" + shown
	}
	return shown
}

// readFailureTail reads the error stream p from its first byte, no further
// than the size it was weighed at, through one redaction stream, keeping the
// last keep+1 bytes of what it writes ([endRing]): with the byte before them,
// [sandbox.KeepEnd] of those is KeepEnd of the whole redaction. ON THE
// RUNNER'S CLOCK: past [sandbox.FailureReadWait] the read stops and withholds
// the end, since an end the redaction has not reached is not the stream's.
func (r *Runner) readFailureTail(p *piece, keep int) (failureTail, error) {
	tail := failureTail{size: p.size}
	if p.body == nil || p.size == 0 {
		return tail, nil
	}
	defer p.close()
	end := &endRing{keep: keep + 1}
	s := redact.NewStream(end)
	src := io.LimitReader(p.body, p.size)
	buf := make([]byte, failureReadPiece)
	began := r.now()
	for {
		if r.now().Sub(began) > sandbox.FailureReadWait {
			tail.withheld = fmt.Sprintf("its redaction, read from its first byte, did not reach its end within "+
				"the %v a collection gives it, and an end the redaction has not reached may still be changed "+
				"by what comes before it", sandbox.FailureReadWait)
			return tail, nil
		}
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := s.Write(buf[:n]); werr != nil {
				return declinedTail(tail, werr)
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return failureTail{}, fmt.Errorf("executor: reading the error stream: %w", err)
		}
	}
	if err := s.Close(); err != nil {
		return declinedTail(tail, err)
	}
	shown, at := sandbox.KeepEnd(end.String(), keep)
	tail.shown, tail.before = shown, end.dropped()+int64(at)
	return tail, nil
}

// declinedTail is tail withheld because its redaction stream declined the text
// ([redact.ErrDecidedEarly]): what it wrote may hold what redaction takes out.
// The ring never fails a write, so any other error is the stream's own fault.
func declinedTail(tail failureTail, err error) (failureTail, error) {
	if !errors.Is(err, redact.ErrDecidedEarly) {
		return failureTail{}, fmt.Errorf("executor: redacting the error stream: %w", err)
	}
	tail.withheld = "its redaction declined it (a key's block read beside a password's key it could " +
		"not decide in one pass), and a text the redaction declines is never shown"
	return tail, nil
}

// endRing is the last keep bytes written to it, in a buffer of at most keep
// bytes however much is written, and how much was written in all.
type endRing struct {
	keep, at int
	buf      []byte
	total    int64
}

// Write keeps p's end; it never fails.
func (e *endRing) Write(p []byte) (int, error) {
	e.total += int64(len(p))
	q := p
	if len(q) > e.keep {
		q = q[len(q)-e.keep:]
	}
	if room := e.keep - len(e.buf); room > 0 {
		take := min(room, len(q))
		if len(e.buf)+take > cap(e.buf) {
			// Grown by hand, never past keep: append's doubling would hold
			// up to twice the bound the collection weighs it at.
			grown := make([]byte, len(e.buf), min(e.keep, max(2*cap(e.buf), len(e.buf)+take)))
			e.buf = grown[:copy(grown, e.buf)]
		}
		e.buf = append(e.buf, q[:take]...)
		q = q[take:]
	}
	for len(q) > 0 {
		k := copy(e.buf[e.at:], q)
		q, e.at = q[k:], (e.at+k)%e.keep
	}
	return len(p), nil
}

// String is what the ring holds, oldest first.
func (e *endRing) String() string {
	return string(e.buf[e.at:]) + string(e.buf[:e.at])
}

// dropped is how many bytes were written before what the ring holds.
func (e *endRing) dropped() int64 { return e.total - int64(len(e.buf)) }
