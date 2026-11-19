package executor

import (
	"bytes"
	"cmp"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools/toolargs"
)

// WHICH OF A ROUND'S CALLS AN OPENCODE SESSION HOLDS, read off its kept export
// by what each call was ([Runner.ReadHoldings]): OpenCode names no call to the
// engine, so the reading counts the run's parts of each class of calls
// ([sandbox.CallClass]), and a class is held only where every call of it has
// a part the continuation hands its model verbatim. The reading is the second
// visitor of the person reader's one export walk ([walkOpenCodeExport]),
// gathering its facts as the walk decodes each message and item once, in
// place. The package doc's "An OpenCode session's calls are read by what they
// were" has the rules, the pool, the order check, the bounds and the memory;
// ADR-0073's amendment "a cut-off OpenCode session is read by content, held to
// its run and its class" what they leave.

// openCodeBridgedPrefix opens the name OpenCode gives each tool of the
// engine's own MCP server, offered directly ([openCodeMCP]): the server's
// name and `_`.
const openCodeBridgedPrefix = config.ToolServerName + "_"

// holdingsFloor is the oldest release of [openCodeHoldingsReleases] by its
// numbers, computed from the set, never its first member: [checkHoldings]
// holds the writer to the set first, so the floor never refuses a reading.
func (OpenCode) holdingsFloor() string {
	floor, least := "", [3]int{}
	for _, release := range openCodeHoldingsReleases {
		numbers, ok := releaseNumbers(release)
		if ok && (floor == "" || slices.Compare(numbers[:], least[:]) < 0) {
			floor, least = release, numbers
		}
	}
	return floor
}

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
	if why := openCodeAskFault(ask); why != "" {
		return &holdingsError{why: "the relaunch asked about calls its log cannot have: " + why}
	}
	return nil
}

// openCodeAskFault is why ask is not one a round's log can make, "" where it
// is ([OpenCode.checkHoldings]).
func openCodeAskFault(ask sandbox.HoldingsAsk) string {
	if len(ask.Calls) == 0 || len(ask.Calls) > sandbox.MaxBridgeCalls {
		return fmt.Sprintf("%d calls, where a round's log lists 1 to %d", len(ask.Calls), sandbox.MaxBridgeCalls)
	}
	carried, asked := make(map[string]int, len(ask.Calls)), 0
	for _, c := range ask.Calls {
		carried[c.ID]++
		if c.Asked {
			asked++
		}
	}
	if asked == 0 {
		return "none of them asked about"
	}
	for i, c := range ask.Calls {
		switch {
		case c.Seq <= 0:
			return fmt.Sprintf("call %d has no admission place", i+1)
		case !c.Asked:
		case c.Appended <= 0 || c.ID != strconv.Itoa(c.Appended):
			return fmt.Sprintf("call %d is asked about, and is not keyed by its place in the log", i+1)
		case c.Held:
			return fmt.Sprintf("call %d is asked about, and a person's approval held it", i+1)
		case carried[c.ID] > 1:
			return fmt.Sprintf("call %d is asked about, and another call of the ask carries its key", i+1)
		}
	}
	inFlight, unnamed, named := 0, 0, 0
	for _, id := range ask.Unlisted {
		switch {
		case id.InFlight:
			inFlight++
		case id.Name == "":
			unnamed++
		default:
			named += len(id.Name) + len(id.Digest)
		}
	}
	switch {
	case inFlight > sandbox.MaxCallsInFlight:
		return fmt.Sprintf("%d calls in flight, where the log holds at most %d", inFlight, sandbox.MaxCallsInFlight)
	case unnamed > 1:
		return "more than one count of dropped calls with no name"
	case named > sandbox.MaxElidedIdentityBytes:
		return "dropped calls whose names and digests weigh more than the log's tally holds"
	}
	return ""
}

// readHoldings reads body, archive's gzipped export, within bounds' plain
// meter and the walk's per-value bound ([readOpenCodeHoldings]), and drains it
// to io.EOF before answering.
func (OpenCode) readHoldings(ctx context.Context, body io.Reader, archive sandbox.SessionArchive,
	ask sandbox.HoldingsAsk, bounds scrubBounds) (sandbox.SessionHoldings, error) {

	zr, err := gzip.NewReader(body)
	if err != nil {
		return sandbox.SessionHoldings{}, &holdingsError{why: "the session's export does not read as one export",
			cause: err}
	}
	holdings, err := readOpenCodeHoldings(ctx, boundedPlain(zr, bounds.plain), maxLineBytes, archive.SessionID,
		archive.RunStart, ask)
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
// scrub's output), for the calls ask marks asked: the one export walk, each
// value held to lineBytes, the reading its visitor ([openCodeHoldingsWalk]);
// nothing but whitespace after the document; then the verdict
// ([openCodeVerdict]), a pure function of the walk's facts and the ask. A
// take reads a session through it too, inside its own budget and slot.
func readOpenCodeHoldings(ctx context.Context, plain io.Reader, lineBytes int, session string,
	start sandbox.RunStart, ask sandbox.HoldingsAsk) (sandbox.SessionHoldings, error) {

	w := newOpenCodeHoldingsWalk(session, start, ask)
	held := &heldValue{r: plain, max: lineBytes}
	dec := jsontext.NewDecoder(held, threadDecoding)
	err := walkOpenCodeExport(ctx, dec, held, w)
	if err == nil {
		held.reset()
		if _, err = dec.ReadToken(); errors.Is(err, io.EOF) {
			err = nil
		} else if err == nil {
			err = errors.New("another value follows the document")
		}
	}
	if err != nil {
		return sandbox.SessionHoldings{}, openCodeReadFailed(ctx, held, lineBytes, err)
	}
	return w.ended(ask)
}

// openCodeReadFailed is why a walk read no export whole: the reading's own
// words, the caller gone, a value past lineBytes, the plain meter's bound, or
// the export read as no export, its cause a decoder's or a reader's words,
// which [holdingsError] cuts.
func openCodeReadFailed(ctx context.Context, held *heldValue, lineBytes int, err error) error {
	var own *holdingsError
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.As(err, &own):
		return own
	case held.over:
		return &holdingsError{why: "a value of the session's export is past " + humanSize(int64(lineBytes))}
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
// it keeps of the document, of the run's start, and of each class's parts.
// It never keeps a message, an item or a text past its visit.
type openCodeHoldingsWalk struct {
	session string
	start   sandbox.RunStart
	// infos and lists are how many `info` and `messages` keys the document
	// holds, and matched whether the last `info`'s id is the session's.
	infos, lists int
	matched      bool
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
}

// partKey is what a part names before its text is read: the tool, the
// arguments' key, and whether it failed.
type partKey struct {
	name   string
	args   [32]byte
	failed bool
}

// classParts is one class's calls in the pool, and the steps its counted
// parts lie in, in the order read.
type classParts struct {
	calls int
	steps []int
}

// newOpenCodeHoldingsWalk is a walk over the session, from start, for ask's
// pool: every call of the ask but those a person's approval held.
func newOpenCodeHoldingsWalk(session string, start sandbox.RunStart, ask sandbox.HoldingsAsk) *openCodeHoldingsWalk {
	w := &openCodeHoldingsWalk{session: session, start: start, seen: start.Began,
		classes: map[sandbox.CallClass]*classParts{}, candidates: map[partKey][]sandbox.CallClass{}}
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
		}
		parts.calls++
	}
	return w
}

// topKey reads `info`, once, an object, and whether its id is the session's;
// every other key is passed, as the import passes it.
func (w *openCodeHoldingsWalk) topKey(name string, value json.RawMessage) error {
	if name != "info" {
		return nil
	}
	if w.infos++; w.infos > 1 {
		return errOpenCodeTwice
	}
	if !wireObject(value) {
		return errOpenCodeMalformed
	}
	return eachKeyInPlace(value, func(key []byte, id json.RawMessage) error {
		if string(key) == "id" {
			w.matched = stringIs(id, w.session, executorkeep.SessionIDMaxBytes)
		}
		return nil
	})
}

// messageList refuses a second `messages` list: the import keeps the last.
func (w *openCodeHoldingsWalk) messageList() error {
	if w.lists++; w.lists > 1 {
		return errOpenCodeTwice
	}
	return nil
}

// notMessage refuses a list element that is no message.
func (*openCodeHoldingsWalk) notMessage() error { return errOpenCodeMalformed }

// message reads one message by its kind: the run's start, a compaction, which
// drops every part fact, and an assistant step after both, whose keys, times
// and items are held to their sets and whose tool items are read as parts
// ([openCodeHoldingsWalk.part]).
func (w *openCodeHoldingsWalk) message(ctx context.Context, m openCodeMessageWire) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(m.Type) == 0 || m.Type[0] != '"' {
		return errOpenCodeMalformed
	}
	var kind openCodeKind
	if wireDecode(m.Type, &kind) != nil {
		return errOpenCodeMalformed
	}
	if !openCodeKindRead(kind) {
		return errOpenCodeShape
	}
	if !w.start.Began && stringIs(m.ID, w.start.After, sandbox.MaxMessageIDBytes) {
		if w.seen {
			return &holdingsError{why: "the place the run began is in its session's export twice"}
		}
		w.seen = true
		return nil
	}
	switch {
	case !w.seen:
		return nil
	case openCodeRestart(kind):
		for _, parts := range w.classes {
			parts.steps = parts.steps[:0]
		}
		w.compacted = true
		return nil
	case kind != openCodeAssistant:
		return nil
	case m.keys.twice:
		return errOpenCodeTwice
	case !m.keys.within(openCodeAssistantNames):
		return errOpenCodeShape
	}
	if err := openCodeStepTimes(m.Time); err != nil {
		return err
	}
	w.step++
	err := eachOpenCodeItem(m.Content, func(item openCodeItemWire) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if item.fault != "" || len(item.Type) == 0 || item.Type[0] != '"' {
			return errOpenCodeMalformed
		}
		switch {
		case stringIs(item.Type, "tool", 4):
			w.part(item)
		case !stringIs(item.Type, "text", 4) && !stringIs(item.Type, "reasoning", 9):
			return errOpenCodeShape
		}
		return nil
	})
	if _, fault := errors.AsType[*openCodeListFault](err); fault {
		return errOpenCodeMalformed
	}
	return err
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

// openCodeKindRead reports whether kind is one of the eleven kinds of message
// an export holds ([openCodeKind]).
func openCodeKindRead(kind openCodeKind) bool {
	switch kind {
	case openCodeUser, openCodeAssistant, openCodeCompaction, openCodeSynthetic, openCodeSystem, openCodeSkill,
		openCodeShell, openCodeIdle, openCodeAgentSwitched, openCodeModelSwitched, openCodeLocationSwitched:
		return true
	}
	return false
}

// openCodeStepNames and openCodePartTimes are the names a step's and a part's
// `time` may hold.
var (
	openCodeStepNames = openCodeNames("created", "streamed", "completed")
	openCodePartTimes = openCodeNames("created", "ran", "completed")
)

// openCodeStepTimes holds a step's `time` to its set, each name once, and its
// `completed` to a JSON number above zero: the export holds settled steps
// only, and a step with any other is one the import may drop.
func openCodeStepTimes(raw json.RawMessage) error {
	if !wireObject(raw) {
		return errOpenCodeMalformed
	}
	var (
		keys      openCodeKeys
		completed json.RawMessage
	)
	_ = eachKeyInPlace(raw, func(name []byte, value json.RawMessage) error {
		if keys.see(openCodeStepNames, name) == 2 {
			completed = value
		}
		return nil
	})
	switch {
	case keys.twice:
		return errOpenCodeTwice
	case keys.other:
		return errOpenCodeShape
	case !positiveNumber(completed):
		return errOpenCodeMalformed
	}
	return nil
}

// positiveNumber reports whether raw is a JSON number above zero: no sign,
// and a digit other than 0 before its exponent.
func positiveNumber(raw json.RawMessage) bool {
	if len(raw) == 0 || raw[0] < '0' || raw[0] > '9' {
		return false
	}
	if e := bytes.IndexAny(raw, "eE"); e >= 0 {
		raw = raw[:e]
	}
	return bytes.ContainsAny(raw, "123456789")
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

// part reads one tool item: counted for the one class whose call it is the
// verbatim answer of, else passed. A part is passed where any rule fails
// ([openCodePartState]): passing only lowers a class's parts, which can turn
// a held class told and never the other way.
func (w *openCodeHoldingsWalk) part(item openCodeItemWire) {
	if !item.keys.within(openCodePartNames) || item.Executed != nil && string(item.Executed) != "false" {
		return
	}
	if item.Time != nil && !keysWithin(item.Time, openCodePartTimes) {
		return
	}
	name, ok := bridgedToolName(item.Name)
	if !ok {
		return
	}
	state, ok := openCodePartState(item.State)
	if !ok {
		return
	}
	args, err := toolargs.Key(state.input)
	if err != nil {
		return
	}
	classes := w.candidates[partKey{name: name, args: args, failed: state.failed}]
	if class, ok := matchedClass(classes, state); ok {
		parts := w.classes[class]
		if len(parts.steps) <= parts.calls {
			parts.steps = append(parts.steps, w.step)
		}
	}
}

// keysWithin reports whether raw is an object whose names are names', each
// once.
func keysWithin(raw json.RawMessage, names map[string]int) bool {
	if !wireObject(raw) {
		return false
	}
	var keys openCodeKeys
	_ = eachKeyInPlace(raw, func(name []byte, _ json.RawMessage) error {
		keys.see(names, name)
		return nil
	})
	return !keys.other && !keys.twice
}

// bridgedToolName is the engine's tool a part's `name` names, `crewlet_` and
// a name the bridge files a tool by, its raw span held to that bound before
// it is decoded.
func bridgedToolName(raw json.RawMessage) (string, bool) {
	most := len(openCodeBridgedPrefix) + harnessprofile.ToolNameMax()
	if len(raw) < 2 || raw[0] != '"' || len(raw) > 6*most+2 {
		return "", false
	}
	var name string
	if wireDecode(raw, &name) != nil {
		return "", false
	}
	tool, ok := strings.CutPrefix(name, openCodeBridgedPrefix)
	if !ok || len(tool) > harnessprofile.ToolNameMax() || !harnessprofile.ToolNameChars(tool) {
		return "", false
	}
	return tool, true
}

// partState is what a counted part's `state` says: its input's raw span, its
// text's (a completed call's one text item, a failed one's message), and
// whether it failed.
type partState struct {
	input, text json.RawMessage
	failed      bool
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

// openCodePartState reads a part's `state` in one pass, whatever its key
// order, keeping each member as the sub-slice it is: exactly OpenCode's keys
// for its status, each once; a completed call's `metadata` exactly
// `{"truncated": false}` and its `content` one text item; a failed call's
// `error` a `tool.execution` with its message; the input an object within the
// longest argument text the log keeps whole. False where any of it is not so.
func openCodePartState(raw json.RawMessage) (partState, bool) {
	if !wireObject(raw) {
		return partState{}, false
	}
	var (
		keys                                  openCodeKeys
		status, input, content, meta, failure json.RawMessage
	)
	_ = eachKeyInPlace(raw, func(name []byte, value json.RawMessage) error {
		switch keys.see(openCodeStateNames, name) {
		case stateStatus:
			status = value
		case stateInput:
			input = value
		case stateContent:
			content = value
		case stateMetadata:
			meta = value
		case stateError:
			failure = value
		}
		return nil
	})
	if len(input) == 0 || input[0] != '{' || len(input) > sandbox.MaxSubmissionArgsBytes {
		return partState{}, false
	}
	switch {
	case stringIs(status, openCodeCompleted, len(openCodeCompleted)) && keys.within(openCodeCompletedState) &&
		keys.seen == openCodeCompletedState && untruncated(meta):
		text, ok := oneTextItem(content)
		return partState{input: input, text: text}, ok
	case stringIs(status, openCodeCallFailed, len(openCodeCallFailed)) && keys.within(openCodeFailedState) &&
		keys.seen == openCodeFailedState:
		text, ok := executionFailure(failure)
		return partState{input: input, text: text, failed: true}, ok
	}
	return partState{}, false
}

// untruncated reports whether raw is exactly `{"truncated": false}`: OpenCode's
// word that it handed the call's whole answer.
func untruncated(raw json.RawMessage) bool {
	if !wireObject(raw) {
		return false
	}
	members, ok := 0, true
	_ = eachKeyInPlace(raw, func(name []byte, value json.RawMessage) error {
		members++
		ok = ok && string(name) == "truncated" && string(value) == "false"
		return nil
	})
	return ok && members == 1
}

// oneTextItem is the text of raw, a completed call's content, where it is one
// item of exactly `type` "text" and a string `text`.
func oneTextItem(raw json.RawMessage) (json.RawMessage, bool) {
	var (
		text  json.RawMessage
		items int
		ok    bool
	)
	if eachElement(raw, func(element json.RawMessage, at int) error {
		if items++; items > 1 {
			return errOpenCodeShape
		}
		element = raw[at : at+len(element)]
		text, ok = memberPair(element, "text", "text")
		return nil
	}) != nil {
		return nil, false
	}
	return text, ok && items == 1
}

// executionFailure is the message of raw, a failed call's error, where it is
// exactly `type` "tool.execution" and a string `message`.
func executionFailure(raw json.RawMessage) (json.RawMessage, bool) {
	return memberPair(raw, "tool.execution", "message")
}

// memberPair is the string member named field of raw, an object of exactly
// `type` (equal to kind) and field, each once.
func memberPair(raw json.RawMessage, kind, field string) (json.RawMessage, bool) {
	if !wireObject(raw) {
		return nil, false
	}
	names := openCodeNames("type", field)
	var (
		keys            openCodeKeys
		typed, valueRaw json.RawMessage
	)
	_ = eachKeyInPlace(raw, func(name []byte, value json.RawMessage) error {
		switch keys.see(names, name) {
		case 0:
			typed = value
		case 1:
			valueRaw = value
		}
		return nil
	})
	ok := !keys.other && !keys.twice && keys.seen == 3 && stringIs(typed, kind, len(kind)) &&
		len(valueRaw) >= 2 && valueRaw[0] == '"'
	return valueRaw, ok
}

// matchedClass is the one class of classes whose output a part's text is,
// false where none is or more than one: a part two cut classes both match
// could hold both classes' calls, so it counts for neither. Its text is
// decoded only where its raw span could hold a candidate's size
// ([textMayBe]), and once; a completed part's empty or blank text never
// counts, since a provider may hand it otherwise.
func matchedClass(classes []sandbox.CallClass, state partState) (sandbox.CallClass, bool) {
	spanned := len(state.text) - 2
	possible := slices.ContainsFunc(classes, func(c sandbox.CallClass) bool { return textMayBe(spanned, c.Text) })
	if !possible {
		return sandbox.CallClass{}, false
	}
	var text string
	if wireDecode(state.text, &text) != nil || !state.failed && strings.TrimSpace(text) == "" {
		return sandbox.CallClass{}, false
	}
	var (
		found   sandbox.CallClass
		matches int
	)
	for _, c := range classes {
		if textMayBe(spanned, c.Text) && textMatches(text, c.Text) {
			found, matches = c, matches+1
		}
	}
	return found, matches == 1
}

// textMayBe reports whether a JSON string spanning spanned bytes between its
// quotes can decode to a text of key's size: a byte decodes to at most three
// (an invalid byte as U+FFFD), and six bytes (an escape) to at least one.
func textMayBe(spanned int, key sandbox.TextKey) bool {
	return key.Bytes > 0 && 3*spanned >= key.Bytes && spanned <= 6*key.Bytes
}

// textMatches reports whether text is the output key is of: every byte, where
// the log kept it whole; its size and all the opening the log kept, where it
// cut it, never a text the take's scrub rewrote, which may differ past it.
func textMatches(text string, key sandbox.TextKey) bool {
	switch {
	case len(text) != key.Bytes:
		return false
	case key.Whole:
		return sumString(text) == key.Sum
	}
	return key.Opening <= len(text) && !strings.Contains(text, redact.Marker) &&
		sumString(text[:key.Opening]) == key.Sum
}

// sumString is s's SHA-256, written a chunk at a time, so a text of the walk's
// bound is never copied whole to be summed.
func sumString(s string) [32]byte {
	h := sha256.New()
	var chunk [32 << 10]byte
	for len(s) > 0 {
		n := copy(chunk[:], s)
		h.Write(chunk[:n])
		s = s[n:]
	}
	var sum [32]byte
	h.Sum(sum[:0])
	return sum
}

// ended is the reading once the walk read the export whole: no reading where
// `info` is absent or names another session, or the run's start was never
// read; else the verdict ([openCodeVerdict]).
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
	return openCodeVerdict(ask, parts, w.compacted)
}

// openCodeVerdict is the reading's answer from the walk's facts, parts each
// stated class's counted parts by the steps they lie in, in the order read:
// the pool's calls counted by class, each class held where every call of it
// has a part and no call the log cannot state may be of it, and the held
// classes' parts in an order the log's explains (the order check). A PURE
// FUNCTION of the facts and the ask, so a take can take it outside a relaunch.
func openCodeVerdict(ask sandbox.HoldingsAsk, parts map[sandbox.CallClass][]int,
	compacted bool) (sandbox.SessionHoldings, error) {

	pool := openCodePool(ask)
	classes := make(map[sandbox.CallClass]sandbox.ClassRecords, len(pool.calls))
	for class, calls := range pool.calls {
		counted := min(len(parts[class]), calls+1)
		records := sandbox.ClassRecords{Calls: calls, Parts: counted, Wild: pool.wild(class)}
		records.Held = records.Calls >= 1 && records.Parts == records.Calls && !records.Wild
		classes[class] = records
	}
	if !openCodeInOrder(ask, parts, classes) {
		return sandbox.SessionHoldings{}, &holdingsError{why: "the session holds the run's calls in an order its " +
			"log does not explain"}
	}
	members := map[string]sandbox.CallClass{}
	for _, c := range ask.Calls {
		if class, ok := sandbox.ClassOf(c); ok && c.Asked {
			members[c.ID] = class
		}
	}
	return sandbox.SessionHoldings{Read: true, Kind: sandbox.HoldingsByContent, Members: members, Classes: classes,
		Compacted: compacted}, nil
}

// openCodeInOrder reports whether the held classes' calls, taken in admission
// order, meet each class's next part in a step never before the last one met:
// a step's calls run at once, and the next step's only after they all joined.
func openCodeInOrder(ask sandbox.HoldingsAsk, parts map[sandbox.CallClass][]int,
	classes map[sandbox.CallClass]sandbox.ClassRecords) bool {

	calls := slices.Clone(ask.Calls)
	slices.SortStableFunc(calls, func(a, b sandbox.LoggedCall) int { return cmp.Compare(a.Seq, b.Seq) })
	next, at := map[sandbox.CallClass]int{}, 0
	for _, c := range calls {
		class, ok := sandbox.ClassOf(c)
		if !ok || c.Held || !classes[class].Held {
			continue
		}
		step := parts[class][next[class]]
		if step < at {
			return false
		}
		next[class]++
		at = step
	}
	return true
}

// openCodeCallPool is a reading's pool: the calls of each stated class, the
// digests they carry and whether one carries none; and the wildcards, every
// call that may have left a part the log cannot state the class of.
type openCodeCallPool struct {
	calls      map[sandbox.CallClass]int
	digests    map[sandbox.CallClass]map[string]bool
	digestless map[sandbox.CallClass]bool
	// named are the wildcards known by tool and digest ("" where none); held
	// the calls a person's approval held, by tool, failure and answer; and
	// unnamed whether a call or count unlisted names no tool, onlyFailed
	// whether every call they count failed, never so for one in flight.
	named               []sandbox.CallIdentity
	held                []heldWildcard
	unnamed, onlyFailed bool
}

// heldWildcard is a call a person's approval held: its part, if any, answers
// with the held answer, whose text key is text where stated.
type heldWildcard struct {
	name   string
	failed bool
	text   sandbox.TextKey
	stated bool
}

// openCodePool is ask's pool: every call but a held one by its class, or a
// wildcard where its class is not stated; each held call, in flight or
// dropped call a wildcard.
func openCodePool(ask sandbox.HoldingsAsk) openCodeCallPool {
	pool := openCodeCallPool{calls: map[sandbox.CallClass]int{}, digests: map[sandbox.CallClass]map[string]bool{},
		digestless: map[sandbox.CallClass]bool{}, onlyFailed: true}
	for _, c := range ask.Calls {
		if c.Held {
			text, stated := sandbox.TextKeyOf(c.Output, c.OutputBytes)
			pool.held = append(pool.held, heldWildcard{name: c.Name, failed: c.Failed, text: text, stated: stated})
			continue
		}
		class, ok := sandbox.ClassOf(c)
		if !ok {
			pool.named = append(pool.named, sandbox.CallIdentity{Name: c.Name, Digest: c.Digest})
			continue
		}
		pool.calls[class]++
		if c.Digest == "" {
			pool.digestless[class] = true
			continue
		}
		if pool.digests[class] == nil {
			pool.digests[class] = map[string]bool{}
		}
		pool.digests[class][c.Digest] = true
	}
	for _, id := range ask.Unlisted {
		if id.Name == "" {
			pool.unnamed, pool.onlyFailed = true, pool.onlyFailed && !id.InFlight && id.OnlyFailed
			continue
		}
		pool.named = append(pool.named, id)
	}
	return pool
}

// wild reports whether a call the log cannot state the class of may be of
// class: one of its tool whose digest is none, or one a call of the class
// carries, or whose class lacks a digest; a held call of its tool, failure
// and answer; or a count with no name, of failures only where it says so.
func (p openCodeCallPool) wild(class sandbox.CallClass) bool {
	if p.unnamed && (!p.onlyFailed || class.Failed) {
		return true
	}
	for _, id := range p.named {
		if id.Name == class.Name && (id.Digest == "" || p.digestless[class] || p.digests[class][id.Digest]) {
			return true
		}
	}
	for _, h := range p.held {
		if h.name == class.Name && h.failed == class.Failed && (!h.stated || h.text == class.Text) {
			return true
		}
	}
	return false
}
