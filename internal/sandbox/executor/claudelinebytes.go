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
// reads `{"parentUuid":null}` as a parent the bytes never name. Read off
// the decode, the chain would judge such a thread path-free where the readers
// start elsewhere, which is a false hold.
//
// WHAT IS HELD IS OF A FIXED SIZE, whatever the line: its length, its first
// [lineHeadBytes] bytes, one bit for the key the dispatch takes a slot by,
// the 36 bytes the readers key it by, and one bit a mark ([byteMark]).
//
// READ OFF THE WHOLE LINE ([wholeLineBytes]), by the functions each fact was
// read by where its reader read the line ([slotKeyed], [slotKeyAt],
// [depthOneOf], and bytes.Contains for a mark).

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
// depth one, outside every string, as the dispatch takes a slot by
// ([slotKeyed]).
func (b *threadBytes) SlotKeyed() bool { return b.slotKeyed }

// KeyedBy reports whether the uuid every large-file reader keys the line by
// is id, byte for byte ([slotKeyAt]): false where they key it by none.
func (b *threadBytes) KeyedBy(id string) bool { return b.keyed && string(b.key[:]) == id }

// Has reports whether the line holds every mark of m, anywhere in its bytes.
func (b *threadBytes) Has(m byteMark) bool { return b.marks&m == m }

// wholeLineBytes is what a reader of its bytes reads of raw, a whole line, its
// break dropped.
func wholeLineBytes(raw []byte) threadBytes {
	b := threadBytes{n: int64(len(raw)), headLen: uint8(min(len(raw), lineHeadBytes)), slotKeyed: slotKeyed(raw)}
	copy(b.head[:], raw)
	if at := slotKeyAt(raw); at >= 0 {
		copy(b.key[:], raw[at:])
		b.keyed = true
	}
	for _, m := range byteMarks {
		if bytes.Contains(raw, m.bytes) {
			b.marks |= m.mark
		}
	}
	return b
}

// slotKeyAt is where, in raw, a record's line, the 36 bytes every large-file
// reader keys it by begin, -1 where they key it by none: the 36 bytes after
// the first `"uuid":"` they follow with `","timestamp":"`, of several such the
// first at the line's object depth one ([depthOneOf]), else the last of them,
// and where none is, after the first `"uuid":"` (2.1.322's pa in ike
// @293793061, keyed alike in F1t @293788207; 2.1.310's vs in HMt @247176207,
// alike in FPt @247171899), none where fewer than 36 bytes follow it. A reader
// reads those bytes as latin1 and compares them as written, so only the same
// bytes are the same uuid. The matches are read as bytes.Index finds them, each
// search resuming past the last match's key: of `"uuid":"uuid":"` the second
// is never one.
func slotKeyAt(raw []byte) int {
	first, good := -1, -1
	var more []int
	for from := 0; ; {
		i := bytes.Index(raw[from:], slotUUIDKey)
		if i < 0 {
			break
		}
		i += from
		if first < 0 {
			first = i
		}
		if after := i + len(slotUUIDKey) + slotKeyLen; after+len(slotTimeAfter) <= len(raw) &&
			bytes.Equal(raw[after:after+len(slotTimeAfter)], slotTimeAfter) {
			if good < 0 {
				good = i
			} else {
				more = append(more, i)
			}
		}
		from = i + len(slotUUIDKey)
	}
	at := first
	switch {
	case more != nil:
		at = depthOneOf(raw, append([]int{good}, more...))
	case good >= 0:
		at = good
	}
	if at < 0 || at+len(slotUUIDKey)+slotKeyLen > len(raw) {
		return -1
	}
	return at + len(slotUUIDKey)
}

// slotKeyed reports whether raw, a whole line, carries `"parentUuid":` at its
// object's depth one, outside every string (2.1.322's N1t @293787634, 2.1.310's
// Ipo @247171334), as a line that begins `{"parentUuid":`, the prune's slot,
// does too. A line whose JSON fails is scanned by its bytes as the readers
// scan it, so its depth is theirs.
func slotKeyed(raw []byte) bool {
	if !bytes.Contains(raw, slotParentKey) {
		return false
	}
	var s jsonDepth
	for i, b := range raw {
		if s.one() && b == '"' && bytes.HasPrefix(raw[i:], slotParentKey) {
			return true
		}
		s.step(b)
	}
	return false
}

// depthOneOf is the first of at, offsets into raw in their order, that lies
// at the line's object depth one outside every string, else the last of them
// (2.1.322's dzt @293787942, 2.1.310's $Mt @247171638).
func depthOneOf(raw []byte, at []int) int {
	var s jsonDepth
	for i, k := 0, 0; k < len(at); i++ {
		if i == at[k] {
			if s.one() {
				return at[k]
			}
			k++
		}
		s.step(raw[i])
	}
	return at[len(at)-1]
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
