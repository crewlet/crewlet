package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/jsonscan"
)

// A PERSON'S TRANSCRIPT OF AN OPENCODE SESSION is a visitor of the one export
// walk ([scanExport]): it holds no message, and no value past the read's value
// bound, which it streams into the entry it makes ([valueText]). A message's
// kind can be written after its texts, so each reading of it (a person's, a
// step's, a compaction's) is shaped as it is read and kept provisional
// ([entryShape]) until the message ends and its kind picks one. The package
// doc's "A session's calls are read by what they were" has the walk.

// PAST THE VALUE BOUND A TEXT IS NEVER HELD, so what needs one whole is not done,
// in the walk and its oracle alike: R1, a text is one entry of its kind, a brief
// never split into notes; R2, a text part joined into one text, an answer's or a
// message's, is kept whatever it holds; R3, a call's input written as a string
// is shown as written, never re-read as JSON; R5, a call's result in a state not
// written in order is said in its place ([callResult]); R6, a text whose
// redaction the stream refuses is said in its place ([streamedEntry]). (R4 is a
// Claude Code system record's.)

// errReadLines ends a read once it has framed more than its bounds' ReadLines
// records and elements ([TranscriptReadLines]).
var errReadLines = errors.New("executor: the read framed more records and elements than one read reads")

// The names a step's and a part's `time` hold that a person reads, by their
// place in [openCodeStepNames] and [openCodePartTimes].
const (
	timeCreated   = 0
	timeRan       = 1
	timeCompleted = 2
)

// personAsks is what a person's transcript reads of an export: a message's
// kind, time, texts, status and content; an item's kind, text, name, time and
// state; a state's status, input, answer and failure; an answer's parts.
var personAsks = exportAsksOf(map[exportFrame]map[int]exportMeans{
	frameMessage: {msgType: meansWord, msgTime: meansFrame, msgText: meansStream, msgContent: meansFrame,
		msgStatus: meansWord, msgSummary: meansStream},
	frameStepTime: {timeCreated: meansCapture},
	frameItem: {itemType: meansWord, itemText: meansStream, itemName: meansStream, itemTime: meansFrame,
		itemState: meansFrame},
	framePartTime: {timeCreated: meansCapture, timeRan: meansCapture, timeCompleted: meansCapture},
	frameState: {stateStatus: meansWord, stateInput: meansStream, stateContent: meansFrame,
		stateError: meansFrame},
	frameAnswerPart: {partType: meansWord, partMime: meansStream, partText: meansStream},
	frameFailure:    {failureType: meansStream, failureMessage: meansStream},
}, false)

// openCodeExport reads the export document on the one export walk, a person's
// transcript its visitor ([openCodeReading]), the archive asked for ctx before
// each piece. A DOCUMENT THAT ENDS BEFORE IT CLOSES ENDS PART WAY THROUGH, and
// one the scanner refuses anywhere else is no export.
func (b *transcriptBuilder) openCodeExport(ctx context.Context, archive io.Reader) error {
	err := scanExport(ctx, &askingReader{ctx: ctx, r: archive}, newOpenCodeReading(b))
	fault, scanned := errors.AsType[*jsonscan.Error](err)
	switch {
	case errors.Is(err, errReadLines):
		return b.linesPast("export", "messages, items and parts")
	case !scanned:
		return err
	case fault.Fault == jsonscan.FaultCut, fault.Fault == jsonscan.FaultNoValue:
		return fmt.Errorf("%w: %w", io.ErrUnexpectedEOF, err)
	}
	return fmt.Errorf("%w: %w", errNotExport, err)
}

// linesPast says where a read of record past its bounds' ReadLines of what it
// frames ended, and ends it there: nothing of the record it ended inside is read.
func (b *transcriptBuilder) linesPast(record, what string) error {
	if err := b.unread(fmt.Sprintf("a part of the %s past the %d %s one read reads was not read, and "+
		"nothing after it could be", record, b.bounds.ReadLines, what)); err != nil {
		return err
	}
	b.cut = CutRead
	return errTranscriptPage
}

// askingReader is r asked for ctx before each read: a read of one long value
// streams it whole, and the walk asks only between messages and items.
type askingReader struct {
	ctx context.Context
	r   io.Reader
}

func (a *askingReader) Read(p []byte) (int, error) {
	if err := a.ctx.Err(); err != nil {
		return 0, err
	}
	return a.r.Read(p)
}

// entryShape is the entries one reading of a record makes, provisional until
// the record ends and its kind picks a reading ([transcriptBuilder.commitShape]):
// each shaped as the page would hold it, from base, the ordinal the record's
// first entry takes. Entries only counted are kept as a count, and none past
// the one the page would end at (full): a text a full reading would make is
// passed over unheld.
type entryShape struct {
	base, n         int
	entries         []shapeEntry
	kept, keptBytes int
	full            bool
}

// shapeEntry is an entry of a shape and its own instant, zero to take the
// record's, and what it does to the calls waiting on their results as it
// commits; or, where count is above zero, that many entries only counted.
type shapeEntry struct {
	p     pendingEntry
	at    time.Time
	count int
	ev    claudeEvent
}

// reset starts a reading of a record whose first entry takes the ordinal base.
func (s *entryShape) reset(base int) {
	*s = entryShape{base: base, entries: s.entries[:0]}
}

// next is the ordinal the shape's next entry takes.
func (s *entryShape) next() int { return s.base + s.n }

// shapes reports whether the shape's next entry, of kind, is shaped: the page
// holds it, as far as the shape can tell.
func (s *entryShape) shapes(b *transcriptBuilder, kind EntryKind) bool {
	return !s.full && b.shapes(kind, s.next())
}

// add puts p, the shape's next entry, dated at, in its place, as the page
// would take it ([transcriptBuilder.commit]).
func (s *entryShape) add(b *transcriptBuilder, p pendingEntry, at time.Time) {
	s.addEvent(b, p, at, claudeEvent{})
}

// addAll adds each of ps, dated by the record.
func (s *entryShape) addAll(b *transcriptBuilder, ps []pendingEntry) {
	for _, p := range ps {
		s.add(b, p, time.Time{})
	}
}

// addEvent is [entryShape.add] of an entry that does ev as it commits.
func (s *entryShape) addEvent(b *transcriptBuilder, p pendingEntry, at time.Time, ev claudeEvent) {
	if s.full {
		return
	}
	c := s.cursor()
	ordinal, kept := c.next, c.take(b, p)
	s.n, s.kept, s.keptBytes, s.full = c.next-s.base, c.kept, c.keptBytes, c.full
	if !kept && (ordinal < b.from || p.passed) {
		if last := len(s.entries) - 1; last >= 0 && s.entries[last].count > 0 && ev.kind == eventNone {
			s.entries[last].count++
			return
		}
		s.entries = append(s.entries, shapeEntry{count: 1, ev: ev})
		return
	}
	s.entries = append(s.entries, shapeEntry{p: p, at: at, ev: ev})
}

// cursor is where the shape's next entry goes.
func (s *entryShape) cursor() shapeCursor {
	return shapeCursor{next: s.next(), kept: s.kept, keptBytes: s.keptBytes, full: s.full}
}

// shapeCursor is a place in a shape, moved past each entry as the shape would
// take it ([entryShape.add]), without adding one: what a reader shapes a text
// at before it knows whether the record keeps it.
type shapeCursor struct {
	next, kept, keptBytes int
	full                  bool
}

// shapes reports whether the entry at the cursor, of kind, is shaped.
func (c *shapeCursor) shapes(b *transcriptBuilder, kind EntryKind) bool {
	return !c.full && b.shapes(kind, c.next)
}

// take moves the cursor past p, and reports whether the page would keep p:
// one before the page or passed over is only counted, and the first that
// would not fit fills the shape.
func (c *shapeCursor) take(b *transcriptBuilder, p pendingEntry) bool {
	if c.full {
		return false
	}
	ordinal := c.next
	c.next++
	if ordinal < b.from || p.passed {
		return false
	}
	if len(b.entries)+c.kept == b.bounds.Entries ||
		len(b.entries)+c.kept > 0 && b.bytes+c.keptBytes+len(p.entry.Text) > b.bounds.Bytes {
		c.full = true
		return true
	}
	c.kept++
	c.keptBytes += len(p.entry.Text)
	return true
}

// commitShape commits s's entries in order ([transcriptBuilder.commit]), each
// but an [EntryUnread] dated at where it has no instant of its own.
func (b *transcriptBuilder) commitShape(s *entryShape, at time.Time) error {
	for _, e := range s.entries {
		tool := b.happen(e.ev)
		if e.count > 0 {
			b.seen += e.count
			if b.seen > b.from {
				b.next = b.seen
			}
			continue
		}
		p := e.p
		if p.entry.Kind != EntryUnread {
			p.entry.At = firstInstant(e.at, at)
		}
		if e.ev.kind == eventResult && p.entry.Kind == EntryToolResult {
			p.entry.Tool, _, _ = entryOpening(tool, transcriptNameMaxBytes)
			p.entry.Tool = strings.Clone(p.entry.Tool)
			if tool == "" {
				p.entry.Tool = e.ev.tool
			}
		}
		if err := b.commit(p); err != nil {
			return err
		}
	}
	return nil
}

// proseInto adds to s the entries a text of words of kind makes, held whole
// ([transcriptBuilder.prose]): none for a blank one; a brief that opens with a
// heading, each note and late call it carries; else one entry.
func (b *transcriptBuilder) proseInto(s *entryShape, kind EntryKind, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	if kind == EntryBrief && opensWithHeading(text) {
		for heading, words := range headedIn(text) {
			if s.full {
				return
			}
			s.add(b, b.held(TranscriptEntry{Kind: heading}, words, s.next()), time.Time{})
		}
		return
	}
	s.add(b, b.held(TranscriptEntry{Kind: kind}, text, s.next()), time.Time{})
}

// valueText is one text of a record as the walk hands it, a piece at a time:
// held to the read's value bound and, past it, streamed into the entry it
// makes ([streamedEntry]) where that is shaped, or only judged blank where it
// is counted. Its writes never fail.
type valueText struct {
	b      *transcriptBuilder
	e      TranscriptEntry
	shaped bool
	held   []byte
	blank  bool
	past   bool
	s      *streamedEntry
}

// begin starts a text that past the bound makes e, streamed where shaped; a
// name, of no kind, is streamed for its opening alone ([valueText.shownName]).
func (t *valueText) begin(b *transcriptBuilder, e TranscriptEntry, shaped bool) *valueText {
	t.b, t.e, t.shaped, t.held, t.blank, t.past, t.s = b, e, shaped, t.held[:0], true, false, nil
	return t
}

func (t *valueText) Write(p []byte) (int, error) {
	t.blank = t.blank && blank(p)
	switch {
	case t.past:
		if t.s != nil {
			_, _ = t.s.Write(p)
		}
	case len(t.held)+len(p) <= t.b.valueMax:
		t.held = append(t.held, p...)
	default:
		t.past = true
		switch {
		case t.shaped:
			t.s = t.b.streamed(t.e)
		case t.e.Kind == "":
			t.s = t.b.streamedOpening()
		}
		if t.s != nil {
			_, _ = t.s.Write(t.held)
			_, _ = t.s.Write(p)
		}
		t.drop()
	}
	return len(p), nil
}

// text is the text held, valid until the next begin.
func (t *valueText) text() string { return string(t.held) }

// drop lets go of what the text held, keeping a buffer of an ordinary text's
// size for the next.
func (t *valueText) drop() { t.held = lineScratch(t.held) }

// end is the one entry a text past the bound makes at ordinal (R1): none for
// a blank one where blank drops it, its stream's where shaped, and one only
// counted otherwise.
func (t *valueText) end(ordinal int, dropsBlank bool) (pendingEntry, bool) {
	switch {
	case dropsBlank && t.blank:
		return pendingEntry{}, false
	case t.s != nil:
		return t.s.end(ordinal), true
	}
	return t.b.held(t.e, "", ordinal), true
}

// openCodeReading is a person's transcript as the export walk's visitor: the
// message it is in, each of its readings provisional ([entryShape]); the item
// and the call's result it is in; and the texts the walk streams to it.
type openCodeReading struct {
	b *transcriptBuilder
	// framed is how many records and elements the read has framed.
	framed int
	msg    readingMessage
	item   readingItem
	result callResult
	// text is the message's or the item's words being read, name the item's
	// tool's, input its call's.
	text, name, input valueText
	compact           jsonCompactor
}

// readingMessage is what a reading keeps of the message it is in: its kind,
// when it was written, a compaction's status, and its three readings, a
// person's (`text`), a step's (`content`) and a compaction's (`summary`).
type readingMessage struct {
	kind, status        string
	created             int64
	user, step, summary entryShape
}

// readingItem is what a reading keeps of the step's item it is in: its kind,
// its words' entry, its tool's name as shown, its call's entry, and its times.
type readingItem struct {
	kind                  string
	prose                 pendingEntry
	said                  bool
	tool                  string
	toolRefused, callRead bool
	call                  pendingEntry
	times                 [3]int64
}

// newOpenCodeReading is a person's reading into b.
func newOpenCodeReading(b *transcriptBuilder) *openCodeReading {
	r := &openCodeReading{b: b}
	r.result.b = b
	return r
}

func (*openCodeReading) asks() *exportAsks { return personAsks }

func (*openCodeReading) digest() *textDigest { return nil }

// frame counts one record or element the read frames, ending the read past
// [TranscriptReadLines].
func (r *openCodeReading) frame() error {
	if r.framed++; r.framed > r.b.bounds.ReadLines {
		return errReadLines
	}
	return nil
}

// open starts a message, an item, a call's state and what it holds, each
// written again replacing what it was.
func (r *openCodeReading) open(f exportFrame) error {
	m := &r.msg
	switch f {
	case frameMessage:
		if err := r.frame(); err != nil {
			return err
		}
		m.kind, m.status, m.created = "", "", 0
		m.user.reset(r.b.seen)
		m.step.reset(r.b.seen)
		m.summary.reset(r.b.seen)
	case frameStepTime:
		m.created = 0
	case frameContent:
		m.step.reset(r.b.seen)
	case frameItem:
		if err := r.frame(); err != nil {
			return err
		}
		r.item = readingItem{}
		r.stateStart()
	case framePartTime:
		r.item.times = [3]int64{}
	case frameState:
		r.stateStart()
	case frameAnswer:
		r.result.answerStart()
	case frameAnswerPart:
		if err := r.frame(); err != nil {
			return err
		}
		r.result.partStart()
	case frameFailure:
		r.result.failureStart()
	}
	return nil
}

// stateStart starts the item's call over: its input none, and its result
// shaped where the step's entry after the call's is.
func (r *openCodeReading) stateStart() {
	r.item.callRead = false
	r.item.call = pendingEntry{}
	// SHAPED WHATEVER KINDS THE FILTER KEEPS: a result past the bound in a
	// state out of order is an [EntryUnread] in its place, which every filter
	// keeps (R5).
	step := &r.msg.step
	ordinal := step.next() + 1
	r.result.start(ordinal, !step.full && ordinal >= r.b.from, r.b.filter.keepsKind(EntryToolResult))
}

// sink is where each streamed text goes: a message's or an item's words, the
// item's tool's name, its call's input, and an answer's or a failure's words.
func (r *openCodeReading) sink(f exportFrame, i int, k jsonscan.Kind) io.Writer {
	m := &r.msg
	switch {
	case f == frameState && i == stateInput:
		r.item.callRead = false
		shaped := m.step.shapes(r.b, EntryToolCall)
		switch {
		case !shaped, k == jsonscan.Null:
			return nil
		case k == jsonscan.String:
			return r.input.begin(r.b, TranscriptEntry{Kind: EntryToolCall}, true)
		}
		r.compact = jsonCompactor{w: r.input.begin(r.b, TranscriptEntry{Kind: EntryToolCall}, true)}
		return &r.compact
	case f == frameAnswerPart:
		return r.result.partSink(i, k)
	case f == frameFailure:
		return r.result.failureSink(i, k)
	case k != jsonscan.String:
		return nil
	case f == frameMessage && i == msgText:
		m.user.reset(r.b.seen)
		return r.text.begin(r.b, TranscriptEntry{Kind: EntryBrief}, m.user.shapes(r.b, EntryBrief))
	case f == frameMessage && i == msgSummary:
		m.summary.reset(r.b.seen)
		return r.text.begin(r.b, TranscriptEntry{Kind: EntrySummary}, m.summary.shapes(r.b, EntrySummary))
	case f == frameItem && i == itemText:
		return r.text.begin(r.b, TranscriptEntry{Kind: EntryModel}, m.step.shapes(r.b, EntryModel))
	case f == frameItem && i == itemName:
		return r.name.begin(r.b, TranscriptEntry{}, false)
	}
	return nil
}

// member keeps a member as read, a key written twice by its last value.
func (r *openCodeReading) member(f exportFrame, i int, v exportValue) error {
	m, it := &r.msg, &r.item
	switch f {
	case frameMessage:
		r.messageMember(i, v)
	case frameStepTime:
		m.created = looseInt(v.text)
	case frameItem:
		r.itemMember(i, v)
	case framePartTime:
		it.times[i] = looseInt(v.text)
	case frameState:
		r.stateMember(i, v)
	case frameAnswerPart:
		r.result.partMember(i, v)
	}
	return nil
}

// messageMember keeps a message's kind, time, status and readings.
func (r *openCodeReading) messageMember(i int, v exportValue) {
	m := &r.msg
	switch i {
	case msgType:
		m.kind = wordOf(v)
	case msgStatus:
		m.status = wordOf(v)
	case msgTime:
		if v.kind != jsonscan.ObjectStart {
			m.created = 0
		}
	case msgText:
		r.prose(&m.user, EntryBrief, v)
	case msgSummary:
		r.prose(&m.summary, EntrySummary, v)
	case msgContent:
		switch v.kind {
		case jsonscan.ArrayStart:
		case jsonscan.Null:
			m.step.reset(r.b.seen)
		default:
			m.step.reset(r.b.seen)
			m.step.add(r.b, r.b.held(TranscriptEntry{Kind: EntryUnread},
				"a message of the export whose content is not a list was not read", m.step.next()), time.Time{})
		}
	}
}

// prose adds the entries the words just read make to s ([transcriptBuilder.proseInto]):
// past the bound, the one entry of kind they make (R1); a value that is no
// string, none.
func (r *openCodeReading) prose(s *entryShape, kind EntryKind, v exportValue) {
	if v.kind != jsonscan.String {
		s.reset(r.b.seen)
		return
	}
	if !r.text.past {
		r.b.proseInto(s, kind, r.text.text())
	} else if p, ok := r.text.end(s.next(), true); ok {
		s.add(r.b, p, time.Time{})
	}
	r.text.drop()
}

// itemMember keeps an item's kind, its words, its tool's name and its state.
func (r *openCodeReading) itemMember(i int, v exportValue) {
	it := &r.item
	switch i {
	case itemType:
		it.kind = wordOf(v)
	case itemText:
		it.said = false
		if v.kind != jsonscan.String {
			return
		}
		if !r.text.past {
			if text := r.text.text(); strings.TrimSpace(text) != "" {
				it.prose, it.said = r.b.held(TranscriptEntry{Kind: EntryModel}, text, r.msg.step.next()), true
			}
		} else {
			it.prose, it.said = r.text.end(r.msg.step.next(), true)
		}
		r.text.drop()
	case itemName:
		it.tool, it.toolRefused = "", false
		if v.kind == jsonscan.String {
			it.tool, it.toolRefused = r.name.shownName()
		}
	case itemTime:
		if v.kind != jsonscan.ObjectStart {
			it.times = [3]int64{}
		}
	case itemState:
		if v.kind != jsonscan.ObjectStart {
			r.stateStart()
		}
	}
}

// stateMember keeps a call's status, its input's entry, and what its answer
// and failure leave of its result.
func (r *openCodeReading) stateMember(i int, v exportValue) {
	switch i {
	case stateStatus:
		r.result.statusMember(wordOf(v))
	case stateInput:
		r.item.call, r.item.callRead = r.inputEntry(v), true
	case stateContent:
		r.result.contentMember(v.kind)
	case stateError:
		r.result.failureEnd(v.kind)
	}
}

// inputEntry is the call's entry its input just read makes: a string as the
// text the model had written, JSON where it parses whole within the bound
// ([openCodeCallInput]) and as written past it (R3); any other value as
// written and compacted ([compactJSON]); null, no input.
func (r *openCodeReading) inputEntry(v exportValue) pendingEntry {
	e, ordinal := TranscriptEntry{Kind: EntryToolCall}, r.msg.step.next()
	defer r.input.drop()
	switch {
	case !r.msg.step.shapes(r.b, EntryToolCall) || v.kind == jsonscan.Null:
		return r.b.held(e, "", ordinal)
	case r.input.past:
		p, _ := r.input.end(ordinal, false)
		return p
	case v.kind == jsonscan.String:
		return r.b.held(e, compactJSON(json.RawMessage(r.input.text())), ordinal)
	}
	return r.b.held(e, r.input.text(), ordinal)
}

// element is an element of a list that is no object: a message that is none
// is said; an item or a part that is none is the zero one, which says nothing.
func (r *openCodeReading) element(f exportFrame, _ jsonscan.Kind) error {
	if f != frameMessages && f != frameContent && f != frameAnswer {
		return nil
	}
	if err := r.frame(); err != nil {
		return err
	}
	if f == frameMessages {
		return r.b.unread("a message of the export is not a message, and was not read")
	}
	return nil
}

// close ends a message, which commits the reading its kind picks; an item,
// whose entries its kind picks; a state, which settles the call's result; and
// an answer's part, which joins it.
func (r *openCodeReading) close(f exportFrame, _ exportValue) error {
	switch f {
	case frameMessage:
		return r.messageEnd()
	case frameItem:
		r.itemEnd()
	case frameState:
		r.result.end()
	case frameAnswerPart:
		r.result.partEnd()
	}
	return nil
}

// messageEnd commits the reading the message's kind picks
// ([transcriptBuilder.openCodeMessage] in the decode walk): a person's, a
// step's, a completed compaction's; none for OpenCode's own kinds; and, for a
// kind this read does not know, says so.
func (r *openCodeReading) messageEnd() error {
	m := &r.msg
	at := unixMillis(m.created)
	switch openCodeKind(m.kind) {
	case openCodeUser:
		return r.b.commitShape(&m.user, at)
	case openCodeAssistant:
		return r.b.commitShape(&m.step, at)
	case openCodeCompaction:
		if m.status != openCodeCompleted {
			return nil
		}
		return r.b.commitShape(&m.summary, at)
	case openCodeSynthetic, openCodeSystem, openCodeSkill, openCodeShell, openCodeIdle,
		openCodeAgentSwitched, openCodeModelSwitched, openCodeLocationSwitched:
		return nil
	}
	return r.b.unread("a message of the export of no kind this read knows was not read")
}

// itemEnd adds the entries the item's kind picks to the step: its words, or
// its call (dated when it ran, or when the model began it) and, once it
// settled, its result (dated when it did), each naming its tool.
func (r *openCodeReading) itemEnd() {
	it, step := &r.item, &r.msg.step
	switch it.kind {
	case "text":
		if it.said {
			step.add(r.b, it.prose, time.Time{})
		}
	case "tool":
		call := it.call
		if !it.callRead {
			call = r.b.held(TranscriptEntry{Kind: EntryToolCall}, "", step.next())
		}
		result, settled := r.result.entry()
		ran := firstInstant(unixMillis(it.times[timeRan]), unixMillis(it.times[timeCreated]))
		step.add(r.b, r.named(call, step.next()), ran)
		if settled {
			step.add(r.b, r.named(result, step.next()), unixMillis(it.times[timeCompleted]))
		}
	}
	r.result.drop()
}

// named is p, the entry at ordinal, naming the item's tool; or, where the
// stream refused the name's redaction, an [EntryUnread] in its place (R6).
func (r *openCodeReading) named(p pendingEntry, ordinal int) pendingEntry {
	switch {
	case r.item.toolRefused:
		return r.b.held(TranscriptEntry{Kind: EntryUnread}, unredactedText, ordinal)
	case p.entry.Kind != EntryUnread:
		p.entry.Tool = r.item.tool
	}
	return p
}

// wordOf is a word read, "" where the value is no string or runs past what a
// word holds, which equals no word a reader compares.
func wordOf(v exportValue) string {
	if v.kind != jsonscan.String || v.past {
		return ""
	}
	return string(v.text)
}

// shownName is the name just read as an entry shows it ([shape]): its
// redaction's opening of [transcriptNameMaxBytes], and whether the stream
// refused a name past the bound (R6). A name is streamed past the bound
// whatever its entries are: it is read before what makes them.
func (t *valueText) shownName() (string, bool) {
	defer t.drop()
	if !t.past {
		shown, _, _ := entryOpening(t.text(), transcriptNameMaxBytes)
		return strings.Clone(shown), false
	}
	shown, err := t.s.opening(transcriptNameMaxBytes)
	return shown, err != nil
}

// jsonCompactor writes a JSON value's bytes as written to w without the white
// space outside its strings: byte for byte what [compactJSON] makes of a value
// the scanner read whole. Its writes never fail.
type jsonCompactor struct {
	w                 io.Writer
	inString, escaped bool
	buf               []byte
}

func (c *jsonCompactor) Write(p []byte) (int, error) {
	out := c.buf[:0]
	for _, ch := range p {
		switch {
		case c.inString:
			out = append(out, ch)
			switch {
			case c.escaped:
				c.escaped = false
			case ch == '\\':
				c.escaped = true
			case ch == '"':
				c.inString = false
			}
		case ch == ' ', ch == '\t', ch == '\n', ch == '\r':
		default:
			out = append(out, ch)
			c.inString = ch == '"'
		}
	}
	c.buf = out
	_, _ = c.w.Write(out)
	return len(p), nil
}

// THE RESULT OF A CALL PAST THE BOUND is written in the order its text is
// shown, its failure's words before its answer, so a stream can take it only
// where the state names its form before its words run past the bound, as
// OpenCode writes one: `status` before `error` and `content`, each written
// once, the failure before the answer, the failure's `type` and `message` once
// each, and each answer part's `type` before its `text`, its `type`, `text`
// and `mime` once each. Its text is the failure's words and the answer's parts
// joined: within the bound it is composed held, whatever the order; past it,
// a text of its own or joined, it streams in order or is said (R5).

// callResult is a call's result as its state is read: the failure's words and
// the answer's parts, held while joined they are within the bound, as
// [openCodeFailure] and [openCodeAnswer] join them, and past it streamed where
// the state is written in order, else said in its place (R5).
type callResult struct {
	b       *transcriptBuilder
	ordinal int
	// shaped says the result is on the page, as far as the step can tell;
	// ofKept that the filter keeps its kind.
	shaped, ofKept bool
	status         string
	// statuses, failures and contents count the state's members read;
	// canonical says the state is written in order so far, spilled that a
	// text of the result ran past the bound.
	statuses, failures, contents int
	canonical, spilled           bool
	// The failure: its message and its type, held, each read, past the bound;
	// messages and types count its members.
	message, kind                     []byte
	messagePast, kindPast             bool
	messages, types, messageLen       int
	streamingMessage, messageStreamed bool
	// The answer: its parts joined, held, and how many; lost where it ran past
	// the bound with no stream to take it.
	answer []byte
	kept   int
	lost   bool
	// The part being read: its kind, its mime's opening, its text held.
	part                               []byte
	partKind                           string
	mime                               []byte
	partBlank, partPast, partStreaming bool
	partTypes, partTexts, partMimes    int
	// stream is the result's text once it runs past the bound; wrote says it
	// holds an element, which the next follows a line apart.
	stream *streamedEntry
	wrote  bool
}

// start starts a state whose result takes ordinal, shaped where the page holds
// it and kept where the filter keeps its kind.
func (c *callResult) start(ordinal int, shaped, ofKept bool) {
	c.drop()
	*c = callResult{b: c.b, ordinal: ordinal, shaped: shaped, ofKept: ofKept, canonical: true, message: c.message,
		kind: c.kind, answer: c.answer, part: c.part, mime: c.mime[:0]}
}

// drop lets go of what the result held once its entry is made, keeping the
// buffers of an ordinary text's size for the next ([lineScratch]): A READ HOLDS
// ONE RESULT'S TEXTS AT A TIME, never what an earlier one grew them to.
func (c *callResult) drop() {
	c.message, c.kind, c.answer, c.part = lineScratch(c.message), lineScratch(c.kind), lineScratch(c.answer),
		lineScratch(c.part)
}

// order notes a member read out of the order a stream takes: the state's
// result can no longer stream, and a stream it opened is let go, with the
// member it was taking.
func (c *callResult) order(ok bool) {
	if !ok {
		c.canonical, c.stream, c.streamingMessage, c.partStreaming = false, nil, false, false
	}
}

// due reports whether the call settled, with its answer or on a failure.
func (c *callResult) due() bool {
	return c.status == openCodeCompleted || c.status == openCodeCallFailed
}

// said is the failure's words as held ([openCodeFailure]): its message, or its
// type where it says nothing more.
func (c *callResult) said() []byte {
	if c.messageLen > 0 {
		return c.message
	}
	return c.kind
}

// saysFailure reports whether the failure says anything: a message, or a type.
func (c *callResult) saysFailure() bool { return c.messageLen > 0 || c.kindPast || len(c.kind) > 0 }

// saidLost reports whether the failure's words ran past the bound with no
// stream to take them.
func (c *callResult) saidLost() bool {
	if c.messageLen > 0 {
		return c.messagePast && !c.messageStreamed
	}
	return c.kindPast
}

func (c *callResult) statusMember(status string) {
	c.statuses++
	c.order(c.statuses == 1 && c.failures == 0 && c.contents == 0)
	c.status = status
}

func (c *callResult) failureStart() {
	c.failures++
	c.order(c.failures == 1 && c.statuses > 0 && c.contents == 0)
	c.message, c.kind, c.messageLen = c.message[:0], c.kind[:0], 0
	c.messagePast, c.kindPast, c.messages, c.types = false, false, 0, 0
	c.streamingMessage, c.messageStreamed = false, false
}

// failureEnd is the state's `error` read: a failure that is no object says
// nothing.
func (c *callResult) failureEnd(k jsonscan.Kind) {
	if k != jsonscan.ObjectStart {
		c.failureStart()
	}
	c.streamingMessage = false
}

// failureSink is where a failure's type or message is read: a member written
// again starts over, ending what the stream took of the one before it.
func (c *callResult) failureSink(i int, k jsonscan.Kind) io.Writer {
	if i == failureMessage {
		c.messages++
		c.order(c.messages == 1)
		c.message, c.messageLen, c.messagePast = c.message[:0], 0, false
		c.streamingMessage, c.messageStreamed = false, false
	} else {
		c.types++
		c.order(c.types == 1)
		c.kind, c.kindPast = c.kind[:0], false
	}
	if k != jsonscan.String || !c.shaped {
		return nil
	}
	if i == failureMessage {
		return writerFunc(c.writeMessage)
	}
	return writerFunc(c.writeKind)
}

func (c *callResult) writeKind(p []byte) {
	switch {
	case c.kindPast:
	case len(c.kind)+len(p) <= c.b.valueMax:
		c.kind = append(c.kind, p...)
	default:
		c.spilled, c.kindPast, c.kind = true, true, c.kind[:0]
	}
}

func (c *callResult) writeMessage(p []byte) {
	c.messageLen += len(p)
	switch {
	case c.streamingMessage:
		_, _ = c.stream.Write(p)
	case c.messagePast:
	case len(c.message)+len(p) <= c.b.valueMax:
		c.message = append(c.message, p...)
	default:
		c.spilled, c.messagePast = true, true
		if c.canonical && c.status == openCodeCallFailed {
			c.open()
			c.element(c.message)
			_, _ = c.stream.Write(p)
			c.streamingMessage, c.messageStreamed = true, true
		}
		c.message = c.message[:0]
	}
}

// open opens the result's stream, its text so far written in the order it is
// shown: the failure's words, unless the stream opens on them, and the
// answer's parts.
func (c *callResult) open() {
	c.stream, c.wrote = c.b.streamedOpening(), false
	if c.ofKept {
		c.stream = c.b.streamed(TranscriptEntry{Kind: EntryToolResult})
	}
	if c.status == openCodeCallFailed && !c.messagePast && c.saysFailure() {
		c.element(c.said())
	}
	if c.kept > 0 {
		c.element(c.answer)
	}
}

// element writes one element of the result's text to its stream, a line after
// the one before it.
func (c *callResult) element(p []byte) {
	if c.wrote {
		_, _ = c.stream.Write([]byte{'\n'})
	}
	_, _ = c.stream.Write(p)
	c.wrote = true
}

// streams reports whether the result's text, run past the bound, can stream:
// it is shaped, in order, settled, and its failure's words are not lost.
func (c *callResult) streams() bool {
	return c.shaped && c.canonical && c.due() &&
		(c.status != openCodeCallFailed || !c.saysFailure() || !c.saidLost())
}

func (c *callResult) answerStart() {
	c.contents++
	c.order(c.contents == 1 && c.statuses > 0)
	c.answer, c.kept, c.lost = c.answer[:0], 0, false
}

// contentMember ends the state's `content`: one that is no list answers
// nothing.
func (c *callResult) contentMember(k jsonscan.Kind) {
	if k != jsonscan.ArrayStart {
		c.answerStart()
	}
}

func (c *callResult) partStart() {
	c.part, c.partKind, c.mime = c.part[:0], "", c.mime[:0]
	c.partBlank, c.partPast, c.partStreaming = true, false, false
	c.partTypes, c.partTexts, c.partMimes = 0, 0, 0
}

// partSink is where a part's mime or text is read.
func (c *callResult) partSink(i int, k jsonscan.Kind) io.Writer {
	switch i {
	case partMime:
		c.partMimes++
		c.order(c.partMimes == 1)
		c.mime = c.mime[:0]
		if k == jsonscan.String && c.shaped {
			return writerFunc(c.writeMime)
		}
	case partText:
		c.partTexts++
		c.order(c.partTexts == 1 && c.partTypes == 1)
		c.part, c.partBlank, c.partPast, c.partStreaming = c.part[:0], true, false, false
		if k == jsonscan.String && c.shaped {
			return writerFunc(c.writePart)
		}
	}
	return nil
}

// partMember keeps a part's kind.
func (c *callResult) partMember(i int, v exportValue) {
	if i == partType {
		c.partTypes++
		c.order(c.partTypes == 1)
		c.partKind = wordOf(v)
	}
}

func (c *callResult) writeMime(p []byte) {
	if keep := len("image/") - len(c.mime); keep > 0 {
		c.mime = append(c.mime, p[:min(len(p), keep)]...)
	}
}

func (c *callResult) writePart(p []byte) {
	c.partBlank = c.partBlank && blank(p)
	switch {
	case c.partStreaming:
		_, _ = c.stream.Write(p)
	case c.partPast:
	case len(c.part)+len(p) <= c.b.valueMax:
		c.part = append(c.part, p...)
	default:
		// PAST THE BOUND A TEXT PART IS KEPT WHATEVER IT HOLDS (R2): the line
		// before it is written before its words are known not to be blank.
		c.spilled, c.partPast = true, true
		if c.partKind == "text" && c.streams() {
			if c.stream == nil {
				c.open()
			}
			c.element(c.part)
			_, _ = c.stream.Write(p)
			c.partStreaming = true
		} else if c.partKind == "text" {
			c.lost = true
		}
		c.part = c.part[:0]
	}
}

// joined is how long the result's text held so far runs with line joined
// after it: the failure's words, where the call failed, then the answer's
// parts, each a line after the one before it.
func (c *callResult) joined(line string) int {
	n, sep := len(c.answer), min(c.kept, 1)
	if c.status == openCodeCallFailed && c.saysFailure() {
		n, sep = n+len(c.said())+sep, 1
	}
	return n + sep + len(line)
}

// partEnd joins the part just read, as [openCodeAnswer] does: a text part's
// words unless blank, a file as the words it reads in, nothing else. Once the
// joined text would run past the bound it streams: written in order, the
// failure is read before the answer, so it is known here.
func (c *callResult) partEnd() {
	var line string
	switch {
	case c.partKind == "file":
		line = openCodeFileText(string(c.mime))
	case c.partKind != "text", c.partStreaming, c.partPast, c.partBlank:
		return
	default:
		line = string(c.part)
	}
	switch {
	case c.lost:
	case c.stream != nil:
		c.element([]byte(line))
	case c.joined(line) <= c.b.valueMax:
		if c.kept > 0 {
			c.answer = append(c.answer, '\n')
		}
		c.answer = append(c.answer, line...)
		c.kept++
	default:
		c.spilled = true
		if !c.streams() {
			c.lost = true
			return
		}
		c.open()
		c.element([]byte(line))
	}
}

// entry is the call's result once its state is read, and whether it settled:
// its answer, or its failure's words and then its answer; and an [EntryUnread]
// where its text ran past the bound in a state not written in order (R5),
// which, out of order, can be known only once its failure is read.
func (c *callResult) entry() (pendingEntry, bool) {
	if !c.due() {
		return pendingEntry{}, false
	}
	e := TranscriptEntry{Kind: EntryToolResult, Failed: c.status == openCodeCallFailed}
	failure := e.Failed && c.saysFailure()
	if c.stream == nil && failure && c.kept > 0 && len(c.said())+1+len(c.answer) > c.b.valueMax {
		c.spilled = true
	}
	switch {
	case !c.shaped:
		return c.b.held(e, "", c.ordinal), true
	case c.spilled && !c.canonical, failure && c.saidLost(), c.lost:
		return c.b.held(TranscriptEntry{Kind: EntryUnread}, fmt.Sprintf("a call's result in the export runs "+
			"past %s before what opens it is written, and was not read", humanSize(int64(c.b.valueMax))),
			c.ordinal), true
	case !c.ofKept:
		return c.b.held(e, "", c.ordinal), true
	case c.stream != nil:
		p := c.stream.end(c.ordinal)
		if p.entry.Kind == EntryToolResult {
			p.entry.Failed = e.Failed
		}
		return p, true
	}
	if !failure {
		return c.b.held(e, string(c.answer), c.ordinal), true
	}
	var text strings.Builder
	text.Grow(len(c.said()) + 1 + len(c.answer))
	text.Write(c.said())
	if c.kept > 0 {
		text.WriteByte('\n')
		text.Write(c.answer)
	}
	return c.b.held(e, text.String(), c.ordinal), true
}

// end ends the state: nothing more is written to its result.
func (c *callResult) end() {
	c.streamingMessage, c.partStreaming = false, false
}

// writerFunc is a function as an [io.Writer] whose writes never fail.
type writerFunc func(p []byte)

func (f writerFunc) Write(p []byte) (int, error) {
	f(p)
	return len(p), nil
}
