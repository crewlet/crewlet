package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// The host's side of the live view (docs/concepts/code-sandbox.md, "Watching
// a run live"): ONE [LiveReading] per watched launch, read from the box at
// most once per [LiveReuse] however many watch, each answer the slice of it
// the asker's cursor lacks.
//
// The text is APPEND-ONLY (a line settles only once no later write can change
// its redaction: [github.com/crewlet/crewlet/internal/redact.Settled]), so an
// offset names one byte for the reading's life. Anything else is a RESET of the
// last [MaxRunTextBytes] in whole lines, never a splice: the EPOCH names the
// origin, the OFFSET how far the viewer holds, the DIGEST the window ending
// there (catching a build that derives other text). A box is read on its host
// whoever holds the seat, so a move of the seat continues the same reading.

// LiveReading is one live reading of one job's account of itself, advanced a
// read at a time by the node that hosts the run's box — what [Runner.Follow]
// begins.
type LiveReading interface {
	// Read reads what the job has said since the last Read, from a box
	// attached for the purpose ([Provider.Attach]): nothing is written,
	// signalled or cleared in it.
	Read(ctx context.Context, box Sandbox) (LiveRead, error)
}

// LiveRead is what one [LiveReading.Read] added.
type LiveRead struct {
	// Origin names the reading this read continues: its account, and where
	// in its stream it began. Text continues every earlier read of the same
	// Origin; another Origin begins anew. DETERMINISTIC, so two readings
	// of the same stream from the same place derive the same text.
	Origin string
	Source OutputSource
	// Text is the display text this read SETTLED: whole lines, redacted
	// over everything written before them, which no later write can
	// change.
	Text string
	// Front is whether the reading began after the account's own start:
	// what came before it was never read.
	Front bool
	// Held is how many bytes are written but not settled yet — a line not
	// finished, a private key whose END may still come, a password whose
	// value is on a later line.
	Held int
	// Finished is whether the job has written its done marker: the wrapper
	// returned, so the stream will not grow again.
	Finished bool
	// AsOf is when the box was read.
	AsOf time.Time
	// Spent is every token the job's model calls have reported since the
	// reading began, for a caller that stops a run at a cap while it works.
	// Zero for a harness whose stream reports none until it ends.
	Spent int
}

// LiveReuse is how long one read of a box answers everybody watching it.
// Below the dashboard's three-second poll (chained after each answer), so one
// viewer is read fresh every poll while any number cost one read per two
// seconds. A gate holds it to the dashboard's interval (client_gate_test.go).
const LiveReuse = 2 * time.Second

// liveRefreshBudget bounds one read of a box. The read is the reading's, not a
// request's: a request waits only its own budget, and an abandoned one leaves
// the read to finish for the next. The completion poll's interval: a box slower
// than that is not answering, and a viewer is better told so on the next poll
// than kept waiting a minute on the poll's own bound ([pollBound]).
const liveRefreshBudget = DefaultPollInterval

// liveIdle is how long a reading nobody asks about is kept: ten dashboard
// polls, so a viewer who looked away returns to the same reading and one who
// left soon costs nothing. [LiveFeeds] sweeps on its own clock ([liveSweep]),
// never inside a request: a quiet node may get none, and in a fleet the host
// is never told a launch stopped (the serving node answers `not_running` from
// the record and stops asking), so a watched reading would stay until the node
// stopped.
const liveIdle = 30 * time.Second

// liveSweep is how often idle readings are looked for: a third of [liveIdle],
// so a reading is let go between liveIdle and four thirds of it after its last
// request; a finer sweep walks every reading more often for a few seconds of
// memory.
const liveSweep = liveIdle / 3

// liveHold is how much of a reading's settled text the host keeps: twice what
// a viewer holds, since a viewer up to [MaxRunTextBytes] behind is answered
// from it and its digest covers the [MaxRunTextBytes] before its offset.
const liveHold = 2 * MaxRunTextBytes

// LiveFeeds is one node's live readings, keyed by the launch they read.
type LiveFeeds struct {
	manager func() *Manager
	now     func() time.Time
	// base bounds every read and the sweep; it ends when the node does, or
	// at Stop.
	base context.Context
	end  context.CancelFunc
	// swept is closed once the sweep has returned.
	swept chan struct{}

	mu    sync.Mutex
	feeds map[feedKey]*liveFeed
}

type feedKey struct{ turn, launch string }

// LiveFeedsOptions configures [NewLiveFeeds].
type LiveFeedsOptions struct {
	// Manager is this node's sandbox manager, resolved per read because an
	// apply swaps it; nil, or a nil answer, is a node with no sandbox
	// backend.
	Manager func() *Manager
	// Now is the clock; nil is the wall clock.
	Now func() time.Time
}

// NewLiveFeeds builds a node's readings and starts their sweep, every read of
// which ends with ctx — and the sweep with it, or at [LiveFeeds.Stop].
func NewLiveFeeds(ctx context.Context, opts LiveFeedsOptions) *LiveFeeds {
	return startLiveFeeds(ctx, opts, nil)
}

// startLiveFeeds is [NewLiveFeeds] with the sweep's tick: every [liveSweep]
// when ticks is nil, or whenever a test sends one.
func startLiveFeeds(ctx context.Context, opts LiveFeedsOptions, ticks <-chan time.Time) *LiveFeeds {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	base, end := context.WithCancel(ctx)
	fs := &LiveFeeds{
		manager: opts.Manager, now: now, base: base, end: end,
		swept: make(chan struct{}), feeds: map[feedKey]*liveFeed{},
	}
	go fs.sweep(ticks)
	return fs
}

// sweep drops, on every tick, the readings nobody has asked about for
// [liveIdle], until the readings end.
func (fs *LiveFeeds) sweep(ticks <-chan time.Time) {
	defer close(fs.swept)
	if ticks == nil {
		ticker := time.NewTicker(liveSweep)
		defer ticker.Stop()
		ticks = ticker.C
	}
	for {
		select {
		case <-fs.base.Done():
			return
		case <-ticks:
			fs.dropIdle()
		}
	}
}

// dropIdle lets go of every reading unused for longer than [liveIdle].
func (fs *LiveFeeds) dropIdle() {
	now := fs.now()
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for key, feed := range fs.feeds {
		if now.Sub(feed.used()) > liveIdle {
			feed.stop()
			delete(fs.feeds, key)
		}
	}
}

// Stop ends every reading, every read in flight and the sweep, and waits for
// the sweep to return — for a node that stops answering before its context
// ends. Safe to call more than once.
//
// A read runs under the node's context, not a request's, since it outlives the
// request that started it ([liveFeed.current]); a request after Stop gets the
// read's own failure rather than reading a box this node is letting go of.
func (fs *LiveFeeds) Stop() {
	fs.end()
	<-fs.swept
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for key, feed := range fs.feeds {
		feed.stop()
		delete(fs.feeds, key)
	}
}

// Forget drops a launch's reading: it stopped running, so nobody can ask about
// it again.
func (fs *LiveFeeds) Forget(turnID, launchID string) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if feed, ok := fs.feeds[feedKey{turnID, launchID}]; ok {
		feed.stop()
		delete(fs.feeds, feedKey{turnID, launchID})
	}
}

// Answer is what this node says about one running launch it owns, to one
// request: what the asker's cursor lacks, or a reset (the zero cursor's
// answer included).
//
// It waits for the reading until ctx ends (the caller sets it from the asker's
// budget), then answers with the last read, stamped with when it was taken; a
// reading that has read nothing by then is an error saying so.
func (fs *LiveFeeds) Answer(ctx context.Context, run PendingRun, cursor TailCursor) (Output, error) {
	feed := fs.feed(run)
	snap, err := feed.current(ctx)
	if err != nil {
		return Output{}, err
	}
	return snap.cursored(cursor), nil
}

// feed is the reading of one launch, made on its first request and kept for as
// long as somebody keeps asking ([LiveFeeds.sweep]).
func (fs *LiveFeeds) feed(run PendingRun) *liveFeed {
	now := fs.now()
	fs.mu.Lock()
	defer fs.mu.Unlock()
	key := feedKey{run.TurnID, run.LaunchID}
	feed, ok := fs.feeds[key]
	if !ok {
		ctx, cancel := context.WithCancel(fs.base)
		feed = &liveFeed{feeds: fs, run: run, ctx: ctx, cancel: cancel, lastUsed: now}
		// NOT KEPT once the readings have ended: its read fails on the
		// ended context, and with the sweep gone nothing would let it go.
		if fs.base.Err() == nil {
			fs.feeds[key] = feed
		}
	}
	feed.touch(now)
	return feed
}

// liveFeed is one launch's reading and everything it has settled.
type liveFeed struct {
	feeds  *LiveFeeds
	run    PendingRun
	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	lastUsed time.Time
	reading  LiveReading
	// inflight is closed when the read in flight ends; nil while none is.
	inflight chan struct{}
	// read is whether any read has landed, and readAt when the last attempt
	// ended, landed or not: what [LiveReuse] is measured from, so a box that
	// fails is not asked again by every request either.
	read   bool
	readAt time.Time
	snap   liveSnapshot
	// err is the last attempt's failure, answered until another lands.
	err error
}

func (f *liveFeed) stop() { f.cancel() }

func (f *liveFeed) touch(now time.Time) {
	f.mu.Lock()
	f.lastUsed = now
	f.mu.Unlock()
}

func (f *liveFeed) used() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastUsed
}

// current is the reading as of no more than [LiveReuse] ago, read now if it is
// older — once, for every request that wants it — and waited for until ctx
// ends, after which the last read stands.
func (f *liveFeed) current(ctx context.Context) (liveSnapshot, error) {
	f.mu.Lock()
	if (f.read || f.err != nil) && f.feeds.now().Sub(f.readAt) < LiveReuse {
		snap, err := f.snap, f.err
		f.mu.Unlock()
		return snap, err
	}
	if f.inflight == nil {
		done := make(chan struct{})
		f.inflight = done
		//nolint:contextcheck // The read is the READING's, not this request's: it runs under the reading's own context and budget so an abandoned request leaves it to finish for the next.
		go f.refresh(done)
	}
	wait := f.inflight
	f.mu.Unlock()

	select {
	case <-wait:
	case <-ctx.Done():
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case f.read && f.err == nil:
		return f.snap, nil
	case f.err != nil:
		return liveSnapshot{}, f.err
	}
	// The box has not answered in this request's time: the read goes on,
	// and the next request is answered from what it finds.
	return liveSnapshot{}, unanswered(ctx, fmt.Errorf("sandbox: reading the box of run %s "+
		"took longer than this request could wait; it goes on, and the next request reads "+
		"what it found: %w", f.run.TurnID, ctx.Err()))
}

// refresh reads the box once, under the reading's own budget rather than any
// request's.
func (f *liveFeed) refresh(done chan struct{}) {
	ctx, cancel := context.WithTimeout(f.ctx, liveRefreshBudget)
	defer cancel()
	read, err := f.readBox(ctx)

	f.mu.Lock()
	defer f.mu.Unlock()
	defer close(done)
	f.inflight = nil
	f.readAt = f.feeds.now()
	if err != nil {
		f.err = err
		return
	}
	f.err = nil
	f.read = true
	f.snap.apply(read)
}

// readBox attaches to the run's box — never resuming it — and advances the
// reading from it.
func (f *liveFeed) readBox(ctx context.Context) (LiveRead, error) {
	// A reading that ended (the node stopped, or the launch was let go)
	// reads nothing, whatever a backend would make of a dead context. A WAIT
	// CLEARS IT ([ErrTailUnanswered]): the node's seats go elsewhere, or the
	// next request reads that the launch stopped.
	if err := ctx.Err(); err != nil {
		return LiveRead{}, fmt.Errorf("sandbox: the reading of run %s has ended: %w: %w",
			f.run.TurnID, ErrTailUnanswered, err)
	}
	var m *Manager
	if f.feeds.manager != nil {
		m = f.feeds.manager()
	}
	if m == nil {
		return LiveRead{}, errNoBackend()
	}
	run := f.run
	// ATTACHED, NEVER RECONNECTED: a read changes nothing, and Connect would
	// wake a box a collection paused just after the read took its running record.
	box, runner, err := m.Attach(ctx, Placement(run.Placement), run.SandboxID, run.Harness)
	if err != nil {
		return LiveRead{}, fmt.Errorf("sandbox: reach the box of run %s: %w", run.TurnID, err)
	}
	f.mu.Lock()
	if f.reading == nil {
		f.reading = runner.Follow(run.Handle())
	}
	reading := f.reading
	f.mu.Unlock()
	read, err := reading.Read(ctx, box)
	if err != nil {
		return LiveRead{}, fmt.Errorf("sandbox: read the box of run %s: %w", run.TurnID, err)
	}
	if read.AsOf.IsZero() {
		read.AsOf = f.feeds.now()
	}
	read.AsOf = read.AsOf.UTC()
	if !read.Source.Valid() {
		read.Source = SourceNone
	}
	return read, nil
}

// liveSnapshot is what a reading has settled, as the host keeps it.
type liveSnapshot struct {
	origin string
	source OutputSource
	front  bool
	held   int
	done   bool
	asOf   time.Time

	// end is how many bytes this origin has settled, and text the last of
	// them — at least [liveHold] of them, starting on a line — from base.
	end  int64
	base int64
	text string
}

// apply adds one read to the snapshot, starting over where its origin moved.
func (s *liveSnapshot) apply(read LiveRead) {
	if read.Origin != s.origin {
		*s = liveSnapshot{origin: read.Origin}
	}
	s.source, s.front, s.held, s.done, s.asOf = read.Source, read.Front, read.Held, read.Finished, read.AsOf
	s.text += read.Text
	s.end += int64(len(read.Text))
	if len(s.text) > liveHold+MaxRunTextBytes {
		// Trimmed in steps, not on every read, and on a line where there is
		// one; a line longer than the whole hold is cut on a character,
		// which a reset window marks as the part it is.
		cut := len(s.text) - liveHold
		if i := strings.LastIndexByte(s.text[:cut], '\n'); i >= 0 {
			cut = i + 1
		}
		for cut < len(s.text) && !utf8.RuneStart(s.text[cut]) {
			cut++
		}
		s.text = s.text[cut:]
		s.base += int64(cut)
	}
}

// digestAt is the digest of the window a viewer holding through offset at
// holds: the [MaxRunTextBytes] before it, or everything before it where that
// is less. False where the host no longer keeps the whole window, and for an
// offset this reading never had — a cursor off the wire is a peer's claim,
// and nothing between there and here held it to a whole number.
func (s liveSnapshot) digestAt(at int64) (string, bool) {
	from := max(at-MaxRunTextBytes, 0)
	if at < 0 || at > s.end || from < s.base {
		return "", false
	}
	sum := sha256.Sum256([]byte(s.text[from-s.base : at-s.base]))
	return hex.EncodeToString(sum[:16]), true
}

// cursored answers a viewer holding through c: what it lacks, or a RESET.
// Both offsets are stated whichever it is — see [Output.Start].
func (s liveSnapshot) cursored(c TailCursor) Output {
	out := Output{
		Source: s.source, AsOf: s.asOf, Finished: s.done,
		Epoch: s.origin, End: s.end, Front: s.front, Held: s.held,
	}
	out.Digest, _ = s.digestAt(s.end)
	if digest, ok := s.digestAt(c.Offset); ok && c.Epoch == s.origin && c.Digest == digest &&
		s.end-c.Offset <= MaxRunTextBytes {
		out.Start = c.Offset
		out.Text = s.text[c.Offset-s.base:]
		return out
	}
	out.Reset = true
	text, at := KeepEnd(s.text, MaxRunTextBytes)
	start := s.base + int64(at)
	out.Text, out.Start = text, start
	out.Cut = start > 0 || s.front
	return out
}
