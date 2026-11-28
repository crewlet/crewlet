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
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/jsonscan"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// transcript reads a Pi archive ([piArchiveFormat]): its session file, a line
// at a time, as the session's entries. Any other member is passed unread.
func (Pi) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
	tr := tar.NewReader(archive)
	b.handed.r = tr
	r := newPiReading(b, bufio.NewReaderSize(&b.handed, lineChunkBytes))
	for {
		h, err := tr.Next()
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return fmt.Errorf("%w: %w", errNotTar, err)
		}
		if !piSessionMember(h.Name) {
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

// piSessionMember reports whether an archive's member named name is a
// session's file: `.pi/sessions/<session>.jsonl`.
func piSessionMember(name string) bool {
	parts := strings.Split(path.Clean(name), "/")
	return len(parts) == 3 && parts[0] == ".pi" && parts[1] == "sessions" &&
		strings.HasSuffix(parts[2], ".jsonl") && parts[2] != ".jsonl"
}

// A PI LINE'S MEMBERS ARE READ BY ITS WHOLE-LINE DECODE'S FIELDS ([fieldKind],
// [piEntry]): a line one is refused by, or that names no type, is no record. A
// message's content is its text blocks joined and its tool calls, none where
// its list's decode refuses it ([piBlocks]).

// piReading is a person's transcript of one session file on the bounded line
// walk ([lineWalk.setupBounded]), in recorded order:
//   - a user message is the brief;
//   - a note the extension handed over (a `crewlet-note` custom message) is
//     each note, late call and wake it carries ([headedIn]);
//   - an assistant message is the model's words and a call per tool call;
//   - a tool result its result, failed where pi recorded it so;
//   - a compaction its summary.
type piReading struct {
	lineWalk
	typ, stamp, customType string
	mistyped               bool
	// The message, where it is one: its role, its result's call and failure,
	// its content's text blocks joined and tool calls, and whether that
	// content's decode took it (blocks).
	message, blocks bool
	role, callID    string
	failed          bool
	text            textJoin
	calls           callList
	// custom is the entry's own content's text, as a custom message's, and
	// customOK whether that content's decode took it.
	custom             textJoin
	customOK           bool
	summary            readText
	words, name, input valueText
	compactor          jsonCompactor
	s                  entryShape
}

// newPiReading is a person's reading into b of the members lines reads.
func newPiReading(b *transcriptBuilder, lines *bufio.Reader) *piReading {
	r := &piReading{}
	r.setupBounded(b, r, lines)
	return r
}

// piTextBlocks is how pi joins a message's text blocks ([piText]): every `text`
// block's words that are not blank, a blank line between each two.
var piTextBlocks = joinRule{kind: "text", sep: "\n\n", dropBlank: true}

// start starts the record the next line holds.
func (r *piReading) start() {
	r.typ, r.stamp, r.customType, r.mistyped = "", "", "", false
	r.messageStart()
	r.custom.start(r.b, TranscriptEntry{Kind: EntryNote}, false, piTextBlocks)
	r.customOK, r.summary = true, readText{}
}

// messageStart starts the record's message: one written again replaces it.
func (r *piReading) messageStart() {
	r.message, r.blocks, r.role, r.callID, r.failed = false, true, "", "", false
	r.text.start(r.b, TranscriptEntry{Kind: EntryModel}, false, piTextBlocks)
	r.calls.reset(r.b.seen)
}

func (*piReading) own(*valueText) error { return nil }

func (r *piReading) linesPast() error { return r.b.linesPast("record", "lines") }

// piEntryFields are the members of an entry that [piEntry] decodes and this
// read does not, by their fields' kinds; piMessageFields a message's, and
// piUsageFields its usage's.
var (
	piEntryFields = map[string]fieldKind{"id": fieldString, "parentId": fieldString, "details": fieldAny,
		"data": fieldAny, "targetId": fieldString}
	piMessageFields = map[string]fieldKind{"stopReason": fieldString, "errorMessage": fieldString,
		"toolName": fieldString}
	piUsageFields = map[string]fieldKind{"input": fieldInt, "output": fieldInt, "cacheRead": fieldInt,
		"cacheWrite": fieldInt}
)

// record reads an entry's type, time, custom type and content, message and
// summary.
func (r *piReading) record() error {
	return r.members(func(name string, k jsonscan.Kind) error {
		var ok bool
		var err error
		switch name {
		case "type":
			r.typ, ok, err = r.wordField(k)
		case "timestamp":
			r.stamp, ok, err = r.wordField(k)
		case "customType":
			r.customType, ok, err = r.wordField(k)
		case "content":
			r.custom.start(r.b, TranscriptEntry{Kind: EntryNote}, r.b.shapes(EntryNote, r.b.seen), piTextBlocks)
			r.customOK, err = r.content(&r.custom, k, nil)
			ok = true
		case "message":
			ok, err = r.messageField(k)
		case "summary":
			r.summary, ok, err = r.stringText(&r.words, TranscriptEntry{Kind: EntrySummary}, k)
		default:
			kind, read := piEntryFields[name]
			if !read {
				return r.scan.Skip()
			}
			ok, err = r.typedField(k, kind)
		}
		r.mistyped = r.mistyped || !ok
		return err
	})
}

// messageField reads an entry's message: its role, content, call and failure.
func (r *piReading) messageField(k jsonscan.Kind) (bool, error) {
	r.messageStart()
	r.message = k == jsonscan.ObjectStart
	return r.objectField(k, func(name string, k jsonscan.Kind) error {
		var ok bool
		var err error
		switch name {
		case "role":
			r.role, ok, err = r.wordField(k)
		case "content":
			b := r.b
			shaped := b.shapes(EntryBrief, b.seen) || b.shapes(EntryModel, b.seen) || b.shapes(EntryToolResult, b.seen)
			r.text.start(b, TranscriptEntry{Kind: EntryModel}, shaped, piTextBlocks)
			r.calls.reset(b.seen)
			r.blocks, err = r.content(&r.text, k, &r.calls)
			ok = true
		case "toolCallId":
			r.callID, ok, err = r.wordField(k)
		case "isError":
			r.failed, ok, err = r.flagField(k)
		case "usage":
			ok, err = r.typedObject(k, piUsageFields)
		default:
			kind, read := piMessageFields[name]
			if !read {
				return r.scan.Skip()
			}
			ok, err = r.typedField(k, kind)
		}
		r.mistyped = r.mistyped || !ok
		return err
	})
}

// content reads a content value as [piBlocks] does: a string one text block, a
// list of text, thinking and toolCall blocks, each text joined into j and,
// where calls is not nil, each tool call added to it. It reports whether the
// content's decode took it: null or no list is no block.
func (r *piReading) content(j *textJoin, k jsonscan.Kind, calls *callList) (bool, error) {
	switch k {
	case jsonscan.String:
		return true, r.wholeText(j)
	case jsonscan.ArrayStart:
	default:
		return k == jsonscan.Null, r.scan.Skip()
	}
	took := true
	err := r.elements(func(k jsonscan.Kind) (bool, error) {
		var kind, id string
		var tool toolName
		var input *valueText
		ok, err := r.joinPart(j, k, func(name string, k jsonscan.Kind) (bool, error) {
			var ok bool
			var err error
			switch name {
			case "id":
				id, ok, err = r.wordField(k)
			case "name":
				tool, ok, err = r.toolName(&r.name, k)
			case "arguments":
				if calls == nil {
					return true, r.scan.Skip()
				}
				input = r.input.begin(r.b, TranscriptEntry{Kind: EntryToolCall}, calls.shapes(r.b))
				r.compactor = jsonCompactor{w: input, buf: r.compactor.buf}
				if teeErr := r.scan.Tee(&r.compactor); teeErr != nil {
					return true, teeErr
				}
				ok, err = true, r.scan.Skip()
			default:
				return true, r.scan.Skip()
			}
			return ok, err
		})
		kind = j.partKind
		took = took && ok
		switch {
		case err == nil && took && calls != nil && kind == "toolCall":
			calls.add(r.b, input, tool, id)
		case input != nil:
			input.drop()
		}
		return true, err
	})
	return took, err
}

// commit commits the reading the entry's type picks, an entry of no type or
// one its decode refused said as no record.
func (r *piReading) commit() error {
	b, s := r.b, &r.s
	if r.mistyped || r.typ == "" {
		return r.refused()
	}
	at := claudeTime(r.stamp)
	s.reset(b.seen)
	switch r.typ {
	case "message":
		if !r.message {
			break
		}
		switch r.role {
		case "user":
			if r.blocks && r.text.said() {
				s.add(b, textAs(b, &r.text, s, EntryBrief), time.Time{})
			}
		case "assistant":
			words := r.blocks && r.text.said()
			if words {
				s.add(b, textAs(b, &r.text, s, EntryModel), time.Time{})
			}
			if err := b.commitThread(s, at); err != nil || !r.blocks {
				return err
			}
			return r.calls.commitAfter(b, words, at)
		case "toolResult":
			// A JOIN OF NO WORDS IS EMPTY, a blank text block being one pi
			// leaves out, whatever runs past the bound (R2).
			p := b.held(TranscriptEntry{Kind: EntryToolResult}, "", s.next())
			if r.blocks && r.text.said() {
				p = textAs(b, &r.text, s, EntryToolResult)
			}
			if p.entry.Kind == EntryToolResult {
				p.entry.Failed = r.failed
			}
			s.addEvent(b, p, time.Time{}, claudeEvent{kind: eventResult, id: r.callID})
		}
	case "custom_message":
		if r.customType == piNoteType {
			r.noteInto(s)
		}
	case "compaction":
		if r.summary.set && !r.summary.blank {
			s.add(b, r.summary.p, time.Time{})
		}
	}
	return b.commitThread(s, at)
}

// noteInto adds a note the extension handed over to s: each note, late call
// and wake it carries where it opens with an engine heading, else one note;
// past the bound the note alone (R1), or the join said where it was lost (R2).
func (r *piReading) noteInto(s *entryShape) {
	j := &r.custom
	switch text, held := j.text(); {
	case !r.customOK:
	case j.lost:
		s.add(r.b, j.entry(s.next()), time.Time{})
	case j.blank():
	case held && opensWithHeading(text):
		r.b.proseInto(s, EntryBrief, text)
	case held:
		r.b.proseInto(s, EntryNote, text)
	default:
		s.add(r.b, textAs(r.b, j, s, EntryNote), time.Time{})
	}
}

// piNode is what [piNotesHeld] keeps of one session entry: its parent, and
// what it is to a note's confirmation.
type piNode struct {
	parent recordWord
	kind   piNodeKind
	inbox  string
	target recordWord
	// summary is a compaction's place among the summaries a read keeps
	// ([piSummaries]), -1 for none.
	summary int
}

type piNodeKind uint8

const (
	// piNote is a note the extension handed over, under its inbox id. The
	// zero kind is an entry of no other kind.
	piNote piNodeKind = iota + 1
	// piAnswer is an assistant message that answered (`stop`, `toolUse`,
	// `length`).
	piAnswer
	// piCompaction is a compaction: past it, a request carries what lay
	// before it only inside its summary.
	piCompaction
	// piEdit is a context_edit, which takes its target out of what the model
	// reads, or replaces it.
	piEdit
)

// piNotesHeld is the inbox ids of takes, in serve order and once each, whose
// note a session file shows reached the model as a current message (ADR-0177):
// on the session's ACTIVE BRANCH (its last entry and every parent back to the
// root), a `crewlet-note` custom message under the take's inbox id, then an
// assistant message that answered, with no compaction between them (one would
// have left the note only inside its summary) and no context_edit taking the
// note out first. A note with no answer after it (a run killed or refused
// after the hand-over, an error or an abort) is not held, so it is served
// again. A last line with no newline is a write cut off and decides nothing;
// a line of no entry read is skipped, which can only cut the branch short and
// hold less. The newest compaction on the branch is the newest summary, read
// into summary where it is not nil ([piSummaries]).
func piNotesHeld(ctx context.Context, r io.Reader, takes []ServedTake, summary *summaryRead) ([]string, error) {
	asked := make(map[string]bool, len(takes))
	for _, take := range takes {
		asked[take.InboxID] = true
	}
	nodes := map[recordWord]piNode{}
	var leaf, held recordWord
	src := &piTail{r: r}
	var w lineWalk
	w.setupNotes(bufio.NewReaderSize(src, lineChunkBytes), maxLineBytes)
	var e piNoteLine
	var sums *piSummaries
	if summary != nil {
		sums, e.summary = &piSummaries{}, summary.newWords()
	}
	for {
		line, err := w.noteLine(ctx, func() error { return e.read(&w) })
		if err != nil {
			return nil, fmt.Errorf("executor: reading which notes the Pi session holds: %w", err)
		}
		if line.none {
			break
		}
		// The line read last is held back until another follows or the
		// file is known to end in a break; one past the bound is no line.
		if !line.past && !held.empty() {
			leaf, held = held, recordWord{}
		}
		if line.past && sums != nil {
			sums.pastAt, sums.past = len(sums.texts), true
		}
		if !line.past && line.object && e.whole && !e.id.empty() && e.typ != "session" {
			n := piNode{parent: e.parent, summary: -1}
			switch {
			case e.typ == "custom_message" && e.customType == piNoteType && asked[e.inbox]:
				n.kind, n.inbox = piNote, e.inbox
			case e.typ == "message" && e.message && e.role == "assistant" && piAnswered(e.stop):
				n.kind = piAnswer
			case e.typ == "compaction":
				n.kind = piCompaction
				if sums != nil {
					n.summary = sums.keep(e.summary)
				}
			case e.typ == "context_edit":
				n.kind, n.target = piEdit, e.target
			}
			nodes[e.id] = n
			held = e.id
		}
		if line.ended {
			break
		}
	}
	if !held.empty() && src.last == '\n' {
		leaf = held
	}
	// The branch, root first.
	var branch []piNode
	seen := map[recordWord]bool{}
	for id := leaf; !id.empty() && !seen[id]; {
		n, ok := nodes[id]
		if !ok {
			break
		}
		seen[id] = true
		branch = append(branch, n)
		id = n.parent
	}
	slices.Reverse(branch)
	if sums != nil {
		sums.newest(branch, summary)
	}
	pending := map[string]bool{}
	confirmed := map[string]bool{}
	for _, n := range branch {
		switch n.kind {
		case piNote:
			pending[n.inbox] = true
		case piAnswer:
			for inbox := range pending {
				confirmed[inbox] = true
			}
			clear(pending)
		case piCompaction:
			clear(pending)
		case piEdit:
			for inbox := range pending {
				if nodes[n.target].inbox == inbox && nodes[n.target].kind == piNote {
					delete(pending, inbox)
				}
			}
		}
	}
	var out []string
	for _, take := range takes {
		if confirmed[take.InboxID] && !slices.Contains(out, take.InboxID) {
			out = append(out, take.InboxID)
		}
	}
	return out, nil
}

// piSummaries is the compactions' summaries a read of a session file keeps
// until its active branch is known, held to [summaryReadMaxBytes] in all, the
// earliest let go first; pastAt is how many came before the last line past the
// bound, which may have been the branch's newest compaction (0 for none).
type piSummaries struct {
	texts  []summaryText
	held   int
	pastAt int
	past   bool
}

// keep keeps a compaction's summary t, and is its place.
func (p *piSummaries) keep(t *summaryText) int {
	// Its words handed over, not copied: t takes a buffer of its own next.
	kept := summaryText{buf: t.buf, set: t.set, past: t.past, odd: t.odd, claim: t.claim, counted: t.counted}
	t.buf, t.counted = nil, 0
	p.texts = append(p.texts, kept)
	p.held += len(kept.buf)
	for i := 0; p.held > summaryReadMaxBytes && i < len(p.texts); i++ {
		if k := &p.texts[i]; k.set && !k.past {
			p.held -= len(k.buf)
			k.buf, k.past = nil, true
			k.claim.track(&k.counted, 0)
		}
	}
	return len(p.texts) - 1
}

// newest reads the newest summary on branch, root first, into s, as the
// transcript reads one: the last compaction's whose summary is not blank.
func (p *piSummaries) newest(branch []piNode, s *summaryRead) {
	at := -1
	for _, n := range slices.Backward(branch) {
		if n.kind != piCompaction || n.summary < 0 {
			continue
		}
		if t := p.texts[n.summary]; t.set && (t.past || len(bytes.TrimSpace(t.buf)) > 0) {
			at = n.summary
			break
		}
	}
	switch {
	case p.past && p.pastAt > at:
		s.unshaped(fmt.Sprintf("a line of the session runs past the %d bytes a line is read to", maxLineBytes))
	case at >= 0:
		s.taken(&p.texts[at])
	}
}

// piNoteLine is what [piNotesHeld] reads of one session line ([piEntry]): its
// type, place on the tree, custom type and note's inbox, a message's role and
// why it stopped, and a context_edit's target; whole where each member
// [piEntry] decodes is of its field's kind ([fieldKind]), and a line that is
// not is no entry: one the confirm reader skips, which can only cut the
// branch it walks short, so it never holds a note it would not hold without it.
type piNoteLine struct {
	typ, customType, inbox, role, stop string
	id, parent, target                 recordWord
	message, whole                     bool
	// summary, where a read asks one, is a compaction's `summary`.
	summary *summaryText
}

// read reads one line's members.
func (l *piNoteLine) read(w *lineWalk) error {
	*l = piNoteLine{whole: true, summary: l.summary}
	if l.summary != nil {
		l.summary.reset()
	}
	return w.members(func(name string, k jsonscan.Kind) error {
		var ok bool
		var err error
		switch name {
		case "type":
			l.typ, ok, err = w.wordField(k)
		case "customType":
			l.customType, ok, err = w.wordField(k)
		case "id":
			l.id, ok, err = w.idField(k)
		case "parentId":
			l.parent, ok, err = w.idField(k)
		case "targetId":
			l.target, ok, err = w.idField(k)
		case "details":
			l.inbox, err = l.details(w, k)
			ok = true
		case "message":
			l.role, l.stop, l.message = "", "", k == jsonscan.ObjectStart
			ok, err = w.objectField(k, func(name string, k jsonscan.Kind) error {
				var took bool
				var fieldErr error
				switch name {
				case "role":
					l.role, took, fieldErr = w.wordField(k)
				case "stopReason":
					l.stop, took, fieldErr = w.wordField(k)
				case "toolCallId":
					took, fieldErr = w.typedField(k, fieldString)
				case "isError":
					_, took, fieldErr = w.flagField(k)
				case "usage":
					took, fieldErr = w.typedObject(k, piUsageFields)
				default:
					kind, read := piMessageFields[name]
					if !read {
						return w.scan.Skip()
					}
					took, fieldErr = w.typedField(k, kind)
				}
				l.whole = l.whole && took
				return fieldErr
			})
		case "summary":
			if l.summary == nil {
				ok, err = w.typedField(k, fieldString)
			} else {
				ok, err = l.summary.read(w, k)
			}
		case "timestamp":
			ok, err = w.typedField(k, fieldString)
		default:
			return w.scan.Skip()
		}
		l.whole = l.whole && ok
		return err
	})
}

// details reads an entry's details, any value, for the inbox id a note's
// carry ([piEntry.inbox]): "" for none, or for details of no such shape.
func (*piNoteLine) details(w *lineWalk, k jsonscan.Kind) (string, error) {
	if k != jsonscan.ObjectStart {
		return "", w.scan.Skip()
	}
	inbox := ""
	err := w.members(func(name string, k jsonscan.Kind) error {
		if name != "inbox" {
			return w.scan.Skip()
		}
		var err error
		inbox, _, err = w.wordField(k)
		return err
	})
	return inbox, err
}

// piTail is r, remembering the last byte it handed out.
type piTail struct {
	r    io.Reader
	last byte
}

func (t *piTail) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		t.last = p[n-1]
	}
	return n, err
}

// notesHeld is the inbox ids of takes ([Pi.inboxID]) whose note the box's copy
// of session shows reached the model ([piNotesHeld]): its [recordConfirmer],
// as its extension confirms nothing. An error where the record could not be
// read, which is neither held nor not.
func (Pi) notesHeld(ctx context.Context, box sandbox.Sandbox, session Session, takes []ServedTake,
	summary *summaryRead) ([]string, error) {
	f, err := box.OpenFile(ctx, PathsFor(box).PiSessionFile(session.ID))
	if err != nil {
		return nil, summary.failedBy(fmt.Errorf("executor: opening the Pi session's record: %w", err))
	}
	defer func() { _ = f.Close() }()
	held, err := piNotesHeld(ctx, f, takes, summary)
	return held, summary.failedBy(unchangedRecord(f, "the Pi session's record", err))
}

// summaryWhy is why ask's summary is not read off its session file, "" where
// it is: on the release its summary was read on, of a session a file is
// named by.
func (Pi) summaryWhy(ask sandbox.MainThreadAsk) string {
	if why := summaryReleaseWhy(ask.Release, piSupported); why != "" {
		return why
	}
	if !sandbox.ValidSessionID(ask.Session) {
		return fmt.Sprintf("the run's Pi session %q is no session id its file is named by", ask.Session)
	}
	return ""
}

// recordSummary reports whether the receipts' walk reads ask's summary: its
// session file is the one walk that reads it.
func (p Pi) recordSummary(ask sandbox.MainThreadAsk) bool { return p.summaryWhy(ask) == "" }

// readMainThread reads the newest summary on the active branch of ask's
// session in box, off its session file ([Pi.notesHeld]), on the release its
// summary was read on alone. Its receipts are its record's ([recordConfirmer]).
func (p Pi) readMainThread(ctx context.Context, box sandbox.Sandbox, _ Paths,
	ask sandbox.MainThreadAsk) sandbox.MainThreadReading {

	if !ask.Summary {
		return sandbox.MainThreadReading{}
	}
	if why := p.summaryWhy(ask); why != "" {
		return sandbox.MainThreadReading{Summary: summaryUnread(why)}
	}
	var large passBudget
	defer large.release()
	summary := summaryRead{claim: newSummaryClaim(ctx, &large)}
	_, err := p.notesHeld(ctx, box, Session{ID: ask.Session}, nil, &summary)
	countRecordChanged(ctx, err)
	return sandbox.MainThreadReading{Summary: summary.reading()}
}

// threadTraits is what Pi's main-thread read settles: the newest summary
// alone, its record's receipts added by [Runner.ThreadTraits].
func (Pi) threadTraits() sandbox.ThreadTraits { return sandbox.ThreadTraits{Summaries: true} }
