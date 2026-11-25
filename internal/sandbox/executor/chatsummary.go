package executor

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/crewlet/crewlet/internal/jsonscan"
)

// chatSummaryLines is a transcript's own reading of a Gemini CLI or Qwen Code
// chat file ([chatReading]), following the count's walk of the file
// ([chatCopyCount]) or handed a record by a walk for notes, so the newest
// summary is read in that one walk by the decode the transcript reads it by.
// Its builder is its own, holding none of what it reads.
type chatSummaryLines struct {
	chatReading
	raw bytes.Reader
	// err is what ended the reading, after which it reads no line.
	err error
}

// errChatSummaryLines is a chat file past the lines and elements a read reads.
var errChatSummaryLines = fmt.Errorf("the chat file holds more than the %d lines and elements a read reads",
	TranscriptReadLines)

// setup makes c rd's reading.
func (c *chatSummaryLines) setup(rd lineRecords) {
	c.setupBounded(newTranscriptBuilder(0, TranscriptReadBounds(), TranscriptFilter{}), rd,
		bufio.NewReaderSize(nil, lineChunkBytes))
}

// take reads raw, the file's next line, unless the reading has ended.
func (c *chatSummaryLines) take(ctx context.Context, raw []byte) {
	if c.err != nil {
		return
	}
	c.raw.Reset(raw)
	c.lines.Reset(&c.raw)
	_, c.err = c.line(ctx)
}

// follow reads r, the chat file as the count's walk hands it on
// ([chatFileFollowed]), to its end or the reading's: its last line, one with
// no break, unread, as the walk passes a torn one over.
func (c *chatSummaryLines) follow(ctx context.Context, r io.Reader) {
	c.lines.Reset(r)
	c.tornUnread = true
	for c.err == nil {
		var ended bool
		if ended, c.err = c.line(ctx); ended {
			return
		}
	}
}

func (*chatSummaryLines) own(*valueText) error { return nil }

func (*chatSummaryLines) linesPast() error { return errChatSummaryLines }

// qwenSummaryReading reads a Qwen Code chat file's compaction records for the
// newest summary by the transcript's one decode of one ([qwenCompaction]).
type qwenSummaryReading struct {
	chatSummaryLines
	newest       *summaryRead
	typ, subtype string
	compaction   qwenCompaction
}

func newQwenSummaryReading(read *summaryRead) *qwenSummaryReading {
	q := &qwenSummaryReading{newest: read}
	q.setup(q)
	return q
}

func (q *qwenSummaryReading) start() {
	q.typ, q.subtype = "", ""
	q.compaction.reset(q.b.valueMax)
}

func (q *qwenSummaryReading) record() error {
	return q.members(func(name string, k jsonscan.Kind) error {
		var err error
		switch name {
		case "type":
			q.typ, err = q.wordValue(k)
		case "subtype":
			q.subtype, err = q.wordValue(k)
		case "systemPayload":
			err = q.compaction.read(&q.chatReading, k)
		default:
			err = q.scan.Skip()
		}
		return err
	})
}

// commit makes a compaction's summary the newest, or one that cannot be said
// where the transcript says it in its place.
func (q *qwenSummaryReading) commit() error {
	if q.typ != "system" || q.subtype != "chat_compression" {
		return nil
	}
	switch words, unread := q.compaction.summary(); {
	case unread != "":
		q.newest.unshaped(unread)
	default:
		q.newest.take(words)
	}
	return nil
}

// geminiSummaryReading reads a Gemini CLI chat file's lines for the replay of
// its newest summary ([geminiSummaries]): a record's id and type and, where its
// content opens a snapshot, its words, as the transcript's reading takes them
// ([chatReading.parts]); a `$patch`'s updates and removals; a `$rewindTo`.
type geminiSummaryReading struct {
	chatSummaryLines
	g       *geminiSummaries
	rec     geminiSnapRecord
	updates []geminiSnapRecord
	removes []string
	rewind  string
	// patched and rewound say the line is a patch or a rewind; lost that it
	// is one this replay cannot follow.
	patched, rewound, lost bool
}

// geminiSnapRecord is one copy of a record: its id, type and snapshot's words.
type geminiSnapRecord struct {
	id, typ string
	snap    summaryText
}

func newGeminiSummaryReading(g *geminiSummaries) *geminiSummaryReading {
	r := &geminiSummaryReading{g: g}
	r.setup(r)
	return r
}

func (r *geminiSummaryReading) start() {
	r.rec.id, r.rec.typ, r.rewind = "", "", ""
	r.rec.snap.reset()
	r.updates, r.removes = r.updates[:0], r.removes[:0]
	r.patched, r.rewound, r.lost = false, false, false
}

func (r *geminiSummaryReading) record() error {
	return r.members(func(name string, k jsonscan.Kind) error {
		switch name {
		case "$patch":
			r.patched = true
			return r.patch(k)
		case "$rewindTo":
			r.rewound = true
			var err error
			r.rewind, err = r.wordValue(k)
			return err
		}
		return r.recordMember(&r.rec, name, k)
	})
}

// recordMember reads one member of a record's copy into rec.
func (r *geminiSummaryReading) recordMember(rec *geminiSnapRecord, name string, k jsonscan.Kind) error {
	var err error
	switch name {
	case "id":
		rec.id, err = r.wordValue(k)
	case "type":
		rec.typ, err = r.wordValue(k)
	case "content":
		rec.snap.reset()
		err = r.parts(k, &chatSinks{snap: &rec.snap})
	default:
		err = r.scan.Skip()
	}
	return err
}

// patch reads a `$patch`: its updates, each a record's copy, and the ids it
// removes; one that orders the records, or of no shape, is lost.
func (r *geminiSummaryReading) patch(k jsonscan.Kind) error {
	if k != jsonscan.ObjectStart {
		r.lost = true
		return r.scan.Skip()
	}
	list := func(k jsonscan.Kind, each func(k jsonscan.Kind) (bool, error)) error {
		if k != jsonscan.ArrayStart {
			r.lost = r.lost || k != jsonscan.Null
			return r.scan.Skip()
		}
		return r.elements(each)
	}
	return r.members(func(name string, k jsonscan.Kind) error {
		switch name {
		case "updates":
			r.updates = r.updates[:0]
			return list(k, func(k jsonscan.Kind) (bool, error) {
				if k != jsonscan.ObjectStart {
					r.lost = true
					return true, r.scan.Skip()
				}
				r.updates = append(r.updates, geminiSnapRecord{})
				u := &r.updates[len(r.updates)-1]
				return true, r.members(func(name string, k jsonscan.Kind) error { return r.recordMember(u, name, k) })
			})
		case "removeIds":
			r.removes = r.removes[:0]
			return list(k, func(k jsonscan.Kind) (bool, error) {
				id, err := r.wordValue(k)
				if id != "" {
					r.removes = append(r.removes, id)
				}
				return true, err
			})
		case "orderIds":
			r.lost = r.lost || k != jsonscan.Null
		}
		return r.scan.Skip()
	})
}

func (r *geminiSummaryReading) commit() error {
	g := r.g
	switch {
	case r.lost:
		g.lost("a patch of the chat file is of no shape this read follows")
	case r.patched:
		for i := range r.updates {
			g.record(&r.updates[i])
		}
		for _, id := range r.removes {
			g.remove(id)
		}
	case r.rewound:
		g.rewindTo(r.rewind)
	default:
		g.record(&r.rec)
	}
	if len(g.place) > geminiCountIDs {
		return errors.New("the chat file holds more records than a read holds")
	}
	return nil
}

// qwenSummaries is a walk for notes' reading of a Qwen Code chat file's newest
// summary ([qwenNotesHeld]): each compaction record's system payload, teed as
// the walk reads it, read after its line by the transcript's decode
// ([qwenSummaryReading]), so receipts and summary take one walk.
type qwenSummaries struct {
	lines   *qwenSummaryReading
	payload qwenPayload
	record  []byte
}

func newQwenSummaries(newest *summaryRead) *qwenSummaries {
	return &qwenSummaries{lines: newQwenSummaryReading(newest)}
}

// qwenCompactionHead opens the compaction record a payload is read as.
const qwenCompactionHead = `{"type":"system","subtype":"chat_compression"`

// line reads the newest summary off the line l just read: a compaction's
// payload by the transcript's decode; a line past the bound, which may have
// been one, or a payload past [qwenPayloadMaxBytes], unsaid.
func (q *qwenSummaries) line(ctx context.Context, line noteLine, l *qwenNoteLine) {
	newest := q.lines.newest
	switch {
	case line.past:
		newest.unshaped(fmt.Sprintf("a line of the chat file runs past the %d bytes a line is read to", maxLineBytes))
	case !line.object || l.typ != "system" || l.subtype != "chat_compression":
	case q.payload.past:
		newest.unshaped(fmt.Sprintf("a compaction record's payload runs past the %d bytes a read holds of one",
			qwenPayloadMaxBytes))
	default:
		q.record = append(q.record[:0], qwenCompactionHead...)
		if q.payload.set {
			q.record = append(append(q.record, `,"systemPayload":`...), q.payload.buf...)
		}
		q.lines.take(ctx, append(q.record, '}'))
	}
}

// qwenPayloadMaxBytes bounds the system payload a walk for notes holds to read
// a compaction's summary: twice TranscriptValueMaxBytes, the summary's texts
// the decode holds at most and as much again of the history kept after them.
// A judgment, which no measured compaction bounds yet.
const qwenPayloadMaxBytes = 2 * TranscriptValueMaxBytes

// qwenPayload is a record's system payload as written ([jsonscan.Scanner.Tee]),
// held to [qwenPayloadMaxBytes]: past it, none. A nil one reads nothing.
type qwenPayload struct {
	buf       []byte
	set, past bool
}

func (p *qwenPayload) reset() {
	if p != nil {
		p.buf, p.set, p.past = p.buf[:0], false, false
	}
}

// tee holds the value w's scanner is at as it is read.
func (p *qwenPayload) tee(w *lineWalk) error {
	if p == nil {
		return nil
	}
	p.reset()
	p.set = true
	return w.scan.Tee(p)
}

func (p *qwenPayload) Write(b []byte) (int, error) {
	switch {
	case p.past:
	case len(p.buf)+len(b) > qwenPayloadMaxBytes:
		p.buf, p.past = p.buf[:0], true
	default:
		p.buf = append(p.buf, b...)
	}
	return len(b), nil
}
