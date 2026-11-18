package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

// THE DIFFERENTIAL'S ORACLE: a line read whole and decoded once
// ([decodeThreadLine]), which the layer's one scanner pass is held to line by
// line (claudelinescan_internal_test.go). Its facts are filled by the layer's
// own rules of each small value and its own automata, so the two readings
// differ in how a line is read, never in what is read of it. A member is read
// by [wholeReader].

// wholeReader reads a member a whole line at a time through the scrub's
// [readLine], each line decoded whole: the member's ends the layer's reader
// is held to ([TestTheReaderEndsAMemberAsTheWholeReaderDoes]). It holds no line
// bound, which the layer has none of, only the member's.
type wholeReader struct {
	r              *bufio.Reader
	maxBytes, left int64
	n              int
	torn           bool
	end            error
}

// newWholeReader reads r, one member, within maxBytes.
func newWholeReader(r io.Reader, maxBytes int64) *wholeReader {
	maxBytes = max(maxBytes, 0)
	return &wholeReader{r: bufio.NewReaderSize(wholeSource{r}, lineChunkBytes), maxBytes: maxBytes, left: maxBytes}
}

// wholeSource tags every error of a member's reader but its end, so one that
// wraps the scrub's bound is never read as the member's bound.
type wholeSource struct{ r io.Reader }

// Read reads the member's reader, tagging every error but io.EOF.
func (s wholeSource) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		err = &wholeSourceError{err: err}
	}
	return n, err
}

// wholeSourceError is an error of a member's reader ([wholeSource]).
type wholeSourceError struct{ err error }

func (e *wholeSourceError) Error() string { return e.err.Error() }

// Next is the member's next whole line, as [threadReader.Next] answers it.
func (t *wholeReader) Next(ctx context.Context) (threadLine, error) {
	if t.end != nil {
		return threadLine{}, t.end
	}
	if err := ctx.Err(); err != nil {
		t.end = err
		return threadLine{}, err
	}
	raw, err := readLine(t.r, int(t.left))
	var source *wholeSourceError
	switch {
	case errors.As(err, &source):
		t.end = source.err
		return threadLine{}, t.end
	case errors.Is(err, errScrubBound):
		t.end = fmt.Errorf("%w: the member runs past the %d bytes it is read within", errThreadBound, t.maxBytes)
		return threadLine{}, t.end
	case errors.Is(err, io.EOF):
		t.torn, t.end = len(raw) > 0, io.EOF
		return threadLine{}, io.EOF
	case err != nil:
		t.end = err
		return threadLine{}, err
	}
	t.left -= int64(len(raw))
	t.n++
	return decodeThreadLine(t.n, raw[:len(raw)-1])
}

// Torn reports whether the member ended in a piece with no break.
func (t *wholeReader) Torn() bool { return t.torn }

// lineBytesOf is the byte facts of raw, a whole line, its break dropped,
// read as its bytes stream past ([lineBytes]).
func lineBytesOf(raw []byte) threadBytes {
	var l lineBytes
	l.write(raw)
	return l.facts()
}

// threadWire is a whole line as [wireDecode] leaves it: the raw of every key
// this layer reads, by its exact name, so an absent key is nil and a null one
// holds `null`. Each is held to its type where it is read: by [headOf],
// [factsOf] or its accessor.
type threadWire struct {
	Type          json.RawMessage `json:"type"`
	Subtype       json.RawMessage `json:"subtype"`
	UUID          json.RawMessage `json:"uuid"`
	ParentUUID    json.RawMessage `json:"parentUuid"`
	IsSidechain   json.RawMessage `json:"isSidechain"`
	AgentID       json.RawMessage `json:"agentId"`
	ReplacesSpan  json.RawMessage `json:"replacesSpan"`
	Message       json.RawMessage `json:"message"`
	Attachment    json.RawMessage `json:"attachment"`
	Origin        json.RawMessage `json:"origin"`
	ToolUseResult json.RawMessage `json:"toolUseResult"`
	APIError      json.RawMessage `json:"isApiErrorMessage"`
	LeafUUID      json.RawMessage `json:"leafUuid"`
	Explicit      json.RawMessage `json:"explicit"`
	Rewound       json.RawMessage `json:"rewound"`
	Compacted     json.RawMessage `json:"compactMetadata"`
	Retracted     json.RawMessage `json:"retractedMessageUuids"`
	Replacements  json.RawMessage `json:"replacements"`
	IsVirtual     json.RawMessage `json:"isVirtual"`
	Shutdown      json.RawMessage `json:"interruptedByShutdown"`
	Summary       json.RawMessage `json:"isCompactSummary"`
	TaskDelivery  json.RawMessage `json:"taskDelivery"`
	Batch         json.RawMessage `json:"batchToolUses"`
	Timestamp     json.RawMessage `json:"timestamp"`
	IsMeta        json.RawMessage `json:"isMeta"`
	Source        json.RawMessage `json:"sourceToolAssistantUUID"`
	Version       json.RawMessage `json:"version"`
}

// decodeThreadLine decodes whole line n, raw (its break dropped), into its head
// and its facts ([factsOf]) by ONE decode that holds every byte to JSON
// ([wireDecode]), and reads its byte facts ([lineBytesOf]), a refused line's
// too. Every line is decoded before anything reads it, with no keyword
// prefilter: a line no keyword marks can still be no record (a write that tore
// into another), and a reader that passed it unread would decide from the
// thread around it as though it were whole.
func decodeThreadLine(n int, raw []byte) (threadLine, error) {
	// HELD TO AN OBJECT FIRST: a JSON null decodes into a struct as nothing,
	// with no error, and would pass for a record of no keys.
	if trimmed := bytes.TrimLeft(raw, " \t\r\n"); len(trimmed) == 0 || trimmed[0] != '{' {
		return threadLine{}, &threadLineError{N: n, Err: errNotOneRecord, Fault: refusedFault(raw), Size: len(raw) + 1,
			Bytes: lineBytesOf(raw)}
	}
	var w threadWire
	if err := wireDecode(raw, &w); err != nil {
		return threadLine{}, &threadLineError{N: n, Err: err, Fault: refusedFault(raw), Size: len(raw) + 1,
			Bytes: lineBytesOf(raw)}
	}
	head, err := w.head()
	if err != nil {
		return threadLine{}, &threadLineError{N: n, Err: err, Fault: faultHead, Type: kindOf(w.Type),
			Size: len(raw) + 1, Bytes: lineBytesOf(raw)}
	}
	return threadLine{N: n, Size: len(raw) + 1, Bytes: lineBytesOf(raw), Head: head,
		f: factsOf(&w, head)}, nil
}

// head is the line's head, its keys held to their types by [headOf].
func (w threadWire) head() (threadHead, error) {
	return headOf(headKeys{typ: headKeyOf(w.Type), subtype: headKeyOf(w.Subtype), agentID: headKeyOf(w.AgentID),
		parent: headKeyOf(w.ParentUUID), sidechain: headKeyOf(w.IsSidechain),
		replacesSpan: headKeyOf(w.ReplacesSpan)})
}

// factsOf is the facts ([lineFacts]) of w, a line's whole decode with head h:
// its small values as written, and each frame its record's type reads. A frame
// the type does not read is left empty, so a record never pays for reading one
// it carries (an api-request-blob's whole request), though [wireDecode] still
// copies its bytes.
func factsOf(w *threadWire, h threadHead) *lineFacts {
	f := &lineFacts{
		uuid: w.UUID, version: w.Version, origin: w.Origin, apiError: w.APIError,
		leafUUID: w.LeafUUID, explicit: w.Explicit, rewound: w.Rewound,
		isVirtual: w.IsVirtual, interruptedByShutdown: w.Shutdown, isCompactSummary: w.Summary,
		timestamp: w.Timestamp, isMeta: w.IsMeta, sourceToolAssistantUUID: w.Source,
		taskDelivery: w.TaskDelivery != nil,
	}
	switch h.Type {
	case recordUser:
		f.message = messageOf(w.Message, blockToolResult)
		f.result = toolResultOf(w.ToolUseResult)
		f.retracted, f.retractedErr = retractedOf(w.Retracted)
	case recordAssistant:
		f.message = messageOf(w.Message, blockToolUse)
		f.retracted, f.retractedErr = retractedOf(w.Retracted)
		f.batch = batchOf(w.Batch)
	case recordAttachment:
		f.attachment = attachmentOf(w.Attachment)
	case recordSystem:
		switch h.Subtype {
		case subtypeCompactBoundary:
			f.compacted = compactOf(w.Compacted)
		case subtypeRefusalFallback:
			f.retracted, f.retractedErr = retractedOf(w.Retracted)
		}
	case recordContentReplacement:
		f.replacements = replacementsOf(w.Replacements)
	}
	return f
}

// messageOf reads a user or assistant record's `message`, raw, by ONE decode
// of its object that every accessor of the message reads; its blocks of kind
// call are its calls ([messageContent]).
func messageOf(raw json.RawMessage, call string) messageFacts {
	m := messageFacts{kind: kindOfValue(raw)}
	if m.kind != '{' {
		return m
	}
	var wire struct {
		Content           json.RawMessage `json:"content"`
		ID                json.RawMessage `json:"id"`
		Model             json.RawMessage `json:"model"`
		StopReason        json.RawMessage `json:"stop_reason"`
		Usage             json.RawMessage `json:"usage"`
		ContextManagement json.RawMessage `json:"context_management"`
	}
	if m.err = wireDecode(raw, &wire); m.err != nil {
		return m
	}
	m.id, m.model, m.stopReason, m.usage, m.contextManagement = wire.ID, wire.Model, wire.StopReason, wire.Usage,
		wire.ContextManagement
	m.content, m.contentErr = messageContent(wire.Content, call)
	return m
}

// toolResultOf is what a user record's own `toolUseResult`, raw, says
// ([carriesHandback], [agentAnswer]).
func toolResultOf(raw json.RawMessage) toolResultFacts {
	r := toolResultFacts{handsBack: carriesHandback(raw)}
	r.completed, r.agent, r.refused = agentAnswer(raw)
	return r
}

// attachmentOf reads an attachment record's `attachment`, raw, once, answering
// every attachment type's shape rule at once, so the accessor picks by the
// attachment's type ([threadLine.AttachmentWellFormed]).
func attachmentOf(raw json.RawMessage) attachmentFacts {
	a := attachmentFacts{kind: kindOfValue(raw)}
	if a.kind != '{' {
		return a
	}
	var wire struct {
		Type      json.RawMessage `json:"type"`
		ToolUseID json.RawMessage `json:"toolUseID"`
		By        json.RawMessage `json:"by"`
		Skills    json.RawMessage `json:"skills"`
		Content   json.RawMessage `json:"content"`
		Names     json.RawMessage `json:"names"`
		Entries   json.RawMessage `json:"entries"`
	}
	if a.err = wireDecode(raw, &wire); a.err != nil {
		return a
	}
	a.typ, a.toolUseID, a.by = wire.Type, wire.ToolUseID, wire.By
	a.content = kindOfValue(wire.Content)
	a.contentStrings = everyElement(wire.Content, func(raw json.RawMessage) bool { return raw[0] == '"' })
	a.contentObjects = everyElement(wire.Content, wireObject)
	a.skillsShaped = everyElement(wire.Skills, func(raw json.RawMessage) bool { return raw[0] == '{' || raw[0] == '[' })
	a.namesKept, a.namesErr = skillNamesKept(wire.Names)
	a.entriesKept = everyElement(wire.Entries, withheldEntryKept)
	return a
}

// compactOf reads a compaction boundary's `compactMetadata`, raw, once
// ([threadLine.Preserved]); the list's answer ([preservedList]) only where
// `preservedMessages` is present and not null.
func compactOf(raw json.RawMessage) compactFacts {
	c := compactFacts{kind: kindOfValue(raw)}
	if c.kind != '{' {
		return c
	}
	var meta struct {
		Messages json.RawMessage `json:"preservedMessages"`
		Segment  json.RawMessage `json:"preservedSegment"`
	}
	if c.err = wireDecode(raw, &meta); c.err != nil {
		return c
	}
	c.messages, c.segment = kindOfValue(meta.Messages), meta.Segment
	if c.messages != 0 && c.messages != 'n' {
		c.list, c.listErr = preservedList(meta.Messages)
	}
	return c
}

// retractedOf reads a record's `retractedMessageUuids`, raw, once
// ([threadLine.Retracted]): nothing where absent; else each entry by
// [threadRetracted.entry], in order, up to the first that is no string, with
// that entry's error, or the list's own.
func retractedOf(raw json.RawMessage) (threadRetracted, error) {
	var r threadRetracted
	if raw == nil {
		return r, nil
	}
	err := eachElement(raw, func(element json.RawMessage, _ int) error {
		entry, err := wireRequiredString("an entry", element)
		if err != nil {
			return err
		}
		r.entry(chainPrefix(entry))
		return nil
	})
	if err != nil {
		return r, fmt.Errorf("retractedMessageUuids: %w", err)
	}
	return r, nil
}

// replacementsOf reads a content-replacement record's `replacements`, raw,
// once: of a list, every element counted, and each entry up to the first its
// rule ([replacedID]) refuses read, with that entry's error.
func replacementsOf(raw json.RawMessage) replacementFacts {
	r := replacementFacts{kind: kindOfValue(raw)}
	if r.kind != '[' {
		return r
	}
	// The walk goes on past a refused entry: the count is of the whole list.
	walked := eachElement(raw, func(element json.RawMessage, _ int) error {
		r.count++
		if r.err == nil {
			id, replaced, err := replacementEntry(element)
			switch {
			case err != nil:
				r.err = err
			case replaced:
				r.ids = append(r.ids, id)
			}
		}
		return nil
	})
	// A list the line's decode read walks clean, but an error met is the list's.
	if walked != nil && r.err == nil {
		r.err = walked
	}
	return r
}

// replacementEntry is element, one raw entry of a content-replacement record's
// list, read by [replacedID].
func replacementEntry(element json.RawMessage) (id string, replaced bool, err error) {
	var entry struct {
		Kind      json.RawMessage `json:"kind"`
		ToolUseID json.RawMessage `json:"toolUseId"`
	}
	if wireObject(element) {
		if err = wireDecode(element, &entry); err != nil {
			return "", false, fmt.Errorf("a replacement: %w", err)
		}
	}
	return replacedID(kindOfValue(element), entry.Kind, entry.ToolUseID)
}

// batchOf reads an assistant record's `batchToolUses`, raw, once
// ([threadLine.BatchToolUses]): whether present, and each entry's id up to the
// first [batchEntryID] refuses, with that entry's error, or the list's own.
func batchOf(raw json.RawMessage) batchFacts {
	b := batchFacts{present: raw != nil}
	if !b.present {
		return b
	}
	b.err = eachElement(raw, func(element json.RawMessage, _ int) error {
		var entry struct {
			ID json.RawMessage `json:"id"`
		}
		if wireObject(element) {
			if err := wireDecode(element, &entry); err != nil {
				return fmt.Errorf("an entry: %w", err)
			}
		}
		id, err := batchEntryID(kindOfValue(element), entry.ID)
		if err != nil {
			return err
		}
		b.ids = append(b.ids, id)
		return nil
	})
	return b
}

// refusedFault is how Claude Code's load reads raw, a whole line its decode
// refused before reading its head ([lineFault]), by ONE jsontext.Decoder under
// the layer's options, in order: a leading NUL or byte order mark; a value cut
// at the line's end; one nested past the decoder's bound; any other failure
// before a whole value; a whole value followed by more; a whole value that is
// no object.
//
// Only [decodeThreadLine]'s refusals call it, so a line taken is decoded once.
// The decoder reads a bytes.Buffer, which jsontext reads with no copy, so a
// refused line costs its decoder's state, never a copy of the line.
func refusedFault(raw []byte) lineFault {
	if len(raw) > 0 && raw[0] == 0 || bytes.HasPrefix(raw, utf8ByteOrderMark) {
		return faultLead
	}
	dec := jsontext.NewDecoder(bytes.NewBuffer(raw), threadDecoding)
	value, err := dec.ReadValue()
	var syntax *jsontext.SyntacticError
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF):
		return faultPrefix
	case errors.As(err, &syntax) && syntax.Err != nil && syntax.Err.Error() == jsontextMaxDepth:
		return faultDepth
	case err != nil:
		return faultSyntax
	}
	if _, err = dec.ReadToken(); !errors.Is(err, io.EOF) {
		return faultTrailing
	}
	if value.Kind() != '{' {
		return faultNotObject
	}
	// An object the decode refused, which these options never do: no fault,
	// read as a line whose load cannot be said, never as one yielding nothing.
	return ""
}

// utf8ByteOrderMark is the byte order mark UTF-8 writes ([refusedFault]).
var utf8ByteOrderMark = []byte{0xef, 0xbb, 0xbf}

// jsontextMaxDepth is the unexported error text encoding/json/jsontext gives a
// value nested past its bound ([faultDepth]);
// [TestALineThatIsNoRecordSaysHowTheLoadReadsIt] fails where a Go release
// words it otherwise.
const jsontextMaxDepth = "exceeded max depth"

// messageContent is a message whose `content` is content, its blocks of kind
// call its calls.
func messageContent(content json.RawMessage, call string) (threadMessage, error) {
	switch {
	case len(content) == 0 || (content[0] != '"' && content[0] != '['):
		return threadMessage{}, fmt.Errorf("content is %s, neither a string nor a list", wireKind(content))
	case content[0] == '"':
		var text string
		if err := wireDecode(content, &text); err != nil {
			return threadMessage{}, fmt.Errorf("content: %w", err)
		}
		// READ AS IT WILL STREAM, in pieces, until all three are decided.
		var (
			says     textSays
			notice   textNotice
			handback textHandback
		)
		cut := textCut{text: text}
		for p := cut.next(); p != nil && (!says.any || !notice.done || !handback.named); p = cut.next() {
			says.write(p)
			notice.write(p)
			handback.write(p)
		}
		notice.end()
		m := threadMessage{isText: true, saysAny: says.any, namesHandback: handback.named}
		m.notification.agent, m.notification.leg, m.notification.ok = notice.notice()
		return m, nil
	}
	// A BLOCK AT A TIME ([eachElement]), the calls grown as accepted, never
	// sized from the list's length, which only a whole decode knows: a list
	// refused at its first block costs that block. Other blocks are only
	// counted ([threadBlocks]).
	var m threadMessage
	// said is the one buffer every block's text is unquoted into.
	var said []byte
	if err := eachElement(content, func(raw json.RawMessage, _ int) error {
		b, text, err := contentBlock(raw)
		if err != nil {
			return fmt.Errorf("content block %d: %w", m.blocks.Len()+1, err)
		}
		m.blocks.add(b.Kind)
		if b.Kind == call {
			m.calls = append(m.calls, b)
		}
		if !m.saysText && (b.Kind == blockText || !m.saysAny) && len(text) > 0 && text[0] == '"' {
			// The line's decode already read it, so the only errors are a lone
			// surrogate or invalid UTF-8, each written as U+FFFD as it reads them.
			said, _ = jsontext.AppendUnquote(said[:0], text)
			var says textSays
			if says.write(said); says.any {
				m.saysAny = true
				m.saysText = b.Kind == blockText
			}
		}
		return nil
	}); err != nil {
		return threadMessage{}, err
	}
	return m, nil
}

// contentBlock is one raw block of a message's content as [blockCallOf] reads
// it, and its raw `text`, which [threadMessage.Says] reads of every block
// whatever its type, valid only while the block's walk runs.
func contentBlock(raw json.RawMessage) (threadCall, json.RawMessage, error) {
	if !wireObject(raw) {
		c, err := blockCallOf(kindOfValue(raw), blockKeys{})
		return c, nil, err
	}
	var b struct {
		Type      json.RawMessage `json:"type"`
		ID        json.RawMessage `json:"id"`
		Name      json.RawMessage `json:"name"`
		Input     json.RawMessage `json:"input"`
		ToolUseID json.RawMessage `json:"tool_use_id"`
		IsError   json.RawMessage `json:"is_error"`
		Content   json.RawMessage `json:"content"`
		Text      json.RawMessage `json:"text"`
	}
	if err := wireDecode(raw, &b); err != nil {
		return threadCall{}, nil, err
	}
	keys := blockKeys{typ: b.Type, id: b.ID, name: b.Name, toolUseID: b.ToolUseID, isError: b.IsError}
	// A target only from an input, a text only from a content; [blockCallOf]
	// keeps a tool_use's target and a tool_result's text alone.
	if b.Input != nil {
		keys.to = sendTarget(b.Input)
	}
	if b.Content != nil {
		keys.text = resultTextOf(b.Content)
	}
	c, err := blockCallOf('{', keys)
	if err != nil {
		return threadCall{}, nil, err
	}
	return c, b.Text, nil
}

// sendTarget is the subagent a SendMessage call's input, raw, sends to, by
// [sendTargetOf].
func sendTarget(input json.RawMessage) string {
	if !wireObject(input) {
		return ""
	}
	var wire struct {
		To json.RawMessage `json:"to"`
	}
	if wireDecode(input, &wire) != nil {
		return ""
	}
	return sendTargetOf('{', wire.To)
}

// resultTextOf is a tool_result's content, raw, as a [threadResultText]: its
// text ([resultText]) streamed through [textResult]; the zero value where the
// content holds none.
func resultTextOf(content json.RawMessage) threadResultText {
	text, ok := resultText(content)
	if !ok {
		return threadResultText{}
	}
	var read textResult
	cut := textCut{text: text}
	for p := cut.next(); p != nil; p = cut.next() {
		read.write(p)
	}
	return read.sum()
}

// errNotOneText is a result's content that is neither a string nor one text
// block ([resultText]).
var errNotOneText = errors.New("not one text block")

// resultText is the text a tool_result's content, raw, holds: a JSON string,
// or a list of exactly one object whose `type` is "text" and whose `text` is a
// string, other keys passed over; false for any other content, absent and null
// included. The list is read no further than its second element
// ([eachElement]), so a long one costs what its first two do.
func resultText(content json.RawMessage) (string, bool) {
	if len(content) > 0 && content[0] == '"' {
		var text string
		if err := wireDecode(content, &text); err != nil {
			return "", false
		}
		return text, true
	}
	text, blocks := "", 0
	err := eachElement(content, func(raw json.RawMessage, _ int) error {
		blocks++
		if blocks > 1 || !wireObject(raw) {
			return errNotOneText
		}
		var block struct {
			Type json.RawMessage `json:"type"`
			Text json.RawMessage `json:"text"`
		}
		if err := wireDecode(raw, &block); err != nil {
			return err
		}
		if !resultElementIsText('{', block.Type, kindOfValue(block.Text)) {
			return errNotOneText
		}
		// [resultElementIsText] held it to a string: no check of its own here.
		return wireDecode(block.Text, &text)
	})
	return text, err == nil && blocks == 1
}

// agentAnswer is what a leg's result record's own toolUseResult, raw, says of
// the leg, by [answerOf]. A result of another shape says nothing: no answer,
// and no refusal.
func agentAnswer(raw json.RawMessage) (completed bool, agent string, refused bool) {
	if !wireObject(raw) {
		return false, "", false
	}
	var wire struct {
		Status  json.RawMessage `json:"status"`
		AgentID json.RawMessage `json:"agentId"`
		Success json.RawMessage `json:"success"`
	}
	if wireDecode(raw, &wire) != nil {
		return false, "", false
	}
	return answerOf(wire.Status, wire.AgentID, wire.Success)
}

// carriesHandback reports whether a leg's result record's own toolUseResult,
// raw, carries a handback: a `handback` or a `handbackReport` key, any value.
//
// A HANDBACK ANSWERS NO LEG. Under a handback contract (both releases arm it on
// a subagent behind a flag on by default: 2.1.322's BFt @288117159, 2.1.310's
// p_t @245867640, armed @302126445) the subagent delivers its report by a call
// of its own ([claudeToolHandback]), and its completion swaps its last reply's
// text for the harness's own words (2.1.322's pkn @302170267,
// `if(r!==void 0){Z=[];...}`, 2.1.310's Owr @255311767). Its answer carries
// the `handback` key beside them (`...ee!==void 0&&{handback:ee}`,
// @302347284), so no answer of the leg is the subagent's report.
func carriesHandback(raw json.RawMessage) bool {
	if !wireObject(raw) {
		return false
	}
	var wire struct {
		Handback       json.RawMessage `json:"handback"`
		HandbackReport json.RawMessage `json:"handbackReport"`
	}
	// ONE THAT DOES NOT DECODE IS READ AS CARRYING ONE, the safe direction.
	if wireDecode(raw, &wire) != nil {
		return true
	}
	return wire.Handback != nil || wire.HandbackReport != nil
}

// errNotEvery stops a walk of a list at its first element a test refuses
// ([everyElement]).
var errNotEvery = errors.New("an element fails its test")

// everyElement reports whether raw is a list whose every element passes keep,
// as JavaScript's Array.isArray and every() read one: a list of none passes.
// It walks an element at a time ([eachElement]), stopping at the first that
// fails.
func everyElement(raw json.RawMessage, keep func(element json.RawMessage) bool) bool {
	if len(raw) == 0 || raw[0] != '[' {
		return false
	}
	return eachElement(raw, func(element json.RawMessage, _ int) error {
		if !keep(element) {
			return errNotEvery
		}
		return nil
	}) == nil
}

// withheldEntryKept reports whether entry, one raw entry of a withheld_memory
// attachment, has the shape Claude Code's resume keeps the attachment with
// ([withheldKept]).
func withheldEntryKept(entry json.RawMessage) bool {
	var e struct {
		Path        json.RawMessage `json:"path"`
		Why         json.RawMessage `json:"why"`
		DisplayPath json.RawMessage `json:"displayPath"`
	}
	if wireObject(entry) && wireDecode(entry, &e) != nil {
		return false
	}
	return withheldKept(kindOfValue(entry), kindOfValue(e.Path), e.Why, kindOfValue(e.DisplayPath))
}

// skillNamesKept reports whether names, a skill_listing attachment's raw
// `names`, has the shape Claude Code's resume keeps the attachment with:
// absent, or a list of at most [skillListingMaxNames] strings, each at most
// [skillListingMaxNameUnits] UTF-16 code units, its length checked before its
// names as the harness does. An error where the answer turns on a name the
// layer cannot count as the harness does ([threadLine.AttachmentWellFormed]).
func skillNamesKept(names json.RawMessage) (bool, error) {
	if names == nil {
		return true, nil
	}
	if names[0] != '[' {
		return false, nil
	}
	var (
		count      int
		unreadable bool
		unquoted   []byte
	)
	kept := eachElement(names, func(name json.RawMessage, _ int) error {
		count++
		if count > skillListingMaxNames || name[0] != '"' {
			return errNotEvery
		}
		// The line's decode already read it, so the only errors are a lone
		// surrogate or invalid UTF-8, each written as U+FFFD as it reads them.
		unquoted, _ = jsontext.AppendUnquote(unquoted[:0], name)
		var units textUnits
		units.write(unquoted)
		kept, unknown := skillNameKept(units.units(), utf8.Valid(name))
		if !kept {
			return errNotEvery
		}
		unreadable = unreadable || unknown
		return nil
	}) == nil
	if !kept {
		return false, nil
	}
	if unreadable {
		return false, errSkillNameUnmeasured
	}
	return true, nil
}

// preservedList is what a compaction's `preservedMessages`, raw, says it kept
// ([threadLine.Preserved]): named whatever it lists, and kept only where it
// lists any, since a resume relinks no record by an empty list yet still drops
// every record before the last boundary.
func preservedList(raw json.RawMessage) (threadPreserved, error) {
	if !wireObject(raw) {
		return threadPreserved{}, fmt.Errorf("compactMetadata.preservedMessages is %s, not an object", wireKind(raw))
	}
	var list struct {
		Anchor json.RawMessage `json:"anchorUuid"`
		UUIDs  json.RawMessage `json:"uuids"`
	}
	if err := wireDecode(raw, &list); err != nil {
		return threadPreserved{}, fmt.Errorf("compactMetadata.preservedMessages: %w", err)
	}
	anchor, err := wireRequiredString("preservedMessages.anchorUuid", list.Anchor)
	if err != nil {
		return threadPreserved{}, err
	}
	var p threadPreserved
	err = eachElement(list.UUIDs, func(element json.RawMessage, _ int) error {
		entry, entryErr := wireRequiredString("an entry", element)
		if entryErr != nil {
			return entryErr
		}
		p.list(uuidBytes(entry))
		return nil
	})
	if err != nil {
		return p, fmt.Errorf("compactMetadata.preservedMessages.uuids: %w", err)
	}
	p.Named, p.Kept, p.Anchor = true, p.Listed > 0, anchor
	return p, nil
}
