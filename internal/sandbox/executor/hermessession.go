package executor

import (
	"archive/tar"
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"time"

	"github.com/crewlet/crewlet/internal/jsonscan"
)

// A HERMES SESSION'S RECORD, as the carry script dumps it ([hermesCarryScript]):
// JSON lines, the store's schema first, then rows, each `{"t": <table>, "r":
// <row>}`. The readers here take the `messages` rows alone, in the order the
// store wrote them, the session's compaction lineage after it, on the bounded
// line walk ([hermesReading], [hermesNotesHeld]): no row is held, and a value
// past [TranscriptValueMaxBytes] streams into its entry.

// hermesRowMaxBytes bounds one row of a Hermes session's record, its break
// among it. Hermes cuts a tool's output it keeps to a size far below this; a
// longer row is not a shape this build read, so it is said and passed over.
const hermesRowMaxBytes = 8 << 20

// A HERMES ROW IS READ AS ITS WHOLE-LINE DECODE READ IT: a line that is no
// row (the schema, the launch, one torn, one a member's field refuses) is
// passed over unsaid ([lineWalk.quiet]); a `messages` row's members are each
// read by its field's kind ([fieldKind]). Its
// content is a string or its `text` parts joined, and none where its list's
// decode refuses it; its tool_calls a JSON list, or a string holding one,
// read by a scanner of its own over the string's words ([hermesReading.calls]),
// none where either refuses them. Past the value bound, a row's text that
// opens with a steered note's framing is read without it, and without the
// framing's close where that ends it ([textFrame]), whatever the row.

// hermesParts is how Hermes joins a list content's parts: every `text`
// part's words, one after the other.
var hermesParts = joinRule{kind: "text"}

// transcript reads a Hermes archive ([hermesArchiveFormat]): its one member's
// messages, each a person's brief or note, the model's words, a call, a
// result or a compaction's summary.
func (Hermes) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
	tr := tar.NewReader(archive)
	b.handed.r = tr
	r := newHermesReading(b, bufio.NewReaderSize(&b.handed, lineChunkBytes))
	for {
		h, err := tr.Next()
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return fmt.Errorf("%w: %w", errNotTar, err)
		}
		if h.Typeflag != tar.TypeReg || h.Name != hermesArchiveMember {
			continue
		}
		r.framed = 0
		r.lines.Reset(&b.handed)
		clear(r.uids)
		if err := r.member(ctx); err != nil {
			return err
		}
	}
}

// hermesReading is a person's transcript of one dumped record on the bounded
// line walk, in recorded order: a compaction's summary as the summary, a
// person's message as a brief or the note it frames, the model's words and
// calls, a tool's result. A row whose message_uid an earlier row carried is a
// copy a compaction wrote again (the kept tail, in place or in a rotated
// child, measured on 0.23.3), and is not read twice; each uid is kept as a
// 16-byte digest, to [hermesUIDsMax] of them, past which the read ends said.
type hermesReading struct {
	lineWalk
	uids map[[16]byte]struct{}
	// The envelope: its table, whether it holds a row (r) and whether that
	// row is an object or null, as a message's decode takes one.
	table          string
	mistyped       bool
	rowSet, rowAny bool
	// The message: whether its decode refused a member, whether it is a
	// compaction's summary, its uid, role, words, answered call and tool, and
	// when it was recorded.
	rowMistyped bool
	summary     bool
	uid         recordWord
	role        string
	words       textJoin
	wordsTook   bool
	frame       textFrame
	callID      string
	tool        toolName
	toolBlank   bool
	at          time.Time
	// calls is the message's calls, callsTook whether their decode took them.
	calls     callList
	callsTook bool
	call      hermesCallRead
	// inner reads a tool_calls string's words as JSON, through innerSrc.
	inner    *jsonscan.Scanner
	innerSrc stringWords
	// name is a tool's name, input a call's arguments, as read.
	name, input valueText
	compactor   jsonCompactor
	s           entryShape
}

// hermesCallRead is one call an assistant message made, in either shape Hermes
// stores: OpenAI's `function` object, or its own flat one.
type hermesCallRead struct {
	id             string
	name, fnName   toolName
	blank, fnBlank bool
	function       bool
	args           hermesArgsFrom
	argsNull       bool
}

// hermesArgsFrom is which member a call's arguments were read from: its own
// `arguments` wins over its function's.
type hermesArgsFrom uint8

const (
	hermesArgsNone hermesArgsFrom = iota
	hermesArgsOwn
	hermesArgsFunction
)

// newHermesReading is a person's reading into b of the records lines reads.
func newHermesReading(b *transcriptBuilder, lines *bufio.Reader) *hermesReading {
	r := &hermesReading{uids: map[[16]byte]struct{}{}}
	r.setupBounded(b, r, lines)
	r.bound, r.src.bound = hermesRowMaxBytes, hermesRowMaxBytes
	r.quiet = true
	return r
}

func (*hermesReading) own(*valueText) error { return nil }

func (r *hermesReading) linesPast() error { return r.b.linesPast("Hermes session", "rows") }

// start starts the record the next line holds.
func (r *hermesReading) start() {
	r.table, r.mistyped, r.rowSet, r.rowAny = "", false, false, false
	r.rowStart()
}

// rowStart starts the line's row: one written again replaces it.
func (r *hermesReading) rowStart() {
	r.rowMistyped, r.summary, r.uid, r.role = false, false, recordWord{}, ""
	r.wordsStart(false)
	r.wordsTook = true
	r.callID, r.tool, r.toolBlank, r.at = "", toolName{}, true, time.Time{}
	r.calls.reset(r.b.seen)
	r.callsTook = false
}

// wordsStart starts the row's words, shaped where shaped says.
func (r *hermesReading) wordsStart(shaped bool) {
	r.words.start(r.b, TranscriptEntry{Kind: EntryModel}, shaped, hermesParts)
	r.frame.frame(hermesNoteOpen, hermesNoteClose)
	r.words.frame = &r.frame
}

// record reads a line's envelope: its table and its row.
func (r *hermesReading) record() error {
	return r.members(func(name string, k jsonscan.Kind) error {
		switch name {
		case "t":
			table, ok, err := r.wordField(k)
			r.table, r.mistyped = table, r.mistyped || !ok
			return err
		case "r":
			r.rowStart()
			r.rowSet, r.rowAny = true, k == jsonscan.ObjectStart || k == jsonscan.Null
			if k != jsonscan.ObjectStart || r.table != "" && r.table != "messages" {
				return r.scan.Skip()
			}
			return r.members(r.messageMember)
		}
		return r.scan.Skip()
	})
}

// messageMember reads one member of a `messages` row.
func (r *hermesReading) messageMember(name string, k jsonscan.Kind) error {
	b := r.b
	ok := true
	var err error
	switch name {
	case "id", "active", "compacted":
		_, ok, err = r.intField(k)
	case "_compressed_summary":
		var n int64
		n, ok, err = r.intField(k)
		r.summary = n != 0
	case "finish_reason", "display_kind":
		ok, err = r.typedField(k, fieldString)
	case "message_uid":
		r.uid, ok, err = r.idField(k)
	case "role":
		r.role, ok, err = r.wordField(k)
	case "tool_call_id":
		r.callID, ok, err = r.wordField(k)
	case "tool_name":
		r.tool, r.toolBlank, ok, err = r.nameField(k)
	case "content":
		shaped := b.shapes(EntrySummary, b.seen) || b.shapes(EntryBrief, b.seen) || b.shapes(EntryModel, b.seen) ||
			b.shapes(EntryToolResult, b.seen)
		r.wordsStart(shaped)
		r.wordsTook, err = r.content(k)
	case "tool_calls":
		r.calls.reset(b.seen)
		r.callsTook, err = r.toolCalls(k)
	case "timestamp":
		r.at, err = r.instant(k)
	default:
		return r.scan.Skip()
	}
	r.rowMistyped = r.rowMistyped || !ok
	return err
}

// nameField reads a tool's name, the string value k begins, and whether it is
// blank (null among them), and reports whether a string field takes it.
func (r *hermesReading) nameField(k jsonscan.Kind) (toolName, bool, bool, error) {
	n, ok, err := r.toolName(&r.name, k)
	return n, k != jsonscan.String || r.name.blank, ok, err
}

// content reads a row's content as its whole-line decode read it: a string its
// words, a list its `text` parts joined, and anything else none. It reports
// whether the list's decode, if a list, takes it.
func (r *hermesReading) content(k jsonscan.Kind) (bool, error) {
	switch k {
	case jsonscan.String:
		return true, r.wholeText(&r.words)
	case jsonscan.ArrayStart:
	default:
		return true, r.scan.Skip()
	}
	took := true
	err := r.elements(func(k jsonscan.Kind) (bool, error) {
		ok, err := r.joinPart(&r.words, k, nil)
		took = took && ok
		return true, err
	})
	return took, err
}

// instant is when a row was recorded: seconds since the epoch, a number or a
// numeral; zero where it holds none.
func (r *hermesReading) instant(k jsonscan.Kind) (time.Time, error) {
	var written string
	switch k {
	case jsonscan.Number:
		raw, err := r.numberAsWritten(k)
		if err != nil || raw == nil {
			return time.Time{}, err
		}
		written = string(raw)
	case jsonscan.String:
		word, err := r.word()
		if err != nil {
			return time.Time{}, err
		}
		written = word
	default:
		return time.Time{}, r.scan.Skip()
	}
	return hermesInstant(written), nil
}

// hermesInstant is seconds since the epoch, written as a numeral, as a time;
// zero for none, or for one not past the epoch.
func hermesInstant(written string) time.Time {
	f, err := strconv.ParseFloat(written, 64)
	if err != nil || f <= 0 || math.IsInf(f, 0) || math.IsNaN(f) {
		return time.Time{}
	}
	sec, frac := math.Modf(f)
	return time.Unix(int64(sec), int64(frac*1e9)).UTC()
}

// toolCalls reads a message's tool_calls into r.calls: a JSON list, or a
// string holding one, read by a scanner of its own over its words. It
// reports whether their decode takes them; null or any other value is none.
func (r *hermesReading) toolCalls(k jsonscan.Kind) (bool, error) {
	switch k {
	case jsonscan.ArrayStart:
		return r.callList(k)
	case jsonscan.String:
	default:
		return false, r.scan.Skip()
	}
	outer := r.scan
	r.innerSrc = stringWords{scan: outer}
	if r.inner == nil {
		r.inner = jsonscan.New(&r.innerSrc)
	} else {
		r.inner.Reset(&r.innerSrc)
	}
	r.scan = r.inner
	took, err := r.innerCalls()
	r.scan = outer
	if r.innerSrc.err != nil {
		return false, r.innerSrc.err
	}
	if _, scanned := errors.AsType[*jsonscan.Error](err); err != nil && !scanned && !errors.Is(err, io.EOF) {
		return false, err
	}
	if err != nil {
		took = false
	}
	// The string's words left past a fault are passed over.
	for {
		_, readErr := outer.Read(r.piece)
		switch {
		case errors.Is(readErr, io.EOF):
			return took, nil
		case readErr != nil:
			return false, readErr
		}
	}
}

// innerCalls reads the one JSON list a tool_calls string holds, nothing but
// blanks after it.
func (r *hermesReading) innerCalls() (bool, error) {
	k, err := r.scan.Next()
	if err != nil {
		return false, err
	}
	took := false
	switch k {
	case jsonscan.ArrayStart:
		if took, err = r.callList(k); err != nil {
			return false, err
		}
	default:
		if err = r.scan.Skip(); err != nil {
			return false, err
		}
	}
	if _, err = r.scan.Next(); !errors.Is(err, io.EOF) {
		return false, err
	}
	return took, nil
}

// callList reads a list of calls into r.calls, and reports whether its decode
// takes it: each element an object of its fields' kinds, or null.
func (r *hermesReading) callList(k jsonscan.Kind) (bool, error) {
	took := true
	_, err := r.listField(k, func(k jsonscan.Kind) error {
		ok, err := r.oneCall(k)
		took = took && ok
		return err
	})
	return took, err
}

// oneCall reads one call and adds it to r.calls ([hermesCallRead]), and
// reports whether its struct takes it.
func (r *hermesReading) oneCall(k jsonscan.Kind) (bool, error) {
	c := &r.call
	*c = hermesCallRead{blank: true, fnBlank: true}
	shaped := r.calls.shapes(r.b)
	took := true
	ok, err := r.objectField(k, func(name string, k jsonscan.Kind) error {
		var ok bool
		var err error
		switch name {
		case "id":
			c.id, ok, err = r.wordField(k)
		case "name":
			c.name, c.blank, ok, err = r.nameField(k)
		case "arguments":
			ok, err = true, r.args(hermesArgsOwn, k, shaped)
		case "function":
			if c.args == hermesArgsFunction {
				c.args = hermesArgsNone
			}
			c.function, c.fnName, c.fnBlank = k == jsonscan.ObjectStart, toolName{}, true
			ok, err = r.objectField(k, func(name string, k jsonscan.Kind) error {
				var fnOK bool
				var fnErr error
				switch name {
				case "name":
					c.fnName, c.fnBlank, fnOK, fnErr = r.nameField(k)
				case "arguments":
					if c.args == hermesArgsOwn {
						return r.scan.Skip()
					}
					fnOK, fnErr = true, r.args(hermesArgsFunction, k, shaped)
				default:
					return r.scan.Skip()
				}
				took = took && fnOK
				return fnErr
			})
		default:
			return r.scan.Skip()
		}
		took = took && ok
		return err
	})
	if err != nil || !ok || !took {
		return ok && took, err
	}
	// THE TOOL a call names: its own name, or where it has a function and its
	// own is blank, the function's; a blank one answers its result with none.
	tool, blank := c.name, c.blank
	if c.function && c.blank {
		tool, blank = c.fnName, c.fnBlank
		if blank {
			tool = toolName{}
		}
	}
	if blank {
		tool.raw = ""
	}
	var in *valueText
	switch {
	case c.args == hermesArgsNone || c.argsNull:
	default:
		in = &r.input
	}
	r.calls.add(r.b, in, tool, c.id)
	return true, nil
}

// args reads a call's arguments from one of its members, as its whole-line
// decode read them: a string as written, null none, any other value compacted.
func (r *hermesReading) args(from hermesArgsFrom, k jsonscan.Kind, shaped bool) error {
	c := &r.call
	c.args, c.argsNull = from, k == jsonscan.Null
	switch k {
	case jsonscan.Null:
		return nil
	case jsonscan.String:
		r.input.begin(r.b, TranscriptEntry{Kind: EntryToolCall}, shaped)
		return r.read(&r.input)
	}
	r.input.begin(r.b, TranscriptEntry{Kind: EntryToolCall}, shaped)
	r.compactor = jsonCompactor{w: &r.input, buf: r.compactor.buf}
	if err := r.scan.Tee(&r.compactor); err != nil {
		return err
	}
	return r.scan.Skip()
}

// commit commits the entries the row makes: none for a line that is no
// `messages` row, or a copy of one read.
func (r *hermesReading) commit() error {
	b, s := r.b, &r.s
	if r.mistyped || r.table != "messages" || !r.rowSet || !r.rowAny || r.rowMistyped ||
		hermesCopy(r.uids, r.uid) {
		return nil
	}
	if len(r.uids) > hermesUIDsMax {
		return b.uidsPast()
	}
	s.reset(b.seen)
	switch {
	case r.summary:
		r.prose(s, EntrySummary)
	case r.role == "user":
		r.prose(s, EntryBrief)
	case r.role == "assistant":
		words := r.prose(s, EntryModel)
		if err := b.commitShape(s, r.at); err != nil || !r.callsTook {
			return err
		}
		return r.calls.commitAfter(b, words, r.at)
	case r.role == "tool":
		tool := r.tool.shown
		if r.toolBlank || r.tool.refused {
			tool = ""
		}
		s.addEvent(b, r.result(s), time.Time{}, claudeEvent{kind: eventResult, id: r.callID, tool: tool})
	}
	return b.commitShape(s, r.at)
}

// left reports whether the row's words ran past the value bound, so its
// frame has seen them ([textFrame]).
func (r *hermesReading) left() bool { return r.words.stream != nil || r.words.over }

// prose adds to s the entries the row's words make as kind
// ([transcriptBuilder.proseInto]): a person's note unframed, and past the
// bound one entry (R1), none where blank once unframed, or said where lost
// (R2); and reports whether it added one.
func (r *hermesReading) prose(s *entryShape, kind EntryKind) bool {
	b, j := r.b, &r.words
	n := s.n
	text, held := j.text()
	switch {
	case !r.wordsTook:
	case j.lost:
		s.add(b, j.entry(s.next()), time.Time{})
	case held:
		if note, ok := hermesSteered(text); ok && kind == EntryBrief {
			text = note
		}
		b.proseInto(s, kind, text)
	default:
		r.frame.end()
		if r.frame.worded {
			s.add(b, textAs(b, j, s, kind), time.Time{})
		}
	}
	return s.n > n
}

// result is the entry a tool's row's words make at s's next ordinal.
func (r *hermesReading) result(s *entryShape) pendingEntry {
	b, j := r.b, &r.words
	switch {
	case !r.wordsTook:
		return b.held(TranscriptEntry{Kind: EntryToolResult}, "", s.next())
	case r.left() && !j.lost:
		r.frame.end()
	}
	return textAs(b, j, s, EntryToolResult)
}

// uidsPast says where a read of a Hermes record past [hermesUIDsMax] uids
// ended, and ends it there, as one past its lines bound ends
// ([transcriptBuilder.linesPast]).
func (b *transcriptBuilder) uidsPast() error {
	if err := b.unread(fmt.Sprintf("a part of the Hermes session past the %d message ids one read keeps was "+
		"not read, and nothing after it could be", hermesUIDsMax)); err != nil {
		return err
	}
	b.cut = CutRead
	return errTranscriptPage
}

// hermesCopy reports whether a row of uid copies one read before it, keeping
// uid's 16-byte digest where it does not; a row of no uid copies none.
func hermesCopy(read map[[16]byte]struct{}, uid recordWord) bool {
	if uid.empty() {
		return false
	}
	sum := uid.sum
	if !uid.long {
		sum = sha256.Sum256([]byte(uid.text))
	}
	key := [16]byte(sum[:16])
	if _, ok := read[key]; ok {
		return true
	}
	read[key] = struct{}{}
	return false
}

// stringWords is a string's words as the scanner reading it hands them: what
// a scanner of a JSON text a string holds reads ([hermesReading.toolCalls]).
// err keeps the outer scan's own failure, which no inner fault hides.
type stringWords struct {
	scan *jsonscan.Scanner
	err  error
}

func (s *stringWords) Read(p []byte) (int, error) {
	n, err := s.scan.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		s.err = err
	}
	return n, err
}
