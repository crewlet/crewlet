package executor

import (
	"context"
	"fmt"
	"io"

	"github.com/crewlet/crewlet/internal/panics"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// What a session's last request carried, read off its archive as a take streams
// it out of its box ([sandbox.TakenSession.Context]). A launch continuing a
// session sends its whole context before its brief, so its floor is that
// context plus the brief (internal/engine's launch floor): the take's estimate
// of the next request (claudenextrequest.go), else the harness's own record of its
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
