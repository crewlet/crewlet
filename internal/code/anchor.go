package code

import (
	"strconv"
	"strings"
)

// WHERE ANCHORED LINES ARE NOW. A finding or a comment is written against a
// range of lines of one blob; when the change moves on, the same lines are
// found in the new blob by the diff between the two — git's zero-context
// hunks, which name exactly the lines each change replaced — and never by
// searching for the lines' text, which repeats in any file of size and would
// place a finding on another copy of its line. (Not a log's reanchor,
// statelog.Reanchor, which moves a state log to its next generation.)
//
// ONE RULE, read wherever lines are placed: the code host's re-anchoring read
// (internal/code/query) for a finding shown on a later revision, and an
// apply_suggestions' build (internal/code/host) for a finding's suggestion at
// the head it applies to, so a suggestion is spliced exactly where a reader
// was shown its finding — the build feeding it one hunk at a time
// ([Placing]), as a diff streams by.

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
func ParseHunk(line string) (Hunk, bool) {
	rest, ok := strings.CutPrefix(line, "@@ -")
	if !ok {
		return Hunk{}, false
	}
	old, rest, ok := strings.Cut(rest, " +")
	if !ok {
		return Hunk{}, false
	}
	add, rest, ok := strings.Cut(rest, " @@")
	if !ok || (rest != "" && rest[0] != ' ' && rest[0] != '\n') {
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
func hunkRange(s string) (start, lines int, ok bool) {
	first, count, two := strings.Cut(s, ",")
	if start, ok = hunkNumber(first); !ok {
		return 0, 0, false
	}
	if !two {
		return start, 1, true
	}
	lines, ok = hunkNumber(count)
	return start, lines, ok
}

// hunkNumber is a header's number: decimal digits alone, as git writes them.
func hunkNumber(s string) (int, bool) {
	if s == "" || s[0] < '0' || s[0] > '9' {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil && strconv.Itoa(n) == s
}

// Reanchor is where lines start to end (1-based, inclusive) of a file are in
// the file a diff makes of it, hunks being that diff's — git's zero-context
// hunks, in their order, none overlapping another — and whether they are
// still the lines they were.
//
// A hunk wholly above the range moves it by the lines it added less the lines
// it removed, and one wholly below it leaves it be: then the range is the
// same lines, true. A hunk that replaces or deletes any of its lines, or
// inserts lines between two of them, changed what the range holds: false,
// OUTDATED, with start and end where the range would begin and end had only
// the hunks above it moved it — a place to show the old finding near, never a
// claim that its lines are there. An insertion right above the range or right
// below it touches none of its lines.
func Reanchor(hunks []Hunk, start, end int) (int, int, bool) {
	p := PlaceLines(start, end)
	for _, h := range hunks {
		if p.Hunk(h) {
			break
		}
	}
	return p.Placed()
}

// Placing is [Reanchor] fed one hunk at a time, IN THE DIFF'S ORDER: the one
// rule for a reader that streams a diff's hunks and keeps none of them, so
// what it holds is a placing an anchor, never a hunk a line of the diff — a
// diff of two large files is millions of hunks, and an anchor is settled by
// the first hunk at or below it. Reanchor is this over a slice, so the two
// can never place lines apart.
type Placing struct {
	start, end, shift int
	settled, kept     bool
}

// PlaceLines starts placing lines start to end (1-based, inclusive) of a file
// in the file a diff makes of it.
func PlaceLines(start, end int) Placing { return Placing{start: start, end: end, kept: true} }

// Hunk moves the placing by the diff's next hunk, and reports whether it is
// settled: a hunk wholly above the range moves it by the lines it added less
// the lines it removed and leaves it open; one wholly below it settles it
// where it is, the same lines; and one that replaces, deletes or inserts
// between any of its lines settles it OUTDATED. A settled placing moves no
// further, whatever hunks follow.
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

// Placed is where the lines are now, and whether they are still the lines
// they were — Reanchor's answer, over the hunks fed so far: once the diff has
// ended, a placing no hunk settled is the same lines, moved by every hunk
// above them.
func (p Placing) Placed() (int, int, bool) { return p.start + p.shift, p.end + p.shift, p.kept }
