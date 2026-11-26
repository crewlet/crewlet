package executor

import (
	"archive/tar"
	"bufio"
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
	"github.com/crewlet/crewlet/internal/shellword"
)

// kimiNotesHeld is the inbox ids of takes, in serve order and once each, whose
// note the main agent's wire.jsonl shows reached the model as a current
// message: its `context.append_message`, a user message under its prompt_id
// ([KimiNoteID]), then a `usage.record` of `usageScope: turn` (a request of the
// agent's loop answered) with no compaction, undo or clear between. Measured on
// 2.7.2: the line is on disk before its request; killed between the two, the
// record holds the line alone; and past a compaction, whose record names no
// message it kept, a request may carry the note only inside its summary
// (ADR-0177), so such a note is told again (code-sandbox.md, "Kimi Code"). A
// last line with no newline is a write cut off; a line past the bound is no
// line. Walked for notes ([lineWalk.noteLine]), and its newest summary, if
// asked ([kimiWireLine.summarised]).
func kimiNotesHeld(ctx context.Context, wire io.Reader, takes []ServedTake, summary *summaryRead) ([]string, error) {
	asked := make(map[string]string, len(takes))
	for _, take := range takes {
		asked[KimiNoteID(take.InboxID)] = take.InboxID
	}
	pending := map[string]bool{}
	held := map[string]bool{}
	// moved is what the line read last moved to held, taken back where that
	// line turns out to be the file's unended last.
	var moved []string
	src := &lastByte{r: wire}
	var w lineWalk
	w.setupNotes(bufio.NewReaderSize(src, lineChunkBytes), maxLineBytes)
	var l kimiWireLine
	if summary != nil {
		l.summary = summary.newWords()
	}
	for {
		line, err := w.noteLine(ctx, func() error { return l.read(&w) })
		if err != nil {
			return nil, fmt.Errorf("executor: reading which notes the Kimi session holds: %w", err)
		}
		if line.none {
			break
		}
		if summary != nil {
			l.summarised(line, summary)
		}
		moved = moved[:0]
		switch typ := l.kind(line); typ {
		case "context.append_message":
			if inbox, ours := asked[l.id]; ours && l.role == "user" {
				pending[inbox] = true
			}
		case "usage.record":
			if l.scope != "turn" {
				break
			}
			for inbox := range pending {
				if !held[inbox] {
					held[inbox] = true
					moved = append(moved, inbox)
				}
			}
			clear(pending)
		case "context.apply_compaction", "context.undo", "context.clear":
			clear(pending)
		}
		if line.ended {
			break
		}
	}
	if src.read && src.last != '\n' {
		for _, inbox := range moved {
			delete(held, inbox)
		}
	}
	var out []string
	for _, take := range takes {
		if held[take.InboxID] && !slices.Contains(out, take.InboxID) {
			out = append(out, take.InboxID)
		}
	}
	return out, nil
}

// kimiWireLine is what the confirm reader reads of one wire.jsonl line: its
// type, a message's id and role, and a usage record's scope; whole where each
// member of those is of its kind, typed where at least its type is.
type kimiWireLine struct {
	typ, id, role, scope string
	whole, typed         bool
	// summary, where a read asks one, is the line's `summary`.
	summary *summaryText
}

// read reads one line's members.
func (l *kimiWireLine) read(w *lineWalk) error {
	*l = kimiWireLine{whole: true, typed: true, summary: l.summary}
	if l.summary != nil {
		l.summary.reset()
	}
	return w.members(func(name string, k jsonscan.Kind) error {
		var ok bool
		var err error
		switch name {
		case "type":
			l.typ, ok, err = w.wordField(k)
			l.typed = l.typed && ok
		case "message":
			l.id, l.role = "", ""
			ok, err = w.objectField(k, func(name string, k jsonscan.Kind) error {
				var took bool
				var fieldErr error
				switch name {
				case "id":
					l.id, took, fieldErr = w.wordField(k)
				case "role":
					l.role, took, fieldErr = w.wordField(k)
				default:
					return w.scan.Skip()
				}
				l.whole = l.whole && took
				return fieldErr
			})
		case "usageScope":
			l.scope, ok, err = w.wordField(k)
		case "summary":
			if l.summary == nil {
				return w.scan.Skip()
			}
			_, err = l.summary.read(w, k)
			return err
		default:
			return w.scan.Skip()
		}
		l.whole = l.whole && ok
		return err
	})
}

// kind is the type the line names as the confirm reader takes it. A LINE READ
// ONLY IN PART STILL COUNTS BY ITS TYPE: a compaction whose other members are
// of another kind still clears, and nothing else is read off it, so no such
// line confirms a note. A line past the bound, or no JSON object, names none.
func (l *kimiWireLine) kind(line noteLine) string {
	switch {
	case line.past, !line.object, !l.typed:
		return ""
	case !l.whole:
		*l = kimiWireLine{typ: l.typ, summary: l.summary}
	}
	return l.typ
}

// summarised makes the line's `summary` the newest summary where the line
// applies a compaction, as the transcript reads one ([kimiReading.commit]):
// the summary Kimi works from, never its `contextSummary`, which names the
// box's paths. A line past the bound leaves the newest unsaid.
func (l *kimiWireLine) summarised(line noteLine, s *summaryRead) {
	switch {
	case line.past:
		s.unshaped(fmt.Sprintf("a line of the record runs past the %d bytes a line is read to", maxLineBytes))
	case line.object && l.typed && l.typ == "context.apply_compaction":
		s.taken(l.summary)
	}
}

// notesHeld is the inbox ids of takes ([Kimi.inboxID]) whose note the box's
// copy of session's main agent record shows reached the model
// ([kimiNotesHeld]): its [recordConfirmer], as its driver confirms nothing.
// The record is found under any workspace directory, since Kimi names that
// directory by the run's working directory: an id matching none, or more than
// one, is a record that could not be read, which is neither held nor not.
func (Kimi) notesHeld(ctx context.Context, box sandbox.Sandbox, session Session,
	takes []ServedTake, summary *summaryRead) ([]string, error) {
	wire, err := kimiMainWire(ctx, box, session.ID)
	if err != nil {
		return nil, summary.failedBy(err)
	}
	defer func() { _ = wire.Close() }()
	held, err := kimiNotesHeld(ctx, wire, takes, summary)
	return held, summary.failedBy(unchangedRecord(wire, "the Kimi session's record", err))
}

// recordSummary reports whether the receipts' walk reads ask's summary: its
// wire.jsonl is the one walk that reads it, on the release its summary was
// read on.
func (Kimi) recordSummary(ask sandbox.MainThreadAsk) bool {
	return summaryReleaseWhy(ask.Release, kimiSupported) == ""
}

// kimiMainWire opens the main agent's wire.jsonl of session in box, under any
// workspace directory: an id matching none, or more than one, is an error.
func kimiMainWire(ctx context.Context, box sandbox.Sandbox, session string) (io.ReadCloser, error) {
	if !strings.HasPrefix(session, "session_") || !sandbox.ValidSessionID(session) {
		return nil, fmt.Errorf("executor: %q is no Kimi session id", session)
	}
	paths := PathsFor(box)
	found, err := box.Exec(ctx, `for w in `+shellword.Quote(paths.KimiSessions())+`/*/`+session+
		`/agents/main/wire.jsonl; do [ -f "$w" ] && printf '%s\n' "$w"; done; :`, sandbox.ExecOptions{})
	if err != nil {
		return nil, fmt.Errorf("executor: finding the Kimi session's record: %w", err)
	}
	records := strings.Fields(strings.TrimSpace(found.Stdout))
	if len(records) != 1 {
		return nil, fmt.Errorf("executor: the box holds %d records of the Kimi session %s, not one", len(records),
			session)
	}
	wire, err := box.OpenFile(ctx, records[0])
	if err != nil {
		return nil, fmt.Errorf("executor: opening the Kimi session's record: %w", err)
	}
	return wire, nil
}

// readMainThread reads the newest summary on the main agent's thread of ask's
// session in box, off its wire.jsonl ([Kimi.notesHeld]), on the release its
// summary was read on alone. Its receipts are its record's ([recordConfirmer]).
func (k Kimi) readMainThread(ctx context.Context, box sandbox.Sandbox, _ Paths,
	ask sandbox.MainThreadAsk) sandbox.MainThreadReading {

	if !ask.Summary {
		return sandbox.MainThreadReading{}
	}
	if why := summaryReleaseWhy(ask.Release, kimiSupported); why != "" {
		return sandbox.MainThreadReading{Summary: summaryUnread(why)}
	}
	var large passBudget
	defer large.release()
	summary := summaryRead{claim: newSummaryClaim(ctx, &large)}
	_, _ = k.notesHeld(ctx, box, Session{ID: ask.Session}, nil, &summary)
	return sandbox.MainThreadReading{Summary: summary.reading()}
}

// threadTraits is what Kimi's main-thread read settles: the newest summary
// alone, its record's receipts added by [Runner.ThreadTraits].
func (Kimi) threadTraits() sandbox.ThreadTraits { return sandbox.ThreadTraits{Summaries: true} }

// transcript reads a Kimi archive ([kimiArchiveFormat]): each agent's
// wire.jsonl, a line at a time, as the session's entries, the main agent's
// its main thread and a subagent's its own. Any other member is passed unread.
func (Kimi) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
	tr := tar.NewReader(archive)
	b.handed.r = tr
	r := newKimiReading(b, bufio.NewReaderSize(&b.handed, lineChunkBytes))
	for {
		h, err := tr.Next()
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return fmt.Errorf("%w: %w", errNotTar, err)
		}
		agent, ok := kimiWireMember(h.Name)
		if !ok {
			continue
		}
		if h.Typeflag != tar.TypeReg {
			if err := b.unread("a transcript of the harness's record is not a plain file in the archive, " +
				"and was not read"); err != nil {
				return err
			}
			continue
		}
		if agent == "main" {
			agent = ""
		}
		r.thread, r.framed = agent, 0
		r.lines.Reset(&b.handed)
		if err := r.member(ctx); err != nil {
			return err
		}
	}
}

// kimiWireMember is the agent whose wire.jsonl an archive's member named name
// is, `wd_<key>/session_<id>/agents/<agent>/wire.jsonl`, false for any other.
func kimiWireMember(name string) (string, bool) {
	parts := strings.Split(path.Clean(name), "/")
	if len(parts) != 5 || !strings.HasPrefix(parts[1], "session_") || parts[2] != "agents" || parts[3] == "" ||
		parts[4] != "wire.jsonl" {
		return "", false
	}
	return parts[3], true
}

// kimiReading is a person's transcript of one Kimi Code agent's wire.jsonl on
// the bounded line walk ([lineWalk.setupBounded]), in recorded order: a
// person's message (`origin.kind: user`, never Kimi's own injected reminders)
// is the brief, or each note, late call and wake it carries where it opens
// with an engine heading ([headedIn]); a text part the model's words; a tool
// call and its result; a compaction its summary. A record's `type` may be
// written after its texts, so each is shaped as it is read ([readText]) and
// the line's end picks one. Its members are read by their whole-line decode's
// fields ([fieldKind]): a line one is refused by, or that names no type, is
// no record.
type kimiReading struct {
	lineWalk
	// thread is the agent the member is of, set as each record commits.
	thread   string
	typ      string
	time     int64
	mistyped bool
	// The message: its role, its origin, its text parts joined.
	role, origin string
	words        textJoin
	// The loop event: its kind, its call's id and tool, its part's kind and
	// text, its call's input, its result's output and failure.
	evType, callID string
	tool           toolName
	partType       string
	part           readText
	call, result   readText
	failed         bool
	summary        readText
	// text is a string member's words, name a tool's, input a value as
	// written; output joins a result's parts.
	text, name, input valueText
	compact           jsonCompactor
	output            textJoin
	s                 entryShape
}

// newKimiReading is a person's reading into b of the members lines reads.
func newKimiReading(b *transcriptBuilder, lines *bufio.Reader) *kimiReading {
	r := &kimiReading{}
	r.setupBounded(b, r, lines)
	return r
}

// start starts the record the next line holds.
func (r *kimiReading) start() {
	r.typ, r.time, r.mistyped = "", 0, false
	r.role, r.origin = "", ""
	r.words.start(r.b, TranscriptEntry{Kind: EntryBrief}, false, kimiParts)
	r.eventStart()
	r.summary = readText{}
}

// eventStart starts the record's loop event: one written again replaces it.
func (r *kimiReading) eventStart() {
	r.evType, r.callID, r.tool, r.partType = "", "", toolName{}, ""
	r.part, r.call, r.result, r.failed = readText{}, readText{}, readText{}, false
}

// kimiParts is how Kimi's text parts join: every `text` part's words, as
// written, one after the other.
var kimiParts = joinRule{kind: "text"}

func (*kimiReading) own(*valueText) error { return nil }

func (r *kimiReading) linesPast() error { return r.b.linesPast("record", "lines") }

// record reads a record's type, time, message, loop event and summary.
func (r *kimiReading) record() error {
	return r.members(func(name string, k jsonscan.Kind) error {
		var ok bool
		var err error
		switch name {
		case "type":
			r.typ, ok, err = r.wordField(k)
		case "time":
			r.time, ok, err = r.intField(k)
		case "message":
			ok, err = r.message(k)
		case "event":
			ok, err = r.event(k)
		case "summary":
			r.summary, ok, err = r.stringText(&r.text, TranscriptEntry{Kind: EntrySummary}, k)
		default:
			return r.scan.Skip()
		}
		r.mistyped = r.mistyped || !ok
		return err
	})
}

// message reads a record's message: its role, its origin's kind and its
// content, a list of parts joined.
func (r *kimiReading) message(k jsonscan.Kind) (bool, error) {
	r.role, r.origin = "", ""
	r.words.start(r.b, TranscriptEntry{Kind: EntryBrief}, false, kimiParts)
	return r.objectField(k, func(name string, k jsonscan.Kind) error {
		var ok bool
		var err error
		switch name {
		case "role":
			r.role, ok, err = r.wordField(k)
		case "origin":
			r.origin = ""
			ok, err = r.objectField(k, func(name string, k jsonscan.Kind) error {
				if name != "kind" {
					return r.scan.Skip()
				}
				var took bool
				var fieldErr error
				r.origin, took, fieldErr = r.wordField(k)
				r.mistyped = r.mistyped || !took
				return fieldErr
			})
		case "content":
			r.words.start(r.b, TranscriptEntry{Kind: EntryBrief}, r.b.shapes(EntryBrief, r.b.seen), kimiParts)
			ok, err = r.listField(k, func(k jsonscan.Kind) error {
				took, partErr := r.joinPart(&r.words, k, nil)
				r.mistyped = r.mistyped || !took
				return partErr
			})
		default:
			return r.scan.Skip()
		}
		r.mistyped = r.mistyped || !ok
		return err
	})
}

// event reads a record's loop event: its type, its call's id, tool and input,
// its part, and its result.
func (r *kimiReading) event(k jsonscan.Kind) (bool, error) {
	r.eventStart()
	return r.objectField(k, func(name string, k jsonscan.Kind) error {
		var ok bool
		var err error
		switch name {
		case "type":
			r.evType, ok, err = r.wordField(k)
		case "toolCallId":
			r.callID, ok, err = r.wordField(k)
		case "name":
			r.tool, ok, err = r.toolName(&r.name, k)
		case "args":
			r.call, err = r.rawText(&r.input, &r.compact, TranscriptEntry{Kind: EntryToolCall}, k)
			ok = true
		case "part":
			r.partType, r.part = "", readText{}
			ok, err = r.objectField(k, func(name string, k jsonscan.Kind) error {
				var took bool
				var fieldErr error
				switch name {
				case "type":
					r.partType, took, fieldErr = r.wordField(k)
				case "text":
					r.part, took, fieldErr = r.stringText(&r.text, TranscriptEntry{Kind: EntryModel}, k)
				default:
					return r.scan.Skip()
				}
				r.mistyped = r.mistyped || !took
				return fieldErr
			})
		case "result":
			r.result, r.failed = readText{}, false
			ok, err = r.objectField(k, func(name string, k jsonscan.Kind) error {
				var took bool
				var fieldErr error
				switch name {
				case "output":
					r.result, fieldErr = r.answer(k)
					took = true
				case "isError":
					r.failed, took, fieldErr = r.flagField(k)
				default:
					return r.scan.Skip()
				}
				r.mistyped = r.mistyped || !took
				return fieldErr
			})
		default:
			return r.scan.Skip()
		}
		r.mistyped = r.mistyped || !ok
		return err
	})
}

// answer reads a tool result's output, any value, as its entry's text: a
// string as it is; a list as its `text` parts joined where every element is
// such a part or null, else the list as written and compacted, both read as
// it is passed; any other value as written and compacted.
func (r *kimiReading) answer(k jsonscan.Kind) (readText, error) {
	e := TranscriptEntry{Kind: EntryToolResult}
	switch k {
	case jsonscan.String, jsonscan.Null:
		t, _, err := r.stringText(&r.text, e, k)
		return t, err
	case jsonscan.ArrayStart:
	default:
		return r.rawText(&r.input, &r.compact, e, k)
	}
	ordinal := r.b.seen
	shaped := r.b.shapes(EntryToolResult, ordinal)
	t := r.input.begin(r.b, e, shaped)
	r.compact = jsonCompactor{w: t, buf: r.compact.buf}
	if err := r.scan.Tee(&r.compact); err != nil {
		return readText{}, err
	}
	r.output.start(r.b, e, shaped, kimiParts)
	parts := true
	err := r.elements(func(k jsonscan.Kind) (bool, error) {
		took, err := r.joinPart(&r.output, k, nil)
		parts = parts && took
		return true, err
	})
	if err != nil {
		t.drop()
		return readText{}, err
	}
	if !parts {
		return readText{p: r.textEntry(t, e, ordinal), set: true}, nil
	}
	t.drop()
	return readText{p: r.output.entry(ordinal), set: true}, nil
}

// commit commits the reading the record's type picks, a record of no type or
// one its decode refused said as no record.
func (r *kimiReading) commit() error {
	b, s := r.b, &r.s
	if r.mistyped || r.typ == "" {
		return r.refused()
	}
	b.thread = r.thread
	s.reset(b.seen)
	switch r.typ {
	case "context.append_message":
		if r.role == "user" && r.origin == "user" {
			r.wordsInto(s)
		}
	case "context.append_loop_event":
		switch r.evType {
		case "content.part":
			if r.partType == "text" && r.part.set && !r.part.blank {
				s.add(b, r.part.p, time.Time{})
			}
		case "tool.call":
			p, ev := r.tool.call(b, r.call.entry(b, EntryToolCall, s.next()), r.callID, s.next())
			s.addEvent(b, p, time.Time{}, ev)
		case "tool.result":
			p := r.result.entry(b, EntryToolResult, s.next())
			if p.entry.Kind == EntryToolResult {
				p.entry.Failed = r.failed
			}
			s.addEvent(b, p, time.Time{}, claudeEvent{kind: eventResult, id: r.callID})
		}
	case "context.apply_compaction":
		if r.summary.set && !r.summary.blank {
			s.add(b, r.summary.p, time.Time{})
		}
	}
	return b.commitThread(s, time.UnixMilli(r.time).UTC())
}

// wordsInto adds the person's message to s: each note, late call and wake it
// carries where it opens with an engine heading, else the brief; past the
// bound the brief alone (R1), or the join said where it was lost (R2).
func (r *kimiReading) wordsInto(s *entryShape) {
	j := &r.words
	text, held := j.text()
	switch {
	case j.lost:
		s.add(r.b, j.entry(s.next()), time.Time{})
	case j.blank():
	case held:
		r.b.proseInto(s, EntryBrief, text)
	default:
		s.add(r.b, j.entry(s.next()), time.Time{})
	}
}
