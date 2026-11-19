package executor

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"io"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools/toolargs"
)

// THE HOLDINGS READING'S ORACLE: its half of the decode walk
// ([walkOpenCodeExport]) as it stood before the reading became a visitor of
// the one export walk ([openCodeHoldingsWalk]), kept so its differential
// holds every answer to it. It shares the reading's state, its pool and its
// verdict, which the move left as they were.

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
