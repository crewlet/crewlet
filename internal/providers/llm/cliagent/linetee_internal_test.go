package cliagent

import (
	"strings"
	"testing"
)

// THE TEE IS BOUNDED BY THE SINK'S CAP. The sink drops past it and still
// reports every byte written, so a child printing one line with no newline in
// it grew the tee's pending line for as long as it printed — the very memory
// the cap exists to protect. Past the cap nothing more is forwarded: the
// answer it would stream is refused as clipped.
func TestTheLineTeeHoldsNoMoreThanTheCap(t *testing.T) {
	t.Parallel()
	var lines []string
	sink := &cappedBuffer{limit: 64}
	tee := &lineTee{sink: sink, on: func(l string) { lines = append(lines, l) }, limit: 64}

	_, _ = tee.Write([]byte("first\nsecond\n"))
	for range 100 {
		_, _ = tee.Write([]byte(strings.Repeat("x", 32)))
	}
	if len(tee.buf) > 64 {
		t.Errorf("the tee holds %d bytes of a line, past the %d-byte cap", len(tee.buf), 64)
	}
	_, _ = tee.Write([]byte("\nlate\n"))
	if strings.Join(lines, "|") != "first|second" {
		t.Errorf("lines forwarded = %q, want only those within the cap", lines)
	}
	if sink.Truncated() == 0 {
		t.Error("the case did not overrun the sink, so it tests nothing")
	}
}

// A RECORD READ AS IT GOES IS READ PAST THE CAP. A run with tools is a phase's
// whole loop, and its record a line per event: every line is the record, past
// the stdout cap too, and only ONE line is bounded — a line longer than the cap
// is skipped to its end, and the lines after it still arrive.
//
// Mutation: stop forwarding past the cap on an every-line tee, and "late"
// never arrives; hold a long line whole, and the bound below is broken.
func TestAnEveryLineTeeForwardsPastTheCapAndBoundsOneLine(t *testing.T) {
	t.Parallel()
	var lines []string
	sink := &cappedBuffer{limit: 64}
	tee := &lineTee{sink: sink, on: func(l string) { lines = append(lines, l) }, limit: 64, every: true}

	_, _ = tee.Write([]byte("first\n"))
	for range 10 {
		_, _ = tee.Write([]byte(strings.Repeat("x", 32)))
	}
	if len(tee.buf) > 64 {
		t.Errorf("the tee holds %d bytes of one line, past the %d-byte bound", len(tee.buf), 64)
	}
	_, _ = tee.Write([]byte("\nlate\n"))
	if strings.Join(lines, "|") != "first|late" {
		t.Errorf("lines forwarded = %q, want the short lines on either side of the long one", lines)
	}
	if sink.Truncated() == 0 {
		t.Error("the case did not overrun the sink, so it tests nothing")
	}
}
