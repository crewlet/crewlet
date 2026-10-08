package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// The owner's side of the live view: one READING per launch being watched,
// shared by everybody watching it, answered by cursor.
//
// # Why a reading, and not a peek per request
//
// A peek used to be a whole read of the box per request: a reconnect, three
// files, a decode and a redaction, for every viewer every three seconds, with
// nothing shared between them and nothing bounding how long one took — the
// asker gave up after its budget and the owner's answer ran on regardless, in
// one of the answer slots every run this node owns shares. And each answer
// re-sent the whole window it had sent three seconds before, which is why the
// window was eight kilobytes: a hundred lines of a run that can go on for
// hours, and nothing older than that on any screen until it was collected.
//
// So the owner keeps ONE [LiveReading] per watched launch, advanced from the
// box at most once per [LiveReuse] however many people are watching, and each
// answer is a slice of what that reading has settled: the bytes the asker does
// not hold yet. A viewer holds what the run's record will hold
// ([MaxRunTextBytes]) and is sent only what is new.
//
// # Why a cursor is safe here
//
// A reading's text is APPEND-ONLY by construction: a runner settles a line
// only once nothing written after it can change how it redacts
// ([github.com/crewlet/crewlet/internal/redact.Settled]), and a transcript
// entry, once decoded, is never decoded again. So an offset into it names the
// same byte for as long as the reading lasts, and a viewer at offset N lacks
// exactly the text after N. What can still go wrong is everything that is not
// this reading — the owner restarted, the run moved to a node on another
// build, the reading began again further on, the account switched from the
// error stream to the transcript, the viewer fell further behind than the
// owner keeps — and every one of those is a RESET carrying the last
// [MaxRunTextBytes] in whole lines, never a splice. Three things decide it,
// each catching what the others cannot: the EPOCH names the reading's origin
// (its account and where in the stream it began), the OFFSET says how far the
// viewer holds, and the DIGEST covers the window the viewer holds ending
// there, so a node that derived different text under the same origin — a
// parser or a redaction rule changed between builds — is caught too.
//
// An owner move on the same build CONTINUES: the new owner's reading of the
// same stream from the same origin derives the same text, and the digest
// says so. A LOCAL box cannot be reached from another host at all, so a move
// there is an error the answer names (the local backend's own sentence).

// LiveReading is one live reading of one job's account of itself, advanced a
// read at a time by the node that owns the run — what [Runner.Follow] begins.
type LiveReading interface {
	// Read reads what the job has said since the last Read, from a box
	// attached for the purpose ([Provider.Attach]): nothing is written,
	// signalled or cleared in it.
	Read(ctx context.Context, box Sandbox) (LiveRead, error)
}

// LiveRead is what one [LiveReading.Read] added.
type LiveRead struct {
	// Origin names the reading this read continues: which account, and
	// where in its stream the reading began. Text continues the text
	// every earlier read of the same Origin settled; a read naming another
	// Origin begins anew, and nothing read before is a prefix of it.
	//
	// DETERMINISTIC, so two owners reading the same stream from the same
	// place name the same origin and derive the same text.
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
	// Finished is whether the job has written its done marker, or its
	// stream's last line says it is over.
	Finished bool
	// AsOf is when the box was read.
	AsOf time.Time
}

// LiveReuse is how long one read of a box answers everybody watching it before
// the box is read again.
//
// BELOW THE DASHBOARD'S POLL, three seconds chained after each answer, so a
// single viewer is read fresh on every poll, while any number of viewers cost
// at most one read of the box every two seconds rather than one each. Held to
// the dashboard's interval by a gate (client_gate_test.go).
const LiveReuse = 2 * time.Second

// liveRefreshBudget bounds one read of a box, which runs for the reading and
// not for any one request: a request waits for it only as long as its own
// budget, and an abandoned request leaves it to finish for the next.
//
// The completion poll's interval, because a box that takes longer than the
// poll's whole interval to read is one that is not answering — and a person
// watching is better told so on the next poll than kept waiting a minute on
// a remote box's per-request client timeout.
const liveRefreshBudget = DefaultPollInterval

// liveIdle is how long a reading nobody asks about is kept. Ten of the
// dashboard's polls: a viewer who looked away for a moment comes back to the
// same reading, and one who left costs nothing for long.
//
// A PROPERTY OF THE READINGS, NOT OF THE TRAFFIC: [LiveFeeds] sweeps on its
// own clock ([liveSweep]). The sweep used to run inside the next request to
// reach this node's readings, which on a quiet node may never come — and in a
// fleet the owner is not told when a launch it was read for stops (the node
// serving the dashboard reads the run's record first, answers `not_running`
// itself and never asks the owner again), so every reading a person had been
// watching stayed in memory until the node stopped.
const liveIdle = 30 * time.Second

// liveSweep is how often the readings nobody has asked about for [liveIdle]
// are looked for: a third of it, so a reading is let go between liveIdle and
// four thirds of it after its last request. A finer sweep would buy a few
// seconds of a reading's memory for a walk of every reading each time.
const liveSweep = liveIdle / 3

// liveHold is how much of a reading's settled text the owner keeps: twice what
// a viewer holds, because a viewer up to [MaxRunTextBytes] behind is answered
// from it, and its digest covers the [MaxRunTextBytes] before where it holds
// to — so the owner needs both.
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
// A read runs under the node's context rather than a request's, because it
// must outlive the request that started it ([liveFeed.current]). A request
// that arrives after Stop is answered with the read's own failure, rather
// than reading a box this node is letting go of.
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
// It waits for the reading at most until ctx ends — which a caller sets from
// the asker's budget — and then answers with the last read there was, stamped
// with when it was taken. A reading that has never read anything by then is an
// error saying so.
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
	return liveSnapshot{}, fmt.Errorf("sandbox: reading the box of run %s took longer than this "+
		"request could wait; it goes on, and the next request reads what it found", f.run.TurnID)
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
	// A reading that ended — the node stopped, or the launch was let go —
	// reads nothing, whatever a backend would make of a dead context.
	if err := ctx.Err(); err != nil {
		return LiveRead{}, fmt.Errorf("sandbox: the reading of run %s has ended: %w", f.run.TurnID, err)
	}
	var m *Manager
	if f.feeds.manager != nil {
		m = f.feeds.manager()
	}
	if m == nil {
		return LiveRead{}, errors.New("sandbox: this node owns the run and has no sandbox " +
			"backend to reach its box with — providers.sandbox was removed by an apply")
	}
	run := f.run
	// ATTACHED, NEVER RECONNECTED: a read changes nothing, and Connect wakes
	// a paused box — which a read that took a running record a moment
	// before the collection paused the box would have done.
	box, runner, err := m.Attach(ctx, Placement(run.Placement), run.SandboxID, run.CodingAgent)
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

// liveSnapshot is what a reading has settled, as the owner keeps it.
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
		// Trimmed in steps rather than on every read, and on a line where
		// there is one — a single line longer than the whole hold is cut
		// on a character, which a reset window marks as the part it is.
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
// is less. False where the owner no longer keeps the whole window, and for an
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
