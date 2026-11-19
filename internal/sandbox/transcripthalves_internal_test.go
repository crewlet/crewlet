package sandbox

import (
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/textcut"
)

// sliceBound is the record's rule over a text held whole, as written before
// the halves: the oracle [TranscriptHalves] is held to.
func sliceBound(text string) (string, int, int) {
	if len(text) <= MaxRunTextBytes {
		return text, 0, 0
	}
	lines := strings.SplitAfter(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	head, size := 0, 0
	for head < len(lines) && size+len(lines[head]) <= transcriptHeadBytes {
		size += len(lines[head])
		head++
	}
	tail, size := len(lines), 0
	for tail > head && size+len(lines[tail-1]) <= transcriptTailBytes {
		size += len(lines[tail-1])
		tail--
	}

	// What lies between the halves; the text is past the budget, so there is
	// always some. A half that kept no whole line keeps its edge line's start
	// or end instead.
	middle := lines[head:tail]
	const marker = "…"
	var startOf, endOf string
	if head == 0 {
		// Within the half with room for the line break it ends on, so a
		// line that did not fit whole is always marked as kept in part.
		startOf = textcut.Within(strings.TrimRight(middle[0], "\n"), transcriptHeadBytes-1) + "\n"
	}
	if tail == len(lines) {
		endOf = textcut.Tail(middle[len(middle)-1], transcriptTailBytes-len(marker))
	}
	dropped := 0
	for _, line := range middle {
		dropped += len(line)
	}
	// Bytes kept of a partial line are not left out; the marker each carries
	// is not content, so it is not counted against what was.
	if startOf != "" {
		dropped -= len(startOf) - len(marker)
	}
	if endOf != "" {
		dropped -= len(endOf) - len(marker)
	}
	// Lines kept in part are not lines left out: one when only one edge line
	// was partial or the text is that one line, two when both edges were.
	partial := 0
	if startOf != "" {
		partial++
	}
	if endOf != "" && (startOf == "" || len(middle) > 1) {
		partial++
	}
	whole := len(middle) - partial

	var b strings.Builder
	b.WriteString(strings.Join(lines[:head], ""))
	b.WriteString(startOf)
	b.WriteString(elidedNote(whole, partial, dropped))
	b.WriteString(endOf)
	b.WriteString(strings.Join(lines[tail:], ""))
	return b.String(), whole, dropped
}

// randomLines is n bytes of lines of random widths, now and then a rune of
// three bytes, so a cut lands inside one.
func randomLines(r *rand.Rand, n int) string {
	var b strings.Builder
	for b.Len() < n {
		w := r.IntN(300)
		for range w {
			if r.IntN(40) == 0 {
				b.WriteString("日")
			} else {
				b.WriteByte(byte('a' + r.IntN(26)))
			}
		}
		b.WriteByte('\n')
	}
	return b.String()[:n]
}

// halvesOf writes text to fresh halves in random pieces and answers them.
func halvesOf(r *rand.Rand, text string) (string, int, int) {
	var h TranscriptHalves
	for len(text) > 0 {
		k := min(len(text), 1+r.IntN(64<<10))
		if r.IntN(8) == 0 {
			k = min(len(text), 1+r.IntN(16))
		}
		if r.IntN(2) == 0 {
			_, _ = h.WriteString(text[:k])
		} else {
			_, _ = h.Write([]byte(text[:k]))
		}
		text = text[k:]
	}
	return h.Bounded()
}

// THE HALVES ARE THE RECORD, however the text arrives: written in random
// pieces, they answer exactly what the rule over the whole text answers, in
// text and counts, at every length about the bound (where the head and the
// ring meet), with a line starting where the end's half begins, a line
// longer than either half, and no line break at all.
func TestATranscriptsHalvesAreItsRecordHoweverItsRedactionArrives(t *testing.T) {
	t.Parallel()
	seed := rand.Uint64()
	t.Logf("seed %d", seed)
	r := rand.New(rand.NewPCG(seed, 1))
	cases := map[string]string{}
	for i := range 24 {
		cases["random length "+string(rune('a'+i))] = randomLines(r, r.IntN(3*MaxRunTextBytes))
	}
	about := randomLines(r, MaxRunTextBytes+1100)
	for k := range 1027 {
		cases["the bound less "+strconv.Itoa(k)] = about[:MaxRunTextBytes-k]
		cases["the bound and "+strconv.Itoa(k)] = about[:MaxRunTextBytes+k]
	}
	for _, d := range []int{-1, 0, 1} {
		// The end: one padded line, then lines of 100 bytes, transcriptTailBytes+d
		// in all, so a line starts that far from the end.
		rest := transcriptTailBytes + d
		pad := rest % 100
		end := strings.Repeat("p", pad+99) + "\n" + strings.Repeat(strings.Repeat("e", 99)+"\n", rest/100-1)
		cases["a line starting the end's half and "+strconv.Itoa(d+1)] = randomLines(r, 100<<10) + "\n" + end
	}
	cases["a first line longer than its half"] = strings.Repeat("f", transcriptHeadBytes+500) + "\n" +
		randomLines(r, MaxRunTextBytes)
	cases["a first line of exactly its half"] = strings.Repeat("f", transcriptHeadBytes-1) + "\n" +
		randomLines(r, MaxRunTextBytes)
	cases["a first line one past its half"] = strings.Repeat("f", transcriptHeadBytes) + "\n" +
		randomLines(r, MaxRunTextBytes)
	cases["a last line longer than its half"] = randomLines(r, MaxRunTextBytes) + "\n" +
		strings.Repeat("l", transcriptTailBytes+500)
	cases["a last line longer than its half, ended"] = randomLines(r, MaxRunTextBytes) + "\n" +
		strings.Repeat("l", transcriptTailBytes+500) + "\n"
	cases["a last line of exactly its half"] = randomLines(r, MaxRunTextBytes) + "\n" +
		strings.Repeat("l", transcriptTailBytes-1) + "\n"
	cases["one line past the whole bound"] = strings.Repeat("日", MaxRunTextBytes)
	cases["no line break"] = strings.Repeat("n", 3*MaxRunTextBytes+7)
	for name, text := range cases {
		want, wantLines, wantBytes := sliceBound(text)
		got, lines, bytes := halvesOf(r, text)
		if got != want || lines != wantLines || bytes != wantBytes {
			t.Errorf("%s (%d bytes): the halves kept %d bytes, %d lines and %d bytes left out; the whole "+
				"text's rule %d bytes, %d and %d", name, len(text), len(got), lines, bytes, len(want),
				wantLines, wantBytes)
		}
	}
}

// THE HALVES HOLD NO MORE THAN THE RECORD KEEPS, whatever was written to them.
func TestATranscriptsHalvesHoldNoMoreThanTheRecordKeeps(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(1, 2))
	piece := randomLines(r, 1<<20)
	var h TranscriptHalves
	for range 64 {
		_, _ = h.WriteString(piece)
	}
	if held := cap(h.head) + cap(h.ring); held > MaxRunTextBytes {
		t.Errorf("after 64 MiB the halves hold %d bytes; the record keeps %d", held, MaxRunTextBytes)
	}
	if got, _, _ := h.Bounded(); len(got) > MaxRunTextBytes {
		t.Errorf("the record is %d bytes, past its bound", len(got))
	}
}
