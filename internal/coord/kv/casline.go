package kv

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/backoff"
)

// fleetWriters is the head count every fleet key is bounded for: the most
// nodes one fleet runs, each one contender on a key once its writes there
// are coalesced.
//
// A HUNDRED AND TWENTY-EIGHT: the largest fleet this engine has been measured
// at is a hundred nodes — the hundred leaf nodes whose claims found the
// replica read (kv.go's "Every single-key read is the leader's"), and the
// hundred nodes claiming ten thousand seats that sized the lease gate's view
// (gate.go) — rounded up to the power of two the backoff's ceiling doubles
// to ([casBackoffLevels]). It is a design limit stated where it binds, not a
// refusal, and it is MEASURED AT ITS OWN SIZE: BenchmarkAnOrgCounterChargedThroughLeaves,
// every node a leaf of its own on one member with sixty-four charges in a
// closed loop on each, the org's counter their one key, fifty thousand
// charges a run, two runs a size, on a four-CPU machine shared with other work
// (load 3.5 to 7). The most attempts any batch took were 4 and 7 at three
// nodes, 6 and 10 at eight, 8 at sixteen, 9 and 10 at thirty-two, 10 at
// sixty-four and 9 and 10 at a hundred and twenty-eight — flat past thirty-two,
// and a twenty-fifth of [fleetCASAttempts] at this head count — and no charge
// failed. What grows with the fleet is the WAIT: the slowest charges' 99th
// percentile was 0.17 and 0.24 s at three nodes, 1.6 and 1.9 s at sixteen,
// 7.7 and 8.0 s at sixty-four and 7.1 and 11.4 s at a hundred and
// twenty-eight, the key taking 2,700 to 4,800 charges a second there — every
// node holding its line busy without a pause, which a fleet's rounds, each
// charged once its model call has answered, do not.
const fleetWriters = 128

// fleetCASAttempts bounds the compare-and-swap attempts one write is carried
// in: twice [fleetWriters], the margin [casAttempts] gives its own head count.
//
// The cost of setting it too high is nil — a retry follows only a write that
// landed, so it runs only while the store is answering and someone is
// winning — and the cost of too low is a healthy contended write failed
// closed as an outage. Exhausting it is [contended], never a refusal.
const fleetCASAttempts = 2 * fleetWriters

// casBackoffLevels is how many times the wait between a lost attempt and the
// next may double: the window at the top level is 1<<casBackoffLevels round
// trips — 256, so the writers of a key at [fleetWriters] spread over twice as
// many round trips as there are of them, and each attempt most likely finds
// the key quiet for the one round trip it needs.
const casBackoffLevels = 8

// casBackoffJitter is the widest spread [backoff.Jitter] gives: a key's
// writers lost the same race at the same instant, and only the spread
// separates them at the next.
const casBackoffJitter = 0.9

// casWindow is the wait before an attempt at level: nothing at level zero or
// with no round trip yet measured, else a jittered window of 2^level round
// trips.
func casWindow(level int, rtt time.Duration) time.Duration {
	if level <= 0 || rtt <= 0 {
		return 0
	}
	return backoff.Jitter(rtt<<min(level, casBackoffLevels), casBackoffJitter)
}

// casRTTGain is how far one attempt's round trip moves the estimate a window
// is counted in: an eighth of the difference, the smoothing TCP gives its
// round-trip estimate (RFC 6298's alpha, 1/8).
const casRTTGain = 8

// casRTTCeiling is the most one attempt's round trip counts for: this many
// times the estimate it moves, so no one attempt moves the estimate by more
// than an eighth of itself ((casRTTCeiling-1)/[casRTTGain]).
//
// A window MULTIPLIES the estimate — by up to 256 — so an estimate one
// attempt can drag would turn one stalled attempt (a starved scheduler, a
// pause on the broker) into minutes of waiting for every write behind it,
// where what separates a key's writers is how long a write USUALLY holds the
// key. A latency that really moved is still followed — by up to an eighth of
// the estimate an attempt, so a tenfold rise is nine-tenths in after
// twenty-six attempts, and every attempt this node makes on the bucket is one.
//
// A GUARD BY CONSTRUCTION MORE THAN A MEASURED WIN. Against a window counted
// in the last attempt's round trip whole, in the benchmark [fleetWriters]
// names, the slowest charges' 99th percentile was 1.7 and 3.7 s here against
// 3.2 and 5.6 s on the last sample at thirty-two nodes, and 7.1 and 11.4 s
// against 5.6 and 10.5 s at a hundred and twenty-eight — within the noise —
// while the most attempts a batch took at a hundred and twenty-eight were 9
// and 10 here against 11 and 11. What the benchmark cannot show is the one
// stalled attempt, which a closed loop dilutes and a burst does not.
const casRTTCeiling = 2

// roundTrip is how long an attempt on one bucket takes this node: from its
// read to its write's answer, landed or lost.
//
// NODE-WIDE, PER BUCKET, AND FED BY EVERY ATTEMPT. A round trip is a fact
// about this node's path to the bucket's stream, never about one key or one
// contention episode, so it lives as long as the store and every attempt the
// node makes on the bucket refines it — a landed one holds the key exactly as
// long as a lost one. Kept per episode it restarted from one sample whenever a
// line went idle, and charges arrive in bursts, so lines idle constantly: the
// first loss of every burst set the window by itself.
//
// SEEDED BY THE SMALLER OF ITS FIRST TWO SAMPLES, and nothing is counted in it
// until it has two. The first attempts a process makes on a bucket are the
// ones most likely to be slow — a connection, a stream's first lookup — and an
// estimate seeded by one of them alone would set every window after it, since
// no later sample may move it by more than an eighth ([casRTTCeiling]). Until
// then a lost attempt is asked again at once, as every loop here once was.
type roundTrip struct {
	mu      sync.Mutex
	samples int
	srtt    time.Duration
}

// observe counts one attempt that took sample.
func (r *roundTrip) observe(sample time.Duration) {
	if sample <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch r.samples {
	case 0:
		r.srtt = sample
	case 1:
		r.srtt = min(r.srtt, sample)
	default:
		sample = min(sample, casRTTCeiling*r.srtt)
		r.srtt += (sample - r.srtt) / casRTTGain
	}
	r.samples++
}

// estimate is the round trip a window is counted in, or zero before there are
// two samples to seed it.
func (r *roundTrip) estimate() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.samples < 2 {
		return 0
	}
	return r.srtt
}

// decayLevel is what a line's level comes to after it sat idle for idle: one
// level down for every window at that level the key was left quiet, and what
// is left over of the window it is in.
//
// A LEVEL OUTLIVES ITS LINE'S IDLENESS, because it is this node's knowledge of
// how contended the key is, and that does not vanish when the node's queue
// empties for a moment — while the node is busy the level comes down one per
// write that lands, about one a window, and idle it comes down at the same
// pace. Forgotten at once, a node whose charges arrive in bursts would come
// back at every burst as though nobody else wrote the key, ahead of the nodes
// still waiting out the windows it once shared with them.
func decayLevel(level int, idle, rtt time.Duration) (int, time.Duration) {
	if rtt <= 0 {
		return 0, 0
	}
	for level > 0 {
		window := rtt << level
		if idle < window {
			break
		}
		idle -= window
		level--
	}
	return level, idle
}

// casRetry runs attempt until it settles, at most [fleetCASAttempts] times,
// waiting between a lost attempt and the next a window ([casWindow]) of the
// round trips trips holds, and feeding every attempt it makes into trips.
// attempt answers settled once it has its answer, or an error, which ends the
// loop as it is; false with no error is a lost race. It answers whether the
// loop settled, and a context that ends during a wait as what is named
// failing.
//
// For a loop whose contention is not coalesced because it is not a
// read-modify-write — the creates answered by the record that won, and the
// unfollow — so each call is its own loop, on its caller's context, and its
// level its own; a line keeps its level on the line ([writeLines]).
func casRetry(ctx context.Context, trips *roundTrip, what string,
	attempt func() (settled bool, err error)) (bool, error) {

	level := 0
	for range fleetCASAttempts {
		if wait := casWindow(level, trips.estimate()); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return false, unavailable(what, ctx.Err())
			case <-timer.C:
			}
		}
		start := time.Now()
		settled, err := attempt()
		if err != nil {
			return settled, err
		}
		trips.observe(time.Since(start))
		if settled {
			return true, nil
		}
		level = min(level+1, casBackoffLevels)
	}
	return false, nil
}

// codec is how one bucket's values are read and written by a line, and what
// a failure to read one is called.
type codec[T any] struct {
	// decode is handed the record's bytes as the store holds them and must
	// answer a value of its own: a write's change may change what it is
	// handed, and a batch decided again is decided from a fresh decode.
	decode func(raw []byte) (T, error)
	encode func(T) ([]byte, error)

	// reading and decoding name the step a failed read or decode is
	// reported as ([unavailable]), shared by every write of the batch it
	// failed.
	reading, decoding string
}

// writeLines coalesces this node's read-modify-writes of one bucket's keys:
// per key, ONE compare-and-swap decides every write in flight on it.
//
// A GROUP COMMIT PER KEY, the code committer's shape (internal/code's
// Committer): one attempt in flight at a time, no linger, and every write
// that queued while it was in flight in the next. An attempt reads the record
// ONCE and decides each write IN ARRIVAL ORDER against what the ones before
// it left, each answered exactly as it would have been alone at that point of
// a serial order — so a charge is admitted or refused on its own and a
// refusal is never a batch's. A write is answered only from the attempt that
// landed, or from the read when nothing in the batch writes; a lost attempt
// wrote nothing, so the batch goes back to the head of the line, everything
// that queued meanwhile behind it, and the next attempt decides them all
// again.
//
// TAKEN OR WITHDRAWN, NEVER BOTH. A write is TAKEN only while an attempt
// carrying it is in flight, and that attempt may land, so a caller who hangs
// up then is told what the attempt did, never that nothing happened — for the
// company's half of a charge, a withdrawal after the write landed would leave
// the round on the company with the seat never counted and nobody told. At
// every other moment nothing it asked for can have landed: before its first
// attempt, and between a lost attempt and the next. Then a write whose
// caller's context ends is WITHDRAWN, answered with its context's error and
// counted nowhere — at once if its goroutine is watching, and otherwise as the
// next attempt is formed, under the lock that forms it. So every wait a line
// makes is made holding no taken write ([writeLines.waitTurn]), and arrivals
// during a wait join the attempt that follows it.
//
// NO GOROUTINE OF ITS OWN. A line is run by the goroutine of the write at its
// head, which waits for its own answer anyway, and handed to the goroutine of
// the next write once that one is answered — so every goroutine here is a
// caller's, each returns once its own write is answered, and nothing outlives
// the calls that started it. An attempt runs on a context with no caller's
// cancellation in it ([context.WithoutCancel]): the client bounds each request
// by its own API timeout, and [fleetCASAttempts] the writes it carries.
//
// A CALLER'S PANIC IS ITS OWN. Every write's change runs on the goroutine
// leading its batch, so a change that panics is recovered there, the write
// answered with its panic — raised again on its own caller's goroutine — and
// the batch decided again without it ([lineWrite.decide]). A panic in the
// line's own work fails the writes of the attempt in flight with an error
// naming the line, never a write, and the stack it panicked on ([linePanic]),
// hands the line on, and is raised again on the goroutine that led it
// ([writeLines.lead]): left to unwind, it left the line led by nobody and
// every later write on the key on this node waiting for ever.
//
// The zero value needs its codec set; the lines map is made on first use.
type writeLines[T any] struct {
	codec codec[T]

	// trips is the round trip every attempt on the bucket feeds and every
	// line's window is counted in ([roundTrip]).
	trips roundTrip

	mu    sync.Mutex
	lines map[string]*writeLine[T]

	// swept is how many lines the map held after its last sweep: an idle
	// line is kept while it remembers a level ([decayLevel]), and a sweep
	// removes the ones that have forgotten it whenever the map has doubled
	// since — so the sweeps cost a constant a line, and the map stays
	// within twice the lines with something to remember.
	swept int

	// observe, when set, is told each settled batch's key, how many writes
	// it answered and the attempts the line made for it; after, when set,
	// stands in for the timer a wait is made with; and now for the clock
	// a wait and an idle line are scheduled by. A test's seams, unset in
	// production — an attempt's round trip is measured on the real clock
	// whatever now says.
	observe func(key string, batch, attempts int)
	after   func(time.Duration) <-chan time.Time
	now     func() time.Time
}

// clock is the instant waits and idleness are scheduled by.
func (l *writeLines[T]) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

// writeLine is one key's queue on this node. Every field is held under its
// writeLines' mu.
type writeLine[T any] struct {
	queue []*lineWrite[T]

	// leader is the write whose goroutine runs the line now: the head of
	// the queue it was handed. Nil exactly when the line is idle.
	leader *lineWrite[T]

	// level is how many times the line's window has doubled ([casWindow]):
	// one up for every attempt that lost, one down for every one that
	// landed — multiplicative increase, linear decrease — and down by the
	// time the line sat idle ([decayLevel]).
	//
	// IT OUTLIVES THE BATCH THAT RAISED IT. A node that went straight back
	// to an immediate attempt after every win would take the key at once
	// each time — its own next batch was formed while it waited — while
	// every node that lost waited out a doubled window, so the node that
	// won last would keep winning and the others would spend their attempts
	// behind it: the capture that makes a fixed attempt count a coin toss.
	// Kept, the level makes a node that has just won wait about as long as
	// the nodes it beat, so each key's writers take it in turns.
	level int

	// lost is how many attempts the line has lost since a batch last
	// settled, for observe.
	lost int

	// until is the instant the line's next attempt may be made: set when a
	// wait is begun, and kept across a hand-on, so the write that inherits
	// the line waits out what is left of it rather than a window anew.
	until time.Time

	// idle is when the line went idle, while it is kept for its level.
	idle time.Time
}

// lineWrite is one caller's write.
type lineWrite[T any] struct {
	ctx      context.Context
	verb     string
	resource string
	change   func(stored T, found bool) (T, bool)

	// attempts is how many attempts have carried the write.
	attempts int

	result   T
	err      error
	panicked *changePanic // its own change's panic, raised on its caller's goroutine
	answered bool
	done     chan struct{} // closed once the write is answered
	lead     chan struct{} // closed once its goroutine is handed the line
}

// write is one read-modify-write of key, coalesced with every other write in
// flight on it on this node.
//
// change is handed the stored value — the zero T and false where the key
// holds none — as the writes before it in its batch left it, and answers the
// value to write and whether to write it at all; write answers what change
// last answered, written or not. verb names the write in the error it fails
// with, and resource names the record a loop that never settles reports. A
// change that panics is raised again here, on its caller's goroutine.
func (l *writeLines[T]) write(ctx context.Context, kv jetstream.KeyValue, js jetstream.JetStream,
	key, resource, verb string, change func(stored T, found bool) (T, bool)) (T, error) {

	w := &lineWrite[T]{
		ctx: ctx, verb: verb, resource: resource, change: change,
		done: make(chan struct{}), lead: make(chan struct{}),
	}
	rtt := l.trips.estimate()
	now := l.clock()
	l.mu.Lock()
	if l.lines == nil {
		l.lines = make(map[string]*writeLine[T])
	}
	ln := l.lines[key]
	switch {
	case ln == nil:
		ln = &writeLine[T]{}
		l.lines[key] = ln
	case ln.leader == nil:
		// IDLE, and kept for the level it remembers: brought down by the
		// time it sat idle, and its next attempt held off by what is left
		// of the window it would have been waiting out.
		level, into := decayLevel(ln.level, now.Sub(ln.idle), rtt)
		ln.level, ln.idle, ln.lost = level, time.Time{}, 0
		ln.until = now.Add(casWindow(level, rtt) - into)
	}
	ln.queue = append(ln.queue, w)
	leads := ln.leader == nil
	if leads {
		ln.leader = w
	}
	l.sweepLocked(now, rtt)
	l.mu.Unlock()

	if !leads {
		select {
		case <-w.done:
			return w.answer()
		case <-w.lead:
		case <-ctx.Done():
			l.withdraw(key, ln, w)
			// Withdrawn, or carried by an attempt in flight, which answers
			// it either way.
			<-w.done
			return w.answer()
		}
	}
	l.lead(ctx, kv, js, key, ln, w)
	<-w.done
	return w.answer()
}

// sweepLocked removes the idle lines whose level has decayed to nothing, once
// the map has doubled since it last did. The caller holds l.mu.
func (l *writeLines[T]) sweepLocked(now time.Time, rtt time.Duration) {
	if len(l.lines) <= 2*l.swept {
		return
	}
	for key, ln := range l.lines {
		if ln.leader != nil {
			continue
		}
		if level, _ := decayLevel(ln.level, now.Sub(ln.idle), rtt); level == 0 {
			delete(l.lines, key)
		}
	}
	l.swept = len(l.lines)
}

// withdraw takes w out of its line if no attempt is carrying it, answering it
// with its context's error, and passes the line on if w was leading it.
func (l *writeLines[T]) withdraw(key string, ln *writeLine[T], w *lineWrite[T]) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.withdrawLocked(key, ln, w)
}

// withdrawLocked is [writeLines.withdraw] with l.mu held.
func (l *writeLines[T]) withdrawLocked(key string, ln *writeLine[T], w *lineWrite[T]) {
	i := slices.Index(ln.queue, w)
	if i < 0 {
		return
	}
	ln.queue = slices.Delete(ln.queue, i, i+1)
	w.settle(*new(T), unavailable(w.verb, w.ctx.Err()))
	if ln.leader == w {
		l.handOn(key, ln)
	}
}

// handOn gives the line to the write at the head of its queue, or retires it:
// removed if it remembers no level, else kept idle for the one it does. The
// caller holds l.mu.
func (l *writeLines[T]) handOn(key string, ln *writeLine[T]) {
	if len(ln.queue) > 0 {
		ln.leader = ln.queue[0]
		close(ln.leader.lead)
		return
	}
	ln.leader = nil
	if ln.level == 0 {
		delete(l.lines, key)
		return
	}
	ln.idle, ln.until = l.clock(), time.Time{}
}

// lead runs the line from w's goroutine — wait, form a batch from the queue,
// one attempt, and again after a loss — and hands it on once w is answered.
//
// W'S OWN CONTEXT IS JUDGED UNDER THE SAME LOCK THAT FORMS EVERY BATCH, so a
// leader whose caller hung up is withdrawn rather than taken, and the line
// goes to the next write without an attempt made on its behalf.
func (l *writeLines[T]) lead(ctx context.Context, kv jetstream.KeyValue, js jetstream.JetStream,
	key string, ln *writeLine[T], w *lineWrite[T]) {

	run := context.WithoutCancel(ctx)
	// carried is what this goroutine has out of the queue and must answer,
	// and no other goroutine may: the batch while its attempt is in flight,
	// and the writes a lost attempt spent the last of their attempts on.
	var carried []*lineWrite[T]
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		// THE LINE'S OWN WORK, never a write's change, which decide has
		// already recovered and answered alone. The stack is taken here,
		// while the panic's frames are still on this goroutine's stack.
		failed := &linePanic{value: r, stack: debug.Stack()}
		l.mu.Lock()
		if i := slices.Index(ln.queue, w); i >= 0 {
			ln.queue = slices.Delete(ln.queue, i, i+1)
			carried = append(carried, w)
		}
		for _, x := range carried {
			if !x.answered {
				x.settle(*new(T), unavailable(x.verb, failed))
			}
		}
		l.handOn(key, ln)
		l.mu.Unlock()
		panic(r)
	}()
	for {
		if !l.waitTurn(key, ln, w) {
			return
		}
		l.mu.Lock()
		if w.ctx.Err() != nil {
			l.withdrawLocked(key, ln, w)
			l.mu.Unlock()
			return
		}
		carried = l.takeLocked(ln, w)
		l.mu.Unlock()

		start := time.Now()
		batch, outcome, failed := l.attempt(run, kv, js, key, carried)
		took := time.Since(start)
		if outcome != attemptFailed && outcome != attemptUnwritten {
			l.trips.observe(took)
		}
		rtt := l.trips.estimate()

		if outcome == attemptLost {
			var spent []*lineWrite[T]
			kept := make([]*lineWrite[T], 0, len(batch))
			// JUDGED UNDER THE LOCK EVERY WITHDRAWAL TAKES. A caller who hung
			// up while the attempt was in flight found nothing to withdraw and
			// is waiting for its answer; judged outside the lock, a hang-up
			// between the judgement and the requeue would put back a write
			// nobody watches any more, and a line handed on to it would wait
			// for ever with every later write on the key behind it.
			l.mu.Lock()
			for _, x := range batch {
				switch x.attempts++; {
				case x.ctx.Err() != nil:
					// The attempt wrote nothing: withdrawn now rather than
					// when the next one forms.
					x.settle(*new(T), unavailable(x.verb, x.ctx.Err()))
				case x.attempts >= fleetCASAttempts:
					spent = append(spent, x)
				default:
					kept = append(kept, x)
				}
			}
			ln.level = min(ln.level+1, casBackoffLevels)
			ln.lost++
			lost := ln.lost
			ln.until = l.clock().Add(casWindow(ln.level, rtt))
			// BACK AT THE HEAD OF THE LINE, ahead of whatever queued while
			// the attempt was in flight: nothing it asked for landed, so a
			// caller who hangs up from here on is withdrawn — and a panic
			// from here on answers only the spent writes, the rest being
			// the queue's again.
			ln.queue = append(kept, ln.queue...)
			carried = spent
			l.mu.Unlock()
			if len(spent) > 0 {
				l.report(key, len(spent), lost)
			}
			for _, x := range spent {
				x.settle(*new(T), contended(x.verb, x.resource))
			}
			carried = nil
			if !w.answered {
				continue
			}
			// Withdrawn, spent, or its own change panicked: the line goes to
			// the next write, which waits out the same window.
			l.mu.Lock()
			l.handOn(key, ln)
			l.mu.Unlock()
			return
		}

		l.mu.Lock()
		if outcome == attemptLanded {
			ln.level = max(ln.level-1, 0)
			// The next batch waits out the window at the level it was left
			// at — the turn a node that just won gives the nodes it beat.
			ln.until = l.clock().Add(casWindow(ln.level, rtt))
		}
		attempts := ln.lost + 1
		ln.lost = 0
		l.mu.Unlock()
		l.report(key, len(batch), attempts)
		for _, x := range batch {
			if failed != nil {
				x.settle(*new(T), failed(x))
			} else {
				x.settle(x.result, nil)
			}
		}
		carried = nil
		l.mu.Lock()
		l.handOn(key, ln)
		l.mu.Unlock()
		return
	}
}

// report tells the test seam about a settled batch.
func (l *writeLines[T]) report(key string, batch, attempts int) {
	if l.observe != nil {
		l.observe(key, batch, attempts)
	}
}

// waitTurn waits out the line's window before w forms a batch, holding nothing
// a caller can no longer take back: every write it will carry is still in the
// queue, where a caller who hangs up withdraws it, and everything that queues
// meanwhile joins the batch. It answers false when w itself was withdrawn
// while it waited — the line then handed on to the next write, which waits out
// what is left of the same window.
func (l *writeLines[T]) waitTurn(key string, ln *writeLine[T], w *lineWrite[T]) bool {
	l.mu.Lock()
	wait := ln.until.Sub(l.clock())
	l.mu.Unlock()
	if wait <= 0 {
		return true
	}
	var over <-chan time.Time
	if l.after != nil {
		over = l.after(wait)
	} else {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		over = timer.C
	}
	select {
	case <-over:
		return true
	case <-w.ctx.Done():
		l.withdraw(key, ln, w)
		return false
	}
}

// takeLocked forms a batch from the whole queue, w first: every write but one
// whose caller has hung up, which is withdrawn. The caller holds l.mu, and has
// judged w's own context under it.
func (l *writeLines[T]) takeLocked(ln *writeLine[T], w *lineWrite[T]) []*lineWrite[T] {
	batch := make([]*lineWrite[T], 0, len(ln.queue))
	for _, x := range ln.queue {
		if x != w && x.ctx.Err() != nil {
			x.settle(*new(T), unavailable(x.verb, x.ctx.Err()))
			continue
		}
		batch = append(batch, x)
	}
	ln.queue = nil
	return batch
}

// attemptOutcome is how one attempt ended.
type attemptOutcome int

const (
	attemptLanded    attemptOutcome = iota // the write landed
	attemptUnwritten                       // nothing in the batch writes: answered from the read
	attemptLost                            // another writer's write landed first; nothing was written
	attemptFailed                          // the store failed, or a value would not encode
)

// attempt makes ONE compare-and-swap for batch: one read, every write decided
// in arrival order against what the ones before it left, and one write of
// what they left. It answers the writes still in the batch — a write whose own
// change panicked is answered with its panic and the rest decided again
// without it — how the attempt ended, and, when it failed, what each write is
// answered with.
func (l *writeLines[T]) attempt(ctx context.Context, kv jetstream.KeyValue, js jetstream.JetStream,
	key string, batch []*lineWrite[T]) ([]*lineWrite[T], attemptOutcome, func(*lineWrite[T]) error) {

	var (
		raw      []byte
		found    bool
		revision uint64
	)
	entry, err := getLatest(ctx, js, kv, key)
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
	case err != nil:
		return batch, attemptFailed, func(*lineWrite[T]) error { return unavailable(l.codec.reading, err) }
	default:
		raw, found, revision = entry.Value(), true, entry.Revision()
	}
	for {
		var stored T
		if found {
			if stored, err = l.codec.decode(raw); err != nil {
				return batch, attemptFailed, func(*lineWrite[T]) error { return unavailable(l.codec.decoding, err) }
			}
		}
		// EACH IN ARRIVAL ORDER, each against what the writes before it
		// left: the value a lone write would have read at that point of a
		// serial order. A write that writes nothing leaves the value as it
		// found it, exactly as it would have left the record.
		next, present, writes, panicked := stored, found, false, -1
		for i, x := range batch {
			changed, write, p := x.decide(next, present)
			if p != nil {
				panicked = i
				x.panicked = p
				break
			}
			x.result = changed
			if write {
				next, present, writes = changed, true, true
			}
		}
		if panicked >= 0 {
			// DECIDED AGAIN WITHOUT IT, from a fresh decode: a change that
			// panicked may have left the value it was handed half changed.
			// A copy, because the batch's slice is the one a panic in the
			// line's own work answers from.
			x := batch[panicked]
			batch = slices.Delete(slices.Clone(batch), panicked, panicked+1)
			x.settle(*new(T), fmt.Errorf("coord/kv: %s: its change panicked: %v", x.verb, x.panicked.value))
			continue
		}
		if !writes {
			return batch, attemptUnwritten, nil
		}
		encoded, err := l.codec.encode(next)
		if err != nil {
			return batch, attemptFailed, func(*lineWrite[T]) error { return err }
		}
		if found {
			_, err = kv.Update(ctx, key, encoded, revision)
		} else {
			_, err = createKey(ctx, js, kv, key, encoded)
		}
		switch {
		case err == nil:
			return batch, attemptLanded, nil
		case errors.Is(err, jetstream.ErrKeyRevisionMismatch), errors.Is(err, jetstream.ErrKeyExists):
			return batch, attemptLost, nil
		default:
			return batch, attemptFailed, func(x *lineWrite[T]) error { return unavailable(x.verb, err) }
		}
	}
}

// changePanic is a write's own change's panic, recovered on the goroutine that
// led its batch and raised again on its caller's, with the stack it happened
// on — the stack the raise itself shows is the caller's, which ran none of it.
type changePanic struct {
	value any
	stack []byte
}

func (p *changePanic) Error() string {
	return fmt.Sprintf("coord/kv: a write's change panicked on the goroutine leading its batch: %v\n\n%s",
		p.value, p.stack)
}

// Unwrap is the panic's own value, where it was an error.
func (p *changePanic) Unwrap() error {
	err, _ := p.value.(error)
	return err
}

// linePanic is a panic in the line's OWN work — its read, its codec, its
// write, its queue's bookkeeping, its seams — recovered on the goroutine leading the batch and answered
// to every write the batch carried, with the stack it happened on.
//
// NEVER A WRITE'S CHANGE: a change that panics is recovered inside the
// decision ([lineWrite.decide]) and answered to its own caller alone, before
// the line's recover could see it. So this error says the line panicked and
// nothing else did: one blaming another write coalesced with this one would
// send its reader looking for a change closure that never panicked.
//
// The outcome is UNKNOWN ([coord.ErrUnavailable] wraps it): the panic may have
// come before the record was read, inside the store client's write, or after
// the write landed, and only the stack says which.
type linePanic struct {
	value any
	stack []byte
}

func (p *linePanic) Error() string {
	return fmt.Sprintf("coord/kv: the line writing this key on this node panicked in its own work "+
		"(reading, decoding, encoding or writing the record, or keeping its queue), never in a write's "+
		"change, so whether this write landed is unknown; it is a bug in the engine, to be reported "+
		"with this stack: %v\n\n%s", p.value, p.stack)
}

// Unwrap is the panic's own value, where it was an error.
func (p *linePanic) Unwrap() error {
	err, _ := p.value.(error)
	return err
}

// decide runs the write's change, recovering a panic in it as the write's own.
func (w *lineWrite[T]) decide(stored T, found bool) (next T, write bool, p *changePanic) {
	defer func() {
		if r := recover(); r != nil {
			p = &changePanic{value: r, stack: debug.Stack()}
		}
	}()
	next, write = w.change(stored, found)
	return next, write, nil
}

// settle answers the write: its result and its error, then its waiter.
func (w *lineWrite[T]) settle(result T, err error) {
	w.result, w.err, w.answered = result, err, true
	close(w.done)
}

// answer is what the write's caller is told, on its own goroutine: its own
// change's panic raised again, or its result.
func (w *lineWrite[T]) answer() (T, error) {
	if w.panicked != nil {
		panic(w.panicked)
	}
	return w.result, w.err
}
