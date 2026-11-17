package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/crewlet/crewlet/internal/jsonscan"
)

// A THREAD LINE READ IN ONE SCANNER PASS, NEVER HELD.
//
// The reader here ([scanReader]) reads a member of a Claude Code session a
// line at a time as [threadReader] does, and answers each line as it does: a
// record with its head, its byte facts and its facts ([threadLine]), a
// [*threadLineError] classed as Claude Code's load reads the line, or what
// ended the reading. It reads each line through [jsonscan] in ONE PASS that
// never holds the line, where [threadReader] holds the whole line and
// decodes it ([decodeThreadLine]), so a line of any length costs what its
// readers read of it: Claude Code keeps no bound on a line (a whole-PDF Read
// writes 55,924,056 bytes on one). Nothing calls it yet: it is held to the
// whole decode, line by line, by the differential in
// claudelinescan_internal_test.go, before it takes that reader's place.
//
// A LINE IS ITS OWN READER ([lineSource]), which ends at the line's break and
// hands the scanner the line without it, so the scanner reads one JSON text
// a line: its break is no blank of the value, and a value cut at the break
// is a value cut at the text's end. Every byte the source hands the scanner,
// and every byte it reads past a fault to reach the break, passes first
// through the member's meter and through the line's byte facts
// ([lineBytes]), a tee of the raw bytes, never of the tokens: a pattern a
// reader of bytes reads can begin inside a name, through an escaped quote,
// which no token says ([threadBytes]). The context of the Next in progress is
// asked before each piece the source takes of the member, at most
// [lineChunkBytes], so a reading overruns its caller's wait by one piece,
// whatever the line's length.
//
// NOTHING OF A LINE COMMITS BEFORE ITS BREAK AND THE SCANNER'S END OF IT
// (io.EOF, or the fault that ends it): what the pass reads lives in the
// line's fresh facts and the reader's own scratch until then, and a piece
// the member ends with no break commits nothing whatever the scanner made of
// it. Then the line is judged in the order Claude Code's load reads it
// ([scanReader.commit]): a fault of the grammar first, wherever in the line
// it lies, since the whole decode reads the whole value before it reads the
// head; then one value that is no object; then the head; then the bounds
// the reading holds a line to; and only then a record. So a fault late in a
// line beats what the line said before it, and a line past a bound is still
// classed exactly where its grammar or its head refuses it: past a bound the
// pass keeps scanning, holding nothing more.
//
// EVERY MEMBER IS READ BY ITS DECODED NAME, exactly, case kept, as the whole
// decode reads a key under [threadDecoding]: `"\u0074ype"` is `type`, and
// `"Type"` is no key the layer reads. A key written twice is read by its
// last value, wholesale: each occurrence of a key the pass keeps replaces
// what the one before it kept.
//
// A VALUE IS KEPT, OR PASSED OVER UNHELD. Of the record itself, the head's
// keys and the small values its accessors decode by the layer's own code
// (a uuid, a timestamp, an origin) are captured as written, within
// [threadValueMax] each, the bytes a json.RawMessage of the whole decode
// holds ([jsonscan.Scanner.Capture]); every other member is passed over,
// validated and held nowhere ([jsonscan.Scanner.Skip]). A value it keeps
// that passes its bound fails the line closed, never read as a guess.

// threadValueMax is the most of one value the layer reads whole: a raw value
// it keeps for an accessor to decode (a uuid, a timestamp, a reply's usage,
// an attachment's type), or one it decides as its object ends (a call's id, a
// tool's name), as written. PAST IT, THE LINE FAILS CLOSED
// ([errThreadBound]): a reader never reads a guess, and a line no release
// writes costs its reading, never a false hold.
//
// 64 KiB: the scanner's own read buffer, so a capture holds a reader at most
// twice that for its life ([jsonscan.Scanner.Capture]), and 160 times the
// longest such value of any probe thread, a reply's usage of 402 bytes (the
// next, a compaction's preserved segment, 153). What a reader reads of a
// longer value it reads streamed ([threadMessage.Says], a result's digests
// [threadResultText]) or by its prefix (a retraction's,
// [threadLine.Retracted]), and those have no such bound.
const threadValueMax = 64 << 10

// errLineMember is a line that runs past what is left of its member's bound
// ([lineSource]): the source's own, set by it alone, so no error of a
// member's reader, one that wraps the take's scrub's bound among them, can
// pass for it.
var errLineMember = errors.New("executor: a thread line runs past what is left of its member's bound")

// scanReader reads one member of a Claude Code session a line at a time, each
// line read in one scanner pass into its head, its byte facts and its facts,
// never held ([scanReader.Next]): the reader [threadReader] becomes. One
// goroutine reads it. ONE READER READS A SESSION'S MEMBERS, one after another
// ([scanReader.Reset]), keeping its buffers.
type scanReader struct {
	r *bufio.Reader
	// maxBytes is the member's bound, and left what of it the lines read so
	// far have not taken.
	maxBytes, left int64
	// n is how many whole lines have been read.
	n int
	// torn is whether the member ended in a piece with no break.
	torn bool
	// end is what ended the reading: io.EOF, errThreadBound, ctx's error, or
	// an error of r. Every Next answers it from then on.
	end error
	// src is the line being read, and scan the scanner that reads it, reset
	// to each line and kept across lines and members.
	src  lineSource
	scan *jsonscan.Scanner
	// dec is the pass's own scratch, kept across lines and members.
	dec lineDecoder
	// valueMax is [threadValueMax]; the differential raises it to compare the
	// two readings' facts at any length (claudelinescan_internal_test.go).
	valueMax int
}

// newScanReader reads r, one member, a line at a time, as [newThreadReader]
// does: at most maxBytes of it; at or below zero, no byte.
func newScanReader(r io.Reader, maxBytes int64) *scanReader {
	return newScanReaderSize(r, maxBytes, lineChunkBytes)
}

// newScanReaderSize is [newScanReader] taking the member chunk bytes at a
// time at most: what a test reads with to move the edge of what the scanner
// is handed through every byte of a short line, since a member's reader alone
// cannot (bufio fills its buffer to a break or to its end whatever pieces the
// member hands it).
func newScanReaderSize(r io.Reader, maxBytes int64, chunk int) *scanReader {
	// Built over no reader and then reset onto r: bufio.NewReaderSize hands
	// back r itself where r is a *bufio.Reader of the size, and a later Reset
	// would then reset the caller's reader.
	t := &scanReader{r: bufio.NewReaderSize(nil, chunk), valueMax: threadValueMax}
	t.scan = jsonscan.New(&t.src)
	t.Reset(r, maxBytes)
	return t
}

// Reset makes t read r, one member, within maxBytes, as [newScanReader] does:
// whatever t read before, and how it ended, is gone, and t reads r exactly as
// a fresh reader would, keeping only its buffers.
func (t *scanReader) Reset(r io.Reader, maxBytes int64) {
	maxBytes = max(maxBytes, 0)
	t.r.Reset(r)
	t.maxBytes, t.left = maxBytes, maxBytes
	t.n, t.torn, t.end = 0, false, nil
}

// Next is the member's next WHOLE line, one that ends in "\n", read without
// its break in one scanner pass. It answers:
//
//   - io.EOF at the member's end. A last piece with no break is never a line,
//     whatever the scanner made of it ([threadReader.Next] says why);
//     [scanReader.Torn] says it was there.
//   - errThreadBound once the member runs past the reader's bound, EVEN
//     WHERE THE BOUND FALLS RIGHT AFTER A LINE BREAK, or once a value the
//     line keeps runs past [threadValueMax]. The reading is over, and stays
//     over.
//   - a *threadLineError for a whole line that is not one record, classed as
//     Claude Code's load reads it ([scanReader.commit]): the reader stays
//     usable.
//   - any error of the member's reader, AS IT IS, which ends the reading.
//   - ctx's error once ctx has ended, asked before each piece of the member
//     the line takes, so a reading overruns its caller's wait by one piece
//     ([lineChunkBytes]) at most, whatever the line's length. The reading is
//     over, and stays over, whatever context a later Next is handed.
func (t *scanReader) Next(ctx context.Context) (threadLine, error) {
	if t.end != nil {
		return threadLine{}, t.end
	}
	t.src.reset(ctx, t.r, t.left)
	facts := new(lineFacts)
	t.scan.Reset(&t.src)
	t.dec.reset(t.valueMax)
	scanned := t.dec.line(t.scan, facts)
	// A FAULT STOPS THE SCANNER BEFORE THE BREAK: the rest of the line is read
	// through the meter and the tee, holding nothing, so a refused line's
	// size and byte facts are the whole line's.
	if !t.src.broke && t.src.cause == nil {
		t.src.drain()
	}
	t.src.ctx = nil
	switch cause := t.src.cause; {
	case errors.Is(cause, errLineMember):
		t.end = fmt.Errorf("%w: the member runs past the %d bytes it is read within", errThreadBound, t.maxBytes)
		return threadLine{}, t.end
	case errors.Is(cause, io.EOF):
		// No break: nothing of the piece commits.
		t.torn, t.end = t.src.n > 0, io.EOF
		return threadLine{}, io.EOF
	case cause != nil:
		t.end = cause
		return threadLine{}, cause
	}
	t.left -= t.src.n + 1
	t.n++
	return t.commit(scanned, facts)
}

// Torn reports whether the member ended in a piece with no break, which
// [scanReader.Next] read as no line: known once Next has answered io.EOF.
func (t *scanReader) Torn() bool { return t.torn }

// commit is the line just read, whole and ended by its break, scanned what
// the pass answered (nil where the line was one JSON value) and facts what it
// filled, judged IN THE ORDER CLAUDE CODE'S LOAD READS IT, the first that
// holds answering:
//
//  1. A fault of the grammar ([jsonscan.Error]), wherever it lies: a NUL or a
//     byte order mark first is faultLead, any other byte the grammar refuses
//     or a line of blanks alone faultSyntax, a value cut at the break
//     faultPrefix, one nested too deep faultDepth, more after the value
//     faultTrailing. Its Err is errNotOneRecord where the line opened no
//     object, as the whole decode's, else the scanner's own words.
//  2. One value that is no object: faultNotObject, errNotOneRecord.
//  3. A head the head's rule refuses ([headOf]), judged on each key's last
//     occurrence and under the last `type`: faultHead, its Type the kind of
//     record that `type` names ([kindOf]).
//  4. A value the line keeps past [threadValueMax]: errThreadBound, and the
//     reading is over.
//  5. Else the record.
//
// Steps 1 to 3 come before the bound, so a line past it is classed exactly
// where its grammar or its head refuses it.
func (t *scanReader) commit(scanned error, facts *lineFacts) (threadLine, error) {
	size, read := int(t.src.n+1), t.src.bytes.facts()
	refused := func(err error, fault lineFault, kind recordKind) (threadLine, error) {
		return threadLine{}, &threadLineError{N: t.n, Err: err, Fault: fault, Type: kind, Size: size, Bytes: read}
	}
	var fault *jsonscan.Error
	switch {
	case errors.As(scanned, &fault):
		err := errNotOneRecord
		if t.dec.object {
			err = fault
		}
		return refused(err, scanFault(fault), "")
	case scanned != nil:
		// No fault of the line's and no error of the member's, which only a
		// misuse of the scanner gives: the reading is over, never a line
		// classed on a pass that did not read it.
		t.end = scanned
		return threadLine{}, scanned
	case !t.dec.object:
		return refused(errNotOneRecord, faultNotObject, "")
	}
	keys := t.dec.headKeys()
	head, err := headOf(keys)
	if err != nil {
		return refused(err, faultHead, kindOf(keys.typ.raw))
	}
	if t.dec.over != "" {
		t.end = fmt.Errorf("%w: line %d's %s runs past the %d bytes a value is read whole within", errThreadBound,
			t.n, t.dec.over, t.valueMax)
		return threadLine{}, t.end
	}
	// The parent as written is the line's own: the pass's buffer is the next
	// line's.
	if keys.parent.kind == '"' {
		head.Parent = slices.Clone(head.Parent)
	}
	return threadLine{N: t.n, Size: size, Bytes: read, Head: head, f: facts}, nil
}

// scanFault is how Claude Code's load reads a line the scanner refused with
// e ([lineFault]): what [refusedFault] reads of the same line.
func scanFault(e *jsonscan.Error) lineFault {
	switch e.Fault {
	case jsonscan.FaultByte:
		if e.Offset == 0 && (e.Rune == 0 || e.Rune == '\uFEFF') {
			return faultLead
		}
		return faultSyntax
	case jsonscan.FaultNoValue:
		return faultSyntax
	case jsonscan.FaultCut:
		return faultPrefix
	case jsonscan.FaultDepth:
		return faultDepth
	case jsonscan.FaultTrailing:
		return faultTrailing
	}
	return ""
}

// lineSource is one line of the member, its break kept back: the reader the
// scanner reads the line through. Every byte it takes of the member it
// counts against what is left of the member's bound and writes to the line's
// byte facts ONCE, as it takes it, and then hands it over, or passes it over
// ([lineSource.drain]).
type lineSource struct {
	// ctx is the Next in progress's; nil between calls.
	ctx context.Context
	r   *bufio.Reader
	// left is what of the member's bound the line may take, its break among
	// it.
	left int64
	// pending is what the member handed and the scanner has not taken, in r's
	// buffer.
	pending []byte
	// n is how many bytes of the line were taken so far, handed or passed
	// over, its break not among them.
	n int64
	// broke is whether the line ended at its break.
	broke bool
	// cause is what ended the line otherwise: io.EOF (the member's end),
	// errLineMember, ctx's error, or the member's reader's own error.
	cause error
	// bytes is the line's byte facts, read as its bytes are taken.
	bytes lineBytes
}

// reset makes s the next line of the member r reads, which may take left
// bytes of it, read under ctx.
func (s *lineSource) reset(ctx context.Context, r *bufio.Reader, left int64) {
	s.ctx, s.r, s.left = ctx, r, left
	s.pending, s.n, s.broke, s.cause = nil, 0, false, nil
	s.bytes.reset()
}

// Read reads the line's next bytes into p, up to its break: io.EOF once the
// line has ended at its break, and what ended it otherwise once it has. It
// never answers no bytes with no error.
func (s *lineSource) Read(p []byte) (int, error) {
	for len(s.pending) == 0 {
		switch {
		case s.broke:
			return 0, io.EOF
		case s.cause != nil:
			return 0, s.cause
		}
		s.take()
	}
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}

// drain takes the rest of the line, through the meter and the tee, handing it
// to no one: what a line holds past the fault that stopped its scanner.
func (s *lineSource) drain() {
	for s.pending = nil; !s.broke && s.cause == nil; s.pending = nil {
		s.take()
	}
}

// take takes the line's next piece of the member into pending: ctx asked
// first; then THE MEMBER'S METER, before the piece's own error, as the line
// primitive meters a line ([readLine]), so a member past its bound in the
// same piece as its reader's failure ends with the bound, and the bound holds
// even right after a break; then the piece, but its break, counted and
// written to the byte facts.
func (s *lineSource) take() {
	if err := s.ctx.Err(); err != nil {
		s.cause = err
		return
	}
	chunk, err := s.r.ReadSlice('\n')
	if s.n+int64(len(chunk)) > s.left {
		s.cause = errLineMember
		return
	}
	if err == nil {
		// ReadSlice answers no error only for a piece its break ends.
		chunk, s.broke = chunk[:len(chunk)-1], true
	}
	s.bytes.write(chunk)
	s.n += int64(len(chunk))
	s.pending = chunk
	switch {
	case err == nil, errors.Is(err, bufio.ErrBufferFull):
	case errors.Is(err, io.EOF):
		// A wrapped io.EOF is the member's end too, as [threadSource] reads
		// it.
		s.cause = io.EOF
	default:
		s.cause = err
	}
}

// lineNameBytes is the most of a member's decoded name the pass reads: more
// than the longest name a frame reads, `sourceToolAssistantUUID` (23 bytes),
// so a name that fills it is no name a frame reads.
const lineNameBytes = 32

// The head's keys, by their place in [lineDecoder.head] and [headNames].
const (
	headType = iota
	headSubtype
	headAgentID
	headParent
	headSidechain
	headReplacesSpan
	headKeyCount
)

// headNames is each head key's name, by its place.
var headNames = [headKeyCount]string{"type", "subtype", "agentId", "parentUuid", "isSidechain", "replacesSpan"}

// recordRaws is each small value of the record its accessors decode by the
// layer's own code, by its name, and where the line's facts keep it as
// written ([lineFacts]).
var recordRaws = [...]struct {
	name string
	at   func(f *lineFacts) *json.RawMessage
}{
	{"uuid", func(f *lineFacts) *json.RawMessage { return &f.uuid }},
	{"version", func(f *lineFacts) *json.RawMessage { return &f.version }},
	{"origin", func(f *lineFacts) *json.RawMessage { return &f.origin }},
	{"isApiErrorMessage", func(f *lineFacts) *json.RawMessage { return &f.apiError }},
	{"leafUuid", func(f *lineFacts) *json.RawMessage { return &f.leafUUID }},
	{"explicit", func(f *lineFacts) *json.RawMessage { return &f.explicit }},
	{"rewound", func(f *lineFacts) *json.RawMessage { return &f.rewound }},
	{"isVirtual", func(f *lineFacts) *json.RawMessage { return &f.isVirtual }},
	{"interruptedByShutdown", func(f *lineFacts) *json.RawMessage { return &f.interruptedByShutdown }},
	{"isCompactSummary", func(f *lineFacts) *json.RawMessage { return &f.isCompactSummary }},
	{"timestamp", func(f *lineFacts) *json.RawMessage { return &f.timestamp }},
	{"isMeta", func(f *lineFacts) *json.RawMessage { return &f.isMeta }},
	{"sourceToolAssistantUUID", func(f *lineFacts) *json.RawMessage { return &f.sourceToolAssistantUUID }},
}

// lineDecoder is the one pass's own state ([lineDecoder.line]): what it reads
// of a line before the line commits, in buffers a reader keeps across lines.
type lineDecoder struct {
	// valueMax is the most of a value the pass captures whole.
	valueMax int
	// name is the current member's decoded name.
	name [lineNameBytes]byte
	// object is whether the line's first token opened an object.
	object bool
	// head is each head key's last occurrence: its kind (0 where absent), and
	// its raw as written where captured.
	head [headKeyCount]scannedKey
	// over is the first value the pass kept that ran past valueMax, by its
	// key; "" where none did.
	over string
}

// scannedKey is one key's last occurrence as the pass read it: kind its
// value's kind, 0 where the key is absent; raw its value as written, in a
// buffer the reader keeps, where kept.
type scannedKey struct {
	kind valueKind
	raw  []byte
	kept bool
}

// reset makes d a pass over a line not yet read, capturing at most valueMax
// bytes of a value.
func (d *lineDecoder) reset(valueMax int) {
	d.valueMax, d.object, d.over = valueMax, false, ""
	for i := range d.head {
		d.head[i] = scannedKey{raw: d.head[i].raw[:0]}
	}
}

// headKeys is the head's keys as the line wrote them last ([headOf]): a key
// past [threadValueMax] by its kind alone.
func (d *lineDecoder) headKeys() headKeys {
	key := func(i int) headKey {
		k := headKey{kind: d.head[i].kind}
		if d.head[i].kept {
			k.raw = d.head[i].raw
		}
		return k
	}
	return headKeys{typ: key(headType), subtype: key(headSubtype), agentID: key(headAgentID), parent: key(headParent),
		sidechain: key(headSidechain), replacesSpan: key(headReplacesSpan)}
}

// line reads one line, the scanner's whole text, into f and d: its value,
// then the scanner's io.EOF or the fault that ends the text. It answers nil
// where the text is one JSON value, else what the scanner answered, the
// first fault met wherever it lies.
func (d *lineDecoder) line(scan *jsonscan.Scanner, f *lineFacts) error {
	k, err := scan.Next()
	if err != nil {
		return err
	}
	if k == jsonscan.ObjectStart {
		d.object = true
		err = d.record(scan, f)
	} else {
		err = scan.Skip()
	}
	if err != nil {
		return err
	}
	// THE TEXT'S END, OR MORE AFTER THE VALUE: the scanner says which only
	// once the source has said where the line ends.
	if _, err = scan.Next(); errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

// record reads the record's object, its opening brace read, into f and d,
// member by member: a head key captured into d, a small value its accessors
// decode captured into f, taskDelivery's presence, and every other member
// passed over unheld.
func (d *lineDecoder) record(scan *jsonscan.Scanner, f *lineFacts) error {
	for {
		k, err := scan.Next()
		if err != nil || k == jsonscan.ObjectEnd {
			return err
		}
		name := d.readName(scan)
		if k, err = scan.Next(); err != nil {
			return err
		}
		if err = d.member(scan, k, name, f); err != nil {
			return err
		}
	}
}

// member reads the record's member name, whose value's first token, k, the
// scanner stands at.
func (d *lineDecoder) member(scan *jsonscan.Scanner, k jsonscan.Kind, name []byte, f *lineFacts) error {
	for i, key := range headNames {
		if string(name) == key {
			raw, kind, err := d.capture(scan, k, key)
			d.head[i] = scannedKey{kind: kind, raw: append(d.head[i].raw[:0], raw...), kept: raw != nil}
			return err
		}
	}
	for _, kept := range recordRaws {
		if string(name) == kept.name {
			raw, _, err := d.capture(scan, k, kept.name)
			var held json.RawMessage
			if raw != nil {
				held = append(held, raw...)
			}
			*kept.at(f) = held
			return err
		}
	}
	if string(name) == "taskDelivery" {
		f.taskDelivery = true
	}
	return scan.Skip()
}

// readName is the decoded text of the name the scanner stands at, in d's
// buffer: nil where it runs to the buffer's length, so no name a frame reads,
// or where reading it failed, which the scanner's next call answers.
func (d *lineDecoder) readName(scan *jsonscan.Scanner) []byte {
	n := 0
	for n < len(d.name) {
		m, err := scan.Read(d.name[n:])
		n += m
		switch {
		case errors.Is(err, io.EOF):
			return d.name[:n]
		case err != nil:
			// io.ErrShortBuffer, the next character outgrowing what is left
			// of the buffer, is a name too long for any frame too.
			return nil
		}
	}
	return nil
}

// capture is the value the scanner stands at, of kind k, the value of key:
// its bytes as written and its kind, valid until the scanner's next call;
// and, where it runs past d's bound, no bytes, its kind as k says it, and the
// line marked past its bound ([lineDecoder.over]).
func (d *lineDecoder) capture(scan *jsonscan.Scanner, k jsonscan.Kind, key string) ([]byte, valueKind, error) {
	raw, whole, err := scan.Capture(d.valueMax)
	switch {
	case err != nil:
		return nil, 0, err
	case !whole:
		if d.over == "" {
			d.over = key
		}
		return nil, scannedKind(k), nil
	}
	return raw, kindOfValue(raw), nil
}

// scannedKind is the kind of a value whose first token is of kind k, as its
// first byte tells it ([valueKind]): a number's as '0', which a value's kind
// reads as any number's.
func scannedKind(k jsonscan.Kind) valueKind {
	switch k {
	case jsonscan.ObjectStart:
		return '{'
	case jsonscan.ArrayStart:
		return '['
	case jsonscan.String:
		return '"'
	case jsonscan.True:
		return 't'
	case jsonscan.False:
		return 'f'
	case jsonscan.Null:
		return 'n'
	}
	return '0'
}
