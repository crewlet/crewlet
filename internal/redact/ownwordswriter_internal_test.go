package redact

import (
	"strconv"
	"strings"
	"testing"
)

// stretchRecorder is a StretchReader that keeps the stretches it is handed, as "offset:stretch", a Mark's
// state put back by Rewind, and holds the writer to its word: a stretch begun before any byte, nothing but a
// Commit or a Rewind of a run marked, and one run undecided at a time.
type stretchRecorder struct {
	t         *testing.T
	stretches []stretchSeen
	// marked says a run is undecided, and markN and markLen are the stretches and the last one's length
	// at its Mark.
	marked         bool
	markN, markLen int
	// handed is every byte handed over, kept or not.
	handed int
}

type stretchSeen struct {
	at   int
	text []byte
}

func (r *stretchRecorder) Stretch(at int) {
	r.stretches = append(r.stretches, stretchSeen{at: at})
}

func (r *stretchRecorder) Write(p []byte) (int, error) {
	if len(r.stretches) == 0 {
		r.t.Fatalf("handed %q before any stretch began", p)
	}
	last := &r.stretches[len(r.stretches)-1]
	last.text = append(last.text, p...)
	r.handed += len(p)
	return len(p), nil
}

func (r *stretchRecorder) Mark() {
	if r.marked {
		r.t.Fatal("a run marked while another is undecided")
	}
	r.marked, r.markN, r.markLen = true, len(r.stretches), 0
	if r.markN > 0 {
		r.markLen = len(r.stretches[r.markN-1].text)
	}
}

func (r *stretchRecorder) Commit() {
	if !r.marked {
		r.t.Fatal("a Commit with no run undecided")
	}
	r.marked = false
}

func (r *stretchRecorder) Rewind() {
	if !r.marked {
		r.t.Fatal("a Rewind with no run undecided")
	}
	r.stretches = r.stretches[:r.markN]
	if r.markN > 0 {
		r.stretches[r.markN-1].text = r.stretches[r.markN-1].text[:r.markLen]
	}
	r.marked = false
}

// kept is what the recorder kept, as stretches reads OwnWords, refusing an empty stretch.
func (r *stretchRecorder) kept() []string {
	var out []string
	for _, s := range r.stretches {
		if len(s.text) == 0 {
			r.t.Fatalf("an empty stretch at %d was kept", s.at)
		}
		out = append(out, strconv.Itoa(s.at)+":"+string(s.text))
	}
	return out
}

// streamedOwnWords is what a recorder keeps of text written to an OwnWordsWriter in the writes cuts make.
func streamedOwnWords(t *testing.T, text string, cuts ...int) []string {
	t.Helper()
	r := &stretchRecorder{t: t}
	w := NewOwnWordsWriter(r)
	from := 0
	for _, cut := range append(cuts, len(text)) {
		if n, err := w.Write([]byte(text[from:cut])); n != cut-from || err != nil {
			t.Fatalf("writing %q answers %d, %v", text[from:cut], n, err)
		}
		from = cut
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if r.marked {
		t.Fatalf("%q ends with a run undecided", text)
	}
	return r.kept()
}

// STREAMED, OWN WORDS ARE OWN WORDS: written to an OwnWordsWriter cut anywhere, or a byte at a time, every
// text of the writtenMarkerLen corpus (each marker the pass writes, markers side by side and at either end,
// runs that close as a marker and as words, a kind's name of any length, a run ended by a byte that opens
// the next, a Marker matched in part) is handed to its reader as exactly OwnWords' stretches, one run
// undecided at a time; and a run is the reader's as it arrives, never held: a fresh writer costs a MiB-long
// undecided run what it costs Marker alone, and a write allocates nothing.
//
// Mutation: close a run at a bracket with no name, drop the byte that ends a run as words, hold a run until
// it closes, or keep a copy of the run, and a case goes red.
func TestStreamedOwnWordsAreOwnWords(t *testing.T) {
	// Not parallel: AllocsPerRun counts a parallel test's allocations as this one's.
	for _, text := range ownWordsTexts() {
		holdStreamedOwnWords(t, text)
	}

	// A run of a MiB, never closed: its reader has every byte of it as it arrives.
	r := &stretchRecorder{t: t}
	w := NewOwnWordsWriter(r)
	piece := []byte(strings.Repeat("ab-", 1<<10))
	pieces := (1 << 20) / len(piece)
	if _, err := w.Write(markerBytes); err != nil {
		t.Fatal(err)
	}
	for range pieces {
		if _, err := w.Write(piece); err != nil {
			t.Fatal(err)
		}
	}
	if want := len(Marker) + pieces*len(piece); r.handed != want || !r.marked {
		t.Fatalf("a run of %d bytes still undecided has handed its reader %d (marked %v)", want, r.handed,
			r.marked)
	}
	// And holds none of it: a fresh writer each run, so a copy grown in one run is not reused by the next,
	// against the same writer handed Marker alone, whatever the writer itself costs.
	runOf := func(n int) float64 {
		return testing.AllocsPerRun(10, func() {
			fresh := NewOwnWordsWriter(discardStretches{})
			_, _ = fresh.Write(markerBytes)
			for range n {
				_, _ = fresh.Write(piece)
			}
			_ = fresh.Close()
		})
	}
	if alone, mib := runOf(0), runOf(pieces); mib != alone {
		t.Fatalf("a MiB-long undecided run allocates %v times a run, Marker alone %v", mib, alone)
	}
	w = NewOwnWordsWriter(discardStretches{})
	text := []byte("words " + Marker + "ab-c] [REDAC more [REDACTED:" + strings.Repeat("x", 64))
	if allocs := testing.AllocsPerRun(10, func() { _, _ = w.Write(text) }); allocs != 0 {
		t.Fatalf("writing a text with runs in it allocates %v times a write", allocs)
	}
}

// ownWordsTexts are the writtenMarkerLen corpus: each marker the pass writes, markers side by side and at
// either end, runs that close as a marker and as words, a kind's name of any length, a run ended by a byte
// that opens the next, and Marker matched in part.
func ownWordsTexts() []string {
	name := strings.Repeat("a-z", 400)
	texts := []string{"", "plain words", "before " + passwordMarker + " after", "before " + keyMarker + " after",
		"a [REDACTED:password][REDACTED:api-key] b", "[REDACTED:password] then", "then [REDACTED:password]",
		"see [REDACTED:] here", "see [REDACTED:Hunter 2] here", "see [REDACTED:password",
		"[REDACTED:x y] [REDACTED:password].", "[REDACTED:" + name + "]", "[REDACTED:" + name + "X",
		"w [REDACTED:" + name + "] w", "[REDACTED:ab[REDACTED:cd]", "[REDACTED:[REDACTED:x]e", "[REDACTED:-]",
		"[REDACTED:]]", "[REDACTED:a]]", "[[REDACTED:x]", "[REDAC[REDACTED:x]", "[REDACTED", "[REDACTED:", "[",
		"x [REDACTED:ab", "[REDACTED:A]", "x[REDACTED:a]y[REDACTED:b]z", "é [REDACTED:k]é", "[[[REDACTED:k]]]",
		"[REDACTED:a][REDACTED:b", "ab[REDACTED:x-y]"}
	for _, r := range rules() {
		texts = append(texts, "before "+r.with+" after")
	}
	return texts
}

// holdStreamedOwnWords holds what an OwnWordsWriter hands its reader of text, a byte at a time and cut
// anywhere in two, to OwnWords' stretches.
func holdStreamedOwnWords(t *testing.T, text string) {
	t.Helper()
	want := strings.Join(stretches(text), "|")
	every := make([]int, 0, len(text))
	for k := 1; k < len(text); k++ {
		every = append(every, k)
	}
	if got := strings.Join(streamedOwnWords(t, text, every...), "|"); got != want {
		t.Fatalf("%q a byte at a time reads as %q, want %q", text, got, want)
	}
	for k := 0; k <= len(text); k++ {
		if got := strings.Join(streamedOwnWords(t, text, k), "|"); got != want {
			t.Fatalf("%q cut at %d reads as %q, want %q", text, k, got, want)
		}
	}
}

// FuzzStreamedOwnWordsAreOwnWords holds an OwnWordsWriter to OwnWords as TestStreamedOwnWordsAreOwnWords
// does, over any text up to 4 KiB, the corpus its seeds.
func FuzzStreamedOwnWordsAreOwnWords(f *testing.F) {
	for _, text := range ownWordsTexts() {
		f.Add(text)
	}
	f.Fuzz(func(t *testing.T, text string) {
		if len(text) <= 4<<10 {
			holdStreamedOwnWords(t, text)
		}
	})
}

// discardStretches is a StretchReader that keeps nothing.
type discardStretches struct{}

func (discardStretches) Stretch(int)                 {}
func (discardStretches) Write(p []byte) (int, error) { return len(p), nil }
func (discardStretches) Mark()                       {}
func (discardStretches) Commit()                     {}
func (discardStretches) Rewind()                     {}
