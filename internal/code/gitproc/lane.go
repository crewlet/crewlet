package gitproc

import (
	"container/list"
	"context"
	"slices"
	"strconv"
	"sync"

	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// MaxLentCores is how many of the READ lane's free cores a receive-pack may be
// lent, each one more thread for its index-pack.
//
// # Why a receive-pack, and why three
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
// transfer lane's memory bound ([TransfersPerProcess]) counts once.
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
// tail. A call site that knows when the index-pack has finished closes
// [Cmd.Indexed], and the loan is repaid then rather than when the process
// exits — the host closes it when the proc-receive hook first reaches its
// relay, which receive-pack starts only once its index-pack has exited, so
// the object-store upload of up to 1.25 GiB and the wait for the push's
// record hold no loan. The cores lent right now are the gauge
// crewlet.code.git.lent_cores, which is how pushes indexing on one core
// because the loans are parked in uploads is seen rather than guessed.
const MaxLentCores = 3

// loan is the cores one receive-pack was lent: repaid exactly once, when its
// call site says the index-pack is done ([Cmd.Indexed]) or when the run ends,
// whichever comes first.
type loan struct {
	p     *Pool
	cores int64
	once  sync.Once
}

// lend lends c the READ lane's free cores when it is a receive-pack, or
// returns nil: nothing lent and nothing to repay. The gauge is set under the
// pool's gauge mutex, as every change to the loans out is, so a later Set
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
// in the transfer lane.
func lendsCores(c Cmd) bool {
	return c.Class == ClassTransfer && c.Args[0] == "receive-pack"
}

// threadsBase is pack.threads' index in the base configuration.
var threadsBase = slices.IndexFunc(baseKeys, func(kv KV) bool { return kv.Key == keyThreads })

// threadsValue is the environment entry giving a process n threads.
func threadsValue(n int64) string {
	return configValue(threadsBase, strconv.FormatInt(n, 10))
}

// lane is one of the pool's admission queues: size slots, granted one at a
// time in arrival order.
//
// The pool's own rather than golang.org/x/sync/semaphore's, because the READ
// lane needs one number the semaphore does not expose — how many slots are free
// right now — and needs it EXACT: read through a second counter beside the
// semaphore, a slot handed straight from a release to a queued waiter reads as
// free for an instant, and a loan sized in that instant claims a core twice.
//
// INVARIANT: a waiter is queued only while every slot is held. release hands a
// slot straight to the oldest waiter rather than freeing it, and acquire
// queues only when none is free — so no slot is ever free while somebody
// waits, and a newcomer can never take a slot ahead of the queue.
type lane struct {
	class Class
	size  int64

	mu      sync.Mutex
	held    int64
	waiters list.List // of *waiter, oldest first
	lent    int64     // READ lane only: threads lent to receive-packs
}

// waiter is one queued acquire. granted is set under the lane's mutex before
// ready closes, so a waiter whose context ended can tell whether a slot
// reached it first.
type waiter struct {
	ready   chan struct{}
	granted bool
}

func newLane(class Class, size int64) *lane {
	return &lane{class: class, size: size}
}

// acquire takes a slot, waiting in arrival order until one is free or ctx
// ends. It returns ctx's error when ctx ended first, and holds nothing then.
func (l *lane) acquire(ctx context.Context) error {
	l.mu.Lock()
	if l.held < l.size && l.waiters.Len() == 0 {
		l.held++
		l.mu.Unlock()
		return nil
	}
	w := &waiter{ready: make(chan struct{})}
	elem := l.waiters.PushBack(w)
	l.mu.Unlock()

	select {
	case <-w.ready:
		return nil
	case <-ctx.Done():
		l.mu.Lock()
		if w.granted {
			// The slot reached this waiter after its context ended and
			// before the lock was taken: pass it on rather than keep a
			// slot nobody will use.
			l.mu.Unlock()
			l.release()
			return ctx.Err()
		}
		l.waiters.Remove(elem)
		l.mu.Unlock()
		return ctx.Err()
	}
}

// tryAcquire takes a free slot or reports that there is none, never waiting.
func (l *lane) tryAcquire() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held < l.size && l.waiters.Len() == 0 {
		l.held++
		return true
	}
	return false
}

// release returns a slot: straight to the oldest waiter if there is one, so
// the invariant holds, else to the lane.
func (l *lane) release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if front := l.waiters.Front(); front != nil {
		w, _ := l.waiters.Remove(front).(*waiter)
		w.granted = true
		close(w.ready)
		return
	}
	l.held--
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

// counts is the lane's held slots and outstanding loans, for the suite.
func (l *lane) counts() (held, lent int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.held, l.lent
}
