package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

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
// refused the text ([redact.ErrDecidedEarly]), none of it, and why.
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
// RUNNER'S CLOCK, BY ITS BOUND: [sandbox.FailureReadWait], or the earlier of
// readBy ([sandbox.RunHandle.FailureReadBy]) and ctx's deadline where they come
// sooner; past it the read stops and withholds the end, since an end the
// redaction has not reached is not the stream's.
func (r *Runner) readFailureTail(ctx context.Context, p *piece, keep int, readBy time.Time) (failureTail, error) {
	tail := failureTail{size: p.size}
	if p.body == nil || p.size == 0 {
		return tail, nil
	}
	defer p.close()
	end := &endRing{keep: keep + 1}
	s := redact.NewStream(end)
	src := io.LimitReader(p.body, p.size)
	buf := make([]byte, failureReadPiece)
	began, read := r.now(), int64(0)
	by := began.Add(sandbox.FailureReadWait)
	for _, sooner := range []time.Time{readBy, deadlineOf(ctx)} {
		if !sooner.IsZero() && sooner.Before(by) {
			by = sooner
		}
	}
	for {
		if r.now().After(by) || ctx.Err() != nil {
			tail.withheld = fmt.Sprintf("it ran past the collection's bound on its read, %v, within which its "+
				"redaction read %s of it from its first byte and did not reach its end, so its end is withheld: "+
				"an end the redaction has not reached may still change with what came before it",
				max(by.Sub(began), 0).Round(time.Second), humanSize(read))
			p.withhold(sandbox.WithheldPastBound, "")
			return tail, nil
		}
		n, err := src.Read(buf)
		read += int64(n)
		if n > 0 {
			if _, werr := s.Write(buf[:n]); werr != nil {
				return declinedTail(p, tail, werr)
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
		return declinedTail(p, tail, err)
	}
	shown, at := sandbox.KeepEnd(end.String(), keep)
	tail.shown, tail.before = shown, end.dropped()+int64(at)
	return tail, nil
}

// deadlineOf is ctx's deadline, or zero where it carries none.
func deadlineOf(ctx context.Context) time.Time {
	d, _ := ctx.Deadline()
	return d
}

// declinedTail is tail withheld because its redaction stream refused the text
// ([redact.ErrDecidedEarly]): what it wrote may hold what redaction takes out.
// The ring never fails a write, so any other error is the stream's own fault.
func declinedTail(p *piece, tail failureTail, err error) (failureTail, error) {
	if !errors.Is(err, redact.ErrDecidedEarly) {
		return failureTail{}, fmt.Errorf("executor: redacting the error stream: %w", err)
	}
	tail.withheld = p.withhold(sandbox.WithheldRefused, redactionRefused)
	return tail, nil
}

// redactionRefused says why a text a redaction stream refused
// ([redact.ErrDecidedEarly]) is not shown, in its kind alone: never a word of
// the text, whose redaction is what could not be trusted.
const redactionRefused = "the redaction stream refused it: a stretch it decided early was contradicted " +
	"by a credential's claim it read later (a key's block beside a password's key), so what it wrote may " +
	"hold what redaction takes out, and none of it is shown"

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
