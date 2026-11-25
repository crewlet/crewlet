package executor

import (
	"archive/tar"
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/jsonscan"
)

// transcript reads a Cursor archive ([cursorArchiveFormat]): its conversation,
// a message a line, as the session's entries. Any other member is passed
// unread.
func (CursorAgent) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
	tr := tar.NewReader(archive)
	b.handed.r = tr
	r := newCursorReading(b, bufio.NewReaderSize(&b.handed, lineChunkBytes))
	for {
		h, err := tr.Next()
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return fmt.Errorf("%w: %w", errNotTar, err)
		}
		if !cursorHistoryMember(h.Name) {
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

// cursorHistoryMember reports whether an archive's member named name is a
// session's conversation: `cursor-history/<session>.jsonl`.
func cursorHistoryMember(name string) bool {
	parts := strings.Split(path.Clean(name), "/")
	return len(parts) == 2 && parts[0] == cursorArchiveDir && strings.HasSuffix(parts[1], ".jsonl") &&
		parts[1] != ".jsonl"
}

// A CURSOR LINE IS ONE MESSAGE OF ITS ConversationHistory, as the export writes
// it: `{"user"|"assistant"|"tool": {...}}`, its content a list of parts, each
// `{"text":{"text"}}` or `{"toolCall":{"toolCallId","toolName","argsJson"}}`.
// Its members are read by its whole-line decode's fields ([fieldKind]): a line
// one of them refuses, or that holds no message, is no record. A message's text
// is its parts' words joined; a call's arguments, a string of JSON, are
// compacted within the value bound and shown as written past it (R3).

// cursorReading is a person's transcript of one conversation on the bounded
// line walk ([lineWalk.setupBounded]), in recorded order: a user's message is
// the brief, an assistant's its words and a call per tool call, a tool's its
// result, failed where Cursor recorded it so. A line holding more than one
// message is read as its user's, else its assistant's.
type cursorReading struct {
	lineWalk
	mistyped bool
	// Each message as written last: whether it is one (an object, not null),
	// and its words; an assistant's calls, a tool's call and failure.
	user, assistant, tool bool
	brief, words, result  textJoin
	calls                 callList
	callID                string
	failed                bool
	name, input           valueText
	s                     entryShape
}

// newCursorReading is a person's reading into b of the members lines reads.
func newCursorReading(b *transcriptBuilder, lines *bufio.Reader) *cursorReading {
	r := &cursorReading{}
	r.setupBounded(b, r, lines)
	return r
}

// cursorParts is how a message's parts are joined: every text part's words
// that are not blank, a blank line between each two.
var cursorParts = joinRule{anyKind: true, sep: "\n\n", dropBlank: true}

// start starts the record the next line holds.
func (r *cursorReading) start() {
	r.mistyped, r.user, r.assistant, r.tool = false, false, false, false
	r.userStart()
	r.assistantStart()
	r.toolStart()
}

// userStart, assistantStart and toolStart start a message of their kind: one
// written again replaces it.
func (r *cursorReading) userStart() {
	r.brief.start(r.b, TranscriptEntry{Kind: EntryBrief}, r.b.shapes(EntryBrief, r.b.seen), cursorParts)
}

func (r *cursorReading) assistantStart() {
	r.words.start(r.b, TranscriptEntry{Kind: EntryModel}, r.b.shapes(EntryModel, r.b.seen), cursorParts)
	r.calls.reset(r.b.seen)
}

func (r *cursorReading) toolStart() {
	r.result.start(r.b, TranscriptEntry{Kind: EntryToolResult}, r.b.shapes(EntryToolResult, r.b.seen),
		cursorParts)
	r.callID, r.failed = "", false
}

func (*cursorReading) own(*valueText) error { return nil }

func (r *cursorReading) linesPast() error { return r.b.linesPast("record", "lines") }

// took notes whether a member's field took its value: one it refuses refuses
// the line.
func (r *cursorReading) took(ok bool) { r.mistyped = r.mistyped || !ok }

// record reads a line's messages.
func (r *cursorReading) record() error {
	return r.members(func(name string, k jsonscan.Kind) error {
		var ok bool
		var err error
		switch name {
		case "user":
			r.userStart()
			r.user = k == jsonscan.ObjectStart
			ok, err = r.message(k, &r.brief, nil, r.userStart)
		case "assistant":
			r.assistantStart()
			r.assistant = k == jsonscan.ObjectStart
			ok, err = r.message(k, &r.words, &r.calls, r.assistantStart)
		case "tool":
			r.toolStart()
			r.tool = k == jsonscan.ObjectStart
			ok, err = r.toolMessage(k)
		default:
			return r.scan.Skip()
		}
		r.took(ok)
		return err
	})
}

// message reads a user's or an assistant's message, its content's words
// joined into j and, where calls is not nil, its tool calls added to it;
// restart starts its content again where it is written again.
func (r *cursorReading) message(k jsonscan.Kind, j *textJoin, calls *callList, restart func()) (bool, error) {
	return r.objectField(k, func(name string, k jsonscan.Kind) error {
		if name != "content" {
			return r.scan.Skip()
		}
		restart()
		ok, err := r.content(j, k, calls)
		r.took(ok)
		return err
	})
}

// toolMessage reads a tool's message: its call, its content's words and its
// failure.
func (r *cursorReading) toolMessage(k jsonscan.Kind) (bool, error) {
	return r.objectField(k, func(name string, k jsonscan.Kind) error {
		var ok bool
		var err error
		switch name {
		case "toolCallId":
			r.callID, ok, err = r.wordField(k)
		case "toolName":
			ok, err = r.typedField(k, fieldString)
		case "content":
			r.result.start(r.b, TranscriptEntry{Kind: EntryToolResult}, r.b.shapes(EntryToolResult, r.b.seen),
				cursorParts)
			ok, err = r.content(&r.result, k, nil)
		case "isError":
			r.failed, ok, err = r.flagField(k)
		default:
			return r.scan.Skip()
		}
		r.took(ok)
		return err
	})
}

// content reads a message's content, a list of parts or null, and reports
// whether its field takes it.
func (r *cursorReading) content(j *textJoin, k jsonscan.Kind, calls *callList) (bool, error) {
	return r.listField(k, func(k jsonscan.Kind) error { return r.part(j, k, calls) })
}

// part reads one part of a content, an object or null: its text joined into
// j, and its tool call added to calls where calls is not nil. A text written
// again replaces it; past the bound it loses the join (R2).
func (r *cursorReading) part(j *textJoin, k jsonscan.Kind, calls *callList) error {
	j.partStart()
	texts := 0
	var call bool
	var id string
	var tool toolName
	var input *valueText
	ok, err := r.objectField(k, func(name string, k jsonscan.Kind) error {
		var ok bool
		var err error
		switch name {
		case "text":
			if texts++; texts > 1 {
				j.partText()
			}
			ok, err = r.objectField(k, func(name string, k jsonscan.Kind) error {
				if name != "text" {
					return r.scan.Skip()
				}
				j.partText()
				switch k {
				case jsonscan.String:
					return r.read(j)
				case jsonscan.Null:
					return nil
				}
				r.took(false)
				return r.scan.Skip()
			})
		case "toolCall":
			if input != nil {
				input.drop()
			}
			call, id, tool, input = k == jsonscan.ObjectStart, "", toolName{}, nil
			ok, err = r.objectField(k, func(name string, k jsonscan.Kind) error {
				var took bool
				var readErr error
				switch {
				case name != "toolCallId" && name != "toolName" && name != "argsJson":
					return r.scan.Skip()
				case calls == nil:
					took, readErr = r.typedField(k, fieldString)
				case name == "toolCallId":
					id, took, readErr = r.wordField(k)
				case name == "toolName":
					tool, took, readErr = r.toolName(&r.name, k)
				default:
					if input != nil {
						input.drop()
					}
					input, took, readErr = r.args(k, calls)
				}
				r.took(took)
				return readErr
			})
		default:
			return r.scan.Skip()
		}
		r.took(ok)
		return err
	})
	j.partEnd()
	r.took(ok)
	switch {
	case err == nil && call && calls != nil:
		calls.add(r.b, input, tool, id)
	case input != nil:
		input.drop()
	}
	return err
}

// args reads a call's arguments, a string of JSON or null (none), into the
// call's input: compacted within the value bound ([compactJSON]), as written
// past it (R3). It reports whether a string field takes the value.
func (r *cursorReading) args(k jsonscan.Kind, calls *callList) (*valueText, bool, error) {
	switch k {
	case jsonscan.Null:
		return nil, true, nil
	case jsonscan.String:
	default:
		return nil, false, r.scan.Skip()
	}
	t := r.input.begin(r.b, TranscriptEntry{Kind: EntryToolCall}, calls.shapes(r.b))
	if err := r.read(t); err != nil {
		t.drop()
		return nil, true, err
	}
	if !t.past {
		t.held = append(t.held[:0], compactJSON(t.held)...)
	}
	return t, true, nil
}

// commit commits the message the line holds, a line of none or one a field
// refused said as no record.
func (r *cursorReading) commit() error {
	b, s := r.b, &r.s
	if r.mistyped || !r.user && !r.assistant && !r.tool {
		return r.refused()
	}
	s.reset(b.seen)
	switch {
	case r.user:
		if r.brief.said() {
			s.add(b, textAs(b, &r.brief, s, EntryBrief), time.Time{})
		}
	case r.assistant:
		words := r.words.said()
		if words {
			s.add(b, textAs(b, &r.words, s, EntryModel), time.Time{})
		}
		if err := b.commitThread(s, time.Time{}); err != nil {
			return err
		}
		return r.calls.commitAfter(b, words, time.Time{})
	default:
		// A JOIN OF NO WORDS IS EMPTY, whatever blank part runs past the
		// bound (R2).
		p := b.held(TranscriptEntry{Kind: EntryToolResult}, "", s.next())
		if r.result.said() {
			p = textAs(b, &r.result, s, EntryToolResult)
		}
		if p.entry.Kind == EntryToolResult {
			p.entry.Failed = r.failed
		}
		s.addEvent(b, p, time.Time{}, claudeEvent{kind: eventResult, id: r.callID})
	}
	return b.commitThread(s, time.Time{})
}
