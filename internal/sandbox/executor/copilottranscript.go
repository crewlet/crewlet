package executor

import (
	"archive/tar"
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/jsonscan"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// transcript reads a Copilot archive ([copilotArchiveFormat]): its
// events.jsonl, a line at a time, as the session's entries. Any other member
// is passed unread.
func (Copilot) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
	tr := tar.NewReader(archive)
	b.handed.r = tr
	r := newCopilotReading(b, bufio.NewReaderSize(&b.handed, lineChunkBytes))
	for {
		h, err := tr.Next()
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return fmt.Errorf("%w: %w", errNotTar, err)
		}
		if !copilotEventsMember(h.Name) {
			continue
		}
		if h.Typeflag != tar.TypeReg {
			if err := b.unread("a transcript of the harness's record is not a plain file in the archive, " +
				"and was not read"); err != nil {
				return err
			}
			continue
		}
		r.framed = 0
		r.lines.Reset(&b.handed)
		if err := r.member(ctx); err != nil {
			return err
		}
	}
}

// copilotEventsMember reports whether an archive's member named name is a
// session's events.jsonl: `session-state/<session>/events.jsonl`.
func copilotEventsMember(name string) bool {
	parts := strings.Split(path.Clean(name), "/")
	return len(parts) == 3 && parts[0] == copilotSessionState && parts[1] != "" && parts[2] == "events.jsonl"
}

// A COPILOT LINE'S MEMBERS ARE READ BY ITS WHOLE-LINE DECODE'S FIELDS
// ([fieldKind], [copilotEvent]): one refused by them is read for its type
// alone, as that decode read it ([decodeCopilotEvent]), and a compaction's
// summary by its own decode. A line that names no type is no record.

// copilotReading is a person's transcript of one events.jsonl on the bounded
// line walk ([lineWalk.setupBounded]), in recorded order, a subagent's lines
// its thread, named by its agent:
//   - a user message is the brief, or, where it opens with an engine heading
//     (a note, a late call, a wake the extension sent), each it carries
//     ([headedIn]);
//   - an assistant message is the model's words and a call per tool request;
//   - a call's completion its result, failed where Copilot recorded it so;
//   - a completed compaction its summary.
type copilotReading struct {
	lineWalk
	typ          string
	typed, whole bool
	// agent is the line's agent as written, or as shown past the value bound;
	// stamp its time.
	agent, stamp string
	// The data: its content, its tool requests, a completion's call, success,
	// result and failure, and a compaction's summary, which its own decode
	// reads where the data's (summed) is of its kind.
	content           textJoin
	calls             callList
	requested         bool
	callID            string
	succeeded         bool
	result, failure   readText
	summary           readText
	summed            bool
	text, name, input valueText
	compactor         jsonCompactor
	s                 entryShape
}

// newCopilotReading is a person's reading into b of the members lines reads.
func newCopilotReading(b *transcriptBuilder, lines *bufio.Reader) *copilotReading {
	r := &copilotReading{}
	r.setupBounded(b, r, lines)
	return r
}

// copilotWords is how a message's content is read: as one text, written whole.
var copilotWords = joinRule{kind: "text"}

// start starts the record the next line holds.
func (r *copilotReading) start() {
	r.typ, r.typed, r.whole, r.agent, r.stamp = "", true, true, "", ""
	r.dataStart()
}

// dataStart starts the record's data: data written again replaces it.
func (r *copilotReading) dataStart() {
	r.content.start(r.b, TranscriptEntry{Kind: EntryBrief}, false, copilotWords)
	r.calls.reset(r.b.seen)
	r.requested, r.callID, r.succeeded = false, "", false
	r.result, r.failure, r.summary, r.summed = readText{}, readText{}, readText{}, true
}

func (*copilotReading) own(*valueText) error { return nil }

func (r *copilotReading) linesPast() error { return r.b.linesPast("record", "lines") }

// copilotEventFields and copilotDataFields are the members of a line and of
// its data that [copilotEvent] decodes and this read does not, by their
// fields' kinds; copilotUsageFields a usage record's.
var (
	copilotEventFields = map[string]fieldKind{"sessionId": fieldString, "exitCode": fieldAny}
	copilotDataFields  = map[string]fieldKind{"sessionId": fieldString, "parentToolCallId": fieldString,
		"clientTurnId": fieldString, "delivery": fieldString, "toolName": fieldString, "arguments": fieldAny,
		"mcpServerName": fieldString, "mcpConfigSource": fieldString, "errorType": fieldString,
		"errorCode": fieldString, "message": fieldString, "aborted": fieldBool, "turns": fieldInt}
	copilotUsageFields = map[string]fieldKind{"inputTokens": fieldInt, "outputTokens": fieldInt,
		"cacheReadTokens": fieldInt, "cacheWriteTokens": fieldInt, "maxPromptTokens": fieldInt}
)

// record reads a line's type, agent, time and data.
func (r *copilotReading) record() error {
	return r.members(func(name string, k jsonscan.Kind) error {
		var ok bool
		var err error
		switch name {
		case "type":
			r.typ, ok, err = r.wordField(k)
			r.typed = r.typed && ok
		case "agentId":
			r.agent, ok, err = r.agentField(k)
		case "timestamp":
			r.stamp, ok, err = r.wordField(k)
		case "data":
			ok, err = r.data(k)
		default:
			kind, read := copilotEventFields[name]
			if !read {
				return r.scan.Skip()
			}
			ok, err = r.typedField(k, kind)
		}
		r.whole = r.whole && ok
		return err
	})
}

// agentField reads a line's agent, the string value k begins: as written
// within the value bound, as an entry shows it past it.
func (r *copilotReading) agentField(k jsonscan.Kind) (string, bool, error) {
	if k != jsonscan.String {
		_, ok, err := r.wordField(k)
		return "", ok, err
	}
	t := r.name.begin(r.b, TranscriptEntry{}, false)
	if err := r.read(t); err != nil {
		t.drop()
		return "", true, err
	}
	if !t.past {
		agent := t.text()
		t.drop()
		return agent, true, nil
	}
	shown, _ := t.shownName()
	return shown, true, nil
}

// data reads a line's data.
func (r *copilotReading) data(k jsonscan.Kind) (bool, error) {
	r.dataStart()
	r.summed = k == jsonscan.ObjectStart || k == jsonscan.Null
	return r.objectField(k, func(name string, k jsonscan.Kind) error {
		var ok bool
		var err error
		switch name {
		case "content":
			ok = k == jsonscan.String || k == jsonscan.Null
			r.content.start(r.b, TranscriptEntry{Kind: EntryBrief},
				r.b.shapes(EntryBrief, r.b.seen) || r.b.shapes(EntryModel, r.b.seen), copilotWords)
			if k == jsonscan.String {
				err = r.wholeText(&r.content)
			} else {
				err = r.scan.Skip()
			}
		case "toolRequests":
			ok, err = true, r.requests(k)
		case "toolCallId":
			r.callID, ok, err = r.wordField(k)
		case "success":
			r.succeeded, ok, err = k == jsonscan.True, true, r.scan.Skip()
		case "result":
			r.result = readText{}
			ok, err = r.objectField(k, func(name string, k jsonscan.Kind) error {
				if name != "content" {
					return r.scan.Skip()
				}
				result, took, readErr := r.stringText(&r.text, TranscriptEntry{Kind: EntryToolResult}, k)
				r.result, r.whole = result, r.whole && took
				return readErr
			})
		case "error":
			ok, err = true, r.failed(k)
		case "summaryContent":
			var took bool
			r.summary, took, err = r.stringText(&r.text, TranscriptEntry{Kind: EntrySummary}, k)
			r.summed = r.summed && took
			return err
		case "usage":
			ok, err = r.typedObject(k, copilotUsageFields)
		default:
			kind, read := copilotDataFields[name]
			if !read {
				return r.scan.Skip()
			}
			ok, err = r.typedField(k, kind)
		}
		r.whole = r.whole && ok
		return err
	})
}

// requests reads an assistant message's tool requests
// (`toolRequests[{toolCallId, name, arguments}]`), a call each, or says they
// have no shape this read knows where their list's decode refuses them.
func (r *copilotReading) requests(k jsonscan.Kind) error {
	r.calls.reset(r.b.seen)
	r.requested = true
	if k != jsonscan.ArrayStart {
		if k != jsonscan.Null {
			r.calls.unshaped(r.b)
		}
		return r.scan.Skip()
	}
	shaped := true
	err := r.elements(func(k jsonscan.Kind) (bool, error) {
		var id string
		var tool toolName
		var input *valueText
		took, err := r.objectField(k, func(name string, k jsonscan.Kind) error {
			var ok bool
			var err error
			switch name {
			case "toolCallId":
				id, ok, err = r.wordField(k)
			case "name":
				tool, ok, err = r.toolName(&r.name, k)
			case "arguments":
				input = r.input.begin(r.b, TranscriptEntry{Kind: EntryToolCall}, r.calls.shapes(r.b))
				r.compactor = jsonCompactor{w: input, buf: r.compactor.buf}
				if teeErr := r.scan.Tee(&r.compactor); teeErr != nil {
					return teeErr
				}
				ok, err = true, r.scan.Skip()
			default:
				return r.scan.Skip()
			}
			shaped = shaped && ok
			return err
		})
		shaped = shaped && took
		switch {
		case err == nil && shaped:
			r.calls.add(r.b, input, tool, id)
		case input != nil:
			input.drop()
		}
		return true, err
	})
	if err == nil && !shaped {
		r.calls.unshaped(r.b)
	}
	return err
}

// failed reads a completion's error, any value, as the text a failed result
// shows: the words of an object's `message` where they are not blank, else
// the error as [errorText] makes it, a string's words or any other value as
// written and compacted.
func (r *copilotReading) failed(k jsonscan.Kind) error {
	e := TranscriptEntry{Kind: EntryToolResult}
	var err error
	switch k {
	case jsonscan.String, jsonscan.Null:
		r.failure, _, err = r.stringText(&r.text, e, k)
		return err
	case jsonscan.ObjectStart:
	default:
		r.failure, err = r.rawText(&r.input, &r.compactor, e)
		return err
	}
	ordinal := r.b.seen
	t := r.input.begin(r.b, e, r.b.shapes(EntryToolResult, ordinal))
	r.compactor = jsonCompactor{w: t, buf: r.compactor.buf}
	if err = r.scan.Tee(&r.compactor); err != nil {
		t.drop()
		return err
	}
	var words readText
	err = r.members(func(name string, k jsonscan.Kind) error {
		if name != "message" {
			return r.scan.Skip()
		}
		message, _, readErr := r.stringText(&r.text, e, k)
		if words = message; !words.set {
			words = readText{blank: true}
		}
		return readErr
	})
	if err != nil {
		t.drop()
		return err
	}
	r.failure = readText{p: r.textEntry(t, e, ordinal), set: true}
	if words.set && !words.blank {
		r.failure = words
	}
	return nil
}

// commit commits the reading the line's type picks: a line read for its type
// alone has no agent, no time and no data but its summary's.
func (r *copilotReading) commit() error {
	b, s := r.b, &r.s
	if !r.typed || r.typ == "" {
		return r.refused()
	}
	at := time.Time{}
	if b.thread = ""; r.whole {
		b.thread, at = r.agent, claudeTime(r.stamp)
	}
	s.reset(b.seen)
	switch r.typ {
	case "user.message":
		if r.whole {
			r.wordsInto(s)
		}
	case "assistant.message":
		if !r.whole {
			break
		}
		words := !r.content.blank()
		if words {
			s.add(b, textAs(b, &r.content, s, EntryModel), time.Time{})
		}
		if err := b.commitThread(s, at); err != nil || !r.requested {
			return err
		}
		return r.calls.commitAfter(b, words, at)
	case "tool.execution_complete":
		failed := !r.whole || !r.succeeded
		p := b.held(TranscriptEntry{Kind: EntryToolResult}, "", s.next())
		switch {
		case !r.whole:
		case failed:
			p = r.failure.entry(b, EntryToolResult, s.next())
		default:
			p = r.result.entry(b, EntryToolResult, s.next())
		}
		if p.entry.Kind == EntryToolResult {
			p.entry.Failed = failed
		}
		id := ""
		if r.whole {
			id = r.callID
		}
		s.addEvent(b, p, time.Time{}, claudeEvent{kind: eventResult, id: id})
	case "session.compaction_complete":
		if r.summed && r.summary.set && !r.summary.blank {
			s.add(b, r.summary.p, time.Time{})
		}
	}
	return b.commitThread(s, at)
}

// wordsInto adds a user message to s: each note, late call and wake it
// carries where it opens with an engine heading, else the brief; past the
// bound the brief alone (R1).
func (r *copilotReading) wordsInto(s *entryShape) {
	j := &r.content
	switch text, held := j.text(); {
	case j.blank():
	case held:
		r.b.proseInto(s, EntryBrief, text)
	default:
		s.add(r.b, textAs(r.b, j, s, EntryBrief), time.Time{})
	}
}

// copilotNotesHeld is the inbox ids of takes, in serve order and once each,
// whose note a session's events.jsonl shows reached the model as a current
// message: the main agent's `user.message` under its clientTurnId
// ([CopilotClientTurnID]), `steering` or `queued`, then a main agent's request
// ([copilotMainRequest]) begun after the line ([copilotBegan]), with no
// compaction between that may have left it only in the summary (ADR-0177): one
// not stated failed, unless the note stands among the trailing messages its
// start counts (`retainedInputMessages`; measured on 1.0.114,
// compacted-queued), as [copilotWindow] counts them. The line alone is no
// proof (ruling 1 of #1818's design): a kill or a refusal can follow it. A
// last line with no newline is a write cut off; one past the bound, no note.
// Walked for notes ([lineWalk.noteLine]), and its newest summary, if asked.
func copilotNotesHeld(ctx context.Context, events io.Reader, takes []ServedTake,
	summary *summaryRead) ([]string, error) {
	notes := copilotNotes{asked: make(map[string]string, len(takes)), pending: map[string]copilotPending{},
		held: map[string]bool{}}
	for _, take := range takes {
		notes.asked[CopilotClientTurnID(take.InboxID)] = take.InboxID
	}
	src := &lastByte{r: events}
	var w lineWalk
	w.setupNotes(bufio.NewReaderSize(src, lineChunkBytes))
	var ev copilotNoteLine
	if summary != nil {
		ev.summary = &summaryText{}
	}
	for {
		line, err := w.noteLine(ctx, func() error { return ev.read(&w) })
		if err != nil {
			return nil, fmt.Errorf("executor: reading which notes the Copilot session holds: %w", err)
		}
		if line.none {
			break
		}
		if summary != nil {
			ev.summarised(line, summary)
		}
		notes.read(ev, ev.decoded(line))
		if line.ended {
			break
		}
	}
	if src.read && src.last != '\n' {
		for _, inbox := range notes.moved {
			delete(notes.held, inbox)
		}
	}
	var out []string
	for _, take := range takes {
		if notes.held[take.InboxID] && !slices.Contains(out, take.InboxID) {
			out = append(out, take.InboxID)
		}
	}
	return out, nil
}

// copilotNotes is what [copilotNotesHeld] keeps as it reads: the notes it is
// asked about, by their clientTurnId; each note sent and not yet held, by its
// line; the lines that may be messages the main agent's model reads; the
// notes the compaction started last carries over whole; the notes held; how
// many lines it read; and what the line read last moved to held, taken back
// where that line turns out to be the file's unended last.
type copilotNotes struct {
	asked    map[string]string
	pending  map[string]copilotPending
	window   copilotWindow
	retained map[string]bool
	held     map[string]bool
	n        int
	moved    []string
}

// read reads the record's next line, ev where ok.
func (c *copilotNotes) read(ev copilotNoteLine, ok bool) {
	c.n++
	c.moved = c.moved[:0]
	c.window.read(c.n, ev, ok)
	if !ok {
		return
	}
	// A note and a request are the main agent's only where the line names
	// no agent at all: the window counts more lines than this holds.
	switch ev.Type {
	case "user.message":
		if !ev.AgentID.empty() {
			return
		}
		inbox, ours := c.asked[ev.Data.ClientTurnID]
		at, timed := copilotTime(ev.Timestamp)
		// A line whose time cannot be read is one no request can be
		// shown to have begun after.
		if ours && timed && (ev.Data.Delivery == "steering" || ev.Data.Delivery == "queued") {
			c.pending[inbox] = copilotPending{line: c.n, at: at}
		}
	case "session.usage_record":
		// A FAILED MAIN REQUEST WRITES NONE (measured on 1.0.114: a
		// provider answering 500 past its five retries left a
		// session.error and no usage record), so it settles nothing.
		if !ev.AgentID.empty() || ev.Data.Usage.InteractionType != copilotMainRequest {
			return
		}
		began, ok := copilotBegan(ev)
		if !ok {
			return
		}
		for inbox, note := range c.pending {
			if note.at.After(began) {
				continue
			}
			delete(c.pending, inbox)
			if !c.held[inbox] {
				c.held[inbox] = true
				c.moved = append(c.moved, inbox)
			}
		}
	case "session.compaction_start":
		c.retained = copilotRetained(c.pending, c.window.messages, ev.Data.RetainedInputMessages)
	case "session.compaction_complete":
		// Past a compaction, a request may carry the note only inside
		// its summary: only one that says it failed keeps every note, and
		// one that applied those it carried over whole.
		if !ev.Data.Failed {
			for inbox := range c.pending {
				if !c.retained[inbox] {
					delete(c.pending, inbox)
				}
			}
		}
		c.retained = nil
	}
}

// copilotRetained is the pending notes a compaction starting now carries over
// whole: those among the last count of messages, by line, since what it keeps
// is the conversation's TRAILING messages ([copilotNoteLine]). Counting every
// line that may be a message makes the window no wider than Copilot's under
// any reading of its count (user messages, or messages of every role), so a
// note an answer, a tool's result or a notification followed falls out of it.
func copilotRetained(pending map[string]copilotPending, messages []int, count int64) map[string]bool {
	if count <= 0 || len(messages) == 0 {
		return nil
	}
	from := messages[len(messages)-int(min(count, int64(len(messages))))]
	kept := map[string]bool{}
	for inbox, note := range pending {
		if note.line >= from {
			kept[inbox] = true
		}
	}
	return kept
}

// copilotWindow is the lines of a session's events.jsonl that may be
// messages the main agent's model reads, by number, as Copilot counts them
// toward a compaction's `retainedInputMessages`. Only what the compaction
// captures prove is no message is left out: the count before each start is
// its `preCompactionMessagesLength` (1.0.114; a test holds them equal);
// any other kind, a line of no kind read, one past the bound, and one naming
// an agent no `subagent.started` announced count, which only narrows the
// window. The compaction's own start is where the count is read.
type copilotWindow struct {
	messages  []int
	announced map[recordWord]bool
}

func (w *copilotWindow) read(n int, ev copilotNoteLine, ok bool) {
	if ok && ev.Type == "subagent.started" && !ev.AgentID.empty() {
		if w.announced == nil {
			w.announced = map[recordWord]bool{}
		}
		w.announced[ev.AgentID] = true
	}
	if ok && (w.announced[ev.AgentID] || ev.Type == "session.compaction_start" || !copilotMessage(ev)) {
		return
	}
	w.messages = append(w.messages, n)
}

// copilotMessage reports whether a line may be a message the model reads:
// every kind but those the compaction captures prove are not ([copilotWindow]),
// a hook's end only where it returned nothing.
func copilotMessage(ev copilotNoteLine) bool {
	switch ev.Type {
	case "hook.start", "assistant.turn_start", "assistant.turn_end", "tool.execution_start",
		"session.start", "session.permissions_changed", "session.usage_record", "session.info":
		return false
	case "hook.end":
		return !ev.bareHook
	}
	return true
}

// copilotPending is a note sent and not yet held: its line's number and time.
type copilotPending struct {
	line int
	at   time.Time
}

// copilotBegan is when the request a main agent's usage record states began:
// its line's time less the request's `duration`, in milliseconds. A request
// begun before a note's line never carried the note, whatever came after it;
// measured on 1.0.114 (inflight-note, inflight-final), a note sent while a
// request is in flight has its line written as the next request is made, so
// every request that held one began 5 to 1,000 ms after its line. False where
// either cannot be read: that record holds nothing.
func copilotBegan(ev copilotNoteLine) (time.Time, bool) {
	at, ok := copilotTime(ev.Timestamp)
	raw := ev.Data.Usage.Duration
	if !ok || len(raw) == 0 || raw[0] < '0' || raw[0] > '9' {
		return time.Time{}, false
	}
	var ms float64
	if json.Unmarshal(raw, &ms) != nil {
		return time.Time{}, false
	}
	return at.Add(-time.Duration(ms * float64(time.Millisecond))), true
}

// copilotTime is a line's `timestamp`, false where it is not RFC 3339.
func copilotTime(stamp string) (time.Time, bool) {
	at, err := time.Parse(time.RFC3339Nano, stamp)
	return at, err == nil
}

// copilotNoteLine is what [copilotNotesHeld] reads of one events.jsonl line:
// its own few members, those Copilot's kind could vary read as written, so
// that no other member of the line can lose them, as a member of another kind
// loses the whole of [decodeCopilotEvent]'s; whole where each of them is of
// its field's kind ([fieldKind]), typed where at least its type is.
type copilotNoteLine struct {
	Type      string
	AgentID   recordWord
	Timestamp string
	Data      struct {
		ClientTurnID string
		Delivery     string
		// Failed is a compaction's completion that states it failed
		// (`success: false`).
		Failed bool
		// RetainedInputMessages, on a compaction's start: "Number of
		// trailing conversation messages (the turn's not-yet-answered input)
		// kept verbatim after the summary instead of being compacted.
		// Omitted when every message at compaction start is summarized."
		// (schemas/session-events.schema.json, 1.0.114's bundle.) A whole
		// int64 ([looseInt]), else 0.
		RetainedInputMessages int64
		Usage                 struct {
			InteractionType string
			// Duration is the request's `duration` as written, where it is
			// a number within [transcriptNameMaxBytes].
			Duration json.RawMessage
		}
	}
	// bareHook is a `hook.end` whose data holds only its id, its kind and its
	// success: a hook that handed the model nothing.
	bareHook     bool
	whole, typed bool
	// summary, where a read asks one, is the data's summaryContent.
	summary *summaryText
}

// read reads one line's members.
func (l *copilotNoteLine) read(w *lineWalk) error {
	*l = copilotNoteLine{whole: true, typed: true, bareHook: true, summary: l.summary}
	if l.summary != nil {
		l.summary.reset()
	}
	return w.members(func(name string, k jsonscan.Kind) error {
		var ok bool
		var err error
		switch name {
		case "type":
			l.Type, ok, err = w.wordField(k)
			l.typed = l.typed && ok
		case "agentId":
			l.AgentID, ok, err = w.idField(k)
		case "timestamp":
			l.Timestamp, ok, err = w.wordField(k)
		case "data":
			l.Data, l.bareHook = copilotNoteLine{}.Data, true
			if l.summary != nil {
				l.summary.reset()
			}
			ok, err = w.objectField(k, func(name string, k jsonscan.Kind) error {
				l.bareHook = l.bareHook && (name == "hookInvocationId" || name == "hookType" || name == "success")
				return l.data(w, name, k)
			})
		default:
			return w.scan.Skip()
		}
		l.whole = l.whole && ok
		return err
	})
}

// data reads one member of the line's data.
func (l *copilotNoteLine) data(w *lineWalk, name string, k jsonscan.Kind) error {
	var ok bool
	var err error
	switch name {
	case "clientTurnId":
		l.Data.ClientTurnID, ok, err = w.wordField(k)
	case "delivery":
		l.Data.Delivery, ok, err = w.wordField(k)
	case "success":
		l.Data.Failed, ok, err = k == jsonscan.False, true, w.scan.Skip()
	case "summaryContent":
		if l.summary == nil {
			return w.scan.Skip()
		}
		_, err = l.summary.read(w, k)
		return err
	case "retainedInputMessages":
		raw, rawErr := w.numberAsWritten(k)
		l.Data.RetainedInputMessages, ok, err = looseInt(raw), true, rawErr
	case "usage":
		l.Data.Usage.InteractionType, l.Data.Usage.Duration = "", nil
		ok, err = w.objectField(k, func(name string, k jsonscan.Kind) error {
			var took bool
			var fieldErr error
			switch name {
			case "interactionType":
				l.Data.Usage.InteractionType, took, fieldErr = w.wordField(k)
			case "duration":
				var raw []byte
				raw, fieldErr = w.numberAsWritten(k)
				l.Data.Usage.Duration, took = slices.Clone(raw), true
			default:
				return w.scan.Skip()
			}
			l.whole = l.whole && took
			return fieldErr
		})
	default:
		return w.scan.Skip()
	}
	l.whole = l.whole && ok
	return err
}

// decoded reports whether the line read is one of a type, keeping what the
// read takes of it: the whole of it, or its type alone where some other member
// is of another kind than its field. A line read for its type is no note's,
// its usage record no main request's, and its compaction one that applied; a
// line past the bound, or no JSON object, is of none.
func (l *copilotNoteLine) decoded(line noteLine) bool {
	switch {
	case line.past, !line.object, !l.typed:
		*l = copilotNoteLine{summary: l.summary}
	case !l.whole:
		*l = copilotNoteLine{Type: l.Type, summary: l.summary}
	case l.Type != "hook.end":
		l.bareHook = false
	}
	return l.Type != ""
}

// summarised makes the line's summaryContent the newest summary where it is a
// main agent's completed compaction's, as the transcript reads one
// ([copilotReading.commit]): a line whose members are not all of their kinds
// is the main agent's there, and a summary of no string none. A line past the
// bound leaves the newest unsaid.
func (l *copilotNoteLine) summarised(line noteLine, s *summaryRead) {
	t := l.summary
	switch {
	case line.past:
		s.unshaped(fmt.Sprintf("a line of the record runs past the %d bytes a line is read to", maxLineBytes))
	case !line.object || !l.typed || l.Type != "session.compaction_complete" || l.whole && !l.AgentID.empty():
	case !t.odd:
		s.taken(t)
	}
}

// copilotMainRequest is the `interactionType` of a main agent's own model
// request, on its `session.usage_record`: a subagent's is
// `conversation-subagent` (and its record names its agent), and the session's
// sampling, background and compaction calls have kinds of their own
// (session-events.d.ts on 1.0.114; measured, subagent-note). Only such a
// request carries the main conversation a note was sent into.
const copilotMainRequest = "conversation-agent"

// notesHeld is the inbox ids of takes ([Copilot.inboxID]) whose note
// the box's copy of session shows reached the model ([copilotNotesHeld]): its
// [recordConfirmer], as its extension confirms nothing (ruling 1 of #1818's
// design), which the Runner's main-thread read asks for the takes it is asked.
// An error where the record could not be read, which is neither held nor not.
func (Copilot) notesHeld(ctx context.Context, box sandbox.Sandbox, session Session,
	takes []ServedTake) ([]string, error) {
	events, err := box.OpenFile(ctx, PathsFor(box).CopilotSessionDir(session.ID)+"/events.jsonl")
	if err != nil {
		return nil, fmt.Errorf("executor: opening the Copilot session's record: %w", err)
	}
	defer func() { _ = events.Close() }()
	return copilotNotesHeld(ctx, events, takes, nil)
}

// readMainThread reads the newest summary on the main thread of ask's session
// in box, off the box's copy of its record ([copilotNotesHeld]), on the
// release its summary was read on alone. Its receipts are its record's
// ([recordConfirmer]).
func (Copilot) readMainThread(ctx context.Context, box sandbox.Sandbox, paths Paths,
	ask sandbox.MainThreadAsk) sandbox.MainThreadReading {

	if !ask.Summary {
		return sandbox.MainThreadReading{}
	}
	if why := summaryReleaseWhy(ask.Release, copilotFloor()); why != "" {
		return sandbox.MainThreadReading{Summary: summaryUnread(why)}
	}
	if !sandbox.ValidSessionID(ask.Session) {
		return sandbox.MainThreadReading{Summary: summaryUnread(fmt.Sprintf(
			"the run's Copilot session %q is no session id its record is named by", ask.Session))}
	}
	events, err := box.OpenFile(ctx, paths.CopilotSessionDir(ask.Session)+"/events.jsonl")
	if err != nil {
		return sandbox.MainThreadReading{Summary: summaryUnread("the session's record did not open: " + err.Error())}
	}
	defer func() { _ = events.Close() }()
	var summary summaryRead
	if _, readErr := copilotNotesHeld(ctx, events, nil, &summary); readErr != nil {
		summary.fail(readErr.Error())
	}
	return sandbox.MainThreadReading{Summary: summary.reading(ask)}
}

// threadTraits is what Copilot's main-thread read settles: the newest summary
// alone, its record's receipts added by [Runner.ThreadTraits].
func (Copilot) threadTraits() sandbox.ThreadTraits { return sandbox.ThreadTraits{Summaries: true} }
