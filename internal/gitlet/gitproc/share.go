package gitproc

import (
	"container/list"
	"math"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// The transfer lane's fair share: whose transfer goes next when every slot is
// held (package doc, "Slots: three lanes, each sized by what it spends";
// docs/concepts/gitlet.md, "Fair share"). A run costs the GREATER of its CPU
// ([Result.CPU]) and its OCCUPANCY, the slot-time it held (occupancyRate): CPU
// alone charges nothing to a slow client holding a slot for an hour, occupancy
// alone undercharges a storm of short, CPU-heavy upload-packs. Occupancy
// ACCRUES while the slot is held, so a principal holding every slot with
// hour-long clones never reads as idle; CPU past it is charged when the run
// ends, the only moment it is known. The share orders only who goes next:
// SlotWait still bounds every wait, and every class but [ShareRun] is
// work-conserving.

// ShareClass is whose turn a TRANSFER waits for when the transfer lane is
// full: a named string with a closed set, so a value a newer build passes is
// a value, refused by [Share.Valid] rather than scheduled as nobody's.
type ShareClass string

const (
	// SharePerson is the holder of a human seat: a person, through any
	// credential of theirs, or a Tier A token or service account the
	// identity directory binds to a human seat nobody else holds. A person
	// waits interactively, at a terminal.
	SharePerson ShareClass = "person"

	// ShareMachine is a credential acting as nobody — a Tier A token or a
	// service account's machine token bound to no seat: a CI system, a
	// mirror, a deploy script — which stands for many jobs behind one
	// credential and waits in a pipeline nobody watches second by second.
	ShareMachine ShareClass = "machine"

	// ShareRun is a coding box: the git a seat's detached executor makes
	// with its run's credential, behind no person at all — generated code,
	// whose turn waits behind every person's and machine's and whose class
	// holds at most half the lane ([ShareClass.Cap]).
	ShareRun ShareClass = "run"
)

// ShareClasses is every class, in the order the scheduler reads them and a
// message lists them; a fresh slice each call, so no caller can change what
// [ShareClass.Valid] accepts.
func ShareClasses() []ShareClass { return []ShareClass{SharePerson, ShareMachine, ShareRun} }

// Valid reports whether c is one of [ShareClasses].
func (c ShareClass) Valid() bool { return c.Weight() > 0 }

// Weight is the class's share of a CONTENDED transfer lane, measured in what
// its runs spend: a person 4, a machine 2, a coding box 1; 0 for a class that
// is none.
//
// A product judgment: a person waits at a terminal, a machine for a pipeline,
// a box for nobody (TestAContendedTransferLaneSharesByClass). The run's 1 is
// unmeasured: BenchmarkTransferShare's runs arms cannot tell weights apart on
// a two-slot lane, so it is to be pinned from a run on a production-wide one
// (docs/concepts/gitlet.md, "Fair share"). A ratio, never a cap, so a lone CI
// token keeps the whole lane; a constant, not an option, so which node a
// balancer picked never decides who waits.
func (c ShareClass) Weight() int {
	switch c {
	case SharePerson:
		return 4
	case ShareMachine:
		return 2
	case ShareRun:
		return 1
	}
	return 0
}

// Cap is the most of a lane of slots slots the class's runs may HOLD at once:
// the whole lane for a person and a machine, and half of it, at least one, for
// a coding box. Uncapped, boxes' client-paced clones (as slow as
// objstore.MiBPace's floor) would hold every slot while their clients read,
// and a person's push or clone would wait out SlotWait; a box is generated
// code, so the lane cannot count on its manners. Half is gitlet.ShareOf's
// bound on seat git work on the read lane, spelled again because neither
// package imports the other (internal/gitlet/host's TestTheRunCapIsTheSeatShare
// holds the two equal).
func (c ShareClass) Cap(slots int) int {
	if c == ShareRun {
		return max(1, slots/2)
	}
	return slots
}

// Share is whose transfer a Cmd is.
type Share struct {
	Class ShareClass
	// Principal is who within the class: for a person the HANDLE of the
	// seat they hold, so one person's laptop, scripts and sign-ins are one
	// principal; for a machine the credential's NAME (`token:<id>`,
	// `pat:<id>`), so a service account with a token per CI runner is a
	// principal per runner; for a coding box its run's LAUNCH id, so each
	// job is a turn of its own and a relaunch starts a fresh one.
	Principal string
}

// maxSharePrincipal is the longest principal [Share.Valid] admits, in bytes:
// about four times the longest name an identity is given (internal/iam's
// MaxLogin, 64; `pat:` and a uuid; a launch uuid), so it refuses nothing a
// configuration or the identity directory admits and leaves a grammar room to
// widen.
const maxSharePrincipal = 255

// Valid reports a known class and a principal of 1..[maxSharePrincipal]
// bytes with no NUL.
//
// The principal keys the lane's book: an empty one would schedule a run as
// nobody's, and one too long or holding a NUL is a caller passing the wrong
// string, so it fails its run where the Cmd is prepared rather than open an
// account for a principal nobody is.
func (s Share) Valid() bool {
	return s.Class.Valid() && s.Principal != "" && len(s.Principal) <= maxSharePrincipal &&
		!strings.ContainsRune(s.Principal, 0)
}

// ShareWindow is how long a principal's spending counts against it: the time
// constant of the exponential decay every charge is read through.
//
// ONE MINUTE: past a CI checkout's burst of lazy fetches (seconds to tens of
// seconds), so a burst is charged while it lasts, and e^-3 (5 %) of it is
// left three minutes on, so a clone this morning does not slow a fetch now.
// BenchmarkTransferShare's decay arms measure it: a person whose own burst is
// three minutes old waits about as one who spent nothing does.
const ShareWindow = time.Minute

// shareForget is the decayed charge, in core-seconds, below which an account
// that neither runs nor waits is forgotten: a millisecond of a core, what a
// whole core-minute decays to after about eleven windows. An account that
// small decides no grant — every fresh principal reads zero — so the book holds
// the principals of the last few minutes and nothing else.
const shareForget = 0.001

// occupancyRate is what a held transfer slot is charged a second, in
// core-seconds, on a pool of cores READ slots (the node's cores) and slots
// TRANSFER slots: cores over slots, the share of a core one slot stands for,
// so a charge is a principal's dominant share of slots (memory) and cores.
// Never over one core: a transfer's git runs one thread (pack.threads=1; a
// receive-pack's lent threads are charged as CPU), so on a lane narrower than
// the cores slots are what runs out, and slots held alike are charged alike.
// Never a constant: a quarter everywhere would charge a slow credential on a
// 32-core, 16-slot node far less than a fast one holding the same slots
// (TestSlotsHeldAlikeAreChargedAlikeWhereSlotsRunOut), and overcharge a lane
// widened past four a core. New derives it where it sizes the lane; the book
// holds it for the lane's life.
func occupancyRate(cores, slots int64) float64 {
	return min(float64(cores)/float64(slots), 1)
}

// windowSeconds is ShareWindow as the decay's arithmetic reads it.
var windowSeconds = ShareWindow.Seconds()

// standing is a decayed charge in core-seconds, as of at, and the runs
// accruing occupancy into it, each at the book's occupancy rate.
//
// EXACT AT EVERY INSTANT: a run's accrual over dt is the decayed integral
// rate·W·(1−e^(−dt/W)), not rate·dt added at the next decision, which would
// make a principal's reading depend on how often others arrived, and read ten
// quiet minutes of holding every slot undecayed at once.
type standing struct {
	charged float64
	at      time.Time
	running int
}

// value is the standing as of t, each running run's accrual at rate
// core-seconds a second included; a t before at (a clock that stepped back)
// reads it as of at.
func (s *standing) value(t time.Time, rate float64) float64 {
	dt := t.Sub(s.at).Seconds()
	if dt <= 0 {
		return s.charged
	}
	keep := math.Exp(-dt / windowSeconds)
	return s.charged*keep + float64(s.running)*rate*windowSeconds*(1-keep)
}

// bring moves the standing to t, folding its runs' accrual at rate into its
// charge.
func (s *standing) bring(t time.Time, rate float64) {
	if t.After(s.at) {
		s.charged = s.value(t, rate)
		s.at = t
	}
}

// account is one principal's standing and its waiters.
type account struct {
	key Share
	standing
	waiters list.List // of *waiter, oldest first
}

// classBook is one class's standing — the sum of its accounts', kept beside
// them so a grant reads a class in one step — and its accounts that hold a
// waiter.
type classBook struct {
	standing
	waiting map[*account]struct{}
}

// run is one admitted transfer: whose it is, and since when it holds its slot.
type run struct {
	acct  *account
	since time.Time
}

// shareBook is the transfer lane's record of who has spent what, read and
// written under the lane's mutex. Nil on the other lanes, which grant in
// arrival order.
type shareBook struct {
	now func() time.Time
	// weight is ShareClass.Weight, replaced only by BenchmarkTransferShare's
	// runs arms (ShareClass.Weight says why it is no option).
	weight    func(ShareClass) int
	occupancy float64 // what a held slot is charged a second (occupancyRate)
	slots     int     // the lane's width, which each class's cap is of (ShareClass.Cap)
	byKey     map[Share]*account
	classes   map[ShareClass]*classBook
	swept     time.Time // the last forgetting pass
	seq       uint64    // arrivals so far: a waiter's place in arrival order
	metrics   *metrics.Recorder
}

// newShareBook is an empty book for a lane of slots slots on the clock now,
// charging a held slot occupancy core-seconds a second (occupancyRate of the
// lane's shape) and reporting each class's standing into rec (nil: nowhere).
func newShareBook(now func() time.Time, slots int, occupancy float64, rec *metrics.Recorder) *shareBook {
	b := &shareBook{
		now: now, weight: ShareClass.Weight, occupancy: occupancy, slots: slots,
		byKey: map[Share]*account{}, classes: map[ShareClass]*classBook{}, metrics: rec,
	}
	for _, c := range ShareClasses() {
		b.classes[c] = &classBook{waiting: map[*account]struct{}{}}
	}
	b.swept = now()
	return b
}

// account is s's account, opened at t when the book holds none.
func (b *shareBook) account(s Share, t time.Time) *account {
	a := b.byKey[s]
	if a == nil {
		a = &account{key: s, standing: standing{at: t}}
		b.byKey[s] = a
	}
	return a
}

// start admits a run of a's at t: from now its slot accrues into a and a's
// class.
func (b *shareBook) start(a *account, t time.Time) *run {
	c := b.classes[a.key.Class]
	a.bring(t, b.occupancy)
	c.bring(t, b.occupancy)
	a.running++
	c.running++
	return &run{acct: a, since: t}
}

// settle ends r at t, given the CPU its process tree spent: its occupancy
// accrued while it ran, and what the CPU spent past it is charged now — so
// the two together charge the GREATER of CPU and occupancy, exactly.
func (b *shareBook) settle(r *run, cpu time.Duration, t time.Time) {
	a := r.acct
	c := b.classes[a.key.Class]
	a.bring(t, b.occupancy)
	c.bring(t, b.occupancy)
	a.running--
	c.running--
	occupied := max(t.Sub(r.since).Seconds(), 0) * b.occupancy
	if past := cpu.Seconds() - occupied; past > 0 {
		a.charged += past
		c.charged += past
	}
	b.forgetIdle(a, t)
}

// enqueue queues w as s's newest waiter at t.
func (b *shareBook) enqueue(w *waiter, s Share, t time.Time) {
	a := b.account(s, t)
	b.seq++
	w.acct, w.seq = a, b.seq
	w.elem = a.waiters.PushBack(w)
	b.classes[s.Class].waiting[a] = struct{}{}
}

// unqueue takes w out of its account's queue, and its account out of its
// class's waiting set once it holds no other waiter.
func (b *shareBook) unqueue(w *waiter) {
	a := w.acct
	a.waiters.Remove(w.elem)
	if a.waiters.Len() == 0 {
		delete(b.classes[a.key.Class].waiting, a)
	}
}

// leave takes a waiter that was never granted out of its queue at t, and
// forgets its account if that left it idle.
func (b *shareBook) leave(w *waiter, t time.Time) {
	b.unqueue(w)
	b.forgetIdle(w.acct, t)
}

// mayStart reports whether a run of class c may take a free slot now: whether
// the class holds fewer than its cap ([ShareClass.Cap]) — every class but a
// coding box's always does.
func (b *shareBook) mayStart(c ShareClass) bool {
	return b.classes[c].running < c.Cap(b.slots)
}

// grant hands a released slot to WHO GOES NEXT at t (next) and starts its run
// on that waiter's account, or answers nil, leaving the slot free, where every
// waiter's class is at its cap: they wait for a release that leaves it under.
//
// Nothing may forget the account between unqueue and start: it is idle then,
// and once forgotten (as a fresh principal's would be) its runs charge an
// account no grant reads, so it, and at every sweep its class, read as having
// spent nothing.
func (b *shareBook) grant(t time.Time) *waiter {
	w := b.next(t)
	if w == nil {
		return nil
	}
	b.unqueue(w)
	w.run = b.start(w.acct, t)
	return w
}

// next is WHO GOES NEXT at t: among the classes holding a waiter that may
// start (mayStart: under its cap), the one with the least standing over its
// weight, the class whose oldest waiter is older on a tie; within it the
// principal with the least standing, the older head waiter on a tie; that
// principal's oldest waiter, still queued — nil where no class holding a
// waiter may start. O(principals waiting): bounded by the requests in flight,
// and measured by BenchmarkTransferGrant.
func (b *shareBook) next(t time.Time) *waiter {
	var (
		pick     *classBook
		score    float64
		pickHead uint64
	)
	for _, cls := range ShareClasses() {
		c := b.classes[cls]
		if len(c.waiting) == 0 || !b.mayStart(cls) {
			continue
		}
		s := c.value(t, b.occupancy) / float64(b.weight(cls))
		switch {
		case pick == nil || s < score:
			pick, score, pickHead = c, s, 0
		case s == score:
			// A TIE, which a fresh book is all of: the older waiter
			// first, as a FIFO lane would. Read only now, since it costs a
			// pass over each tied class's waiting principals.
			if pickHead == 0 {
				pickHead = oldestHead(pick)
			}
			if h := oldestHead(c); h < pickHead {
				pick, pickHead = c, h
			}
		}
	}
	if pick == nil {
		return nil
	}
	var (
		who   *account
		least float64
	)
	for a := range pick.waiting {
		v := a.value(t, b.occupancy)
		if who == nil || v < least || (v == least && head(a).seq < head(who).seq) {
			who, least = a, v
		}
	}
	return head(who)
}

// head is a's oldest waiter; a holds one.
func head(a *account) *waiter {
	w, _ := a.waiters.Front().Value.(*waiter)
	return w
}

// oldestHead is the arrival order of the oldest waiter c's principals hold.
func oldestHead(c *classBook) uint64 {
	oldest := uint64(math.MaxUint64)
	for a := range c.waiting {
		oldest = min(oldest, head(a).seq)
	}
	return oldest
}

// forgetIdle deletes a at t when it neither runs nor waits and its standing
// has decayed under shareForget, taking what is left of it out of its class.
func (b *shareBook) forgetIdle(a *account, t time.Time) {
	if a.running > 0 || a.waiters.Len() > 0 {
		return
	}
	v := a.value(t, b.occupancy)
	if v >= shareForget {
		return
	}
	delete(b.byKey, a.key)
	c := b.classes[a.key.Class]
	c.bring(t, b.occupancy)
	c.charged = max(c.charged-v, 0)
}

// sweep forgets every idle account at most once a ShareWindow — on the
// lane's own events, so the book needs no goroutine — and sets each class's
// standing to the sum of its accounts', exactly: the sum kept beside them
// drifts by a rounding a step, and the forgotten accounts' crumbs leave it.
func (b *shareBook) sweep(t time.Time) {
	if t.Sub(b.swept) < ShareWindow {
		return
	}
	b.swept = t
	sums := make(map[ShareClass]*standing, len(b.classes))
	for cls := range b.classes {
		sums[cls] = &standing{at: t}
	}
	for _, a := range b.byKey {
		b.forgetIdle(a, t)
		if b.byKey[a.key] == nil {
			continue
		}
		a.bring(t, b.occupancy)
		s := sums[a.key.Class]
		s.charged += a.charged
		s.running += a.running
	}
	for cls, c := range b.classes {
		c.standing = *sums[cls]
	}
}

// report sets each class's standing at t as the gauge crewlet.code.git.share
// reads it: the figure the next grant is decided by. Set at every event of the
// lane, under its mutex, so a later reading never carries an older one; a
// lane idle since holds the last.
func (b *shareBook) report(t time.Time) {
	if b.metrics == nil {
		return
	}
	for _, cls := range ShareClasses() {
		b.metrics.Set(metrics.CodeGitShare, b.classes[cls].value(t, b.occupancy), metrics.Attrs{"share": string(cls)})
	}
}
