package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A CLAUDE CODE MAIN THREAD, WALKED ONCE BY EVERY READING ASKED OF IT.
//
// Two places read a session's main thread whole: the take's context meter, over
// the archive it keeps (measure.go), and the read that collects a job, over the
// copy its box holds (internal/sandbox's mainthread.go,
// [Runner.ReadMainThread]). Each is ONE WALK of the thread's whole lines
// through the line layer ([walkThread]), into which every reading the caller
// asks is composed ([threadReading]): the count of what Claude Code counts the
// session at ([resumedReading]), and at the take the floor's figure, the last
// request's input ([contextReading]). A reading never reads the thread itself,
// so a thread is read once whatever readings ask of it, and a reading a later
// change adds joins the same walk rather than read the thread again.
//
// IN THE BOX, the read is a job's collection's, stop's or loss's, within the one
// wait every reading of it shares (internal/sandbox's MainThreadReadWait),
// cheapest first: a session whose id names no file is unread; a release the
// count's rules were not read in is [sandbox.CountRelease]; a saved compaction
// Claude Code reads ([claudePrecompact]), found by a look at each name's size,
// is [sandbox.CountPrecompact]; each marks the count alone, and the thread is
// opened, streamed (through envd on e2b) and walked where the count is still
// asked OR takes are ([hookReading], the package doc's "A hook's note").

// threadReading is one reader's account of a Claude Code thread, composed into
// the ONE walk of its whole lines ([walkThread]): handed every whole line in
// file order, each line the layer refuses, and then how the walk ended. A
// reading never reads the thread itself, so a thread is read once whatever
// readings ask of it: the count ([resumedReading]) and the meter's floor figure
// ([contextReading]) now, and any later reading of the thread when it joins.
type threadReading interface {
	// line is the thread's next whole line that is one record, the
	// reading's to keep ([threadLine]).
	line(l threadLine)
	// refused is the thread's next whole line that is no record: what it
	// costs the reading is the reading's to decide (the layer's rule,
	// [threadReader.Next]).
	refused(err *threadLineError)
	// end is how the walk ended: err nil at the member's end, torn whether a
	// last piece with no break was left unread; else what ended it
	// ([errThreadBound], the member reader's error, ctx's).
	end(torn bool, err error)
}

// walkThread reads r, one thread member, a whole line at a time through the
// layer's reader ([newThreadReader]) within maxBytes, handing each line to
// every reading in the order given, ctx asked by the reader before each line
// ([threadReader.Next]), and each reading's end once. It goes on past a
// refused line, the reader staying usable, so a reading that reads past one
// (the floor's figure, which counts nothing of it) reads every line after it.
//
// HELD TO A BASE, past it ONE SLOT (the package doc's "A collection's read of
// a thread"): the walk takes large's slot before it hands on a byte of a line
// past [collectionBaseLineBytes] ([lineRun]), or reads a line after a reading
// holds more than [collectionBaseRecords] records ([recordKeeper]); a wait
// its ctx ends ends the walk with ctx's error. Its caller gives large back.
func walkThread(ctx context.Context, r io.Reader, maxBytes int64, large *passBudget, readings ...threadReading) {
	escalate := func() error { return large.escalate(ctx) }
	reader := newThreadReader(&lineRun{r: r, escalate: escalate}, maxBytes)
	for _, reading := range readings {
		if asker, ok := reading.(takeAsker); ok {
			reader.Asking(asker.asked())
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
			if err := escalate(); err != nil {
				ended(false, err)
				return
			}
			escalated = true
		}
	}
}

// recordKeeper is a reading that keeps records from line to line (the chain
// in a collection's pass): records is how many it holds now.
type recordKeeper interface {
	records() int
}

// takeAsker is a reading that asks which take tokens a hook's words name
// ([threadReader.Asking]): the receipts' ([hookReading]).
type takeAsker interface {
	asked() []string
}

// keepsPastBase reports whether a reading holds more than
// [collectionBaseRecords] records.
func keepsPastBase(readings []threadReading) bool {
	for _, reading := range readings {
		if keeper, ok := reading.(recordKeeper); ok && keeper.records() > collectionBaseRecords {
			return true
		}
	}
	return false
}

// collectionBaseLineBytes is the longest line a pass reads before it takes
// the large slot: 256 KiB, four times the census's p99 line, about 2.3 MiB
// held for it at its worst content (the package doc's "A collection's read
// of a thread" has the derivation and the measurement).
const collectionBaseLineBytes = 256 << 10

// collectionBaseRecords is the most records a pass's readings keep before it
// takes the large slot: 2^13, about 2.7 MiB of the chain at End's peak, a
// thread of some 16 MiB of ordinary lines (the package doc, as above).
const collectionBaseRecords = 1 << 13

// CollectionLargeReadsAtOnce is how many passes past their base this process
// reads at once: ONE, a pool apart from [TranscriptReadsAtOnce], so a person
// paging a transcript never spends a collection's MainThreadReadWait (the
// package doc's "A collection's read of a thread" says why one, not two).
const CollectionLargeReadsAtOnce = 1

// collectionLargeReads holds a slot for each pass past its base, at most
// [CollectionLargeReadsAtOnce], taken through [passBudget.escalate].
var collectionLargeReads = make(chan struct{}, CollectionLargeReadsAtOnce)

// passBudget is one pass's claim on [collectionLargeReads]: none while the
// pass is within its base, which no token holds, and ONE slot from the first
// escalate on, however often it is asked, until release. A take's scrub and
// its meter are one pass and share one budget, so escalate is safe from both
// goroutines, each waiting within its own ctx. The zero value holds nothing.
type passBudget struct {
	mu   sync.Mutex
	took func()
	// taking closes once the wait for the slot now in flight ends.
	taking chan struct{}
}

// escalate takes the pass's one large slot, waiting for it, or for the wait
// another user of the pass has in flight, for as long as ctx lasts and holding
// nothing while it waits; a no-op once it is held.
func (b *passBudget) escalate(ctx context.Context) error {
	for {
		b.mu.Lock()
		if b.took != nil {
			b.mu.Unlock()
			return nil
		}
		if taking := b.taking; taking != nil {
			b.mu.Unlock()
			select {
			case <-taking:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		taking := make(chan struct{})
		b.taking = taking
		b.mu.Unlock()
		took, err := takeSlot(ctx, collectionLargeReads)
		b.mu.Lock()
		b.took, b.taking = took, nil
		b.mu.Unlock()
		close(taking)
		return err
	}
}

// release gives back the slot the pass took, if it took one: at the pass's
// end, once nothing reads under it.
func (b *passBudget) release() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.took != nil {
		b.took()
		b.took = nil
	}
}

// lineRun is a member's reader that calls escalate BEFORE it hands on a byte
// of a line past [collectionBaseLineBytes], counting the bytes since the last
// break; past that it only passes bytes through. Beneath a bufio reader, its
// read-ahead may escalate a line early, never late, and the line primitive
// above it ([readLine]) stays the one copy. An escalate error is its own.
type lineRun struct {
	r        io.Reader
	escalate func() error
	run      int
	past     bool
}

func (l *lineRun) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	if l.past {
		return n, err
	}
	for rest := p[:n]; ; {
		brk := bytes.IndexByte(rest, '\n')
		if brk < 0 {
			l.run += len(rest)
		} else {
			l.run += brk
		}
		if l.run > collectionBaseLineBytes {
			if escErr := l.escalate(); escErr != nil {
				return 0, escErr
			}
			l.past = true
			return n, err
		}
		if brk < 0 {
			return n, err
		}
		l.run, rest = 0, rest[brk+1:]
	}
}

// mainThreadHarness is a harness that reads the main thread of the session a
// job's box ran ([Runner.ReadMainThread]): Claude Code's.
type mainThreadHarness interface {
	readMainThread(ctx context.Context, box sandbox.Sandbox, paths Paths,
		ask sandbox.MainThreadAsk) sandbox.MainThreadReading
}

// ReadMainThread reads the main thread of the session a job's box ran, once,
// for every reading ask asks of it (internal/sandbox's mainthread.go), through
// the runner's harness: zero for a harness that reads none, as OpenCode's
// reads none.
func (r *Runner) ReadMainThread(ctx context.Context, box sandbox.Sandbox,
	ask sandbox.MainThreadAsk) sandbox.MainThreadReading {

	reads, ok := r.harness.(mainThreadHarness)
	if !ok {
		return sandbox.MainThreadReading{}
	}
	return reads.readMainThread(ctx, box, PathsFor(box), ask)
}

var _ mainThreadHarness = ClaudeCode{}

// readMainThread is Claude Code's read of the main thread of ask's session in
// box, in the order the file comment states: the session's id, the release,
// the saved compaction's two names, then ONE walk of the thread for every
// reading asked. The release and the saved compaction mark the count alone:
// takes asked are read in the walk all the same, the chain first on each line.
func (ClaudeCode) readMainThread(ctx context.Context, box sandbox.Sandbox, paths Paths,
	ask sandbox.MainThreadAsk) sandbox.MainThreadReading {

	var reading sandbox.MainThreadReading
	if !ask.Count && len(ask.Takes) == 0 {
		return reading
	}
	marked := func(mark sandbox.CountMark) {
		if ask.Count && reading.Count == nil {
			reading.Count = &sandbox.ResumedCount{Mark: mark}
		}
	}
	unread := func(why string) sandbox.MainThreadReading {
		marked(sandbox.CountUnread)
		if len(ask.Takes) > 0 {
			reading.Receipts = sandbox.HookReceipts{Why: why}
		}
		return reading
	}
	thread, ok := claudeMainThread(ask.Session)
	if !ok {
		return unread("no session names the job's main thread")
	}
	if ask.Release == "" || !releaseAtLeast(ask.Release, claudeCountFloor) {
		marked(sandbox.CountRelease)
	}
	saved, _ := claudePrecompact(ask.Session)
	for _, name := range saved {
		if !ask.Count || reading.Count != nil {
			break
		}
		// A LOOK AT ITS SIZE, never a read of its body: a file of no bytes is
		// one the harness cannot read, so it holds no saved compaction.
		tail, err := box.ReadTail(ctx, paths.ClaudeConfigDir()+"/"+name, 0)
		switch {
		case err != nil:
			marked(sandbox.CountUnread)
		case tail.Size > 0:
			marked(sandbox.CountPrecompact)
		}
	}
	counts := ask.Count && reading.Count == nil
	if !counts && len(ask.Takes) == 0 {
		return reading
	}
	body, err := box.OpenFile(ctx, paths.ClaudeConfigDir()+"/"+thread)
	if err != nil {
		return unread("the thread did not open")
	}
	defer func() { _ = body.Close() }()
	var large passBudget
	defer large.release()
	var (
		readings []threadReading
		count    resumedReading
		hooks    *hookReading
	)
	if len(ask.Takes) > 0 {
		chain := &chainReading{c: newThreadChain("")}
		hooks = newHookReading(chain, ask.Takes)
		readings = append(readings, chain)
	}
	if counts {
		readings = append(readings, &count)
	}
	if hooks != nil {
		readings = append(readings, hooks)
	}
	walkThread(ctx, body, threadPlainMax, &large, readings...)
	if counts {
		reading.Count = count.count(0)
	}
	if hooks != nil {
		reading.Receipts = hooks.receipts(ctx, ask)
	}
	return reading
}
