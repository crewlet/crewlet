package textindex

import (
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// maxEscape is the most bytes [JSON] reads one character from: a surrogate pair's two \u escapes.
const maxEscape = 12

// A Finder is [Reading.FindIn] of a text handed over as it is read, in stretches ([Finder.Stretch]) whose
// bytes come in writes of any length ([Finder.Write]): [Finder.Found] answers what FindIn answers of the
// stretches handed over so far, the last ending where it stands. However the writes cut the text, it reads
// each stretch as FindIn reads a piece, from its first byte.
//
// IT CARRIES A WORD IN PROGRESS AND, FOR [JSON], AN ESCAPE IN PROGRESS, never the text: at most
// [MaxTermLength] bytes and a rune of the word, since a word's term is decided once that many are read, and
// the 11 bytes of an escape not yet whole. A Finder of two terms allocates 672 bytes once, its struct
// among them, and nothing as it reads, whatever the text (TestAFinderHoldsItsTermsAndNothingItReads).
type Finder struct {
	r Reading
	// index numbers each term to find, nil where the finder finds nothing; seen marks the ones seen, and
	// order holds them in the order seen, which a rewind takes back.
	index map[string]int
	seen  []bool
	order []int
	s     finderState
	// mark is the state Mark took, while marked.
	mark   finderState
	marked bool
}

// finderState is everything a Finder carries along a text, which [Finder.Mark] takes and [Finder.Rewind]
// puts back: what it found (the first offset, and how many terms of order), where the next byte to read sits
// (off, where pend's bytes, held until they make a whole character, begin), and the word in progress.
type finderState struct {
	off, first, found int
	pend              [maxEscape]byte
	pendLen           int
	// The word in progress: inWord says there is one, begun at wordAt, word[:wordLen] its folded bytes, and
	// decided says its term was decided once it ran past MaxTermLength, so the rest of it is passed.
	inWord          bool
	decided         bool
	wordAt, wordLen int
	word            [MaxTermLength + utf8.UTFMax]byte
}

// Finder is a [Finder] of terms in a text r reads, as [Reading.FindIn] finds them. With no terms, or under
// no reading, it finds nothing.
func (r Reading) Finder(terms []string) *Finder {
	f := &Finder{r: r, s: finderState{first: -1}}
	if len(terms) == 0 || !r.Valid() {
		return f
	}
	f.index = make(map[string]int, len(terms))
	for _, term := range terms {
		if _, ok := f.index[term]; !ok {
			f.index[term] = len(f.index)
		}
	}
	f.seen = make([]bool, len(f.index))
	f.order = make([]int, 0, len(f.index))
	return f
}

// Stretch ends the stretch being read, its last word and escape read as a piece's last, and begins one at
// the byte offset at in the text. Write before any Stretch begins a stretch at 0.
func (f *Finder) Stretch(at int) {
	f.endStretch()
	f.s.off = at
}

// Write reads p as the stretch's next bytes. It never fails, and holds none of p.
func (f *Finder) Write(p []byte) (int, error) {
	n := len(p)
	if held := f.s.pendLen; held > 0 {
		// The held bytes and p's first make the held character whole: any maxEscape bytes from it do.
		var joined [2 * maxEscape]byte
		k := copy(joined[:], f.s.pend[:held])
		k += copy(joined[k:], p)
		used := f.read(joined[:k], false)
		if used < held {
			f.s.pendLen = copy(f.s.pend[:], joined[used:k])
			return n, nil
		}
		f.s.pendLen = 0
		p = p[used-held:]
	}
	used := f.read(p, false)
	f.s.pendLen = copy(f.s.pend[:], p[used:])
	return n, nil
}

// Found answers what [Reading.FindIn] answers of the stretches handed over so far, the last ending where it
// stands: where the earliest of the terms begins, -1 where none does, and whether every one is held. A
// hit that begins before [Finder.Settled] stays whatever is written next; one at or past it may not.
func (f *Finder) Found() (first int, all bool) {
	if f.index == nil {
		return -1, false
	}
	saved := f.s
	f.endStretch()
	first, all = f.s.first, f.s.found == len(f.index)
	f.restore(saved)
	return first, all
}

// Settled is the offset before which every hit is decided: no byte written next takes back a hit
// [Finder.Found] answers before it, or adds one. It is where the word in progress begins, while its term
// is undecided, else where the next character to read begins; so a reader that keeps the text around the
// first hit (a window) keeps it from Settled until Found answers a hit before it.
func (f *Finder) Settled() int {
	if f.s.inWord && !f.s.decided {
		return f.s.wordAt
	}
	return f.s.off
}

// Mark takes the finder's state at the end of what it has read, for Rewind to put back: what it found and
// where, the word and escape in progress, and where it stands. A run of the text whose reading is not yet
// decided (a redaction's `[REDACTED:` that may close as a marker, which a reader is not shown as words) is
// written after Mark, and then kept with Commit or taken back with Rewind. A reader that acts on what
// Found answers inside the run (a window that keeps the text after the first hit) takes its own state at
// the same Mark and puts it back at the same Rewind. A Mark replaces any earlier one.
func (f *Finder) Mark() {
	f.mark, f.marked = f.s, true
}

// Rewind puts back the state Mark took, everything read since unread, a Stretch begun since among it.
// With no Mark it does nothing.
func (f *Finder) Rewind() {
	if f.marked {
		f.restore(f.mark)
		f.marked = false
	}
}

// Commit keeps what was read since Mark, which no Rewind then takes back. With no Mark it does nothing.
func (f *Finder) Commit() {
	f.marked = false
}

// restore puts st back, the terms seen since it unseen.
func (f *Finder) restore(st finderState) {
	for _, i := range f.order[st.found:] {
		f.seen[i] = false
	}
	f.order = f.order[:st.found]
	f.s = st
}

// done reports whether nothing more can be found: there is nothing to find, or every term is seen.
func (f *Finder) done() bool {
	return f.index == nil || f.s.found == len(f.index)
}

// endStretch reads the stretch to its end: the held bytes as the last it has, and the word they end.
func (f *Finder) endStretch() {
	if n := f.s.pendLen; n > 0 {
		f.s.pendLen = 0
		f.read(f.s.pend[:n], true)
	}
	f.endWord()
}

// read reads b, the stretch's bytes from off on, a character at a time as far as each is whole, and all of
// them where final, the stretch ending with b; it answers how many bytes it read. Once nothing more can be
// found it passes b whole.
func (f *Finder) read(b []byte, final bool) int {
	i := 0
	for i < len(b) && !f.done() {
		c, n := f.char(b[i:], final)
		if n == 0 {
			break
		}
		if isWordChar(c) {
			f.inWord(c, f.s.off+i)
		} else {
			f.endWord()
		}
		i += n
	}
	if f.done() {
		i = len(b)
	}
	f.s.off += i
	return i
}

// char is the character b opens with as the finder's reading reads it ([Reading.char]), and how many bytes
// it is written in; or 0 where bytes after b could still change either, unless final.
func (f *Finder) char(b []byte, final bool) (rune, int) {
	if !final {
		switch {
		case f.r == JSON && b[0] == '\\':
			if len(b) < maxEscape && escapeOpen(b) {
				return 0, 0
			}
		case !utf8.FullRune(b):
			return 0, 0
		}
	}
	return f.r.char(string(b[:min(len(b), maxEscape)]))
}

// escapeOpen reports whether bytes after b, which opens with a backslash and is shorter than maxEscape,
// could change the escape [JSON] reads it to open with: b is the opening of a \u escape, or a surrogate's
// whole \u escape and the opening of one after it, which may make a pair with it.
func escapeOpen(b []byte) bool {
	if !uOpening(b) {
		return false
	}
	if len(b) < 6 {
		return true
	}
	high, _ := hexUnit(string(b[2:6]))
	return utf16.IsSurrogate(high) && uOpening(b[6:])
}

// uOpening reports whether b, as far as it goes, opens a \u escape: a backslash, a u and four hex digits.
func uOpening(b []byte) bool {
	for i, c := range b[:min(len(b), 6)] {
		switch {
		case i == 0 && c != '\\', i == 1 && c != 'u':
			return false
		case i >= 2 && (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F'):
			return false
		}
	}
	return true
}

// inWord reads c, a word's character at the byte offset at: it begins a word or goes on with one, and
// decides the word's term once it runs past MaxTermLength, where [truncateTerm] cuts it.
func (f *Finder) inWord(c rune, at int) {
	s := &f.s
	if !s.inWord {
		s.inWord, s.decided, s.wordAt, s.wordLen = true, false, at, 0
	}
	if s.decided {
		return
	}
	s.wordLen += utf8.EncodeRune(s.word[s.wordLen:], unicode.ToLower(c))
	if s.wordLen > MaxTermLength {
		f.term(truncateTerm(s.word[:s.wordLen]), s.wordAt)
		s.decided = true
	}
}

// endWord ends the word in progress, whose term, where it is one and undecided, is its whole.
func (f *Finder) endWord() {
	s := &f.s
	if s.inWord && !s.decided && s.wordLen >= MinTermLength {
		f.term(s.word[:s.wordLen], s.wordAt)
	}
	s.inWord = false
}

// term finds t, a term of the text beginning at the byte offset at, where it is one to find.
func (f *Finder) term(t []byte, at int) {
	i, wanted := f.index[string(t)]
	if !wanted {
		return
	}
	if f.s.first < 0 {
		f.s.first = at
	}
	if !f.seen[i] {
		f.seen[i] = true
		f.order = append(f.order, i)
		f.s.found++
	}
}
