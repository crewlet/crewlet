package executor

import (
	"bufio"
	"context"
	"io"
	"time"

	"github.com/crewlet/crewlet/internal/jsonscan"
)

// GEMINI CLI'S SESSION FILE, read as a transcript: one JSON object a line, a
// header, `$set` lines, and entries, each `user` or `gemini` (measured on
// 0.70.0). A `user` entry's content is the API's parts
// ([chatReading.parts]): the harness's own session context first, then the
// brief, each call's response, and the note a finish was blocked with. A
// `gemini` entry is one response, its words a string and its calls in
// `toolCalls`; 0.70.0 writes a response up to three times as it goes (its
// words, its calls scheduled, its calls run), each a whole copy under the
// response's id, so the latest copy is the response, read once the next entry
// begins.

// A COMPRESSION READS AS CLAUDE CODE'S COMPACTION DOES ([claudeReading]):
// every entry before it stays where it was read, and its summary, the user
// entry its three lines name ([geminiCompressionWatch]), is [EntrySummary],
// its first text a thought is not. The rest of the sync, up to the `$patch`
// (or `$rewindTo`) closing it and naming the entries a resume drops, which
// stay on disk, restates what was already read, never read again: its copies
// would read the kept entries twice and confirm a note's one delivery as two.

// geminiSessionContext opens the user entry Gemini CLI writes ahead of the
// brief, its working directory and date: the harness's own words, never the
// run's.
const geminiSessionContext = "<session_context>"

// geminiSession reads one Gemini CLI main session from r into b on the
// bounded line walk ([lineWalk.setupBounded]), entry by entry, until b's page
// is whole or r ends.
func (b *transcriptBuilder) geminiSession(ctx context.Context, r io.Reader) error {
	g := &geminiReading{}
	g.held, g.over, g.after = &g.turns[0], &g.turns[1], &g.turns[2]
	g.setupBounded(b, g, bufio.NewReaderSize(r, lineChunkBytes))
	if err := g.member(ctx); err != nil {
		return err
	}
	return g.flush()
}

// geminiReading is a person's transcript of a Gemini CLI session: each line's
// members read loosely, as its whole-line decode read them. A RESPONSE IS
// HELD, SHAPED, UNTIL THE NEXT ENTRY BEGINS, so a line is shaped in each
// reading its `type` and `id` may pick: a copy of the response held (over), a
// response after it (after), a user entry after it, and a compression's
// summary there. A line said in its place ends the response, as an entry does.
type geminiReading struct {
	chatReading
	id          recordWord
	typ, stamp  string
	patched     bool
	restating   bool
	heldOn      bool
	turns       [3]geminiTurn
	held        *geminiTurn
	over, after *geminiTurn
	user, sum   entryShape
	compression geminiCompressionWatch
}

// geminiTurn is a response as shaped: its id and time, its words' entry (at
// most one) and its calls after or without it, and where its last ends.
type geminiTurn struct {
	id     recordWord
	at     time.Time
	words  entryShape
	calls  callList
	worded bool
	end    int
}

// reset starts a response whose words take the ordinal base.
func (t *geminiTurn) reset(base int) {
	t.words.reset(base)
	t.calls.reset(base)
}

func (r *geminiReading) start() {
	r.id, r.typ, r.stamp, r.patched = recordWord{}, "", "", false
	r.firstSeen = false
	r.compression.next(r.lines)
	over, after := r.b.seen, r.b.seen
	if r.heldOn {
		after = r.held.end
	}
	r.over.reset(over)
	r.after.reset(after)
	r.user.reset(after)
	r.sum.reset(after)
}

func (*geminiReading) own(*valueText) error { return nil }

func (r *geminiReading) linesPast() error {
	if err := r.flush(); err != nil {
		return err
	}
	return r.b.linesPast("record", "lines")
}

func (r *geminiReading) said() error { return r.flush() }

// record reads a line's id, type, time, content, calls and sync marks; a line
// read while a compression's sync restates the history only for its marks.
func (r *geminiReading) record() error {
	return r.members(func(name string, k jsonscan.Kind) error {
		var err error
		switch name {
		case "id":
			r.id, _, err = r.idField(k)
		case "type":
			r.typ, err = r.wordValue(k)
		case "timestamp":
			r.stamp, err = r.wordValue(k)
		case "$patch", "$rewindTo":
			r.patched = true
			err = r.scan.Skip()
		case "content":
			err = r.content(k)
		case "toolCalls":
			err = r.toolCalls(k)
		default:
			err = r.scan.Skip()
		}
		return err
	})
}

// content reads a line's content in every reading: its parts as a user
// entry's and the first text as a summary, and as a string, the response's
// words.
func (r *geminiReading) content(k jsonscan.Kind) error {
	if r.restating {
		return r.scan.Skip()
	}
	r.over.words.reset(r.over.words.base)
	r.after.words.reset(r.after.words.base)
	r.user.reset(r.user.base)
	r.sum.reset(r.sum.base)
	sinks := chatSinks{user: &r.user, own: geminiSessionContext, words: []*entryShape{&r.over.words, &r.after.words}}
	if r.compression.opens {
		sinks.summary = &r.sum
	}
	return r.parts(k, &sinks)
}

// toolCalls reads a response's calls into both its readings: each object of
// the list a call, its id, its tool's name and its input as written and
// compacted.
func (r *geminiReading) toolCalls(k jsonscan.Kind) error {
	if r.restating {
		return r.scan.Skip()
	}
	r.over.calls.reset(r.over.words.base)
	r.after.calls.reset(r.after.words.base)
	if k != jsonscan.ArrayStart {
		return r.scan.Skip()
	}
	shapes := []*entryShape{&r.over.calls.after, &r.over.calls.alone, &r.after.calls.after, &r.after.calls.alone}
	return r.elements(func(k jsonscan.Kind) (bool, error) {
		if k != jsonscan.ObjectStart {
			return true, r.scan.Skip()
		}
		var id string
		var tool toolName
		var input *valueText
		err := r.members(func(name string, k jsonscan.Kind) error {
			var err error
			switch name {
			case "id":
				id, err = r.wordValue(k)
			case "name":
				tool, _, err = r.toolName(&r.name, k)
			case "args":
				shaped := false
				for _, s := range shapes {
					shaped = shaped || s.shapes(r.b, EntryToolCall)
				}
				input = r.args.begin(r.b, TranscriptEntry{Kind: EntryToolCall}, shaped)
				r.compactor = jsonCompactor{w: input, buf: r.compactor.buf}
				if err = r.scan.Tee(&r.compactor); err == nil {
					err = r.scan.Skip()
				}
			default:
				err = r.scan.Skip()
			}
			return err
		})
		if err != nil {
			return true, err
		}
		addCall(r.b, input, tool, id, shapes...)
		return true, nil
	})
}

// commit commits the reading the line picks: none for a line closing a sync
// or read while one restates; a response's copy held in place of the one
// held, or after it once that is committed; a user entry, once the response
// held is committed, as a compression's summary or as its parts.
func (r *geminiReading) commit() error {
	if r.patched {
		r.restating = false
		return nil
	}
	if r.restating {
		return nil
	}
	at := chatStamp(r.stamp)
	switch r.typ {
	case "gemini":
		turn := &r.over
		if r.heldOn && r.held.id != r.id {
			if err := r.flush(); err != nil {
				return err
			}
			turn = &r.after
		}
		t := *turn
		t.id, t.at, t.worded = r.id, at, t.words.n > 0
		t.end = t.calls.alone.next()
		if t.worded {
			t.end = t.calls.after.next()
		}
		r.held, *turn, r.heldOn = t, r.held, true
	case "user":
		if err := r.flush(); err != nil {
			return err
		}
		if r.compression.summary(r.lines) {
			// Whole: a heading inside a summary heads nothing.
			r.restating = true
			return r.b.commitThread(&r.sum, at)
		}
		return r.b.commitThread(&r.user, at)
	}
	return nil
}

// flush commits the response held, its words and then its calls.
func (r *geminiReading) flush() error {
	if !r.heldOn {
		return nil
	}
	r.heldOn = false
	t := r.held
	if err := r.b.commitThread(&t.words, t.at); err != nil {
		return err
	}
	return t.calls.commitAfter(r.b, t.worded, t.at)
}
