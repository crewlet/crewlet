package executor

import (
	"archive/tar"
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/jsonscan"
)

// A PERSON'S TRANSCRIPT OF A CODEX SESSION is read off its archive's one
// member ([codexArchiveFormat]) on the bounded line walk ([codexReading]),
// holding no line and no value past [TranscriptValueMaxBytes], which streams
// into its entry. Only the response items are read, never the event_msg lines
// that repeat them, so no entry is read twice.

// transcript reads a Codex archive ([codexArchiveFormat]).
func (Codex) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
	tr := tar.NewReader(archive)
	b.handed.r = tr
	r := newCodexReading(b, bufio.NewReaderSize(&b.handed, lineChunkBytes))
	for {
		h, err := tr.Next()
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return fmt.Errorf("%w: %w", errNotTar, err)
		}
		if h.Typeflag != tar.TypeReg || !codexIsMember(h.Name) {
			continue
		}
		r.framed = 0
		r.lines.Reset(&b.handed)
		if err := r.member(ctx); err != nil {
			return err
		}
	}
}

// codexIsMember reports whether name is a Codex archive's member: a rollout
// under the name it is restored by ([codexCarriedName]), whatever thread it
// names.
func codexIsMember(name string) bool {
	rest, ok := strings.CutPrefix(name, "rollout-"+codexCarriedStamp+"-")
	return ok && strings.HasSuffix(rest, ".jsonl") && !strings.Contains(rest, "/")
}

// codexWalkBound is the longest rollout line a walk reads, its break among
// it: [codexRolloutLine] reads one of [codexRolloutLineMaxBytes] and its break.
const codexWalkBound = codexRolloutLineMaxBytes + 1

// A CODEX LINE IS TAKEN AS CODEX TAKES ITS ROLLOUT: a line that is no record,
// or whose envelope's decode refuses it, is a FRAGMENT Codex left where it was
// cut off, which it skips, writing the next record on a line of its own
// (measured on 0.171.0), so it makes no entry and is never said; a last line
// with no break is one Codex was still writing, never read ([lineWalk.quiet],
// [lineWalk.tornUnread]). A payload one of its fields refuses is said. Each
// member is read by its whole-line decode's field ([fieldKind]); a function
// call's arguments past the value bound are shown as written (R3), not
// compacted, and a tool whose name or namespace runs past
// [transcriptNameMaxBytes] shows the two openings joined ([codexTool]).

// codexReading is a person's transcript of one rollout on the bounded line
// walk: a person's message is the brief, or each note, late call and wake it
// carries where it opens with an engine heading ([headedIn]); the model's
// words; a call and its answer; a compaction's summary. Bookkeeping lines
// (session_meta, turn_context, the event_msg lines that repeat each item,
// token counts) and reasoning make none, as do kinds a later release adds; an
// item of a kind this read does not know is said. A line's `type`, and an
// item's, may be written after the members whose reading it picks, so each is
// shaped as it is read and the line's end picks one.
type codexReading struct {
	lineWalk
	typ, stamp       string
	compactionOutput bool
	// fragment says the envelope's decode refused the line.
	fragment bool
	// payload says the line has one; compacted that a compaction's decode
	// takes it, its message summary.
	payload, compacted bool
	summary            textJoin
	item               codexItemRead
	// text is a string member's words, name a tool's, raw a value as
	// written; listRaw is a call's output list as written, held to the value
	// bound, never streamed.
	text, name, raw, listRaw valueText
	compactor, listCompact   jsonCompactor
	s                        entryShape
}

// codexItemRead is a response item as the line's walk reads it, each kind's
// members beside the others: Codex's ResponseItem, tagged by `type`.
type codexItemRead struct {
	// took says the item's decode takes the payload.
	took              bool
	typ, role, callID string
	name, namespace   toolName
	// content is a message's parts joined.
	content textJoin
	// args is a function call's arguments, input a custom tool's, action a
	// built-in call's, each its call's entry; output a call's answer.
	args, input, action readText
	output              codexOutput
	// meta is a message's metadata, who wrote it.
	meta codexMeta
}

// codexOutput is a call's answer as written: a text, a
// list of parts joined, or any other value compacted.
type codexOutput struct {
	list bool
	// value is a string's or another value's entry.
	value readText
	// parts joins a list's, and partsTook says its decode takes it: a list
	// it refuses is shown as written ([codexReading.listRaw]).
	parts     textJoin
	partsTook bool
}

// The joins of a Codex item: a message's input and output texts, an image a
// placeholder; a call's output parts, each but an image by its text.
var (
	codexContentParts = joinRule{kind: "input_text", also: "output_text", image: "input_image"}
	codexOutputParts  = joinRule{every: true, image: "input_image"}
	codexWholeText    = joinRule{anyKind: true}
)

// newCodexReading is a person's reading into b of the rollouts lines reads.
func newCodexReading(b *transcriptBuilder, lines *bufio.Reader) *codexReading {
	r := &codexReading{}
	r.setupBounded(b, r, lines)
	r.bound, r.src.bound = codexWalkBound, codexWalkBound
	r.quiet, r.tornUnread = true, true
	return r
}

func (*codexReading) own(*valueText) error { return nil }

func (r *codexReading) linesPast() error { return r.b.linesPast("Codex rollout", "lines") }

// start starts the record the next line holds.
func (r *codexReading) start() {
	r.typ, r.stamp, r.compactionOutput, r.fragment = "", "", false, false
	r.payloadStart()
	r.payload = false
}

// payloadStart starts the line's payload: one written again replaces it.
func (r *codexReading) payloadStart() {
	r.payload, r.compacted = true, true
	r.summary.start(r.b, TranscriptEntry{Kind: EntrySummary}, false, codexWholeText)
	it := &r.item
	it.took, it.typ, it.role, it.callID = true, "", "", ""
	it.name, it.namespace = toolName{}, toolName{}
	it.content.start(r.b, TranscriptEntry{Kind: EntryModel}, false, codexContentParts)
	it.args, it.input, it.action = readText{}, readText{}, readText{}
	it.output.list, it.output.value, it.output.partsTook = false, readText{}, true
	it.output.parts.start(r.b, TranscriptEntry{Kind: EntryToolResult}, false, codexOutputParts)
	r.listRaw.begin(r.b, TranscriptEntry{Kind: EntryToolResult}, false)
	it.meta = codexMeta{userKinds: true}
}

// record reads a line's envelope: its time, type, payload and metadata.
func (r *codexReading) record() error {
	return r.members(func(name string, k jsonscan.Kind) error {
		var ok bool
		var err error
		switch name {
		case "timestamp":
			r.stamp, ok, err = r.wordField(k)
		case "type":
			r.typ, ok, err = r.wordField(k)
		case "payload":
			ok, err = true, r.payloadField(k)
		case "metadata":
			r.compactionOutput = false
			ok, err = r.objectField(k, func(name string, k jsonscan.Kind) error {
				if name != "compaction_output" {
					return r.scan.Skip()
				}
				flag, took, err := r.flagField(k)
				r.compactionOutput, r.fragment = flag, r.fragment || !took
				return err
			})
		default:
			return r.scan.Skip()
		}
		r.fragment = r.fragment || !ok
		return err
	})
}

// payloadField reads the line's payload as a compaction's and as a response
// item's: null is either of no members, and a value of another kind neither.
func (r *codexReading) payloadField(k jsonscan.Kind) error {
	r.payloadStart()
	switch k {
	case jsonscan.Null:
		return nil
	case jsonscan.ObjectStart:
	default:
		r.compacted, r.item.took = false, false
		return r.scan.Skip()
	}
	it := &r.item
	return r.members(func(name string, k jsonscan.Kind) error {
		ok := true
		var err error
		switch name {
		case "message":
			if r.typ != "" && r.typ != "compacted" {
				ok, err = r.typedField(k, fieldString)
				r.compacted = r.compacted && ok
				return err
			}
			r.summary.start(r.b, TranscriptEntry{Kind: EntrySummary}, r.b.shapes(EntrySummary, r.b.seen),
				codexWholeText)
			switch k {
			case jsonscan.String:
				err = r.wholeText(&r.summary)
			case jsonscan.Null:
			default:
				ok, err = false, r.scan.Skip()
			}
			r.compacted = r.compacted && ok
			return err
		case "type":
			it.typ, ok, err = r.wordField(k)
		case "role":
			it.role, ok, err = r.wordField(k)
		case "call_id":
			it.callID, ok, err = r.wordField(k)
		case "name":
			it.name, ok, err = r.toolName(&r.name, k)
		case "namespace":
			it.namespace, ok, err = r.toolName(&r.name, k)
		case "content":
			ok, err = r.content(k)
		case "arguments":
			if !r.reads("function_call") {
				ok, err = r.typedField(k, fieldString)
				break
			}
			it.args, ok, err = r.argsText(k)
		case "input":
			if !r.reads("custom_tool_call") {
				ok, err = r.typedField(k, fieldString)
				break
			}
			it.input, ok, err = r.stringText(&r.text, TranscriptEntry{Kind: EntryToolCall}, k)
		case "action":
			if !r.reads("web_search_call", "local_shell_call") {
				return r.scan.Skip()
			}
			it.action, err = r.rawText(&r.raw, &r.compactor, TranscriptEntry{Kind: EntryToolCall})
		case "output":
			if !r.reads("function_call_output", "custom_tool_call_output") {
				return r.scan.Skip()
			}
			err = r.output(k)
		case "internal_chat_message_metadata_passthrough":
			ok, err = it.meta.read(&r.lineWalk, k)
		default:
			return r.scan.Skip()
		}
		it.took = it.took && ok
		return err
	})
}

// reads reports whether the item's value of one of kinds is read: its type is
// one of them, or not yet read.
func (r *codexReading) reads(kinds ...string) bool {
	return r.item.typ == "" || slices.Contains(kinds, r.item.typ)
}

// content reads a message's content, a list of parts joined
// ([codexContentParts]), and reports whether its decode takes it.
func (r *codexReading) content(k jsonscan.Kind) (bool, error) {
	b, j := r.b, &r.item.content
	j.start(b, TranscriptEntry{Kind: EntryModel}, b.shapes(EntryBrief, b.seen) || b.shapes(EntryModel, b.seen),
		codexContentParts)
	took := true
	ok, err := r.listField(k, func(k jsonscan.Kind) error {
		part, err := r.joinPart(j, k, nil)
		took = took && part
		return err
	})
	return ok && took, err
}

// argsText reads a function call's arguments, a string, as its call's entry:
// compacted as JSON ([compactJSON]) within the value bound, and past it as
// written (R3); null is none.
func (r *codexReading) argsText(k jsonscan.Kind) (readText, bool, error) {
	b, e := r.b, TranscriptEntry{Kind: EntryToolCall}
	ordinal := b.seen
	switch k {
	case jsonscan.Null:
		return readText{p: b.held(e, "", ordinal), set: true, blank: true}, true, nil
	case jsonscan.String:
	default:
		return readText{}, false, r.scan.Skip()
	}
	t := r.text.begin(b, e, b.shapes(EntryToolCall, ordinal))
	if err := r.read(t); err != nil {
		t.drop()
		return readText{}, true, err
	}
	if !t.past {
		defer t.drop()
		return readText{p: b.held(e, compactJSON([]byte(t.text())), ordinal), set: true}, true, nil
	}
	return readText{p: r.textEntry(t, e, ordinal), set: true}, true, nil
}

// output reads a call's output: a string as written, null none, a list of
// parts joined ([codexOutputParts]), and any other value compacted.
func (r *codexReading) output(k jsonscan.Kind) error {
	b, o := r.b, &r.item.output
	e := TranscriptEntry{Kind: EntryToolResult}
	o.list, o.value, o.partsTook = false, readText{}, true
	switch k {
	case jsonscan.String, jsonscan.Null:
		var err error
		o.value, _, err = r.stringText(&r.text, e, k)
		return err
	case jsonscan.ArrayStart:
	default:
		var err error
		o.value, err = r.rawText(&r.raw, &r.compactor, e)
		return err
	}
	o.list = true
	o.parts.start(b, e, b.shapes(EntryToolResult, b.seen), codexOutputParts)
	r.listRaw.begin(b, TranscriptEntry{Kind: EntryToolResult}, false)
	r.listCompact = jsonCompactor{w: &r.listRaw, buf: r.listCompact.buf}
	if err := r.scan.Tee(&r.listCompact); err != nil {
		return err
	}
	return r.elements(func(k jsonscan.Kind) (bool, error) {
		ok, err := r.joinPart(&o.parts, k, nil)
		o.partsTook = o.partsTook && ok
		return true, err
	})
}

// commit commits the entries the line's type picks; a fragment makes none.
func (r *codexReading) commit() error {
	if r.fragment {
		return nil
	}
	b, s := r.b, &r.s
	s.reset(b.seen)
	switch r.typ {
	case "compacted":
		if !r.payload || !r.compacted {
			s.add(b, b.held(TranscriptEntry{Kind: EntryUnread}, "a Codex compaction of a shape this read does "+
				"not know", s.next()), time.Time{})
			break
		}
		proseJoin(b, s, &r.summary, EntrySummary)
	case "response_item":
		r.itemInto(s)
	}
	return b.commitShape(s, codexInstant(r.stamp))
}

// itemInto adds the entries a response item makes to s.
func (r *codexReading) itemInto(s *entryShape) {
	b, it := r.b, &r.item
	if !r.payload || !it.took {
		s.add(b, b.held(TranscriptEntry{Kind: EntryUnread}, "a Codex record of a shape this read does not know",
			s.next()), time.Time{})
		return
	}
	switch it.typ {
	case "message":
		switch {
		case it.meta.persons(it.role):
			proseJoin(b, s, &it.content, EntryBrief)
		case it.role == "assistant" && !r.compactionOutput:
			proseJoin(b, s, &it.content, EntryModel)
		}
	case "reasoning", "ghost_snapshot":
	case "function_call":
		r.callInto(s, it.args)
	case "custom_tool_call":
		r.callInto(s, it.input)
	case "web_search_call", "local_shell_call":
		p := it.action.entry(b, EntryToolCall, s.next())
		if p.entry.Kind == EntryToolCall {
			p.entry.Tool = strings.TrimSuffix(it.typ, "_call")
		}
		s.add(b, p, time.Time{})
	case "function_call_output", "custom_tool_call_output":
		s.addEvent(b, r.outputEntry(s), time.Time{}, claudeEvent{kind: eventResult, id: it.callID})
	default:
		s.add(b, b.held(TranscriptEntry{Kind: EntryUnread}, fmt.Sprintf("a Codex record of a kind this read does "+
			"not know (%.64q)", it.typ), s.next()), time.Time{})
	}
}

// callInto adds a call of the item's tool to s, its input in.
func (r *codexReading) callInto(s *entryShape, in readText) {
	b, it := r.b, &r.item
	p, ev := codexTool(it.name, it.namespace).call(b, in.entry(b, EntryToolCall, s.next()), it.callID, s.next())
	s.addEvent(b, p, time.Time{}, ev)
}

// codexTool is the tool a call names: an MCP tool as Codex offers it to the
// model, its namespace before its name. Past [transcriptNameMaxBytes] either
// is its opening, so the tool shows the two openings joined, and is one no
// result is answered by.
func codexTool(name, namespace toolName) toolName {
	if namespace.raw == "" && !namespace.past {
		return name
	}
	if !name.past && !namespace.past {
		return heldToolName(namespace.raw + "__" + name.raw)
	}
	part := func(n toolName) string {
		if n.past {
			return n.shown
		}
		return n.raw
	}
	t := heldToolName(part(namespace) + "__" + part(name))
	t.raw, t.past, t.refused = "", true, name.refused || namespace.refused
	return t
}

// outputEntry is the entry a call's output makes at s's next ordinal: a list
// its parts joined, or as written where its decode refuses it; one of no shape
// past the value bound is said in its place.
func (r *codexReading) outputEntry(s *entryShape) pendingEntry {
	b, o := r.b, &r.item.output
	switch {
	case !o.list:
		return o.value.entry(b, EntryToolResult, s.next())
	case o.partsTook:
		return textAs(b, &o.parts, s, EntryToolResult)
	case r.listRaw.past:
		return b.held(TranscriptEntry{Kind: EntryUnread}, fmt.Sprintf("a tool's result of the harness's "+
			"record of no shape this read knows runs past %s, and was not read", humanSize(int64(b.valueMax))),
			s.next())
	case !s.shapes(b, EntryToolResult):
		return b.held(TranscriptEntry{Kind: EntryToolResult}, "", s.next())
	}
	return b.held(TranscriptEntry{Kind: EntryToolResult}, r.listRaw.text(), s.next())
}

// codexInstant is a rollout line's timestamp, zero where it carries none an
// answer can carry ([carriedInstant]).
func codexInstant(stamp string) time.Time {
	at, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return time.Time{}
	}
	return carriedInstant(at.UTC())
}
