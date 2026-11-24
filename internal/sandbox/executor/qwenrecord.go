package executor

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"time"

	"github.com/crewlet/crewlet/internal/jsonscan"
)

// QWEN CODE'S CHAT FILE, read as a transcript: one record a line, written as
// the run goes (measured on 0.28.4). A `user` record is the brief, or with
// subtype `mid_turn_user_message` a message the helper handed between steps
// behind Qwen Code's framing (a note, a late call or a wake by its heading,
// [headingFraming]); an `assistant` record is one response, its parts the
// API's ([chatReading.parts]): its reasoning, its words and its calls; a
// `tool_result` record is a call's response, written once; a `system` record
// is Qwen Code's own bookkeeping and never the conversation, but for a
// `chat_compression` record, read as the summary a resume goes on from
// ([qwenCompaction]). Read in file order: a headless turn appends one chain,
// and Qwen Code rewinds a session only at a person's command.

// qwenSession reads one Qwen Code session from r into b on the bounded line
// walk ([lineWalk.setupBounded]), record by record, until b's page is whole or
// r ends.
func (b *transcriptBuilder) qwenSession(ctx context.Context, r io.Reader) error {
	q := &qwenReading{}
	q.setupBounded(b, q, bufio.NewReaderSize(r, lineChunkBytes))
	return q.member(ctx)
}

// qwenReading is a person's transcript of a Qwen Code chat file: each
// record's members read loosely, as its whole-line decode read them, its
// message's parts shaped as a response's and as a user record's until its
// `type` picks one.
type qwenReading struct {
	chatReading
	typ, subtype, stamp string
	// message says the record's message is an object; asst and user are its
	// parts read as a response and as a user record.
	message    bool
	asst, user entryShape
	compaction qwenCompaction
	s          entryShape
}

func (r *qwenReading) start() {
	r.typ, r.subtype, r.stamp, r.message = "", "", "", false
	r.asst.reset(r.b.seen)
	r.user.reset(r.b.seen)
	r.compaction.reset(r.b.valueMax)
}

func (*qwenReading) own(*valueText) error { return nil }

func (r *qwenReading) linesPast() error { return r.b.linesPast("record", "lines") }

// record reads a record's type, subtype, time, message and system payload.
func (r *qwenReading) record() error {
	return r.members(func(name string, k jsonscan.Kind) error {
		var err error
		switch name {
		case "type":
			r.typ, err = r.wordValue(k)
		case "subtype":
			r.subtype, err = r.wordValue(k)
		case "timestamp":
			r.stamp, err = r.wordValue(k)
		case "message":
			err = r.messageValue(k)
		case "systemPayload":
			err = r.compaction.read(&r.chatReading, k)
		default:
			err = r.scan.Skip()
		}
		return err
	})
}

// messageValue reads a record's message, an object's parts in both readings.
func (r *qwenReading) messageValue(k jsonscan.Kind) error {
	r.asst.reset(r.b.seen)
	r.user.reset(r.b.seen)
	if r.message = k == jsonscan.ObjectStart; !r.message {
		return r.scan.Skip()
	}
	sinks := chatSinks{model: &r.asst, user: &r.user}
	return r.members(func(name string, k jsonscan.Kind) error {
		if name != "parts" {
			return r.scan.Skip()
		}
		r.asst.reset(r.b.seen)
		r.user.reset(r.b.seen)
		return r.parts(k, &sinks)
	})
}

// commit commits the reading the record's type picks: a compaction's
// summary, a response's words and calls, or a user record's brief, notes and
// results ("" no text of Qwen Code's own passed over).
func (r *qwenReading) commit() error {
	at := chatStamp(r.stamp)
	switch {
	case r.typ == "system" && r.subtype == "chat_compression":
		return r.compaction.commit(r.b, &r.s, at)
	case !r.message:
	case r.typ == "assistant":
		return r.b.commitThread(&r.asst, at)
	case r.typ == "user", r.typ == "tool_result":
		return r.b.commitThread(&r.user, at)
	}
	return nil
}

// qwenCompaction is a chat_compression record's summary as a person's read
// takes it (measured on 0.28.4): the user text of compressedHistory's first
// summaryPrefixLength entries, one [EntrySummary], its headings heading
// nothing. The prefix's model entry is Qwen Code's canned acknowledgement and a
// copy of the tail's opening call, and every entry after the prefix copies a
// record this file already holds, so neither is read again. A prefix that does
// not read makes every entry the summary's; a payload its decode refuses is
// said. 0.28.4 WRITES THE PREFIX'S LENGTH AFTER THE HISTORY, so the user texts
// are held, joined, with each entry's end, while within the value bound: a
// summary past it is said in its place, never streamed.
type qwenCompaction struct {
	max             int
	object, refused bool
	prefixSet       bool
	prefix          int64
	entries         int
	held            []byte
	// ends is, for each entry that joined texts, its index and the join's
	// length after it, each as a uvarint from the last ones (lastEntry,
	// lastEnd): two or three bytes an entry against the three or more each
	// adds to held, so the two stay within twice the value bound.
	ends               []byte
	lastEntry, lastEnd int
	// over is the first entry whose texts ran the join past the bound, -1 for
	// none.
	over int
	// The entry read: its role and its texts after the join's, and whether
	// they ran past the bound; the part read: where its text begins in entry,
	// and what it holds.
	role                                  string
	entry                                 []byte
	entryPast                             bool
	mark                                  int
	partText, partThought, partPast, word bool
}

// reset starts a payload read at the value bound max.
func (c *qwenCompaction) reset(max int) {
	*c = qwenCompaction{max: max, held: lineScratch(c.held), ends: lineScratch(c.ends), entry: lineScratch(c.entry),
		over: -1}
}

// read reads a record's system payload: its prefix's length and its
// compressed history, each checked by its whole-line decode's field.
func (c *qwenCompaction) read(r *chatReading, k jsonscan.Kind) error {
	c.reset(c.max)
	if c.object = k == jsonscan.ObjectStart; !c.object {
		return r.scan.Skip()
	}
	return r.members(func(name string, k jsonscan.Kind) error {
		switch name {
		case "summaryPrefixLength":
			n, ok, err := r.intField(k)
			c.refused = c.refused || !ok
			c.prefix, c.prefixSet = n, ok && k == jsonscan.Number
			return err
		case "compressedHistory":
			c.entries, c.held, c.ends, c.over, c.lastEntry, c.lastEnd = 0, c.held[:0], c.ends[:0], -1, 0, 0
			switch k {
			case jsonscan.Null:
				return nil
			case jsonscan.ArrayStart:
				return r.elements(func(k jsonscan.Kind) (bool, error) { return true, c.historyEntry(r, k) })
			}
			c.refused = true
		}
		return r.scan.Skip()
	})
}

// historyEntry reads one entry of the history: null an entry of no role, an
// object its role and parts, any other value one its decode refuses.
func (c *qwenCompaction) historyEntry(r *chatReading, k jsonscan.Kind) error {
	c.role, c.entry, c.entryPast = "", c.entry[:0], false
	switch k {
	case jsonscan.Null:
	case jsonscan.ObjectStart:
		err := r.members(func(name string, k jsonscan.Kind) error {
			var err error
			switch name {
			case "role":
				c.role, err = r.wordValue(k)
			case "parts":
				c.entry, c.entryPast = c.entry[:0], false
				err = c.parts(r, k)
			default:
				err = r.scan.Skip()
			}
			return err
		})
		if err != nil {
			return err
		}
	default:
		c.refused = true
		return r.scan.Skip()
	}
	c.entryEnd()
	return nil
}

// entryEnd ends the entry read: a user's joins its texts while within the
// bound, and its index and the join's length are kept as [qwenCompaction.ends].
func (c *qwenCompaction) entryEnd() {
	e := c.entries
	c.entries++
	switch {
	case c.role != "user" || c.over >= 0 || len(c.entry) == 0 && !c.entryPast:
	case c.entryPast:
		c.over = e
	default:
		c.held = append(c.held, c.entry...)
		c.ends = binary.AppendUvarint(c.ends, uint64(e-c.lastEntry))
		c.ends = binary.AppendUvarint(c.ends, uint64(len(c.held)-c.lastEnd))
		c.lastEntry, c.lastEnd = e, len(c.held)
	}
}

// summaryEnd is the join's length after the last entry before prefix that
// joined texts, 0 for none.
func (c *qwenCompaction) summaryEnd(prefix int) int {
	end, entry, at := 0, 0, 0
	for p := c.ends; len(p) > 0; {
		de, n := binary.Uvarint(p)
		dl, m := binary.Uvarint(p[n:])
		p = p[n+m:]
		if entry += int(de); entry >= prefix {
			break
		}
		at += int(dl)
		end = at
	}
	return end
}

// parts reads an entry's parts as its whole-line decode read them
// ([chatReading.parts]), joining each text a thought is not.
func (c *qwenCompaction) parts(r *chatReading, k jsonscan.Kind) error {
	switch k {
	case jsonscan.String:
		c.partStart()
		if err := c.text(r); err != nil {
			return err
		}
		c.partText = r.trim.n > 0
		c.partEnd()
		return nil
	case jsonscan.ArrayStart:
		return r.elements(func(k jsonscan.Kind) (bool, error) {
			if k != jsonscan.ObjectStart {
				return true, r.scan.Skip()
			}
			c.partStart()
			err := r.members(func(name string, k jsonscan.Kind) error {
				switch name {
				case "text":
					if c.partText = k == jsonscan.String; c.partText {
						return c.text(r)
					}
					c.entry = c.entry[:c.mark]
				case "thought":
					c.partThought = k == jsonscan.True
				}
				return r.scan.Skip()
			})
			c.partEnd()
			return true, err
		})
	}
	return r.scan.Skip()
}

// partStart starts a part.
func (c *qwenCompaction) partStart() {
	c.mark, c.partText, c.partThought, c.partPast, c.word = len(c.entry), false, false, false, false
}

// text reads the current string as the part's text, trimmed, after the
// join's separator, held while the join stays within the bound.
func (c *qwenCompaction) text(r *chatReading) error {
	c.entry, c.partPast = c.entry[:c.mark], false
	if len(c.held) > 0 || c.mark > 0 {
		_, _ = c.Write([]byte("\n\n"))
	}
	err := r.read(r.trim.begin(c, c.max))
	c.word = r.trim.headN > 0
	return err
}

// Write holds p, the part's text's next bytes, while the join stays within
// the bound; past it the part is past.
func (c *qwenCompaction) Write(p []byte) (int, error) {
	switch {
	case c.over >= 0, c.partPast:
	case len(c.held)+len(c.entry)+len(p) > c.max:
		c.partPast = true
	default:
		c.entry = append(c.entry, p...)
	}
	return len(p), nil
}

// partEnd keeps the part's text where it is one a thought is not and holds
// words, the entry past the bound where it ran past it.
func (c *qwenCompaction) partEnd() {
	if !c.partText || c.partThought || !c.word {
		c.entry = c.entry[:c.mark]
		return
	}
	if c.partPast {
		c.entry, c.entryPast = c.entry[:c.mark], true
	}
}

// commit commits the record's summary into s at its line's first ordinal:
// the texts of the entries before the prefix; one past the bound, or a
// payload of no shape, said in its place.
func (c *qwenCompaction) commit(b *transcriptBuilder, s *entryShape, at time.Time) error {
	s.reset(b.seen)
	if !c.object || c.refused {
		s.add(b, b.held(TranscriptEntry{Kind: EntryUnread}, qwenCompactionUnread, s.next()), time.Time{})
		return b.commitThread(s, at)
	}
	prefix := c.entries
	if c.prefixSet && c.prefix >= 0 {
		prefix = int(min(c.prefix, int64(prefix)))
	}
	end := c.summaryEnd(prefix)
	switch {
	case c.over >= 0 && prefix > c.over:
		s.add(b, b.held(TranscriptEntry{Kind: EntryUnread}, fmt.Sprintf("a compaction record's summary runs "+
			"past %s, and was not read", humanSize(int64(c.max))), s.next()), time.Time{})
	case end > 0:
		s.add(b, b.held(TranscriptEntry{Kind: EntrySummary}, string(c.held[:end]), s.next()), time.Time{})
	}
	return b.commitThread(s, at)
}

// qwenCompactionUnread is what a compaction record whose payload has no shape
// this read knows says in its place.
const qwenCompactionUnread = "a compaction record of no shape this read knows"
