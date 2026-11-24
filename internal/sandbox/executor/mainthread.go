package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A CLAUDE CODE MAIN THREAD, WALKED ONCE BY EVERY READING ASKED OF IT.
//
// The take's context meter (measure.go) and the job's collection before it
// ([Runner.ReadMainThread]) each read a thread in ONE WALK ([walkThread]) that
// every reading asked is composed into ([threadReading]).
//
// IN THE BOX the read, a collection's, stop's or loss's, runs within
// internal/sandbox's MainThreadReadWait, cheapest check first: a session id
// naming no file is unread; a release below [claudeCountFloor], or none, is
// [sandbox.CountRelease]; a saved compaction found by its size
// ([claudePrecompact]) is [sandbox.CountPrecompact], each marking the count
// alone; the walk runs for the count, or takes it can decide ([unreadTakes]).

// threadReading is one reader's account of a Claude Code thread, composed into
// the one walk ([walkThread]) and never reading the thread itself, so a thread
// is read once however many readings ask: the count ([resumedReading]), the
// estimate of the next request ([nextReading]), the meter's last request's
// input ([contextReading]), and any reading added later.
type threadReading interface {
	// line is the thread's next whole line that is one record ([threadLine]).
	line(l threadLine)
	// refused is the next whole line that is no record; what it costs is the
	// reading's to decide ([threadReader.Next]).
	refused(err *threadLineError)
	// end is how the walk ended: err nil at the member's end, torn whether a
	// last piece with no break was left unread; else what ended it
	// ([errThreadBound], the member reader's error, ctx's).
	end(torn bool, err error)
}

// walkThread reads r, one thread member, a line at a time within maxBytes,
// handing each line to every reading in order and each reading's end once,
// ctx heard through [threadReader.Next] alone. It goes on past a refused
// line, so a reading that tolerates one (the floor's figure) reads every line
// after it.
//
// HELD TO A BASE, past it ONE SLOT (the package doc's "A collection's read of
// a thread"): held before a line's facts charge past [collectionBaseLineCharge]
// ([threadReader.Gate]) or once a reading keeps more than
// [collectionBaseRecords] records, to the end where one keeps records
// ([recordKeeper]; its caller releases large), for the line alone otherwise. A
// wait its ctx ends ends the walk ([errLargeWait]).
func walkThread(ctx context.Context, r io.Reader, maxBytes int64, large *passBudget, readings ...threadReading) {
	gate := large.gate(ctx)
	if keepsRecords(readings) {
		gate.drop = nil
	}
	reader := newThreadReader(r, maxBytes)
	reader.Gate(gate)
	defer reader.dropGate()
	for _, reading := range readings {
		if asker, ok := reading.(lineAsker); ok {
			asker.ask(reader)
		}
	}
	ended := func(torn bool, err error) {
		for _, reading := range readings {
			reading.end(torn, err)
		}
	}
	escalated := false
	for {
		l, err := reader.Next(ctx)
		var refused *threadLineError
		switch {
		case err == nil:
			for _, reading := range readings {
				reading.line(l)
			}
		case errors.As(err, &refused):
			for _, reading := range readings {
				reading.refused(refused)
			}
		case errors.Is(err, io.EOF):
			ended(reader.Torn(), nil)
			return
		default:
			ended(false, err)
			return
		}
		if !escalated && keepsPastBase(readings) {
			if err := gate.hold(); err != nil {
				ended(false, err)
				return
			}
			escalated = true
		}
	}
}

// recordKeeper is a reading that keeps records from line to line (the chain
// in a collection's pass): records is how many it holds now, every item it
// keeps of them counted ([threadChain.items]).
type recordKeeper interface {
	records() int
}

// lineAsker is a reading that asks reader for what it reads of every line,
// before its first line: the receipts' the take tokens a hook's words name
// ([hookReading], [threadReader.Asking]), keeping a refusal as its own, and
// the next request's every line's estimate ([nextReading],
// [threadReader.Estimating]).
type lineAsker interface {
	ask(reader *threadReader)
}

// keepsRecords reports whether a reading keeps records from line to line, so
// what it holds of a line past the base outlives that line.
func keepsRecords(readings []threadReading) bool {
	for _, reading := range readings {
		if _, ok := reading.(recordKeeper); ok {
			return true
		}
	}
	return false
}

// keepsPastBase reports whether a reading holds more than
// [collectionBaseRecords] records, as [recordKeeper] counts them.
func keepsPastBase(readings []threadReading) bool {
	for _, reading := range readings {
		if keeper, ok := reading.(recordKeeper); ok && keeper.records() > collectionBaseRecords {
			return true
		}
	}
	return false
}

// collectionBaseLineCharge is what a line's facts may charge ([lineMeter])
// before its pass takes the large slot, its length taking nothing: 128 KiB,
// held as 160 KiB at most, a charged byte holding 1.25 bytes (a string just
// past 32 KiB takes whole 8 KiB pages): what [baseTierReadBytes] leaves beside
// the chain at [collectionBaseRecords], the reader within a line and the
// strings a charge covers. The facts of the census's lines hold 753 bytes at
// most. The package doc's "A collection's read of a thread" has the
// derivation and the measurement.
const collectionBaseLineCharge = 1 << 17

// collectionBaseRecords is the most a pass's readings keep before it takes the
// large slot, in the chain's items ([threadChain.items]), its call ids among
// them: 2^13, at most 2.7 MiB at End's peak, a thread of at least 4,096
// records, some 8 MiB of ordinary lines (the package doc, as above).
const collectionBaseRecords = 1 << 13

// The bases' coefficients in bytes, each at its worst (the package doc's THE
// COEFFICIENTS): one of the chain's items at End's peak
// (BenchmarkAThreadChainAtItsBounds); a line's reader within the line; and the
// strings one charge covers, the last with the buffer json v2 unquotes it
// through. TestTheBaseCoefficientsHold measures the last two. baseTierReadBytes
// is what the base tier counts each read in flight at, about 5.3 MiB, and a
// collection's least weight of the node's answer budget ([collectionWeight]):
// what its fixed per-read buffers hold whatever its pieces weigh.
const (
	chainItemBytes     = 344
	readerLineBytes    = 1730 << 10
	chargeStringsBytes = 808 << 10
	baseTierReadBytes  = 5450 << 10
)

// A pass at both bases holds no more than the base tier counts it at, its
// facts at 1.25 bytes a charged byte: a negative constant does not compile.
const _ = uint(baseTierReadBytes - collectionBaseRecords*chainItemBytes - readerLineBytes - chargeStringsBytes -
	collectionBaseLineCharge*5/4)

// CollectionLargeReadsAtOnce is how many passes past their base this process
// reads at once: ONE, a pool apart from [TranscriptReadsAtOnce], so a person
// paging a transcript never spends a collection's MainThreadReadWait (the
// package doc's "A collection's read of a thread" says why one, not two).
const CollectionLargeReadsAtOnce = 1

// collectionLargeReads holds a slot for each pass past its base, at most
// [CollectionLargeReadsAtOnce], taken through [passBudget.hold].
var collectionLargeReads = make(chan struct{}, CollectionLargeReadsAtOnce)

// passBudget is one pass's claim on [collectionLargeReads]: none while every
// piece the pass holds is within its base, which no token holds, and ONE slot
// while it holds any piece past it ([passBudget.hold]), given back with the
// last [passBudget.drop], or by [passBudget.release] at the end of a pass that
// keeps what it read. A take's scrub and its meter are one pass and share one
// budget, each waiting within its own ctx: with two, the scrub's write would
// wait on the meter and the meter on the slot the scrub holds. The zero value
// holds nothing.
type passBudget struct {
	mu   sync.Mutex
	took func()
	// holds is how many pieces past the base the pass holds now.
	holds int
	// taking closes once the wait for the slot now in flight ends.
	taking chan struct{}
}

// errLargeWait is a pass whose wait for the large slot ended with its ctx: a
// failure of the read, never a reading of what it read, so a fallback that
// reads a rewrite's failure as "not JSON" passes it on.
var errLargeWait = errors.New("executor: the wait for the large read slot ended")

// hold holds the pass's one large slot for one more piece, waiting for it, or
// for the wait another user of the pass has in flight, for as long as ctx
// lasts and holding nothing while it waits.
func (b *passBudget) hold(ctx context.Context) error {
	for {
		b.mu.Lock()
		if b.took != nil {
			b.holds++
			b.mu.Unlock()
			return nil
		}
		if taking := b.taking; taking != nil {
			b.mu.Unlock()
			select {
			case <-taking:
				continue
			case <-ctx.Done():
				return fmt.Errorf("%w: %w", errLargeWait, ctx.Err())
			}
		}
		taking := make(chan struct{})
		b.taking = taking
		b.mu.Unlock()
		took, err := takeLargeSlot(ctx)
		b.mu.Lock()
		b.taking = nil
		if err == nil {
			b.took, b.holds = took, b.holds+1
		}
		b.mu.Unlock()
		close(taking)
		if err != nil {
			return fmt.Errorf("%w: %w", errLargeWait, err)
		}
		return nil
	}
}

// takeLargeSlot takes a slot of [collectionLargeReads] ([takeSlot]). A take
// that waits says so as its wait ends (executor_large_read_waited): that the
// slot is contended, and where its read ran out waiting, why the count reads
// unread or the take was refused.
func takeLargeSlot(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case collectionLargeReads <- struct{}{}:
		return func() { <-collectionLargeReads }, nil
	default:
	}
	start := time.Now()
	took, err := takeSlot(ctx, collectionLargeReads)
	log.InfoContext(ctx, "executor_large_read_waited", "took", err == nil,
		"waited_ms", time.Since(start).Milliseconds())
	return took, err
}

// drop gives back one piece's hold, and the slot with the last.
func (b *passBudget) drop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.holds > 0 {
		b.holds--
	}
	if b.holds == 0 && b.took != nil {
		b.took()
		b.took = nil
	}
}

// release gives back the slot the pass holds, whatever its pieces: at the end
// of a pass that keeps what it read ([walkThread]), once nothing reads under it.
func (b *passBudget) release() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.holds = 0
	if b.took != nil {
		b.took()
		b.took = nil
	}
}

// pieceGate is one reader's way to its pass's slot: hold waits within the
// reader's ctx; drop is nil where a hold is kept to the pass's end.
type pieceGate struct {
	hold func() error
	drop func()
}

// gate is b's gate for a reader waiting within ctx.
func (b *passBudget) gate(ctx context.Context) pieceGate {
	return pieceGate{hold: func() error { return b.hold(ctx) }, drop: b.drop}
}

// mainThreadHarness is a harness that reads the main thread of the session a
// job's box ran ([Runner.ReadMainThread]): Claude Code, Codex, Gemini CLI, Grok
// Build, Muse Code, OpenCode and Qwen Code, each read settling what its
// [threadHarness] says.
type mainThreadHarness interface {
	readMainThread(ctx context.Context, box sandbox.Sandbox, paths Paths,
		ask sandbox.MainThreadAsk) sandbox.MainThreadReading
}

// ReadMainThread reads the main thread of the session a job's box ran, once
// for every reading ask asks of it, through the runner's harness: zero for a
// harness that reads none. A harness that confirms off its session record
// ([recordConfirmer]) answers the takes asked from that record, through the
// same receipts, whether or not it reads a whole thread.
func (r *Runner) ReadMainThread(ctx context.Context, box sandbox.Sandbox,
	ask sandbox.MainThreadAsk) sandbox.MainThreadReading {

	var reading sandbox.MainThreadReading
	if reads, ok := r.harness.(mainThreadHarness); ok {
		reading = reads.readMainThread(ctx, box, PathsFor(box), ask)
	}
	if records, ok := r.harness.(recordConfirmer); ok && len(ask.Takes) > 0 {
		reading.Receipts = recordReceipts(ctx, records, box, ask)
	}
	return reading
}
