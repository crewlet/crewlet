package harness

import (
	"strings"
	"testing"
)

// EVERY LINE IS HANDED ON, AND ONE LINE IS BOUNDED. The capture is the buffer
// now, so the splitter holds only the line being assembled: a line longer
// than maxOutput is skipped to its end rather than held, and the lines after
// it still arrive — a run's record is read past maxOutput as a pipe's was.
// The unterminated line written last is what the end's rule reads.
//
// Mutation: hold a long line whole, and the bound below is broken; stop
// handing lines on after a long one, and "late" never arrives.
func TestALineSplitterBoundsOneLineAndHandsOnTheRest(t *testing.T) {
	t.Parallel()
	var lines []string
	s := &lineSplitter{on: func(l string) { lines = append(lines, l) }}

	s.write([]byte("first\n"))
	chunk := []byte(strings.Repeat("x", 1<<20))
	for range maxOutput>>20 + 1 {
		s.write(chunk)
	}
	if len(s.buf) > maxOutput {
		t.Errorf("the splitter holds %d bytes of one line, past the %d-byte bound", len(s.buf), maxOutput)
	}
	if rest := s.rest(); len(rest) != 0 {
		t.Errorf("a line past the bound is offered as the last line (%d bytes)", len(rest))
	}
	s.write([]byte("\nlate\ntail"))
	if got := strings.Join(lines, "|"); got != "first|late" {
		t.Errorf("lines handed on = %q, want the short lines on either side of the long one", got)
	}
	if got := string(s.rest()); got != "tail" {
		t.Errorf("the unterminated last line = %q, want tail", got)
	}
}
