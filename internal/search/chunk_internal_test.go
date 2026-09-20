package search

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A DOCUMENT IS EMBEDDED WHOLE, which is the property the old 8 KiB cut did
// not have: everything past it was never sent to the provider, so it was in no
// vector and no query could reach it. A handbook whose rate-limit section is
// on page four answered nothing to "how do we handle rate limits", from a
// corpus holding the answer.
func TestEveryPartOfALongDocumentReachesAWindow(t *testing.T) {
	t.Parallel()

	// A marker well past where the old cut fell, so a test that passes
	// cannot be passing on a document that simply fits.
	const marker = "quarterly budget approved by the finance lead"
	body := strings.Repeat("The platform has many procedures. ", 1200) +
		marker + strings.Repeat(" and more prose after it.", 200)
	doc := Document{ID: "p1", Title: "Platform Handbook", Body: body}

	windows := doc.chunks()
	if len(windows) < 2 {
		t.Fatalf("a %d-byte document produced %d window(s)", len(body), len(windows))
	}
	found := false
	for _, w := range windows {
		if strings.Contains(w, marker) {
			found = true
		}
		if !utf8.ValidString(w) {
			t.Error("a window is not valid UTF-8: a rune was split, which " +
				"reaches the provider as a replacement character inside the " +
				"text it is meant to represent")
		}
	}
	if !found {
		t.Errorf("no window holds text %d bytes in: the document is cut, not "+
			"chunked, and everything past the cut is unfindable",
			strings.Index(body, marker))
	}
}

// THE TITLE LEADS EVERY WINDOW BUT THE FIRST, which is the one thing a naive
// split loses: a vector for page four of a runbook, with no idea which
// runbook, matches a query about its subject no better than anybody else's
// page four.
func TestEveryWindowSaysWhichDocumentItIsFrom(t *testing.T) {
	t.Parallel()

	doc := Document{
		ID: "p1", Title: "Incident Runbook",
		Body: strings.Repeat("Step after step after step. ", 800),
	}
	windows := doc.chunks()
	if len(windows) < 2 {
		t.Fatalf("produced %d window(s), want several", len(windows))
	}
	for i, w := range windows {
		if !strings.Contains(w, "Incident Runbook") {
			t.Errorf("window %d does not name its document: %q…", i, w[:40])
		}
	}
	// AND THE FIRST DOES NOT SAY IT TWICE. It already opens with the
	// title, because that is how the document's own text begins.
	if strings.Count(windows[0], "Incident Runbook") != 1 {
		t.Errorf("the first window repeats the title: %q…", windows[0][:60])
	}
}

// A DOCUMENT THAT FITS IS UNCHANGED, which is most of any corpus: one window,
// no repeated title, no overlap — byte for byte what this duty sent before
// chunking existed. The provider bill and the stream rise only for the long
// documents that were not indexed at all.
func TestAShortDocumentIsStillOneWindow(t *testing.T) {
	t.Parallel()

	doc := Document{ID: "t1", Title: "Fix the flake", Body: "It is a port race."}
	windows := doc.chunks()
	if len(windows) != 1 {
		t.Fatalf("a short document produced %d windows", len(windows))
	}
	if windows[0] != doc.text() {
		t.Errorf("window = %q, want the document's own text %q",
			windows[0], doc.text())
	}
}

// WINDOWS OVERLAP, so a paragraph straddling a boundary is represented whole
// in at least one of them. Without it half a thought lands in each vector and
// a query about it matches neither well.
func TestConsecutiveWindowsOverlap(t *testing.T) {
	t.Parallel()

	doc := Document{
		ID: "p1", Title: "T",
		Body: strings.Repeat("alpha beta gamma delta epsilon ", 600),
	}
	windows := doc.chunks()
	if len(windows) < 2 {
		t.Fatalf("produced %d window(s), want several", len(windows))
	}
	// The tail of the first window appears in the second.
	tail := windows[0][len(windows[0])-EmbedChunkOverlap/2:]
	if !strings.Contains(windows[1], strings.TrimSpace(tail)) {
		t.Error("the second window does not repeat the end of the first: a " +
			"paragraph on the boundary is half in each vector and whole in none")
	}
}

// A WINDOW WITH NO WHITESPACE IN IT still terminates. A base64 blob or a
// minified line has no word edge to back up to, and a cut that collapsed to
// the window's start would re-emit the same bytes for ever.
func TestADocumentWithNoWordBoundariesStillTerminates(t *testing.T) {
	t.Parallel()

	doc := Document{ID: "p1", Title: "Blob", Body: strings.Repeat("x", 30_000)}
	windows := doc.chunks()
	if len(windows) < 2 {
		t.Fatalf("produced %d window(s) for 30 000 bytes", len(windows))
	}
	// Bounded by the arithmetic rather than by a magic number: every
	// window but the last advances by at least one stride.
	if len(windows) > 30_000/(EmbedChunkBytes-EmbedChunkOverlap)+4 {
		t.Errorf("produced %d windows, far past what the stride allows — the "+
			"cut is not advancing", len(windows))
	}
}

// AN EMPTY DOCUMENT IS NO WINDOWS, not one empty one: an empty input is never
// sent to the provider, and a record for it would be a vector of nothing.
func TestAnEmptyDocumentProducesNoWindows(t *testing.T) {
	t.Parallel()

	if got := (Document{ID: "t1"}).chunks(); len(got) != 0 {
		t.Errorf("an empty document produced %d window(s)", len(got))
	}
}
