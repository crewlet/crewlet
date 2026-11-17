package executor

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

// A THREAD LINE'S FACTS, READ OFF ITS WHOLE DECODE.
//
// The producer of a line's facts ([lineFacts]) that reads the line whole: ONE
// decode of the line into the raw of every key the layer reads
// ([threadWire], [decodeThreadLine]), the head held to its types ([headOf]),
// and then each frame its record's type reads, walked by the layer's own
// walks ([eachElement]) and read by each reader's rule ([factsOf]). Every
// rule it reads a small value by is the layer's one rule of that value
// (a block's [blockCallOf], a send's [sendTargetOf], a leg's answer's
// [answerOf], a list's entries' [replacedID], [batchEntryID],
// [threadRetracted.entry] and [threadPreserved.list]...), and every text it
// reads a property of is read by the automaton that reads it as it streams
// ([textSays], [textNotice], [textHandback], [textResult], [textUnits],
// [textTakes]), so what it fills is any reading's of the line by those rules.
//
// A line it refuses is classed by how Claude Code's load reads it
// ([refusedFault]), which reads the line again, and only a refused line.

// threadWire is a whole line as its one decode ([wireDecode]) leaves it: every
// key this layer reads, by its exact name and raw, so a key that is absent is
// nil and one that is null holds the bytes `null`. Each is held to its type
// where it is read: the head's by [headOf], every other by the rule its
// facts are read by ([factsOf]) or by its accessor.
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

// decodeThreadLine decodes whole line n, raw (its break dropped), into its
// head and its facts: ONE decode of the whole line ([wireDecode]), which
// holds every byte of it to JSON, then the head's keys held to their types,
// then the facts its accessors read ([factsOf]); and reads its byte facts
// ([lineBytesOf]), a refused line's too; asked is the take tokens its reader
// asks of a hook's words ([threadReader.Asking]). Nothing reads a
// line before this has: no keyword decides which lines are decoded, since a
// line no keyword marks can still be no record (a write that tore into
// another), and a reader that passed it unread would decide from the thread
// around it as though it were whole.
func decodeThreadLine(n int, raw []byte, asked []string) (threadLine, error) {
	// HELD TO AN OBJECT BEFORE IT IS DECODED: a JSON null decodes into a struct
	// as nothing at all, with no error, and would pass for a record of no keys.
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

// head is the line's head, its keys held to their types by the head's rule
// ([headOf]).
func (w threadWire) head() (threadHead, error) {
	return headOf(headKeys{typ: headKeyOf(w.Type), subtype: headKeyOf(w.Subtype), agentID: headKeyOf(w.AgentID),
		parent: headKeyOf(w.ParentUUID), sidechain: headKeyOf(w.IsSidechain),
		replacesSpan: headKeyOf(w.ReplacesSpan)})
}

// factsOf is what a line's accessors read of w, its whole decode, h its head
// ([lineFacts]): its small values as written, and each frame its record's
// type reads, read by each reader's rule. A frame the type does not read is
// left empty, so a record of another type never pays for reading a frame it
// carries, an api-request-blob's whole request among them, though the line's
// decode ([wireDecode]) still copies that frame's bytes. asked is the take
// tokens its reader asks of a hook's words ([threadReader.Asking]).
func factsOf(w *threadWire, h threadHead, asked []string) *lineFacts {
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
		f.attachment = attachmentOf(w.Attachment, asked)
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

// messageOf is a user or assistant record's `message`, raw, read once: its
// kind, its small values as written, and its content, its blocks of kind
// call its calls ([messageContent]). ONE DECODE of the message's object,
// which every accessor of the message reads.
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

// toolResultOf is what a user record's own `toolUseResult`, raw, says: whether
// it carries a handback ([carriesHandback]), and what it says of a subagent's
// leg ([agentAnswer]).
func toolResultOf(raw json.RawMessage) toolResultFacts {
	r := toolResultFacts{handsBack: carriesHandback(raw)}
	r.completed, r.agent, r.refused = agentAnswer(raw)
	return r
}

// attachmentOf is an attachment record's `attachment`, raw, read once: its
// kind, its small values as written, what each attachment's shape rule asks
// of its lists ([threadLine.AttachmentWellFormed]), every rule's at once, so
// the accessor picks by the attachment's type, and which of the asked take
// tokens its words name ([takesOf]).
func attachmentOf(raw json.RawMessage, asked []string) attachmentFacts {
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
	}
	if a.err = wireDecode(raw, &wire); a.err != nil {
		return a
	}
	a.typ, a.toolUseID, a.by, a.event = wire.Type, wire.ToolUseID, wire.By, wire.HookEvent
	if len(asked) > 0 {
		a.contentTokens = takesOf(wire.Content, asked)
		if typ, err := wireRequiredString("type", wire.Type); err == nil && typ == hookSuccess {
			a.stdoutTokens = takesOf(wire.Stdout, asked)
		}
	}
	a.content = kindOfValue(wire.Content)
	a.contentStrings = everyElement(wire.Content, func(raw json.RawMessage) bool { return raw[0] == '"' })
	a.contentObjects = everyElement(wire.Content, wireObject)
	a.skillsShaped = everyElement(wire.Skills, func(raw json.RawMessage) bool { return raw[0] == '{' || raw[0] == '[' })
	a.namesKept, a.namesErr = skillNamesKept(wire.Names)
	a.entriesKept = everyElement(wire.Entries, withheldEntryKept)
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

// compactOf is a compaction's boundary's `compactMetadata`, raw, read once
// ([threadLine.Preserved]): its kind, its `preservedMessages`' kind and, where
// that is there and not null, the list's answer ([preservedList]), and its
// `preservedSegment` as written.
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

// retractedOf is a record's `retractedMessageUuids`, raw, read once
// ([threadLine.Retracted]): nothing where it is absent; else each entry, a
// string, read by its rule ([threadRetracted.entry]), in order, up to the
// first that is no string, with that entry's error, or the list's own.
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
		r.entry(entry)
		return nil
	})
	if err != nil {
		return r, fmt.Errorf("retractedMessageUuids: %w", err)
	}
	return r, nil
}

// replacementsOf is a content-replacement record's `replacements`, raw, read
// once: its kind and, of a list, every element counted, and each entry before
// the first its rule refuses read by it ([replacedID]), in order, with that
// entry's error ([threadLine.ReplacedResults], [threadLine.ReplacementCount]).
func replacementsOf(raw json.RawMessage) replacementFacts {
	r := replacementFacts{kind: kindOfValue(raw)}
	if r.kind != '[' {
		return r
	}
	// EVERY ELEMENT IS COUNTED, and each read by its rule until one fails it:
	// the count is of the list, whatever its entries hold.
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
	// The walk itself meets no error of a list the line's decode read, but
	// one it met is the list's, never read past.
	if walked != nil && r.err == nil {
		r.err = walked
	}
	return r
}

// replacementEntry is element, one entry of a content-replacement record's
// list, raw, read by the entry's rule ([replacedID]).
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

// batchOf is an assistant record's `batchToolUses`, raw, read once: whether it
// is there, and each entry's id before the first entry its rule refuses
// ([batchEntryID]), in order, with that entry's error, or the list's own
// ([threadLine.BatchToolUses]).
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
// refused before it read the line's head ([lineFault]), with ONE
// jsontext.Decoder over the line, under the layer's own options, in this
// order: a leading NUL or byte order mark; a value cut at the line's end; a
// value nested past the decoder's bound; any other failure before a whole
// value; a whole value followed by more; and a whole value that is no object.
//
// ONLY A REFUSED LINE IS READ AGAIN, and only here: [decodeThreadLine] calls
// this from its refusals alone, so a line it takes is decoded once, and what
// it takes and refuses is what it took and refused before lines said how the
// load reads them. The decoder reads the line where it lies, from a
// bytes.Buffer, which jsontext reads with no copy, so a refused line costs
// what its decoder's state costs and never a copy of the line.
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
	// One object the decode refused, which no decode under these options
	// does: no fault, which every reader reads as a line whose load it cannot
	// say, never as one that yields the load nothing.
	return ""
}

// utf8ByteOrderMark is the byte order mark UTF-8 writes ([refusedFault]).
var utf8ByteOrderMark = []byte{0xef, 0xbb, 0xbf}

// jsontextMaxDepth is what encoding/json/jsontext says of a value nested past
// its bound ([faultDepth]), an error it does not export: held to it by
// [TestALineThatIsNoRecordSaysHowTheLoadReadsIt], which fails where a release
// of Go says it otherwise.
const jsontextMaxDepth = "exceeded max depth"

// kindOf is raw, a refused record's `type`, as the kind of record Claude
// Code's load reads where it is a string naming one ([recordKind]), the
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
		// READ AS IT WILL STREAM ([textSays], [textNotice], [textHandback]),
		// a piece at a time, until all three are decided.
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
	// A BLOCK AT A TIME ([eachElement]), and the calls grown from what was
	// accepted, never sized from the list's length, which only a whole decode
	// knows: a list refused at its first block costs that block. Every other
	// block is counted ([threadBlocks]), and held as nothing more.
	var m threadMessage
	// said is the block's text, unquoted into the one buffer every block
	// reuses, where a block's text may still change what the message says.
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
			// The block's text was read whole with its line, so unquoting it
			// meets no error but a lone surrogate's or invalid UTF-8's, each
			// written as U+FFFD, as the layer's decode reads it.
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

// contentBlock is one block of a message's content, raw, its kind and, for a
// call block, its call, by the one rule of a block ([blockCallOf]); and the
// block's `text`, raw, which [threadMessage.Says] reads of every block
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
	// A TARGET AND A TEXT ONLY WHERE THE BLOCK CARRIES WHAT MAKES THEM: an
	// input, a content. The rule keeps the target of a tool_use and the text
	// of a tool_result alone.
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

// sendTarget is the subagent a SendMessage call's input, raw, sends to, by the
// rule of a send's target ([sendTargetOf]): its `to`, matched exactly and a
// key written twice by its last value.
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

// resultTextOf is a tool_result's content, raw, as the reading compares it
// with a log's output: its text ([resultText]) read as it will stream
// ([textResult]), a piece at a time, into its length and its digests; no
// text, the zero value, where the content holds none.
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

// resultText is the text a tool_result's content, raw, holds: a JSON string, or
// a list of exactly one element, an object whose `type` is "text" and whose
// `text` is a string, other keys passed over, each key matched exactly and a
// key written twice read by its last value ([wireDecode]); false for every
// other content, absent and null among it.
//
// THE LIST IS READ NO FURTHER THAN ITS SECOND ELEMENT ([eachElement]), so a
// list a line long costs what its first two elements do.
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
		// A STRING, which the rule held it to: decoded as one, with no check
		// of its own beside the rule's.
		return wireDecode(block.Text, &text)
	})
	return text, err == nil && blocks == 1
}

// agentAnswer is what a leg's result record's own toolUseResult, raw, says of
// the leg, read by its exact keys and a key written twice by its last value,
// by the rule of a leg's answer ([answerOf]). A result of another shape says
// none of it: no answer, and no refusal.
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
// raw, carries a handback: a `handback` or a `handbackReport` key, any value,
// read by its exact key.
//
// A HANDBACK ANSWERS NO LEG. Under a handback contract, which both releases
// arm on a run of a subagent behind a flag on by default (2.1.322's BFt
// @288117159, 2.1.310's p_t @245867640, armed @302126445), the subagent
// delivers its report through a call of its own ([claudeToolHandback]), and
// its completion drops the text of its last reply and answers its caller
// with words of the harness's own in its place: that the report was delivered
// as a message, that it has not reported yet, or that none was delivered
// (2.1.322's pkn @302170267, `if(r!==void 0){Z=[];...}`, 2.1.310's Owr
// @255311767). A call's own answer then carries the `handback` key beside
// that text (`...ee!==void 0&&{handback:ee}`, spread into its result at
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

// everyElement reports whether raw is a list, and every element of it passes
// keep, as JavaScript's Array.isArray and every() read one: a list of none
// passes. Walked an element at a time ([eachElement]), stopping at the
// first that fails, so a long list costs its reader one element at a time.
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

// withheldEntryKept reports whether entry, one of a withheld_memory
// attachment's entries, raw, is of the shape Claude Code's resume keeps the
// attachment with, by the rule of such an entry ([withheldKept]).
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
// `names`, is of the shape Claude Code's resume keeps the attachment with:
// absent, or a list of at most [skillListingMaxNames] strings, each at most
// [skillListingMaxNameUnits] UTF-16 code units long, its length checked
// before its names, as the harness checks them. An error where the answer
// turns on a name the layer cannot count as the harness does
// ([threadLine.AttachmentWellFormed]).
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
		// Read whole with its line, the name unquotes with no error but a lone
		// surrogate's or invalid UTF-8's, each written as U+FFFD, as the
		// layer's decode reads it.
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
		return false, errors.New("a skill's name holds a byte that is no UTF-8, so its length in UTF-16 code units " +
			"is not one the layer can count as Claude Code does")
	}
	return true, nil
}

// preservedList is what a compaction's `preservedMessages`, raw, says it kept
// ([threadLine.Preserved]): named whatever it lists, and kept where it lists
// any, since a resume relinks by a list of none no record, and still drops by
// it every record before the last boundary.
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
		p.list(entry)
		return nil
	})
	if err != nil {
		return p, fmt.Errorf("compactMetadata.preservedMessages.uuids: %w", err)
	}
	p.Named, p.Kept, p.Anchor = true, p.Listed > 0, anchor
	return p, nil
}
