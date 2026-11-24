package executor

import (
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/jsonscan"
)

// readText is the one entry a text a record's member holds makes at the
// record's first ordinal, shaped as the member ends ([lineWalk.stringText]):
// set once the member is read, blank where its text is white space alone.
type readText struct {
	p          pendingEntry
	set, blank bool
}

// entry is the text's entry of kind at ordinal, an empty text's where the
// member was not read.
func (t readText) entry(b *transcriptBuilder, kind EntryKind, ordinal int) pendingEntry {
	if t.set {
		return t.p
	}
	return b.held(TranscriptEntry{Kind: kind}, "", ordinal)
}

// stringText reads the string value k begins into t as the one entry e it
// makes at the line's first ordinal, an empty one for null, and reports
// whether a string field takes the value, passing over one it does not.
func (w *lineWalk) stringText(t *valueText, e TranscriptEntry, k jsonscan.Kind) (readText, bool, error) {
	ordinal := w.b.seen
	switch k {
	case jsonscan.Null:
		return readText{p: w.b.held(e, "", ordinal), set: true, blank: true}, true, nil
	case jsonscan.String:
	default:
		return readText{}, false, w.scan.Skip()
	}
	t.begin(w.b, e, w.b.shapes(e.Kind, ordinal))
	if err := w.read(t); err != nil {
		t.drop()
		return readText{}, true, err
	}
	blank := t.blank
	return readText{p: w.textEntry(t, e, ordinal), set: true, blank: blank}, true, nil
}

// rawText reads the value the current token begins, any value, into t as
// written and compacted ([compactJSON]) as the scan passes it, as the one
// entry e it makes at the line's first ordinal.
func (w *lineWalk) rawText(t *valueText, c *jsonCompactor, e TranscriptEntry) (readText, error) {
	ordinal := w.b.seen
	t.begin(w.b, e, w.b.shapes(e.Kind, ordinal))
	*c = jsonCompactor{w: t, buf: c.buf}
	if err := w.scan.Tee(c); err != nil {
		t.drop()
		return readText{}, err
	}
	if err := w.scan.Skip(); err != nil {
		t.drop()
		return readText{}, err
	}
	blank := t.blank
	return readText{p: w.textEntry(t, e, ordinal), set: true, blank: blank}, nil
}

// textEntry is the one entry t, a text just read, makes as e at ordinal: its
// stream's past the bound where shaped, one only counted past it where not,
// else its text held. t is let go of.
func (w *lineWalk) textEntry(t *valueText, e TranscriptEntry, ordinal int) pendingEntry {
	defer t.drop()
	switch {
	case t.past && t.s != nil:
		return t.s.end(ordinal)
	case t.past:
		return w.b.held(e, "", ordinal)
	}
	return w.b.held(e, t.text(), ordinal)
}

// joinPart reads one element of a list j joins: an object whose `type` (none
// for a rule of any kind) and `text` it joins and whose other members more
// reads (nil for none), or null. It reports whether the element's struct
// takes it.
func (w *lineWalk) joinPart(j *textJoin, k jsonscan.Kind,
	more func(name string, k jsonscan.Kind) (bool, error)) (bool, error) {
	j.partStart()
	took := true
	ok, err := w.objectField(k, func(name string, k jsonscan.Kind) error {
		switch {
		case name == "type" && !j.rule.anyKind:
			kind, ok, err := w.wordField(k)
			j.partType(kind)
			took = took && ok
			return err
		case name == "text":
			j.partText()
			switch k {
			case jsonscan.String:
				return w.read(j)
			case jsonscan.Null:
				return nil
			}
			took = false
			return w.scan.Skip()
		}
		if more == nil {
			return w.scan.Skip()
		}
		ok, err := more(name, k)
		took = took && ok
		return err
	})
	j.partEnd()
	return ok && took, err
}

// toolName is a call's tool as its entry shows it ([shape]), its redaction's
// opening of [transcriptNameMaxBytes] streamed past the value bound, and as
// written within that bound (raw) for its result; past says it was longer,
// and refused that the stream refused its redaction (R6).
type toolName struct {
	shown, raw    string
	past, refused bool
}

// toolName reads the string value k begins, a tool's name, through t, and
// reports whether a string field takes it.
func (w *lineWalk) toolName(t *valueText, k jsonscan.Kind) (toolName, bool, error) {
	switch k {
	case jsonscan.Null:
		return toolName{}, true, nil
	case jsonscan.String:
	default:
		return toolName{}, false, w.scan.Skip()
	}
	t.begin(w.b, TranscriptEntry{}, false)
	if err := w.read(t); err != nil {
		t.drop()
		return toolName{}, true, err
	}
	var n toolName
	if n.past = t.past || len(t.held) > transcriptNameMaxBytes; !n.past {
		n.raw = t.text()
	}
	n.shown, n.refused = t.shownName()
	return n, true, nil
}

// heldToolName is name, a tool's held whole, as [lineWalk.toolName] reads one
// within the value bound.
func heldToolName(name string) toolName {
	n := toolName{past: len(name) > transcriptNameMaxBytes}
	if !n.past {
		n.raw = name
	}
	shown, _, _ := entryOpening(name, transcriptNameMaxBytes)
	n.shown = strings.Clone(shown)
	return n
}

// call is p, the entry of a call of the tool at ordinal, naming it, and what
// it does as it commits: it remembers the tool by id, unless either runs past
// what is remembered ([transcriptBuilder.called]); a call whose tool's
// redaction the stream refused is said in its place (R6).
func (n toolName) call(b *transcriptBuilder, p pendingEntry, id string, ordinal int) (pendingEntry, claudeEvent) {
	ev := claudeEvent{kind: eventCall, id: id, tool: n.raw}
	if n.past {
		ev.id = ""
	}
	switch {
	case n.refused:
		p = b.held(TranscriptEntry{Kind: EntryUnread}, unredactedText, ordinal)
		ev.id = ""
	case p.entry.Kind == EntryToolCall:
		p.entry.Tool = n.shown
	}
	return p, ev
}

// commitThread commits s's entries as [transcriptBuilder.commitShape] does,
// each of the thread being read: a record's thread may be written after its
// texts, which are shaped as they are read.
func (b *transcriptBuilder) commitThread(s *entryShape, at time.Time) error {
	shown, _, _ := entryOpening(b.thread, transcriptNameMaxBytes)
	shown = strings.Clone(shown)
	for i := range s.entries {
		if s.entries[i].count == 0 {
			s.entries[i].p.entry.Thread = shown
		}
	}
	return b.commitShape(s, at)
}

// callList is the calls a message makes after the entry its words make, which
// only the record's end decides: each call shaped once into both readings,
// after that entry (after) and with none (alone), as the page would take it.
type callList struct {
	after, alone entryShape
}

// reset starts the calls of a record whose first entry takes the ordinal base.
func (c *callList) reset(base int) {
	c.after.reset(base + 1)
	c.alone.reset(base)
}

// shapes reports whether either reading shapes its next call.
func (c *callList) shapes(b *transcriptBuilder) bool {
	return c.after.shapes(b, EntryToolCall) || c.alone.shapes(b, EntryToolCall)
}

// add adds a call of tool under id to both readings, its input read into t
// (begun as shaped by [callList.shapes]), nil for none; t is let go of.
func (c *callList) add(b *transcriptBuilder, t *valueText, tool toolName, id string) {
	addCall(b, t, tool, id, &c.after, &c.alone)
}

// addCall adds a call of tool under id to each of shapes, the readings of one
// record, its input read into t (begun as shaped where any of them shapes its
// next call), nil for none, its entry shaped once; t is let go of.
func addCall(b *transcriptBuilder, t *valueText, tool toolName, id string, shapes ...*entryShape) {
	e := TranscriptEntry{Kind: EntryToolCall}
	var shaped pendingEntry
	done := false
	for _, s := range shapes {
		var p pendingEntry
		switch {
		case !s.shapes(b, EntryToolCall) || t != nil && t.past && t.s == nil:
			p = b.held(e, "", s.next())
		case done:
			p = shaped
		case t == nil:
			shaped, done = b.held(e, "", s.next()), true
			p = shaped
		case t.past:
			shaped, done = t.s.end(s.next()), true
			p = shaped
		default:
			shaped, done = b.held(e, t.text(), s.next()), true
			p = shaped
		}
		p, ev := tool.call(b, p, id, s.next())
		s.addEvent(b, p, time.Time{}, ev)
	}
	if t != nil {
		t.drop()
	}
}

// unshaped says, in both readings and in place of every call, that the
// message's calls have no shape this read knows.
func (c *callList) unshaped(b *transcriptBuilder) {
	c.reset(c.alone.base)
	for _, s := range [2]*entryShape{&c.after, &c.alone} {
		s.add(b, b.held(TranscriptEntry{Kind: EntryUnread}, unshapedCalls, s.next()), time.Time{})
	}
}

// unshapedCalls is what a message's calls of no shape this read knows say in
// their place.
const unshapedCalls = "a message's tool requests have no shape this read knows, and were not read"

// commitAfter commits the calls as the reading the message's words picked:
// after their entry where they made one.
func (c *callList) commitAfter(b *transcriptBuilder, words bool, at time.Time) error {
	if words {
		return b.commitThread(&c.after, at)
	}
	return b.commitThread(&c.alone, at)
}

// textAs is the entry text t makes as kind in s, where it was read as another
// kind of words ([readingOf] reads them alike): held, its stream's where s
// shapes the kind, else one only counted.
func textAs(b *transcriptBuilder, j *textJoin, s *entryShape, kind EntryKind) pendingEntry {
	e := TranscriptEntry{Kind: kind}
	switch text, held := j.text(); {
	case j.lost:
		return j.entry(s.next())
	case !s.shapes(b, kind):
		return b.held(e, "", s.next())
	case held:
		return b.held(e, text, s.next())
	}
	return withKind(j.entry(s.next()), kind)
}

// wholeText reads the current string into j as one part of j's kind: a text a
// record holds whole, joined as a list of one would be.
func (w *lineWalk) wholeText(j *textJoin) error {
	j.partStart()
	j.partType(j.rule.kind)
	j.partText()
	err := w.read(j)
	j.partEnd()
	return err
}

// proseJoin adds to s the entries the text j joined makes as kind, as
// [transcriptBuilder.proseInto] makes a held one: past the bound one entry
// (R1), none for a blank one, or the join said where it was lost (R2).
func proseJoin(b *transcriptBuilder, s *entryShape, j *textJoin, kind EntryKind) {
	switch text, held := j.text(); {
	case j.lost:
		s.add(b, j.entry(s.next()), time.Time{})
	case held:
		b.proseInto(s, kind, text)
	case !j.blank():
		s.add(b, textAs(b, j, s, kind), time.Time{})
	}
}
