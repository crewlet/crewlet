package executor

import (
	"archive/tar"
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
// the brief, never the static floor alone (internal/engine's launch floor).
// That context is the harness's own estimate of the next request where the
// take read one (nextrequest.go); the harness's own record of its last
// request, cache included, is the figure the floor reads where it read none:
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
// ([sandbox.TakenSession.Count], resumed.go), and what it estimates that
// request at ([sandbox.TakenSession.Next], nextrequest.go), off the same read:
// a Claude Code archive's main thread is walked ONCE, through the line layer
// ([walkThread]), by three readings, the last request's input
// ([contextReading]), the count ([resumedReading]) and the estimate of the
// next request ([nextReading]). So the meter DECIDES something now (whether a relaunch
// that continues the capture is told it as a cut-off one), and reads its
// archive's members by the layer's rules: a member named absolutely or with a
// `..` element, or a main thread held twice, leaves the count unread, since the
// member it would read need not be the file a resumed session holds; a saved
// compaction under either of its names marks it ([claudePrecompact]); and a
// thread written by a release older than the count's floor is no count of a
// session ([claudeCountFloor]).
//
// THE FLOOR'S FIGURE IS WHAT THE LAYER READS OF A REPLY ([contextReading]):
// the input side of the last reply of the main thread's own
// ([threadHead.Main]) whose usage the layer reads ([threadLine.Usage]), by
// the one rule a reply's usage is read by in a thread and in a run's stream
// ([readUsage]), the count's included. So the two readings of one walk never
// part on which record is a main-thread reply or on what its usage says: each
// key exactly as written, each count a whole number in digits alone, and the
// usage counted as Claude Code counts it, a reply read otherwise giving no
// figure and the reply before it standing. Never encoding/json's reading of
// the whole line, a rule of its own that matched a key in any case, merged a
// key written twice, read a null or a negative count as 0, and took a record
// that does not say it is a sidechain, or that names an agent, for the main
// thread's; and that refused the whole line for any key it read, in any case,
// holding a value of another kind, where the layer reads each key it reads by
// its last value as written, so a reply that one refused for a `Type` of
// another kind, an earlier `isSidechain` that is no boolean or an earlier
// `message` that is no object now gives its figure. Every reply measured
// carries a usage the layer reads ([threadLine.Usage] has the measure). A
// last piece with no break is no line (the box cuts it away before a resume),
// and a line the layer refuses counts nothing.
//
// ITS PACE: the walk is fed by the pipe the take writes into, so a meter slower
// than the scrub would hold the take up inside its wait. The walk decodes each
// line whole into its head and every fact the layer's readers read
// ([decodeThreadLine]), a message's blocks and their results among them, which
// neither of the walk's readings reads, beside the scrub's rewrite, gzip and
// checksum of the same bytes (BenchmarkTheMeterAtItsBounds measures both): at
// the plaintext a 32 MiB archive holds, the meter took 1.4 s where the scrub
// took 7.0 s, on two cores.

// contextMeter reads an archive written to it and keeps the whole input of the
// last request its transcript records, what Claude Code counts it at and what
// it estimates its next request at.
type contextMeter struct {
	pw       *io.PipeWriter
	done     chan struct{}
	tokens   int64
	counts   *sandbox.ResumedCount
	estimate sandbox.NextRequest
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
			next  sandbox.NextRequest
			err   error
		)
		switch layout {
		case archiveTar:
			n, count, next, err = meterArchive(ctx, pr, session, release)
		case archiveDocument:
			n, err = lastExportInput(pr)
		}
		if err == nil {
			m.tokens = n
		}
		m.counts, m.estimate = count, next
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

// next is what the meter estimated the session's next request at, once the
// archive has ended: zero for none, and for an archive of another harness's
// layout.
func (m *contextMeter) next() sandbox.NextRequest {
	<-m.done
	return m.estimate
}

// meterArchive is the input of the last request a Claude Code archive's main
// thread of session records, zero where it records none, what Claude Code
// counts the session at, and what it estimates the next request at, read off
// ONE walk of that thread ([walkThread]) by the three readings. The count is
// marked where the archive's members say so (the file comment) and unread
// where the archive does not read to its end, and the estimate is none where
// the count is unread or of a release its rules were not read in; err is the
// archive's own, where its gzip or tar framing failed.
//
// A SAVED COMPACTION LEAVES THE ESTIMATE STANDING: it marks the count, since
// the harness may swap it in for the session, but what the harness swaps in is
// smaller than the session it replaces, so the estimate of the session whole
// is what the floor holds the launch to, the safe side.
func meterArchive(ctx context.Context, r io.Reader, session, release string) (int64, *sandbox.ResumedCount,
	sandbox.NextRequest, error) {

	var (
		callers countMarks
		count   resumedReading
		next    nextReading
		figure  contextReading
		walked  bool
	)
	if release == "" || !releaseAtLeast(release, claudeCountFloor) {
		callers = callers.with(sandbox.CountRelease)
	}
	unread := func(err error) (int64, *sandbox.ResumedCount, sandbox.NextRequest, error) {
		return 0, count.count(callers.with(sandbox.CountUnread)), sandbox.NextRequest{}, err
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
		walkThread(ctx, tr, threadPlainMax, &figure, &count, &next)
	}
	if !walked {
		callers = callers.with(sandbox.CountUnread)
	}
	estimate := next.next()
	if blind := countMarks(0).with(sandbox.CountUnread).with(sandbox.CountRelease); callers&blind != 0 {
		estimate = sandbox.NextRequest{}
	}
	return figure.context(), count.count(callers), estimate, nil
}

// contextReading is the floor's figure as a reading of the walk
// ([threadReading]): the input side of the last main-thread reply whose usage
// the layer reads, its uncached input, its cache writes and its cache reads
// (the file comment). A line the layer refuses, and a reply whose usage it
// cannot read, count nothing; a walk that ended on an error leaves no figure,
// as a read that failed always has.
type contextReading struct {
	last   int64
	failed bool
}

var _ threadReading = (*contextReading)(nil)

// line reads one line's figure, where it is a main-thread reply whose usage
// the layer reads and whose input side is above zero.
func (c *contextReading) line(l threadLine) {
	if l.Head.Type != recordAssistant || !l.Head.Main() {
		return
	}
	u, ok, err := l.Usage()
	if err != nil || !ok {
		return
	}
	if n := u.Input + u.CacheWrite + u.CacheRead; n > 0 {
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
