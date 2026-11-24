package executor

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/jsonscan"
)

// transcript reads a Muse Code archive ([museArchiveFormat]): the main
// thread's session.jsonl and each subagent's, a line at a time, as the
// session's entries, a subagent's named by its thread. Any other member is
// passed unread.
func (Muse) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
	tr := tar.NewReader(archive)
	b.handed.r = tr
	r := newMuseReading(b, bufio.NewReaderSize(&b.handed, lineChunkBytes))
	for {
		h, err := tr.Next()
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return fmt.Errorf("%w: %w", errNotTar, err)
		}
		thread, ok := museRecordMember(h.Name)
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
		b.thread, r.framed = thread, 0
		r.lines.Reset(&b.handed)
		if err := r.member(ctx); err != nil {
			return err
		}
	}
}

// museRecordMember reports whether an archive's member named name is a
// session's record, `<YYYY>/<MM>/<DD>/<session>/session.jsonl`, or a
// subagent's, `.../<session>/subagent/<thread>/session.jsonl`, with the
// subagent's thread ("" for the main thread's).
func museRecordMember(name string) (thread string, ok bool) {
	parts := strings.Split(path.Clean(name), "/")
	for _, p := range parts {
		if p == "" || p == ".." {
			return "", false
		}
	}
	switch {
	case len(parts) == 5 && parts[4] == "session.jsonl":
		return "", true
	case len(parts) == 7 && parts[4] == "subagent" && parts[6] == "session.jsonl":
		return parts[5], true
	}
	return "", false
}

// museReading is a person's transcript of one session.jsonl on the bounded
// line walk ([lineWalk.setupBounded]), in recorded order: a run's start is
// its prompt, the brief or each engine-headed part it carries ([headedIn]);
// an assistant message the model's words; a committed call or result batch a
// call or a result each; a hook's context (a block of [museHookSource]) each
// note, late call and wake it carries; an installed compaction its summary.
// A line of a JSON value but an object, of another payload type or of an
// envelope its decode refuses is passed over; a runtime record whose event its
// decode refuses ([fieldKind]) is said. An event's kind can follow what it
// picks (Muse writes `entries` before `kind`), so each member is shaped into
// its reading as it is read, and the line's end picks one.
type museReading struct {
	lineWalk
	payloadType string
	at          int64
	// envelope says each member of the envelope its decode reads is of its
	// field's kind, payload that one is written, and shaped that it decodes
	// with its event; mistyped that a member of the event does not.
	envelope, payload, shaped, mistyped bool
	kind, source                        string
	// The readings an event's kinds pick, each shaped as its member is read.
	brief, notes, results, batch entryShape
	calls                        callList
	model, summary               readText
	text, name, input            valueText
	compact                      jsonCompactor
	s                            entryShape
}

// newMuseReading is a person's reading into b of the members lines reads.
func newMuseReading(b *transcriptBuilder, lines *bufio.Reader) *museReading {
	r := &museReading{}
	r.setupBounded(b, r, lines)
	r.passes = func(jsonscan.Kind) bool { return true }
	return r
}

// museStreamFields are the members of a record's stream its envelope's decode
// reads, by their fields' kinds.
var museStreamFields = map[string]fieldKind{"kind": fieldString, "id": fieldString}

// museUnshapedRecord is what a runtime record whose event its decode refuses
// says in its place.
const museUnshapedRecord = "a record of the harness's session has no shape this read knows, and was not read"

// start starts the record the next line holds.
func (r *museReading) start() {
	r.payloadType, r.at, r.envelope, r.payload = "", 0, true, false
	r.payloadStart()
}

// payloadStart starts the record's payload: one written again replaces it.
func (r *museReading) payloadStart() {
	r.shaped = true
	r.eventStart()
}

// eventStart starts the payload's event: one written again replaces it.
func (r *museReading) eventStart() {
	b := r.b
	r.kind, r.source, r.mistyped = "", "", false
	r.model, r.summary = readText{}, readText{}
	r.brief.reset(b.seen)
	r.notes.reset(b.seen)
	r.results.reset(b.seen)
	r.batch.reset(b.seen)
	r.calls.reset(b.seen)
}

func (*museReading) own(*valueText) error { return nil }

func (r *museReading) linesPast() error { return r.b.linesPast("record", "lines") }

// record reads a record's envelope and its payload's event.
func (r *museReading) record() error {
	return r.members(func(name string, k jsonscan.Kind) error {
		var ok bool
		var err error
		switch name {
		case "payload_type":
			r.payloadType, ok, err = r.wordField(k)
		case "recorded_at":
			r.at, ok, err = r.intField(k)
		case "stream":
			ok, err = r.typedObject(k, museStreamFields)
		case "payload":
			r.payloadStart()
			r.payload = true
			r.shaped, err = r.objectField(k, func(name string, k jsonscan.Kind) error {
				if name != "event" {
					return r.scan.Skip()
				}
				r.eventStart()
				took, eventErr := r.objectField(k, r.eventMember)
				r.mistyped = r.mistyped || !took
				return eventErr
			})
			r.shaped = r.shaped && !r.mistyped
			return err
		default:
			return r.scan.Skip()
		}
		r.envelope = r.envelope && ok
		return err
	})
}

// eventMember reads one member of a runtime record's event into the reading
// it makes.
func (r *museReading) eventMember(name string, k jsonscan.Kind) error {
	var ok bool
	var err error
	switch name {
	case "kind":
		r.kind, ok, err = r.wordField(k)
	case "source":
		r.source, ok, err = r.wordField(k)
	case "prompt":
		ok, err = r.prompt(k)
	case "text":
		ok, err = r.words(k)
	case "replacement":
		r.summary = readText{}
		ok, err = r.objectField(k, func(name string, k jsonscan.Kind) error {
			if name != "text" {
				return r.scan.Skip()
			}
			var took bool
			var textErr error
			r.summary, took, textErr = r.stringText(&r.text, TranscriptEntry{Kind: EntrySummary}, k)
			r.mistyped = r.mistyped || !took
			return textErr
		})
	case "tool_calls":
		r.calls.reset(r.b.seen)
		ok, err = r.listField(k, r.call)
	case "results":
		r.results.reset(r.b.seen)
		ok, err = r.listField(k, r.result)
	case "entries":
		r.batch.reset(r.b.seen)
		ok, err = r.listField(k, r.update)
	default:
		return r.scan.Skip()
	}
	r.mistyped = r.mistyped || !ok
	return err
}

// textOf reads the string value k begins into r.text, which past the bound
// makes an entry of kind, streamed where shaped: nil for null, and false
// where a string field does not take the value, passed over.
func (r *museReading) textOf(k jsonscan.Kind, kind EntryKind, shaped bool) (*valueText, bool, error) {
	switch k {
	case jsonscan.Null:
		return nil, true, nil
	case jsonscan.String:
	default:
		return nil, false, r.scan.Skip()
	}
	t := r.text.begin(r.b, TranscriptEntry{Kind: kind}, shaped)
	if err := r.read(t); err != nil {
		t.drop()
		return nil, true, err
	}
	return t, true, nil
}

// prompt reads a run's prompt into the brief's reading: each note, late call
// and wake it carries where it opens with an engine heading, else the brief;
// past the bound the brief alone (R1).
func (r *museReading) prompt(k jsonscan.Kind) (bool, error) {
	b, s := r.b, &r.brief
	s.reset(b.seen)
	t, ok, err := r.textOf(k, EntryBrief, s.shapes(b, EntryBrief))
	if t == nil {
		return ok, err
	}
	defer t.drop()
	switch {
	case t.blank:
	case !t.past:
		b.proseInto(s, EntryBrief, t.text())
	default:
		p, _ := t.end(s.next(), true)
		s.add(b, p, time.Time{})
	}
	return true, nil
}

// words reads an event's text into the model's words' reading and the hook
// context's: each note, late call and wake a hook's context carries where it
// opens with an engine heading, none past the bound (R1).
func (r *museReading) words(k jsonscan.Kind) (bool, error) {
	b := r.b
	r.model = readText{}
	r.notes.reset(b.seen)
	t, ok, err := r.textOf(k, EntryModel, b.shapes(EntryModel, b.seen))
	if t == nil {
		return ok, err
	}
	r.notesInto(&r.notes, t)
	blank := t.blank
	r.model = readText{p: r.textEntry(t, TranscriptEntry{Kind: EntryModel}, b.seen), set: true, blank: blank}
	return true, nil
}

// notesInto adds to s each note, late call and wake t, a hook's context just
// read, carries where it opens with an engine heading: none past the bound
// (R1).
func (r *museReading) notesInto(s *entryShape, t *valueText) {
	if t.past || t.blank || !bytes.Contains(t.held, []byte(headingOpen)) {
		return
	}
	if text := t.text(); opensWithHeading(text) {
		r.b.proseInto(s, EntryBrief, text)
	}
}

// call reads one of a batch's calls: its id, tool and arguments.
func (r *museReading) call(k jsonscan.Kind) error {
	var id string
	var tool toolName
	var input *valueText
	took, err := r.objectField(k, func(name string, k jsonscan.Kind) error {
		var ok bool
		var err error
		switch name {
		case "call_id":
			id, ok, err = r.wordField(k)
		case "name":
			tool, ok, err = r.toolName(&r.name, k)
		case "args":
			if input != nil {
				input.drop()
			}
			input, err = r.args(k)
			ok = true
		default:
			return r.scan.Skip()
		}
		r.mistyped = r.mistyped || !ok
		return err
	})
	r.mistyped = r.mistyped || !took
	if err != nil || r.mistyped {
		if input != nil {
			input.drop()
		}
		return err
	}
	r.calls.add(r.b, input, tool, id)
	return nil
}

// args reads a call's arguments, any value, into r.input as the text its entry
// shows: a string, as Muse records the object the model wrote, read as JSON
// within the value bound ([compactJSON]) and as written past it (R3); any
// other value as written and compacted; null, none.
func (r *museReading) args(k jsonscan.Kind) (*valueText, error) {
	if k == jsonscan.Null {
		return nil, nil
	}
	t := r.input.begin(r.b, TranscriptEntry{Kind: EntryToolCall}, r.calls.shapes(r.b))
	if k == jsonscan.String {
		if err := r.read(t); err != nil {
			t.drop()
			return nil, err
		}
		if !t.past {
			text := compactJSON(json.RawMessage(t.text()))
			t.held = append(t.held[:0], text...)
		}
		return t, nil
	}
	r.compact = jsonCompactor{w: t, buf: r.compact.buf}
	err := r.scan.Tee(&r.compact)
	if err == nil {
		err = r.scan.Skip()
	}
	if err != nil {
		t.drop()
		return nil, err
	}
	return t, nil
}

// result reads one of a batch's results into the results' reading: its text,
// blank or not, and the call it answers.
func (r *museReading) result(k jsonscan.Kind) error {
	b, s := r.b, &r.results
	e := TranscriptEntry{Kind: EntryToolResult}
	var id string
	p := b.held(e, "", s.next())
	took, err := r.objectField(k, func(name string, k jsonscan.Kind) error {
		var ok bool
		var err error
		switch name {
		case "tool_call_id":
			id, ok, err = r.wordField(k)
		case "text":
			var t *valueText
			t, ok, err = r.textOf(k, EntryToolResult, s.shapes(b, EntryToolResult))
			switch {
			case t != nil:
				p = r.textEntry(t, e, s.next())
			case ok:
				p = b.held(e, "", s.next())
			}
		default:
			return r.scan.Skip()
		}
		r.mistyped = r.mistyped || !ok
		return err
	})
	r.mistyped = r.mistyped || !took
	if err == nil && !r.mistyped {
		s.addEvent(b, p, time.Time{}, claudeEvent{kind: eventResult, id: id})
	}
	return err
}

// update reads one of a batch's entries, its update's source and text, into
// the batch's reading: each note, late call and wake a hook's context
// carries where it opens with an engine heading, none past the bound (R1).
func (r *museReading) update(k jsonscan.Kind) error {
	took, err := r.objectField(k, func(name string, k jsonscan.Kind) error {
		if name != "update" {
			return r.scan.Skip()
		}
		var source string
		var t *valueText
		ok, err := r.objectField(k, func(name string, k jsonscan.Kind) error {
			var took bool
			var err error
			switch name {
			case "source":
				source, took, err = r.wordField(k)
			case "text":
				if t != nil {
					t.drop()
				}
				t, took, err = r.textOf(k, EntryNote, false)
			default:
				return r.scan.Skip()
			}
			r.mistyped = r.mistyped || !took
			return err
		})
		r.mistyped = r.mistyped || !ok
		if t != nil {
			if err == nil && source == museHookSource {
				r.notesInto(&r.batch, t)
			}
			t.drop()
		}
		return err
	})
	r.mistyped = r.mistyped || !took
	return err
}

// commit commits the reading the event's kind picks: none for a record of
// another payload type or of an envelope its decode refuses, and one whose
// payload it refuses said.
func (r *museReading) commit() error {
	b := r.b
	if !r.envelope || r.payloadType != "runtime.session" {
		return nil
	}
	if !r.payload || !r.shaped {
		return b.unread(museUnshapedRecord)
	}
	at := museTime(r.at)
	s := &r.s
	s.reset(b.seen)
	switch r.kind {
	case "started":
		s = &r.brief
	case "assistant_message_committed":
		r.textInto(s, r.model)
	case "assistant_tool_calls_committed":
		return r.calls.commitAfter(b, false, at)
	case "tool_result_batch_committed":
		s = &r.results
	case "context_block_updated":
		if r.source == museHookSource {
			s = &r.notes
		}
	case "context_block_update_batch_committed":
		s = &r.batch
	case "context_compaction_installed":
		r.textInto(s, r.summary)
	}
	return b.commitThread(s, at)
}

// textInto adds t to s, unless it is blank or was not read.
func (r *museReading) textInto(s *entryShape, t readText) {
	if t.set && !t.blank {
		s.add(r.b, t.p, time.Time{})
	}
}

// museHookSource is the source of a context block a hook's answer added
// (measured on 1.4.25).
const museHookSource = "runtime_hook"

// museTime is a record's `recorded_at`, microseconds since the epoch, in UTC;
// zero where it carries none, or none an answer can carry ([carriedInstant]).
func museTime(micros int64) time.Time {
	if micros <= 0 {
		return time.Time{}
	}
	return carriedInstant(time.UnixMicro(micros).UTC())
}
