package gitproc

import (
	"container/list"
	"context"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// MaxLentCores is how many of the READ lane's free cores a push's receive-pack
// may be lent, each one more thread for its index-pack.
//
// # Why a push's receive-pack, and why three
//
// Every git process runs at pack.threads=1, because a READ or MAINTENANCE slot
// IS one core and a transfer shares one. The exception is the one phase of a
// transfer that both threads and has the engine waiting on it: receive-pack's
// index-pack resolving the deltas of the pack a client just pushed. It runs on
// one node per push, a person waits for it, and on an idle node the cores it
// could use sit idle. Measured with git 2.43 on four vCPUs: a 97 MB pack
// indexes in 12.1 s on one thread and 6.4 s on four; a 37 MB pack of this
// repository's own history in 2.8 s on one, 2.2 s on two, 1.9 s on three and
// 1.8 s on four. Three more cores is where the curve has flattened — the
// receiving phase before resolution is single-threaded whatever the setting.
//
// Not upload-pack: pack-objects holds pack.windowMemory PER THREAD, which the
// transfer lane's memory bound ([TransfersPerProcess]) counts once. Nor a
// push's ADVERTISEMENT, the receive-pack --advertise-refs every push's GET is
// answered with before its POST: it writes the refs and exits, with no pack,
// no index-pack and no hook, so a loan would buy it nothing — and would hold
// up to three cores for as long as its client takes to read the refs, so a
// push whose POST starts meanwhile would be lent less, or nothing, and index
// on one core. Which of the two a receive-pack is, is read from its options
// (cmd.go's receivePackOptions), held to the spellings the pool reads exactly.
//
// # What a lent thread costs in memory
//
// Resident, typically little: index-pack held 12 MiB at one thread and 23 MiB
// at four on the 37 MB pack, 25 MiB and 48 MiB on the 97 MB one (git 2.43).
// That is the typical case and NOT the bound: index-pack caps its delta base
// cache at core.deltaBaseCacheLimit TIMES its threads (git's
// base_cache_limit = delta_base_cache_limit × nr_threads), so on a push of
// large, delta-heavy blobs every lent thread may hold up to 96 MiB (git's
// default limit) more than the one-thread transfer TransfersPerProcess
// counts. The loans out never reach the read lane's width (below), so a node
// holds less than 96 MiB more a core than TransfersPerProcess's worst case
// — about 900 MiB a core rather than 800 — and that doc says so.
//
// # Sized by the free cores, less the push's own
//
// A loan is sized by the READ lane's FREE cores at the moment the receive-pack
// starts — those neither a held read nor an earlier loan claims — LESS ONE:
// the receive-pack's own thread resolves deltas beside the lent ones and
// needs a core too, so lent every free core a push would run one thread more
// than there are cores free, raising the cache cap above for no speed at all.
// An idle four-core node lends three (four threads), an idle two-core node
// one, a one-core node none, and a loaded node nothing: a push there runs on
// one core, exactly as before.
//
// # Lent, never held
//
// The loan does not HOLD the read slots it was sized by. A read is admitted
// exactly as if no loan existed, and what a loan holds is only its COUNT,
// which later loans are sized against: no loan is ever made against a core a
// held read or another loan already claims. Held, the slots would sit idle
// through everything a push does other than resolve deltas — its client's
// upload before (hours, at the transfer floor) and the engine's hook after —
// so a few slow pushes would narrow the read lane to nothing while the CPU
// idled, the failure the transfer lane exists to prevent, and a push's own
// hook READS would queue for cores the push itself held idle. A read that
// arrives after a loan and takes its core shares that core with the loan's
// threads for as long as both run — the scheduler shares it, as it shares a
// CPU-bound transfer — so the overcommit a loan can cause is bounded by the
// loans out, never by the reads, which keep every slot they always had.
//
// # Reserved from start to repayment — repaid once the index-pack is done
//
// The COUNT is the trade-off a loan makes. pack.threads is fixed when the
// process starts, before a byte of the pack has arrived, so a loan is made at
// the start and its count covers the client's whole upload: while a slow push
// uploads, a later push on that node is lent less. What CAN end early is the
// tail. The call site of every push's receive-pack closes [Cmd.Indexed] once
// the index-pack has finished, and the loan is repaid then rather than when the
// process exits — the host closes it when the proc-receive hook first reaches
// its relay, which receive-pack starts only once its index-pack has exited, so
// the object-store upload of up to 1.25 GiB and the wait for the push's record
// hold no loan. A push's receive-pack without one is REFUSED rather than lent
// to its exit, on every node, one with no core to lend included: a call site
// that forgot it would park every push's loan in its upload and record wait, so
// every later push on the node would index on one core — and that call site's
// own tests, run on a node with nothing to lend or no second push behind the
// first, would still pass. The cores lent right now are the gauge
// crewlet.code.git.lent_cores, which is how pushes indexing on one core because
// the loans are parked in slow clients' uploads is seen rather than guessed.
const MaxLentCores = 3

// loan is the cores one receive-pack was lent: repaid exactly once, when its
// call site says the index-pack is done ([Cmd.Indexed]) or when the run ends,
// whichever comes first.
type loan struct {
	p     *Pool
	cores int64
	once  sync.Once
}

// lend lends c the READ lane's free cores when it is a push's receive-pack,
// or returns nil: nothing lent and nothing to repay. The gauge is set under
// the pool's gauge mutex, as every change to the loans out is, so a later Set
// never carries an older count.
func (p *Pool) lend(c Cmd) *loan {
	if !lendsCores(c) {
		return nil
	}
	p.gaugeMu.Lock()
	defer p.gaugeMu.Unlock()
	k, out := p.read.lend(MaxLentCores)
	if k == 0 {
		return nil
	}
	p.recordLent(out)
	return &loan{p: p, cores: k}
}

// repay returns the loan's cores to the lane; every call after the first does
// nothing.
func (l *loan) repay() {
	l.once.Do(func() {
		l.p.gaugeMu.Lock()
		defer l.p.gaugeMu.Unlock()
		l.p.recordLent(l.p.read.repay(l.cores))
	})
}

// recordLent sets the lent-cores gauge to out; the caller holds gaugeMu.
func (p *Pool) recordLent(out int64) {
	if p.metrics != nil {
		p.metrics.Set(metrics.CodeGitLentCores, float64(out), nil)
	}
}

// lendsCores reports whether c is a run the READ lane lends to: a receive-pack
// in the transfer lane that RECEIVES a pack — never a push's advertisement,
// which writes the refs and exits with no index-pack to thread. prepare holds
// a receive-pack's options to the spellings it reads first
// (receivePackOptions), so which of the two c is, is exact.
func lendsCores(c Cmd) bool {
	return c.Class == ClassTransfer && c.Args[0] == "receive-pack" && !advertises(c.Args)
}

// threadsBase is pack.threads' index in the base configuration.
var threadsBase = slices.IndexFunc(baseKeys, func(kv KV) bool { return kv.Key == keyThreads })

// threadsValue is the environment entry giving a process n threads.
func threadsValue(n int64) string {
	return configValue(threadsBase, strconv.FormatInt(n, 10))
}

// lane is one of the pool's admission queues: size slots, each released to a
// waiter rather than freed while anybody waits.
//
// The pool's own rather than golang.org/x/sync/semaphore's, because the READ
// lane needs one number the semaphore does not expose — how many slots are free
// right now — and needs it EXACT: read through a second counter beside the
// semaphore, a slot handed straight from a release to a queued waiter reads as
// free for an instant, and a loan sized in that instant claims a core twice.
// And because the TRANSFER lane chooses WHICH waiter a released slot goes to
// (share.go), which no semaphore lets a caller decide.
//
// INVARIANT: a waiter is queued only while every slot is held — or, in the
// TRANSFER lane, while its class holds as many slots as its cap allows
// ([ShareClass.Cap]: a coding box's class holds at most half the lane), so
// no slot is ever free while a waiter that MAY take it waits. release hands a
// slot straight to such a waiter rather than freeing it, and an arrival
// queues only when no slot is free or its class is at its cap — so a newcomer
// can never take a slot ahead of a waiter that could have taken it. The READ
// and MAINTENANCE lanes hand it to the oldest waiter; the TRANSFER lane to the
// waiter its share book names among those that may take it, and frees it
// where none may: a box waiter at its class's cap is granted at the first
// release that leaves its class under it.
type lane struct {
	class Class
	size  int64

	mu      sync.Mutex
	held    int64
	waiters list.List  // an arrival-order lane's waiters, oldest first
	waiting int64      // waiters queued, in either kind of lane
	lent    int64      // READ lane only: threads lent to receive-packs
	book    *shareBook // TRANSFER lane only: who has spent what; nil elsewhere
}

// waiter is one queued acquire. granted — and, in the transfer lane, the run
// the grant started — is set under the lane's mutex before ready closes, so a
// waiter whose context ended can tell whether a slot reached it first.
type waiter struct {
	ready   chan struct{}
	granted bool
	elem    *list.Element // where it is queued: the lane's list, or its account's

	// The transfer lane's: whose turn it waits for, its place in arrival
	// order, and the run its grant started.
	acct *account
	seq  uint64
	run  *run
}

func newLane(class Class, size int64) *lane {
	return &lane{class: class, size: size}
}

// newShareLane is a lane of size slots granted by book (share.go), which caps
// each class of a lane of that width.
func newShareLane(size int64, book *shareBook) *lane {
	return &lane{class: ClassTransfer, size: size, book: book}
}

// acquire takes a slot for s's run, waiting until one is free or ctx ends:
// in arrival order, or in a share lane in the order its book decides. It
// returns ctx's error when ctx ended first, and holds nothing then. The run is
// the share lane's — what release settles — and nil in the other lanes.
func (l *lane) acquire(ctx context.Context, s Share) (*run, error) {
	l.mu.Lock()
	r, w := l.arrive(s)
	l.mu.Unlock()
	if w == nil {
		return r, nil
	}

	select {
	case <-w.ready:
		return w.run, nil
	case <-ctx.Done():
		l.mu.Lock()
		if w.granted {
			// The slot reached this waiter after its context ended and
			// before the lock was taken: pass it on rather than keep a
			// slot nobody will use.
			l.mu.Unlock()
			l.release(w.run, 0)
			return nil, ctx.Err()
		}
		l.leave(w)
		l.mu.Unlock()
		return nil, ctx.Err()
	}
}

// arrive is one acquire's arrival, under l.mu: a free slot at once when
// nobody waits — whatever the arrival's share, so a lane never holds a slot
// free while it could be used — or a waiter queued for one. In the transfer
// lane a free slot is taken whoever waits, by the invariant (a free slot
// there means every waiter's class is at its cap), unless the arrival's own
// class is at its cap, when it queues behind its class.
func (l *lane) arrive(s Share) (*run, *waiter) {
	var now time.Time
	if l.book != nil {
		now = l.book.now()
		l.book.sweep(now)
		defer l.book.report(now)
	}
	if l.held < l.size && l.mayTake(s) {
		l.held++
		if l.book == nil {
			return nil, nil
		}
		return l.book.start(l.book.account(s, now), now), nil
	}
	w := &waiter{ready: make(chan struct{})}
	l.waiting++
	if l.book == nil {
		w.elem = l.waiters.PushBack(w)
	} else {
		l.book.enqueue(w, s, now)
	}
	return nil, w
}

// mayTake reports whether an arrival of s may take a slot that is free, under
// l.mu: in an arrival-order lane only while nobody waits — whoever waits came
// first — and in the transfer lane while s's class is under its cap, since
// there a free slot is one no waiter may take (the invariant).
func (l *lane) mayTake(s Share) bool {
	if l.book == nil {
		return l.waiting == 0
	}
	return l.book.mayStart(s.Class)
}

// leave takes a waiter that was never granted out of its queue, under l.mu.
func (l *lane) leave(w *waiter) {
	l.waiting--
	if l.book == nil {
		l.waiters.Remove(w.elem)
		return
	}
	now := l.book.now()
	l.book.leave(w, now)
	l.book.report(now)
}

// tryAcquire takes a free slot or reports that there is none, never waiting.
// An arrival-order lane's: the maintenance lane's admission.
func (l *lane) tryAcquire() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held < l.size && l.waiting == 0 {
		l.held++
		return true
	}
	return false
}

// release returns a slot — and, in a share lane, settles r given the CPU its
// process tree spent: straight to the next waiter that may take it if there
// is one, so the invariant holds, else to the lane. The settle comes first, so
// a box's release leaves its own class under its cap and its own class's
// waiters among those that may take the slot.
func (l *lane) release(r *run, cpu time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var now time.Time
	if l.book != nil {
		now = l.book.now()
		l.book.settle(r, cpu, now)
		defer l.book.report(now)
	}
	if l.waiting == 0 {
		l.held--
		return
	}
	var w *waiter
	if l.book == nil {
		w, _ = l.waiters.Remove(l.waiters.Front()).(*waiter)
	} else if w = l.book.grant(now); w == nil {
		// EVERY WAITER IS A CLASS AT ITS CAP: the slot is the lane's,
		// free, for whoever arrives that may take it.
		l.held--
		return
	}
	l.waiting--
	w.granted = true
	close(w.ready)
}

// lend lends up to n of the lane's cores that neither a held slot nor an
// earlier loan is using, LESS ONE for the borrower's own thread
// ([MaxLentCores]), and reports how many it lent and the loans now out. A
// queued waiter means every slot is held (the invariant), so it lends nothing
// then without asking.
func (l *lane) lend(n int64) (lent, out int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := min(n, l.size-l.held-l.lent-1)
	if k <= 0 {
		return 0, l.lent
	}
	l.lent += k
	return k, l.lent
}

// repay returns a loan of k cores, and reports the loans still out.
func (l *lane) repay(k int64) (out int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lent -= k
	return l.lent
}

// free is the slots neither held nor lent, and none while a run waits: a
// waiter is queued only while every slot is held (the invariant), so a slot
// that reads free then is one a release is handing straight to it. In the
// transfer lane a slot can be free while coding boxes at their class's cap
// wait, and reads none all the same: whether a run could take it turns on the
// run's class, which this reading is not told.
func (l *lane) free() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.waiting > 0 {
		return 0
	}
	return max(l.size-l.held-l.lent, 0)
}

// counts is the lane's held slots and outstanding loans, for the suite.
func (l *lane) counts() (held, lent int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.held, l.lent
}
