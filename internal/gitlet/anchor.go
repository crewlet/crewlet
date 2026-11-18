package gitlet

import (
	"bytes"
	"math"
)

// WHERE ANCHORED LINES ARE NOW. A finding's or comment's lines are found in a
// later blob by git's zero-context hunks between the two, never by searching
// for their text, which repeats in any file of size and would place a finding
// on another copy of its line. (Not statelog.Reanchor, a log's next generation.)
//
// ONE RULE for every placing: internal/gitlet/query's re-anchoring read and
// internal/gitlet/host's apply_suggestions build (streaming through [Placing]),
// so a suggestion is spliced exactly where its finding was shown.

// Hunk is one hunk of a unified diff as its header gives it: the OldLines
// lines from OldStart that it replaces, and the NewLines lines from NewStart
// that replace them, lines counted from 1. A hunk that only adds is written
// with OldLines 0 and OldStart the line the new ones follow — 0 above the
// first — and one that only deletes with NewLines 0 and NewStart the line the
// deletion follows.
type Hunk struct {
	OldStart, OldLines int
	NewStart, NewLines int
}

// ParseHunk reads a hunk header — `@@ -<start>[,<lines>] +<start>[,<lines>]
// @@`, a count left out for one line as git writes it, and whatever git
// writes after the closing `@@` — or false where line is no such header.
//
// IT ALLOCATES NOTHING: a diff of two large files is millions of headers, and
// a streaming reader would otherwise make garbage that grows with the diff.
func ParseHunk(line []byte) (Hunk, bool) {
	rest, ok := bytes.CutPrefix(line, []byte("@@ -"))
	if !ok {
		return Hunk{}, false
	}
	old, rest, ok := bytes.Cut(rest, []byte(" +"))
	if !ok {
		return Hunk{}, false
	}
	add, rest, ok := bytes.Cut(rest, []byte(" @@"))
	if !ok || (len(rest) > 0 && rest[0] != ' ' && rest[0] != '\n') {
		return Hunk{}, false
	}
	var h Hunk
	if h.OldStart, h.OldLines, ok = hunkRange(old); !ok {
		return Hunk{}, false
	}
	if h.NewStart, h.NewLines, ok = hunkRange(add); !ok {
		return Hunk{}, false
	}
	return h, true
}

// hunkRange reads one side of a hunk header, "<start>" or "<start>,<lines>",
// each in canonical decimal.
func hunkRange(s []byte) (start, lines int, ok bool) {
	first, count, two := bytes.Cut(s, []byte(","))
	if start, ok = hunkNumber(first); !ok {
		return 0, 0, false
	}
	if !two {
		return start, 1, true
	}
	lines, ok = hunkNumber(count)
	return start, lines, ok
}

// hunkNumber is a header's number: decimal digits alone, as git writes them —
// no sign, and no leading zero but a zero's own — and none past what an int
// holds.
func hunkNumber(s []byte) (int, bool) {
	if len(s) == 0 || (len(s) > 1 && s[0] == '0') {
		return 0, false
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' || n > (math.MaxInt-int(c-'0'))/10 {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

// Reanchor is where lines start to end (1-based, inclusive) of a file are in
// the file a diff makes of it, hunks being that diff's — git's zero-context
// hunks, in order, none overlapping — and whether they are still the lines
// they were.
//
// A hunk wholly above the range shifts it by the lines it added less those it
// removed; hunks wholly above or below keep it the same lines, true. One that
// replaces, deletes or inserts between any of its lines makes it OUTDATED,
// false, placed where only the hunks above would have moved it: a place to
// show the old finding near, never a claim its lines are there. An insertion
// right above or right below the range touches none of its lines.
func Reanchor(hunks []Hunk, start, end int) (int, int, bool) {
	p := PlaceLines(start, end)
	for _, h := range hunks {
		if p.Hunk(h) {
			break
		}
	}
	return p.Placed()
}

// Placing is [Reanchor] fed one hunk at a time, IN THE DIFF'S ORDER, for a
// reader that streams a diff's millions of hunks and keeps none: an anchor is
// settled by the first hunk at or below it. Reanchor is this over a slice, so
// the two can never place lines apart.
type Placing struct {
	start, end, shift int
	settled, kept     bool
}

// PlaceLines starts placing lines start to end (1-based, inclusive) of a file
// in the file a diff makes of it.
func PlaceLines(start, end int) Placing { return Placing{start: start, end: end, kept: true} }

// Hunk moves the placing by the diff's next hunk and reports whether it is
// settled, by [Reanchor]'s rule: a hunk above moves it and leaves it open, one
// below settles it kept, one touching its lines settles it OUTDATED. A settled
// placing moves no further.
func (p *Placing) Hunk(h Hunk) bool {
	if p.settled {
		return true
	}
	if h.OldLines == 0 {
		// An insertion after old line OldStart: above the range where it
		// follows a line above it, inside where it follows one of its lines
		// but the last.
		switch {
		case h.OldStart < p.start:
			p.shift += h.NewLines
		case h.OldStart >= p.end:
			p.settled = true
		default:
			p.settled, p.kept = true, false
		}
		return p.settled
	}
	last := h.OldStart + h.OldLines - 1
	switch {
	case last < p.start:
		p.shift += h.NewLines - h.OldLines
	case h.OldStart > p.end:
		p.settled = true
	default:
		p.settled, p.kept = true, false
	}
	return p.settled
}

// Placed is Reanchor's answer over the hunks fed so far: once the diff has
// ended, a placing no hunk settled is the same lines, moved by every hunk
// above them.
func (p Placing) Placed() (int, int, bool) { return p.start + p.shift, p.end + p.shift, p.kept }
