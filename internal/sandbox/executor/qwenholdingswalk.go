package executor

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/jsonscan"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools/toolargs"
)

// qwenStepCallsMax is the most calls a step of a Qwen Code session holds that
// a reading reads: past it, no reading. 4,096 keeps the open step's calls
// within 1.4 MiB at the longest id a reading reads ([qwenCallIDMost]).
const qwenStepCallsMax = 4096

// qwenCallIDMost is the longest call id a reading reads, in bytes: 256, past
// any id a provider hands Qwen Code.
const qwenCallIDMost = 256

// qwenNameMost and qwenWordMost are the longest member name and short value
// (a type, a status, a uuid) a reading compares; a longer one is none of its.
const (
	qwenNameMost = 32
	qwenWordMost = 64
)

// The reading's words for a chat file it does not read.
var (
	errQwenRecord = &holdingsError{why: "a line of the session's chat file is not one JSON object"}
	errQwenShape  = &holdingsError{why: "a record of the session's chat file is of a shape this build does not read"}
	errQwenTwice  = &holdingsError{why: "a key of the session's chat file is written twice"}
	errQwenChain  = &holdingsError{why: "the session's chat file is not the one chain of records its load walks, " +
		"each uuid once"}
	errQwenSession = &holdingsError{why: "a record of the session's chat file names another session"}
	errQwenRelease = &holdingsError{why: "a record of the session's chat file is another release's"}
	errQwenBrief   = &holdingsError{why: "the line after the one the run began after is not its brief"}
	errQwenShort   = &holdingsError{why: "the session's chat file is shorter than the lines its run began after"}
	errQwenCalls   = &holdingsError{why: "a step of the session's chat file holds a call this build does not read"}
	errQwenAnswer  = &holdingsError{why: "an answer in the session's chat file is to no unanswered call of its step"}
)

// qwenKeys is which of a frame's names an object held, whether one was held
// twice, and whether it held another.
type qwenKeys struct {
	seen         uint32
	twice, other bool
}

// add keeps name index i, -1 for none of the frame's.
func (k *qwenKeys) add(i int) {
	if i < 0 {
		k.other = true
		return
	}
	if k.seen&(1<<i) != 0 {
		k.twice = true
	}
	k.seen |= 1 << i
}

// exactly reports whether the object held mask's names, each once, and no
// other.
func (k qwenKeys) exactly(mask uint32) bool { return k.seen == mask && !k.other && !k.twice }

// qwenFrame is the kind of object a reading walks the members of.
type qwenFrame uint8

const (
	qwenFrameRecord qwenFrame = iota
	qwenFrameMessage
	qwenFramePart
	qwenFrameCall
	qwenFrameAnswer
	qwenFrameResponse
	qwenFrameResult
	qwenFramePayload
	qwenFrameOther
)

// The names each frame reads, each its index in [qwenFrameNames].
const (
	qwRecUUID = iota
	qwRecParent
	qwRecSession
	qwRecTimestamp
	qwRecType
	qwRecProvenance
	qwRecCwd
	qwRecVersion
	qwRecExecution
	qwRecMessage
	qwRecPrompt
	qwRecSubtype
	qwRecPayload
	qwRecModel
	qwRecUsage
	qwRecResult
)

const (
	qwMsgRole = iota
	qwMsgParts
)

const (
	qwPartText = iota
	qwPartThought
	qwPartCall
	qwPartAnswer
)

const (
	qwCallID = iota
	qwCallName
	qwCallArgs
)

const (
	qwAnswerID = iota
	qwAnswerName
	qwAnswerResponse
)

const (
	qwRespOutput = iota
	qwRespError
)

const (
	qwResCallID = iota
	qwResStatus
	qwResDisplay
	qwResError
	qwResErrorType
	qwResExecution
)

const qwPayloadHistory = 0

// qwenFrameNames are the names each frame reads.
var qwenFrameNames = [...][]string{
	qwenFrameRecord: {"uuid", "parentUuid", "sessionId", "timestamp", "type", "provenance", "cwd", "version",
		"executionContext", "message", "promptId", "subtype", "systemPayload", "model", "usageMetadata",
		"toolCallResult"},
	qwenFrameMessage:  {"role", "parts"},
	qwenFramePart:     {"text", "thought", "functionCall", "functionResponse"},
	qwenFrameCall:     {"id", "name", "args"},
	qwenFrameAnswer:   {"id", "name", "response"},
	qwenFrameResponse: {"output", "error"},
	qwenFrameResult:   {"callId", "status", "resultDisplay", "error", "errorType", "executionStatus"},
	qwenFramePayload:  {"compressedHistory"},
	qwenFrameOther:    {},
}

// The member sets a record holds, by its type and subtype (0.28.4's, measured):
// every record's eight, and its kind's.
var (
	qwenBase = namesMask(qwRecUUID, qwRecParent, qwRecSession, qwRecTimestamp, qwRecType, qwRecProvenance, qwRecCwd,
		qwRecVersion)
	qwenUserNames      = qwenBase | namesMask(qwRecExecution, qwRecMessage, qwRecPrompt)
	qwenNoteNames      = qwenBase | namesMask(qwRecExecution, qwRecSubtype, qwRecMessage, qwRecPayload)
	qwenAssistantNames = qwenBase | namesMask(qwRecModel, qwRecMessage, qwRecUsage)
	qwenResultNames    = qwenBase | namesMask(qwRecMessage, qwRecResult)
	qwenSystemNames    = qwenBase | namesMask(qwRecSubtype, qwRecPayload)
	qwenMessageNames   = namesMask(qwMsgRole, qwMsgParts)
	qwenCallNames      = namesMask(qwCallID, qwCallName, qwCallArgs)
	qwenAnswerNames    = namesMask(qwAnswerID, qwAnswerName, qwAnswerResponse)
	qwenSuccessNames   = namesMask(qwResCallID, qwResStatus, qwResDisplay, qwResExecution)
	qwenFailureNames   = namesMask(qwResCallID, qwResStatus, qwResDisplay, qwResError, qwResErrorType, qwResExecution)
)

// qwenKind is a record's type and subtype, as a reading reads them.
type qwenKind uint8

const (
	qwenKindOther qwenKind = iota
	qwenKindUser
	qwenKindNote
	qwenKindAssistant
	qwenKindResult
	qwenKindSystem
	qwenKindCompression
)

// qwenTypes are the record types 0.28.4 writes, each its index in a line's
// facts from 1; qwenSubtypes the subtypes, a person's note's first, then a
// system record's, chat_compression last.
var (
	qwenTypes    = []string{"user", "assistant", "tool_result", "system"}
	qwenSubtypes = []string{"mid_turn_user_message", "session_approval_mode", "file_history_snapshot",
		"attribution_snapshot", "ui_telemetry", "branch_checkpoint", "chat_compression"}
)

// The indexes of [qwenTypes] and of a note's and a compaction's subtype.
const (
	qwenTypeUser = 1 + iota
	qwenTypeAssistant
	qwenTypeResult
	qwenTypeSystem
)

const qwenSubtypeNote = 1

var qwenSubtypeCompression = len(qwenSubtypes)

// qwenWordIndex is word's index in words from 1, 0 for none of them.
func qwenWordIndex(words []string, word []byte) int {
	for i, w := range words {
		if string(word) == w {
			return i + 1
		}
	}
	return 0
}

// qwenStepCall is a call of the open step: the engine's tool its name names
// (named, its keys exactly Qwen Code's three), its arguments' key (argsOK),
// and whether an answer to it was read.
type qwenStepCall struct {
	tool                    string
	named, argsOK, answered bool
	args                    [32]byte
}

// qwenLineCall is a call as its line is read: its id, whether the id is one
// a reading reads, and its keys.
type qwenLineCall struct {
	id   string
	idOK bool
	keys qwenKeys
	call qwenStepCall
}

// qwenAnswer is what a reading keeps of a line's tool answer, decided at its
// end ([qwenHoldingsWalk.answer]).
type qwenAnswer struct {
	keys                  qwenKeys
	id                    [qwenCallIDMost]byte
	idN                   int
	idOK                  bool
	tool                  string
	named                 bool
	response              qwenKeys
	text, failed          bool
	result                qwenKeys
	callID                [qwenCallIDMost]byte
	callIDN               int
	callIDOK              bool
	succeeded, erred      bool
	ranSuccess, ranFailed bool
	empty, mcp            bool
}

// qwenLine is what a reading keeps of the line it is in, decided at its end
// ([qwenHoldingsWalk.decide]).
type qwenLine struct {
	keys                 qwenKeys
	uuid                 [36]byte
	uuidOK               bool
	parentNull, parentOK bool
	sessionOK, releaseOK bool
	typ, subtype         int
	message              bool
	msgKeys              qwenKeys
	roleUser             bool
	parts                bool
	text, thought        int
	calls, answers, bad  int
	payload              qwenKeys
	history              jsonscan.Kind
	answer               qwenAnswer
}

// qwenHoldingsWalk is one reading's state: the session, the run's start in
// lines, the chain so far (the previous uuid, every uuid kept once), the open
// step's calls, each class's parts ([classParts]) and whether a compaction
// followed the start; and the line being read, which only its end applies.
type qwenHoldingsWalk struct {
	session, release string
	start            int64
	began            bool
	prev             [36]byte
	ids              onceEach
	step             int
	open             map[string]*qwenStepCall
	stepCalls        []qwenStepCall
	counted          []*classParts
	classes          map[sandbox.CallClass]*classParts
	candidates       map[partKey][]sandbox.CallClass
	compacted        bool
	text             *textDigest
	sc               *jsonscan.Scanner
	l                qwenLine
	calls            []qwenLineCall
	pt               qwenPart
	name             [qwenNameMost + utf8.UTFMax]byte
	word             [qwenWordMost + utf8.UTFMax]byte
	idBuf            [qwenCallIDMost + utf8.UTFMax]byte
}

// newQwenHoldingsWalk is a walk over the session from start, for ask's pool,
// as [newOpenCodeHoldingsWalk] builds it.
func newQwenHoldingsWalk(session string, start sandbox.RunStart, ask sandbox.HoldingsAsk) *qwenHoldingsWalk {
	w := &qwenHoldingsWalk{session: session, release: ask.Resumer, start: start.Lines, began: start.Began,
		open: map[string]*qwenStepCall{}, classes: map[sandbox.CallClass]*classParts{},
		candidates: map[partKey][]sandbox.CallClass{}, sc: jsonscan.New(nil)}
	var openings []int
	for _, c := range ask.Calls {
		class, ok := sandbox.ClassOf(c)
		if !ok {
			continue
		}
		parts := w.classes[class]
		if parts == nil {
			parts = &classParts{}
			w.classes[class] = parts
			key := partKey{name: class.Name, args: class.Args, failed: class.Failed}
			w.candidates[key] = append(w.candidates[key], class)
			if !class.Text.Whole {
				openings = append(openings, class.Text.Opening)
			}
		}
		parts.calls++
	}
	w.text = newTextDigest(openings)
	return w
}

// line reads line n, body, as one record, and applies it once body has
// answered io.EOF ([chatLines]).
func (w *qwenHoldingsWalk) line(ctx context.Context, n int, body io.Reader) error {
	w.l, w.calls = qwenLine{}, w.calls[:0]
	w.sc.Reset(body)
	kind, err := w.sc.Next()
	if err != nil {
		return qwenScanFailed(err)
	}
	if kind != jsonscan.ObjectStart {
		return errQwenRecord
	}
	if err = w.members(ctx, qwenFrameRecord, &w.l.keys); err != nil {
		return qwenScanFailed(err)
	}
	if _, err = w.sc.Next(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errQwenRecord
		}
		return qwenScanFailed(err)
	}
	return w.decide(n)
}

// qwenScanFailed is why a line read as no record: the reading's own words, or
// the scanner's or its reader's, which [holdingsError] cuts.
func qwenScanFailed(err error) error {
	var own *holdingsError
	if errors.As(err, &own) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return &holdingsError{why: errQwenRecord.why, cause: err}
}

// members walks the members of the object the scanner is at the start of,
// keeping their names in keys: a name of the frame's written twice is no
// reading, as is anything the reading cannot know is one value.
func (w *qwenHoldingsWalk) members(ctx context.Context, f qwenFrame, keys *qwenKeys) error {
	for {
		kind, err := w.sc.Next()
		if err != nil {
			return err
		}
		if kind == jsonscan.ObjectEnd {
			return nil
		}
		name, ok, err := qwenText(w.sc, w.name[:], qwenNameMost)
		if err != nil {
			return err
		}
		i := -1
		if ok {
			i = qwenNameIndex(f, name)
		}
		if keys.add(i); keys.twice {
			return errQwenTwice
		}
		if kind, err = w.sc.Next(); err != nil {
			return err
		}
		if err = w.member(ctx, f, i, kind); err != nil {
			return err
		}
	}
}

// qwenNameIndex is name's index among f's names, -1 for none.
func qwenNameIndex(f qwenFrame, name []byte) int {
	for i, n := range qwenFrameNames[f] {
		if string(name) == n {
			return i
		}
	}
	return -1
}

// qwenText reads sc's current name or string into buf, false where its decoded
// text runs past most bytes (the rest left to the scanner's next call).
func qwenText(sc *jsonscan.Scanner, buf []byte, most int) ([]byte, bool, error) {
	n := 0
	for {
		m, err := sc.Read(buf[n:])
		n += m
		switch {
		case errors.Is(err, io.EOF):
			return buf[:n], n <= most, nil
		case err != nil:
			return nil, false, err
		case n > most:
			return nil, false, nil
		}
	}
}

// wordOf is the current value's text where it is a string of at most
// [qwenWordMost] bytes.
func (w *qwenHoldingsWalk) wordOf(kind jsonscan.Kind) ([]byte, bool, error) {
	if kind != jsonscan.String {
		return nil, false, w.sc.Skip()
	}
	return qwenText(w.sc, w.word[:], qwenWordMost)
}

// member keeps member i of frame f, its value of kind, passing every value it
// does not read.
func (w *qwenHoldingsWalk) member(ctx context.Context, f qwenFrame, i int, kind jsonscan.Kind) error {
	switch f {
	case qwenFrameRecord:
		return w.recordMember(ctx, i, kind)
	case qwenFrameMessage:
		return w.messageMember(ctx, i, kind)
	case qwenFramePart:
		return w.partMember(ctx, i, kind)
	case qwenFrameCall:
		return w.callMember(i, kind)
	case qwenFrameAnswer:
		return w.answerMember(ctx, i, kind)
	case qwenFrameResponse:
		if (i == qwRespOutput || i == qwRespError) && kind == jsonscan.String {
			w.l.answer.text, w.l.answer.failed = true, i == qwRespError
			return w.text.scan(w.sc)
		}
	case qwenFrameResult:
		return w.resultMember(ctx, i, kind)
	case qwenFramePayload:
		if i == qwPayloadHistory {
			w.l.history = kind
		}
	}
	return w.sc.Skip()
}

// recordMember keeps a record's identity, kind, release, message, payload and
// tool result.
func (w *qwenHoldingsWalk) recordMember(ctx context.Context, i int, kind jsonscan.Kind) error {
	l := &w.l
	switch i {
	case qwRecUUID:
		word, ok, err := w.wordOf(kind)
		if l.uuidOK = ok && qwenUUID(word); l.uuidOK {
			copy(l.uuid[:], word)
		}
		return err
	case qwRecParent:
		if kind == jsonscan.Null {
			l.parentNull = true
			return nil
		}
		word, ok, err := w.wordOf(kind)
		l.parentOK = ok && string(word) == string(w.prev[:])
		return err
	case qwRecSession:
		word, ok, err := w.wordOf(kind)
		l.sessionOK = ok && string(word) == w.session
		return err
	case qwRecVersion:
		word, ok, err := w.wordOf(kind)
		l.releaseOK = ok && string(word) == w.release
		return err
	case qwRecType, qwRecSubtype:
		word, ok, err := w.wordOf(kind)
		switch {
		case !ok:
		case i == qwRecType:
			l.typ = qwenWordIndex(qwenTypes, word)
		default:
			l.subtype = qwenWordIndex(qwenSubtypes, word)
		}
		return err
	case qwRecMessage:
		if kind == jsonscan.ObjectStart {
			l.message = true
			return w.members(ctx, qwenFrameMessage, &l.msgKeys)
		}
	case qwRecPayload:
		if kind == jsonscan.ObjectStart {
			return w.members(ctx, qwenFramePayload, &l.payload)
		}
	case qwRecResult:
		if kind == jsonscan.ObjectStart {
			return w.members(ctx, qwenFrameResult, &l.answer.result)
		}
	}
	return w.sc.Skip()
}

// messageMember keeps a message's role and its parts, asking ctx before each.
func (w *qwenHoldingsWalk) messageMember(ctx context.Context, i int, kind jsonscan.Kind) error {
	switch {
	case i == qwMsgRole:
		word, ok, err := w.wordOf(kind)
		w.l.roleUser = ok && string(word) == "user"
		return err
	case i != qwMsgParts || kind != jsonscan.ArrayStart:
		return w.sc.Skip()
	}
	w.l.parts = true
	for {
		kind, err := w.sc.Next()
		switch {
		case err != nil:
			return err
		case kind == jsonscan.ArrayEnd:
			return nil
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = w.part(ctx, kind); err != nil {
			return err
		}
	}
}

// qwenPart is the shape of the part being read: its text a string, its thought
// a boolean, its call or answer an object.
type qwenPart struct{ text, thought, call, answer bool }

// part reads one part of a message, and counts it by its shape: a text, a
// thought, a call or an answer, else a part no record of 0.28.4 holds.
func (w *qwenHoldingsWalk) part(ctx context.Context, kind jsonscan.Kind) error {
	l := &w.l
	if kind != jsonscan.ObjectStart {
		l.bad++
		return w.sc.Skip()
	}
	var keys qwenKeys
	calls := len(w.calls)
	w.pt = qwenPart{}
	if err := w.members(ctx, qwenFramePart, &keys); err != nil {
		return err
	}
	p := w.pt
	switch {
	case keys.exactly(namesMask(qwPartText)) && p.text:
		l.text++
	case keys.exactly(namesMask(qwPartText, qwPartThought)) && p.text && p.thought:
		l.thought++
	case keys.exactly(namesMask(qwPartCall)) && p.call && len(w.calls) == calls+1:
		l.calls++
	case keys.exactly(namesMask(qwPartAnswer)) && p.answer:
		l.answers++
	default:
		l.bad++
	}
	return nil
}

// partMember keeps a part's members by their kinds, reading a call or an
// answer whole.
func (w *qwenHoldingsWalk) partMember(ctx context.Context, i int, kind jsonscan.Kind) error {
	p := &w.pt
	switch i {
	case qwPartText:
		p.text = kind == jsonscan.String
	case qwPartThought:
		p.thought = kind == jsonscan.True || kind == jsonscan.False
	case qwPartCall:
		if kind != jsonscan.ObjectStart {
			break
		}
		if len(w.calls) == qwenStepCallsMax {
			return errQwenCalls
		}
		p.call = true
		w.calls = append(w.calls, qwenLineCall{})
		c := &w.calls[len(w.calls)-1]
		if err := w.members(ctx, qwenFrameCall, &c.keys); err != nil {
			return err
		}
		c.call.named = c.call.named && c.keys.exactly(qwenCallNames)
		return nil
	case qwPartAnswer:
		if kind == jsonscan.ObjectStart {
			p.answer = true
			return w.members(ctx, qwenFrameAnswer, &w.l.answer.keys)
		}
	}
	return w.sc.Skip()
}

// A call's arguments are captured whole wherever the log keeps them whole.
var _ [exportValueMaxBytes - sandbox.MaxSubmissionArgsBytes]struct{}

// callMember keeps a call's id, the tool its name names, and its arguments'
// key: an object within [sandbox.MaxSubmissionArgsBytes], keyed by the log's
// own rule.
func (w *qwenHoldingsWalk) callMember(i int, kind jsonscan.Kind) error {
	c := &w.calls[len(w.calls)-1]
	switch {
	case i == qwCallID && kind == jsonscan.String:
		id, ok, err := qwenText(w.sc, w.idBuf[:], qwenCallIDMost)
		if c.idOK = ok && len(id) > 0; c.idOK {
			c.id = string(id)
		}
		return err
	case i == qwCallName && kind == jsonscan.String:
		raw, whole, err := w.sc.Capture(qwenNameSpan())
		if err == nil && whole {
			c.call.tool, c.call.named = qwenBridgedTool(raw)
		}
		return err
	case i == qwCallArgs && kind == jsonscan.ObjectStart:
		raw, whole, err := w.sc.Capture(sandbox.MaxSubmissionArgsBytes)
		if err == nil && whole {
			c.call.args, err = toolargs.Key(raw)
			c.call.argsOK, err = err == nil, nil
		}
		return err
	}
	return w.sc.Skip()
}

// qwenNameSpan is the longest raw span a bridged name may be written in: six
// bytes a character escaped, and the quotes.
func qwenNameSpan() int { return 6*(len(qwenBridgedPrefix())+qwenToolNameMost) + 2 }

// answerMember keeps an answer's id, the tool its name names, and its
// response's one text.
func (w *qwenHoldingsWalk) answerMember(ctx context.Context, i int, kind jsonscan.Kind) error {
	a := &w.l.answer
	switch {
	case i == qwAnswerID && kind == jsonscan.String:
		id, ok, err := qwenText(w.sc, w.idBuf[:], qwenCallIDMost)
		if a.idOK = ok && len(id) > 0; a.idOK {
			a.idN = copy(a.id[:], id)
		}
		return err
	case i == qwAnswerName && kind == jsonscan.String:
		raw, whole, err := w.sc.Capture(qwenNameSpan())
		if err == nil && whole {
			a.tool, a.named = qwenBridgedTool(raw)
		}
		return err
	case i == qwAnswerResponse && kind == jsonscan.ObjectStart:
		return w.members(ctx, qwenFrameResponse, &a.response)
	}
	return w.sc.Skip()
}

// resultMember keeps a tool result's call id, statuses, failure and its
// failure's type; its display is never read.
func (w *qwenHoldingsWalk) resultMember(ctx context.Context, i int, kind jsonscan.Kind) error {
	a := &w.l.answer
	switch i {
	case qwResCallID:
		if kind != jsonscan.String {
			break
		}
		id, ok, err := qwenText(w.sc, w.idBuf[:], qwenCallIDMost)
		if a.callIDOK = ok && len(id) > 0; a.callIDOK {
			a.callIDN = copy(a.callID[:], id)
		}
		return err
	case qwResStatus, qwResExecution, qwResErrorType:
		word, ok, err := w.wordOf(kind)
		switch {
		case !ok:
		case i == qwResStatus:
			a.succeeded, a.erred = string(word) == "success", string(word) == "error"
		case i == qwResExecution:
			a.ranSuccess, a.ranFailed = string(word) == "success", string(word) == "error"
		default:
			a.mcp = string(word) == "mcp_tool_error"
		}
		return err
	case qwResError:
		if kind != jsonscan.ObjectStart {
			break
		}
		var keys qwenKeys
		err := w.members(ctx, qwenFrameOther, &keys)
		a.empty = keys == qwenKeys{}
		return err
	}
	return w.sc.Skip()
}

// decide applies line n, read whole: a record of 0.28.4's shapes, of this
// session and release, on the one chain each uuid once; the line after the
// run's start its brief; and, after the start, a compaction, a step or a
// tool's answer.
func (w *qwenHoldingsWalk) decide(n int) error {
	l := &w.l
	kind, names := w.kind()
	switch {
	case l.keys.twice:
		return errQwenTwice
	case !l.keys.exactly(names) || !w.shaped(kind):
		return errQwenShape
	case !l.uuidOK || n == 1 && !l.parentNull || n > 1 && !l.parentOK:
		return errQwenChain
	case !l.sessionOK:
		return errQwenSession
	case !l.releaseOK:
		return errQwenRelease
	case kind == qwenKindCompression && l.history != jsonscan.ArrayStart:
		return errQwenShape
	}
	w.ids.add(qwenUUIDKey(l.uuid))
	w.prev = l.uuid
	if !w.began && int64(n) == w.start+1 && kind != qwenKindUser {
		return errQwenBrief
	}
	if kind == qwenKindAssistant {
		if err := w.nextStep(int64(n) > w.start); err != nil {
			return err
		}
	}
	switch {
	case int64(n) <= w.start:
	case kind == qwenKindCompression:
		for _, parts := range w.classes {
			parts.steps, parts.pending = parts.steps[:0], 0
		}
		clear(w.open)
		w.counted, w.compacted = w.counted[:0], true
	case kind == qwenKindResult:
		return w.answer()
	}
	return nil
}

// kind is the line's record kind and the names it holds, qwenKindOther and
// none where 0.28.4 writes no such record: an unknown subtype, read as none
// of [qwenSubtypes], is no kind.
func (w *qwenHoldingsWalk) kind() (qwenKind, uint32) {
	l := &w.l
	sub := l.keys.seen&(1<<qwRecSubtype) != 0
	switch {
	case l.typ == qwenTypeUser && !sub:
		return qwenKindUser, qwenUserNames
	case l.typ == qwenTypeUser && l.subtype == qwenSubtypeNote:
		return qwenKindNote, qwenNoteNames
	case l.typ == qwenTypeAssistant && !sub:
		return qwenKindAssistant, qwenAssistantNames
	case l.typ == qwenTypeResult && !sub:
		return qwenKindResult, qwenResultNames
	case l.typ == qwenTypeSystem && l.subtype == qwenSubtypeCompression:
		return qwenKindCompression, qwenSystemNames
	case l.typ == qwenTypeSystem && l.subtype > qwenSubtypeNote:
		return qwenKindSystem, qwenSystemNames
	}
	return qwenKindOther, 0
}

// shaped reports whether the line's message is exactly `{role, parts}` with
// the parts kind's records hold: a person's text; a step's text, thought and
// calls; a tool's one answer, as the user's.
func (w *qwenHoldingsWalk) shaped(kind qwenKind) bool {
	l := &w.l
	switch kind {
	case qwenKindOther:
		return false
	case qwenKindSystem, qwenKindCompression:
		return true
	}
	if !l.message || !l.msgKeys.exactly(qwenMessageNames) || !l.parts || l.bad > 0 {
		return false
	}
	switch kind {
	case qwenKindUser, qwenKindNote:
		return l.thought == 0 && l.calls == 0 && l.answers == 0
	case qwenKindAssistant:
		return l.answers == 0
	}
	return l.roleUser && l.answers == 1 && l.text == 0 && l.thought == 0 && l.calls == 0
}

// nextStep closes the open step, committing its parts, and opens the line's
// calls as the next, counted where it is the run's: each call's id read, once
// in its step.
func (w *qwenHoldingsWalk) nextStep(counted bool) error {
	w.closeStep()
	clear(w.open)
	w.stepCalls = w.stepCalls[:0]
	for _, c := range w.calls {
		if !c.idOK {
			return errQwenCalls
		}
		w.stepCalls = append(w.stepCalls, c.call)
	}
	for i := range w.calls {
		if _, twice := w.open[w.calls[i].id]; twice {
			return errQwenCalls
		}
		w.open[w.calls[i].id] = &w.stepCalls[i]
	}
	if !counted {
		clear(w.open)
		return nil
	}
	w.step++
	return nil
}

// closeStep commits the open step's counted parts to its step, each class's
// to its calls and one more ([openCodeHoldingsWalk.messageEnd]).
func (w *qwenHoldingsWalk) closeStep() {
	for _, parts := range w.counted {
		for ; parts.pending > 0 && len(parts.steps) <= parts.calls; parts.pending-- {
			parts.steps = append(parts.steps, w.step)
		}
		parts.pending = 0
	}
	w.counted = w.counted[:0]
}

// answer reads a tool's answer after the run's start: to a call of the open
// step not yet answered, else no reading (a load drops a second answer); and
// counted for the one class whose call it is the verbatim answer of, else
// passed. Passing only lowers a class's parts, never holds one.
func (w *qwenHoldingsWalk) answer() error {
	a := &w.l.answer
	var call *qwenStepCall
	if a.idOK {
		call = w.open[string(a.id[:a.idN])]
	}
	if call == nil || call.answered {
		return errQwenAnswer
	}
	call.answered = true
	ran := a.result.exactly(qwenSuccessNames) && a.succeeded && a.ranSuccess && !a.failed ||
		a.result.exactly(qwenFailureNames) && a.erred && a.ranFailed && a.empty && a.mcp && a.failed
	switch {
	case !call.named || !call.argsOK || !a.keys.exactly(qwenAnswerNames) || !a.named || a.tool != call.tool,
		!a.text || a.response.other || a.response.twice || a.response.seen&(a.response.seen-1) != 0,
		!ran, !a.callIDOK || string(a.callID[:a.callIDN]) != string(a.id[:a.idN]),
		w.text.blank || w.qwenWords(call.tool, a.failed):
		return nil
	}
	for _, stub := range qwenStubPrefixes {
		if w.text.opens(stub) {
			return nil
		}
	}
	class, ok := matchedClass(w.candidates[partKey{name: call.tool, args: call.args, failed: a.failed}], w.text,
		a.failed)
	if !ok {
		return nil
	}
	parts := w.classes[class]
	if parts.pending == 0 {
		w.counted = append(w.counted, parts)
	}
	parts.pending++
	return nil
}

// qwenWords reports whether the text digested is what 0.28.4 hands its model
// in place of an empty output of tool, or of a failure that sent no text.
func (w *qwenHoldingsWalk) qwenWords(tool string, failed bool) bool {
	words := qwenNoOutputOpen + qwenBridgedPrefix() + tool + qwenNoOutputEnd
	if failed {
		words = qwenFailureWords
	}
	return w.text.n == len(words) && w.text.whole == sha256.Sum256([]byte(words))
}

// ended is the reading once the file was read whole, lines its whole lines:
// no reading where it is shorter than the run's start or names a uuid twice;
// else the verdict ([contentVerdict]), the open step's parts committed.
func (w *qwenHoldingsWalk) ended(ask sandbox.HoldingsAsk, lines int) (sandbox.SessionHoldings, error) {
	switch {
	case int64(lines) < w.start:
		return sandbox.SessionHoldings{}, errQwenShort
	case w.ids.twice():
		return sandbox.SessionHoldings{}, errQwenChain
	}
	w.closeStep()
	parts := make(map[sandbox.CallClass][]int, len(w.classes))
	for class, p := range w.classes {
		parts[class] = p.steps
	}
	return contentVerdict(ask, parts, w.compacted)
}
