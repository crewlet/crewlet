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
	"strings"
	"unicode/utf8"
	"unsafe"

	"github.com/crewlet/crewlet/internal/jsonscan"
)

// A THREAD LINE READ IN ONE SCANNER PASS, NEVER HELD: [threadReader] reads
// each line through [jsonscan] without holding it, so a line of any length
// costs what its readers read of it. Why a tee of the raw bytes, why nothing
// commits before the break, the order a line is judged in, names by their
// decoded text and last occurrence, the three means a value is read by, the
// type's gate and the meter: the package doc, "A thread line is read in one
// scanner pass, never held".

// threadValueMax is the most of one value the layer reads whole: a raw value
// it keeps for an accessor to decode (a uuid, a timestamp, a reply's usage,
// an attachment's type), or one it decides as its object ends (a call's id, a
// tool's name), as written. PAST IT, THE LINE FAILS CLOSED
// ([errThreadBound]): a reader never reads a guess, and a line no release
// writes costs its reading, never a false hold. 64 KiB: the scanner's own read
// buffer, and 160 times the longest such value of any probe thread, a reply's
// usage of 402 bytes. A longer value is read streamed ([threadMessage.Says],
// [threadResultText]) or by its prefix ([threadLine.Retracted]), unbounded. A
// reader's scratch a value grew past [lineScratchKeep] goes with its line.
const threadValueMax = 64 << 10

// lineScratchKeep is the most of a scratch buffer a reader keeps past the line
// that grew it: ten times the longest value of any probe thread, so an
// ordinary line reuses its buffers, while a line of values at
// [threadValueMax] pays for its scratch alone rather than the reader holding
// seventeen such buffers, over 1 MiB, for every member it goes on to read.
const lineScratchKeep = 4 << 10

// lineScratch is b emptied for the next line, or nil where a line grew it
// past [lineScratchKeep].
func lineScratch(b []byte) []byte {
	if cap(b) > lineScratchKeep {
		return nil
	}
	return b[:0]
}

// errLineMember is a line that runs past what is left of its member's bound
// ([lineSource]): the source's own, set by it alone, so no error of a
// member's reader, one that wraps the take's scrub's bound among them, can
// pass for it.
var errLineMember = errors.New("executor: a thread line runs past what is left of its member's bound")

// threadReader reads one member of a Claude Code session a line at a time,
// each line read in one scanner pass into its head, its byte facts and its
// facts, never held ([threadReader.Next]). One goroutine reads it. A THREAD'S
// MEMBER: a subagent's metadata is one JSON document with no line break
// ([claudeSubagentMeta]), which it reads as nothing but a torn piece. ONE
// READER READS A SESSION'S MEMBERS, one after another ([threadReader.Reset]),
// keeping its buffers whatever members it reads, never one a member.
type threadReader struct {
	r *bufio.Reader
	// maxBytes is the member's bound, and left what of it the lines read so
	// far have not taken.
	maxBytes, left int64
	// n is how many whole lines have been read, and maxLines the most it may
	// be.
	n, maxLines int
	// torn is whether the member ended in a piece with no break.
	torn bool
	// end is what ended the reading: io.EOF, errThreadBound, ctx's error, or
	// an error of r. Every Next answers it from then on.
	end error
	// src is the line being read, and scan the scanner that reads it, reset
	// to each line and kept across lines and members; bytes the line's byte
	// facts, src's tee.
	src   lineSource
	scan  *jsonscan.Scanner
	bytes lineBytes
	// dec is the pass's own scratch, kept across lines and members.
	dec lineDecoder
	// valueMax is [threadValueMax] and meterMax [threadLineMax]; the
	// differential raises both to compare the pass's facts with the whole
	// decode's at any length (claudelinescan_internal_test.go).
	valueMax, meterMax int
	// asked is the take tokens each line names of a hook's words
	// ([threadReader.Asking]), and estimating whether each line reads its
	// estimate ([threadReader.Estimating]).
	asked      []string
	estimating bool
	// gate is the pass's large slot each line's facts hold before they charge
	// past the base ([threadReader.Gate]); nil where no slot gates them.
	gate *lineGate
	// summary is where each line's message content is read whole
	// ([threadReader.Summarizing]); nil where none is.
	summary *summaryText
}

// lineGate is a reader's way to its pass's large slot ([threadReader.Gate]):
// held whether it holds it now, for its last line or, where drop is nil, to
// the pass's end.
type lineGate struct {
	pieceGate
	held bool
}

// newThreadReader reads r, one member, a line at a time: at most maxBytes of
// it, the caller's figure ([threadPlainMax] for a whole thread, what is left
// of an archive's budget for one member of it); at or below zero, no byte;
// and at most [threadLinesMax] whole lines ([threadReader.Reset] takes
// another). The member is a thread, never a subagent's metadata
// ([claudeSubagentMeta]).
func newThreadReader(r io.Reader, maxBytes int64) *threadReader {
	return newThreadReaderSize(r, maxBytes, lineChunkBytes)
}

// newThreadReaderSize is [newThreadReader] taking the member chunk bytes at a
// time at most: what a test reads with to move the edge of what the scanner
// is handed through every byte of a short line, since a member's reader alone
// cannot (bufio fills its buffer to a break or to its end whatever pieces the
// member hands it).
func newThreadReaderSize(r io.Reader, maxBytes int64, chunk int) *threadReader {
	// Built over no reader and then reset onto r: bufio.NewReaderSize hands
	// back r itself where r is a *bufio.Reader of the size, and a later Reset
	// would then reset the caller's reader.
	t := &threadReader{r: bufio.NewReaderSize(nil, chunk), valueMax: threadValueMax, meterMax: threadLineMax}
	t.src.tee = &t.bytes
	t.scan = jsonscan.New(&t.src)
	t.Reset(r, maxBytes, threadLinesMax)
	return t
}

// Reset makes t read r, one member, within maxBytes and maxLines whole lines,
// as [newThreadReader] does: whatever t read before, how it ended among it (a
// torn piece, a bound, an error, a context that ended), is gone, and t reads
// r exactly as a fresh reader would, keeping only its buffers.
func (t *threadReader) Reset(r io.Reader, maxBytes int64, maxLines int) {
	maxBytes = max(maxBytes, 0)
	t.dropGate()
	t.gate = nil
	t.r.Reset(r)
	t.maxBytes, t.left = maxBytes, maxBytes
	t.n, t.maxLines, t.torn, t.end, t.asked, t.estimating = 0, max(maxLines, 0), false, nil, nil, false
	t.summary = nil
}

// Summarizing makes every line t reads from here on read its message's
// content, where it is one string, into s, whole to [summaryReadMaxBytes] and
// past it none ([summaryText]): set only where the line's last message's last
// content is a string the pass read. The one reader of a summary's line asks
// it ([claudeSummaryLine]); every other pass stops reading a content once its
// automata decide. Called before the first [threadReader.Next];
// [threadReader.Reset] clears it.
func (t *threadReader) Summarizing(s *summaryText) { t.summary = s }

// Estimating makes every line t reads from here on read Claude Code's estimate
// of its content ([threadLine.Estimate], [threadLine.AttachmentEstimate]),
// which a line read otherwise answers [errNotEstimated]: each length counted
// as the line passes, at any length, at what counting costs the pass (the
// package doc's "A thread line is read in one scanner pass"), so a reading
// that reads it asks ([nextReading]) and no other pays. Called before the
// first [threadReader.Next]; [threadReader.Reset] clears it.
func (t *threadReader) Estimating() { t.estimating = true }

// Asking makes every line t reads from here on name which of tokens, take
// tokens ([claudeTakeToken]), a hook's words hold ([threadLine.HookTakes]):
// bit i for tokens[i]. Called before the first [threadReader.Next];
// [threadReader.Reset] clears it. Past [maxAskedTakes] tokens it asks nothing
// and answers [errAskedPastMasks]: the ask comes off a row, which a peer with
// wider bounds may have written, so its caller decides nothing of them.
func (t *threadReader) Asking(tokens []string) error {
	if len(tokens) > maxAskedTakes {
		t.asked = nil
		return fmt.Errorf("%w: %d asked, %d held", errAskedPastMasks, len(tokens), maxAskedTakes)
	}
	t.asked = slices.Clone(tokens)
	return nil
}

// Gate makes every line t reads from here on hold gate BEFORE its facts charge
// past [collectionBaseLineCharge] ([lineMeter]), and drop it as the next line
// begins or at [threadReader.dropGate]; where gate's drop is nil, it holds the
// slot to the pass's end, for its caller to release. A line's length takes
// nothing: what no reader reads of it is passed over unheld. Called before the
// first [threadReader.Next]; [threadReader.Reset] clears it.
func (t *threadReader) Gate(gate pieceGate) { t.gate = &lineGate{pieceGate: gate} }

// dropGate drops the slot t holds for its last line, where its gate drops.
func (t *threadReader) dropGate() {
	if g := t.gate; g != nil && g.held && g.drop != nil {
		g.drop()
		g.held = false
	}
}

// Next is the member's next WHOLE line, one that ends in "\n", read without its
// break in one scanner pass: io.EOF at the member's end, a last piece with no
// break never a line ([threadReader.Torn]); a *threadLineError for a whole line
// that is no record, the reader still usable; and, each ending the reading for
// good, errThreadBound once the member runs past its bound (EVEN RIGHT AFTER A
// BREAK) or holds a whole line past its lines' bound, read whole first, once
// the pass captures a value past [threadValueMax] or the facts run past
// [threadLineMax], or once the line's type reads a frame passed over for an
// earlier type; the member's reader's error AS IT IS, one wrapping the scrub's
// bound among them; ctx's error, asked before each piece the line takes; and,
// first, a wait for the slot ([threadReader.Gate]) that ended. The package doc,
// "A thread line is read in one scanner pass, never held", says why each.
func (t *threadReader) Next(ctx context.Context) (threadLine, error) {
	t.dropGate()
	if t.end != nil {
		return threadLine{}, t.end
	}
	t.src.reset(ctx, t.r, t.left)
	t.bytes.reset()
	facts := new(lineFacts)
	t.scan.Reset(&t.src)
	t.dec.reset(t.valueMax, t.meterMax)
	t.dec.asked, t.dec.estimating, t.dec.summary = t.asked, t.estimating, t.summary
	if t.summary != nil {
		t.summary.reset()
	}
	if t.gate != nil && !t.gate.held {
		t.dec.meter.gate = t.gate
	}
	scanned := t.dec.line(t.scan, facts)
	// A FAULT STOPS THE SCANNER BEFORE THE BREAK: the rest of the line is read
	// through the meter and the tee, holding nothing, so a refused line's
	// size and byte facts are the whole line's.
	if !t.src.broke && t.src.cause == nil && t.dec.meter.waitErr == nil {
		t.src.drain()
	}
	t.src.ctx = nil
	if err := t.dec.meter.waitErr; err != nil {
		t.end = err
		return threadLine{}, err
	}
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
	if t.n > t.maxLines {
		t.end = fmt.Errorf("%w: the member holds more than the %d whole lines it is read within", errThreadBound,
			t.maxLines)
		return threadLine{}, t.end
	}
	return t.commit(scanned, facts)
}

// Torn reports whether the member ended in a piece with no break, which
// [threadReader.Next] read as no line: known once Next has answered io.EOF.
func (t *threadReader) Torn() bool { return t.torn }

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
func (t *threadReader) commit(scanned error, facts *lineFacts) (threadLine, error) {
	size, read := int(t.src.n+1), t.bytes.facts()
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
	case t.dec.meter.waitErr != nil:
		t.end = t.dec.meter.waitErr
		return threadLine{}, t.end
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
	if t.dec.estimating {
		t.dec.finishEstimate(facts)
	}
	// The parent as written is the line's own: the pass's buffer is the next
	// line's.
	if keys.parent.kind == '"' {
		head.Parent = slices.Clone(head.Parent)
	}
	return threadLine{N: t.n, Size: size, Bytes: read, Head: head, f: facts}, nil
}

// kindOf is raw, a refused record's `type`, as the [recordKind] it names, the
// layer's own string; the zero kind for anything else, so no string the box
// wrote is held.
func kindOf(raw json.RawMessage) recordKind {
	kind, err := wireRequiredString("type", raw)
	if err != nil {
		return ""
	}
	for _, k := range recordKinds {
		if string(k) == kind {
			return k
		}
	}
	return ""
}

// scanFault is how Claude Code's load reads a line the scanner refused with
// e ([lineFault]).
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
// counts against what is left of the member's bound and writes to its tee
// ONCE, as it takes it, and then hands it over, or passes it over
// ([lineSource.drain]). The tee is the thread layer's byte facts
// ([lineBytes]), and none for a reader that keeps none.
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
	// tee is what each byte taken is written to, nil for nothing; its
	// writes never fail.
	tee io.Writer
	// bound is the longest line the source hands, its break among it, as
	// [readLines] bounds one, 0 for none; past says the line ran past it,
	// and was then passed over to its end, handed to no one.
	bound int64
	past  bool
	// skipped is the bytes of the line its reader passed over as images'
	// ([lineWalk.pass]), which the bound does not count, and slack what it
	// takes past the bound before it is past: the image bytes taken that the
	// reader has yet to pass ([imageSlack]), 0 on a walk that passes none.
	skipped, slack int64
}

// reset makes s the next line of the member r reads, which may take left
// bytes of it, read under ctx.
func (s *lineSource) reset(ctx context.Context, r *bufio.Reader, left int64) {
	s.ctx, s.r, s.left = ctx, r, left
	s.pending, s.n, s.broke, s.cause, s.past, s.skipped = nil, 0, false, nil, false, 0
}

// over reports whether the line, ended, ran past the bound: what of it was
// not an image's, its break among it, longer than the bound.
func (s *lineSource) over() bool {
	n := s.n - s.skipped
	if s.broke {
		n++
	}
	return s.past || s.bound > 0 && n > s.bound
}

// Read reads the line's next bytes into p, up to its break: io.EOF once the
// line has ended at its break, and what ended it otherwise once it has. For a
// p of at least one byte it never answers no bytes with no error. A line past
// the bound ends where it passed it, its rest passed over ([lineSource.take]).
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
// first; then THE MEMBER'S METER, before the piece's own error, as the
// differential's whole reader meters a line (claudelinewhole_internal_test.go),
// so a member past its bound in the same piece as its reader's failure ends
// with the bound, and the bound holds even right after a break; then the
// piece, but its break, counted and written to the tee, and handed on only
// while the line, its break among it and its images not, is within its own
// bound and slack ([lineSource.over] judges it exactly once it ends).
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
	if s.bound > 0 && s.n+int64(len(chunk))-s.skipped > s.bound+s.slack {
		s.past = true
	}
	if err == nil {
		// ReadSlice answers no error only for a piece its break ends.
		chunk, s.broke = chunk[:len(chunk)-1], true
	}
	if s.tee != nil {
		_, _ = s.tee.Write(chunk)
	}
	s.n += int64(len(chunk))
	if s.pending = chunk; s.past {
		s.pending = nil
	}
	switch {
	case err == nil, errors.Is(err, bufio.ErrBufferFull):
	case errors.Is(err, io.EOF):
		// A wrapped io.EOF is the member's end too: only the member's
		// reader's other errors are its own.
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
// its length (a string right after its decode, past the bound by the three a
// charge covers at most, "A collection's read of a thread"), an array by its
// capacity times its element's size, before it is made ([grow]). NOTHING IS
// REFUNDED, so the meter bounds what the line's facts keep, every copy of a
// value counted. Not charged: the facts' struct, the reader's buffers, what
// the pass makes and drops. A charge past unheld first holds gate's slot, if
// any ([threadReader.Gate]).
type lineMeter struct {
	left int
	over bool
	// unheld is what the facts may take before gate's slot (gate nil where
	// none gates the line); waitErr, a wait for it that ended ([errLargeWait]),
	// after which the meter is over and the reading ends ([threadReader.Next]).
	unheld  int
	gate    *lineGate
	waitErr error
}

// take charges n bytes, reporting whether they were left: false, and the
// meter over, where they were not, or where it was over before, or where the
// wait for the slot a charge past unheld holds first ended.
func (m *lineMeter) take(n int) bool {
	if m.over || n > m.left {
		m.over = true
		return false
	}
	if g := m.gate; g != nil && !g.held && n > m.unheld {
		if m.waitErr = g.hold(); m.waitErr != nil {
			m.over = true
			return false
		}
		g.held = true
	}
	m.left, m.unheld = m.left-n, m.unheld-n
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
	// asked is the take tokens the line's reader asks of a hook's words
	// ([threadReader.Asking]).
	asked []string
	// estimating is whether the line's reader asked for its estimate
	// ([threadReader.Estimating]); images how many images the record's task
	// delivery carries, and rendered its stored rendering, each by its last
	// occurrence, which the estimate reads once the line ends
	// ([lineDecoder.finishEstimate]).
	estimating bool
	images     int64
	rendered   renderedCharge
	// summary is where a message's content string is read whole, nil where
	// the reader asks none ([threadReader.Summarizing]).
	summary *summaryText
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
	// est is what the estimate's rule of an element reads of it
	// ([estimateObject]), but its type, read off typ as the block ends.
	est estimateObject
}

// reset makes b a block not yet read, its buffers kept.
func (b *blockScan) reset() {
	b.typ, b.id, b.name, b.toolUseID, b.isError = b.typ[:0], b.id[:0], b.name[:0], b.toolUseID[:0], b.isError[:0]
	b.input, b.to, b.says, b.text, b.est = 0, b.to[:0], textSays{}, threadResultText{}, estimateObject{}
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
// bytes of a value and holding at most meterMax bytes of facts, its scratch
// kept but what the line before grew past [lineScratchKeep].
func (d *lineDecoder) reset(valueMax, meterMax int) {
	d.valueMax, d.object, d.over = valueMax, false, ""
	d.meter = lineMeter{left: meterMax, unheld: collectionBaseLineCharge}
	for i := range d.head {
		d.head[i] = scannedKey{raw: lineScratch(d.head[i].raw)}
	}
	d.typed, d.typeText, d.missed = false, lineScratch(d.typeText), 0
	d.images, d.rendered = 0, renderedCharge{}
	for i := range d.held {
		d.held[i] = lineScratch(d.held[i])
	}
	b := &d.block
	for _, buf := range [...]*[]byte{&b.typ, &b.id, &b.name, &b.toolUseID, &b.isError, &b.to, &b.resultType} {
		*buf = lineScratch(*buf)
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
// ([gates]), so a record of another type holds nothing of it; and of a
// message, the calls of the kind the record makes alone, a user's results or
// an assistant's uses, in order.
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
// decode captured into f, taskDelivery's and rowsAheadOfPutBack's presence,
// a frame its type may read ([gates]), and every other member passed over
// unheld; estimating, the images taskDelivery carries and the record's stored
// rendering besides.
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
	switch {
	case string(name) == "rowsAheadOfPutBack":
		f.putBack = true
		return scan.Skip()
	case string(name) == "taskDelivery":
		f.taskDelivery = true
		if !d.estimating {
			return scan.Skip()
		}
		var err error
		d.images, err = d.taskImages(scan, k)
		return err
	case string(name) == "rendered" && d.estimating:
		var err error
		d.rendered, err = d.renderedOf(scan, k)
		return err
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

// message reads a user or assistant record's `message`, whose first token, k,
// the scanner stands at, into m: its kind; of an object, its small values as
// written and its content ([lineDecoder.content]), a content it lacks an error
// of the message's.
func (d *lineDecoder) message(scan *jsonscan.Scanner, k jsonscan.Kind, m *messageFacts) error {
	m.kind = scannedKind(k)
	if d.summary != nil {
		d.summary.reset()
	}
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

// errContentKind is a message's content of kind, neither a string nor a
// list.
func errContentKind(kind valueKind) error {
	return fmt.Errorf("content is %s, neither a string nor a list", kind)
}

// content reads a message's `content`, whose first token, k, the scanner
// stands at, into m, whatever an earlier `content` left: one string, streamed
// ([lineDecoder.contentText]); a list, a block at a time
// ([lineDecoder.blocks]); anything else the message's error. Estimating, the
// scanner counts the value as it passes, and a string is passed to its end
// for its length.
func (d *lineDecoder) content(scan *jsonscan.Scanner, k jsonscan.Kind, m *messageFacts) error {
	m.content, m.contentErr = threadMessage{}, nil
	m.charge, m.emptyText = estimateCharge{}, false
	if d.summary != nil {
		d.summary.reset()
		d.summary.set = k == jsonscan.String
	}
	if d.estimating {
		scan.CountUnits(true)
		defer scan.CountUnits(false)
	}
	switch k {
	case jsonscan.String:
		if err := d.contentText(scan, &m.content); err != nil || !d.estimating {
			return err
		}
		if err := scan.Skip(); err != nil {
			return err
		}
		units := scan.TextUnits()
		m.charge, m.emptyText = estimateCharge{est: estimateUnits(units)}, units == 0
		return nil
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
		if d.summary != nil {
			_, _ = d.summary.Write(p)
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	if d.summary != nil {
		if err := d.readRest(scan, d.summary); err != nil {
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

// readRest reads what is left of the current string into w, a piece at a
// time.
func (d *lineDecoder) readRest(scan *jsonscan.Scanner, w io.Writer) error {
	for {
		n, err := scan.Read(d.text[:])
		_, _ = w.Write(d.text[:n])
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return err
		}
	}
}

// blocks reads a message's content that is a list, its opening bracket the
// scanner's current token, into m, a block at a time: each decided by the one
// rule of a block as it ends ([blockCallOf]), counted by its kind, a call
// kept of either kind (the record's type, which keeps one, may come after
// the message: [settle]), and whether its text says anything; the first
// block the rule refuses the message's error, and every block after it
// passed over.
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
			// The block's brace, which Next took, begins its length as written.
			start := scan.Units() - 1
			if err = d.readBlock(scan); err != nil {
				return err
			}
			call, refused = blockCallOf('{', d.block.keys())
			if refused == nil && d.estimating {
				m.charge = m.charge.plus(d.block.charge(scan.Units() - start))
			}
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
			err := d.hold(scan, k, "message.content.name", &b.name)
			b.est.name = estTextOf(scan, k)
			return err
		case "tool_use_id":
			return d.hold(scan, k, "message.content.tool_use_id", &b.toolUseID)
		case "is_error":
			return d.hold(scan, k, "message.content.is_error", &b.isError)
		case "input":
			return d.input(scan, k)
		case "content":
			return d.resultContent(scan, k)
		case "text":
			err := d.blockText(scan, k)
			if err == nil && d.estimating && k == jsonscan.String {
				err = scan.Skip()
			}
			b.est.text = estTextOf(scan, k)
			return err
		}
		if d.estimating {
			return d.estKey(scan, k, name, &b.est, 1)
		}
		return scan.Skip()
	})
}

// input reads a block's `input`, whose first token, k, the scanner stands at:
// its kind, and of an object its `to` as written, every other key passed over.
func (d *lineDecoder) input(scan *jsonscan.Scanner, k jsonscan.Kind) error {
	b := &d.block
	b.input, b.to = scannedKind(k), b.to[:0]
	start := scan.Units() - takenOf(k)
	var err error
	if k != jsonscan.ObjectStart {
		err = scan.Skip()
	} else {
		err = d.members(scan, func(name []byte, k jsonscan.Kind) error {
			if string(name) == "to" {
				return d.hold(scan, k, "message.content.input.to", &b.to)
			}
			return scan.Skip()
		})
	}
	b.est.input, b.est.inputSet = scan.Units()-start, k != jsonscan.Null
	return err
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
// stands at, into the block's text: one string, streamed whole into its length
// and digests ([textResult]); a list whose one element is an object whose
// `type` is the string "text" and whose `text` is a string, that string
// streamed whole; no text for anything else, a list of two or more elements,
// or one whose first is no object, among it, and every element after the first
// passed over.
//
// Estimating, every element is read by the estimate's rule of an element too
// ([lineDecoder.estElement]), the first as the text's digest besides, into
// the block's content as the estimate reads it.
func (d *lineDecoder) resultContent(scan *jsonscan.Scanner, k jsonscan.Kind) error {
	b := &d.block
	b.text, b.est.content = threadResultText{}, estimateList{kind: scannedKind(k)}
	switch k {
	case jsonscan.String:
		if err := d.resultText(scan); err != nil {
			return err
		}
		b.text = d.result.sum()
		b.est.content.all.est = estimateUnits(scan.TextUnits())
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
		digest := at == 0 && k == jsonscan.ObjectStart
		if !digest {
			b.text = threadResultText{}
		}
		switch {
		case d.estimating:
			var c estimateCharge
			c, err = d.estElement(scan, k, 2, digest)
			b.est.content.add(k == jsonscan.ObjectStart, c)
		case digest:
			err = d.resultElement(scan, nil, nil)
		default:
			err = scan.Skip()
		}
		if err != nil {
			return err
		}
	}
}

// resultElement reads a result's first element, an object, its opening brace
// the scanner's current token, as the text's digest: its `type` held as
// written and its `text` streamed whole ([lineDecoder.resultText]), each by
// its last occurrence, the block's text where the element is one text block
// ([resultElementIsText]). Estimating, est takes both as the estimate's rule
// reads them, and other every other key ([lineDecoder.estElement]); else
// every other key is passed over.
func (d *lineDecoder) resultElement(scan *jsonscan.Scanner, est *estimateObject,
	other func(name []byte, k jsonscan.Kind) error) error {
	b := &d.block
	text := valueKind(0)
	b.resultType = b.resultType[:0]
	err := d.members(scan, func(name []byte, k jsonscan.Kind) error {
		switch string(name) {
		case "type":
			err := d.hold(scan, k, "message.content.content.type", &b.resultType)
			if est != nil {
				est.typ = estimateTypeOf(b.resultType, estimateElementTypes[:])
			}
			return err
		case "text":
			text = scannedKind(k)
			var err error
			if k != jsonscan.String {
				err = scan.Skip()
			} else {
				err = d.resultText(scan)
			}
			if est != nil {
				est.text = estTextOf(scan, k)
			}
			return err
		}
		if other != nil {
			return other(name, k)
		}
		return scan.Skip()
	})
	if err != nil {
		return err
	}
	if resultElementIsText('{', present(b.resultType), text) {
		b.text = d.result.sum()
	}
	return nil
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

// toolResult reads a user record's own `toolUseResult`, whose first token, k,
// the scanner stands at, into r: of an object, whether it carries a `handback`
// or a `handbackReport`, any value, and what its `status`, `agentId` and
// `success`, held as written until it ends, say of a subagent's leg
// ([answerOf]); every other key (a file's base64, a command's output, a patch)
// passed over, and every other kind nothing.
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

// attachment reads an attachment record's `attachment`, whose first token, k,
// the scanner stands at, into a: its kind; of an object, its `type`,
// `toolUseID`, `by` and `hookEvent` kept as written; what every shape rule asks
// of its lists, every rule's at once, since its type may be its last key
// ([threadLine.AttachmentWellFormed]); and which asked take tokens a hook
// record's string `content` ([hookRecordPrefix]), and a hook_success's string
// `stdout`, name ([lineDecoder.takes]). No element of a list is held.
func (d *lineDecoder) attachment(scan *jsonscan.Scanner, k jsonscan.Kind, a *attachmentFacts) error {
	a.kind = scannedKind(k)
	if k != jsonscan.ObjectStart {
		return scan.Skip()
	}
	// A listing that lacks its names is kept ([lineDecoder.skillNames]).
	a.namesKept = true
	err := d.members(scan, func(name []byte, k jsonscan.Kind) error {
		var err error
		switch string(name) {
		case "type":
			a.typ, err = d.keep(scan, k, "attachment.type")
		case "toolUseID":
			a.toolUseID, err = d.keep(scan, k, "attachment.toolUseID")
		case "by":
			a.by, err = d.keep(scan, k, "attachment.by")
		case "hookEvent":
			a.event, err = d.keep(scan, k, "attachment.hookEvent")
		case "taskDelivery", "reads":
			switch {
			case !d.estimating:
				err = scan.Skip()
			case string(name) == "reads":
				a.pdf, err = d.pdfReads(scan, k)
			default:
				a.images, err = d.taskImages(scan, k)
			}
		case "stdout":
			a.stdoutTokens, err = d.takes(scan, k)
		case "content":
			if k == jsonscan.String && len(d.asked) > 0 {
				a.content, a.contentStrings, a.contentObjects = scannedKind(k), false, false
				a.contentTokens, err = d.takes(scan, k)
				break
			}
			a.contentTokens = 0
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
	// A CONTENT'S TOKENS ARE A HOOK RECORD'S ALONE, and a stdout's a
	// hook_success's, by its last type, which may come after them.
	typ, typErr := wireRequiredString("type", a.typ)
	if typErr != nil || !strings.HasPrefix(typ, hookRecordPrefix) {
		a.contentTokens = 0
	}
	if typErr != nil || typ != hookSuccess {
		a.stdoutTokens = 0
	}
	return err
}

// takes is which of d's asked take tokens the value the scanner stands at,
// of kind k, names where it is a string ([textTakes]), its decoded text read
// a piece at a time until every asked token is met, the rest left to the
// scanner's next call; none, the value passed over, for any other value or
// where nothing is asked. Bit i for asked[i].
func (d *lineDecoder) takes(scan *jsonscan.Scanner, k jsonscan.Kind) (uint64, error) {
	if k != jsonscan.String || len(d.asked) == 0 {
		return 0, scan.Skip()
	}
	takes := textTakes{asked: d.asked}
	for all := ^uint64(0) >> (64 - len(d.asked)); takes.bits != all; {
		n, err := scan.Read(d.text[:])
		takes.write(d.text[:n])
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, err
		}
	}
	return takes.bits, nil
}

// skillNames reads a skill listing's `names`, whose first token, k, the
// scanner stands at: kept where it is a list of at most [skillListingMaxNames]
// strings, each kept by the rule of a name ([skillNameKept]), read in order
// and the first that is not deciding it; and unmeasured instead where every
// name is kept but one past [skillListingMaxNameUnits] held a byte that is no
// UTF-8, which the layer cannot measure ([errSkillNameUnmeasured]). Each name
// streams into its length in UTF-16 code units ([textUnits]) and whether its
// bytes as written were UTF-8 ([jsonscan.Scanner.InvalidUTF8]), which its
// decoded text cannot say.
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
// token, k, the scanner stands at: of an object, its `path`'s and
// `displayPath`'s kinds and its `why` held as written, each by its last
// occurrence, decided by the rule of such an entry ([withheldKept]) once it
// ends; anything else no entry the rule keeps.
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
// token, k, the scanner stands at, into c: its kind; of an object, its
// `preservedMessages` ([lineDecoder.preservedMessages]) and its
// `preservedSegment` kept as written.
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
// token, k, the scanner stands at, into c, its shapes checked in its order
// though its keys come in any: the list's kind, none where it is null; an
// object's `anchorUuid`, held as written until it ends; then its `uuids`, each
// entry read by its opening into a canonical uuid or none
// ([threadPreserved.list]), in order, up to the first that is no string. Where
// the anchor is no string the list holds no entry, as the list's walk never
// began.
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
// the scanner stands at, into f: of a list, each entry read by its opening
// into a canonical prefix or none ([threadRetracted.entry]), in order, up to
// the first that is no string, that entry's error; anything else the list's
// own.
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
// first token, k, the scanner stands at, into r: its kind; of a list, EVERY
// element counted, and each read by the rule of an entry ([replacedID]) until
// one fails it, its `kind` and `toolUseId` held as written until it ends, the
// id of each that replaces a tool's result kept, in order.
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

// batch reads an assistant record's `batchToolUses`, whose first token, k, the
// scanner stands at, into b: that it is there; of a list, each entry's `id`,
// held as written until the entry ends, read by the rule of an entry
// ([batchEntryID]), in order, up to the first it refuses, that entry's error;
// anything else the list's own.
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
