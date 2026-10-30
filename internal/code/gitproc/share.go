package gitproc

import (
	"container/list"
	"math"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE TRANSFER LANE'S FAIR SHARE: whose transfer goes next when every slot is
// held.
//
// # Why the transfer lane is not first come, first served
//
// A transfer slot is held by whoever asked first, and one credential can ask
// for every slot: a CI system checking out a monorepo behind one token, a
// mirror catching up, a box's blame lazily fetching a blob at a time. In
// arrival order a person's clone queued behind sixty-four of them waits for
// sixty-four of them, and a lane of SlotWait answers it busy. So the lane
// grants a released slot to the CLASS of principal furthest below its weighted
// share of what the lane's runs have spent lately, and within the class to
// the PRINCIPAL that spent least — so the credential that made a storm waits
// behind every credential of its class that asked less, and a person waits
// behind it only while people have spent more than twice what the machines
// have lately: the machines' third of a contended lane.
//
// # Charged by what a transfer spends, and only while it matters
//
// A run costs the GREATER of the CPU its process tree spent ([Result.CPU]) and
// its OCCUPANCY, the slot-time it held at the share of a core one slot stands
// for on the lane's own shape (occupancyRate: a quarter of a core at the
// default four a core, a whole core on a lane narrower than the cores): CPU
// alone would charge nothing to a slow client holding a slot for an hour,
// occupancy alone would charge a lazy-fetch storm of short, CPU-heavy
// upload-packs a fraction of what it burns. Occupancy ACCRUES while the run
// holds its slot, so a principal holding every slot with hour-long clones
// never reads as idle; what CPU spends past the occupancy is charged when the
// run ends, the only moment it is known. Every charge DECAYS over
// [ShareWindow], so a burst counts against its credential while it lasts and
// is forgotten minutes after.
//
// # What it keeps of a FIFO lane, and what it does not
//
// It never leaves a slot free while anybody who MAY take it waits
// (WORK-CONSERVING for persons and machines: an arrival of either class that
// finds a free slot takes it whatever waits, so one class alone runs the lane
// at its full width), and it keeps a FIFO lane's BOUND: no waiter waits longer
// than SlotWait, whichever order it is granted in, so the share decides only
// who is answered first inside it. It does not keep a FIFO lane's ORDER, and
// so not every wait is as short as arrival order would make it: a person's
// clone waits behind a machine's while people have spent more than twice what
// the machines have lately, and the principal that spent most in its class
// goes last — while others who spent less keep arriving it can be passed over
// at every release, wait out SlotWait and be answered busy where arrival order
// would have handed it the next slot. That is the share doing what it is for:
// holding back the credential whose burst it is.
//
// # The one class that is not work-conserving: a coding box's
//
// A coding box's git ([ShareRun]) HOLDS at most half the lane
// ([ShareClass.Cap]), even with the other half free. The share orders only who
// is granted NEXT, so a class that holds every slot with client-paced
// transfers keeps it for as long as its clients read: a burst of coding-run
// launches is a clone a repository each, minutes apiece on a large one, and a
// box reading slowly holds its slot at objstore.MiBPace's floor — thirty
// seconds a mebibyte, a gigabyte in eight and a half hours — so with every
// slot a box's, a person's push or clone (receive-pack and both
// advertisements run in this lane too) waits out SlotWait and is answered
// busy. A box is generated code, so the lane cannot count on its manners: half
// is the bound the engine already holds seat git work to on the read lane
// (code.ShareOf), and the other half is what every person and machine shares.
// Persons and machines keep the whole lane when no box is waiting: the cap
// binds the run class alone. So the lane's INVARIANT is that no slot is free
// while a waiter that may take it waits — a box waiter at its class's cap may
// not, and is granted at the first release that leaves its class under it.

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

	// ShareRun is a coding box: the git a seat's detached coding run makes
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
// A JUDGMENT CALL, and a product one rather than a measurement — a person
// waits at a terminal, a machine credential stands for a pipeline nobody
// watches second by second, and a box is a seat's detached run that nobody
// watches at all: with every class waiting, persons take four sevenths of what
// the lane spends, machines two and boxes one
// (TestAContendedTransferLaneSharesByClass), and while people have spent less
// lately than twice what the machines have — as when one person clones
// beside sixty-four of one CI token's clones — a person's clone is granted
// the first slot released rather than the sixty-fifth: BenchmarkTransferShare
// measures its wait for a slot at 60 ms at the p99 there, one release's turn,
// against 1.46 s in arrival order (one count of twenty clones). Beside coding
// boxes too — the CI token's sixty-four and sixty-four boxes, half of them
// reading at the slowest pace a transfer is waited on (its runs arms, on a
// lane of two slots, one count of five hundred clones) — the person's clone
// took 83 ms at the p99 against 42 ms alone, with none of theirs refused and
// the boxes holding one slot of the two: within the twice its idle figure the
// search for the run's weight is held to, at 4:2:1, the first it tries (8:4:1
// and 16:8:1, which keep persons at twice the machines too, measured 86 and
// 97 ms), so the box's 1 stands. A RATIO, never
// a cap, for persons and machines: the share is work-conserving for them, so a
// machine alone takes the whole lane (its machine-alone arm runs as many
// clones a second as arrival order's) — a flat per-credential cap would
// throttle the CI system that checks out through one token on an idle node.
// The box class's cap is a bound of its own ([ShareClass.Cap]), never a
// weight. A constant of the class rather than an option, because two git
// nodes weighing the classes differently would make which node a balancer
// picked decide who waits.
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
// a coding box — code.ShareOf's arithmetic, the half the engine holds seat git
// work to on the read lane, spelled again here because neither package
// imports the other (internal/code/host's TestTheRunCapIsTheSeatShare holds
// the two equal). Why the box class alone is capped is the package doc's "The
// one class that is not work-conserving".
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

// maxSharePrincipal is the longest principal [Share.Valid] admits, in bytes.
//
// 255: just under four times the longest name an identity is given today —
// a seat handle and a Tier A token's login both stop at internal/iam's
// MaxLogin (64), a machine token's name is `pat:` and a uuid, and a coding
// run's launch id a uuid — so the bound refuses nothing a configuration or
// the identity directory admits, and leaves a grammar room to widen without
// this package following it.
const maxSharePrincipal = 255

// Valid reports a known class and a principal of 1..[maxSharePrincipal]
// bytes with no NUL.
//
// The principal keys the lane's book, and an empty one would schedule a run
// as nobody's. One past the bound, or holding a NUL, is no name any identity
// is given — it is a caller passing the wrong string, a body or a value read
// from the wrong field — so it is refused where the Cmd is prepared, failing
// the run that carried it loudly, rather than opened as an account the book
// then charges for a principal nobody is.
func (s Share) Valid() bool {
	return s.Class.Valid() && s.Principal != "" && len(s.Principal) <= maxSharePrincipal &&
		!strings.ContainsRune(s.Principal, 0)
}

// ShareWindow is how long a principal's spending counts against it: the time
// constant of the exponential decay every charge is read through.
//
// ONE MINUTE: past a CI checkout's burst of lazy fetches — seconds to tens of
// seconds — so the burst is charged to the credential that made it while it
// lasts, and gone within a few minutes (e^-3, 5 %, of a charge is left after
// three), so a person's clone this morning does not slow their fetch now.
// BenchmarkTransferShare's decay arms measure it (one count of twenty clones
// an arm, on its lane as wide as the cores, where a held slot is charged a
// core a second): a person whose own burst of sixty-four clones is a minute
// old waits 187 ms at the p99 behind a CI token's sixty-four, and three
// minutes old 67 ms, about as one who spent nothing does (60 ms).
const ShareWindow = time.Minute

// shareForget is the decayed charge, in core-seconds, below which an account
// that neither runs nor waits is forgotten: a millisecond of a core, what a
// whole core-minute decays to after about eleven windows. An account that
// small decides no grant — every fresh principal reads zero — so the book holds
// the principals of the last few minutes and nothing else.
const shareForget = 0.001

// occupancyRate is what a held transfer slot is charged a second, in
// core-seconds, on a pool of cores READ slots — the node's cores, as the pool
// counts them — and slots TRANSFER slots: the share of a core one slot stands
// for, cores over slots, and never more than one. New derives it where it
// sizes the lane; the book holds it for the lane's life.
//
// # A charge is a principal's dominant share, in core-seconds
//
// A run holds two things the lane runs short of: a SLOT, which is memory
// (TransferShare backs each), and the CPU its git spends. The charge is the
// greater of the two, each counted against what the lane has of it — so a
// slot-second is worth what one slot is of the cores the lane's transfers run
// on. Those are the node's cores, every slot sharing them: a quarter of a core
// a slot at the default four a core (TransfersPerProcess), where a lane of
// fast clients runs out of cores before slots, so a CPU-heavy storm pays up to
// four times what a client its link paces pays for the same slot-time. But
// never more than one core a slot, because a transfer's git runs on one
// thread (pack.threads=1 in the base; a receive-pack's lent threads are the
// READ lane's cores, and charged as the CPU they spend): a lane narrower than
// the cores can use no more cores than it has slots. On such a lane — 32 cores
// whose 8 GiB back 16 slots, or a git.max_transfers below the cores — a held
// slot is charged a whole core a second, which the CPU a transfer spends on
// its one thread does not pass, so two principals holding the same slots as
// long are charged alike however fast their clients read: slots are what that
// node runs out of. A lane an operator widened past four a core charges each
// slot less — a sixteenth at sixteen a core — since its slots are cheaper
// than its cores.
//
// NEVER A CONSTANT. A quarter of a core on every shape — the slot's share at
// the default — charges the 32-core node's slow credential, four slots held a
// minute by clients its links pace, 38 core-seconds (decayed) where a fast
// one holding the same four slots as long pays 218
// (TestSlotsHeldAlikeAreChargedAlikeWhereSlotsRunOut), so the slow one is
// granted ahead at every contended release though both held the same share
// of what that node is short of — the skew the occupancy charge exists to
// prevent; and on a lane widened to sixteen a core it reads slot-time at four
// times what a slot there stands for.
func occupancyRate(cores, slots int64) float64 {
	return min(float64(cores)/float64(slots), 1)
}

// windowSeconds is ShareWindow as the decay's arithmetic reads it.
var windowSeconds = ShareWindow.Seconds()

// standing is a decayed charge in core-seconds, as of at, and the runs
// accruing occupancy into it, each at the book's occupancy rate.
//
// EXACT AT EVERY INSTANT, whatever the decisions in between: a run accrues
// rate core-seconds a second continuously, so its contribution over dt is the
// decayed integral rate·W·(1−e^(−dt/W)) rather than rate·dt added at the next
// decision — added that way, a principal's reading would depend on how often
// others happened to arrive, and one holding every slot for ten quiet minutes
// would read ten minutes undecayed at once.
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
	// weight is each class's share of a contended lane: ShareClass.Weight,
	// which a benchmark's search replaces (BenchmarkTransferShare's runs
	// arms) and nothing else does, since two nodes weighing the classes
	// differently would let a balancer decide who waits.
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
// on that waiter's account — or answers nil, granting nothing, where every
// waiter is of a class at its cap: the slot is then the lane's, free, and the
// waiters keep waiting for a release that leaves their class under it.
//
// THE WAITER IS NEVER LEFT IDLE IN BETWEEN: its account holds no run and no
// other waiter at the instant it is taken out of its queue, and an account
// forgotten then — one that had spent nothing, every fresh principal — would
// start its run on an account the book no longer holds, so the principal's
// next arrival would open a fresh one at zero and every charge its runs made
// would go where no grant reads it: persons reading as having spent nothing
// for ever, and their class's standing reset to nearly nothing at every sweep
// (measured: persons took four grants to the machine's one where their weights
// give two).
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
