package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// WHAT A SESSION'S LAST REQUEST CARRIED, read off its archive as a take streams
// it out of its box ([sandbox.TakenSession.Context]).
//
// A launch that continues a session sends the session's whole context again
// before it reads a word of its brief, so its budget floor is that context and
// the brief, never the static floor alone (internal/engine's launch floor). The
// harness's own record of its last request is that context, cache included:
//
//   - Claude Code's transcript keeps every assistant message with the usage
//     of the request that produced it, so the last one on the main thread (a
//     subagent's messages are sidechains, or files of their own) is the
//     input the session's last request carried: its uncached input, its cache
//     reads and its cache writes;
//   - OpenCode's export keeps every settled assistant message with its
//     tokens, and the newest that measured a prompt is the input the
//     session's last request carried, its input, cache reads and cache
//     writes, by ONE RULE for every reader of an OpenCode session's measure
//     ([measuredPrompt]): this one, and the read that measures the copy a
//     job's box holds as the job ends, off its session's own message list
//     ([openCodeContext]).
//
// READ OFF THE ARCHIVE AS IT IS KEPT, the scrub's output, so it is the session a
// later launch continues and not the box's copy, and in a goroutine of its own
// beside the take, which every byte reaches and which never holds the take up:
// what it cannot read it says nothing about (zero), and it drains the rest.
//
// AND WHAT CLAUDE CODE COUNTS THE SESSION AT before its next request
// ([sandbox.TakenSession.Count], resumed.go), off the same read: a Claude Code
// archive's main thread is walked ONCE, through the line layer ([walkThread]),
// by two readings, the floor's figure ([contextReading]) and the count
// ([resumedReading]). So the meter DECIDES something now (whether a relaunch
// that continues the capture is told it as a cut-off one), and reads its
// archive's members by the layer's rules: a member named absolutely or with a
// `..` element, or a main thread held twice, leaves the count unread, since the
// member it would read need not be the file a resumed session holds; a saved
// compaction under either of its names marks it ([claudePrecompact]); and a
// thread written by a release older than the count's floor is no count of a
// session ([claudeCountFloor]).
//
// THE FLOOR'S FIGURE KEEPS ITS RULE ([contextReading]): the last main-thread
// assistant line's input side, read off each whole line's bytes by
// encoding/json as every capture's figure has been, the one decode of the walk
// outside the layer's options, so the figure every reader of a stamp reads
// ([sandbox.SessionCapture.ContextTokens]) does not move with this change. Two
// differences, both of lines no resume reads: a last piece with no break is no
// line (the box cuts it away before a resume), and a line the layer refuses
// counts nothing, as encoding/json counted nothing of it.
//
// ITS PACE: the walk is fed by the pipe the take writes into, so a meter slower
// than the scrub would hold the take up inside its wait. The walk is one JSON
// scan of a line and a few byte searches, beside the scrub's rewrite, gzip and
// checksum of the same bytes (BenchmarkTheMeterAtItsBounds measures both).

// contextMeter reads an archive written to it and keeps the whole input of the
// last request its transcript records and what Claude Code counts it at.
type contextMeter struct {
	pw     *io.PipeWriter
	done   chan struct{}
	tokens int64
	counts *sandbox.ResumedCount
}

// newContextMeter starts a meter for an archive of layout holding session,
// written by release (the box's word on its harness's release,
// [sandbox.SessionArchive.Version]), read within ctx, the take's own: a take
// cut short ends the walk, whose count is then unread.
func newContextMeter(ctx context.Context, layout archiveLayout, session, release string) *contextMeter {
	pr, pw := io.Pipe()
	m := &contextMeter{pw: pw, done: make(chan struct{})}
	go func() {
		defer close(m.done)
		var (
			n     int64
			count *sandbox.ResumedCount
			err   error
		)
		switch layout {
		case archiveTar:
			n, count, err = meterArchive(ctx, pr, session, release)
		case archiveDocument:
			n, err = lastExportInput(pr)
		}
		if err == nil {
			m.tokens = n
		}
		m.counts = count
		// DRAINED WHATEVER IT FOUND, so the take that feeds it never waits on
		// a meter that stopped reading; the pipe ends when the take finishes.
		_, _ = io.Copy(io.Discard, pr)
	}()
	return m
}

// Write hands the meter what the take wrote. It never fails the take: a meter
// that cannot read the archive measures nothing.
func (m *contextMeter) Write(p []byte) (int, error) {
	_, _ = m.pw.Write(p)
	return len(p), nil
}

// finish ends the archive, err where the take that fed it failed.
func (m *contextMeter) finish(err error) { _ = m.pw.CloseWithError(err) }

// context is what the meter read, once the archive has ended.
func (m *contextMeter) context() int64 {
	<-m.done
	return m.tokens
}

// count is what the meter counted the session at, once the archive has ended:
// nil for an archive of another harness's layout.
func (m *contextMeter) count() *sandbox.ResumedCount {
	<-m.done
	return m.counts
}

// meterArchive is the input of the last request a Claude Code archive's main
// thread of session records, zero where it records none, and what Claude Code
// counts the session at, read off ONE walk of that thread ([walkThread]) by
// both readings. The count is marked where the archive's members say so (the
// file comment) and unread where the archive does not read to its end; err is
// the archive's own, where its gzip or tar framing failed.
func meterArchive(ctx context.Context, r io.Reader, session, release string) (int64, *sandbox.ResumedCount,
	error) {

	var (
		callers countMarks
		count   resumedReading
		figure  contextReading
		walked  bool
	)
	if release == "" || !releaseAtLeast(release, claudeCountFloor) {
		callers = callers.with(sandbox.CountRelease)
	}
	unread := func(err error) (int64, *sandbox.ResumedCount, error) {
		return 0, count.count(callers.with(sandbox.CountUnread)), err
	}
	zr, err := gzip.NewReader(r)
	if err != nil {
		return unread(err)
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return unread(err)
		}
		// A MEMBER NAMED ABSOLUTELY OR WITH A `..` ELEMENT, wherever it
		// falls: the file it restores over need not be the one this reads
		// ([claudeMemberOf]).
		if path.IsAbs(h.Name) || slices.Contains(strings.Split(h.Name, "/"), "..") {
			callers = callers.with(sandbox.CountUnread)
		}
		member, ok := claudeMemberOf(session, path.Clean(h.Name))
		switch {
		case !ok:
			continue
		case member.Kind == memberPrecompact:
			// ITS PRESENCE IS THE FACT, nothing of it read: a file of no bytes
			// is one the harness cannot read, so it holds none.
			if h.Typeflag != tar.TypeReg || h.Size > 0 {
				callers = callers.with(sandbox.CountPrecompact)
			}
			continue
		case member.Kind != memberMainThread || h.Typeflag != tar.TypeReg:
			continue
		case walked:
			// A MAIN THREAD HELD TWICE: which copy a restore leaves is the
			// archive's order, so the count reads neither, and the floor's
			// figure reads the later as it always has.
			callers = callers.with(sandbox.CountUnread)
			figure = contextReading{}
			walkThread(ctx, tr, threadPlainMax, &figure)
			continue
		}
		walked = true
		walkThread(ctx, tr, threadPlainMax, &figure, &count)
	}
	if !walked {
		callers = callers.with(sandbox.CountUnread)
	}
	return figure.context(), count.count(callers), nil
}

// contextReading is the floor's figure as a reading of the walk
// ([threadReading]): the input side of the last main-thread assistant line, by
// the rule every capture's figure has been read by ([assistantInput]), applied
// to each whole line's bytes and to nothing else. A line the layer refuses
// counts nothing, as that rule counted nothing of one; a walk that ended on an
// error leaves no figure, as a read that failed always has.
type contextReading struct {
	last   int64
	failed bool
}

var _ threadReading = (*contextReading)(nil)

// line reads one whole line's figure, where it has one.
func (c *contextReading) line(l threadLine) {
	if n := assistantInput(l.Raw); n > 0 {
		c.last = n
	}
}

// refused counts nothing.
func (c *contextReading) refused(*threadLineError) {}

// end keeps the figure where the walk read the thread to its end.
func (c *contextReading) end(_ bool, err error) { c.failed = err != nil }

// context is the figure, zero where none was read whole.
func (c *contextReading) context() int64 {
	if c.failed {
		return 0
	}
	return c.last
}

// assistantInput is the whole input one transcript line's assistant message
// records, zero for any other line — a subagent's included.
func assistantInput(line []byte) int64 {
	// MOST LINES ARE NOT ONE: a tool's result, a person's message, a summary.
	if !bytes.Contains(line, []byte(`"assistant"`)) || !bytes.Contains(line, []byte(`"usage"`)) {
		return 0
	}
	var rec struct {
		Type        string `json:"type"`
		IsSidechain bool   `json:"isSidechain"`
		Message     struct {
			Usage *struct {
				Input      int64 `json:"input_tokens"`
				CacheWrite int64 `json:"cache_creation_input_tokens"`
				CacheRead  int64 `json:"cache_read_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &rec) != nil || rec.Type != "assistant" || rec.IsSidechain ||
		rec.Message.Usage == nil {
		return 0
	}
	u := rec.Message.Usage
	return max(u.Input, 0) + max(u.CacheWrite, 0) + max(u.CacheRead, 0)
}

// lastExportInput is the input of the last request an OpenCode export records:
// its last assistant message that measured a prompt.
func lastExportInput(r io.Reader) (int64, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return 0, err
	}
	dec := json.NewDecoder(zr)
	if err = expectDelim(dec, '{'); err != nil {
		return 0, err
	}
	var last int64
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return 0, err
		}
		if key != "messages" {
			if err = skipValue(dec); err != nil {
				return 0, err
			}
			continue
		}
		if err = expectDelim(dec, '['); err != nil {
			return 0, err
		}
		for dec.More() {
			n, measured, msgErr := exportMessageInput(dec)
			if msgErr != nil {
				return 0, msgErr
			}
			if measured {
				last = n
			}
		}
		if err = expectDelim(dec, ']'); err != nil {
			return 0, err
		}
	}
	return last, nil
}

// exportMessageInput reads one message of an export and answers the input its
// request measured, and whether it measured one by [measuredPrompt]'s rule.
//
// THE MESSAGE IS THE ONE EVERY READER OF AN EXPORT TAKES ([openCodeMessage]),
// so this and a session's transcript never disagree about what a message is,
// its kind or its failure. A KEY AT A TIME, the export streamed: each of the
// small values the measure reads, its type, its time, its tokens and its error,
// is taken raw by its exact name and decoded on its own, by its keys exactly as
// written ([openCodeMeasuredWire]), and every other field is passed over a
// token at a time, so a message's content is never held whole. Decoded by
// encoding/json into the message's fields, the tokens were read with a key that
// differs from one they declare only in case taken for that one, the last of
// the two written winning.
func exportMessageInput(dec *json.Decoder) (int64, bool, error) {
	if err := expectDelim(dec, '{'); err != nil {
		return 0, false, err
	}
	var w openCodeMeasuredWire
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return 0, false, err
		}
		switch key {
		case "type":
			err = dec.Decode(&w.Type)
		case "time":
			err = dec.Decode(&w.Time)
		case "tokens":
			err = dec.Decode(&w.Tokens)
		case "error":
			err = dec.Decode(&w.Error)
		default:
			err = skipValue(dec)
		}
		if err != nil {
			return 0, false, err
		}
	}
	if err := expectDelim(dec, '}'); err != nil {
		return 0, false, err
	}
	m, _, err := w.message()
	if err != nil {
		return 0, false, err
	}
	n, measured := measuredPrompt(m)
	return n, measured, nil
}

// openCodeMeasuredWire is the part of one message of an OpenCode session the
// measure of its request reads, each key by its exact name and raw: its id, its
// kind, its time, its tokens and its error, as an export's message and a page of
// the session's message list each carry them. Every other key, its content
// above all, is passed over unread.
type openCodeMeasuredWire struct {
	ID     json.RawMessage `json:"id"`
	Type   json.RawMessage `json:"type"`
	Time   json.RawMessage `json:"time"`
	Tokens json.RawMessage `json:"tokens"`
	Error  json.RawMessage `json:"error"`
}

// message is the message w holds, as far as [measuredPrompt] reads one, each
// value decoded on its own BY ITS KEYS EXACTLY AS WRITTEN ([wireDecode]):
// its kind a string, its time's `completed` a whole number, its tokens'
// counts whole numbers, null as absent; and its id. AN ERROR, never a message
// read as one that measured nothing, where any of them is of another type: a
// reader that passed over a message it could not read would take an earlier
// message's figure for a request that may have carried more.
//
// ONLY WHAT THE MEASURE READS IS HELD TO ITS TYPE: of the time, its
// `completed` alone ([openCodeSettled]), and of the tokens, the prompt's
// counts alone ([openCodeTokens]). A value the measure never reads, a step's
// `created` or its reply's count, is no reason to lose a figure its counts
// and its completion state.
func (w openCodeMeasuredWire) message() (openCodeMessage, string, error) {
	var (
		m       openCodeMessage
		id      string
		settled openCodeSettled
	)
	for _, v := range [...]struct {
		key string
		raw json.RawMessage
		to  any
	}{
		{"id", w.ID, &id},
		{"type", w.Type, &m.Type},
		{"time", w.Time, &settled},
		{"tokens", w.Tokens, &m.Tokens},
	} {
		if v.raw == nil {
			continue
		}
		if err := wireDecode(v.raw, v.to); err != nil {
			return openCodeMessage{}, "", fmt.Errorf("a message's %s: %w", v.key, err)
		}
	}
	m.Time.Completed = settled.Completed
	m.Error = w.Error
	return m, id, nil
}

// openCodeSettled is the part of a message's time the measure reads: when its
// step settled, zero while it is in flight ([measuredPrompt]).
type openCodeSettled struct {
	Completed int64 `json:"completed"`
}

// measuredPrompt is the input one message of an OpenCode session says its
// step's request carried, and whether it measured one: THE ONE RULE for every
// reader of an OpenCode session's measure, an export's ([lastExportInput]) and
// the session's own message list's as a job ends ([openCodeContext]). A message
// measured a prompt where it is an assistant's step, settled (its time's
// `completed` set: an export holds settled messages alone, and the list hands
// the step in flight too), ended on no error (its `error` absent or null), and
// carries tokens whose prompt, its input, cache reads and cache writes, is
// above zero. A count below zero is read as none.
func measuredPrompt(m openCodeMessage) (int64, bool) {
	if m.Type != openCodeAssistant || m.Time.Completed <= 0 || !absent(m.Error) || m.Tokens == nil {
		return 0, false
	}
	n := max(m.Tokens.Input, 0) + max(m.Tokens.Cache.Read, 0) + max(m.Tokens.Cache.Write, 0)
	return n, n > 0
}

// skipValue passes over the next value whole, a token at a time.
func skipValue(dec *json.Decoder) error {
	depth := 0
	for {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := t.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
		if depth == 0 {
			return nil
		}
	}
}
