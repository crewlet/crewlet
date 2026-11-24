package executor

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/crewlet/crewlet/internal/jsonscan"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// WHAT OPENCODE COUNTS A SESSION AT BEFORE ITS NEXT REQUEST, read off the
// session's export ([openCodeCountReading]) in ONE WALK on the scanner, which
// holds no string: the last counted reply's input, cache, output and
// reasoning, and every message after it, the reply's tool results among them,
// at a quarter of its bytes rounded up, never under OpenCode's own estimate;
// restarted at the newest completed compaction, its restart the newest
// compaction of any status, the reply's provider beside it. The package doc
// ("What OpenCode counts a session at before its next request") says why the
// charge bounds OpenCode's, and where each rule was read.

// exportReading is what one walk of an OpenCode export read
// ([meterExport]).
type exportReading struct {
	// figure is the input the session's last request carried, by the one rule
	// for an OpenCode session's measure ([measuredPrompt]); zero where the
	// export did not read.
	figure int64
	// count is what OpenCode counts the session at before its next request:
	// a bound, or a mark saying why there is none.
	count *sandbox.ResumedCount
	// provider is the provider of the reply the count is anchored on, "" where
	// the count names none.
	provider string
	// next is the estimate of the session's next request: the count's bound,
	// at OpenCode's one rate, zero where the count is marked.
	next sandbox.NextRequest
}

// meter reads an OpenCode archive for the take's meter ([carrier.meter],
// [meterExport]).
func (OpenCode) meter(ctx context.Context, r io.Reader, _, release string,
	_ *passBudget) (meterReading, error) {

	export, err := meterExport(ctx, r, release)
	return meterReading{tokens: export.figure, count: export.count, provider: export.provider,
		next: export.next}, err
}

// meterExport reads r, an OpenCode export gzipped, once, within ctx: the
// floor's figure and OpenCode's count of the session ([openCodeCountReading]),
// the count read by the rules of release, the box's word on the release that
// wrote it ([openCodeCountRead]). An export that does not read to its end has
// no figure and an unread count; err is what ended the read.
func meterExport(ctx context.Context, r io.Reader, release string) (exportReading, error) {
	var (
		count   openCodeCountReading
		callers countMarks
	)
	if !openCodeCountRead(release) {
		callers = callers.with(sandbox.CountRelease)
	}
	figure, err := countExport(ctx, r, &count)
	if err != nil {
		return exportReading{count: count.count(callers.with(sandbox.CountUnread))}, err
	}
	out := exportReading{figure: figure, count: count.count(callers)}
	if bound, ok := out.count.Bound(); ok {
		out.next = sandbox.NextRequest{Tokens: bound}
	}
	if out.count.Mark == "" || out.count.Restart != nil {
		out.provider = count.provider
	}
	return out, nil
}

// openCodeRestart reports whether a message of kind is one where an OpenCode
// session restarts: a compaction, whatever its status. THE ONE PREDICATE every
// reader of an OpenCode session's restarts calls, so the count's identity
// ([openCodeCountReading]) and a reading of what a session holds never part on
// where it restarts.
func openCodeRestart(kind openCodeKind) bool { return kind == openCodeCompaction }

// openCodeCountReading is OpenCode's count of a session, read forward over its
// export's messages in order (the file comment). It keeps no string of the
// export but the newest compaction's id and the anchor's provider, each held
// to its bound.
type openCodeCountReading struct {
	// anchored says a counted reply was read since the session's start or its
	// newest completed compaction; figure is that reply's counts, tail the
	// charge of everything after it, and marks the files after it.
	anchored     bool
	figure, tail int64
	marks        countMarks
	// provider is the anchor's provider.
	provider string
	// restart is the newest compaction's id, "" where there is none.
	restart string
	// unread says the message list was given twice, so which one a reader
	// keeps is the reader's.
	unread bool
	// unsure says a value the count from its anchor on reads did not read: a
	// step's tokens, the anchor's provider, a compaction's status, or a tool's
	// answer of no shape OpenCode writes, at the anchor or after it. A later
	// anchor, or a completed compaction, starts the count again and clears it.
	unsure bool
	// unnamed says the newest compaction's id did not read.
	unnamed bool
}

// openCodeCountTokens is what one reply's request measured, as the count reads
// it: the prompt's three parts, and the output and reasoning OpenCode counts
// after them.
type openCodeCountTokens struct {
	Input     int64 `json:"input"`
	Output    int64 `json:"output"`
	Reasoning int64 `json:"reasoning"`
	Cache     struct {
		Read  int64 `json:"read"`
		Write int64 `json:"write"`
	} `json:"cache"`
}

// exportMessage is one message of an export as the walk reads it: its small
// values raw, its span, and what its content and files hold.
type exportMessage struct {
	// wire is what the floor's figure reads ([measuredPrompt]), its id left
	// out: the figure never reads one, so an id that does not read costs it
	// nothing.
	wire              openCodeMeasuredWire
	id, status, model json.RawMessage
	// span is the message's bytes as the export holds it, from its opening
	// brace to its closing one.
	span int64
	// results is the charge of its tool calls' answers and failures, a
	// quarter of each one's span rounded up.
	results int64
	// files are the marks of the files it names: a person's, or a tool's
	// answer's.
	files countMarks
	// odd says a value the count reads is of no shape OpenCode writes.
	odd bool
}

// message is one message read, in order.
func (r *openCodeCountReading) message(m exportMessage, kind openCodeKind) {
	switch {
	case openCodeRestart(kind):
		id, ok := exportString(m.id)
		r.restart, r.unnamed = id, !ok || id == "" || len(id) > sandbox.RestartIDMaxBytes
		status, ok := exportString(m.status)
		switch {
		case !ok:
			// WHETHER THE COUNT RESTARTED HERE is not known.
			r.unsure = true
		case status == openCodeCompleted:
			// THE COUNT RESTARTS: OpenCode counts from the compaction on, and a
			// reply after it anchors the count again.
			r.anchored, r.figure, r.tail, r.marks, r.provider, r.unsure = false, 0, 0, 0, "", false
			return
		}
		r.after(m)
	case kind == openCodeAssistant && absent(m.wire.Error) && !absent(m.wire.Tokens):
		var t openCodeCountTokens
		if err := wireDecode(m.wire.Tokens, &t); err != nil {
			// A STEP THAT MAY BE THE ANCHOR, unread.
			r.unsure = true
			return
		}
		prompt := max(t.Input, 0) + max(t.Cache.Read, 0) + max(t.Cache.Write, 0)
		if prompt <= 0 {
			r.after(m)
			return
		}
		var model struct {
			ProviderID json.RawMessage `json:"providerID"`
		}
		provider, ok := "", false
		if wireDecode(m.model, &model) == nil {
			provider, ok = exportString(model.ProviderID)
		}
		named := ok && provider != "" && sandbox.ValidCountProvider(provider)
		if !named {
			provider = ""
		}
		// THE ANCHOR: its counts, and its own tool results after it, which
		// OpenCode counts as the messages that follow its reply.
		r.anchored, r.figure, r.provider = true, prompt+max(t.Output, 0)+max(t.Reasoning, 0), provider
		r.tail, r.marks, r.unsure = m.results, m.files, !named || m.odd
	default:
		r.after(m)
	}
}

// after charges a message that is no anchor to the tail, where a reply
// anchors the count: its whole span, and its files' marks.
func (r *openCodeCountReading) after(m exportMessage) {
	if !r.anchored {
		return
	}
	r.tail += quarter(m.span)
	r.marks |= m.files
	r.unsure = r.unsure || m.odd
}

// count is the bound the reading holds, or the first mark of callers' and its
// own ([sandbox.CountMarks]' order); and, where the export was read whole by
// the rules of its release, the newest compaction's id.
func (r *openCodeCountReading) count(callers countMarks) *sandbox.ResumedCount {
	marks := callers
	if r.unread || r.unsure || r.unnamed {
		marks = marks.with(sandbox.CountUnread)
	}
	if r.anchored {
		marks |= r.marks
	} else {
		marks = marks.with(sandbox.CountUnanchored)
	}
	out := &sandbox.ResumedCount{Tokens: r.figure + r.tail}
	// THE IDENTITY IS BLIND ONLY WHERE THE EXPORT WAS NOT READ WHOLE OR NOT BY
	// THE RULES OF ITS RELEASE, as a Claude Code count's is: every other mark
	// says what the bytes cannot bound, never where the session restarts.
	if blind := countMarks(0).with(sandbox.CountUnread).with(sandbox.CountRelease); marks&blind == 0 {
		out.Restart = new(r.restart)
	}
	if mark := marks.first(); mark != "" {
		out.Tokens, out.Mark = 0, mark
	}
	return out
}

// quarter is n bytes at four a token, rounded up.
func quarter(n int64) int64 { return (max(n, 0) + 3) / 4 }

// exportString is raw read as a string by its exact keys, and whether it read
// as one: absent, null or another kind is none.
func exportString(raw json.RawMessage) (string, bool) {
	if absent(raw) {
		return "", false
	}
	var s string
	if wireDecode(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// exportValueMaxBytes is the most of one of a message's small values the walk
// holds: the scanner's own read buffer, 64 KiB, so a value within it is
// captured from the buffer it is already in. A message's kind, status and id
// are words, its time, tokens and model a few values each, under a few
// hundred bytes; and its error, which the measure reads only for whether it is
// there, is never read past it.
const exportValueMaxBytes = 64 << 10

// errorPastBound stands for a message's error past [exportValueMaxBytes]: no
// null, so the step ended on an error, which is all a reader reads of one.
var errorPastBound = json.RawMessage("true")

// countExport reads r, an OpenCode export gzipped, once on the export walk
// ([scanExport]), within ctx: each message to count in order, and the floor's
// figure, its last message that measured a prompt ([measuredPrompt]). An
// error where the export is no export, or does not read to its end.
func countExport(ctx context.Context, r io.Reader, count *openCodeCountReading) (int64, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return 0, err
	}
	m := &exportMeter{count: count}
	if err = scanExport(ctx, zr, m); err != nil {
		return 0, err
	}
	return m.last, nil
}

// meterAsks is what the meter reads of an export: a message's small values,
// captured, its content's tool calls and its files.
var meterAsks = exportAsksOf(map[exportFrame]map[int]exportMeans{
	frameMessage: {msgID: meansCapture, msgType: meansCapture, msgTime: meansCapture, msgTokens: meansCapture,
		msgError: meansCapture, msgStatus: meansCapture, msgModel: meansCapture, msgContent: meansFrame,
		msgFiles: meansFrame},
	frameItem:       {itemType: meansWord, itemState: meansFrame},
	frameState:      {stateContent: meansFrame, stateError: meansFrame},
	frameAnswerPart: {partType: meansWord, partMime: meansWord},
	frameFile:       {fileType: meansWord, fileMime: meansWord},
}, false)

// exportMeter is the meter as the export walk's visitor: each message's small
// values by their exact names, captured at [exportValueMaxBytes] and decoded
// by [openCodeMeasuredWire]'s one rule; of each tool call the spans of its
// answer and its failure, and the files its answer holds; a person's files.
//
// HELD TO [exportValueMaxBytes]: an error past it is one (it is no null), an
// id, a status or a model past it does not read, and a kind, a time or tokens
// past it is no message OpenCode writes, which the meter reads as an export it
// cannot read, rather than hold whatever a box wrote there.
type exportMeter struct {
	count *openCodeCountReading
	// last is the floor's figure; lists how many `messages` lists were met.
	last  int64
	lists int
	// m is the message being read, its captures in held, a buffer each,
	// reused message to message; captured says m holds one.
	m        exportMessage
	held     [exportMembersMax][]byte
	captured bool
	// The item, its tool call's state and the answer's part or file being
	// read: the item a tool's, its results, files and oddity; the state's
	// charge, files and oddity; the part's kind text and its type a PDF.
	tool                  bool
	results, charge       int64
	itemFiles, stateFiles countMarks
	itemOdd, stateOdd     bool
	partText, partPDF     bool
}

func (*exportMeter) asks() *exportAsks { return meterAsks }

func (*exportMeter) digest() *textDigest { return nil }

func (*exportMeter) sink(exportFrame, int, jsonscan.Kind) io.Writer { return nil }

// open starts what a frame's members fold into.
func (e *exportMeter) open(f exportFrame) error {
	switch f {
	case frameMessages:
		// A LIST GIVEN TWICE: the figure reads both, as it always has, and the
		// count reads neither, since which a reader keeps is the reader's.
		if e.lists++; e.lists > 1 {
			e.count.unread = true
		}
	case frameMessage:
		// A MESSAGE THAT CAPTURED NOTHING LEAVES NOTHING TO CLEAR, so a list of
		// empty messages costs no clearing of one.
		if e.captured {
			e.m, e.captured = exportMessage{}, false
		} else {
			e.m.span, e.m.results, e.m.files, e.m.odd = 0, 0, 0, false
		}
	case frameItem:
		e.tool, e.results, e.itemFiles, e.itemOdd = false, 0, 0, false
	case frameState:
		e.charge, e.stateFiles, e.stateOdd = 0, 0, false
	case frameAnswerPart, frameFile:
		e.partText, e.partPDF = false, false
	}
	return nil
}

// member folds one member, a key written twice as the meter always has: a
// capture and a word by its last, a list's marks and charges by every one.
func (e *exportMeter) member(f exportFrame, i int, v exportValue) error {
	switch f {
	case frameMessage:
		return e.message(i, v)
	case frameItem:
		switch {
		case i == itemType:
			e.tool = v.is("tool")
		case v.kind == jsonscan.ObjectStart:
			e.results, e.itemFiles, e.itemOdd = e.charge, e.stateFiles, e.stateOdd
		default:
			// A STATE OF NO OBJECT: OpenCode writes an object, so another value
			// is odd (null is none).
			e.results, e.itemFiles, e.itemOdd = 0, 0, v.kind != jsonscan.Null
		}
	case frameState:
		// AN ANSWER OR A FAILURE IS CHARGED AT A QUARTER OF ITS SPAN, a
		// container's, and one that is neither a container nor null is odd.
		switch v.kind {
		case jsonscan.Null:
		case jsonscan.ArrayStart, jsonscan.ObjectStart:
			e.charge += quarter(v.span)
		default:
			e.stateOdd = true
		}
	case frameAnswerPart:
		switch i {
		case partType:
			e.partText = v.is("text")
		case partMime:
			e.partPDF = v.is("application/pdf")
		}
	case frameFile:
		if i == fileMime {
			e.partPDF = v.is("application/pdf")
		}
	}
	return nil
}

// message folds one member of a message: content and files that are no list
// are odd and an unknown file (null is none); every other is a capture.
func (e *exportMeter) message(i int, v exportValue) error {
	var to *json.RawMessage
	switch i {
	case msgContent:
		e.m.odd = e.m.odd || v.kind != jsonscan.Null && v.kind != jsonscan.ArrayStart
		return nil
	case msgFiles:
		if v.kind != jsonscan.Null && v.kind != jsonscan.ArrayStart {
			e.m.files = e.m.files.with(sandbox.CountMedia)
		}
		return nil
	case msgID:
		to = &e.m.id
	case msgType:
		to = &e.m.wire.Type
	case msgTime:
		to = &e.m.wire.Time
	case msgTokens:
		to = &e.m.wire.Tokens
	case msgError:
		to = &e.m.wire.Error
	case msgStatus:
		to = &e.m.status
	case msgModel:
		to = &e.m.model
	}
	e.captured = true
	switch {
	case !v.past:
		e.held[i] = append(e.held[i][:0], v.text...)
		*to = e.held[i]
	case i == msgError, i == msgID, i == msgStatus, i == msgModel:
		// An error past the bound is one; an id, a status or a model past it
		// is no string, so it reads as none, and the count that needs it is
		// unread.
		*to = errorPastBound
	default:
		return fmt.Errorf("%w: a message's %s runs past %d bytes", errNotExport, exportMessagePast[i],
			exportValueMaxBytes)
	}
	return nil
}

// exportMessagePast names the members of a message past whose bound the meter
// reads no export.
var exportMessagePast = map[int]string{msgType: "type", msgTime: "time", msgTokens: "tokens"}

// element folds an element of a list that is no object: a file in an answer
// or a person's files, an unknown one; a message, no export.
func (e *exportMeter) element(f exportFrame, k jsonscan.Kind) error {
	switch f {
	case frameMessages:
		return fmt.Errorf("%w: it holds %s where a message belongs", errNotExport, k)
	case frameAnswer:
		e.stateFiles = e.stateFiles.with(sandbox.CountMedia)
	case frameFiles:
		e.m.files = e.m.files.with(sandbox.CountMedia)
	}
	return nil
}

// close ends a frame: a message counted, an item's tool call charged to it, a
// part or file marked.
func (e *exportMeter) close(f exportFrame, v exportValue) error {
	switch f {
	case frameMessage:
		e.m.span = v.span
		// A MESSAGE THAT CAPTURED NOTHING decodes to no kind and no measure, so
		// it is counted with no decode: an empty message costs no allocation.
		var msg openCodeMessage
		if e.captured {
			var err error
			if msg, _, err = e.m.wire.message(); err != nil {
				return err
			}
		}
		if n, measured := measuredPrompt(msg); measured {
			e.last = n
		}
		e.count.message(e.m, msg.Type)
	case frameItem:
		if e.tool {
			e.m.results += e.results
			e.m.files |= e.itemFiles
			e.m.odd = e.m.odd || e.itemOdd
		}
	case frameAnswerPart:
		// IN A TOOL'S ANSWER a text part is no file; every other is one, a PDF
		// where its type says so.
		switch {
		case e.partText:
		case e.partPDF:
			e.stateFiles = e.stateFiles.with(sandbox.CountPDF)
		default:
			e.stateFiles = e.stateFiles.with(sandbox.CountMedia)
		}
	case frameFile:
		if e.partPDF {
			e.m.files = e.m.files.with(sandbox.CountPDF)
		} else {
			e.m.files = e.m.files.with(sandbox.CountMedia)
		}
	}
	return nil
}

// expectKind reads the next token, an error where it is not of kind want.
func expectKind(sc *jsonscan.Scanner, want jsonscan.Kind) error {
	k, err := sc.Next()
	if err != nil {
		return err
	}
	if k != want {
		return fmt.Errorf("%w: it holds %s where %s belongs", errNotExport, k, want)
	}
	return nil
}

var _ mainThreadHarness = OpenCode{}

// readMainThread is OpenCode's read of ask's session in box: the export the
// job's script left as the job ended ([openCodeCapture], [Paths.SessionCapture]),
// which is the copy the box holds once its harness ended its own run (the
// collection asks nothing of a job whose harness did not), walked ONCE for the
// count and the estimate ([meterExport]). In this order, cheapest first: a
// release outside the set the count's rules were read in is
// [sandbox.CountRelease] with no read ([openCodeCountRead]); an export the
// script left none of, or one that does not open, is [sandbox.CountUnread];
// and only then is the export streamed and walked. A
// count marked before the walk leaves the estimate none, as a Claude Code
// thread's does.
func (OpenCode) readMainThread(ctx context.Context, box sandbox.Sandbox, paths Paths,
	ask sandbox.MainThreadAsk) sandbox.MainThreadReading {

	if !ask.Count && !ask.Next {
		return sandbox.MainThreadReading{}
	}
	marked := func(mark sandbox.CountMark) sandbox.MainThreadReading {
		if !ask.Count {
			return sandbox.MainThreadReading{}
		}
		return sandbox.MainThreadReading{Count: &sandbox.ResumedCount{Mark: mark}}
	}
	if !openCodeCountRead(ask.Release) {
		return marked(sandbox.CountRelease)
	}
	body, err := box.OpenFile(ctx, paths.SessionCapture())
	if err != nil {
		return marked(sandbox.CountUnread)
	}
	defer func() { _ = body.Close() }()
	read, _ := meterExport(ctx, body, ask.Release)
	var reading sandbox.MainThreadReading
	if ask.Count {
		reading.Count, reading.Provider = read.count, read.provider
	}
	if ask.Next {
		reading.Next = read.next
	}
	return reading
}
