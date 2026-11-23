package executor

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/jsonscan"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools/toolargs"
)

// WHICH OF A ROUND'S CALLS AN OPENCODE SESSION HOLDS, read off its kept export
// by what each call was ([Runner.ReadHoldings]): OpenCode names no call to the
// engine, so the reading counts the run's parts of each class of calls
// ([sandbox.CallClass]) the continuation hands its model verbatim, and the
// verdict of a reading by content ([contentVerdict]) holds a class only where
// every call of it has a part. The reading is a visitor of the one export walk
// ([scanExport]), keeping what it asks of each member as the walk reads it and
// deciding each item and message at its end; it holds no value whole. The
// package doc's "A session's calls are read by what they were" has the
// rules, the pool, the order check, the bounds and the memory; ADR-0073's
// amendment "a cut-off OpenCode session is read by content, held to its run
// and its class" what they leave.

// openCodeBridgedPrefix opens the name OpenCode gives each tool of the
// engine's own MCP server, offered directly ([openCodeMCP]): the server's
// name and `_`, as OpenCode's built-in profile states it
// ([harnessprofile.Profile.ModelToolPrefix]), whose suite holds it to the
// name config reserves. Never config's own constant: config's suite imports
// this package. A profile stating none leaves it "", which [bridgedToolName]
// reads as naming no tool of the engine's, so every call is told.
var openCodeBridgedPrefix = sync.OnceValue(func() string {
	p, ok := harnessprofile.Builtin(OpenCodeName)
	if !ok {
		return ""
	}
	return p.ModelToolPrefix()
})

// holdingsFloor is the oldest release of [openCodeHoldingsReleases]
// ([releaseSetFloor]).
func (OpenCode) holdingsFloor() string { return releaseSetFloor(openCodeHoldingsReleases) }

// checkHoldings holds ask, and the archive its row describes, to what an
// OpenCode reading answers, before anything is opened: a writer the set reads,
// the archive's and the ask's alike, compared whole; a session id of
// OpenCode's shape; a run whose start the capture names; 1 to
// [sandbox.MaxBridgeCalls] calls, at least one asked, each with its admission
// place, each asked one keyed by its place in the log alone (callKey's
// predicate, read back), never held for a person; and the calls the log does
// not list within the log's own bounds.
func (OpenCode) checkHoldings(archive sandbox.SessionArchive, ask sandbox.HoldingsAsk) error {
	switch {
	case !openCodeHoldingsRead(ask.Resumer) || archive.Version != ask.Resumer:
		return &holdingsError{why: fmt.Sprintf("OpenCode %.64q wrote the session, and a reading reads only the "+
			"releases its rules were measured on (openCodeHoldingsRead, internal/sandbox/executor)", archive.Version)}
	case !strings.HasPrefix(archive.SessionID, "ses_") || !sandbox.ValidSessionID(archive.SessionID):
		return &holdingsError{why: "the session's id names no OpenCode session"}
	case !archive.RunStart.Known():
		return &holdingsError{why: "the run that packed the session recorded no place it began in it"}
	}
	if why := logPlaceAskFault(ask); why != "" {
		return &holdingsError{why: "the relaunch asked about calls its log cannot have: " + why}
	}
	return nil
}

// readHoldings reads body, archive's gzipped export, within bounds' plain
// meter ([readOpenCodeHoldings]), and drains it to io.EOF before answering.
func (OpenCode) readHoldings(ctx context.Context, body io.Reader, archive sandbox.SessionArchive,
	ask sandbox.HoldingsAsk, bounds scrubBounds) (sandbox.SessionHoldings, error) {

	zr, err := gzip.NewReader(body)
	if err != nil {
		return sandbox.SessionHoldings{}, &holdingsError{why: "the session's export does not read as one export",
			cause: err}
	}
	holdings, err := readOpenCodeHoldings(ctx, boundedPlain(zr, bounds.plain), archive.SessionID, archive.RunStart,
		ask)
	if err != nil {
		return sandbox.SessionHoldings{}, err
	}
	// DRAINED TO io.EOF: what the store hands out is provisional until then,
	// and its stream latches its error.
	if _, err = io.Copy(io.Discard, body); err != nil {
		return sandbox.SessionHoldings{}, &holdingsError{
			why: "the session's stream did not end where its archive did", cause: err}
	}
	return holdings, nil
}

// readOpenCodeHoldings reads plain, a session's export as kept (the take's
// scrub's output), for the calls ask marks asked: the one export walk
// ([scanExport]), the reading its visitor ([openCodeHoldingsWalk]), nothing
// but white space after the document; then the verdict ([contentVerdict]), a
// pure function of the walk's facts and the ask. No value is held whole, so no
// part of an export is past a bound but the plaintext's.
func readOpenCodeHoldings(ctx context.Context, plain io.Reader, session string, start sandbox.RunStart,
	ask sandbox.HoldingsAsk) (sandbox.SessionHoldings, error) {

	w := newOpenCodeHoldingsWalk(session, start, ask)
	if err := scanExport(ctx, plain, w); err != nil {
		return sandbox.SessionHoldings{}, openCodeReadFailed(ctx, err)
	}
	return w.ended(ask)
}

// openCodeReadFailed is why a walk read no export whole: the reading's own
// words, the caller gone, the plain meter's bound, or the export read as no
// export, its cause the scanner's or a reader's words, which [holdingsError]
// cuts.
func openCodeReadFailed(ctx context.Context, err error) error {
	var own *holdingsError
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.As(err, &own):
		return own
	case errors.Is(err, errScrubBound):
		return &holdingsError{why: "the session's export is past the bound a kept session unpacks to", cause: err}
	}
	return &holdingsError{why: "the session's export does not read as one export", cause: err}
}

// The reading's words for an export it does not read: of a shape this build
// does not read, with a key written twice, or not one export.
var (
	errOpenCodeShape = &holdingsError{why: "a message of the session's export is of a shape this build " +
		"does not read"}
	errOpenCodeTwice     = &holdingsError{why: "a key of the session's export is written twice"}
	errOpenCodeMalformed = &holdingsError{why: "the session's export does not read as one export"}
)

// openCodeHoldingsWalk is one reading's state, the export walk's visitor: what
// it keeps of the document, of the run's start, and of each class's parts,
// and of the message, the item and the state it is in, which their ends
// decide. It never keeps a message, an item or a text past its visit. It asks
// `info`'s id, a message's id, a part's name, its `executed` and input, a
// step's `completed` and a state's metadata captured within
// [exportValueMaxBytes]; each type as a word; a message, an item, their times,
// a state, an answer, a metadata and a failure as frames with their key facts
// ([openCodeKeys]); an answer's text and a failure's message as a digest. An
// item's verdict waits for its message's end, whose kind may come after it.
type openCodeHoldingsWalk struct {
	session string
	start   sandbox.RunStart
	// infos and lists are how many `info` and `messages` keys the document
	// holds, and matched whether the last `info`'s id is the session's;
	// infoMatched is that of the `info` being read.
	infos, lists         int
	matched, infoMatched bool
	// seen is whether the run's start was read, step the ordinal of the
	// latest assistant message after it, and compacted whether a compaction
	// followed it.
	seen      bool
	step      int
	compacted bool
	// classes are the pool's stated classes, each with its calls and the
	// steps of its live parts, kept to Calls+1; candidates the classes by
	// tool, arguments and failure, which a part is matched against.
	classes    map[sandbox.CallClass]*classParts
	candidates map[partKey][]sandbox.CallClass
	// msg, item and state are what the reading keeps of the message, the
	// item and the tool call's state it is in; text the digest of the latest
	// answer's or failure's text.
	msg   holdingsMessage
	item  holdingsItem
	state holdingsState
	text  *textDigest
}

// partKey is what a part names before its text is read: the tool, the
// arguments' key, and whether it failed.
type partKey struct {
	name   string
	args   [32]byte
	failed bool
}

// classParts is one class's calls in the pool, the steps its counted parts
// lie in, in the order read, and the parts of the message being read that
// count for it, which its end commits.
type classParts struct {
	calls   int
	steps   []int
	pending int
}

// holdingsMessage is what a reading keeps of the message it is in, which the
// message's end decides in the order the checks run
// ([openCodeHoldingsWalk.messageEnd]): an item is read before its message's
// kind may be, as a person's message writes its text before its type.
type holdingsMessage struct {
	// typed says its `type` is a string, kind which of the eleven where it
	// is one (read); start that its id is the run's start.
	typed, read, start bool
	kind               openCodeKind
	// time and content are the kinds of its `time` and `content`, timeKeys
	// the time's names, and completed whether its `completed` is a number
	// above zero.
	time, content jsonscan.Kind
	timeKeys      openCodeKeys
	completed     bool
	// fault is why the first of its items that is none reads as no export;
	// counted the classes its parts count for.
	fault   error
	counted []*classParts
}

// itemProse and itemTool are the kinds of item a step holds: its text or its
// reasoning, or a tool it called; itemOther is any other.
const (
	itemOther = iota
	itemProse
	itemTool
)

// holdingsItem is what a reading keeps of the item it is in, which the item's
// end decides ([openCodeHoldingsWalk.itemEnd]).
type holdingsItem struct {
	typed bool
	kind  int
	// tool is the engine's tool its name names (named); executedFalse says
	// `executed` is exactly false, timeOK its `time` within its set.
	tool                         string
	named, executedFalse, timeOK bool
	// counts says its state is one a part counts by, failed that it failed,
	// and args its input's key ([openCodeHoldingsWalk.stateEnd]).
	counts, failed bool
	args           [32]byte
}

// holdingsState is what a reading keeps of a tool call's state, which its
// end decides ([openCodeHoldingsWalk.stateEnd]): its status, its input's
// key, its answer's items and whether the first is one text, its metadata
// exactly untruncated, its failure exactly a tool's with its message; and of
// the answer's part, the metadata's and the failure's members, each as read.
type holdingsState struct {
	completed, failed, argsOK  bool
	args                       [32]byte
	answers                    int
	answerOK, untruncated, ran bool
	partText, partString       bool
	truncatedFalse             bool
	failType, failString       bool
}

// newOpenCodeHoldingsWalk is a walk over the session, from start, for ask's
// pool: every call of the ask but those a person's approval held; its digest
// keeps the sum of every opening a cut class keeps.
func newOpenCodeHoldingsWalk(session string, start sandbox.RunStart, ask sandbox.HoldingsAsk) *openCodeHoldingsWalk {
	w := &openCodeHoldingsWalk{session: session, start: start, seen: start.Began,
		classes: map[sandbox.CallClass]*classParts{}, candidates: map[partKey][]sandbox.CallClass{}}
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

// holdingsAsks is what a reading reads of an export: `info`'s id; a message's
// id, kind, time and content; an item's kind, name, `executed`, time and
// state; a state's status, input, answer, metadata and failure, each held to
// its shape; and an answer's text and a failure's message by their digest.
var holdingsAsks = exportAsksOf(map[exportFrame]map[int]exportMeans{
	frameDocument: {docInfo: meansFrame},
	frameInfo:     {infoID: meansCapture},
	frameMessage:  {msgID: meansCapture, msgType: meansWord, msgTime: meansFrame, msgContent: meansFrame},
	frameStepTime: {stepCompleted: meansCapture},
	frameItem: {itemType: meansWord, itemName: meansCapture, itemExecuted: meansCapture, itemTime: meansFrame,
		itemState: meansFrame},
	frameState: {stateStatus: meansWord, stateInput: meansCapture, stateContent: meansFrame,
		stateMetadata: meansFrame, stateError: meansFrame},
	frameAnswerPart: {partType: meansWord, partText: meansDigest},
	frameMetadata:   {metaTruncated: meansCapture},
	frameFailure:    {failureType: meansWord, failureMessage: meansDigest},
}, true)

func (*openCodeHoldingsWalk) asks() *exportAsks { return holdingsAsks }

func (w *openCodeHoldingsWalk) digest() *textDigest { return w.text }

func (*openCodeHoldingsWalk) sink(exportFrame, int, jsonscan.Kind) io.Writer { return nil }

// open refuses a second `messages` list (the import keeps the last), and
// starts what a frame's members are kept in.
func (w *openCodeHoldingsWalk) open(f exportFrame) error {
	switch f {
	case frameMessages:
		if w.lists++; w.lists > 1 {
			return errOpenCodeTwice
		}
	case frameInfo:
		w.infoMatched = false
	case frameMessage:
		for _, parts := range w.msg.counted {
			parts.pending = 0
		}
		w.msg = holdingsMessage{counted: w.msg.counted[:0]}
	case frameStepTime:
		w.msg.completed = false
	case frameItem:
		w.item = holdingsItem{}
	case frameState:
		w.state = holdingsState{}
	case frameAnswerPart:
		w.state.partText, w.state.partString = false, false
	case frameMetadata:
		w.state.truncatedFalse = false
	case frameFailure:
		w.state.failType, w.state.failString = false, false
	}
	return nil
}

// member keeps one member, a key written twice by its last value: `info` is
// read once, an object, and every other key is passed, as the import passes
// it.
func (w *openCodeHoldingsWalk) member(f exportFrame, i int, v exportValue) error {
	switch f {
	case frameDocument:
		if w.infos++; w.infos > 1 {
			return errOpenCodeTwice
		}
		if v.kind != jsonscan.ObjectStart {
			return errOpenCodeMalformed
		}
		w.matched = w.infoMatched
	case frameInfo:
		w.infoMatched = !v.past && stringIs(v.text, w.session, executorkeep.SessionIDMaxBytes)
	case frameMessage:
		w.messageMember(i, v)
	case frameStepTime:
		w.msg.completed = !v.past && positiveNumber(v.text)
	case frameItem:
		w.itemMember(i, v)
	case frameState:
		w.stateMember(i, v)
	case frameAnswerPart:
		if i == partType {
			w.state.partText = v.is("text")
		} else {
			w.state.partString = v.kind == jsonscan.String
		}
	case frameMetadata:
		w.state.truncatedFalse = !v.past && string(v.text) == "false"
	case frameFailure:
		if i == failureType {
			w.state.failType = v.is("tool.execution")
		} else {
			w.state.failString = v.kind == jsonscan.String
		}
	}
	return nil
}

// messageMember keeps a message's id (compared with the run's start), kind,
// time and content.
func (w *openCodeHoldingsWalk) messageMember(i int, v exportValue) {
	m := &w.msg
	switch i {
	case msgID:
		m.start = !w.start.Began && !v.past && stringIs(v.text, w.start.After, sandbox.MaxMessageIDBytes)
	case msgType:
		m.typed = v.kind == jsonscan.String
		m.kind, m.read = "", false
		if m.typed && !v.past {
			m.kind, m.read = openCodeKindNamed(v.text)
		}
	case msgTime:
		m.time, m.timeKeys = v.kind, v.keys
	case msgContent:
		m.content = v.kind
	}
}

// itemMember keeps an item's kind, the tool its name names, its `executed`,
// whether its `time` holds its set's names each once, and its state's verdict.
func (w *openCodeHoldingsWalk) itemMember(i int, v exportValue) {
	it := &w.item
	switch i {
	case itemType:
		it.typed, it.kind = v.kind == jsonscan.String, itemOther
		switch {
		case v.is("tool"):
			it.kind = itemTool
		case v.is("text"), v.is("reasoning"):
			it.kind = itemProse
		}
	case itemName:
		it.tool, it.named = "", false
		if !v.past {
			it.tool, it.named = bridgedToolName(v.text, openCodeBridgedPrefix())
		}
	case itemExecuted:
		it.executedFalse = !v.past && string(v.text) == "false"
	case itemTime:
		it.timeOK = v.kind == jsonscan.ObjectStart && !v.keys.other && !v.keys.twice
	case itemState:
		it.counts, it.failed, it.args = w.stateEnd(v)
	}
}

// A part's input is captured whole wherever the log keeps a call's arguments
// whole: this fails to compile where [exportValueMaxBytes] is shorter than
// [sandbox.MaxSubmissionArgsBytes].
var _ [exportValueMaxBytes - sandbox.MaxSubmissionArgsBytes]struct{}

// stateMember keeps a state's status, its input's key (an object within
// [sandbox.MaxSubmissionArgsBytes], keyed by the log's own rule), and whether
// its metadata and its failure are exactly what OpenCode writes.
func (w *openCodeHoldingsWalk) stateMember(i int, v exportValue) {
	s := &w.state
	switch i {
	case stateStatus:
		s.completed, s.failed = v.is(openCodeCompleted), v.is(openCodeCallFailed)
	case stateInput:
		s.argsOK = v.kind == jsonscan.ObjectStart && !v.past && len(v.text) <= sandbox.MaxSubmissionArgsBytes
		if s.argsOK {
			var err error
			s.args, err = toolargs.Key(v.text)
			s.argsOK = err == nil
		}
	case stateMetadata:
		s.untruncated = v.kind == jsonscan.ObjectStart && v.keys.within(openCodeUntruncated) &&
			v.keys.seen == openCodeUntruncated && s.truncatedFalse
	case stateError:
		s.ran = v.kind == jsonscan.ObjectStart && v.keys.within(openCodeFailureNames) &&
			v.keys.seen == openCodeFailureNames && s.failType && s.failString
	}
}

// The names an untruncated metadata, a failure and an answer's text part hold,
// each exactly once ([openCodeKeys.within]).
var (
	openCodeUntruncated  = namesMask(metaTruncated)
	openCodeFailureNames = namesMask(failureType, failureMessage)
	openCodeTextPart     = namesMask(partType, partText)
)

// stateEnd is a part's `state`, v its value as walked: whether a part counts
// by it, whether it failed, and its input's key. Exactly OpenCode's keys for
// its status, each once; a completed call's `metadata` exactly `{"truncated":
// false}` and its `content` one text item; a failed call's `error` a
// `tool.execution` with its message; the input an object within the longest
// argument text the log keeps whole. False where any of it is not so.
func (w *openCodeHoldingsWalk) stateEnd(v exportValue) (counts, failed bool, args [32]byte) {
	s := &w.state
	switch {
	case v.kind != jsonscan.ObjectStart || !s.argsOK:
	case s.completed && v.keys.within(openCodeCompletedState) && v.keys.seen == openCodeCompletedState &&
		s.untruncated:
		return s.answers == 1 && s.answerOK, false, s.args
	case s.failed && v.keys.within(openCodeFailedState) && v.keys.seen == openCodeFailedState:
		return s.ran, true, s.args
	}
	return false, false, args
}

// element refuses a list element that is no message, and keeps an item no
// object as its message's fault, and an answer's part no object as one that
// is no text.
func (w *openCodeHoldingsWalk) element(f exportFrame, _ jsonscan.Kind) error {
	switch f {
	case frameMessages:
		return errOpenCodeMalformed
	case frameContent:
		if w.msg.fault == nil {
			w.msg.fault = errOpenCodeMalformed
		}
	case frameAnswer:
		w.state.answers++
		w.state.answerOK = false
	}
	return nil
}

// close decides a frame at its end: an answer's part, an item, a message.
func (w *openCodeHoldingsWalk) close(f exportFrame, v exportValue) error {
	switch f {
	case frameAnswerPart:
		s := &w.state
		s.answers++
		s.answerOK = s.answers == 1 && v.keys.within(openCodeTextPart) && v.keys.seen == openCodeTextPart &&
			s.partText && s.partString
	case frameItem:
		w.itemEnd(v.keys)
	case frameMessage:
		return w.messageEnd(v.keys)
	}
	return nil
}

// itemEnd decides an item, in order of its message's items: its kind a
// string, one of text, reasoning or tool, a tool's part counted for the one
// class whose call it is the verbatim answer of, else passed. The first item
// that is none is its message's fault, which its end reads.
func (w *openCodeHoldingsWalk) itemEnd(keys openCodeKeys) {
	if w.msg.fault != nil {
		return
	}
	switch it := &w.item; {
	case !it.typed:
		w.msg.fault = errOpenCodeMalformed
	case it.kind == itemTool:
		w.part(keys)
	case it.kind != itemProse:
		w.msg.fault = errOpenCodeShape
	}
}

// part counts a tool item for the one class whose call it is the verbatim
// answer of, its message's end committing it; else passes it. A part is
// passed where any rule fails: passing only lowers a class's parts, which can
// turn a held class told and never the other way.
func (w *openCodeHoldingsWalk) part(keys openCodeKeys) {
	it := &w.item
	switch {
	case !keys.within(openCodePartNames), keys.has(itemExecuted) && !it.executedFalse,
		keys.has(itemTime) && !it.timeOK, !it.named, !it.counts:
		return
	}
	class, ok := matchedClass(w.candidates[partKey{name: it.tool, args: it.args, failed: it.failed}], w.text,
		it.failed)
	if !ok {
		return
	}
	parts := w.classes[class]
	if parts.pending == 0 {
		w.msg.counted = append(w.msg.counted, parts)
	}
	parts.pending++
}

// messageEnd reads one message by its kind, in the order its checks always
// ran: the run's start; a compaction, which drops every part fact; and an
// assistant step after both, whose keys, times and items are held to their
// sets, and whose counted parts are committed to its step.
func (w *openCodeHoldingsWalk) messageEnd(keys openCodeKeys) error {
	m := &w.msg
	switch {
	case !m.typed:
		return errOpenCodeMalformed
	case !m.read:
		return errOpenCodeShape
	case m.start && w.seen:
		return &holdingsError{why: "the place the run began is in its session's export twice"}
	case m.start:
		w.seen = true
		return nil
	case !w.seen:
		return nil
	case openCodeRestart(m.kind):
		for _, parts := range w.classes {
			parts.steps = parts.steps[:0]
		}
		w.compacted = true
		return nil
	case m.kind != openCodeAssistant:
		return nil
	case keys.twice:
		return errOpenCodeTwice
	case !keys.within(openCodeAssistantNames):
		return errOpenCodeShape
	case m.time != jsonscan.ObjectStart:
		return errOpenCodeMalformed
	case m.timeKeys.twice:
		return errOpenCodeTwice
	case m.timeKeys.other:
		return errOpenCodeShape
	case !m.completed:
		// A SETTLED STEP: the export holds settled steps only, and a step with
		// any other `completed` is one the import may drop.
		return errOpenCodeMalformed
	}
	w.step++
	switch {
	case m.content != jsonscan.ArrayStart:
		return errOpenCodeMalformed
	case m.fault != nil:
		return m.fault
	}
	for _, parts := range m.counted {
		for ; parts.pending > 0 && len(parts.steps) <= parts.calls; parts.pending-- {
			parts.steps = append(parts.steps, w.step)
		}
		parts.pending = 0
	}
	return nil
}

// openCodeAssistantNames are the names an assistant step may hold
// ([openCodeMessageNames]): those the measured releases write, `metadata` not
// among them, and none a person's or a compaction's.
var openCodeAssistantNames = namesMask(msgID, msgType, msgAgent, msgModel, msgContent, msgSnapshot, msgFinish,
	msgRawFinish, msgProviderState, msgNative, msgCost, msgTokens, msgError, msgRetry, msgTime)

// namesMask is the mask of a decode's names of the indexes given ([openCodeKeys]).
func namesMask(indexes ...int) uint32 {
	var mask uint32
	for _, i := range indexes {
		mask |= 1 << i
	}
	return mask
}

// openCodeKinds are the eleven kinds of message an export holds
// ([openCodeKind]).
var openCodeKinds = []openCodeKind{openCodeUser, openCodeAssistant, openCodeCompaction, openCodeSynthetic,
	openCodeSystem, openCodeSkill, openCodeShell, openCodeIdle, openCodeAgentSwitched, openCodeModelSwitched,
	openCodeLocationSwitched}

// openCodeKindRead reports whether kind is one of [openCodeKinds].
func openCodeKindRead(kind openCodeKind) bool { return slices.Contains(openCodeKinds, kind) }

// openCodeKindNamed is the kind word is, and whether it is one of
// [openCodeKinds].
func openCodeKindNamed(word []byte) (openCodeKind, bool) {
	for _, kind := range openCodeKinds {
		if string(word) == string(kind) {
			return kind, true
		}
	}
	return "", false
}

// openCodeStepNames and openCodePartTimes are the names a step's and a part's
// `time` may hold. `firstToken`, which 2.3.2 writes on every step a model answered, is a stamp
// read for nothing, as `created` and `streamed` are; it comes after
// `completed`, whose index [openCodeStepTimes] reads.
var (
	openCodeStepNames = openCodeNames("created", "streamed", "completed", "firstToken")
	openCodePartTimes = openCodeNames("created", "ran", "completed")
)

// positiveNumber reports whether raw is a JSON number above zero as a
// double reads it, as JSON.parse does: one that underflows to 0 is falsy to
// the import, and one past the double's range is refused with it. raw is a
// value the walk validated, so a leading digit makes it a number.
func positiveNumber(raw json.RawMessage) bool {
	if len(raw) == 0 || raw[0] < '0' || raw[0] > '9' {
		return false
	}
	v, err := strconv.ParseFloat(string(raw), 64)
	return err == nil && v > 0
}

// stringIs reports whether raw is a JSON string whose text is want, its span
// read only within what a text of most bytes can be written in (six bytes a
// character escaped, and the quotes), so no long value is decoded to be
// compared.
func stringIs(raw json.RawMessage, want string, most int) bool {
	if len(raw) < 2 || raw[0] != '"' || len(raw) > 6*most+2 {
		return false
	}
	if bytes.IndexByte(raw, '\\') < 0 {
		return string(raw[1:len(raw)-1]) == want
	}
	var got string
	return wireDecode(raw, &got) == nil && got == want
}

// openCodePartNames are the names a counted part may hold: OpenCode's six.
// A part holding another (its text, a provider's state) is passed.
var openCodePartNames = namesMask(itemType, itemID, itemName, itemExecuted, itemState, itemTime)

// bridgedToolName is the engine's tool a part's `name` names, prefix
// (`crewlet_`) and a name the bridge files a tool by, its raw span held to
// that bound before it is decoded; none where prefix is "".
func bridgedToolName(raw json.RawMessage, prefix string) (string, bool) {
	most := len(prefix) + harnessprofile.ToolNameMax()
	if prefix == "" || len(raw) < 2 || raw[0] != '"' || len(raw) > 6*most+2 {
		return "", false
	}
	var name string
	if wireDecode(raw, &name) != nil {
		return "", false
	}
	tool, ok := strings.CutPrefix(name, prefix)
	if !ok || len(tool) > harnessprofile.ToolNameMax() || !harnessprofile.ToolNameChars(tool) {
		return "", false
	}
	return tool, true
}

// The names a part's `state` is read by, each its index in [openCodeStateNames].
const (
	stateStatus = iota
	stateInput
	stateContent
	stateMetadata
	stateError
)

var (
	openCodeStateNames     = openCodeNames("status", "input", "content", "metadata", "error")
	openCodeCompletedState = namesMask(stateStatus, stateInput, stateContent, stateMetadata)
	openCodeFailedState    = namesMask(stateStatus, stateInput, stateError)
)

// ended is the reading once the walk read the export whole: no reading where
// `info` is absent or names another session, or the run's start was never
// read; else the verdict ([contentVerdict]).
func (w *openCodeHoldingsWalk) ended(ask sandbox.HoldingsAsk) (sandbox.SessionHoldings, error) {
	switch {
	case w.infos == 0:
		return sandbox.SessionHoldings{}, errOpenCodeMalformed
	case !w.matched:
		return sandbox.SessionHoldings{}, &holdingsError{why: "the export names another session"}
	case !w.seen:
		return sandbox.SessionHoldings{}, &holdingsError{why: "the place the run began is not in its session's " +
			"export"}
	}
	parts := make(map[sandbox.CallClass][]int, len(w.classes))
	for class, p := range w.classes {
		parts[class] = p.steps
	}
	return contentVerdict(ask, parts, w.compacted)
}
