package mcp

import (
	"bufio"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// straddle builds a line whose rune at byte offset start runs across the
// maxStderrLine boundary, followed by enough tail to make the line certainly
// over-long. What a correct cut keeps is exactly the ASCII prefix, because the
// only rune it can walk back to is the one before the straddling one.
func straddle(t *testing.T, start int, r rune) (input, wantKept string) {
	t.Helper()
	prefix := strings.Repeat("x", start)
	input = prefix + string(r) + strings.Repeat("y", 64) + "\n"
	if start+utf8.RuneLen(r) <= maxStderrLine {
		t.Fatalf("rune at %d (%d bytes) does not reach the cap at %d",
			start, utf8.RuneLen(r), maxStderrLine)
	}
	return input, prefix
}

// A RETAINED STDERR LINE IS NEVER CUT INSIDE A RUNE.
//
// The line reaches slog and the crash tail a failed start reports, so a cut
// that lands mid-rune leaves invalid UTF-8 in the one diagnostic an operator
// has when a server refuses to start: slog's JSON handler substitutes U+FFFD
// and the child's own words stop being quotable.
//
// Every straddle position for a 2-, 3- and 4-byte rune is covered, plus the
// boundary the cut falls exactly ON, which must NOT be walked back.
//
// Both reader sizes matter and they fail differently. At 64 KiB the whole line
// arrives in one chunk. At 16 bytes bufio fragments it, so the rune that
// straddles the cap also straddles a CHUNK boundary — the case a chunk-local
// cut cannot see at all, since it walks back inside its own chunk and leaves
// the previous chunk's half-rune committed in the buffer.
func TestARetainedStderrLineIsCutOnARuneBoundary(t *testing.T) {
	t.Parallel()
	for _, r := range []rune{'é', '€', '𝄞'} {
		w := utf8.RuneLen(r)
		for off := 1; off < w; off++ {
			start := maxStderrLine - off // the rune crosses the cap
			for _, bufSize := range []int{16, 64 << 10} {
				input, want := straddle(t, start, r)
				line, truncated, err := readBoundedLine(
					bufio.NewReaderSize(strings.NewReader(input), bufSize))
				if err != nil {
					t.Fatalf("rune %q at %d (buf %d): %v", r, start, bufSize, err)
				}
				if !utf8.ValidString(line) {
					t.Errorf("rune %q at %d (buf %d): the retained line is not valid UTF-8 "+
						"(last bytes %x)", r, start, bufSize, line[max(0, len(line)-4):])
				}
				if line != want {
					t.Errorf("rune %q at %d (buf %d): kept %d bytes, want the %d-byte prefix",
						r, start, bufSize, len(line), len(want))
				}
				if len(line) > maxStderrLine {
					t.Errorf("rune %q at %d (buf %d): kept %d bytes, past the %d-byte cap",
						r, start, bufSize, len(line), maxStderrLine)
				}
				if !truncated {
					t.Errorf("rune %q at %d (buf %d): a shortened line reported no truncation, "+
						"so the pump appends no marker and a reader sees a severed line as a whole one",
						r, start, bufSize)
				}
			}
		}
	}
}

// A CUT THAT LANDS ON A RUNE BOUNDARY KEEPS THAT RUNE'S PREDECESSORS WHOLE and
// walks back no further: the rune-safe cut must not cost a character it did not
// have to.
func TestACutLandingOnARuneBoundaryWalksBackNoFurther(t *testing.T) {
	t.Parallel()
	prefix := strings.Repeat("x", maxStderrLine-3) + "€" // ends exactly at the cap
	input := prefix + "€" + strings.Repeat("y", 8) + "\n"
	line, truncated, err := readBoundedLine(
		bufio.NewReaderSize(strings.NewReader(input), 64<<10))
	if err != nil {
		t.Fatalf("readBoundedLine: %v", err)
	}
	if !truncated {
		t.Error("an over-long line reported no truncation")
	}
	if line != prefix {
		t.Errorf("kept %d bytes, want the %d-byte prefix ending on the boundary",
			len(line), len(prefix))
	}
}

// A LINE THAT FITS EXACTLY IS RETURNED WHOLE AND UNMARKED, and one byte more
// is marked. This is what the probe byte buys: without reading past the cap,
// "at the cap" and "over the cap" are the same observation, and every line of
// exactly maxStderrLine bytes would carry a marker saying bytes were dropped
// when none were.
func TestAStderrLineAtTheCapIsNotMarkedAndOneByteMoreIs(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		size          int
		wantTruncated bool
		wantKept      int
	}{
		{"exactly at the cap", maxStderrLine, false, maxStderrLine},
		{"one byte over", maxStderrLine + 1, true, maxStderrLine},
		{"well under", 12, false, 12},
	} {
		for _, bufSize := range []int{16, 64 << 10} {
			line, truncated, err := readBoundedLine(bufio.NewReaderSize(
				strings.NewReader(strings.Repeat("x", tc.size)+"\n"), bufSize))
			if err != nil {
				t.Fatalf("%s (buf %d): %v", tc.name, bufSize, err)
			}
			if truncated != tc.wantTruncated {
				t.Errorf("%s (buf %d): truncated = %v, want %v",
					tc.name, bufSize, truncated, tc.wantTruncated)
			}
			if len(line) != tc.wantKept {
				t.Errorf("%s (buf %d): kept %d bytes, want %d",
					tc.name, bufSize, len(line), tc.wantKept)
			}
		}
	}
}

// THE MARKED LINE A READER ACTUALLY SEES IS VALID UTF-8, end to end: a real
// child writes a real over-long non-ASCII line down a real pipe, and what the
// crash tail hands to client.reportStartFailure and Bridge.StderrTail is
// checked as a whole — the cut, the marker and the order they go in.
//
// The unit cases above pin the cut; this pins that nothing between the pipe
// and the tail undoes it.
func TestAnOverlongNonASCIIStderrLineReachesTheTailValid(t *testing.T) {
	t.Parallel()
	input, kept := straddle(t, maxStderrLine-2, '€')
	c := mustConnect(t, helperSpec(t, "accented", "serve", map[string]string{
		helperStderrEnv: strings.TrimSuffix(input, "\n"),
	}))
	if _, err := c.listTools(t.Context()); err != nil {
		t.Fatalf("listTools: %v", err)
	}
	got := waitForTail(t, c, 1)[0]
	if !utf8.ValidString(got) {
		t.Errorf("the retained line is not valid UTF-8 (last bytes %x)",
			got[max(0, len(got)-8):])
	}
	if !strings.HasSuffix(got, truncationMarker) {
		t.Error("a truncated line must say so, or the reader believes the server stopped mid-word")
	}
	if got != kept+truncationMarker {
		t.Errorf("retained %d bytes, want the %d-byte prefix plus the marker",
			len(got), len(kept))
	}
}

// A LINE THE CUT KEEPS NOTHING OF STILL SAYS SO.
//
// A rune-safe cut can legitimately keep zero bytes: if the whole first
// maxStderrLine bytes are UTF-8 continuation bytes there is no rune boundary to
// walk back to, which is what a server dumping binary down stderr produces. The
// line must not vanish — "the server said something enormous" is exactly the
// fact the bound exists to preserve, and a reader shown nothing cannot tell it
// from a server that said nothing.
//
// Driven through the real relay rather than readBoundedLine alone, because the
// marker is the pump's half of the contract and the defect is an ordering one:
// marking inside the `line != ""` check drops the line instead.
func TestAStderrLineTheCutEmptiesIsStillMarked(t *testing.T) {
	t.Parallel()
	// Every byte a continuation byte, so utf8.RuneStart is false everywhere
	// and the walk-back reaches zero.
	binary := strings.Repeat("\x80", maxStderrLine+64)

	if _, truncated, err := readBoundedLine(
		bufio.NewReaderSize(strings.NewReader(binary+"\n"), 64<<10)); err != nil || !truncated {
		t.Fatalf("readBoundedLine reported truncated=%v, err=%v; want true, nil", truncated, err)
	}

	relay, err := newStderrRelay("binary", discardLogger())
	if err != nil {
		t.Fatalf("newStderrRelay: %v", err)
	}
	t.Cleanup(relay.forceClose)
	if _, err := relay.writer().WriteString(binary + "\n"); err != nil {
		t.Fatalf("write to the relay: %v", err)
	}
	relay.closeWriter()
	if !relay.drained(10 * time.Second) {
		t.Fatal("the stderr pump never reached EOF")
	}
	got, _ := relay.lines()
	if len(got) != 1 {
		t.Fatalf("the tail holds %d lines, want the one that was written: %q", len(got), got)
	}
	if got[0] != truncationMarker {
		t.Errorf("a line cut to nothing was recorded as %q, want the marker alone", got[0])
	}
}

// THE TAIL WINDOW SAYS WHAT IT DROPPED, AND THE COUNT IS ASKED FOR.
//
// tailLines is a second cut, taken on the COLLECTION where maxStderrLine's is
// taken on one line, and an unmarked collection cut is the failure this whole
// rule is about: fifty lines from a server that wrote fifty and fifty from one
// that wrote five thousand are the same slice while being opposite diagnoses
// ("that is everything it said" against "that is the end of a flood").
//
// The count is returned BESIDE the window rather than left to be inferred from
// len == tailLines, because that inference is wrong in both directions — a
// server that wrote exactly tailLines lines dropped nothing, and the window's
// length says nothing about how far past it the server went.
//
// Driven through the real relay rather than a slice: the count is the pump's
// half of the contract, and the defect this pins is the reslice that dropped
// older lines without counting them.
func TestTheStderrTailWindowSaysWhatItDropped(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		written     int
		wantKept    int
		wantDropped int
	}{
		// A window that reports drops it did not take misleads exactly as
		// badly as one that hides the drops it did, so both ends are pinned.
		{"inside the window", tailLines - 1, tailLines - 1, 0},
		{"exactly the window", tailLines, tailLines, 0},
		{"one past the window", tailLines + 1, tailLines, 1},
		{"a flood", tailLines + 20, tailLines, 20},
	} {
		relay, err := newStderrRelay("chatty", discardLogger())
		if err != nil {
			t.Fatalf("%s: newStderrRelay: %v", tc.name, err)
		}
		t.Cleanup(relay.forceClose)
		var script strings.Builder
		for i := range tc.written {
			fmt.Fprintf(&script, "line-%d\n", i)
		}
		if _, err := relay.writer().WriteString(script.String()); err != nil {
			t.Fatalf("%s: write to the relay: %v", tc.name, err)
		}
		relay.closeWriter()
		if !relay.drained(10 * time.Second) {
			t.Fatalf("%s: the stderr pump never reached EOF", tc.name)
		}

		lines, dropped := relay.lines()
		if len(lines) != tc.wantKept {
			t.Errorf("%s: kept %d lines, want %d", tc.name, len(lines), tc.wantKept)
		}
		if dropped != tc.wantDropped {
			t.Errorf("%s: dropped = %d after %d lines, want %d",
				tc.name, dropped, tc.written, tc.wantDropped)
		}
		// The window keeps the END — a server's dying words, not its banner —
		// so the drop count is a count of the OLDEST lines and the two
		// statements have to agree.
		if len(lines) > 0 {
			wantFirst := fmt.Sprintf("line-%d", tc.wantDropped)
			wantLast := fmt.Sprintf("line-%d", tc.written-1)
			if lines[0] != wantFirst || lines[len(lines)-1] != wantLast {
				t.Errorf("%s: window is %q…%q, want %q…%q",
					tc.name, lines[0], lines[len(lines)-1], wantFirst, wantLast)
			}
		}
	}
}
