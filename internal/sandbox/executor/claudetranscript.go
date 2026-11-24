package executor

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/jsonscan"
)

// A PERSON'S TRANSCRIPT OF A CLAUDE CODE SESSION is read on the line walk
// ([lineWalk]). A record's kind (`type`, `isMeta`, `isCompactSummary`) can be
// written after its message and a block's `type` after its words, so each
// reading of a record is shaped as it is read and kept provisional
// ([entryShape]) until the line ends and its kind picks one.

// claudeMessageKinds are the kinds of a record's message's three readings, by
// their place in [claudeRecordRead.msg]: the model's words, a person's brief
// (split into the notes and late calls it opens with) and a compaction's
// summary.
var claudeMessageKinds = [3]EntryKind{EntryModel, EntryBrief, EntrySummary}

const (
	readModel = iota
	readBrief
	readSummary
)

// claudeArchive reads the session's transcript and each subagent's, a member
// at a time, passing every other member unread. What a member hands out is
// metered apart from what the archive inflates to ([TranscriptReadBytes]), and
// every member is read through ONE reading, its buffers kept: an archive may
// hold hundreds of thousands of empty members.
func (b *transcriptBuilder) claudeArchive(ctx context.Context, archive io.Reader) error {
	tr := tar.NewReader(archive)
	b.handed.r = tr
	r := newClaudeReading(b, bufio.NewReaderSize(&b.handed, lineChunkBytes))
	for {
		h, err := tr.Next()
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return fmt.Errorf("%w: %w", errNotTar, err)
		}
		thread, ok := claudeThread(h.Name)
		if !ok {
			continue
		}
		b.thread = thread
		clear(b.calls)
		if h.Typeflag != tar.TypeReg {
			// A TRANSCRIPT THAT IS NO PLAIN FILE (a link, a directory) is not read
			// through: it names another member or a path outside the archive. Said where
			// it was.
			if unreadErr := b.unread("a transcript of the harness's record is not a plain " +
				"file in the archive, and was not read"); unreadErr != nil {
				return unreadErr
			}
			continue
		}
		r.lines.Reset(&b.handed)
		if memberErr := r.member(ctx); memberErr != nil {
			return memberErr
		}
	}
}

// claudeMember reads member, one transcript of a Claude Code archive, as
// [transcriptBuilder.claudeArchive] reads each.
func (b *transcriptBuilder) claudeMember(ctx context.Context, member io.Reader) error {
	return newClaudeReading(b, bufio.NewReaderSize(member, lineChunkBytes)).member(ctx)
}

// claudeReading is a person's transcript of a Claude Code session as the line
// walk reads it: the record the line holds, the block and the result it is in,
// and the texts it reads into.
type claudeReading struct {
	lineWalk
	rec claudeRecordRead
	blk claudeBlockRead
	res claudeAnswer
	// text is a message's words, name a tool's, input a call's input compacted.
	text, name, input valueText
	compact           jsonCompactor
	// handed is a hook_additional_context's content being read: the one text
	// whose notes are read ([claudeReading.attachment]).
	handed bool
}

// newClaudeReading is a person's reading into b of the members lines reads.
func newClaudeReading(b *transcriptBuilder, lines *bufio.Reader) *claudeReading {
	r := &claudeReading{}
	r.setup(b, r, lines)
	r.res.b = b
	return r
}

// start starts the record the next line holds.
func (r *claudeReading) start() { r.rec.reset(r.b) }

// linesPast says where the read passed its lines bound, and ends it.
func (r *claudeReading) linesPast() error {
	return r.b.linesPast("harness's record", "lines, blocks and parts")
}

// own adds the notes and late calls a text of the record carries to its own
// readings ([claudeRecordRead.notes]), where the text is one a hook handed its
// model ([claudeReading.attachment]); every other copy of a hook's words is
// passed over. A text past the value bound names none (R1). The caller is
// asked before each.
func (r *claudeReading) own(t *valueText) error {
	if !r.handed || t.past || !bytes.Contains(t.held, []byte(headingOpen)) {
		return nil
	}
	for kind, words := range headedIn(t.text()) {
		if err := r.asked(); err != nil || r.rec.notes.full && r.rec.hooked.full {
			return err
		}
		r.heldIn(r.entry(kind), words, kindShape{&r.rec.notes, kind}, kindShape{&r.rec.hooked, kind})
	}
	return nil
}

// record reads a record's members: its kind, subtype and time, whether the
// harness wrote it for itself or as a compaction's summary, its message, and a
// system record's content; every other member for its own words. A KEY WRITTEN
// TWICE HOLDS THE LAST VALUE, as JSON.parse reads it.
func (r *claudeReading) record() error {
	rec := &r.rec
	return r.members(func(name string, k jsonscan.Kind) error {
		var err error
		switch name {
		case "type":
			rec.kind, err = r.wordValue(k)
		case "subtype":
			rec.subtype, err = r.wordValue(k)
		case "timestamp":
			var stamp string
			stamp, err = r.wordValue(k)
			rec.at = claudeTime(stamp)
		case "isMeta":
			rec.meta = k == jsonscan.True
			err = r.value(k)
		case "isCompactSummary":
			rec.compact = k == jsonscan.True
			err = r.value(k)
		case "message":
			err = r.message(k)
		case "content":
			err = r.systemContent(k)
		case "attachment":
			err = r.attachment(k)
		default:
			err = r.value(k)
		}
		return err
	})
}

// attachment reads a record's attachment. THE NOTES, LATE CALLS AND WAKES A
// HOOK HANDED ITS MODEL ARE READ OFF ONE RECORD, its firing's
// `hook_additional_context`, from its `content` strings alone. Measured on
// 2.1.310 and 2.1.322 (testdata/claude/<release>/holdings/s1): a firing writes
// that one record and copies its words into the attachment's `rendered` form,
// the request's `api-request-blob`, the `hook_success` stdout and, at Stop,
// `stop_hook_summary`, each read as the note again (#1054); 2.1.163, 2.1.263
// and 2.1.330 build the same attachment. One of another type hands none,
// whatever order its members are written in.
func (r *claudeReading) attachment(k jsonscan.Kind) error {
	if k != jsonscan.ObjectStart {
		return r.value(k)
	}
	// A KEY WRITTEN TWICE HOLDS THE LAST VALUE: an attachment or its content
	// written again replaces what the first handed.
	r.rec.handedOver()
	var kind string
	err := r.members(func(name string, k jsonscan.Kind) error {
		switch name {
		case "type":
			var err error
			kind, err = r.wordValue(k)
			return err
		case "content":
			r.rec.handedOver()
			r.handed = true
			defer func() { r.handed = false }()
		}
		return r.value(k)
	})
	if err == nil && kind != hookAdditionalContext {
		r.rec.handedOver()
	}
	return err
}

// handedOver drops the notes the record's readings hold: only an attachment's
// content fills them ([claudeReading.own]).
func (rec *claudeRecordRead) handedOver() {
	rec.notes.reset(rec.notes.base)
	rec.hooked.reset(rec.hooked.base)
}

// message reads a record's message, an object whose content is a string or a
// list of blocks, into its three readings; one of no shape this read knows is
// said rather than read as a message that said nothing.
func (r *claudeReading) message(k jsonscan.Kind) error {
	r.rec.resetMessage(r.b)
	switch k {
	case jsonscan.Null:
		return nil
	case jsonscan.ObjectStart:
		return r.members(func(name string, k jsonscan.Kind) error {
			if name != "content" {
				return r.value(k)
			}
			return r.content(k)
		})
	}
	r.unshaped()
	return r.value(k)
}

// unshaped says, in each reading of the message, that it has no shape this
// read knows.
func (r *claudeReading) unshaped() {
	r.rec.resetMessage(r.b)
	for i := range r.rec.msg {
		s := &r.rec.msg[i]
		s.add(r.b, r.b.held(r.entry(EntryUnread), unshapedText, s.next()), time.Time{})
	}
}

// unshapedText is what a message of no shape this read knows says in its place.
const unshapedText = "a message of the harness's record has no shape this read knows, and was not read"

// content reads a message's content: a string is one text of each reading's
// kind, and a list of blocks an entry a block ([claudeReading.block]); content
// that is neither is said.
func (r *claudeReading) content(k jsonscan.Kind) error {
	r.rec.resetMessage(r.b)
	switch k {
	case jsonscan.Null:
		return nil
	case jsonscan.String:
		t := r.text.begin(r.b, r.entry(EntryModel), r.rec.shapesAny(r.b, claudeMessageKinds[:]...))
		if err := r.read(t); err != nil {
			return err
		}
		if err := r.own(t); err != nil {
			return err
		}
		var words [3][]pendingEntry
		r.prose(t, &words)
		for i := range r.rec.msg {
			r.rec.msg[i].addAll(r.b, words[i])
		}
		t.drop()
		return nil
	case jsonscan.ArrayStart:
		return r.elements(func(k jsonscan.Kind) (bool, error) {
			if err := r.frame(); err != nil {
				return true, err
			}
			if k != jsonscan.ObjectStart {
				return false, nil
			}
			return true, r.block()
		})
	}
	r.unshaped()
	return r.value(k)
}

// systemContent reads a record's own content, the one line the run's own hook
// prints as it hands the model a note or a late call ([claudeSteerSaid]), the
// first entry of a system record's reading where its subtype says so. Past the
// value bound it is no hook line (R4).
func (r *claudeReading) systemContent(k jsonscan.Kind) error {
	rec := &r.rec
	rec.hookSet = false
	if k != jsonscan.String {
		return r.value(k)
	}
	t := r.str.begin(r.b, TranscriptEntry{Kind: EntryNote}, false)
	defer t.drop()
	if err := r.read(t); err != nil {
		return err
	}
	if !t.past {
		if said, ok := claudeSteerContent(t.text()); ok {
			hookLine, _, _ := strings.Cut(strings.TrimSpace(said), "\n")
			if hookLine = strings.TrimSpace(hookLine); hookLine != "" {
				e := r.entry(hookLineKind(hookLine))
				rec.hook, rec.hookSet = r.b.held(e, hookLine, rec.notes.base), true
			}
		}
	}
	return r.own(t)
}

// entry is an entry of kind of the thread being read.
func (r *claudeReading) entry(kind EntryKind) TranscriptEntry {
	return TranscriptEntry{Kind: kind, Thread: r.b.thread}
}

// kindShape is a reading and the kind an entry takes in it.
type kindShape struct {
	s    *entryShape
	kind EntryKind
}

// heldIn adds e, its text raw and held, to each reading as the kind it names,
// shaping it once ([transcriptBuilder.held]) for all that keep it on the page:
// THE SHAPING IS THE SAME FOR EVERY KIND OF WORDS, which read alike
// ([readingOf]), so only the kind and the ordinal differ.
func (r *claudeReading) heldIn(e TranscriptEntry, raw string, into ...kindShape) {
	var shaped pendingEntry
	done := false
	for _, ks := range into {
		s := ks.s
		e.Kind = ks.kind
		if !s.shapes(r.b, ks.kind) {
			s.add(r.b, r.b.held(e, "", s.next()), time.Time{})
			continue
		}
		if !done {
			shaped, done = r.b.held(e, raw, s.next()), true
		}
		s.add(r.b, withKind(shaped, ks.kind), time.Time{})
	}
}

// withKind is p as an entry of kind, unless it is said in its place.
func withKind(p pendingEntry, kind EntryKind) pendingEntry {
	if p.entry.Kind != EntryUnread && !p.passed {
		p.entry.Kind = kind
	}
	return p
}

// prose is the entries the text t just read makes in each reading of the
// message, as the page would take each ([shapeCursor]): none for a blank one;
// in a brief that opens with a heading, each note and late call it carries;
// else one entry of the reading's kind. PAST THE BOUND a text is one entry of
// its kind (R1), its stream ended once for every reading that shapes it.
func (r *claudeReading) prose(t *valueText, out *[3][]pendingEntry) {
	for i := range out {
		out[i] = out[i][:0]
	}
	if t.past {
		if t.blank {
			return
		}
		r.streamedInto(t, out)
		return
	}
	text := t.text()
	if strings.TrimSpace(text) == "" {
		return
	}
	var shaped pendingEntry
	done := false
	for i := range r.rec.msg {
		c, kind := r.rec.msg[i].cursor(), claudeMessageKinds[i]
		if kind == EntryBrief && opensWithHeading(text) {
			for heading, words := range headedIn(text) {
				if c.full {
					break
				}
				p := r.b.held(r.entry(heading), "", c.next)
				if c.shapes(r.b, heading) {
					p = r.b.held(r.entry(heading), words, c.next)
				}
				c.take(r.b, p)
				out[i] = append(out[i], p)
			}
			continue
		}
		if !c.shapes(r.b, kind) {
			out[i] = append(out[i], r.b.held(r.entry(kind), "", c.next))
			continue
		}
		if !done {
			shaped, done = r.b.held(r.entry(kind), text, c.next), true
		}
		out[i] = append(out[i], withKind(shaped, kind))
	}
}

// streamedInto is the one entry t, a text past the bound, makes in each
// reading of the message, of its kind: its stream's, ended once, where the
// reading shapes it, and one only counted or passed where not.
func (r *claudeReading) streamedInto(t *valueText, out *[3][]pendingEntry) {
	var streamed pendingEntry
	ended := false
	for i := range r.rec.msg {
		s, kind := &r.rec.msg[i], claudeMessageKinds[i]
		if t.s == nil || !s.shapes(r.b, kind) {
			out[i] = append(out[i], r.b.held(r.entry(kind), "", s.next()))
			continue
		}
		if !ended {
			streamed, ended = t.s.end(s.next()), true
		}
		out[i] = append(out[i], withKind(streamed, kind))
	}
}

// claudeEvent is what an entry of a message's reading does, as it commits, to
// the calls the transcript waits on: a call remembers its tool
// ([transcriptBuilder.called]), and a result names the tool it answers
// ([transcriptBuilder.answered]). An id or a tool past [transcriptNameMaxBytes]
// is "" and remembers nothing.
type claudeEvent struct {
	kind     claudeEventKind
	id, tool string
}

// happen does ev to the calls the transcript waits on, as the entry that does it
// commits, and is the tool a result's call was of, as written.
func (b *transcriptBuilder) happen(ev claudeEvent) string {
	switch ev.kind {
	case eventCall:
		b.called(ev.id, ev.tool)
	case eventResult:
		return b.answered(ev.id)
	}
	return ""
}

// claudeEventKind is what a [claudeEvent] does.
type claudeEventKind uint8

const (
	eventNone claudeEventKind = iota
	eventCall
	eventResult
)

// claudeRecordRead is what a reading keeps of the record its line holds: its
// kind and the facts that pick a reading, its time, and its readings, the
// message's three ([claudeMessageKinds]) and its own two, the notes of every
// string and name with and without the hook line before them.
type claudeRecordRead struct {
	kind, subtype string
	meta, compact bool
	at            time.Time
	msg           [3]entryShape
	notes, hooked entryShape
	hook          pendingEntry
	hookSet       bool
}

// reset starts the record a line holds, its entries from the ordinal b's next
// takes.
func (rec *claudeRecordRead) reset(b *transcriptBuilder) {
	rec.kind, rec.subtype, rec.meta, rec.compact, rec.at = "", "", false, false, time.Time{}
	rec.hook, rec.hookSet = pendingEntry{}, false
	rec.resetMessage(b)
	rec.notes.reset(b.seen)
	rec.hooked.reset(b.seen + 1)
}

// resetMessage starts the message's readings over: a message or its content
// written again replaces what it was.
func (rec *claudeRecordRead) resetMessage(b *transcriptBuilder) {
	for i := range rec.msg {
		rec.msg[i].reset(b.seen)
	}
}

// shapesAny reports whether a reading of the message shapes its next entry as
// the kind its place names among kinds.
func (rec *claudeRecordRead) shapesAny(b *transcriptBuilder, kinds ...EntryKind) bool {
	for i := range rec.msg {
		if rec.msg[i].shapes(b, kinds[i]) {
			return true
		}
	}
	return false
}

// commit commits the reading the record's kind picks: the model's for an
// assistant's; a person's brief, or a compaction's summary, for a user's the
// harness did not write for itself; and for every other, the notes and late
// calls its strings and names carry, behind the hook line where a system
// record's subtype and content name one.
func (r *claudeReading) commit() error {
	rec, b := &r.rec, r.b
	switch {
	case rec.kind == "assistant":
		return b.commitShape(&rec.msg[readModel], rec.at)
	case rec.kind == "user" && !rec.meta && rec.compact:
		return b.commitShape(&rec.msg[readSummary], rec.at)
	case rec.kind == "user" && !rec.meta:
		return b.commitShape(&rec.msg[readBrief], rec.at)
	case rec.kind == "system" && rec.subtype == "informational" && rec.hookSet:
		hook := rec.hook
		if hook.entry.Kind != EntryUnread {
			hook.entry.At = rec.at
		}
		if err := b.commit(hook); err != nil {
			return err
		}
		return b.commitShape(&rec.hooked, rec.at)
	}
	return b.commitShape(&rec.notes, rec.at)
}

// claudeBlockRead is what a reading keeps of the block it is in: its kind, the
// call's and the result's ids, whether the result failed, and the entries each
// of its members makes in each reading of the message, of which its kind picks
// one at its end.
type claudeBlockRead struct {
	kind, id, toolUseID string
	failed              bool
	// words are the entries its `text` makes, call its `input`'s, result its
	// `content`'s; callRead and resultRead say the member was read.
	words                [3][]pendingEntry
	call, result         [3]pendingEntry
	callRead, resultRead bool
	// tool is its `name` as shown, raw as written within
	// [transcriptNameMaxBytes] and namePast past it, refused where the stream
	// refused its redaction (R6).
	tool, raw             string
	namePast, toolRefused bool
}

// block reads one block of a message's content: its words, a tool call and
// its input, or a tool's result, by its `type`, which may be written after
// them. A thinking block is the model's working and no entry, and so is a
// block of a type this build does not know.
func (r *claudeReading) block() error {
	blk := &r.blk
	*blk = claudeBlockRead{words: blk.words}
	for i := range blk.words {
		blk.words[i] = blk.words[i][:0]
	}
	err := r.members(func(name string, k jsonscan.Kind) error {
		var err error
		switch name {
		case "type":
			blk.kind, err = r.wordValue(k)
		case "text":
			err = r.blockText(k)
		case "id":
			blk.id, err = r.wordValue(k)
		case "name":
			err = r.toolName(k)
		case "input":
			err = r.callInput(k)
		case "tool_use_id":
			blk.toolUseID, err = r.wordValue(k)
		case "is_error":
			blk.failed = k == jsonscan.True
			err = r.value(k)
		case "content":
			err = r.resultContent(k)
		default:
			err = r.value(k)
		}
		return err
	})
	if err != nil {
		return err
	}
	r.blockEnd()
	return nil
}

// blockText reads a block's `text`, its entries in each reading made as it
// ends; a value that is no string says nothing.
func (r *claudeReading) blockText(k jsonscan.Kind) error {
	blk := &r.blk
	for i := range blk.words {
		blk.words[i] = blk.words[i][:0]
	}
	if k != jsonscan.String {
		return r.value(k)
	}
	t := r.text.begin(r.b, r.entry(EntryModel), r.rec.shapesAny(r.b, claudeMessageKinds[:]...))
	if err := r.read(t); err != nil {
		return err
	}
	if err := r.own(t); err != nil {
		return err
	}
	r.prose(t, &blk.words)
	t.drop()
	return nil
}

// toolName reads a block's `name`, as an entry shows it ([shape]): its
// redaction's opening of [transcriptNameMaxBytes], streamed past the bound,
// where a redaction the stream refuses makes the call an [EntryUnread] (R6).
func (r *claudeReading) toolName(k jsonscan.Kind) error {
	blk := &r.blk
	blk.tool, blk.raw, blk.namePast, blk.toolRefused = "", "", false, false
	if k != jsonscan.String {
		return r.value(k)
	}
	t := r.name.begin(r.b, TranscriptEntry{}, false)
	if err := r.read(t); err != nil {
		return err
	}
	if err := r.own(t); err != nil {
		return err
	}
	if blk.namePast = t.past || len(t.held) > transcriptNameMaxBytes; !blk.namePast {
		blk.raw = t.text()
	}
	blk.tool, blk.toolRefused = t.shownName()
	return nil
}

// callInput reads a block's `input`, any value, as the call's entry shows it:
// its bytes as written and compacted ([compactJSON]) as the scan passes them,
// held to the bound and streamed past it, while its strings and names are read
// for their own words.
func (r *claudeReading) callInput(k jsonscan.Kind) error {
	blk := &r.blk
	blk.callRead = true
	shaped := r.rec.shapesAny(r.b, EntryToolCall, EntryToolCall, EntryToolCall)
	t := r.input.begin(r.b, r.entry(EntryToolCall), shaped)
	r.compact = jsonCompactor{w: t, buf: r.compact.buf}
	if err := r.scan.Tee(&r.compact); err != nil {
		return err
	}
	// A NUMBER'S TEXT ENDS ONLY AS IT IS PASSED, and the tee with it.
	err := r.value(k)
	if err == nil && k == jsonscan.Number {
		err = r.scan.Skip()
	}
	if err != nil {
		return err
	}
	defer t.drop()
	var shapedCall pendingEntry
	done := false
	for i := range r.rec.msg {
		s := &r.rec.msg[i]
		switch {
		case !s.shapes(r.b, EntryToolCall) || t.past && t.s == nil:
			blk.call[i] = r.b.held(r.entry(EntryToolCall), "", s.next())
			continue
		case done:
		case t.past:
			shapedCall, done = t.s.end(s.next()), true
		default:
			shapedCall, done = r.b.held(r.entry(EntryToolCall), t.text(), s.next()), true
		}
		blk.call[i] = shapedCall
	}
	return nil
}

// resultContent reads a block's `content`, a tool's result: a string, or a
// list of parts joined ([claudeAnswer]); a value of another kind answers
// nothing.
func (r *claudeReading) resultContent(k jsonscan.Kind) error {
	blk, c := &r.blk, &r.res
	blk.resultRead = true
	shaped := r.rec.shapesAny(r.b, EntryToolResult, EntryToolResult, EntryToolResult)
	c.start(shaped)
	var err error
	switch k {
	case jsonscan.String:
		t := r.text.begin(r.b, r.entry(EntryToolResult), shaped)
		if err = r.read(t); err != nil {
			return err
		}
		if err = r.own(t); err != nil {
			return err
		}
		c.whole(t)
	case jsonscan.ArrayStart:
		err = r.elements(func(k jsonscan.Kind) (bool, error) {
			if frameErr := r.frame(); frameErr != nil {
				return true, frameErr
			}
			if k != jsonscan.ObjectStart {
				return false, nil
			}
			return true, r.part()
		})
	default:
		err = r.value(k)
	}
	if err != nil {
		return err
	}
	for i := range r.rec.msg {
		blk.result[i] = c.entry(r.entry(EntryToolResult), &r.rec.msg[i])
	}
	r.text.drop()
	c.answer = lineScratch(c.answer)
	return nil
}

// part reads one part of a tool's result, its `type` and `text`, and joins it.
func (r *claudeReading) part() error {
	c := &r.res
	c.partStart()
	err := r.members(func(name string, k jsonscan.Kind) error {
		switch name {
		case "type":
			kind, err := r.wordValue(k)
			c.partType(kind)
			return err
		case "text":
			if k != jsonscan.String {
				c.partText(nil)
				return r.value(k)
			}
			t := r.text.begin(r.b, TranscriptEntry{Kind: EntryNote}, false)
			c.partText(t)
			if err := r.read(&c.partWriter); err != nil {
				return err
			}
			return r.own(t)
		}
		return r.value(k)
	})
	if err != nil {
		return err
	}
	c.partEnd()
	return nil
}

// blockEnd adds the entries the block's kind picks to each reading of the
// message: its words; its call, naming its tool; or its result, which names the
// tool its call was of as it commits.
func (r *claudeReading) blockEnd() {
	blk := &r.blk
	for i := range r.rec.msg {
		s := &r.rec.msg[i]
		switch blk.kind {
		case "text":
			s.addAll(r.b, blk.words[i])
		case "tool_use":
			p := blk.call[i]
			if !blk.callRead {
				p = r.b.held(r.entry(EntryToolCall), "", s.next())
			}
			ev := claudeEvent{kind: eventCall, id: blk.id, tool: blk.raw}
			if blk.namePast {
				ev.id = ""
			}
			switch {
			case blk.toolRefused:
				p = r.b.held(r.entry(EntryUnread), unredactedText, s.next())
				ev.id = ""
			case p.entry.Kind == EntryToolCall:
				p.entry.Tool = blk.tool
			}
			s.addEvent(r.b, p, time.Time{}, ev)
		case "tool_result":
			p := blk.result[i]
			if !blk.resultRead {
				p = r.b.held(r.entry(EntryToolResult), "", s.next())
			}
			if p.entry.Kind == EntryToolResult {
				p.entry.Failed = blk.failed
			}
			s.addEvent(r.b, p, time.Time{}, claudeEvent{kind: eventResult, id: blk.toolUseID})
		}
	}
}

// claudeAnswer is a tool's result as its block's `content` is read: a string
// whole, or its parts joined a line apart as [claudeResultText] joins them,
// held while joined they are within the bound and streamed past it. PAST THE
// BOUND A TEXT PART IS KEPT WHATEVER IT HOLDS (R2), where the part is written in
// order, its `type` once and then its `text` once: a result holding a part
// whose text runs past the bound written otherwise is said in its place, as
// its kind is not known before its words are.
type claudeAnswer struct {
	b      *transcriptBuilder
	shaped bool
	// answer is the parts joined, held, kept how many; text the result's
	// string; lost that a part ran past the bound out of order.
	answer []byte
	kept   int
	text   *valueText
	lost   bool
	// The part being read: its kind, its text, how often each was written.
	partKind                           string
	partTypes, partTexts               int
	part                               *valueText
	partPast, partStreaming, partBlank bool
	partWriter                         partWriter
	// stream is the result's text once it runs past the bound; wrote says it
	// holds an element, which the next follows a line apart; shaped its entry
	// once shaped, the same in every reading that shapes it.
	stream   *streamedEntry
	wrote    bool
	entryP   pendingEntry
	entrySet bool
}

// partWriter is where a part's text is read: held to the bound in its text,
// and past it streamed on into the result's where it can.
type partWriter struct{ c *claudeAnswer }

func (w *partWriter) Write(p []byte) (int, error) {
	w.c.writePart(p)
	return len(p), nil
}

// start starts a result, streamed past the bound where shaped.
func (c *claudeAnswer) start(shaped bool) {
	*c = claudeAnswer{b: c.b, shaped: shaped, answer: c.answer[:0]}
	c.partWriter.c = c
}

// whole takes t, the result's string, as its text.
func (c *claudeAnswer) whole(t *valueText) { c.text = t }

func (c *claudeAnswer) partStart() {
	c.partKind, c.partTypes, c.partTexts, c.part = "", 0, 0, nil
	c.partPast, c.partStreaming, c.partBlank = false, false, true
}

// partType keeps a part's kind. Written again after its text ran past the
// bound, the part is out of order.
func (c *claudeAnswer) partType(kind string) {
	c.partTypes++
	c.partKind = kind
	c.outOfOrder()
}

// partText starts a part's text, read into t; nil for a value that is no
// string. Written again after its text ran past the bound, the part is out of
// order.
func (c *claudeAnswer) partText(t *valueText) {
	c.partTexts++
	c.outOfOrder()
	c.part, c.partBlank = t, true
}

// outOfOrder loses the result where the part's text ran past the bound and the
// part is no longer written in order.
func (c *claudeAnswer) outOfOrder() {
	if c.partPast && (c.partTypes != 1 || c.partTexts != 1) {
		c.lost, c.partStreaming = true, false
	}
}

func (c *claudeAnswer) writePart(p []byte) {
	t := c.part
	c.partBlank = c.partBlank && blank(p)
	switch {
	case c.partStreaming:
		_, _ = c.stream.Write(p)
		return
	case c.partPast:
		return
	case len(t.held)+len(p) <= c.b.valueMax:
		_, _ = t.Write(p)
		return
	}
	c.partPast = true
	switch {
	case c.partTypes != 1 || c.partTexts != 1:
		c.lost = true
	case c.partKind == "text" && c.shaped && !c.lost:
		if c.stream == nil {
			c.open()
		}
		c.element(t.held)
		_, _ = c.stream.Write(p)
		c.partStreaming = true
	}
	t.past = true
	t.drop()
}

// open starts the result's stream, holding the parts joined so far.
func (c *claudeAnswer) open() {
	c.stream, c.wrote = c.b.streamed(TranscriptEntry{Kind: EntryToolResult, Thread: c.b.thread}), false
	if c.kept > 0 {
		c.element(c.answer)
	}
}

// element writes one element of the result's text to its stream, a line after
// the one before it.
func (c *claudeAnswer) element(p []byte) {
	if c.wrote {
		_, _ = c.stream.Write([]byte{'\n'})
	}
	_, _ = c.stream.Write(p)
	c.wrote = true
}

// partEnd joins the part just read, as [claudeResultText] does: a text part's
// words unless blank, past the bound whatever they hold; an image as
// [claudeImagePlaceholder]; nothing else. The parts joined past the bound
// stream where the result is shaped.
func (c *claudeAnswer) partEnd() {
	var line []byte
	switch {
	case c.partStreaming:
		c.partStreaming = false
		return
	case c.partKind == "image":
		line = []byte(claudeImagePlaceholder)
	case c.partPast, c.partKind != "text", c.part == nil, c.partBlank:
		return
	default:
		line = c.part.held
	}
	switch {
	case c.lost:
	case c.stream != nil:
		c.element(line)
	case len(c.answer)+min(c.kept, 1)+len(line) <= c.b.valueMax:
		if c.kept > 0 {
			c.answer = append(c.answer, '\n')
		}
		c.answer = append(c.answer, line...)
		c.kept++
	case c.shaped:
		c.open()
		c.element(line)
	}
}

// entry is the result's entry in s, at its next ordinal: an [EntryUnread]
// where a part ran past the bound out of order, which every filter keeps; one
// only counted or passed where s does not shape it; else its text, held or
// streamed, shaped once for every reading that shapes it.
func (c *claudeAnswer) entry(e TranscriptEntry, s *entryShape) pendingEntry {
	ordinal := s.next()
	switch {
	case c.lost:
		return c.b.held(TranscriptEntry{Kind: EntryUnread, Thread: e.Thread}, fmt.Sprintf("a tool's result in "+
			"the harness's record runs past %s in a part written out of order, and was not read",
			humanSize(int64(c.b.valueMax))), ordinal)
	case !s.shapes(c.b, EntryToolResult):
		return c.b.held(e, "", ordinal)
	case c.entrySet:
		return c.entryP
	case c.text != nil && c.text.past && c.text.s == nil:
		return c.b.held(e, "", ordinal)
	case c.text != nil && c.text.past:
		c.entryP = c.text.s.end(ordinal)
	case c.text != nil:
		c.entryP = c.b.held(e, c.text.text(), ordinal)
	case c.stream != nil:
		c.entryP = c.stream.end(ordinal)
	default:
		c.entryP = c.b.held(e, string(c.answer), ordinal)
	}
	c.entrySet = true
	return c.entryP
}

// transcript reads a Claude Code archive ([claudeArchiveFormat]).
func (ClaudeCode) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
	return b.claudeArchive(ctx, archive)
}

// claudeThread is whose transcript a member of the archive named name is: the
// session's own, `projects/crewlet/<session>.jsonl` (the empty thread), or a
// subagent's, `projects/crewlet/<session>/subagents/<name>.jsonl` (the thread
// named after its file) — and false for every other member.
func claudeThread(name string) (string, bool) {
	rest, ok := strings.CutPrefix(path.Clean(name), "projects/"+claudeProjectDir+"/")
	if !ok {
		return "", false
	}
	stem, ok := strings.CutSuffix(rest, ".jsonl")
	if !ok {
		return "", false
	}
	switch parts := strings.Split(stem, "/"); {
	case len(parts) == 1 && parts[0] != "":
		return "", true
	case len(parts) == 3 && parts[1] == "subagents" && parts[2] != "":
		return parts[2], true
	}
	return "", false
}

// hookLineKind is the kind of entry the line the run's own hook printed is: a
// wake where it opens with wakes ([hookWakeOne], a count and [hookWakesMany]),
// a late call where it names such calls alone ([hookCallOne], a count and
// [hookCallsMany]), and a note otherwise, as every line naming a note opens
// with it.
func hookLineKind(line string) EntryKind {
	count, rest, _ := strings.Cut(line, " ")
	counted := count != "" && strings.Trim(count, "0123456789") == ""
	switch {
	case strings.HasPrefix(line, hookWakeOne) || (counted && strings.HasPrefix(rest, hookWakesMany)):
		return EntryWake
	case strings.HasPrefix(line, hookCallOne) || (counted && strings.HasPrefix(rest, hookCallsMany)):
		return EntryLateCall
	}
	return EntryNote
}

// claudeTime is a record's `timestamp`, an RFC 3339 instant, or zero where it
// carries none this read can parse, or none an answer can carry
// ([carriedInstant]): the parser takes the year 0000 at any offset, which is a
// year before it in UTC.
func claudeTime(raw string) time.Time {
	at, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}
	}
	return carriedInstant(at.UTC())
}
