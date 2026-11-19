package executor

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools/toolargs"
)

// THE EXPORT WALK'S ORACLES: the decode walk ([walkOpenCodeExport]) the
// holdings reading and a person's transcript read an export by before each
// became a visitor of the one export walk ([openCodeHoldingsWalk],
// [openCodeReading]), kept so their differentials hold every answer to it. The
// holdings half shares the reading's state, its pool and its verdict; the
// person's half reads each message held whole, applying where it decides them
// what reads otherwise past the value bound (R1 to R6) and the read's lines
// bound.

// wholeHoldings is a reading as the decode walk's visitor.
type wholeHoldings struct{ *openCodeHoldingsWalk }

// readOpenCodeHoldingsWhole reads plain, a session's export as kept (the take's
// scrub's output), for the calls ask marks asked: the one export walk, each
// value held to lineBytes, the reading its visitor ([openCodeHoldingsWalk]);
// nothing but whitespace after the document; then the verdict
// ([openCodeVerdict]), a pure function of the walk's facts and the ask. A
// take reads a session through it too, inside its own budget and slot.
func readOpenCodeHoldingsWhole(ctx context.Context, plain io.Reader, lineBytes int, session string,
	start sandbox.RunStart, ask sandbox.HoldingsAsk) (sandbox.SessionHoldings, error) {

	w := newOpenCodeHoldingsWalk(session, start, ask)
	held := &heldValue{r: plain, max: lineBytes}
	dec := jsontext.NewDecoder(held, threadDecoding)
	err := walkOpenCodeExport(ctx, dec, held, wholeHoldings{w})
	if err == nil {
		held.reset()
		if _, err = dec.ReadToken(); errors.Is(err, io.EOF) {
			err = nil
		} else if err == nil {
			err = errors.New("another value follows the document")
		}
	}
	if err != nil {
		return sandbox.SessionHoldings{}, openCodeReadFailedWhole(ctx, held, lineBytes, err)
	}
	return w.ended(ask)
}

// openCodeReadFailedWhole is why a walk read no export whole: the reading's own
// words, the caller gone, a value past lineBytes, the plain meter's bound, or
// the export read as no export, its cause a decoder's or a reader's words,
// which [holdingsError] cuts.
func openCodeReadFailedWhole(ctx context.Context, held *heldValue, lineBytes int, err error) error {
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

// topKey reads `info`, once, an object, and whether its id is the session's;
// every other key is passed, as the import passes it.
func (w wholeHoldings) topKey(name string, value json.RawMessage) error {
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
func (w wholeHoldings) messageList() error {
	if w.lists++; w.lists > 1 {
		return errOpenCodeTwice
	}
	return nil
}

// notMessage refuses a list element that is no message.
func (wholeHoldings) notMessage() error { return errOpenCodeMalformed }

// message reads one message by its kind: the run's start, a compaction, which
// drops every part fact, and an assistant step after both, whose keys, times
// and items are held to their sets and whose tool items are read as parts
// ([wholeHoldings.part]).
func (w wholeHoldings) message(ctx context.Context, m openCodeMessageWire) error {
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

// openCodeStepTimes holds a step's `time` to its set, each name once, and its
// `completed` to a number above zero ([positiveNumber]): the export holds
// settled steps only, and a step with any other is one the import may drop.
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

// part reads one tool item: counted for the one class whose call it is the
// verbatim answer of, else passed. A part is passed where any rule fails
// ([openCodePartState]): passing only lowers a class's parts, which can turn
// a held class told and never the other way.
func (w wholeHoldings) part(item openCodeItemWire) {
	if !item.keys.within(openCodePartNames) || item.Executed != nil && string(item.Executed) != "false" {
		return
	}
	if item.Time != nil && !keysWithin(item.Time, openCodePartTimes) {
		return
	}
	name, ok := bridgedToolName(item.Name, openCodeBridgedPrefix())
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
	if class, ok := matchedClassWhole(classes, state); ok {
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

// partState is what a counted part's `state` says: its input's raw span, its
// text's (a completed call's one text item, a failed one's message), and
// whether it failed.
type partState struct {
	input, text json.RawMessage
	failed      bool
}

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

// matchedClassWhole is the one class of classes whose output a part's text is,
// false where none is or more than one: a part two cut classes both match
// could hold both classes' calls, so it counts for neither. Its text is
// decoded only where its raw span could hold a candidate's size
// ([textMayBe]), and once; a completed part's empty or blank text never
// counts, since a provider may hand it otherwise.
func matchedClassWhole(classes []sandbox.CallClass, state partState) (sandbox.CallClass, bool) {
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

// errValuePastBound is one value of an export past the bound one may hold.
var errValuePastBound = errors.New("executor: a value of the export is past the bound one may hold")

// openCodeContent is one item of an assistant step's content
// (`SessionMessage.AssistantContent`), told apart by `type`: `text` the model
// wrote, its `reasoning`, or a `tool` it called — the tool's Name, the call
// as it stands, and when OpenCode recorded it, in milliseconds since the
// epoch: when the model began it, when it ran, when it settled. Read an item
// at a time ([eachOpenCodeItem]).
type openCodeContent struct {
	Type  string        `json:"type"`
	Text  string        `json:"text"`
	Name  string        `json:"name"`
	State openCodeState `json:"state"`
	Time  struct {
		Created   int64 `json:"created"`
		Ran       int64 `json:"ran"`
		Completed int64 `json:"completed"`
	} `json:"time"`
}

// openCodeState is a tool call as it stands (`SessionMessage.ToolState`), told
// apart by `status`: `streaming` while the model writes its input, which is
// then the text written so far; `running` once it is called; `completed`, its
// answer the list in `content`; or `error`, its failure in `error` and what
// it answered before it in `content`. Each raw, and read by what it holds.
type openCodeState struct {
	Status  string          `json:"status"`
	Input   json.RawMessage `json:"input"`
	Content json.RawMessage `json:"content"`
	Error   json.RawMessage `json:"error"`
}

// see notes name, one of names' or not, and is its index in names, -1 where
// it is none of them.
func (k *openCodeKeys) see(names map[string]int, name []byte) int {
	i, ok := names[string(name)]
	if !ok {
		i = -1
	}
	k.note(i)
	return i
}

// openCodeItemWire is one item of a step's content as its one decode leaves
// it ([decodeOpenCodeItem]): each key an [openCodeContent] holds, and the
// call's `executed`, by its exact name and raw, a sub-slice of the content
// read in place; the item's key facts; and fault, why an element is no item,
// "" where it is one.
type openCodeItemWire struct {
	Type, Text, Name, State, Time, Executed json.RawMessage
	keys                                    openCodeKeys
	fault                                   string
}

// decodeOpenCodeItem is the item element holds, its members read in place, a
// name written twice by its last value, as JSON.parse reads it.
func decodeOpenCodeItem(element json.RawMessage) openCodeItemWire {
	if !wireObject(element) {
		return openCodeItemWire{fault: "an item of a step's content is no object"}
	}
	var w openCodeItemWire
	_ = eachKeyInPlace(element, func(name []byte, value json.RawMessage) error {
		switch w.keys.see(openCodeItemNames, name) {
		case itemType:
			w.Type = value
		case itemText:
			w.Text = value
		case itemName:
			w.Name = value
		case itemState:
			w.State = value
		case itemTime:
			w.Time = value
		case itemExecuted:
			w.Executed = value
		}
		return nil
	})
	return w
}

// item is the item w holds, DECODED LOOSELY, as the export's whole decode read
// one: a key of another kind than its field's reads as that field's zero
// value, a call's state or its times that are no object as none, and the rest
// of the item as written.
func (w openCodeItemWire) item() openCodeContent {
	item := openCodeContent{Type: looseString(w.Type), Text: looseString(w.Text), Name: looseString(w.Name)}
	if wireObject(w.State) {
		var status json.RawMessage
		_ = eachKeyInPlace(w.State, func(name []byte, value json.RawMessage) error {
			switch string(name) {
			case "status":
				status = value
			case "input":
				item.State.Input = value
			case "content":
				item.State.Content = value
			case "error":
				item.State.Error = value
			}
			return nil
		})
		item.State.Status = looseString(status)
	}
	if wireObject(w.Time) {
		_ = eachKeyInPlace(w.Time, func(name []byte, value json.RawMessage) error {
			switch string(name) {
			case "created":
				item.Time.Created = looseInt(value)
			case "ran":
				item.Time.Ran = looseInt(value)
			case "completed":
				item.Time.Completed = looseInt(value)
			}
			return nil
		})
	}
	return item
}

// openCodeListFault is why a step's content is no list, or stops part way:
// its walk's own error ([eachOpenCodeItem]).
type openCodeListFault struct{ err error }

func (f *openCodeListFault) Error() string {
	return "a step's content is not one list: " + f.err.Error()
}

func (f *openCodeListFault) Unwrap() error { return f.err }

// eachOpenCodeItem hands each item of a step's content to each, in order, and
// returns each's first error, reading no item after it; else, where the
// content is no list or stops part way, an [*openCodeListFault]. An element
// that is no object is handed as a wire whose fault says so.
//
// AN ITEM AT A TIME ([eachElement]), each decoded once, in place
// ([decodeOpenCodeItem]): never the whole list first, which held 160 bytes per
// element before any was read, and never a copy of an item's value.
func eachOpenCodeItem(content json.RawMessage, each func(openCodeItemWire) error) error {
	var stopped error
	err := eachElement(content, func(element json.RawMessage, _ int) error {
		stopped = each(decodeOpenCodeItem(element))
		return stopped
	})
	switch {
	case stopped != nil:
		return stopped
	case err != nil:
		return &openCodeListFault{err: err}
	}
	return nil
}

// openCodeExportWhole reads the export document a message at a time, each
// held whole, passing its other fields: `{"info": …, "messages": [{"type":
// "user", …}, …]}`. A DOCUMENT THAT ENDS BEFORE IT CLOSES ENDS PART WAY
// THROUGH: the decoder's syntax error at the input's end, which reads the
// input to its end, is the document cut short, not "no export".
func (b *transcriptBuilder) openCodeExportWhole(ctx context.Context, archive io.Reader) error {
	held := &heldValue{r: archive, max: math.MaxInt}
	dec := jsontext.NewDecoder(held, threadDecoding)
	err := b.pastHeld(held, walkOpenCodeExport(ctx, dec, held, &openCodePerson{b: b}))
	var syntax *jsontext.SyntacticError
	switch {
	case !errors.As(err, &syntax):
		return err
	case held.ended && errors.Is(syntax.Err, io.ErrUnexpectedEOF):
		return fmt.Errorf("%w: %w", io.ErrUnexpectedEOF, err)
	}
	return fmt.Errorf("%w: %w", errNotExport, err)
}

// openCodeVisitor is what the decode walk ([walkOpenCodeExport]) hands what it
// reads to, as it reads it: a person's transcript ([openCodePerson]), its one
// reader, a holdings reading being a visitor of the one export walk
// ([scanExport]). Each value it is handed is a sub-slice of the walk's
// buffer, valid only until it returns, and an error it answers ends the walk.
type openCodeVisitor interface {
	// topKey is a top-level key but `messages`, its value held to the bound.
	topKey(name string, value json.RawMessage) error
	// messageList is a `messages` key, before its list is walked.
	messageList() error
	// message is one message of a list, decoded once ([openCodeNext]).
	message(ctx context.Context, w openCodeMessageWire) error
	// notMessage is an element of a list that is no object.
	notMessage() error
}

// walkOpenCodeExport walks the document dec reads through held, handing each
// key, list and message to v as it reads it ([openCodeVisitor]): held is reset
// before every key and message, and every `messages` list met is walked. It
// returns at the document's `}`, reading nothing after it. EVERY VALUE IS READ
// IN PLACE in dec's buffer, which holds one value at a time, never copied out.
func walkOpenCodeExport(ctx context.Context, dec *jsontext.Decoder, held *heldValue, v openCodeVisitor) error {
	if err := expectDelim(dec, '{'); err != nil {
		return err
	}
	for held.reset(); dec.PeekKind() != '}'; held.reset() {
		key, keyErr := dec.ReadToken()
		if keyErr != nil {
			return keyErr
		}
		held.reset()
		if name := key.String(); name != "messages" {
			value, passErr := dec.ReadValue()
			if passErr != nil {
				return passErr
			}
			if keyErr = v.topKey(name, value); keyErr != nil {
				return keyErr
			}
			continue
		}
		if listErr := v.messageList(); listErr != nil {
			return listErr
		}
		if openErr := expectDelim(dec, '['); openErr != nil {
			return openErr
		}
		for held.reset(); dec.PeekKind() != ']'; held.reset() {
			if messageErr := openCodeNext(ctx, dec, v); messageErr != nil {
				return messageErr
			}
		}
		if closeErr := expectDelim(dec, ']'); closeErr != nil {
			return closeErr
		}
	}
	return expectDelim(dec, '}')
}

// openCodeNext reads the next message of the list, in place, and hands it to
// v decoded once ([decodeOpenCodeMessage]). A message that is no object is
// handed as none; within one, a field whose type a release changed is
// skipped rather than losing the message, as the stream's events are
// ([decodeOpenCodeEvent]), and every key is read exactly as written.
func openCodeNext(ctx context.Context, dec *jsontext.Decoder, v openCodeVisitor) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	raw, err := dec.ReadValue()
	if err != nil {
		return err
	}
	if raw.Kind() != '{' {
		return v.notMessage()
	}
	return v.message(ctx, decodeOpenCodeMessage(raw))
}

// openCodePerson is a person's transcript as the decode walk's visitor: it
// reads each message by its kind, says an element that is no message, and
// passes every other key, as the import does; and it ends the read past the
// messages, items and parts one read frames ([transcriptLinesMax]), framed
// counting how many it has, nothing of the message it ends inside read.
type openCodePerson struct {
	b      *transcriptBuilder
	framed int
}

func (*openCodePerson) topKey(string, json.RawMessage) error { return nil }

func (*openCodePerson) messageList() error { return nil }

func (p *openCodePerson) message(ctx context.Context, w openCodeMessageWire) error {
	if p.framed += framedIn(w.raw); p.framed > transcriptLinesMax {
		return p.b.linesPast()
	}
	return p.b.openCodeMessage(ctx, w.message())
}

func (p *openCodePerson) notMessage() error {
	if p.framed++; p.framed > transcriptLinesMax {
		return p.b.linesPast()
	}
	return p.b.unread("a message of the export is not a message, and was not read")
}

// framedIn is how many records and elements of them the one export walk frames
// for a person in message: the message, every element of each of its
// `content` lists, and every element of each `content` list of each `state`
// of each item that is an object, every key written twice among them.
func framedIn(message json.RawMessage) int {
	n := 1
	each := func(object json.RawMessage, name string, of func(json.RawMessage)) {
		_ = eachKeyInPlace(object, func(key []byte, value json.RawMessage) error {
			if string(key) == name {
				of(value)
			}
			return nil
		})
	}
	elements := func(list json.RawMessage, of func(json.RawMessage)) {
		_ = eachElement(list, func(element json.RawMessage, _ int) error {
			n++
			of(element)
			return nil
		})
	}
	each(message, "content", func(content json.RawMessage) {
		elements(content, func(item json.RawMessage) {
			each(item, "state", func(state json.RawMessage) {
				each(state, "content", func(answer json.RawMessage) { elements(answer, func(json.RawMessage) {}) })
			})
		})
	})
	return n
}

// openCodeMessageWire is one message of an export as its one decode leaves it
// ([decodeOpenCodeMessage]): each key a reader reads of it, by its exact name
// and raw, a sub-slice of the message read in place, and its key facts.
type openCodeMessageWire struct {
	ID, Type, Time, Text, Content, Status, Summary json.RawMessage
	keys                                           openCodeKeys
	raw                                            json.RawMessage
}

// decodeOpenCodeMessage is the message raw holds, read in place, a name
// written twice by its last value, as JSON.parse reads it.
func decodeOpenCodeMessage(raw json.RawMessage) openCodeMessageWire {
	w := openCodeMessageWire{raw: raw}
	_ = eachKeyInPlace(raw, func(name []byte, value json.RawMessage) error {
		switch w.keys.see(openCodeMessageNames, name) {
		case msgID:
			w.ID = value
		case msgType:
			w.Type = value
		case msgTime:
			w.Time = value
		case msgText:
			w.Text = value
		case msgContent:
			w.Content = value
		case msgStatus:
			w.Status = value
		case msgSummary:
			w.Summary = value
		}
		return nil
	})
	return w
}

// message is the message w holds, DECODED LOOSELY, as the export's whole
// decode read one: a key of another kind than its field's reads as that
// field's zero value, a time that is no object as none, and the rest of the
// message as written. Its tokens, its error and when a step settled are left
// as none: a person's transcript reads none of them, and the context a session
// carries reads them off the export itself ([exportMeter]).
func (w openCodeMessageWire) message() openCodeMessage {
	m := openCodeMessage{Type: openCodeKind(looseString(w.Type)), Text: looseString(w.Text), Content: w.Content,
		Status: looseString(w.Status), Summary: looseString(w.Summary)}
	if wireObject(w.Time) {
		_ = eachKeyInPlace(w.Time, func(name []byte, value json.RawMessage) error {
			if string(name) == "created" {
				m.Time.Created = looseInt(value)
			}
			return nil
		})
	}
	return m
}

// pastHeld is how a walk of an export ended: a part of it past the bound one
// may hold is said, and ends the read where it was; every other end is
// returned as it came.
func (b *transcriptBuilder) pastHeld(held *heldValue, err error) error {
	if err == nil || !held.over {
		return err
	}
	if unreadErr := b.unread(fmt.Sprintf("a part of the export past %s was not read, and "+
		"nothing after it could be", humanSize(int64(b.bounds.LineBytes)))); unreadErr != nil {
		return unreadErr
	}
	b.cut = CutRead
	return errTranscriptPage
}

// openCodeMessage reads one message by its kind: a person's is the brief, a
// note or a late call (the run's plugin admits both as one), by its text; an
// assistant's step is the model's words and calls
// ([transcriptBuilder.openCodeStep]); a completed compaction is its summary.
//
// EVERY OTHER KIND IS OPENCODE'S OWN and no entry, as Claude Code's own records
// are none: synthetic notices (never searched for a note: one can carry what a
// tool printed), system instruction updates, skill bodies, a person's shell
// commands, idle, and switches of agent, model or location. A compaction
// running or failed handed the model no summary. A KIND THIS READ DOES NOT KNOW
// IS SAID ([EntryUnread]): it is from a release this read was not written for,
// and its words may be what the model read.
func (b *transcriptBuilder) openCodeMessage(ctx context.Context, m openCodeMessage) error {
	at := unixMillis(m.Time.Created)
	switch m.Type {
	case openCodeUser:
		return b.proseWhole(ctx, EntryBrief, at, m.Text)
	case openCodeAssistant:
		return b.openCodeStep(ctx, m.Content, at)
	case openCodeCompaction:
		if m.Status != openCodeCompleted {
			return nil
		}
		return b.proseWhole(ctx, EntrySummary, at, m.Summary)
	case openCodeSynthetic, openCodeSystem, openCodeSkill, openCodeShell, openCodeIdle,
		openCodeAgentSwitched, openCodeModelSwitched, openCodeLocationSwitched:
		return nil
	}
	return b.unread("a message of the export of no kind this read knows was not read")
}

// openCodeStep reads an assistant step's content, an item at a time
// ([eachOpenCodeItem]): its text as the model's words, and each tool it called
// ([transcriptBuilder.openCodeTool]). Reasoning is the model's working and no
// entry, as Claude Code's thinking is none, and so is an item of a type this
// build does not know; content that is no list is said.
func (b *transcriptBuilder) openCodeStep(ctx context.Context, content json.RawMessage, at time.Time) error {
	content = bytes.TrimSpace(content)
	switch {
	case absent(content):
		return nil
	case content[0] != '[':
		return b.unread("a message of the export whose content is not a list was not read")
	}
	err := eachOpenCodeItem(content, func(w openCodeItemWire) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch item := w.item(); item.Type {
		case "text":
			return b.proseWhole(ctx, EntryModel, at, item.Text)
		case "tool":
			return b.openCodeTool(item, w.State, at)
		}
		return nil
	})
	// A LIST THAT STOPS PART WAY is read up to its fault: the items before it
	// were read.
	if _, fault := errors.AsType[*openCodeListFault](err); fault {
		return nil
	}
	return err
}

// openCodeTool reads one tool the model called: the call, its input as JSON,
// dated when it ran — or, one that never ran, when the model began it — and,
// once it settled, what answered it, dated when it did: a completed call's
// answer, or a failed one's failure, marked. A call OpenCode stopped while the
// model was still writing it, or while it ran, has no result.
//
// PAST THE VALUE BOUND: a tool's name past it whose redaction the stream
// refuses makes both entries [EntryUnread] (R6); an input written as a string
// past it is shown as written (R3); and a result is held to what the walk can
// stream ([resultPast]).
func (b *transcriptBuilder) openCodeTool(item openCodeContent, state json.RawMessage, at time.Time) error {
	called := firstInstant(unixMillis(item.Time.Ran), firstInstant(unixMillis(item.Time.Created), at))
	settled := firstInstant(unixMillis(item.Time.Completed), at)
	due := item.State.Status == openCodeCompleted || item.State.Status == openCodeCallFailed
	if len(item.Name) > b.valueMax {
		s := b.streamedOpening()
		writeRunes(s, item.Name)
		if _, err := s.opening(transcriptNameMaxBytes); err != nil {
			if err = b.add(TranscriptEntry{Kind: EntryUnread}, unredactedText); err != nil || !due {
				return err
			}
			return b.add(TranscriptEntry{Kind: EntryUnread}, unredactedText)
		}
	}
	if err := b.add(TranscriptEntry{Kind: EntryToolCall, Tool: item.Name, At: called},
		openCodeCallInput(item.State.Input, b.valueMax)); err != nil || !due {
		return err
	}
	if resultPast(item.State, state, b.valueMax) {
		return b.unread(fmt.Sprintf("a call's result in the export runs past %s before what opens it is "+
			"written, and was not read", humanSize(int64(b.valueMax))))
	}
	if item.State.Status == openCodeCompleted {
		return b.add(TranscriptEntry{Kind: EntryToolResult, Tool: item.Name, At: settled},
			openCodeAnswer(item.State.Content, b.valueMax))
	}
	return b.add(TranscriptEntry{Kind: EntryToolResult, Tool: item.Name, At: settled, Failed: true},
		openCodeFailure(item.State, b.valueMax))
}

// resultPast reports whether a settled call's result is said in its place
// (R5): a text of it ran past valueMax (a failure's message or type, an
// answer's part, or its parts joined, in any occurrence of each, or the
// result's text, the failure's words and the answer joined) in a state not
// written in order ([stateInOrder]); or its failure says only its type, past
// valueMax.
func resultPast(s openCodeState, state json.RawMessage, valueMax int) bool {
	failure := map[string]json.RawMessage{}
	_ = eachKeyInPlace(s.Error, func(name []byte, value json.RawMessage) error {
		failure[string(name)] = value
		return nil
	})
	if s.Status == openCodeCallFailed && looseString(failure["message"]) == "" &&
		len(looseString(failure["type"])) > valueMax {
		return true
	}
	if stateInOrder(state) {
		return false
	}
	past := s.Status == openCodeCallFailed && len(openCodeFailure(s, valueMax)) > valueMax
	_ = eachKeyInPlace(state, func(name []byte, value json.RawMessage) error {
		switch string(name) {
		case "error":
			_ = eachKeyInPlace(value, func(key []byte, said json.RawMessage) error {
				past = past || (string(key) == "message" || string(key) == "type") && len(looseString(said)) > valueMax
				return nil
			})
		case "content":
			past = past || len(openCodeAnswer(value, valueMax)) > valueMax
			_ = eachElement(value, func(part json.RawMessage, _ int) error {
				_ = eachKeyInPlace(part, func(key []byte, text json.RawMessage) error {
					past = past || string(key) == "text" && len(looseString(text)) > valueMax
					return nil
				})
				return nil
			})
		}
		return nil
	})
	return past
}

// stateInOrder reports whether a call's state is written in the order its
// result is shown in, as OpenCode writes one: `status` before `error` and
// `content`, each written once, `error` before `content`, the failure's `type`
// and `message` once each, and each part of the answer its `type` before its
// `text`, its `type`, `text` and `mime` once each. ONLY THE MEMBERS THE
// RESULT'S TEXT IS MADE OF COUNT: a walk never asks for another, so another
// written twice never reaches it.
func stateInOrder(state json.RawMessage) bool {
	ok, seen := true, map[string]int{}
	once := func(object json.RawMessage, members ...string) {
		count := map[string]int{}
		_ = eachKeyInPlace(object, func(name []byte, _ json.RawMessage) error {
			if key := string(name); slices.Contains(members, key) {
				count[key]++
				ok = ok && count[key] < 2 && (key != "text" || count["type"] == 1)
			}
			return nil
		})
	}
	_ = eachKeyInPlace(state, func(name []byte, value json.RawMessage) error {
		key := string(name)
		seen[key]++
		switch key {
		case "status":
			ok = ok && seen[key] == 1 && seen["error"] == 0 && seen["content"] == 0
		case "error":
			ok = ok && seen[key] == 1 && seen["status"] > 0 && seen["content"] == 0
			if wireObject(value) {
				once(value, "type", "message")
			}
		case "content":
			ok = ok && seen[key] == 1 && seen["status"] > 0
			_ = eachElement(value, func(part json.RawMessage, _ int) error {
				if wireObject(part) {
					once(part, "type", "text", "mime")
				}
				return nil
			})
		}
		return nil
	})
	return ok
}

// proseWhole is [transcriptBuilder.prose], but a text past the value bound is
// one entry of kind, or none where blank (R1).
func (b *transcriptBuilder) proseWhole(ctx context.Context, kind EntryKind, at time.Time, text string) error {
	switch {
	case len(text) <= b.valueMax:
		return b.prose(ctx, kind, at, text)
	case strings.TrimSpace(text) == "":
		return nil
	}
	return b.add(TranscriptEntry{Kind: kind, At: at, Thread: b.thread}, text)
}

// openCodeCallInput is a call's input as one line of JSON: the object it was
// called with, or, for a call whose input the model was still writing, the
// text it had written — as JSON where it parses so far and is within
// valueMax (R3), and as written where it does not.
func openCodeCallInput(raw json.RawMessage, valueMax int) string {
	var written string
	if wireDecode(raw, &written) == nil {
		if len(written) > valueMax {
			return written
		}
		return compactJSON(json.RawMessage(written))
	}
	return compactJSON(raw)
}

// openCodeAnswer is what a tool answered, the list `Tool.Content` is, read an
// item at a time ([toolAnswer]): each text item's text, a line apart, as a
// Claude Code tool's result reads ([claudeResultText]), and each file it
// answered with as the words a file reads in ([openCodeFileText]). "" for an
// answer that is no list. A text item past valueMax is kept whatever it holds
// (R2).
func openCodeAnswer(raw json.RawMessage, valueMax int) string {
	return toolAnswer(raw, func(kind, text, mime string) (string, bool) {
		switch kind {
		case "text":
			return text, strings.TrimSpace(text) != "" || len(text) > valueMax
		case "file":
			return openCodeFileText(mime), true
		}
		return "", false
	})
}

// openCodeFailure is what a failed call answered, as OpenCode hands it to the
// model: the failure's message — the object `SessionError.Error`, its class
// where it says nothing more — and then whatever the call answered before it
// failed. The failure is decoded loosely, as the message around it is, and by
// its keys exactly as written.
func openCodeFailure(state openCodeState, valueMax int) string {
	var kind, message json.RawMessage
	if wireObject(state.Error) {
		_ = eachKeyInPlace(state.Error, func(name []byte, value json.RawMessage) error {
			switch string(name) {
			case "type":
				kind = value
			case "message":
				message = value
			}
			return nil
		})
	}
	said := cmp.Or(looseString(message), looseString(kind))
	answered := openCodeAnswer(state.Content, valueMax)
	switch {
	case answered == "":
		return said
	case said == "":
		return answered
	}
	return said + "\n" + answered
}

// expectDelim reads the next token of dec, which must be want; where it is
// not, the failure names what KIND of token was there, never the token — a
// string of the export is the box's words, and its whole value.
func expectDelim(dec *jsontext.Decoder, want jsontext.Kind) error {
	tok, err := dec.ReadToken()
	if err != nil {
		return err
	}
	if got := tok.Kind(); got != want {
		return fmt.Errorf("%w: it holds %s where %q belongs", errNotExport, tokenKind(got), string(rune(want)))
	}
	return nil
}

// tokenKind is what kind of JSON token kind is, in a few words: a delimiter by
// itself, which is one of four characters, and any other token by its kind.
func tokenKind(kind jsontext.Kind) string {
	switch kind {
	case '{', '}', '[', ']':
		return fmt.Sprintf("%q", string(rune(kind)))
	case '"':
		return "a string"
	case '0':
		return "a number"
	case 't', 'f':
		return "a boolean"
	case 'n':
		return "null"
	}
	return "a value"
}

// heldValue bounds what a JSON decoder pulls into its buffer between two
// resets — one value of an export, about — so a message too large to hold is
// refused as it is read rather than once the decoder has held all of it; and
// says whether what it reads came to its end, which tells a document cut short
// from one that holds something else ([transcriptBuilder.openCodeExport]).
type heldValue struct {
	r     io.Reader
	max   int
	n     int
	over  bool
	ended bool
}

// reset starts the count over, for the next value.
func (h *heldValue) reset() { h.n = 0 }

func (h *heldValue) Read(p []byte) (int, error) {
	if h.n >= h.max {
		h.over = true
		return 0, errValuePastBound
	}
	if len(p) > h.max-h.n {
		p = p[:h.max-h.n]
	}
	n, err := h.r.Read(p)
	h.n += n
	if errors.Is(err, io.EOF) {
		h.ended = true
	}
	return n, err
}
