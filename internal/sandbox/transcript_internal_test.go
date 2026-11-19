package sandbox

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// longRun is how many tool lines a run too long for its record wrote: some
// 470 KB, past [MaxRunTextBytes] by enough that a middle is always left out,
// so every branch of the bound is reached; more lines only cost redaction.
const longRun = 4_000

// toolLines is n distinct transcript lines of the width an OpenCode tool line
// runs to, numbered so a test can say exactly which were kept.
func toolLines(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("[tool 1] bash: go test ./pkg/%06d/... %s\n", i, strings.Repeat("-", 80))
	}
	return out
}

// A TRANSCRIPT THAT FITS IS CARRIED AS IT IS, redacted and nothing else.
func TestATranscriptThatFitsIsCarriedWhole(t *testing.T) {
	t.Parallel()
	text := strings.Join(toolLines(100), "")
	got, lines, bytes := boundTranscript(text)
	if got != text || lines != 0 || bytes != 0 {
		t.Errorf("a transcript inside the bound changed: %d bytes → %d, %d lines and %d bytes left out",
			len(text), len(got), lines, bytes)
	}
}

// A LONG TRANSCRIPT KEEPS ITS START AND ITS END, in whole lines, and says
// exactly how much of its middle it left out: the start holds the run's plan,
// and a cut without a count reads as the whole.
func TestALongTranscriptKeepsItsStartAndEndAndCountsItsMiddle(t *testing.T) {
	t.Parallel()
	all := toolLines(longRun)
	got, lines, bytes := boundTranscript(strings.Join(all, ""))

	head, rest, found := strings.Cut(got, "left out here")
	if !found {
		t.Fatalf("no note where the middle was: %.200q", got)
	}
	noteStart := strings.LastIndex(head, "\n[note] ") + 1
	keptHead := head[:noteStart]
	_, keptTail, _ := strings.Cut(rest, "\n")

	if !strings.HasPrefix(keptHead, all[0]) || len(keptHead) > transcriptHeadBytes ||
		len(keptHead) < transcriptHeadBytes-len(all[0]) {
		t.Errorf("the start kept %d bytes from line 0; want whole lines filling %d", len(keptHead), transcriptHeadBytes)
	}
	if !strings.HasSuffix(keptTail, all[len(all)-1]) || len(keptTail) > transcriptTailBytes ||
		len(keptTail) < transcriptTailBytes-len(all[0]) {
		t.Errorf("the end kept %d bytes ending on the last line; want whole lines filling %d",
			len(keptTail), transcriptTailBytes)
	}
	if strings.Count(keptHead, "\n")+strings.Count(keptTail, "\n")+lines != len(all) {
		t.Errorf("kept %d + %d lines and counted %d left out, of %d",
			strings.Count(keptHead, "\n"), strings.Count(keptTail, "\n"), lines, len(all))
	}
	if want := len(strings.Join(all, "")) - len(keptHead) - len(keptTail); bytes != want {
		t.Errorf("counted %d bytes left out; want %d", bytes, want)
	}
	if !strings.Contains(head[noteStart:], fmt.Sprintf("[note] %d line(s)", lines)) {
		t.Errorf("the note does not say the count it returns: %q", head[noteStart:])
	}
}

// A LINE LONGER THAN ITS HALF KEEPS ITS OWN START, OR ITS OWN END, on a
// character and marked — never nothing, and never a fragment read as whole.
func TestALineLongerThanItsHalfKeepsAMarkedPartOfItself(t *testing.T) {
	t.Parallel()
	// One line, longer than the whole bound, of three-byte runes so a byte
	// count lands mid-character two times in three.
	one := strings.Repeat("日", MaxRunTextBytes)
	got, lines, bytes := boundTranscript(one)
	if !utf8.ValidString(got) {
		t.Fatal("a cut went through a character")
	}
	start, end, found := strings.Cut(got, " in whole lines\n")
	if !found || !strings.HasPrefix(start, "日") || !strings.Contains(start, "…\n[note] ") {
		t.Errorf("the line's start is not kept and marked: %.60q", start)
	}
	if !strings.HasPrefix(end, "…") || !strings.HasSuffix(end, "日") {
		t.Errorf("the line's end is not kept and marked: …%q", end[max(0, len(end)-20):])
	}
	if lines != 0 || bytes <= 0 || !strings.Contains(got, "of 1 long line(s)") {
		t.Errorf("counted %d whole lines and %d bytes; want no whole line, the rest of the one counted",
			lines, bytes)
	}
	if len(got) > MaxRunTextBytes+1024 {
		t.Errorf("the record kept %d bytes, past the bound and its note", len(got))
	}

	// And an over-long FIRST line keeps its start before the end's whole lines.
	first := strings.Repeat("x", transcriptHeadBytes*2) + "\n" + strings.Join(toolLines(3000), "")
	got, lines, _ = boundTranscript(first)
	if !strings.HasPrefix(got, "xxx") || !strings.Contains(got, "x…\n[note] ") {
		t.Errorf("an over-long first line was not kept in part, marked: %.40q", got)
	}
	if !strings.Contains(got, "and part of 1 more") || lines <= 0 {
		t.Errorf("the note does not say a line was kept in part: lines=%d", lines)
	}
}

// REDACTED WHOLE, THEN BOUNDED. A private key spans lines, so a whole-line cut
// can fall inside one, keeping its BEGIN and dropping its END, which no pattern
// then recognises, and the key's body would reach the launch's record.
func TestAKeyAcrossEitherCutIsRedactedFirst(t *testing.T) {
	t.Parallel()
	const body = "MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun\n"
	key := []string{"-----BEGIN RSA PRIVATE KEY-----\n"}
	for range 16 {
		key = append(key, body)
	}
	key = append(key, "-----END RSA PRIVATE KEY-----\n")

	all := toolLines(longRun)
	width := len(all[0])
	// ONE BOUNDARY AT A TIME: two blocks would let the pattern run from the
	// first one's BEGIN to the second one's END across the note, and redact
	// everything between whichever order the cut came in.
	half := len(strings.Join(key, "")) / 2
	for name, at := range map[string]int{
		// Half the block's bytes before the boundary, half after it.
		"where the start's half ends": (transcriptHeadBytes - half) / width,
		"where the end's half begins": len(all) - (transcriptTailBytes-half)/width,
	} {
		var lines []string
		lines = append(lines, all[:at]...)
		lines = append(lines, key...)
		lines = append(lines, all[at:]...)

		got, _, _ := boundTranscript(strings.Join(lines, ""))
		if !strings.Contains(got, "left out here") {
			t.Fatalf("%s: the transcript was not bounded, so the case tests nothing", name)
		}
		if strings.Contains(got, "MIIEowIBAAKCAQEA") {
			t.Errorf("%s: a key's body survived the bound — the block straddled the cut and was "+
				"redacted after it", name)
		}
	}
}

// THE RECORD CARRIES WHAT WAS LEFT OUT, on the launch's own record, from the
// one place the bound is applied.
func TestTheRecordCarriesWhatTheBoundLeftOut(t *testing.T) {
	t.Parallel()
	c := &Coordinator{}
	got := c.fitResult(t.Context(), PendingRun{}, Result{Transcript: strings.Join(toolLines(longRun), "")})
	if got.TranscriptElidedLines <= 0 || got.TranscriptElidedBytes <= 0 || len(got.Transcript) > MaxRunTextBytes+1024 {
		t.Errorf("fitResult left %d lines / %d bytes out and kept %d bytes",
			got.TranscriptElidedLines, got.TranscriptElidedBytes, len(got.Transcript))
	}
	rec := collectedRecord(PendingRun{}, LaunchRecord{}, got, time.Now())
	if rec.ActivityTranscriptElidedLines != got.TranscriptElidedLines ||
		rec.ActivityTranscriptElidedBytes != got.TranscriptElidedBytes {
		t.Errorf("the record carries %d/%d, the result %d/%d", rec.ActivityTranscriptElidedLines,
			rec.ActivityTranscriptElidedBytes, got.TranscriptElidedLines, got.TranscriptElidedBytes)
	}
}

// A BOUNDED TRANSCRIPT IS WITHIN THE BOUND, its note included, so bounding it
// again keeps it whole and counts nothing; and the box's half ADDS what a
// runner counted as it read the transcript to its own count.
func TestBoundingABoundTranscriptChangesNothing(t *testing.T) {
	t.Parallel()
	for name, text := range map[string]string{
		"tool lines":                strings.Join(toolLines(longRun), ""),
		"one line past the bound":   strings.Repeat("日", MaxRunTextBytes),
		"an over-long first line":   strings.Repeat("x", transcriptHeadBytes*2) + "\n" + strings.Join(toolLines(3000), ""),
		"an over-long last line":    strings.Join(toolLines(3000), "") + strings.Repeat("y", transcriptTailBytes*2),
		"lines filling both halves": strings.Repeat(strings.Repeat("z", transcriptHeadBytes-1)+"\n", 8),
	} {
		once, lines, bytes := boundTranscript(text)
		if lines+bytes == 0 {
			t.Fatalf("%s: the transcript was not bounded, so the case tests nothing", name)
		}
		if len(once) > MaxRunTextBytes {
			t.Errorf("%s: a bounded transcript is %d bytes, past the bound of %d", name, len(once), MaxRunTextBytes)
		}
		if again, l, b := boundTranscript(once); again != once || l != 0 || b != 0 {
			t.Errorf("%s: bounding it again changed it: %d bytes → %d, %d lines and %d bytes counted",
				name, len(once), len(again), l, b)
		}
		got := boundBox(Result{Transcript: once, TranscriptElidedLines: lines, TranscriptElidedBytes: bytes})
		if got.Transcript != once || got.TranscriptElidedLines != lines || got.TranscriptElidedBytes != bytes {
			t.Errorf("%s: the box's half kept %d lines and %d bytes of a runner's %d and %d", name,
				got.TranscriptElidedLines, got.TranscriptElidedBytes, lines, bytes)
		}
	}
	got := boundBox(Result{Transcript: strings.Join(toolLines(longRun), ""), TranscriptElidedLines: 7,
		TranscriptElidedBytes: 700})
	_, lines, bytes := boundTranscript(strings.Join(toolLines(longRun), ""))
	if got.TranscriptElidedLines != lines+7 || got.TranscriptElidedBytes != bytes+700 {
		t.Errorf("the box's half counted %d lines and %d bytes; want its own %d and %d plus the runner's 7 and 700",
			got.TranscriptElidedLines, got.TranscriptElidedBytes, lines, bytes)
	}
	if got := boundBox(Result{TranscriptElidedLines: -3, TranscriptElidedBytes: -4}); got.TranscriptElidedLines != 0 ||
		got.TranscriptElidedBytes != 0 {
		t.Errorf("a runner's negative counts survived: %d lines, %d bytes", got.TranscriptElidedLines,
			got.TranscriptElidedBytes)
	}
}

// THE NOTE FITS ITS ROOM at any counts a transcript can reach, so the halves
// and the note together stay within [MaxRunTextBytes].
func TestTheElidedNoteFitsItsRoom(t *testing.T) {
	t.Parallel()
	const most = math.MaxInt / 2
	for _, c := range [][3]int{{most, 0, most}, {0, most, most}, {most, most, most}, {1, 1, 1}} {
		if note := elidedNote(c[0], c[1], c[2]); len(note) > transcriptNoteRoom {
			t.Errorf("the note at %v is %d bytes, past its room of %d: %q", c, len(note), transcriptNoteRoom, note)
		}
	}
	if transcriptHeadBytes+transcriptNoteRoom+transcriptTailBytes != MaxRunTextBytes {
		t.Errorf("the halves and the note's room add to %d, not the bound %d",
			transcriptHeadBytes+transcriptNoteRoom+transcriptTailBytes, MaxRunTextBytes)
	}
}
