package sandbox

import (
	"strings"
	"testing"
)

// A TRANSCRIPT'S HALVES CUT TO A LENGTH KEEP ITS WHOLE LINES BEFORE IT: within
// what the record holds whole, cut back to the last line break, the lines and
// bytes recounted; past it, cut within the end's ring when the end keeps its
// half, refused where the bytes dropped or the break it would end at are no
// longer held, or the end left could not keep its half.
//
// Mutations, each red: keep the line the cut falls inside; recount nothing;
// cut into bytes no longer held.
func TestATranscriptsHalvesCutToALengthKeepTheirWholeLines(t *testing.T) {
	t.Parallel()
	var short TranscriptHalves
	_, _ = short.WriteString("one\ntwo\nthree\nfour")
	if !short.Cut(len("one\ntwo\nthr")) {
		t.Fatal("a cut within what is held whole was refused")
	}
	if text, _, _ := short.Bounded(); text != "one\ntwo\n" || short.breaks != 2 || short.bytes != len(text) {
		t.Errorf("cut to its whole lines it is %q, %d breaks of %d bytes; want the two lines", text, short.breaks,
			short.bytes)
	}

	line := strings.Repeat("x", 99) + "\n"
	var long TranscriptHalves
	for long.bytes < 2*MaxRunTextBytes {
		_, _ = long.WriteString(line)
	}
	whole := long.bytes
	if !long.Cut(whole - 10) {
		t.Fatal("a cut of a line's end within the ring was refused")
	}
	if long.bytes != whole-len(line) || long.last != '\n' {
		t.Errorf("cut it holds %d bytes ending %q; want the line it fell inside dropped", long.bytes, long.last)
	}
	if long.Cut(long.bytes - MaxRunTextBytes) {
		t.Error("a cut past what the ring holds was taken")
	}
}
