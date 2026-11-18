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
	"strings"
	"unicode/utf8"
)

// THE DIFFERENTIAL'S ORACLE: a line read whole and decoded once
// ([decodeThreadLine]), which the layer's one scanner pass is held to line by
// line (claudelinescan_internal_test.go). Its facts are filled by the layer's
// own rules of each small value and its own automata, so the two readings
// differ in how a line is read, never in what is read of it; its estimate by
// the estimate's own rules (claudelineestimate.go) over each value walked
// whole ([estimateWalk]), every length read off the value as written. A
// member is read by [wholeReader].

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
	return decodeThreadLine(t.n, raw[:len(raw)-1], nil)
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
	Rendered      json.RawMessage `json:"rendered"`
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
// too; asked is the take tokens its reader asks of a hook's words
// ([threadReader.Asking]). Every line is decoded before anything reads it, with
// no keyword prefilter: a line no keyword marks can still be no record (a
// write that tore into another), and a reader that passed it unread would
// decide from the thread around it as though it were whole.
func decodeThreadLine(n int, raw []byte, asked []string) (threadLine, error) {
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
		f: factsOf(&w, head, asked)}, nil
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
// copies its bytes. asked is the take tokens its reader asks of a hook's words
// ([threadReader.Asking]).
func factsOf(w *threadWire, h threadHead, asked []string) *lineFacts {
	f := &lineFacts{
		uuid: w.UUID, version: w.Version, origin: w.Origin, apiError: w.APIError,
		leafUUID: w.LeafUUID, explicit: w.Explicit, rewound: w.Rewound,
		isVirtual: w.IsVirtual, interruptedByShutdown: w.Shutdown, isCompactSummary: w.Summary,
		timestamp: w.Timestamp, isMeta: w.IsMeta, sourceToolAssistantUUID: w.Source,
		taskDelivery: w.TaskDelivery != nil, estimating: true,
	}
	switch h.Type {
	case recordUser:
		f.message = messageOf(w.Message, blockToolResult, w.TaskDelivery)
		f.result = toolResultOf(w.ToolUseResult)
		f.retracted, f.retractedErr = retractedOf(w.Retracted)
	case recordAssistant:
		f.message = messageOf(w.Message, blockToolUse, w.TaskDelivery)
		f.retracted, f.retractedErr = retractedOf(w.Retracted)
		f.batch = batchOf(w.Batch)
	case recordAttachment:
		f.attachment = attachmentOf(w.Attachment, w.Rendered, asked)
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
// call are its calls ([messageContent]), and Claude Code's estimate of the
// record, its content walked whole within [estimateNestMax], counts the
// images its `taskDelivery`, delivery, carries ([messageCharge]).
func messageOf(raw json.RawMessage, call string, delivery json.RawMessage) messageFacts {
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
	if m.contentErr != nil {
		return m
	}
	if m.charge, m.estimateErr = estimateContent(wire.Content, estimateNestMax); m.estimateErr == nil {
		m.emptyText = string(wire.Content) == `""`
		m.estimate, m.estimateErr = messageCharge(m.charge, m.emptyText, taskImages(delivery))
	}
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
// attachment's type ([threadLine.AttachmentWellFormed]), Claude Code's
// estimate of the record by its own `rendered` ([attachmentCharge]), and
// which of the asked take tokens a hook record's words name ([takesOf]).
func attachmentOf(raw, rendered json.RawMessage, asked []string) attachmentFacts {
	a := attachmentFacts{kind: kindOfValue(raw)}
	if a.kind != '{' {
		return a
	}
	var wire struct {
		Type      json.RawMessage `json:"type"`
		ToolUseID json.RawMessage `json:"toolUseID"`
		By        json.RawMessage `json:"by"`
		HookEvent json.RawMessage `json:"hookEvent"`
		Skills    json.RawMessage `json:"skills"`
		Content   json.RawMessage `json:"content"`
		Stdout    json.RawMessage `json:"stdout"`
		Names     json.RawMessage `json:"names"`
		Entries   json.RawMessage `json:"entries"`
		// A PDF read's reads and a queued command's delivery, which its
		// estimate reads.
		Reads        json.RawMessage `json:"reads"`
		TaskDelivery json.RawMessage `json:"taskDelivery"`
	}
	if a.err = wireDecode(raw, &wire); a.err != nil {
		return a
	}
	a.typ, a.toolUseID, a.by, a.event = wire.Type, wire.ToolUseID, wire.By, wire.HookEvent
	if len(asked) > 0 {
		if typ, err := wireRequiredString("type", wire.Type); err == nil && strings.HasPrefix(typ, hookRecordPrefix) {
			a.contentTokens = takesOf(wire.Content, asked)
			if typ == hookSuccess {
				a.stdoutTokens = takesOf(wire.Stdout, asked)
			}
		}
	}
	a.content = kindOfValue(wire.Content)
	a.contentStrings = everyElement(wire.Content, func(raw json.RawMessage) bool { return raw[0] == '"' })
	a.contentObjects = everyElement(wire.Content, wireObject)
	a.skillsShaped = everyElement(wire.Skills, func(raw json.RawMessage) bool { return raw[0] == '{' || raw[0] == '[' })
	a.namesKept, a.namesErr = skillNamesKept(wire.Names)
	a.entriesKept = everyElement(wire.Entries, withheldEntryKept)
	a.pdf, a.images = pdfPagesOf(wire.Reads), taskImages(wire.TaskDelivery)
	a.estimate, a.estimated = attachmentCharge(wire.Type, a.pdf, a.images, renderedEstimate(rendered))
	return a
}

// takesOf is which of asked, take tokens, raw names where it is a string,
// decoded once as the layer reads one and read as it will stream
// ([textTakes]): bit i for asked[i]; none for every other value.
func takesOf(raw json.RawMessage, asked []string) uint64 {
	var text string
	if len(raw) == 0 || raw[0] != '"' || wireDecode(raw, &text) != nil {
		return 0
	}
	takes := textTakes{asked: asked}
	cut := textCut{text: text}
	for p := cut.next(); p != nil; p = cut.next() {
		takes.write(p)
	}
	return takes.bits
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

// quotedUnits is how many UTF-16 code units JavaScript counts raw, one JSON
// string as written with its quotes, as long once it is decoded: an escape one
// unit (a character past the basic plane is written as two `\u` escapes, its
// two units), a character past the basic plane written as itself two, every
// other character one, and each byte that is no UTF-8 one, as the line's
// decode reads each as U+FFFD; JavaScript's decoder reads a run of such bytes
// as fewer characters, so the count is never under the harness's. It is
// [textUnits] over the decoded text with no cap, read where the string lies,
// so a text of any length costs no copy.
func quotedUnits(raw []byte) int64 {
	if len(raw) < 2 {
		return 0
	}
	var n int64
	for p := raw[1 : len(raw)-1]; len(p) > 0; {
		switch {
		case p[0] == '\\':
			// The line decoded whole, so every escape is whole: `\uXXXX`
			// six bytes, every other two.
			if len(p) > 1 && p[1] == 'u' {
				p = p[min(6, len(p)):]
			} else {
				p = p[min(2, len(p)):]
			}
			n++
		case p[0] < utf8.RuneSelf:
			p = p[1:]
			n++
		default:
			r, size := utf8.DecodeRune(p)
			p = p[size:]
			n++
			if r > 0xffff {
				n++
			}
		}
	}
	return n
}

// writtenUnits is how many UTF-16 code units the text p, JSON as written, is
// long: what the harness counts a value at where it charges the value's
// JSON.stringify, which writes the value as the line holds it.
func writtenUnits(p []byte) int64 {
	var n int64
	for len(p) > 0 {
		if p[0] < utf8.RuneSelf {
			p = p[1:]
			n++
			continue
		}
		r, size := utf8.DecodeRune(p)
		p = p[size:]
		n++
		if r > 0xffff {
			n++
		}
	}
	return n
}

// estimateWalk walks one JSON value once, as its decoder reads it, charging
// each element as it passes ([estimateWalk.element]). The length of what an
// element writes is read off raw, the value walked, from where the last
// length read ended (at) to where the element ends, so every byte of the value
// is counted once however deeply its elements nest: units is the length of
// raw[:at].
// Objects past nestMax element lists, where it is above zero, are passed over
// as the nesting fault, as the line's pass reads them.
type estimateWalk struct {
	dec     *jsontext.Decoder
	raw     []byte
	at      int
	units   int64
	nestMax int
}

// estimateContent is a message's content, raw, as the harness estimates it
// (EX), in one walk: a string at its length, a list element by element,
// anything else nothing, and nothing where raw is absent, the harness's
// `!e`.
func estimateContent(raw json.RawMessage, nestMax int) (estimateCharge, error) {
	if len(raw) == 0 {
		return estimateCharge{}, nil
	}
	w := &estimateWalk{dec: jsontext.NewDecoder(bytes.NewBuffer(raw), threadDecoding), raw: raw, nestMax: nestMax}
	l, err := w.list(1)
	return l.all, err
}

// unitsTo is the length of raw[:off], read on from where the last read ended:
// off is never before it, as the decoder reads the value in order.
func (w *estimateWalk) unitsTo(off int) int64 {
	w.units += writtenUnits(w.raw[w.at:off])
	w.at = off
	return w.units
}

// offset is where the decoder has read to.
func (w *estimateWalk) offset() int { return int(w.dec.InputOffset()) }

// list reads the next value as a list of elements at level ([estimateList]).
func (w *estimateWalk) list(level int) (estimateList, error) {
	l := estimateList{kind: valueKind(w.dec.PeekKind())}
	switch l.kind {
	case '"':
		v, err := w.dec.ReadValue()
		l.all.est = estimateUnits(quotedUnits(v))
		return l, err
	case '[':
	default:
		return l, w.dec.SkipValue()
	}
	if _, err := w.dec.ReadToken(); err != nil {
		return l, err
	}
	for w.dec.PeekKind() != ']' {
		object := w.dec.PeekKind() == '{'
		c, err := w.element(level)
		if err != nil {
			return l, err
		}
		l.add(object, c)
	}
	_, err := w.dec.ReadToken()
	return l, err
}

// element reads the next value as one element of a content list (n7e): a
// string at its length, an object by its type ([estimateObject]), anything
// else nothing.
func (w *estimateWalk) element(level int) (estimateCharge, error) {
	switch w.dec.PeekKind() {
	case '"':
		v, err := w.dec.ReadValue()
		return estimateCharge{est: estimateUnits(quotedUnits(v))}, err
	case '{':
		if w.nestMax > 0 && level > w.nestMax {
			return estimateCharge{fault: estimateFault{what: estimateFaultNested}}, w.dec.SkipValue()
		}
		return w.object(level)
	}
	return estimateCharge{}, w.dec.SkipValue()
}

// text reads the next value as a text.
func (w *estimateWalk) text() (estimateText, error) {
	v, err := w.dec.ReadValue()
	if err != nil || kindOfValue(v) != '"' {
		return estimateText{kind: kindOfValue(v)}, err
	}
	return estimateText{kind: '"', units: quotedUnits(v)}, nil
}

// object reads the next value, an object, as one element, and charges it by
// its type ([estimateObject.charge]), handed the length it writes.
func (w *estimateWalk) object(level int) (estimateCharge, error) {
	if _, err := w.dec.ReadToken(); err != nil {
		return estimateCharge{}, err
	}
	start := w.unitsTo(w.offset() - 1)
	var o estimateObject
	for w.dec.PeekKind() != '}' {
		name, err := w.dec.ReadValue()
		if err != nil {
			return estimateCharge{}, err
		}
		if err = w.key(&o, name, level); err != nil {
			return estimateCharge{}, err
		}
	}
	if _, err := w.dec.ReadToken(); err != nil {
		return estimateCharge{}, err
	}
	return o.charge(w.unitsTo(w.offset()) - start), nil
}

// key reads the value of the key name, as written, into o, where a rule reads
// it; every other key's value is passed over. name is compared before the
// value is read, since the decoder hands it out only until its next read.
func (w *estimateWalk) key(o *estimateObject, name []byte, level int) error {
	var (
		v   jsontext.Value
		err error
	)
	switch {
	case quotedIs(name, "type"):
		if v, err = w.dec.ReadValue(); err == nil {
			o.typ = estimateTypeOf(v, estimateElementTypes[:])
		}
	case quotedIs(name, "text"):
		o.text, err = w.text()
	case quotedIs(name, "thinking"):
		o.thinking, err = w.text()
	case quotedIs(name, "data"):
		o.data, err = w.text()
	case quotedIs(name, "name"):
		o.name, err = w.text()
	case quotedIs(name, "title"):
		if v, err = w.dec.ReadValue(); err == nil {
			o.titleKind, o.titleJSON = kindOfValue(v), jsonTitled(v)
		}
	case quotedIs(name, "input"):
		if v, err = w.dec.ReadValue(); err == nil {
			end := w.offset()
			from := w.unitsTo(end - len(v))
			o.input, o.inputSet = w.unitsTo(end)-from, kindOfValue(v) != 'n'
		}
	case quotedIs(name, "source"):
		o.sourceKind, o.source, err = w.source(level)
	case quotedIs(name, "content"):
		o.content, err = w.list(level + 1)
	default:
		err = w.dec.SkipValue()
	}
	return err
}

// source reads the next value as a document's source: its kind, and where it
// is an object its type, data and content.
func (w *estimateWalk) source(level int) (valueKind, estimateSource, error) {
	kind := valueKind(w.dec.PeekKind())
	if kind != '{' {
		return kind, estimateSource{}, w.dec.SkipValue()
	}
	var s estimateSource
	if _, err := w.dec.ReadToken(); err != nil {
		return kind, s, err
	}
	for w.dec.PeekKind() != '}' {
		name, err := w.dec.ReadValue()
		if err != nil {
			return kind, s, err
		}
		switch {
		case quotedIs(name, "type"):
			var v jsontext.Value
			if v, err = w.dec.ReadValue(); err == nil {
				s.typ = estimateTypeOf(v, estimateSourceTypes[:])
			}
		case quotedIs(name, "data"):
			s.data, err = w.text()
		case quotedIs(name, "content"):
			s.content, err = w.list(level + 1)
		default:
			err = w.dec.SkipValue()
		}
		if err != nil {
			return kind, s, err
		}
	}
	_, err := w.dec.ReadToken()
	return kind, s, err
}

// jsonTitled reports whether raw, a document's title as written, is a string
// whose last part after a dot, lower-cased, is `json`, `jsonl` or `jsonc`
// (`e.title?.split(".").pop()?.toLowerCase()`): the harness counts such a
// document's text at two characters a token. A title of no dot is its own
// last part. Read where the title lies, character by character, holding at
// most the five of its last part a match needs: no character past ASCII
// lower-cases to one of those letters.
func jsonTitled(raw []byte) bool {
	if len(raw) < 2 || raw[0] != '"' {
		return false
	}
	var (
		part [len("jsonl")]byte
		n    int
		long bool
	)
	for p := raw[1 : len(raw)-1]; len(p) > 0; {
		r, size := rune(p[0]), 1
		switch {
		case p[0] == '\\':
			r, size = quotedEscape(p)
		case p[0] >= utf8.RuneSelf:
			r, size = utf8.DecodeRune(p)
		}
		p = p[size:]
		switch {
		case r == '.':
			n, long = 0, false
		case long:
		case n == len(part) || r >= utf8.RuneSelf:
			long = true
		default:
			if r >= 'A' && r <= 'Z' {
				r += 'a' - 'A'
			}
			part[n] = byte(r)
			n++
		}
	}
	if long {
		return false
	}
	switch string(part[:n]) {
	case "json", "jsonl", "jsonc":
		return true
	}
	return false
}

// quotedEscape is the character the escape p opens stands for, and how many
// bytes it takes: a `\u` escape its code unit (a surrogate standing alone,
// which is no ASCII character either way), every other its character.
func quotedEscape(p []byte) (rune, int) {
	if len(p) < 2 {
		return utf8.RuneError, len(p)
	}
	if p[1] != 'u' || len(p) < 6 {
		switch p[1] {
		case 'b':
			return '\b', 2
		case 'f':
			return '\f', 2
		case 'n':
			return '\n', 2
		case 'r':
			return '\r', 2
		case 't':
			return '\t', 2
		}
		return rune(p[1]), 2
	}
	var r rune
	for _, h := range p[2:6] {
		switch {
		case h >= '0' && h <= '9':
			r = r<<4 | rune(h-'0')
		case h >= 'a' && h <= 'f':
			r = r<<4 | rune(h-'a'+10)
		case h >= 'A' && h <= 'F':
			r = r<<4 | rune(h-'A'+10)
		}
	}
	return r, 6
}

// taskImages is how many images a task delivery, raw, carries (S
// @292454728): the elements of its MCP task snapshot's result's content,
// where it carries a snapshot, else of its own result's, that are objects of
// type image; none where any of those is no object, or the content no list.
func taskImages(delivery json.RawMessage) int64 {
	var d struct {
		Snapshot json.RawMessage `json:"mcpTaskSnapshot"`
		Result   json.RawMessage `json:"result"`
	}
	if !wireObject(delivery) || wireDecode(delivery, &d) != nil {
		return 0
	}
	result := d.Result
	if wireObject(d.Snapshot) {
		var snapshot struct {
			Result json.RawMessage `json:"result"`
		}
		if wireDecode(d.Snapshot, &snapshot) != nil {
			return 0
		}
		result = snapshot.Result
	}
	var r struct {
		Content json.RawMessage `json:"content"`
	}
	if !wireObject(result) || wireDecode(result, &r) != nil || kindOfValue(r.Content) != '[' {
		return 0
	}
	var images int64
	_ = eachElement(r.Content, func(element json.RawMessage, _ int) error {
		var e struct {
			Type json.RawMessage `json:"type"`
		}
		if wireObject(element) && wireDecode(element, &e) == nil && quotedIs(e.Type, estimateImage) {
			images++
		}
		return nil
	})
	return images
}

// errNotRendered is a stored rendering that does not hold to the shape the
// harness reads one by ([renderedEstimate]), and a PDF read whose pages do
// not read ([pdfPagesOf]).
var errNotRendered = errors.New("no rendering the harness reads")

// renderedEstimate is an attachment record's stored rendering, raw, charged as
// the harness charges it: a list of at least one message, each an object
// whose `content` is a non-empty string or a non-empty list of objects of
// type `text` whose text is a non-empty string, every content at its length,
// each text block on its own; false for any other value, absent among them
// (Cje's schema @298645370, whose objects keep any other key).
func renderedEstimate(rendered json.RawMessage) renderedCharge {
	if kindOfValue(rendered) != '[' {
		return renderedCharge{}
	}
	var (
		sum      threadEstimate
		messages int
	)
	err := eachElement(rendered, func(element json.RawMessage, _ int) error {
		messages++
		var m struct {
			Content json.RawMessage `json:"content"`
		}
		if !wireObject(element) || wireDecode(element, &m) != nil {
			return errNotRendered
		}
		e, ok := renderedContent(m.Content)
		if !ok {
			return errNotRendered
		}
		sum = sum.plus(e)
		return nil
	})
	return renderedCharge{est: sum, ok: err == nil && messages > 0}
}

// renderedContent is one rendered message's content, raw, charged
// ([renderedEstimate]); false where it does not hold to the shape.
func renderedContent(content json.RawMessage) (threadEstimate, bool) {
	switch kindOfValue(content) {
	case '"':
		return estimateUnits(quotedUnits(content)), string(content) != `""`
	case '[':
	default:
		return threadEstimate{}, false
	}
	var (
		sum    threadEstimate
		blocks int
	)
	err := eachElement(content, func(element json.RawMessage, _ int) error {
		blocks++
		var b struct {
			Type json.RawMessage `json:"type"`
			Text json.RawMessage `json:"text"`
		}
		if !wireObject(element) || wireDecode(element, &b) != nil || !quotedIs(b.Type, blockText) ||
			kindOfValue(b.Text) != '"' || string(b.Text) == `""` {
			return errNotRendered
		}
		sum = sum.plus(estimateUnits(quotedUnits(b.Text)))
		return nil
	})
	return sum, err == nil && blocks > 0
}

// pdfPagesOf is a PDF read's `reads`, raw, as its pages ([pdfPages]).
func pdfPagesOf(reads json.RawMessage) pdfPages {
	if kindOfValue(reads) != '[' {
		return pdfPages{}
	}
	var pages int64
	err := eachElement(reads, func(element json.RawMessage, _ int) error {
		var r struct {
			Pages json.RawMessage `json:"pages"`
		}
		if !wireObject(element) || wireDecode(element, &r) != nil || r.Pages == nil {
			return errNotRendered
		}
		n, err := wireCount("pages", r.Pages)
		if err != nil || n < 1 {
			return errNotRendered
		}
		pages = min(pages+n, threadEstimateMax)
		return nil
	})
	return pdfPages{pages: pages, ok: err == nil}
}
