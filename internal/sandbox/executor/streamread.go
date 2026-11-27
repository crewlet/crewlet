package executor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/crewlet/crewlet/internal/activityline"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// streamRead is a run's event stream read from its open, no further than the
// size it was weighed at and no later than by on the runner's clock: what the
// open found of a stream its job's writers may still add to, since a job ends
// when its harness exits and a child it left can go on writing.
type streamRead struct {
	body       io.Reader
	src        io.Reader
	size, read int64
	now        func() time.Time
	by         time.Time
	done       <-chan struct{}
	// past is a read its bound ended; change what became of the file once
	// the read reached its size ([sandbox.ChangeOf]), short of it a change.
	past   bool
	began  time.Time
	change sandbox.FileChange
}

// readStream is p read as a [streamRead] under ctx by by, zero for no bound
// but ctx's own end.
func (r *Runner) readStream(ctx context.Context, p *piece, by time.Time) *streamRead {
	if d, ok := ctx.Deadline(); ok && (by.IsZero() || d.Before(by)) {
		by = d
	}
	return &streamRead{body: p.body, src: io.LimitReader(p.body, p.size), size: p.size, now: r.now, by: by,
		done: ctx.Done(), began: r.now()}
}

func (s *streamRead) Read(p []byte) (int, error) {
	if s.past || s.change != "" {
		return 0, io.EOF
	}
	select {
	case <-s.done:
		s.past = true
		return 0, io.EOF
	default:
	}
	if !s.by.IsZero() && s.now().After(s.by) {
		s.past = true
		return 0, io.EOF
	}
	n, err := s.src.Read(p)
	s.read += int64(n)
	if errors.Is(err, io.EOF) {
		change, cerr := sandbox.ChangeOf(s.body)
		if cerr != nil {
			return n, fmt.Errorf("executor: telling whether the event stream changed while it was read: %w", cerr)
		}
		if s.read < s.size {
			change = sandbox.FileChanged
		}
		s.change = change
	}
	return n, err
}

// whole reports whether the read reached the stream's end as one version of
// it, so its last line, unended, is a line: neither cut by its bound nor
// still being written.
func (s *streamRead) whole() bool { return !s.past && s.change == sandbox.FileUnchanged }

// pastNote is what stands for a transcript whose stream's read its bound
// ended: how long the read had, how much it read and of what.
func (s *streamRead) pastNote() string {
	return activityline.Words(activityline.TagNote, fmt.Sprintf("the run's transcript is not shown: the "+
		"collection's read of its event stream ran past the %v it had, after %s of its %s",
		max(s.by.Sub(s.began), 0).Round(time.Second), humanSize(s.read), humanSize(s.size)))
}

// pastError is why a run whose stream's read its bound ended said nothing of
// how it ended, where its result is its stream's.
func (s *streamRead) pastError() string {
	return fmt.Sprintf("the collection's read of the run's event stream ran past the %v it had, after %s "+
		"of its %s, before the stream said how the run ended",
		max(s.by.Sub(s.began), 0).Round(time.Second), humanSize(s.read), humanSize(s.size))
}

// unendedNote says a transcript is of a stream still being written when its
// reading ended, cut where its redaction had settled.
func unendedNote() string {
	return activityline.Words(activityline.TagNote, "the run's event stream was still being written when the "+
		"collection read it: its transcript ends where the stream's redaction had settled")
}

// eachLineOf is [eachLine] of a stream that may not have ended: its unended
// last line is a line only where whole says the read reached its end.
func eachLineOf(r io.Reader, dec Decoder, bound int, whole func() bool) error {
	l := streamLines{bound: bound, ends: true}
	line := func(line []byte) error {
		dec.Line(line)
		return nil
	}
	skipped := func(n int64, head, tail []byte) error {
		dec.Skipped(LongLine{Size: n, Bound: bound, Head: head, Tail: tail})
		return nil
	}
	if err := l.read(bufio.NewReaderSize(r, lineChunkBytes), false, line, skipped); err != nil {
		return err
	}
	if !whole() {
		return nil
	}
	return l.read(bufio.NewReader(eofReader{}), true, line, skipped)
}

// eofReader is a reader at its end.
type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, io.EOF }

// streamUnended is a decoder whose transcript a stream still being written
// ends ([transcriptLines.unended]): every runner's event decoder, as a test
// holds.
type streamUnended interface {
	streamUnended()
}

func (d *codexEvents) streamUnended()    { d.transcript.unended = true }
func (d *claudeEvents) streamUnended()   { d.transcript.unended = true }
func (d *geminiEvents) streamUnended()   { d.transcript.unended = true }
func (d *grokEvents) streamUnended()     { d.transcript.unended = true }
func (d *cursorEvents) streamUnended()   { d.transcript.unended = true }
func (d *copilotEvents) streamUnended()  { d.transcript.unended = true }
func (d *kimiEvents) streamUnended()     { d.transcript.unended = true }
func (d *museEvents) streamUnended()     { d.transcript.unended = true }
func (d *hermesEvents) streamUnended()   { d.transcript.unended = true }
func (d *piEvents) streamUnended()       { d.transcript.unended = true }
func (d *qwenEvents) streamUnended()     { d.transcript.unended = true }
func (d *openCodeEvents) streamUnended() { d.transcript.unended = true }
