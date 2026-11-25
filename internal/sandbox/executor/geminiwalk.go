package executor

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"slices"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/jsonscan"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools/toolargs"
)

// THE GEMINI CLI READING'S WALK ([geminiHoldingsWalk]): each line of the chat
// file ([chatLines]) on one scanner, its facts kept per line and applied only
// at its end, the records replayed as 0.70.0's loader replays them: each id a
// place, a restated id its place kept and its facts replaced, `$patch`'s
// updates and removals, `$rewindTo` dropping its record and every later one.
// Every key set is exact and no key may come twice. The package doc's "A
// session's calls are read by what they were" has the rules.

// The bounds of what a walk keeps ([geminiHoldingsWalk]): ids ever, and the
// run's records, calls and answers. Past any, the session is unread.
const (
	geminiHoldingsIDs     = 1 << 14
	geminiHoldingsRunMost = 1 << 13
	// geminiIDBytes is the longest id a walk reads: Gemini CLI's are 36 or
	// 32 bytes (a uuid, the session context's), its calls' some 150.
	geminiIDBytes = 512
	// geminiArgsBytes is the most of a call's `args` a walk captures, at
	// least what the log keeps whole (compile-asserted below).
	geminiArgsBytes = 64 << 10
)

// A call's arguments are captured whole wherever the log keeps them whole:
// this fails to compile where [geminiArgsBytes] is shorter than
// [sandbox.MaxSubmissionArgsBytes].
var _ [geminiArgsBytes - sandbox.MaxSubmissionArgsBytes]struct{}

// geminiName is a name a chat file's object holds, its bit in a key set.
type geminiName uint8

const (
	gnSessionID geminiName = iota
	gnProjectHash
	gnStartTime
	gnLastUpdated
	gnKind
	gnSet
	gnRewind
	gnPatch
	gnID
	gnType
	gnTimestamp
	gnContent
	gnDisplayContent
	gnModel
	gnThoughts
	gnTokens
	gnToolCalls
	gnUpdates
	gnRemoveIDs
	gnArgs
	gnDescription
	gnDisplayName
	gnName
	gnRenderMarkdown
	gnResult
	gnResultDisplay
	gnStatus
	gnText
	gnFunctionResponse
	gnResponse
	gnOutput
	gnOther // any name of no set
)

var geminiNames = map[string]geminiName{
	"sessionId": gnSessionID, "projectHash": gnProjectHash, "startTime": gnStartTime, "lastUpdated": gnLastUpdated,
	"kind": gnKind, "$set": gnSet, "$rewindTo": gnRewind, "$patch": gnPatch, "id": gnID, "type": gnType,
	"timestamp": gnTimestamp, "content": gnContent, "displayContent": gnDisplayContent, "model": gnModel,
	"thoughts": gnThoughts, "tokens": gnTokens, "toolCalls": gnToolCalls, "updates": gnUpdates,
	"removeIds": gnRemoveIDs, "args": gnArgs, "description": gnDescription, "displayName": gnDisplayName,
	"name": gnName, "renderOutputAsMarkdown": gnRenderMarkdown, "result": gnResult,
	"resultDisplay": gnResultDisplay, "status": gnStatus, "text": gnText, "functionResponse": gnFunctionResponse,
	"response": gnResponse, "output": gnOutput,
}

// geminiNameBytes is the longest name [geminiNames] holds, read whole.
const geminiNameBytes = len("renderOutputAsMarkdown")

// geminiSet is a key set, a bit a name.
type geminiSet uint64

func geminiSetOf(names ...geminiName) geminiSet {
	var s geminiSet
	for _, n := range names {
		s |= 1 << n
	}
	return s
}

// The key sets 0.70.0 writes (measured): the header's exactly; every other
// object's within its set.
var (
	geminiHeaderKeys = geminiSetOf(gnSessionID, gnProjectHash, gnStartTime, gnLastUpdated, gnKind)
	geminiSetKeys    = geminiSetOf(gnLastUpdated, gnSessionID)
	geminiPatchKeys  = geminiSetOf(gnUpdates, gnRemoveIDs)
	geminiUpdateKeys = geminiSetOf(gnID, gnContent, gnToolCalls)
	geminiUserKeys   = geminiSetOf(gnID, gnType, gnTimestamp, gnContent, gnDisplayContent)
	geminiReplyKeys  = geminiSetOf(gnID, gnType, gnTimestamp, gnContent, gnModel, gnThoughts, gnTokens, gnToolCalls)
	geminiCallKeys   = geminiSetOf(gnArgs, gnDescription, gnDisplayName, gnID, gnName, gnRenderMarkdown, gnResult,
		gnResultDisplay, gnStatus, gnTimestamp)
	geminiAnswerKeys   = geminiSetOf(gnID, gnName, gnResponse)
	geminiResponseKeys = geminiSetOf(gnOutput)
	// geminiLineKeys are every name a line holds at its top, whatever its kind.
	geminiLineKeys = geminiHeaderKeys | geminiUserKeys | geminiReplyKeys | geminiSetOf(gnSet, gnRewind, gnPatch)
)

// geminiObject is which object of a line a walk is in.
type geminiObject uint8

const (
	goLine geminiObject = iota
	goSet
	goPatch
	goUpdate
	goCall
	goPart
	goAnswer
	goResponse
)

// geminiStrict are the objects whose names a walk holds to their set, any
// other a shape it does not read; a part, an answer and its response are read
// as they are, one of another shape never counted.
var geminiStrict = map[geminiObject]geminiSet{goLine: geminiLineKeys, goSet: geminiSetKeys,
	goPatch: geminiPatchKeys, goUpdate: geminiUpdateKeys, goCall: geminiCallKeys}

// geminiID is an id as a walk keeps it: the first 16 bytes of its decoded
// text's SHA-256, read as one id where 0.70.0 reads one.
type geminiID [16]byte

// geminiIDOf is the id raw, a captured value, names: ok where it is a string
// of at most [geminiIDBytes]; long where it is a longer one.
func geminiIDOf(raw []byte, k jsonscan.Kind, whole bool) (id geminiID, ok, long bool) {
	if k != jsonscan.String {
		return id, false, false
	}
	if !whole {
		return id, false, true
	}
	text := raw[1 : len(raw)-1]
	if bytes.IndexByte(text, '\\') >= 0 {
		var s string
		if wireDecode(raw, &s) != nil {
			return id, false, false
		}
		text = []byte(s)
	}
	if len(text) > geminiIDBytes {
		return id, false, true
	}
	sum := sha256.Sum256(text)
	copy(id[:], sum[:])
	return id, true, false
}

// geminiContent is what a walk keeps of a record's `content`: its kind,
// whether every part is one answer alone, and its answers, a range of the
// line's.
type geminiContent struct {
	kind        jsonscan.Kind // "" where absent
	answersOnly bool
	from, to    int
}

// geminiCallFact is one `toolCalls` element as a walk keeps it: its id, its
// candidate key (the tool and arguments' key of a class, -1 none), and
// whether it succeeded.
type geminiCallFact struct {
	id      geminiID
	idOK    bool
	key     int32
	success bool
}

// geminiAnswerFact is one `functionResponse` part: its id, and the one class its
// output is the verbatim answer of, -1 none.
type geminiAnswerFact struct {
	id    geminiID
	idOK  bool
	class int32
}

// geminiUpdate is one update of a `$patch`.
type geminiUpdate struct {
	id      geminiID
	idOK    bool
	content geminiContent
}

// geminiLine is what a walk keeps of the line it reads, applied at its end.
type geminiLine struct {
	n                   int
	keys                geminiSet
	sessionOK, kindMain bool
	setOther            bool
	rewind              geminiID
	rewindOK            bool
	id                  geminiID
	idOK                bool
	reply               bool
	typed               bool
	content             geminiContent
	updates             []geminiUpdate
	removes             []geminiID
}

// geminiRec is one id ever: its place, its kind, whether it is in the view
// and a compression's summary ([geminiCompressionLines]), and its run facts ([geminiRun], -1 where it first
// appeared before the run's start).
type geminiRec struct {
	place                   int32
	run                     int32
	reply, inView, snapshot bool
}

// geminiRun is a run record's facts: for a reply, whether its content is a
// string and its calls; for a person's record, whether it is answers alone
// and its answers; each a range of the walk's arenas.
type geminiRun struct {
	callsAt, calls, answersAt, answers int32
	text, answersOnly                  bool
}

// geminiPlace is a place in the view's order: a record and its place there,
// stale once the record left the view or took a later place.
type geminiPlace struct{ rec, place int32 }

// geminiHoldingsWalk is one reading's walk of a chat file: the pool's
// classes, the line it is in, every id, the run's facts, and the scanner,
// digest and buffers it reuses.
type geminiHoldingsWalk struct {
	ctx     context.Context
	session string
	start   sandbox.RunStart
	prefix  string
	sc      *jsonscan.Scanner
	// classes are the pool's stated classes; keys the (tool, arguments) of
	// those that did not fail, classKey each class's, and candidates the
	// classes of each key a part is matched against.
	classes    []sandbox.CallClass
	calls      map[sandbox.CallClass]int
	classIndex map[sandbox.CallClass]int32
	keys       []partKey
	keyIndex   map[partKey]int32
	classKey   []int32
	candidates map[partKey][]sandbox.CallClass
	text       *textDigest
	// header is whether line 1 was the session's header.
	header bool
	ln     geminiLine
	// head is the line's opening, compressions what its forms say of the
	// summaries it ends ([geminiCompressionLines]).
	head         lineHead
	compressions geminiCompressionLines[geminiID]
	// lineCalls and lineAnswers are where the line's calls and answers begin
	// in the arenas, written there as read and named by a run only once
	// applied.
	lineCalls, lineAnswers int
	// part and answer are what a walk keeps of the part and the answer it
	// is in.
	part     geminiPartState
	answer   geminiAnswerState
	callRead geminiCallRead
	// ids, recs and order are every id, its record, and the view's places.
	ids       map[geminiID]int32
	recs      []geminiRec
	order     []geminiPlace
	nextPlace int32
	inView    int
	// runs are the run's records, callArena and answerArena their facts,
	// liveCalls and liveAnswers how many the runs name; callHint a reply's
	// call by its id, a hint an answer is matched by and the end confirms.
	runs                   []geminiRun
	callArena              []geminiCallFact
	answerArena            []geminiAnswerFact
	liveCalls, liveAnswers int
	callHint               map[uint32]int32
	// name, word and piece are buffers a name, a word and a text are read
	// into; held is a wrapped text's held-back closing.
	name   [geminiNameBytes + 1]byte
	word   [16]byte
	piece  []byte
	held   []byte
	tail   []byte
	edge   []byte
	needle bool
	span   []int32
}

// geminiPartState is what a walk keeps of a content part: whether it holds a
// name but `text` and `functionResponse`.
type geminiPartState struct {
	other bool
}

// geminiAnswerState is what a walk keeps of a `functionResponse`: its id,
// its tool, its response's names, and whether its output is wrapped whole.
type geminiAnswerState struct {
	response                   geminiSet
	id                         geminiID
	idOK, named, other, output bool
	tool                       string
}

// newGeminiHoldingsWalk is a walk over the session from start for ask's
// pool: every call of the ask but those a person's approval held ([ClassOf]),
// its digest keeping the sum of every opening a cut class keeps.
func newGeminiHoldingsWalk(ctx context.Context, session string, start sandbox.RunStart,
	ask sandbox.HoldingsAsk) *geminiHoldingsWalk {

	w := &geminiHoldingsWalk{ctx: ctx, sc: jsonscan.New(nil), session: session, start: start, prefix: geminiBridgedPrefix(),
		calls: map[sandbox.CallClass]int{}, classIndex: map[sandbox.CallClass]int32{},
		keyIndex: map[partKey]int32{}, candidates: map[partKey][]sandbox.CallClass{}, ids: map[geminiID]int32{},
		callHint: map[uint32]int32{}, piece: make([]byte, textDigestBytes),
		head: lineHead{head: make([]byte, 0, lineFormMaxBytes+1)},
		held: make([]byte, 0, 2*len(geminiWrapClose)), tail: make([]byte, 0, len(geminiWrapNeedle)),
		edge: make([]byte, 0, 2*len(geminiWrapNeedle))}
	var openings []int
	for _, c := range ask.Calls {
		class, ok := sandbox.ClassOf(c)
		if !ok {
			continue
		}
		if _, seen := w.classIndex[class]; !seen {
			w.classIndex[class] = int32(len(w.classes))
			w.classes = append(w.classes, class)
			key := partKey{name: class.Name, args: class.Args, failed: class.Failed}
			k := int32(-1)
			if !class.Failed {
				var known bool
				if k, known = w.keyIndex[key]; !known {
					k = int32(len(w.keys))
					w.keyIndex[key] = k
					w.keys = append(w.keys, key)
				}
				w.candidates[key] = append(w.candidates[key], class)
			}
			w.classKey = append(w.classKey, k)
			if !class.Text.Whole {
				openings = append(openings, class.Text.Opening)
			}
		}
		w.calls[class]++
	}
	w.text = newTextDigest(openings)
	return w
}

// The words of a Gemini CLI reading for a file it does not read: a shape
// 0.70.0 was not measured writing, a key written twice, a line of no JSON
// object, and what the walk keeps past its bounds.
var (
	errGeminiShape = &holdingsError{why: "a line of the session's chat file is of a shape this build does not " +
		"read"}
	errGeminiTwice     = &holdingsError{why: "a key of the session's chat file is written twice"}
	errGeminiMalformed = &holdingsError{why: "a line of the session's chat file is no JSON object"}
	errGeminiBound     = &holdingsError{why: "the session's chat file holds more records, calls or answers " +
		"than a reading keeps"}
	errGeminiHeader = &holdingsError{why: "the session's chat file does not open with its own session's header"}
	errGeminiRewind = &holdingsError{why: "the session's chat file rewinds to a record its history does not hold, " +
		"which 0.70.0 reads as clearing it"}
	errGeminiSetOther = &holdingsError{why: "the session's chat file names another session"}
)

// line reads line n of the chat file, body, and applies its facts once body
// answered io.EOF ([chatLines]).
func (w *geminiHoldingsWalk) line(n int, body io.Reader) error {
	w.head.reset(body)
	w.sc.Reset(&w.head)
	w.ln = geminiLine{n: n, updates: w.ln.updates[:0], removes: w.ln.removes[:0]}
	w.lineCalls, w.lineAnswers = len(w.callArena), len(w.answerArena)
	k, err := w.sc.Next()
	switch {
	case err != nil:
		return w.scanFailed(err)
	case k != jsonscan.ObjectStart:
		return errGeminiMalformed
	}
	keys, err := w.object(goLine)
	if err != nil {
		return w.scanFailed(err)
	}
	if _, err = w.sc.Next(); !errors.Is(err, io.EOF) {
		return w.scanFailed(err)
	}
	w.ln.keys = keys
	if err = w.apply(); err != nil {
		return err
	}
	// A SUMMARY IS KNOWN A LINE LATE, by the reply after it; a record its
	// id's next line rewrites is read afresh ([geminiHoldingsWalk.entry]).
	user := w.ln.n > 1 && w.ln.typed && !w.ln.reply
	if summary, ok := w.compressions.line(w.head.head, w.ln.id, user); ok {
		if i, known := w.ids[summary]; known && !w.recs[i].reply {
			w.recs[i].snapshot = true
		}
	}
	w.compact()
	return nil
}

// scanFailed is the reading's words for a scanner's err: the grammar's
// refusal is a line of no JSON; anything else, the reader's own or one
// beneath, passes as it is.
func (w *geminiHoldingsWalk) scanFailed(err error) error {
	if _, grammar := errors.AsType[*jsonscan.Error](err); grammar {
		return errGeminiMalformed
	}
	return err
}

// object reads the current object's members, its opening brace read, to its
// closing one: each name once, a strict object's within its set; the set of
// its names in [geminiNames].
func (w *geminiHoldingsWalk) object(o geminiObject) (geminiSet, error) {
	var keys geminiSet
	strict, isStrict := geminiStrict[o]
	for {
		k, err := w.sc.Next()
		switch {
		case err != nil:
			return keys, err
		case k == jsonscan.ObjectEnd:
			return keys, nil
		}
		name, err := w.nameOf()
		if err != nil {
			return keys, err
		}
		bit := geminiSetOf(name)
		switch {
		case name != gnOther && keys&bit != 0:
			return keys, errGeminiTwice
		case isStrict && (name == gnOther || strict&bit == 0):
			return keys, errGeminiShape
		}
		if name != gnOther {
			keys |= bit
		}
		if err = w.member(o, name); err != nil {
			return keys, err
		}
	}
}

// nameOf is the current name in [geminiNames], gnOther where it is none.
func (w *geminiHoldingsWalk) nameOf() (geminiName, error) {
	text, past, err := w.read(w.name[:])
	if err != nil || past {
		return gnOther, err
	}
	if n, ok := geminiNames[string(text)]; ok {
		return n, nil
	}
	return gnOther, nil
}

// read is the current name's or string's decoded text where it fits in buf
// less a byte; else past, its rest passed over by the next Next.
func (w *geminiHoldingsWalk) read(buf []byte) ([]byte, bool, error) {
	n := 0
	for n < len(buf) {
		m, err := w.sc.Read(buf[n:])
		n += m
		switch {
		case errors.Is(err, io.EOF):
			return buf[:n], false, nil
		case errors.Is(err, io.ErrShortBuffer):
			return nil, true, nil
		case err != nil:
			return nil, false, err
		}
	}
	return nil, true, nil
}

// wordIs reports whether the next value is a string whose text is one of
// words, and which: -1 for none.
func (w *geminiHoldingsWalk) wordIs(words ...string) (int, error) {
	k, err := w.sc.Next()
	if err != nil {
		return -1, err
	}
	if k != jsonscan.String {
		return -1, w.sc.Skip()
	}
	text, past, err := w.read(w.word[:])
	if err != nil || past {
		return -1, err
	}
	return slices.Index(words, string(text)), nil
}

// capture is the next value as written, within limit, and its kind.
func (w *geminiHoldingsWalk) capture(limit int) ([]byte, jsonscan.Kind, bool, error) {
	k, err := w.sc.Next()
	if err != nil {
		return nil, k, false, err
	}
	raw, whole, err := w.sc.Capture(limit)
	return raw, k, whole, err
}

// idOf is the id the next value names ([geminiIDOf]).
func (w *geminiHoldingsWalk) idOf() (geminiID, bool, bool, error) {
	raw, k, whole, err := w.capture(6*geminiIDBytes + 2)
	if err != nil {
		return geminiID{}, false, false, err
	}
	id, ok, long := geminiIDOf(raw, k, whole)
	return id, ok, long, nil
}

// skip passes the next value whole.
func (w *geminiHoldingsWalk) skip() error {
	if _, err := w.sc.Next(); err != nil {
		return err
	}
	return w.sc.Skip()
}

// member reads the value of object o's member name.
func (w *geminiHoldingsWalk) member(o geminiObject, name geminiName) error {
	switch o {
	case goLine:
		return w.lineMember(name)
	case goSet:
		return w.setMember(name)
	case goPatch:
		return w.patchMember(name)
	case goUpdate:
		return w.updateMember(name)
	case goCall:
		return w.callMember(name)
	case goPart:
		return w.partMember(name)
	case goAnswer:
		return w.answerMember(name)
	}
	return w.responseMember(name)
}

// lineMember reads a line's member: the header's session and kind, a
// `$set`, a `$rewindTo`, a `$patch`, an entry's id, type, content and calls.
func (w *geminiHoldingsWalk) lineMember(name geminiName) error {
	ln := &w.ln
	switch name {
	case gnSessionID:
		raw, _, whole, err := w.capture(6*executorkeep.SessionIDMaxBytes + 2)
		ln.sessionOK = err == nil && whole && stringIs(raw, w.session, executorkeep.SessionIDMaxBytes)
		return err
	case gnKind:
		i, err := w.wordIs("main")
		ln.kindMain = i == 0
		return err
	case gnSet:
		return w.objectValue(goSet)
	case gnRewind:
		id, ok, long, err := w.idOf()
		switch {
		case err != nil:
			return err
		case long:
			return errGeminiRewind
		case !ok:
			return errGeminiShape
		}
		ln.rewind, ln.rewindOK = id, true
		return nil
	case gnPatch:
		return w.objectValue(goPatch)
	case gnID:
		id, ok, long, err := w.idOf()
		switch {
		case err != nil:
			return err
		case long:
			return errGeminiBound
		case !ok:
			return errGeminiShape
		}
		ln.id, ln.idOK = id, true
		return nil
	case gnType:
		i, err := w.wordIs("user", "gemini")
		if err == nil && i < 0 {
			return errGeminiShape
		}
		ln.typed, ln.reply = true, i == 1
		return err
	case gnContent:
		return w.content(&ln.content)
	case gnToolCalls:
		return w.toolCalls()
	}
	return w.skip()
}

// objectValue reads the next value as object o, a shape not read where it
// is no object.
func (w *geminiHoldingsWalk) objectValue(o geminiObject) error {
	k, err := w.sc.Next()
	switch {
	case err != nil:
		return err
	case k != jsonscan.ObjectStart:
		return errGeminiShape
	}
	_, err = w.object(o)
	return err
}

// setMember reads a `$set`'s member: its session, which must be the reading's.
func (w *geminiHoldingsWalk) setMember(name geminiName) error {
	if name != gnSessionID {
		return w.skip()
	}
	raw, _, whole, err := w.capture(6*executorkeep.SessionIDMaxBytes + 2)
	if err == nil && (!whole || !stringIs(raw, w.session, executorkeep.SessionIDMaxBytes)) {
		w.ln.setOther = true
	}
	return err
}

// patchMember reads a `$patch`'s updates, each an object, and its removals,
// each an id; past [geminiHoldingsIDs] of either, the line is past a bound.
func (w *geminiHoldingsWalk) patchMember(name geminiName) error {
	k, err := w.sc.Next()
	switch {
	case err != nil:
		return err
	case k != jsonscan.ArrayStart:
		return errGeminiShape
	}
	for {
		if err = w.ctx.Err(); err != nil {
			return err
		}
		if k, err = w.sc.Next(); err != nil || k == jsonscan.ArrayEnd {
			return err
		}
		if name == gnUpdates {
			if k != jsonscan.ObjectStart {
				return errGeminiShape
			}
			if len(w.ln.updates) == geminiHoldingsIDs {
				return errGeminiBound
			}
			w.ln.updates = append(w.ln.updates, geminiUpdate{})
			if _, err = w.object(goUpdate); err != nil {
				return err
			}
			if !w.ln.updates[len(w.ln.updates)-1].idOK {
				return errGeminiShape
			}
			continue
		}
		raw, whole, err := w.sc.Capture(6*geminiIDBytes + 2)
		if err != nil {
			return err
		}
		id, ok, long := geminiIDOf(raw, k, whole)
		switch {
		case !ok && !long:
			return errGeminiShape
		case long:
			// AN ID NO RECORD CAN BEAR: a longer one is no reading at its
			// record, so its removal removes nothing.
		case len(w.ln.removes) == geminiHoldingsIDs:
			return errGeminiBound
		default:
			w.ln.removes = append(w.ln.removes, id)
		}
	}
}

// updateMember reads an update's id and content; its results are never read.
func (w *geminiHoldingsWalk) updateMember(name geminiName) error {
	u := &w.ln.updates[len(w.ln.updates)-1]
	switch name {
	case gnID:
		id, ok, long, err := w.idOf()
		switch {
		case err != nil:
			return err
		case !ok || long:
			return errGeminiShape
		}
		u.id, u.idOK = id, true
		return nil
	case gnContent:
		var c geminiContent
		err := w.content(&c)
		w.ln.updates[len(w.ln.updates)-1].content = c
		return err
	}
	return w.skip()
}

// toolCalls reads a reply's calls, each an object, into the line's.
func (w *geminiHoldingsWalk) toolCalls() error {
	k, err := w.sc.Next()
	switch {
	case err != nil:
		return err
	case k != jsonscan.ArrayStart:
		return errGeminiShape
	}
	for {
		if err = w.ctx.Err(); err != nil {
			return err
		}
		if k, err = w.sc.Next(); err != nil || k == jsonscan.ArrayEnd {
			return err
		}
		if k != jsonscan.ObjectStart {
			return errGeminiShape
		}
		if len(w.callArena)-w.lineCalls == geminiHoldingsRunMost {
			return errGeminiBound
		}
		w.callArena = append(w.callArena, geminiCallFact{key: -1})
		w.callRead = geminiCallRead{}
		keys, err := w.object(goCall)
		if err != nil {
			return err
		}
		if keys&geminiSetOf(gnStatus) == 0 {
			return errGeminiShape
		}
	}
}

// geminiCallRead is a call's tool and arguments' key as read so far.
type geminiCallRead struct {
	tool          string
	named, argsOK bool
	args          [32]byte
}

// callMember reads a call's id, name, arguments and status.
func (w *geminiHoldingsWalk) callMember(name geminiName) error {
	c := &w.callArena[len(w.callArena)-1]
	switch name {
	case gnID:
		id, ok, _, err := w.idOf()
		c.id, c.idOK = id, ok
		return err
	case gnName:
		raw, k, whole, err := w.capture(6*(len(w.prefix)+harnessprofile.ToolNameMax()) + 2)
		tool, named := "", false
		if err == nil && whole && k == jsonscan.String {
			tool, named = bridgedToolName(raw, w.prefix)
		}
		w.callTool(c, tool, named, nil, false)
		return err
	case gnArgs:
		raw, k, whole, err := w.capture(geminiArgsBytes)
		if err != nil {
			return err
		}
		ok := whole && k == jsonscan.ObjectStart && len(raw) <= sandbox.MaxSubmissionArgsBytes
		var sum [32]byte
		if ok {
			sum, err = toolargs.Key(raw)
			ok = err == nil
		}
		w.callTool(c, "", false, &sum, ok)
		return nil
	case gnStatus:
		i, err := w.wordIs("scheduled", "success", "error")
		if err == nil && i < 0 {
			return errGeminiShape
		}
		c.success = i == 1
		return err
	}
	return w.skip()
}

// callTool keeps a call's tool or arguments as read, the other as it was,
// and sets its key once both are known: -1 where no class has them.
func (w *geminiHoldingsWalk) callTool(c *geminiCallFact, tool string, named bool, args *[32]byte, argsOK bool) {
	r := &w.callRead
	if args == nil {
		r.tool, r.named = tool, named
	} else {
		r.args, r.argsOK = *args, argsOK
	}
	c.key = -1
	if r.named && r.argsOK {
		if k, ok := w.keyIndex[partKey{name: r.tool, args: r.args}]; ok {
			c.key = k
		}
	}
}

// content reads a `content`: a string, or parts, each an object; anything
// else is kept by its kind alone.
func (w *geminiHoldingsWalk) content(c *geminiContent) error {
	k, err := w.sc.Next()
	if err != nil {
		return err
	}
	*c = geminiContent{kind: k, from: len(w.answerArena), to: len(w.answerArena)}
	if k != jsonscan.ArrayStart {
		return w.sc.Skip()
	}
	parts := 0
	c.answersOnly = true
	for {
		if err = w.ctx.Err(); err != nil {
			return err
		}
		if k, err = w.sc.Next(); err != nil {
			return err
		}
		if k == jsonscan.ArrayEnd {
			break
		}
		if k != jsonscan.ObjectStart {
			return errGeminiShape
		}
		w.part = geminiPartState{}
		keys, err := w.object(goPart)
		if err != nil {
			return err
		}
		if keys != geminiSetOf(gnFunctionResponse) || w.part.other {
			c.answersOnly = false
		}
		parts++
	}
	c.answersOnly = c.answersOnly && parts > 0
	c.to = len(w.answerArena)
	return nil
}

// partMember reads a part's text and its answer; any other name marks it as
// no answer alone.
func (w *geminiHoldingsWalk) partMember(name geminiName) error {
	switch name {
	case gnText:
		k, err := w.sc.Next()
		switch {
		case err != nil:
			return err
		case k != jsonscan.String:
			w.part.other = true
			return w.sc.Skip()
		}
		return nil
	case gnFunctionResponse:
		k, err := w.sc.Next()
		switch {
		case err != nil:
			return err
		case k != jsonscan.ObjectStart:
			return errGeminiShape
		}
		w.answer = geminiAnswerState{}
		keys, err := w.object(goAnswer)
		if err != nil {
			return err
		}
		return w.answered(keys)
	}
	w.part.other = true
	return w.skip()
}

// answerMember reads an answer's id, name and response; any other name marks
// it as no answer of the shape a part counts by.
func (w *geminiHoldingsWalk) answerMember(name geminiName) error {
	a := &w.answer
	switch name {
	case gnID:
		id, ok, _, err := w.idOf()
		a.id, a.idOK = id, ok
		return err
	case gnName:
		raw, k, whole, err := w.capture(6*(len(w.prefix)+harnessprofile.ToolNameMax()) + 2)
		a.tool, a.named = "", false
		if err == nil && whole && k == jsonscan.String {
			a.tool, a.named = bridgedToolName(raw, w.prefix)
		}
		return err
	case gnResponse:
		k, err := w.sc.Next()
		switch {
		case err != nil:
			return err
		case k != jsonscan.ObjectStart:
			a.other = true
			return w.sc.Skip()
		}
		a.response, err = w.object(goResponse)
		return err
	}
	a.other = true
	return w.skip()
}

// responseMember reads a response's output, wrapped and digested; any other
// name marks the answer as of another shape.
func (w *geminiHoldingsWalk) responseMember(name geminiName) error {
	k, err := w.sc.Next()
	switch {
	case err != nil:
		return err
	case name != gnOutput || k != jsonscan.String:
		w.answer.other = true
		return w.sc.Skip()
	}
	w.answer.output, err = w.wrapped()
	return err
}

// answered keeps the answer just read: its id, and the one class whose call
// it answers verbatim, matched by the tool and arguments of the call its id
// names (a hint the end confirms, [geminiHoldingsWalk.countReply]) and its
// text's digest; -1 where any rule fails.
func (w *geminiHoldingsWalk) answered(keys geminiSet) error {
	s := &w.answer
	a := geminiAnswerFact{id: s.id, idOK: s.idOK, class: -1}
	if keys == geminiAnswerKeys && s.response == geminiResponseKeys && !s.other && s.output && s.named && s.idOK {
		if k, ok := w.callHint[geminiHint(s.id)]; ok && k >= 0 && w.keys[k].name == s.tool {
			if class, ok := matchedClass(w.candidates[w.keys[k]], w.text, false); ok {
				a.class = w.classIndex[class]
			}
		}
	}
	if len(w.answerArena)-w.lineAnswers == geminiHoldingsRunMost {
		return errGeminiBound
	}
	w.answerArena = append(w.answerArena, a)
	return nil
}

// geminiHint is the key of an id in [geminiHoldingsWalk.callHint]: four of
// its bytes, since a hint two ids share only tells their calls.
func geminiHint(id geminiID) uint32 { return binary.LittleEndian.Uint32(id[:4]) }

// The wrapper 0.70.0 puts around an MCP tool's one text part (measured), and
// the closing tag a text inside it may not hold raw: a second part's.
const (
	geminiWrapOpen   = "<untrusted_context>\n"
	geminiWrapClose  = "\n</untrusted_context>"
	geminiWrapNeedle = "</untrusted_context>"
)

// wrapped reads the current string, an output, into the digest a piece at a
// time: its opening checked, its closing held back and checked, the text
// between digested ([textDigest]) and held to no raw closing tag. True where
// all of it holds.
func (w *geminiHoldingsWalk) wrapped() (bool, error) {
	w.text.reset()
	var open [len(geminiWrapOpen)]byte
	opened := 0
	held := w.held[:0]
	w.tail, w.needle = w.tail[:0], false
	for {
		n, err := w.sc.Read(w.piece)
		p := w.piece[:n]
		if opened < len(open) {
			c := copy(open[opened:], p)
			opened, p = opened+c, p[c:]
		}
		if len(p) > 0 {
			held = w.pass(held, p)
		}
		switch {
		case errors.Is(err, io.EOF):
			ok := opened == len(open) && string(open[:]) == geminiWrapOpen && len(held) >= len(geminiWrapClose) &&
				string(held[len(held)-len(geminiWrapClose):]) == geminiWrapClose
			if ok {
				w.emit(held[:len(held)-len(geminiWrapClose)])
			}
			w.text.end()
			w.held = held[:0]
			return ok && !w.needle, nil
		case err != nil:
			return false, err
		}
	}
}

// pass emits held and p but their last bytes of the closing's length, cut at
// a character's start so the digest reads whole characters; the rest is the
// held bytes it answers, at most the closing and a character's three more.
func (w *geminiHoldingsWalk) pass(held, p []byte) []byte {
	total := len(held) + len(p)
	if total <= len(geminiWrapClose) {
		return append(held, p...)
	}
	at := func(i int) byte {
		if i < len(held) {
			return held[i]
		}
		return p[i-len(held)]
	}
	cut := total - len(geminiWrapClose)
	for cut > 0 && !utf8.RuneStart(at(cut)) {
		cut--
	}
	if cut <= len(held) {
		w.emit(held[:cut])
		kept := copy(held, held[cut:])
		return append(held[:kept], p...)
	}
	w.emit(held)
	w.emit(p[:cut-len(held)])
	return append(held[:0], p[cut-len(held):]...)
}

// emit digests q, the next piece of the text inside the wrapper, noting a raw
// closing tag in it or across its edge with the piece before.
func (w *geminiHoldingsWalk) emit(q []byte) {
	if len(q) == 0 {
		return
	}
	w.text.write(q)
	if w.needle {
		return
	}
	keep := len(geminiWrapNeedle) - 1
	w.edge = append(append(w.edge[:0], w.tail...), q[:min(len(q), keep)]...)
	w.needle = bytes.Contains(w.edge, []byte(geminiWrapNeedle)) || bytes.Contains(q, []byte(geminiWrapNeedle))
	if len(q) >= keep {
		w.tail = append(w.tail[:0], q[len(q)-keep:]...)
	} else {
		w.tail = append(w.tail[:0], w.edge[max(0, len(w.edge)-keep):]...)
	}
}

// apply applies the line's facts, read whole: line 1 the session's header; a
// `$set` naming no other session; a `$rewindTo`; a `$patch`; else an entry.
func (w *geminiHoldingsWalk) apply() error {
	ln := &w.ln
	if ln.n == 1 {
		if ln.keys != geminiHeaderKeys || !ln.sessionOK || !ln.kindMain {
			return errGeminiHeader
		}
		w.header = true
		return nil
	}
	switch ln.keys {
	case geminiSetOf(gnSet):
		if ln.setOther {
			return errGeminiSetOther
		}
		return nil
	case geminiSetOf(gnRewind):
		return w.rewind(ln.rewind)
	case geminiSetOf(gnPatch):
		return w.patch()
	}
	return w.entry()
}

// entry applies a record's copy: a new id at the next place, one in the view
// at its own, one that left the view at the next; its facts replaced.
func (w *geminiHoldingsWalk) entry() error {
	ln := &w.ln
	allowed := geminiUserKeys
	if ln.reply {
		allowed = geminiReplyKeys
	}
	if !ln.typed || !ln.idOK || ln.keys&^allowed != 0 {
		return errGeminiShape
	}
	i, known := w.ids[ln.id]
	if !known {
		if len(w.recs) == geminiHoldingsIDs {
			return errGeminiBound
		}
		run := int32(-1)
		if w.start.Began || int64(ln.n) > w.start.Lines {
			if len(w.runs) == geminiHoldingsRunMost {
				return errGeminiBound
			}
			run = int32(len(w.runs))
			w.runs = append(w.runs, geminiRun{})
		}
		i = int32(len(w.recs))
		w.recs = append(w.recs, geminiRec{run: run})
		w.ids[ln.id] = i
	}
	if !w.recs[i].inView {
		w.place(i)
	}
	r := &w.recs[i]
	r.reply, r.snapshot = ln.reply, false
	if r.run < 0 {
		return nil
	}
	f := &w.runs[r.run]
	f.text, f.answersOnly = ln.reply && ln.content.kind == jsonscan.String, !ln.reply && ln.content.answersOnly
	calls, answers := [2]int{w.lineCalls, len(w.callArena)}, [2]int{ln.content.from, ln.content.to}
	if ln.reply {
		answers = [2]int{}
	} else {
		calls = [2]int{}
	}
	if err := w.setCalls(f, calls); err != nil {
		return err
	}
	return w.setAnswers(f, answers)
}

// patch applies a `$patch`: each update's content to its record where it is
// in the view (0.70.0 ignores the rest), then each removal.
func (w *geminiHoldingsWalk) patch() error {
	for _, u := range w.ln.updates {
		i, known := w.ids[u.id]
		if !known || !w.recs[i].inView || u.content.kind == "" {
			continue
		}
		r := &w.recs[i]
		if r.run < 0 {
			continue
		}
		f := &w.runs[r.run]
		if r.reply {
			f.text = u.content.kind == jsonscan.String
			continue
		}
		f.answersOnly = u.content.answersOnly
		if err := w.setAnswers(f, [2]int{u.content.from, u.content.to}); err != nil {
			return err
		}
	}
	for _, id := range w.ln.removes {
		if i, known := w.ids[id]; known && w.recs[i].inView {
			w.recs[i].inView = false
			w.inView--
		}
	}
	return nil
}

// rewind applies a `$rewindTo`: its record, which must be in the view, and
// every later place leave it.
func (w *geminiHoldingsWalk) rewind(id geminiID) error {
	i, known := w.ids[id]
	if !known || !w.recs[i].inView {
		return errGeminiRewind
	}
	at, _ := slices.BinarySearchFunc(w.order, w.recs[i].place, func(e geminiPlace, place int32) int {
		return cmp.Compare(e.place, place)
	})
	for _, e := range w.order[at:] {
		if w.live(e) {
			w.recs[e.rec].inView = false
			w.inView--
		}
	}
	w.order = w.order[:at]
	return nil
}

// geminiSlack is how far an arena or the order runs past twice what is live
// before it is compacted.
const geminiSlack = 1 << 10

// place puts record i at the view's next place.
func (w *geminiHoldingsWalk) place(i int32) {
	r := &w.recs[i]
	r.place, r.inView = w.nextPlace, true
	w.nextPlace++
	w.inView++
	w.order = append(w.order, geminiPlace{rec: i, place: r.place})
	if len(w.order) > 2*w.inView+geminiSlack {
		w.order = slices.DeleteFunc(w.order, func(e geminiPlace) bool { return !w.live(e) })
	}
}

// live reports whether e is its record's place in the view.
func (w *geminiHoldingsWalk) live(e geminiPlace) bool {
	r := w.recs[e.rec]
	return r.inView && r.place == e.place
}

// setCalls names the arena's calls in span, [from, to), as a run record's,
// each id's hint with them.
func (w *geminiHoldingsWalk) setCalls(f *geminiRun, span [2]int) error {
	w.liveCalls += span[1] - span[0] - int(f.calls)
	if w.liveCalls > geminiHoldingsRunMost {
		return errGeminiBound
	}
	f.callsAt, f.calls = int32(span[0]), int32(span[1]-span[0])
	for _, c := range w.callArena[span[0]:span[1]] {
		if c.idOK {
			w.callHint[geminiHint(c.id)] = c.key
		}
	}
	if len(w.callHint) > geminiHoldingsIDs {
		return errGeminiBound
	}
	return nil
}

// setAnswers names the arena's answers in span as a run record's.
func (w *geminiHoldingsWalk) setAnswers(f *geminiRun, span [2]int) error {
	w.liveAnswers += span[1] - span[0] - int(f.answers)
	if w.liveAnswers > geminiHoldingsRunMost {
		return errGeminiBound
	}
	f.answersAt, f.answers = int32(span[0]), int32(span[1]-span[0])
	return nil
}

// compact rewrites an arena holding more than twice what the runs name, once
// a line is applied: a line's facts are written in place as read, and those
// no run names (a record before the start, a copy restated) are left behind.
func (w *geminiHoldingsWalk) compact() {
	if len(w.callArena) > 2*w.liveCalls+geminiSlack {
		arena := make([]geminiCallFact, 0, 2*w.liveCalls)
		for j := range w.runs {
			r := &w.runs[j]
			at := int32(len(arena))
			arena = append(arena, w.callArena[r.callsAt:r.callsAt+r.calls]...)
			r.callsAt = at
		}
		w.callArena = arena
	}
	if len(w.answerArena) > 2*w.liveAnswers+geminiSlack {
		arena := make([]geminiAnswerFact, 0, 2*w.liveAnswers)
		for j := range w.runs {
			r := &w.runs[j]
			at := int32(len(arena))
			arena = append(arena, w.answerArena[r.answersAt:r.answersAt+r.answers]...)
			r.answersAt = at
		}
		w.answerArena = arena
	}
}

// errGeminiShort is a chat file holding fewer whole lines than its run began
// after: a file cut or rewritten since.
var errGeminiShort = &holdingsError{why: "the session's chat file holds fewer lines than its run began after"}

// ended is the reading once every line was read: no reading without the
// header or the run's start; else each class's parts by the step of the reply
// they answer, in the final view's order, after its newest summary
// ([geminiHoldingsWalk.countReply]), and the verdict ([contentVerdict]).
func (w *geminiHoldingsWalk) ended(ask sandbox.HoldingsAsk, lines int) (sandbox.SessionHoldings, error) {
	switch {
	case !w.header:
		return sandbox.SessionHoldings{}, errGeminiHeader
	case !w.start.Began && int64(lines) < w.start.Lines:
		return sandbox.SessionHoldings{}, errGeminiShort
	}
	restart, compacted := int32(-1), false
	for _, e := range w.order {
		if r := w.recs[e.rec]; w.live(e) && !r.reply && r.snapshot {
			restart, compacted = e.place, r.run >= 0
		}
	}
	parts := make(map[sandbox.CallClass][]int, len(w.classes))
	reply, step := int32(-1), 0
	span := w.span[:0]
	for _, e := range w.order {
		if !w.live(e) {
			continue
		}
		if !w.recs[e.rec].reply {
			span = append(span, e.rec)
			continue
		}
		w.countReply(reply, step, span, restart, parts)
		reply, step, span = e.rec, step+1, span[:0]
	}
	w.countReply(reply, step, span, restart, parts)
	w.span = span
	return contentVerdict(ask, parts, compacted)
}

// countReply counts the parts of reply g, at step, that the person's records
// of span (those before the next reply) answer, by every rule of a counted
// part: g the run's, after the restart, its content a string; every record of
// span the run's; its calls' ids each read and once; a call succeeded, of a
// class's tool and arguments still; its first answer in span in a record of
// answers alone, of the class it matched.
func (w *geminiHoldingsWalk) countReply(g int32, step int, span []int32, restart int32,
	parts map[sandbox.CallClass][]int) {

	if g < 0 {
		return
	}
	r := w.recs[g]
	if r.run < 0 || restart >= 0 && r.place < restart || !w.runs[r.run].text {
		return
	}
	for _, u := range span {
		if w.recs[u].run < 0 {
			return
		}
	}
	f := w.runs[r.run]
	calls := w.callArena[f.callsAt : f.callsAt+f.calls]
	for i, c := range calls {
		if !c.idOK || slices.ContainsFunc(calls[:i], func(d geminiCallFact) bool { return d.id == c.id }) {
			return
		}
	}
	for _, c := range calls {
		if !c.success || c.key < 0 {
			continue
		}
		a, answersOnly, found := w.firstAnswer(span, c.id)
		if !found || !answersOnly || a.class < 0 || w.classKey[a.class] != c.key {
			continue
		}
		class := w.classes[a.class]
		if steps := parts[class]; len(steps) <= w.calls[class] {
			parts[class] = append(steps, step)
		}
	}
}

// firstAnswer is the first answer of span's records naming id, as 0.70.0
// hands the first of an id, and whether its record is answers alone.
func (w *geminiHoldingsWalk) firstAnswer(span []int32, id geminiID) (geminiAnswerFact, bool, bool) {
	for _, u := range span {
		f := w.runs[w.recs[u].run]
		for _, a := range w.answerArena[f.answersAt : f.answersAt+f.answers] {
			if a.idOK && a.id == id {
				return a, f.answersOnly, true
			}
		}
	}
	return geminiAnswerFact{}, false, false
}
