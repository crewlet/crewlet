package executor

import "bytes"

// A THREAD LINE'S BYTE FACTS: WHAT A READER OF ITS BYTES READS OF IT.
//
// Claude Code's large-file readers read a line by its bytes, never by its
// decode ([threadChain], THE LARGE FILE): whether it begins `{"parentUuid":`
// and then its parent, whether it carries that key at its object's depth one,
// the uuid after `"uuid":"` they key it by, and whether it holds
// `"isSidechain":true` anywhere. The resumed count marks a line by the bytes
// its media and its server's clearing edit are written with
// ([resumedReading]). So every whole line, a record or one the layer refuses,
// carries what those readers read of its bytes ([threadBytes]), beside what
// its accessors read of its decode ([lineFacts]).
//
// BYTE FACTS STAY BYTE FACTS, never read off the decode. A pattern can begin
// inside a name, through an escaped quote: a call's input keyed
// `x"isSidechain` and valued true is written `"x\"isSidechain":true`, whose
// bytes hold `"isSidechain":true`, so every reader of bytes reads that record
// as a sidechain's where a reader of its decode reads none; and the decode
// reads `{"parent\u0055uid":null}` as a parent no reader of bytes reads. Read
// off the decode, the chain would judge such a thread path-free where the
// readers start elsewhere, which is a false hold.
//
// WHAT IS HELD IS OF A FIXED SIZE, whatever the line: its length, its first
// [lineHeadBytes] bytes, one bit for the key the dispatch takes a slot by,
// the 36 bytes the readers key it by, and one bit a mark ([byteMark]).
//
// READ AS THE LINE STREAMS PAST ([lineBytes]), in pieces of any size, so a
// reader of a line need never hold it to read what its bytes say. The scan is
// the readers' own ([jsonDepth]), stepped over every byte but those inside a
// string that step it nowhere, which it jumps past to the next quote or
// backslash. So it visits EVERY QUOTE OF THE LINE, an escaped one too, and
// reads the depth at each before stepping it; and since every pattern a
// reader reads begins with a quote, every pattern is compared at each quote
// with the bytes from it, a comparison a piece ends inside kept with how far
// it matched and finished from the next piece. A comparison reads its bytes
// from the pieces, never from what the scan stepped, so the jumps skip no
// byte a comparison needs. Of the uuid keys, one is taken only 8 bytes or
// more past the last taken, as the readers' search resumes past a key's own
// bytes, and the 51 bytes after it (its 36 and `","timestamp":"`) are read
// into a window of its own; the key the line is keyed by is chosen at its end
// ([lineBytes.facts]). What the stream keeps is bounded whatever the line
// holds: the facts, the scan, a few dozen comparisons and at most eight
// windows, about 650 bytes.
//
// THE WHOLE-LINE FUNCTIONS THE READERS USED BEFORE ARE ITS ORACLE, in the
// tests (claudelinebytes_internal_test.go): what the stream reads of a line
// in pieces of any size is what they read of the whole line.

// slotKeyLen is how many bytes a large-file reader keys a slot by, and reads a
// parent as: a canonical uuid's.
const slotKeyLen = 36

// lineHeadBytes is how many of a line's first bytes its byte facts hold
// ([threadBytes.Head]): `{"parentUuid":`, a quoted canonical uuid and
// `,"isSidechain":true`, the longest head any reader of the head reads
// ([slotHead], [sidechainHeaded], and the subagent's head in
// [threadChain.slotFacts]). 2.1.322's dispatch reads a line's first 256 bytes
// for `"isSidechain":`, which the chain models through this exact head, so
// these are the bytes of that window it reads.
const lineHeadBytes = len(`{"parentUuid":`) + slotKeyLen + 2 + len(`,"isSidechain":true`)

// byteMark is a set of marks a line's bytes may hold anywhere, each the exact
// bytes of [byteMarks].
type byteMark uint8

const (
	// markSidechain is `"isSidechain":true`, by which a large-file reader
	// marks a slot a sidechain's ([slotSidechain]).
	markSidechain byteMark = 1 << iota
	// markPDF, markImage and markDocument are a PDF read, an image and a
	// document, as JSON.stringify writes their types ([pdfReadMark],
	// [imageMark], [documentMark]).
	markPDF
	markImage
	markDocument
	// markClearing is the server's clearing edit ([clearingEdit]).
	markClearing
	// markUnmetered is the provisional record the harness never anchors on
	// ([unmetered]).
	markUnmetered
)

// byteMarks is each mark and the bytes it is.
var byteMarks = [...]struct {
	mark  byteMark
	bytes []byte
}{
	{markSidechain, slotSidechain},
	{markPDF, pdfReadMark},
	{markImage, imageMark},
	{markDocument, documentMark},
	{markClearing, clearingEdit},
	{markUnmetered, unmetered},
}

// threadBytes is what a reader of a line's bytes reads of one whole line, its
// break dropped: a value of a fixed size, its reader's to keep, which never
// holds the line ([threadLine.Bytes], [threadLineError.Bytes]). Its zero value
// is a line of no bytes.
type threadBytes struct {
	// n is how many bytes the line holds.
	n int64
	// head is the line's first bytes, headLen of them, at most
	// [lineHeadBytes].
	head    [lineHeadBytes]byte
	headLen uint8
	// key is what the readers key the line by, where keyed.
	key   [slotKeyLen]byte
	keyed bool
	// slotKeyed is [threadBytes.SlotKeyed].
	slotKeyed bool
	// marks is the marks the line holds.
	marks byteMark
}

// Len is how many bytes the line holds, its break dropped.
func (b *threadBytes) Len() int64 { return b.n }

// Head is the line's first bytes, at most [lineHeadBytes] of them: every byte
// a reader of a line's head reads.
func (b *threadBytes) Head() []byte { return b.head[:b.headLen] }

// SlotKeyed reports whether the line carries `"parentUuid":` at its object's
// depth one, outside every string (2.1.322's N1t @293787634, 2.1.310's Ipo
// @247171334), as a line that begins `{"parentUuid":`, the prune's slot, does
// too: whether the dispatch may take the line for a slot. A line whose JSON
// fails is scanned by its bytes as the readers scan it, so its depth is
// theirs.
func (b *threadBytes) SlotKeyed() bool { return b.slotKeyed }

// KeyedBy reports whether the uuid every large-file reader keys the line by
// is id, byte for byte: false where they key it by none. They key it by the
// 36 bytes after the first `"uuid":"` they follow with `","timestamp":"`, of
// several such the first at the line's object depth one, else the last of
// them, and where none is, after the first `"uuid":"` (2.1.322's pa in ike
// @293793061 with dzt @293787942, keyed alike in F1t @293788207; 2.1.310's vs
// in HMt @247176207 with $Mt @247171638, alike in FPt @247171899), none where
// fewer than 36 bytes follow it. A reader reads those bytes as latin1 and
// compares them as written, so only the same bytes are the same uuid.
func (b *threadBytes) KeyedBy(id string) bool { return b.keyed && string(b.key[:]) == id }

// Has reports whether the line holds every mark of m, anywhere in its bytes.
func (b *threadBytes) Has(m byteMark) bool { return b.marks&m == m }

// lineBytesOf is the byte facts of raw, a whole line, its break dropped,
// read as its bytes stream past ([lineBytes]).
func lineBytesOf(raw []byte) threadBytes {
	var l lineBytes
	l.write(raw)
	return l.facts()
}

// lineBytes reads a whole line's byte facts ([threadBytes]) off its bytes as
// they stream past ([lineBytes.write]), in pieces of any size, never holding
// the line: its zero value is a line of no bytes read yet, and
// [lineBytes.facts] the facts of the bytes written so far, read as a whole
// line. What it keeps is bounded whatever the line holds (READ AS THE LINE
// STREAMS PAST, above).
type lineBytes struct {
	// out is the facts read so far: the length, the head, the dispatch's key
	// and the marks as met; the key the readers key the line by is chosen at
	// the end ([lineBytes.facts]).
	out threadBytes
	// depth is the readers' own scan of the bytes so far ([jsonDepth]).
	depth jsonDepth
	// cmps is each comparison begun at a quote that the bytes so far end
	// inside, in the order begun: at most one a quote of a pattern, so a few
	// dozen.
	cmps []patternCmp
	// keys is each taken `"uuid":"` whose 51 bytes after the key the bytes so
	// far have not all reached, in the order taken: at most eight, since one
	// is taken only 8 bytes past the last.
	keys []keyWindow
	// took is whether a `"uuid":"` was taken, and taken where the last began.
	took  bool
	taken int64
	// first is the 36 bytes after the first `"uuid":"`, where firstFull says
	// all 36 followed it, once its window is read whole; until then its
	// window holds them ([lineBytes.facts]).
	first     [slotKeyLen]byte
	firstFull bool
	// goods is how many keys `","timestamp":"` follows, 2 for two or more;
	// lastGood the last of them, and oneGood the first at the line's depth
	// one, where hasOne.
	goods    uint8
	lastGood [slotKeyLen]byte
	oneGood  [slotKeyLen]byte
	hasOne   bool
}

// linePatterns is what [lineBytes] compares at each quote: the dispatch's
// key (patternParent), the readers' uuid key (patternUUID), and each mark of
// [byteMarks] in its order from patternMarks. Each begins with a quote.
var linePatterns = func() [patternMarks + len(byteMarks)][]byte {
	var p [patternMarks + len(byteMarks)][]byte
	p[patternParent], p[patternUUID] = slotParentKey, slotUUIDKey
	for i, m := range byteMarks {
		p[patternMarks+i] = m.bytes
	}
	return p
}()

const (
	patternParent = iota
	patternUUID
	patternMarks
)

// patternCmp is a comparison of linePatterns[pattern] with the bytes from
// start, the quote it begins at, of which matched have matched; one is
// whether the scan stood at depth one at that quote ([jsonDepth.one]).
type patternCmp struct {
	start   int64
	pattern uint8
	matched uint8
	one     bool
}

// keyWindow is a taken `"uuid":"` that began at start, one whether at depth
// one, and the bytes after its key, n of them so far: the 36 it may be keyed
// by and the 15 `","timestamp":"` must be. first is whether it is the line's
// first.
type keyWindow struct {
	start int64
	after [slotKeyLen + len(`","timestamp":"`)]byte
	n     uint8
	one   bool
	first bool
}

// write reads p, the line's next bytes, its break never among them.
func (l *lineBytes) write(p []byte) {
	base := l.out.n
	if base < int64(lineHeadBytes) {
		l.out.headLen = uint8(base) + uint8(copy(l.out.head[base:], p))
	}
	l.out.n += int64(len(p))
	// WHAT THE BYTES BEFORE p LEFT OPEN goes first, in the order begun: the
	// windows of keys taken, then the comparisons a quote began, whose keys
	// taken here are newer than every window.
	keys := l.keys[:0]
	for i := range l.keys {
		if !l.fill(&l.keys[i], p) {
			keys = append(keys, l.keys[i])
		}
	}
	l.keys = keys
	cmps := l.cmps[:0]
	for _, c := range l.cmps {
		pattern := linePatterns[c.pattern]
		m := min(len(pattern)-int(c.matched), len(p))
		switch {
		case !bytes.Equal(p[:m], pattern[c.matched:int(c.matched)+m]):
		case int(c.matched)+m == len(pattern):
			l.matched(c, p, base)
		default:
			c.matched += uint8(m)
			cmps = append(cmps, c)
		}
	}
	// THEN p, BYTE BY BYTE AS THE READERS SCAN IT: every byte the scan
	// steps is stepped, a quote compared with each pattern before it is, and
	// inside a string the scan jumps to the next quote or backslash, past
	// bytes that step it nowhere. Every quote of the line is visited, an
	// escaped one too (its backslash's next byte), so a pattern that begins
	// inside a name through an escaped quote is met as the readers meet it.
	l.cmps = cmps
	quote, backslash := -1, -1
	for i := 0; i < len(p); i++ {
		if l.depth.inString && !l.depth.escape {
			if quote < i {
				quote = indexFrom(p, i, '"')
			}
			if backslash < i {
				backslash = indexFrom(p, i, '\\')
			}
			if i = min(quote, backslash); i == len(p) {
				return
			}
		}
		if p[i] == '"' {
			l.compare(p, i, base)
		}
		l.depth.step(p[i])
	}
}

// indexFrom is where the first c of p at or past from lies, len(p) where none
// does.
func indexFrom(p []byte, from int, c byte) int {
	if at := bytes.IndexByte(p[from:], c); at >= 0 {
		return from + at
	}
	return len(p)
}

// compare begins a comparison of every pattern a quote may begin with the
// bytes from p[i], a quote at base+i, the scan standing where it stands
// before stepping it.
func (l *lineBytes) compare(p []byte, i int, base int64) {
	one := l.depth.one()
	for k, pattern := range linePatterns {
		switch {
		case k == patternParent && (!one || l.out.slotKeyed):
			continue
		case k >= patternMarks && l.out.marks&byteMarks[k-patternMarks].mark != 0:
			continue
		}
		m := min(len(pattern), len(p)-i)
		if !bytes.Equal(p[i:i+m], pattern[:m]) {
			continue
		}
		c := patternCmp{start: base + int64(i), pattern: uint8(k), matched: uint8(m), one: one}
		if m < len(pattern) {
			l.cmps = append(l.cmps, c)
			continue
		}
		l.matched(c, p, base)
	}
}

// matched files c, a comparison every byte of its pattern matched, in p, the
// piece at base.
func (l *lineBytes) matched(c patternCmp, p []byte, base int64) {
	switch {
	case c.pattern == patternParent:
		l.out.slotKeyed = true
	case c.pattern >= patternMarks:
		l.out.marks |= byteMarks[c.pattern-patternMarks].mark
	case l.took && c.start-l.taken < int64(len(slotUUIDKey)):
		// NOT TAKEN: the readers' search resumes past the last taken key, so a
		// key that begins inside it is never met.
	default:
		w := keyWindow{start: c.start, one: c.one, first: !l.took}
		l.took, l.taken = true, c.start
		if !l.fill(&w, p[c.start+int64(len(slotUUIDKey))-base:]) {
			l.keys = append(l.keys, w)
		}
	}
}

// fill reads into w the bytes of p after its key, p beginning where w's
// bytes so far end; true once all its bytes are read, w then filed.
func (l *lineBytes) fill(w *keyWindow, p []byte) bool {
	w.n += uint8(copy(w.after[w.n:], p))
	if int(w.n) < len(w.after) {
		return false
	}
	if w.first {
		l.first, l.firstFull = [slotKeyLen]byte(w.after[:slotKeyLen]), true
	}
	if bytes.Equal(w.after[slotKeyLen:], slotTimeAfter) {
		l.goods = min(l.goods+1, 2)
		l.lastGood = [slotKeyLen]byte(w.after[:slotKeyLen])
		if w.one && !l.hasOne {
			l.oneGood, l.hasOne = l.lastGood, true
		}
	}
	return true
}

// facts is the facts of the bytes written so far, read as a whole line: a
// key whose bytes the line ends inside is no good one, and the first key is
// one only where all 36 of its bytes followed it.
func (l *lineBytes) facts() threadBytes {
	out := l.out
	first, full := l.first, l.firstFull
	for _, w := range l.keys {
		if w.first && int(w.n) >= slotKeyLen {
			first, full = [slotKeyLen]byte(w.after[:slotKeyLen]), true
		}
	}
	switch {
	case l.goods > 1 && l.hasOne:
		out.key, out.keyed = l.oneGood, true
	case l.goods > 0:
		out.key, out.keyed = l.lastGood, true
	case full:
		out.key, out.keyed = first, true
	}
	return out
}

// jsonDepth is the scan both large-file readers make of a line's bytes where
// they look for a key at its object's depth one: how many objects deep it
// stands outside every string, a string ending at its next quote that no
// backslash escapes (2.1.322's N1t and dzt, 2.1.310's Ipo and $Mt).
type jsonDepth struct {
	depth            int
	inString, escape bool
}

// step reads b, the line's next byte.
func (s *jsonDepth) step(b byte) {
	switch {
	case s.escape:
		s.escape = false
	case s.inString:
		switch b {
		case '\\':
			s.escape = true
		case '"':
			s.inString = false
		}
	case b == '"':
		s.inString = true
	case b == '{':
		s.depth++
	case b == '}':
		s.depth--
	}
}

// one reports whether the scan stands at depth one, outside every string.
func (s *jsonDepth) one() bool { return s.depth == 1 && !s.inString }
