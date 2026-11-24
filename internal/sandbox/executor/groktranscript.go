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
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/jsonscan"
)

// transcript reads a Grok Build archive ([grokArchiveFormat]): each session's
// record, `<key>/<session>/chat_history.jsonl`, a line at a time, as the
// session's entries, the first its main thread and each subagent's, after it,
// a thread named by its session. Any other member is passed unread. Every
// record's lines, a subagent's included, count against
// [TranscriptBounds.ReadLines].
func (Grok) transcript(ctx context.Context, b *transcriptBuilder, archive io.Reader) error {
	tr := tar.NewReader(archive)
	b.handed.r = tr
	r := newGrokReading(b, bufio.NewReaderSize(&b.handed, lineChunkBytes))
	main := ""
	for {
		h, err := tr.Next()
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return fmt.Errorf("%w: %w", errNotTar, err)
		}
		parts := strings.Split(path.Clean(h.Name), "/")
		if len(parts) != 3 || parts[0] == ".." || parts[1] == ".." || parts[2] != "chat_history.jsonl" {
			continue
		}
		if main == "" {
			main = parts[1]
		}
		b.thread = ""
		if parts[1] != main {
			b.thread = parts[1]
		}
		if h.Typeflag != tar.TypeReg {
			if err := b.unread("a transcript of the harness's record is not a plain file in the archive, " +
				"and was not read"); err != nil {
				return err
			}
			continue
		}
		r.lines.Reset(&b.handed)
		if err := r.member(ctx); err != nil {
			return err
		}
	}
}

// grokReading is a person's transcript of a chat_history.jsonl on the bounded
// line walk ([lineWalk.setupBounded]), in recorded order: a person's prompt
// (a user line of no synthetic_reason with its prompt_index, or the tags a
// compaction keeps it in without one) is the brief, or each engine-headed part
// it carries ([headedIn]); a compaction's summary ([grokReading.summary]) the
// summary; a hook's words the run's settings answered with ([grokHookText])
// each note, late call and wake they carry; an assistant line the model's
// words and a call each, a tool result its call's result. The record carries
// no time. A line's members are read by its whole-line decode's fields
// ([grokLine], [fieldKind]): a line one refuses is no record, and null one of
// no members. Its content is a string or its parts' texts joined, whatever
// their type, none where its decode refuses it.
type grokReading struct {
	lineWalk
	typ, callID      string
	reason           recordWord
	prompt, mistyped bool
	// words is the content's text; contentOK whether its decode reads one,
	// and framed whether the line's type, read before it, is a user's.
	words             textJoin
	frame             textFrame
	contentOK, framed bool
	calls             callList
	name, input       valueText
	s                 entryShape
}

// grokParts is how Grok joins a content's parts ([grokLine.text]): every
// part's text, whatever its type, one after the other.
var grokParts = joinRule{anyKind: true}

// The tags Grok wraps a person's prompt in.
const (
	grokQueryOpen  = "<user_query>\n"
	grokQueryClose = "\n</user_query>"
)

// grokCompactionReason is the synthetic_reason of the lines a compaction
// writes, and grokUserInfoOpen opens the one of them that is Grok's context
// sent again, not the summary; the prompt it keeps loses its prompt_index
// (measured on 1.0.92, compacted.chat.jsonl).
const (
	grokCompactionReason = "compaction_meta"
	grokUserInfoOpen     = "<user_info>"
)

// newGrokReading is a person's reading into b of the members lines reads.
func newGrokReading(b *transcriptBuilder, lines *bufio.Reader) *grokReading {
	r := &grokReading{}
	r.setupBounded(b, r, lines)
	r.passes = func(k jsonscan.Kind) bool { return k == jsonscan.Null }
	return r
}

// start starts the record the next line holds.
func (r *grokReading) start() {
	r.typ, r.callID, r.reason, r.prompt, r.mistyped = "", "", recordWord{}, false, false
	r.words.start(r.b, TranscriptEntry{Kind: EntryModel}, false, grokParts)
	r.contentOK, r.framed = false, false
	r.calls.reset(r.b.seen)
}

func (*grokReading) own(*valueText) error { return nil }

func (r *grokReading) linesPast() error { return r.b.linesPast("record", "lines") }

// record reads a line's type, reason, prompt, content, calls and result's
// call.
func (r *grokReading) record() error {
	return r.members(func(name string, k jsonscan.Kind) error {
		var ok bool
		var err error
		switch name {
		case "type":
			r.typ, ok, err = r.wordField(k)
		case "synthetic_reason":
			r.reason, ok, err = r.idField(k)
		case "prompt_index":
			_, ok, err = r.intField(k)
			r.prompt = ok && k == jsonscan.Number
		case "tool_call_id":
			r.callID, ok, err = r.wordField(k)
		case "content":
			ok, err = true, r.content(k)
		case "tool_calls":
			r.calls.reset(r.b.seen)
			ok, err = r.listField(k, r.call)
		default:
			return r.scan.Skip()
		}
		r.mistyped = r.mistyped || !ok
		return err
	})
}

// content reads a line's content: a string, or a list of parts, each an
// object whose `text` it joins, or null. PAST THE VALUE BOUND A USER'S WORDS
// ARE CUT OF THEIR PROMPT'S TAGS AS THEY STREAM ([textFrame]), where the type
// is read before them, as Grok writes it; a line naming its type after them
// keeps the tags there.
func (r *grokReading) content(k jsonscan.Kind) error {
	b := r.b
	shaped := b.shapes(EntryBrief, b.seen) || b.shapes(EntryModel, b.seen) || b.shapes(EntryToolResult, b.seen) ||
		b.shapes(EntrySummary, b.seen)
	r.words.start(b, TranscriptEntry{Kind: EntryModel}, shaped, grokParts)
	if r.framed = r.typ == "user"; r.framed {
		r.frame.frame(grokQueryOpen, grokQueryClose)
		r.words.frame = &r.frame
	}
	var err error
	switch k {
	case jsonscan.String:
		r.contentOK, err = true, r.wholeText(&r.words)
	case jsonscan.ArrayStart:
		r.contentOK = true
		err = r.elements(func(k jsonscan.Kind) (bool, error) {
			took, partErr := r.joinPart(&r.words, k, nil)
			r.contentOK = r.contentOK && took
			return true, partErr
		})
	default:
		r.contentOK, err = false, r.scan.Skip()
	}
	if r.framed {
		r.frame.end()
	}
	return err
}

// call reads one of a line's calls: its id, tool and arguments, a string.
// WITHIN THE VALUE BOUND its arguments are read as JSON, use_tool's as the
// tool they name and its input ([grokUseTool]); past it, as written (R3), of
// the tool the call names.
func (r *grokReading) call(k jsonscan.Kind) error {
	var id string
	var tool toolName
	var input *valueText
	took, err := r.objectField(k, func(name string, k jsonscan.Kind) error {
		var ok bool
		var err error
		switch name {
		case "id":
			id, ok, err = r.wordField(k)
		case "name":
			tool, ok, err = r.toolName(&r.name, k)
		case "arguments":
			if input != nil {
				input.drop()
				input = nil
			}
			switch k {
			case jsonscan.String:
				input = r.input.begin(r.b, TranscriptEntry{Kind: EntryToolCall}, r.calls.shapes(r.b))
				ok, err = true, r.read(input)
			case jsonscan.Null:
				ok = true
			default:
				err = r.scan.Skip()
			}
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
	if input != nil && !input.past {
		text := compactJSON(json.RawMessage(input.text()))
		if tool.raw == "use_tool" {
			if named, in, ok := grokUseTool(input.text()); ok {
				tool, text = heldToolName(named), in
			}
		}
		input.held = append(input.held[:0], text...)
	}
	r.calls.add(r.b, input, tool, id)
	return nil
}

// grokUseTool is the tool use_tool's arguments name and its input compacted
// ([compactJSON]), false where they name none.
func grokUseTool(arguments string) (tool, input string, ok bool) {
	var use struct {
		ToolName  string          `json:"tool_name"`
		ToolInput json.RawMessage `json:"tool_input"`
	}
	if json.Unmarshal([]byte(arguments), &use) != nil || use.ToolName == "" {
		return "", "", false
	}
	return use.ToolName, compactJSON(use.ToolInput), true
}

// commit commits the reading the line's type picks, a line its decode
// refused said as no record.
func (r *grokReading) commit() error {
	b, s, j := r.b, &r.s, &r.words
	if r.mistyped {
		return r.refused()
	}
	s.reset(b.seen)
	switch r.typ {
	case "user":
		prompt := r.reason.empty() && (r.prompt || !j.lost && j.opensWith(grokQueryOpen))
		summary := r.summary()
		kind := EntryBrief
		if summary {
			kind = EntrySummary
		}
		switch text, held := j.text(); {
		case !r.contentOK:
		case held:
			if words, hook := grokHookText(r.reason.text, text); hook {
				r.hookInto(s, words)
			} else if prompt || summary {
				b.proseInto(s, kind, strings.TrimSuffix(strings.TrimPrefix(text, grokQueryOpen), grokQueryClose))
			}
		case prompt || summary:
			r.promptInto(s, kind)
		}
	case "assistant":
		words := r.contentOK && j.said()
		if words {
			s.add(b, textAs(b, j, s, EntryModel), time.Time{})
		}
		if err := b.commitThread(s, time.Time{}); err != nil {
			return err
		}
		return r.calls.commitAfter(b, words, time.Time{})
	case "tool_result":
		p := b.held(TranscriptEntry{Kind: EntryToolResult}, "", s.next())
		if r.contentOK {
			p = textAs(b, j, s, EntryToolResult)
		}
		s.addEvent(b, p, time.Time{}, claudeEvent{kind: eventResult, id: r.callID})
	}
	return b.commitThread(s, time.Time{})
}

// hookInto adds the notes, late calls and wakes a hook's words carry to s,
// without the take's token: none where they open with no engine heading.
// Past the value bound a hook's words carry none (R1).
func (r *grokReading) hookInto(s *entryShape, words string) {
	if token := grokTokenOf(words); token != "" {
		words = strings.TrimSuffix(strings.TrimRight(words, " \n"), token)
	}
	if words = strings.TrimSpace(words); words != "" && opensWithHeading(words) {
		r.b.proseInto(s, EntryBrief, words)
	}
}

// summary reports whether a user line is a compaction's summary: one of
// [grokCompactionReason] that does not open with [grokUserInfoOpen]. One whose
// join was lost cannot be told, and is said in its place.
func (r *grokReading) summary() bool {
	return r.reason.is(grokCompactionReason) && (r.words.lost || !r.words.opensWith(grokUserInfoOpen))
}

// promptInto adds a person's prompt, or a compaction's summary, past the value
// bound to s as kind: one entry (R1), none where it is blank once cut of a
// prompt's tags, or the join said where it was lost (R2).
func (r *grokReading) promptInto(s *entryShape, kind EntryKind) {
	j := &r.words
	switch {
	case j.lost:
		s.add(r.b, j.entry(s.next()), time.Time{})
	case r.framed && !r.frame.worded, !r.framed && j.blank():
	default:
		s.add(r.b, textAs(r.b, j, s, kind), time.Time{})
	}
}
