package redact

import (
	"strconv"
	"strings"
	"testing"
)

// stretches is text's stretches as OwnWords yields them, as "offset:stretch".
func stretches(text string) []string {
	var out []string
	for at, stretch := range OwnWords(text) {
		out = append(out, strconv.Itoa(at)+":"+stretch)
	}
	return out
}

// OWN WORDS ARE A REDACTED TEXT'S OWN, NEVER A MARKER'S. Every marker this
// pass writes — each rule's, a password's and a private key's — is a gap
// between the stretches, each stretch at its offset in the text, so a search
// that reads the stretches finds no word a marker spells; and a "[REDACTED:"
// the pass did not write — one naming no kind, or no kind's name before its
// bracket — is the text's own words and stays in its stretch.
//
// Mutation: read a marker as Marker up to any bracket, keep a marker's words
// in a stretch, or count a stretch's offset from the marker before it, and a
// case goes red.
func TestOwnWordsAreARedactedTextsOwnNeverAMarkers(t *testing.T) {
	t.Parallel()
	written := []string{passwordMarker, keyMarker}
	for _, r := range rules() {
		written = append(written, r.with)
	}
	for _, marker := range written {
		text := "before " + marker + " after"
		got := stretches(text)
		if want := []string{"0:before ", strconv.Itoa(len("before "+marker)) + ": after"}; strings.Join(got, "|") !=
			strings.Join(want, "|") {
			t.Errorf("%q reads as %q, want %q: the marker the pass writes is no stretch", text, got, want)
		}
	}
	for name, c := range map[string]struct {
		text string
		want []string
	}{
		"markers side by side": {"a [REDACTED:password][REDACTED:api-key] b",
			[]string{"0:a ", "39: b"}},
		"a marker opening the text": {"[REDACTED:password] then", []string{"19: then"}},
		"a marker closing the text": {"then [REDACTED:password]", []string{"0:then "}},
		"nothing redacted":          {"plain words", []string{"0:plain words"}},
		"no kind named":             {"see [REDACTED:] here", []string{"0:see [REDACTED:] here"}},
		"no kind's name":            {"see [REDACTED:Hunter 2] here", []string{"0:see [REDACTED:Hunter 2] here"}},
		"never closed":              {"see [REDACTED:password", []string{"0:see [REDACTED:password"}},
		"one written after one not": {"[REDACTED:x y] [REDACTED:password].",
			[]string{"0:[REDACTED:x y] ", "34:."}},
	} {
		if got := stretches(c.text); strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("%s: %q reads as %q, want %q", name, c.text, got, c.want)
		}
	}
}

// MARKERS AROUND NAMES THE RUN A CUT WOULD SPLIT: an offset strictly inside a
// marker the pass wrote, or a run of them side by side, is inside the whole
// run, from where its first marker begins to where its last ends; an offset at
// a run's edge — the text's start before a marker opening it among them — or
// in the text's own words, a "[REDACTED:" the pass did not write among them, is
// inside none.
//
// Mutation: end a run where its first marker ends, or count an edge as inside,
// and a case goes red.
func TestMarkersAroundNamesTheRunACutWouldSplit(t *testing.T) {
	t.Parallel()
	text := "a " + passwordMarker + keyMarker + " b [REDACTED:Hunter 2] c " + passwordMarker
	first, run := len("a "), len("a "+passwordMarker+keyMarker)
	last := len(text) - len(passwordMarker)
	for name, c := range map[string]struct {
		at          int
		begin, end  int
		insideOfOne bool
	}{
		"inside the first of two side by side":  {first + 3, first, run, true},
		"inside the second of two side by side": {run - 3, first, run, true},
		"between the two":                       {first + len(passwordMarker), first, run, true},
		"at the run's start":                    {first, 0, 0, false},
		"at the run's end":                      {run, 0, 0, false},
		"in the text's own words":               {run + 2, 0, 0, false},
		"in a marker the pass did not write":    {run + 6, 0, 0, false},
		"inside the marker closing the text":    {last + 1, last, len(text), true},
		"at the text's end":                     {len(text), 0, 0, false},
	} {
		begin, end, inside := MarkersAround(text, c.at)
		if inside != c.insideOfOne || inside && (begin != c.begin || end != c.end) {
			t.Errorf("%s: at %d is inside (%d, %d): %v; want (%d, %d): %v", name, c.at, begin, end, inside,
				c.begin, c.end, c.insideOfOne)
		}
	}
	if begin, end, inside := MarkersAround(passwordMarker+" then", 0); inside {
		t.Errorf("the start of a text a marker opens is inside (%d, %d), want the run's edge", begin, end)
	}
}
