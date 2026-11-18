package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"unicode/utf8"
	"unsafe"

	"github.com/crewlet/crewlet/internal/jsonscan"
)

// A THREAD LINE READ IN ONE SCANNER PASS, NEVER HELD: [scanReader] answers
// each line as [threadReader] does, through [jsonscan], without holding the
// line, so a line of any length costs what its readers read of it. Why a tee
// of the raw bytes, why nothing commits before the break, the order a line is
// judged in, names by their decoded text and last occurrence, the three means
// a value is read by, the type's gate and the meter: the package doc, "A
// thread line is read in one scanner pass, never held".

// threadValueMax is the most of one value the layer reads whole: a raw value
// it keeps for an accessor to decode (a uuid, a timestamp, a reply's usage,
// an attachment's type), or one it decides as its object ends (a call's id, a
// tool's name), as written. PAST IT, THE LINE FAILS CLOSED
// ([errThreadBound]): a reader never reads a guess, and a line no release
// writes costs its reading, never a false hold. 64 KiB: the scanner's own read
// buffer, and 160 times the longest such value of any probe thread, a reply's
// usage of 402 bytes. A longer value is read streamed ([threadMessage.Says],
// [threadResultText]) or by its prefix ([threadLine.Retracted]), unbounded.
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
	// valueMax is [threadValueMax] and meterMax [threadLineMax]; the
	// differential raises both to compare the two readings' facts at any
	// length (claudelinescan_internal_test.go).
	valueMax, meterMax int
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
	t := &scanReader{r: bufio.NewReaderSize(nil, chunk), valueMax: threadValueMax, meterMax: threadLineMax}
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

// Next is the member's next WHOLE line, read without its break in one
// scanner pass, answered as [threadReader.Next] answers it: io.EOF at the
// member's end, a last piece with no break never a line ([scanReader.Torn]);
// a *threadLineError for a whole line that is no record, the reader still
// usable; and, each ending the reading for good, errThreadBound once the
// member runs past its bound (EVEN RIGHT AFTER A BREAK), once the pass captures
// a value past [threadValueMax] (a replaced occurrence or a frame the final
// type drops among them) or the facts run past [threadLineMax], or once the
// line's type reads a frame passed over for an earlier type; the member's
// reader's error as it is; and ctx's error, asked before each piece the line
// takes, so a reading overruns its caller's wait by one [lineChunkBytes].
func (t *scanReader) Next(ctx context.Context) (threadLine, error) {
	if t.end != nil {
		return threadLine{}, t.end
	}
	t.src.reset(ctx, t.r, t.left)
	facts := new(lineFacts)
	t.scan.Reset(&t.src)
	t.dec.reset(t.valueMax, t.meterMax)
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
// filled, judged IN THE ORDER CLAUDE CODE'S LOAD READS IT (the package doc
// says why), the first that holds answering: a fault of the grammar wherever
// it lies ([scanFault]), its Err errNotOneRecord where the line opened no
// object, else the scanner's words; one value that is no object; a head the
// head's rule refuses ([headOf]) under each key's last occurrence, its Type
// the kind the last `type` names ([kindOf]); a value past [threadValueMax],
// facts past [threadLineMax] or a frame passed over for an earlier type
// ([lineDecoder.passedOver]), errThreadBound, ending the reading; else the
// record, each frame settled to its type ([settle]).
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
	// THE HEAD'S STRINGS ARE THE LINE'S TOO, decoded by its rule just now,
	// and its parent as written is copied for the line below.
	charge := len(head.Type) + len(head.Subtype) + len(head.AgentID)
	if keys.parent.kind == '"' {
		charge += len(head.Parent)
	}
	t.dec.meter.take(charge)
	switch {
	case t.dec.over != "":
		t.end = fmt.Errorf("%w: line %d's %s runs past the %d bytes a value is read whole within", errThreadBound,
			t.n, t.dec.over, t.valueMax)
		return threadLine{}, t.end
	case t.dec.meter.over:
		t.end = fmt.Errorf("%w: line %d's facts run past the %d bytes a line's facts are held within",
			errThreadBound, t.n, t.meterMax)
		return threadLine{}, t.end
	}
	if frame := t.dec.passedOver(head); frame != "" {
		t.end = fmt.Errorf("%w: line %d's %s came before a later type made it a record that reads it",
			errThreadBound, t.n, frame)
		return threadLine{}, t.end
	}
	settle(facts, head)
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
// line has ended at its break, and what ended it otherwise once it has. For a
// p of at least one byte it never answers no bytes with no error.
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

// lineMeter is what one line's facts may still take of [threadLineMax], a
// bound in sum (the package doc says why): left, the bytes the pass may still
// keep for them; over, whether a charge found too few left, after which every
// charge is refused and the pass holds nothing more of the line. EVERY
// ALLOCATION THE FACTS KEEP IS CHARGED as it is made: a kept raw or string by
// its length (a string right after its decode, so the meter passes its bound
// by one value's decode at most), an array by its capacity times its
// element's size, before it is made ([grow]). NOTHING IS REFUNDED, so the
// meter bounds what the line's facts keep, every copy of a value counted. Not
// charged: the facts' struct, the reader's buffers, and what the pass makes
// and drops (a refused block's strings, a frame's error).
type lineMeter struct {
	left int
	over bool
}

// take charges n bytes, reporting whether they were left: false, and the
// meter over, where they were not, or where it was over before.
func (m *lineMeter) take(n int) bool {
	if m.over || n > m.left {
		m.over = true
		return false
	}
	m.left -= n
	return true
}

// grow is s with room for one element more: s itself where it has room, else
// a copy of it in a new array of twice its capacity, or one, but of no more
// than most elements and no more than the meter has left, the whole array
// charged before it is made. ok false, s as it is and the meter over, where
// not one element more is left.
func grow[S ~[]E, E any](m *lineMeter, s S, most int) (S, bool) {
	if len(s) < cap(s) {
		return s, true
	}
	size := int(unsafe.Sizeof(*new(E)))
	n := min(max(2*cap(s), 1), most, m.left/size)
	if n <= len(s) || !m.take(n*size) {
		m.over = true
		return s, false
	}
	grown := make(S, len(s), n)
	copy(grown, s)
	return grown, true
}

// lineDecoder is the one pass's own state ([lineDecoder.line]): what it reads
// of a line before the line commits, in buffers a reader keeps across lines.
type lineDecoder struct {
	// valueMax is the most of a value the pass captures whole, and meter what
	// the line's facts may still take ([lineMeter]).
	valueMax int
	meter    lineMeter
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
	// typed is whether the last `type` read so far is a string, and typeText
	// its decoded text, empty where the string ran past valueMax, which names
	// no record ([lineDecoder.admits]).
	typed    bool
	typeText []byte
	// missed is each gated frame whose last occurrence the pass passed over
	// for the type read before it ([lineDecoder.passedOver]).
	missed gatedFrames
	// text is the buffer every streamed text is read through, a piece at a
	// time.
	text [textPieceBytes]byte
	// says, notice and handback read a message's content that is one string
	// ([threadMessage.Says], [threadMessage.Notification],
	// [threadMessage.NamesHandback]).
	says     textSays
	notice   textNotice
	handback textHandback
	// block is the content block being read, and result the text of a
	// tool_result's content being read.
	block  blockScan
	result textResult
	// held is the small values of the payload object being read that the
	// rule deciding it reads once it ends, as written, empty where it lacks
	// them: a toolUseResult's `status`, `agentId` and `success`
	// ([answerOf]); a replacement's `kind` and `toolUseId` ([replacedID]); a
	// batch entry's `id` ([batchEntryID]); a withheld memory entry's `why`
	// ([withheldKept]); a kept list's `anchorUuid`. Objects of no two of
	// them are open at once.
	held [3][]byte
	// open is a list entry's opening ([lineDecoder.opening]): the most of an
	// entry any reader reads, a uuid's 36 bytes, one more, and room for one
	// character more, which a read never splits.
	open [uuidTextBytes + 1 + utf8.UTFMax]byte
}

// uuidTextBytes is a canonical uuid's length as written ([uuidBytes]), and
// retractionPrefixBytes the characters of a retraction's entry the chain
// reads ([chainPrefix]).
const (
	uuidTextBytes         = 36
	retractionPrefixBytes = 24
)

// The frames a record's type gates ([gates]), each a member of a
// gatedFrames.
type gatedFrames uint8

const (
	gateMessage gatedFrames = 1 << iota
	gateToolUseResult
	gateAttachment
	gateCompacted
	gateRetracted
	gateReplacements
	gateBatch
)

// gates is each frame a record's type gates ([lineDecoder.admits]): its key,
// the types of record whose accessors read it, and on a system record the
// one subtype they read it on, "" where they read it on every subtype; and
// how the pass reads it into a line's facts. In this order the commit names
// the first a line's type reads that the pass passed over
// ([lineDecoder.passedOver]).
var gates = [...]struct {
	frame   gatedFrames
	key     string
	types   []string
	subtype string
	read    func(d *lineDecoder, scan *jsonscan.Scanner, k jsonscan.Kind, f *lineFacts) error
}{
	{gateMessage, "message", []string{recordUser, recordAssistant}, "",
		func(d *lineDecoder, scan *jsonscan.Scanner, k jsonscan.Kind, f *lineFacts) error {
			return d.message(scan, k, &f.message)
		}},
	{gateToolUseResult, "toolUseResult", []string{recordUser}, "",
		func(d *lineDecoder, scan *jsonscan.Scanner, k jsonscan.Kind, f *lineFacts) error {
			return d.toolResult(scan, k, &f.result)
		}},
	{gateAttachment, "attachment", []string{recordAttachment}, "",
		func(d *lineDecoder, scan *jsonscan.Scanner, k jsonscan.Kind, f *lineFacts) error {
			return d.attachment(scan, k, &f.attachment)
		}},
	{gateCompacted, "compactMetadata", []string{recordSystem}, subtypeCompactBoundary,
		func(d *lineDecoder, scan *jsonscan.Scanner, k jsonscan.Kind, f *lineFacts) error {
			return d.compacted(scan, k, &f.compacted)
		}},
	{gateRetracted, "retractedMessageUuids", []string{recordUser, recordAssistant, recordSystem},
		subtypeRefusalFallback, func(d *lineDecoder, scan *jsonscan.Scanner, k jsonscan.Kind, f *lineFacts) error {
			return d.retracted(scan, k, f)
		}},
	{gateReplacements, "replacements", []string{recordContentReplacement}, "",
		func(d *lineDecoder, scan *jsonscan.Scanner, k jsonscan.Kind, f *lineFacts) error {
			return d.replacements(scan, k, &f.replacements)
		}},
	{gateBatch, "batchToolUses", []string{recordAssistant}, "",
		func(d *lineDecoder, scan *jsonscan.Scanner, k jsonscan.Kind, f *lineFacts) error {
			return d.batch(scan, k, &f.batch)
		}},
}

// drop empties f's facts of frame, as a line that never wrote its key holds
// them.
func (f *lineFacts) drop(frame gatedFrames) {
	switch frame {
	case gateMessage:
		f.message = messageFacts{}
	case gateToolUseResult:
		f.result = toolResultFacts{}
	case gateAttachment:
		f.attachment = attachmentFacts{}
	case gateCompacted:
		f.compacted = compactFacts{}
	case gateRetracted:
		f.retracted, f.retractedErr = threadRetracted{}, nil
	case gateReplacements:
		f.replacements = replacementFacts{}
	case gateBatch:
		f.batch = batchFacts{}
	}
}

// blockScan is the content block being read, until it ends, where the one
// rule of a block decides it from what it holds ([blockCallOf]): each key the
// rule reads, as written, in a buffer the reader keeps across blocks, empty
// where the block lacks the key; its input's kind and that input's `to`;
// whether its `text` says anything; and its content's text.
type blockScan struct {
	typ, id, name, toolUseID, isError []byte
	input                             valueKind
	to                                []byte
	says                              textSays
	text                              threadResultText
	// resultType is the `type` of its content's first element, as written.
	resultType []byte
}

// reset makes b a block not yet read, its buffers kept.
func (b *blockScan) reset() {
	b.typ, b.id, b.name, b.toolUseID, b.isError = b.typ[:0], b.id[:0], b.name[:0], b.toolUseID[:0], b.isError[:0]
	b.input, b.to, b.says, b.text = 0, b.to[:0], textSays{}, threadResultText{}
}

// keys is what the rule of a block reads of b ([blockKeys]), its keys the
// buffers b holds, valid until b reads its next block.
func (b *blockScan) keys() blockKeys {
	return blockKeys{typ: present(b.typ), id: present(b.id), name: present(b.name), toolUseID: present(b.toolUseID),
		isError: present(b.isError), to: sendTargetOf(b.input, present(b.to)), text: b.text}
}

// present is raw, a value as written, or nil, an absent key, where it holds
// no byte: no value as written is empty.
func present(raw []byte) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	return raw
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
// bytes of a value and holding at most meterMax bytes of facts.
func (d *lineDecoder) reset(valueMax, meterMax int) {
	d.valueMax, d.meter, d.object, d.over = valueMax, lineMeter{left: meterMax}, false, ""
	for i := range d.head {
		d.head[i] = scannedKey{raw: d.head[i].raw[:0]}
	}
	d.typed, d.typeText, d.missed = false, d.typeText[:0], 0
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

// admits reports whether a gated frame read on records of types alone is read
// where the pass stands: unless the last `type` read so far is a string
// naming none of them. Before any type, or after one that is no string, the
// frame is read, since the record's last type may still read it.
func (d *lineDecoder) admits(types []string) bool {
	if !d.typed {
		return true
	}
	for _, t := range types {
		if string(d.typeText) == t {
			return true
		}
	}
	return false
}

// passedOver is the key of the first frame ([gates]) a record of head's type
// reads that the pass passed over for the type the line wrote before it
// ([lineDecoder.admits]); "" where none. Judged by the type alone, as the
// pass judged it: a frame a system record reads on one subtype is missed on
// a system record of any.
func (d *lineDecoder) passedOver(head threadHead) string {
	for _, g := range gates {
		if d.missed&g.frame != 0 && slices.Contains(g.types, head.Type) {
			return g.key
		}
	}
	return ""
}

// settle is f, the facts of a record of head, as its accessors read them: a
// frame its type, or on a system record its subtype, does not read emptied
// ([gates]), so a record of another type holds nothing of it, as the whole
// decode reads none ([factsOf]); and of a message, the calls of the kind the
// record makes alone, a user's results or an assistant's uses, in order.
func settle(f *lineFacts, head threadHead) {
	for _, g := range gates {
		reads := slices.Contains(g.types, head.Type) &&
			(head.Type != recordSystem || g.subtype == "" || head.Subtype == g.subtype)
		if !reads {
			f.drop(g.frame)
		}
	}
	switch head.Type {
	case recordUser:
		f.message.content.calls = callsOf(f.message.content.calls, blockToolResult)
	case recordAssistant:
		f.message.content.calls = callsOf(f.message.content.calls, blockToolUse)
	}
}

// callsOf is the calls of calls of kind, in order, in calls' own array.
func callsOf(calls []threadCall, kind string) []threadCall {
	kept := calls[:0]
	for _, c := range calls {
		if c.Kind == kind {
			kept = append(kept, c)
		}
	}
	clear(calls[len(kept):])
	if len(kept) == 0 {
		return nil
	}
	return kept
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
		err = d.members(scan, func(name []byte, k jsonscan.Kind) error { return d.member(scan, k, name, f) })
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

// members reads the object whose opening brace the scanner stands at, member
// by member, to its closing brace: each member's decoded name ([readName])
// and its value's first token handed to each, which reads the value, the
// name valid only until each reads anything more.
func (d *lineDecoder) members(scan *jsonscan.Scanner, each func(name []byte, k jsonscan.Kind) error) error {
	for {
		k, err := scan.Next()
		if err != nil || k == jsonscan.ObjectEnd {
			return err
		}
		name := d.readName(scan)
		if k, err = scan.Next(); err != nil {
			return err
		}
		if err = each(name, k); err != nil {
			return err
		}
	}
}

// member reads the record's member name, whose value's first token, k, the
// scanner stands at: a head key captured into d, a small value its accessors
// decode captured into f, taskDelivery's presence, a frame its type may read
// ([gates]), and every other member passed over unheld.
func (d *lineDecoder) member(scan *jsonscan.Scanner, k jsonscan.Kind, name []byte, f *lineFacts) error {
	for i, key := range headNames {
		if string(name) == key {
			raw, kind, err := d.capture(scan, k, key)
			d.head[i] = scannedKey{kind: kind, raw: append(d.head[i].raw[:0], raw...), kept: raw != nil}
			if i == headType {
				d.sawType(kind, raw)
			}
			return err
		}
	}
	for _, kept := range recordRaws {
		if string(name) == kept.name {
			raw, err := d.keep(scan, k, kept.name)
			*kept.at(f) = raw
			return err
		}
	}
	if string(name) == "taskDelivery" {
		f.taskDelivery = true
		return scan.Skip()
	}
	for _, g := range gates {
		if string(name) != g.key {
			continue
		}
		// WHATEVER AN EARLIER OCCURRENCE LEFT GOES, read or passed over.
		f.drop(g.frame)
		if !d.admits(g.types) {
			d.missed |= g.frame
			return scan.Skip()
		}
		d.missed &^= g.frame
		return g.read(d, scan, k, f)
	}
	return scan.Skip()
}

// sawType reads the record's `type` just captured, of kind and raw as
// written (nil where it ran past the bound), as the type the line has
// written so far ([lineDecoder.admits]).
func (d *lineDecoder) sawType(kind valueKind, raw []byte) {
	d.typed, d.typeText = kind == '"', d.typeText[:0]
	if d.typed && raw != nil {
		// The raw is a whole string the scanner read, so unquoting it meets
		// no error but a lone surrogate's or invalid UTF-8's, each written as
		// U+FFFD, as the head's rule decodes it.
		d.typeText, _ = jsontext.AppendUnquote(d.typeText, raw)
	}
}

// message reads a user or assistant record's `message`, whose first token,
// k, the scanner stands at, into m, as [messageOf] reads it: its kind; of an
// object, its small values as written and its content
// ([lineDecoder.content]), a content it lacks an error of the message's.
func (d *lineDecoder) message(scan *jsonscan.Scanner, k jsonscan.Kind, m *messageFacts) error {
	m.kind = scannedKind(k)
	if k != jsonscan.ObjectStart {
		return scan.Skip()
	}
	content := false
	err := d.members(scan, func(name []byte, k jsonscan.Kind) error {
		var err error
		switch string(name) {
		case "content":
			content = true
			return d.content(scan, k, m)
		case "id":
			m.id, err = d.keep(scan, k, "message.id")
		case "model":
			m.model, err = d.keep(scan, k, "message.model")
		case "stop_reason":
			m.stopReason, err = d.keep(scan, k, "message.stop_reason")
		case "usage":
			m.usage, err = d.keep(scan, k, "message.usage")
		case "context_management":
			m.contextManagement, err = d.keep(scan, k, "message.context_management")
		default:
			err = scan.Skip()
		}
		return err
	})
	if !content {
		m.contentErr = errContentKind(0)
	}
	return err
}

// errContentKind is a message's content of kind, neither a string nor a list
// ([messageContent]).
func errContentKind(kind valueKind) error {
	return fmt.Errorf("content is %s, neither a string nor a list", kind)
}

// content reads a message's `content`, whose first token, k, the scanner
// stands at, into m, as [messageContent] reads it, whatever an earlier
// `content` left: one string, streamed ([lineDecoder.contentText]); a list,
// a block at a time ([lineDecoder.blocks]); anything else the message's
// error.
func (d *lineDecoder) content(scan *jsonscan.Scanner, k jsonscan.Kind, m *messageFacts) error {
	m.content, m.contentErr = threadMessage{}, nil
	switch k {
	case jsonscan.String:
		return d.contentText(scan, &m.content)
	case jsonscan.ArrayStart:
		return d.blocks(scan, m)
	}
	m.contentErr = errContentKind(scannedKind(k))
	return scan.Skip()
}

// contentText reads a message's content that is one string, the scanner at
// it, into c: its text streamed into the three automata its readers ask, a
// piece at a time, until all three have decided, and the rest passed over.
func (d *lineDecoder) contentText(scan *jsonscan.Scanner, c *threadMessage) error {
	d.says, d.notice, d.handback = textSays{}, textNotice{}, textHandback{}
	for !d.says.any || !d.notice.done || !d.handback.named {
		n, err := scan.Read(d.text[:])
		p := d.text[:n]
		d.says.write(p)
		d.notice.write(p)
		d.handback.write(p)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	d.notice.end()
	*c = threadMessage{isText: true, saysAny: d.says.any, namesHandback: d.handback.named}
	if agent, leg, ok := d.notice.notice(); d.meter.take(len(agent) + len(leg)) {
		c.notification = taskNotice{agent: agent, leg: leg, ok: ok}
	}
	return nil
}

// blocks reads a message's content that is a list, its opening bracket the
// scanner's current token, into m, a block at a time: each decided by the one
// rule of a block as it ends ([blockCallOf]), counted by its kind, a call
// kept of either kind (the record's type, which keeps one, may come after
// the message: [settle]), and whether its text says anything; the first
// block the rule refuses the message's error, and every block after it
// passed over, as [messageContent]'s walk stops there.
func (d *lineDecoder) blocks(scan *jsonscan.Scanner, m *messageFacts) error {
	c := &m.content
	for {
		k, err := scan.Next()
		if err != nil || k == jsonscan.ArrayEnd {
			return err
		}
		if m.contentErr != nil {
			if err = scan.Skip(); err != nil {
				return err
			}
			continue
		}
		var (
			call    threadCall
			refused error
		)
		if k == jsonscan.ObjectStart {
			if err = d.readBlock(scan); err != nil {
				return err
			}
			call, refused = blockCallOf('{', d.block.keys())
		} else {
			if err = scan.Skip(); err != nil {
				return err
			}
			call, refused = blockCallOf(scannedKind(k), blockKeys{})
		}
		if refused != nil {
			m.contentErr = fmt.Errorf("content block %d: %w", c.blocks.Len()+1, refused)
			*c = threadMessage{}
			continue
		}
		c.blocks.add(call.Kind)
		if call.Kind == blockToolUse || call.Kind == blockToolResult {
			d.keepCall(c, call)
		}
		// A block the rule took is an object, so what its text said is its
		// own.
		if d.block.says.any {
			c.saysAny = true
			c.saysText = c.saysText || call.Kind == blockText
		}
	}
}

// keepCall keeps call, of either kind, among c's calls, its strings charged
// as the rule just made them and the array it joins before it grows; none
// where the line's facts have no room left ([lineMeter]).
func (d *lineDecoder) keepCall(c *threadMessage, call threadCall) {
	if !d.meter.take(len(call.ID) + len(call.Name) + len(call.To)) {
		return
	}
	var ok bool
	if c.calls, ok = grow(&d.meter, c.calls, math.MaxInt); ok {
		c.calls = append(c.calls, call)
	}
}

// readBlock reads a content block that is an object, its opening brace the
// scanner's current token, into d.block: each key the rule of a block reads
// held as written, its input's `to` ([lineDecoder.input]), its content's text
// ([lineDecoder.resultContent]) and whether its text says anything
// ([lineDecoder.blockText]), each key by its last occurrence. Nothing is
// decided before the block ends: its type may be its last key.
func (d *lineDecoder) readBlock(scan *jsonscan.Scanner) error {
	b := &d.block
	b.reset()
	return d.members(scan, func(name []byte, k jsonscan.Kind) error {
		switch string(name) {
		case "type":
			return d.hold(scan, k, "message.content.type", &b.typ)
		case "id":
			return d.hold(scan, k, "message.content.id", &b.id)
		case "name":
			return d.hold(scan, k, "message.content.name", &b.name)
		case "tool_use_id":
			return d.hold(scan, k, "message.content.tool_use_id", &b.toolUseID)
		case "is_error":
			return d.hold(scan, k, "message.content.is_error", &b.isError)
		case "input":
			return d.input(scan, k)
		case "content":
			return d.resultContent(scan, k)
		case "text":
			return d.blockText(scan, k)
		}
		return scan.Skip()
	})
}

// input reads a block's `input`, whose first token, k, the scanner stands
// at, as [sendTarget] reads it: its kind, and of an object its `to` as
// written, every other key passed over.
func (d *lineDecoder) input(scan *jsonscan.Scanner, k jsonscan.Kind) error {
	b := &d.block
	b.input, b.to = scannedKind(k), b.to[:0]
	if k != jsonscan.ObjectStart {
		return scan.Skip()
	}
	return d.members(scan, func(name []byte, k jsonscan.Kind) error {
		if string(name) == "to" {
			return d.hold(scan, k, "message.content.input.to", &b.to)
		}
		return scan.Skip()
	})
}

// blockText reads a block's `text`, whose first token, k, the scanner stands
// at: whether it is a string that says anything ([textSays]), streamed until
// a character says it, the rest passed over.
func (d *lineDecoder) blockText(scan *jsonscan.Scanner, k jsonscan.Kind) error {
	b := &d.block
	b.says = textSays{}
	if k != jsonscan.String {
		return scan.Skip()
	}
	for !b.says.any {
		n, err := scan.Read(d.text[:])
		b.says.write(d.text[:n])
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// resultContent reads a block's `content`, whose first token, k, the scanner
// stands at, into the block's text as [resultText] reads it: one string,
// streamed whole into its length and digests ([textResult]); a list whose
// one element is an object whose `type` is the string "text" and whose
// `text` is a string, that string streamed whole; no text for anything else,
// a list of two or more elements, or one whose first is no object, among it,
// and every element after the first passed over.
func (d *lineDecoder) resultContent(scan *jsonscan.Scanner, k jsonscan.Kind) error {
	b := &d.block
	b.text = threadResultText{}
	switch k {
	case jsonscan.String:
		if err := d.resultText(scan); err != nil {
			return err
		}
		b.text = d.result.sum()
		return nil
	case jsonscan.ArrayStart:
	default:
		return scan.Skip()
	}
	for at := 0; ; at++ {
		k, err := scan.Next()
		if err != nil || k == jsonscan.ArrayEnd {
			return err
		}
		if at > 0 || k != jsonscan.ObjectStart {
			b.text = threadResultText{}
			if err = scan.Skip(); err != nil {
				return err
			}
			continue
		}
		text := valueKind(0)
		b.resultType = b.resultType[:0]
		err = d.members(scan, func(name []byte, k jsonscan.Kind) error {
			switch string(name) {
			case "type":
				return d.hold(scan, k, "message.content.content.type", &b.resultType)
			case "text":
				text = scannedKind(k)
				if k != jsonscan.String {
					return scan.Skip()
				}
				return d.resultText(scan)
			}
			return scan.Skip()
		})
		if err != nil {
			return err
		}
		if resultElementIsText('{', present(b.resultType), text) {
			b.text = d.result.sum()
		}
	}
}

// resultText reads the string the scanner stands at whole into d.result,
// whatever it read before.
func (d *lineDecoder) resultText(scan *jsonscan.Scanner) error {
	d.result.reset()
	for {
		n, err := scan.Read(d.text[:])
		d.result.write(d.text[:n])
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// toolResult reads a user record's own `toolUseResult`, whose first token,
// k, the scanner stands at, into r, as [toolResultOf] reads it: of an object,
// whether it carries a `handback` or a `handbackReport`, any value, and what
// its `status`, `agentId` and `success`, held as written until it ends, say
// of a subagent's leg ([answerOf]); every other key (a file's base64, a
// command's output, a patch) passed over, and every other kind nothing.
func (d *lineDecoder) toolResult(scan *jsonscan.Scanner, k jsonscan.Kind, r *toolResultFacts) error {
	if k != jsonscan.ObjectStart {
		return scan.Skip()
	}
	status, agent, success := d.clearHeld()
	err := d.members(scan, func(name []byte, k jsonscan.Kind) error {
		switch string(name) {
		case "handback", "handbackReport":
			r.handsBack = true
		case "status":
			return d.hold(scan, k, "toolUseResult.status", status)
		case "agentId":
			return d.hold(scan, k, "toolUseResult.agentId", agent)
		case "success":
			return d.hold(scan, k, "toolUseResult.success", success)
		}
		return scan.Skip()
	})
	if err != nil {
		return err
	}
	r.completed, r.agent, r.refused = answerOf(present(*status), present(*agent), present(*success))
	if !d.meter.take(len(r.agent)) {
		r.agent = ""
	}
	return nil
}

// clearHeld empties d's held values ([lineDecoder.held]) for the object about
// to be read, and hands their buffers, kept.
func (d *lineDecoder) clearHeld() (*[]byte, *[]byte, *[]byte) {
	for i := range d.held {
		d.held[i] = d.held[i][:0]
	}
	return &d.held[0], &d.held[1], &d.held[2]
}

// attachment reads an attachment record's `attachment`, whose first token,
// k, the scanner stands at, into a, as [attachmentOf] reads it: its kind; of
// an object, its `type`, `toolUseID` and `by` kept as written, and what every
// attachment's shape rule asks of its lists, every rule's at once, since its
// type may be its last key ([threadLine.AttachmentWellFormed]): `content`'s
// kind and whether it is a list of strings alone or of objects alone,
// `skills` whether it is a list of objects and lists, `names` the skill
// listing's answer ([lineDecoder.skillNames]), and `entries` whether every
// entry is one a withheld memory keeps ([lineDecoder.withheldEntry]). No
// element of a list is held: a file's content, its whole body, is passed
// over.
func (d *lineDecoder) attachment(scan *jsonscan.Scanner, k jsonscan.Kind, a *attachmentFacts) error {
	a.kind = scannedKind(k)
	if k != jsonscan.ObjectStart {
		return scan.Skip()
	}
	// A listing that lacks its names is kept ([skillNamesKept]).
	a.namesKept = true
	return d.members(scan, func(name []byte, k jsonscan.Kind) error {
		var err error
		switch string(name) {
		case "type":
			a.typ, err = d.keep(scan, k, "attachment.type")
		case "toolUseID":
			a.toolUseID, err = d.keep(scan, k, "attachment.toolUseID")
		case "by":
			a.by, err = d.keep(scan, k, "attachment.by")
		case "content":
			strs, objs := true, true
			var list bool
			list, err = d.elements(scan, k, func(k jsonscan.Kind) error {
				strs, objs = strs && k == jsonscan.String, objs && k == jsonscan.ObjectStart
				return scan.Skip()
			})
			a.content, a.contentStrings, a.contentObjects = scannedKind(k), list && strs, list && objs
		case "skills":
			shaped := true
			var list bool
			list, err = d.elements(scan, k, func(k jsonscan.Kind) error {
				shaped = shaped && (k == jsonscan.ObjectStart || k == jsonscan.ArrayStart)
				return scan.Skip()
			})
			a.skillsShaped = list && shaped
		case "names":
			var unmeasured bool
			a.namesKept, unmeasured, err = d.skillNames(scan, k)
			a.namesErr = nil
			if unmeasured {
				a.namesErr = errSkillNameUnmeasured
			}
		case "entries":
			kept := true
			var list bool
			list, err = d.elements(scan, k, func(k jsonscan.Kind) error {
				if !kept {
					return scan.Skip()
				}
				var entryErr error
				kept, entryErr = d.withheldEntry(scan, k)
				return entryErr
			})
			a.entriesKept = list && kept
		default:
			err = scan.Skip()
		}
		return err
	})
}

// skillNames reads a skill listing's `names`, whose first token, k, the
// scanner stands at, as [skillNamesKept] reads it: kept where it is a list of
// at most [skillListingMaxNames] strings, each kept by the rule of a name
// ([skillNameKept]), read in order and the first that is not deciding it; and
// unmeasured instead where every name is kept but one past
// [skillListingMaxNameUnits] held a byte that is no UTF-8, which the layer
// cannot measure ([errSkillNameUnmeasured]). Each name streams into its
// length in UTF-16 code units ([textUnits]) and whether its bytes as written
// were UTF-8 ([jsonscan.Scanner.InvalidUTF8]), which its decoded text cannot
// say.
func (d *lineDecoder) skillNames(scan *jsonscan.Scanner, k jsonscan.Kind) (kept, unmeasured bool, err error) {
	count, unreadable := 0, false
	kept = true
	list, walked := d.elements(scan, k, func(k jsonscan.Kind) error {
		if !kept {
			return scan.Skip()
		}
		if count++; count > skillListingMaxNames || k != jsonscan.String {
			kept = false
			return scan.Skip()
		}
		var units textUnits
		for {
			n, err := scan.Read(d.text[:])
			units.write(d.text[:n])
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
		}
		name, unknown := skillNameKept(units.units(), !scan.InvalidUTF8())
		kept, unreadable = name, unreadable || unknown
		return nil
	})
	switch {
	case walked != nil:
		return false, false, walked
	case !list || !kept:
		return false, false, nil
	case unreadable:
		return false, true, nil
	}
	return true, false, nil
}

// withheldEntry reads one entry of a withheld memory's `entries`, whose first
// token, k, the scanner stands at, as [withheldEntryKept] reads it: of an
// object, its `path`'s and `displayPath`'s kinds and its `why` held as
// written, each by its last occurrence, decided by the rule of such an entry
// ([withheldKept]) once it ends; anything else no entry the rule keeps.
func (d *lineDecoder) withheldEntry(scan *jsonscan.Scanner, k jsonscan.Kind) (bool, error) {
	if k != jsonscan.ObjectStart {
		return withheldKept(scannedKind(k), 0, nil, 0), scan.Skip()
	}
	why, _, _ := d.clearHeld()
	var path, displayPath valueKind
	err := d.members(scan, func(name []byte, k jsonscan.Kind) error {
		switch string(name) {
		case "path":
			path = scannedKind(k)
		case "displayPath":
			displayPath = scannedKind(k)
		case "why":
			return d.hold(scan, k, "attachment.entries.why", why)
		}
		return scan.Skip()
	})
	return withheldKept('{', path, present(*why), displayPath), err
}

// compacted reads a compaction's boundary's `compactMetadata`, whose first
// token, k, the scanner stands at, into c, as [compactOf] reads it: its kind;
// of an object, its `preservedMessages` ([lineDecoder.preservedMessages]) and
// its `preservedSegment` kept as written.
func (d *lineDecoder) compacted(scan *jsonscan.Scanner, k jsonscan.Kind, c *compactFacts) error {
	c.kind = scannedKind(k)
	if k != jsonscan.ObjectStart {
		return scan.Skip()
	}
	return d.members(scan, func(name []byte, k jsonscan.Kind) error {
		var err error
		switch string(name) {
		case "preservedMessages":
			err = d.preservedMessages(scan, k, c)
		case "preservedSegment":
			c.segment, err = d.keep(scan, k, "compactMetadata.preservedSegment")
		default:
			err = scan.Skip()
		}
		return err
	})
}

// preservedMessages reads a compaction's `preservedMessages`, whose first
// token, k, the scanner stands at, into c, as [preservedList] reads it, its
// shapes checked in its order though its keys come in any: the list's kind,
// none where it is null; an object's `anchorUuid`, held as written until it
// ends; then its `uuids`, each entry read by its opening into a canonical
// uuid or none ([threadPreserved.list]), in order, up to the first that is no
// string. Where the anchor is no string the list holds no entry, as the
// list's walk never began.
func (d *lineDecoder) preservedMessages(scan *jsonscan.Scanner, k jsonscan.Kind, c *compactFacts) error {
	c.messages, c.list, c.listErr = scannedKind(k), threadPreserved{}, nil
	switch k {
	case jsonscan.Null:
		return scan.Skip()
	case jsonscan.ObjectStart:
	default:
		c.listErr = fmt.Errorf("compactMetadata.preservedMessages is %s, not an object", c.messages)
		return scan.Skip()
	}
	anchor, _, _ := d.clearHeld()
	// A list that lacks its uuids is one whose walk refuses them.
	uuids := fmt.Errorf("is %s, not a list", valueKind(0))
	err := d.members(scan, func(name []byte, k jsonscan.Kind) error {
		switch string(name) {
		case "anchorUuid":
			return d.hold(scan, k, "compactMetadata.preservedMessages.anchorUuid", anchor)
		case "uuids":
			c.list, uuids = threadPreserved{}, nil
			list, err := d.elements(scan, k, func(k jsonscan.Kind) error {
				if uuids != nil {
					return scan.Skip()
				}
				if k != jsonscan.String {
					uuids = fmt.Errorf("an entry is %s, not a string", scannedKind(k))
					return scan.Skip()
				}
				opening, err := d.opening(scan, uuidTextBytes)
				if err != nil {
					return err
				}
				u, canonical := uuidBytes(opening)
				if canonical && len(c.list.UUIDs) < threadChainMaxRecords {
					var room bool
					if c.list.UUIDs, room = grow(&d.meter, c.list.UUIDs, threadChainMaxRecords); !room {
						return nil
					}
				}
				c.list.list(u, canonical)
				return nil
			})
			if !list {
				uuids = fmt.Errorf("is %s, not a list", scannedKind(k))
			}
			return err
		}
		return scan.Skip()
	})
	if err != nil {
		return err
	}
	switch named, err := wireRequiredString("preservedMessages.anchorUuid", present(*anchor)); {
	case err != nil:
		c.list, c.listErr = threadPreserved{}, err
	case uuids != nil:
		c.listErr = fmt.Errorf("compactMetadata.preservedMessages.uuids: %w", uuids)
	case d.meter.take(len(named)):
		c.list.Named, c.list.Kept, c.list.Anchor = true, c.list.Listed > 0, named
	}
	return nil
}

// retracted reads a record's `retractedMessageUuids`, whose first token, k,
// the scanner stands at, into f, as [retractedOf] reads it: of a list, each
// entry read by its opening into a canonical prefix or none
// ([threadRetracted.entry]), in order, up to the first that is no string,
// that entry's error; anything else the list's own.
func (d *lineDecoder) retracted(scan *jsonscan.Scanner, k jsonscan.Kind, f *lineFacts) error {
	list, err := d.elements(scan, k, func(k jsonscan.Kind) error {
		if f.retractedErr != nil {
			return scan.Skip()
		}
		if k != jsonscan.String {
			f.retractedErr = fmt.Errorf("retractedMessageUuids: an entry is %s, not a string", scannedKind(k))
			return scan.Skip()
		}
		opening, err := d.opening(scan, retractionPrefixBytes)
		if err != nil {
			return err
		}
		p, canonical := chainPrefix(opening)
		if canonical && len(f.retracted.prefixes) < threadChainMaxRecords {
			var room bool
			if f.retracted.prefixes, room = grow(&d.meter, f.retracted.prefixes, threadChainMaxRecords); !room {
				return nil
			}
		}
		f.retracted.entry(p, canonical)
		return nil
	})
	if !list {
		f.retractedErr = fmt.Errorf("retractedMessageUuids: is %s, not a list", scannedKind(k))
	}
	return err
}

// replacements reads a content-replacement record's `replacements`, whose
// first token, k, the scanner stands at, into r, as [replacementsOf] reads
// it: its kind; of a list, EVERY element counted, and each read by the rule
// of an entry ([replacedID]) until one fails it, its `kind` and `toolUseId`
// held as written until it ends, the id of each that replaces a tool's
// result kept, in order.
func (d *lineDecoder) replacements(scan *jsonscan.Scanner, k jsonscan.Kind, r *replacementFacts) error {
	r.kind = scannedKind(k)
	_, err := d.elements(scan, k, func(k jsonscan.Kind) error {
		r.count++
		if r.err != nil {
			return scan.Skip()
		}
		kind, toolUseID, _ := d.clearHeld()
		var err error
		if k == jsonscan.ObjectStart {
			err = d.members(scan, func(name []byte, k jsonscan.Kind) error {
				switch string(name) {
				case "kind":
					return d.hold(scan, k, "replacements.kind", kind)
				case "toolUseId":
					return d.hold(scan, k, "replacements.toolUseId", toolUseID)
				}
				return scan.Skip()
			})
		} else {
			err = scan.Skip()
		}
		if err != nil {
			return err
		}
		id, replaced, refused := replacedID(scannedKind(k), present(*kind), present(*toolUseID))
		switch {
		case refused != nil:
			r.err = refused
		case replaced:
			r.ids = d.keepID(r.ids, id)
		}
		return nil
	})
	return err
}

// batch reads an assistant record's `batchToolUses`, whose first token, k,
// the scanner stands at, into b, as [batchOf] reads it: that it is there; of
// a list, each entry's `id`, held as written until the entry ends, read by
// the rule of an entry ([batchEntryID]), in order, up to the first it
// refuses, that entry's error; anything else the list's own.
func (d *lineDecoder) batch(scan *jsonscan.Scanner, k jsonscan.Kind, b *batchFacts) error {
	b.present = true
	list, err := d.elements(scan, k, func(k jsonscan.Kind) error {
		if b.err != nil {
			return scan.Skip()
		}
		id, _, _ := d.clearHeld()
		var err error
		if k == jsonscan.ObjectStart {
			err = d.members(scan, func(name []byte, k jsonscan.Kind) error {
				if string(name) == "id" {
					return d.hold(scan, k, "batchToolUses.id", id)
				}
				return scan.Skip()
			})
		} else {
			err = scan.Skip()
		}
		if err != nil {
			return err
		}
		switch kept, refused := batchEntryID(scannedKind(k), present(*id)); {
		case refused != nil:
			b.err = refused
		default:
			b.ids = d.keepID(b.ids, kept)
		}
		return nil
	})
	if !list {
		b.err = fmt.Errorf("is %s, not a list", scannedKind(k))
	}
	return err
}

// keepID is ids with id after them, id charged as its rule just made it and
// the array before it grows; ids as they are where the line's facts have no
// room left ([lineMeter]).
func (d *lineDecoder) keepID(ids []string, id string) []string {
	if !d.meter.take(len(id)) {
		return ids
	}
	ids, ok := grow(&d.meter, ids, math.MaxInt)
	if !ok {
		return ids
	}
	return append(ids, id)
}

// elements reads the value whose first token, k, the scanner stands at, as a
// list: each element's first token handed to each, which reads the element
// whole, in order, to the list's end. list false, and the value passed over,
// where it is no list.
func (d *lineDecoder) elements(scan *jsonscan.Scanner, k jsonscan.Kind,
	each func(k jsonscan.Kind) error) (bool, error) {
	if k != jsonscan.ArrayStart {
		return false, scan.Skip()
	}
	for {
		k, err := scan.Next()
		if err != nil || k == jsonscan.ArrayEnd {
			return true, err
		}
		if err = each(k); err != nil {
			return true, err
		}
	}
}

// opening is the decoded text of the string the scanner stands at as far as
// a reader of its first n characters reads it: all of it where it is at most
// n bytes long, else its first n bytes and at least one more, so a reader
// tells an entry of exactly n bytes from a longer one. The rest is left to
// the scanner's next call, which passes it over. In d's buffer, valid until
// the next opening; n is at most [uuidTextBytes], so the buffer always has
// room for the character after it.
func (d *lineDecoder) opening(scan *jsonscan.Scanner, n int) ([]byte, error) {
	got := 0
	for got <= n {
		m, err := scan.Read(d.open[got:])
		got += m
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	return d.open[:got], nil
}

// keep is the value the scanner stands at, of kind k, the value of key, as
// written, in a copy of its own for the line's facts to hold, charged before
// it is made: nil where it runs past d's bound, which marks the line
// ([lineDecoder.capture]), or where the line's facts have no room left for
// it ([lineMeter]).
func (d *lineDecoder) keep(scan *jsonscan.Scanner, k jsonscan.Kind, key string) (json.RawMessage, error) {
	raw, _, err := d.capture(scan, k, key)
	if raw == nil || !d.meter.take(len(raw)) {
		return nil, err
	}
	return append(json.RawMessage(nil), raw...), err
}

// hold is the value the scanner stands at, of kind k, the value of key, as
// written, into *buf, a buffer the reader keeps, for the rule that decides
// its object to read when the object ends: emptied where it runs past d's
// bound, which marks the line ([lineDecoder.capture]).
func (d *lineDecoder) hold(scan *jsonscan.Scanner, k jsonscan.Kind, key string, buf *[]byte) error {
	raw, _, err := d.capture(scan, k, key)
	*buf = append((*buf)[:0], raw...)
	return err
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
