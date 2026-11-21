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

	"github.com/crewlet/crewlet/internal/panics"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// What a session's last request carried, read off its archive as a take streams
// it out of its box ([sandbox.TakenSession.Context]). A launch continuing a
// session sends its whole context before its brief, so its floor is that
// context plus the brief (internal/engine's launch floor): the take's estimate
// of the next request (nextrequest.go), else the harness's own record of its
// last request, cache included. Claude Code: the last main-thread assistant
// message's usage (subagents are sidechains). OpenCode: the newest settled
// message that measured a prompt, by ONE rule for every reader
// ([measuredPrompt]: [meterExport], [openCodeContext]).

// The meter reads the scrub's output, the archive as kept, in a goroutine beside
// the take that never holds it up: what it cannot read counts as zero, and it
// drains the rest. It also yields the harness's count and next-request estimate
// ([sandbox.TakenSession.Count], [sandbox.TakenSession.Next]; package doc) in
// ONE walk ([meterExport], [walkThread] with [contextReading], [resumedReading],
// [nextReading]). Refusals leave the count unread: a member named absolutely or
// with `..`, a main thread held twice, a release below [claudeCountFloor];
// [claudePrecompact] marks a saved compaction. A usage is read by the layer's
// one rule ([readUsage]), keys exactly as written, never encoding/json's looser
// reading, so the readings never part on what a main-thread reply is. A reply
// it cannot read gives no figure and the one before stands; a last piece with
// no break is no line (the box cuts it before a resume).

// Pace: the walk is fed by the take's pipe, so a slower meter would hold the
// take up. BenchmarkTheMeterAtItsBounds measures it against the scrub.

// contextMeter reads an archive written to it and keeps the whole input of the
// last request its transcript records, what its harness counts it at, with
// the provider of the reply an OpenCode count is anchored on, and what it
// estimates its next request at.
type contextMeter struct {
	pw       *io.PipeWriter
	done     chan struct{}
	tokens   int64
	counts   *sandbox.ResumedCount
	provider string
	estimate sandbox.NextRequest
}

// newContextMeter starts a meter for an archive of layout holding session,
// written by release (the box's word on its harness's release,
// [sandbox.SessionArchive.Version]), read within ctx, the take's own: a take
// cut short ends the walk, whose count is then unread; ctx also ends its wait
// for the take's large slot, which its walk holds a line past the base at a
// time, as the take's scrub holds its pieces ([passBudget]).
func newContextMeter(ctx context.Context, layout archiveLayout, session, release string,
	large *passBudget) *contextMeter {

	return startMeter(ctx, func(r io.Reader) (meterReading, error) {
		var read meterReading
		var err error
		switch layout {
		case archiveTar:
			read.tokens, read.count, read.next, err = meterArchive(ctx, r, session, release, large)
		case archiveDocument:
			var export exportReading
			export, err = meterExport(ctx, r, release)
			read = meterReading{tokens: export.figure, count: export.count, provider: export.provider,
				next: export.next}
		}
		return read, err
	})
}

// meterReading is what a meter read off its archive: the last request's
// input, the harness's count, the provider an OpenCode count is anchored on,
// and the estimate of the next request.
type meterReading struct {
	tokens   int64
	count    *sandbox.ResumedCount
	provider string
	next     sandbox.NextRequest
}

// startMeter starts a meter that reads what is written to it with measure, on
// a goroutine of its own ([meterContained]).
func startMeter(ctx context.Context, measure func(io.Reader) (meterReading, error)) *contextMeter {
	pr, pw := io.Pipe()
	m := &contextMeter{pw: pw, done: make(chan struct{})}
	go func() {
		defer close(m.done)
		read, err := meterContained(ctx, pr, measure)
		if err == nil {
			m.tokens = read.tokens
		}
		m.counts, m.provider, m.estimate = read.count, read.provider, read.next
		// DRAINED WHATEVER IT FOUND, so the take that feeds it never waits on
		// a meter that stopped reading; the pipe ends when the take finishes.
		_, _ = io.Copy(io.Discard, pr)
	}()
	return m
}

// meterContained runs measure over r, a panic in it the meter's measuring
// nothing rather than the end of the engine.
//
// THE METER READS WHAT A BOX WROTE on a goroutine of its own, beside the take's
// scrub, which nothing above it can recover: a defect the box's text reaches in
// the transcript's walk or the export's scan would end the engine's process,
// every seat the node holds with it, as one in the scrub would
// (scrubContained). So its panic is contained here, told to whoever observes
// contained panics under the take's scope, its stack logged
// (session_meter_panicked), and the meter measures nothing, as one that cannot
// read its archive does; it never fails the take, and is drained as ever.
func meterContained(ctx context.Context, r io.Reader,
	measure func(io.Reader) (meterReading, error)) (read meterReading, err error) {

	defer func() {
		if p := recover(); p != nil {
			contained := panics.Recovered(panics.ScopeSessionTake, p)
			log.ErrorContext(ctx, "session_meter_panicked", "panic", fmt.Sprint(p), "stack", contained.Stack)
			read, err = meterReading{}, contained.Err()
		}
	}()
	return measure(r)
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
// Claude Code's count of a thread, OpenCode's of an export.
func (m *contextMeter) count() *sandbox.ResumedCount {
	<-m.done
	return m.counts
}

// countProvider is the provider of the reply an OpenCode count is anchored
// on, once the archive has ended: "" for a Claude Code count, and where no
// reply anchored one.
func (m *contextMeter) countProvider() string {
	<-m.done
	return m.provider
}

// next is what the meter estimated the session's next request at, once the
// archive has ended: zero for none.
func (m *contextMeter) next() sandbox.NextRequest {
	<-m.done
	return m.estimate
}

// meterArchive is the input of the last request a Claude Code archive's main
// thread of session records, zero where it records none, what Claude Code
// counts the session at, and what it estimates the next request at, read off
// ONE walk of that thread ([walkThread]) by the three readings, under large.
// The count is marked where the archive's members say so (the file comment)
// and unread where the archive does not read to its end, and the estimate is
// none where the count is unread or of a release its rules were not read in;
// err is the archive's own, where its gzip or tar framing failed. A SAVED
// COMPACTION LEAVES THE ESTIMATE STANDING: it marks the count, since the
// harness may swap it in, but what it swaps in is smaller than the session it
// replaces, so the estimate of the session whole is the floor's safe side.
func meterArchive(ctx context.Context, r io.Reader, session, release string,
	large *passBudget) (int64, *sandbox.ResumedCount, sandbox.NextRequest, error) {

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
			walkThread(ctx, tr, threadPlainMax, large, &figure)
			continue
		}
		walked = true
		walkThread(ctx, tr, threadPlainMax, large, &figure, &count, &next)
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
// value decoded on its own by its keys exactly as written ([wireDecode]):
// kind a string, time's `completed` and the tokens' counts whole numbers, null
// as absent. A value of another type is an error, never a message that
// measured nothing: skipping it would take an earlier message's figure for a
// request that may have carried more. Only what the measure reads is held to
// its type: `completed` alone ([openCodeSettled]) and the prompt's counts
// alone ([openCodeTokens]); any other value cannot cost a figure.
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
// reader of an OpenCode session's measure, an export's ([meterExport]) and
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
