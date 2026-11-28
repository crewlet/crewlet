package executor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/crewlet/crewlet/internal/activityline"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// streamRead is a run's event stream read from its open, no further than the
// size it was weighed at: what the open found of a stream its job's writers
// may still add to, since a job ends when its harness exits and a child it
// left can go on writing. Nothing but the read's end or its context's ends
// it, and a context's end is an error, never the stream's end, so no short
// read is ever taken for the stream.
type streamRead struct {
	ctx        context.Context
	body       io.Reader
	src        io.Reader
	size, read int64
	// change is what became of the file once the read reached its end
	// ([sandbox.ChangeOf]), short of its size a change.
	change sandbox.FileChange
}

// readStream is p read as a [streamRead] under ctx.
func readStream(ctx context.Context, p *piece) *streamRead {
	return &streamRead{ctx: ctx, body: p.body, src: io.LimitReader(p.body, p.size), size: p.size}
}

func (s *streamRead) Read(p []byte) (int, error) {
	if s.change != "" {
		return 0, io.EOF
	}
	if err := s.ctx.Err(); err != nil {
		// Wrapped where the read's error is ([Runner.decodeLines]).
		return 0, err
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
// it, so its last line, unended, is a line: not still being written.
func (s *streamRead) whole() bool { return s.change == sandbox.FileUnchanged }

// unendedNote says a transcript is of a stream still being written when its
// reading ended, cut where its redaction had settled.
func unendedNote() string {
	return activityline.Words(activityline.TagNote, "the run's event stream was still being written when the "+
		"collection read it: its transcript ends where the stream's redaction had settled")
}

// eachLineOf is [eachLine] of a stream that may not have ended: its unended
// last line is a line only where whole says the read reached its end. Where
// last is set, it marks the stream's last non-blank line handed on.
func eachLineOf(r io.Reader, dec Decoder, bound int, whole func() bool, last *streamLast) error {
	l := streamLines{bound: bound, ends: true}
	line := func(line []byte) error {
		if last != nil && len(bytes.TrimSpace(line)) > 0 {
			// The line is what of held its break left, from held's start.
			last.mark(l.taken-int64(len(l.held)), line)
		}
		dec.Line(line)
		return nil
	}
	skipped := func(n int64, head, tail []byte) error {
		if last != nil {
			last.skip(n, bound)
		}
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
