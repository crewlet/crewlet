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
// may be lent, as threads for the index-pack a person waits on: on four vCPUs
// (git 2.43) a 37 MB pack indexes in 2.8 s on one thread, 1.9 s on three and
// 1.8 s on four. Never upload-pack, whose pack.windowMemory is per thread, nor
// a push's --advertise-refs, which runs no index-pack. A loan is the free cores
// less one for the push's own thread, holds no read slot, may add up to 96 MiB
// of delta base cache a thread ([TransfersPerProcess] says so), and is repaid
// at [Cmd.Indexed]: a receive-pack without one is refused, since it would park
// every loan through the push's upload. Gauge: crewlet.code.git.lent_cores
// (package doc, "Lending a push cores").
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
// or returns nil. Every change to the loans out is made under gaugeMu, so a
// later Set never carries an older count.
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

// lendsCores reports whether c is a transfer-lane receive-pack that RECEIVES
// a pack, never a push's advertisement. It is exact because prepare holds a
// receive-pack's options to the spellings it reads (receivePackOptions).
func lendsCores(c Cmd) bool {
	return c.Class == ClassTransfer && c.Args[0] == "receive-pack" && !advertises(c.Args)
}

// threadsBase is pack.threads' index in the base configuration.
var threadsBase = slices.IndexFunc(baseKeys, func(kv KV) bool { return kv.Key == keyThreads })

// threadsValue is the environment entry giving a process n threads.
func threadsValue(n int64) string {
	return configValue(threadsBase, strconv.FormatInt(n, 10))
}

// lane is one of the pool's admission queues: size slots, each handed to a
// waiter rather than freed while anybody waits. Not x/sync/semaphore: the READ
// lane needs the free slots EXACTLY (a counter beside a semaphore reads a slot
// handed to a waiter as free for an instant, and a loan sized then claims a
// core twice), and the TRANSFER lane picks which waiter gets a slot (share.go).
//
// INVARIANT: a waiter is queued only while every slot is held or, in the
// TRANSFER lane, its class is at its cap ([ShareClass.Cap]), so no slot is
// free while a waiter that may take it waits, and no newcomer passes one.
// release hands the slot to the oldest waiter, or in the TRANSFER lane to the
// one the share book names among those that may take it, freeing it when none
// may.
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

// waiter is one queued acquire. granted (and the transfer lane's run) is set
// under the lane's mutex before ready closes, so a waiter whose context ended
// can tell whether a slot reached it first.
type waiter struct {
	ready   chan struct{}
	granted bool
	elem    *list.Element // where it is queued: the lane's list, or its account's

	// The transfer lane's: its account, arrival order and granted run.
	acct *account
	seq  uint64
	run  *run
}

func newLane(class Class, size int64) *lane {
	return &lane{class: class, size: size}
}

// newShareLane is a lane of size slots granted by book (share.go).
func newShareLane(size int64, book *shareBook) *lane {
	return &lane{class: ClassTransfer, size: size, book: book}
}

// acquire takes a slot for s's run, waiting until one is free or ctx ends,
// when it returns ctx's error and holds nothing. The run is what a share
// lane's release settles; nil in the other lanes.
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
			// Granted after ctx ended: pass the slot on.
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
// mayTake allows, else a waiter queued for one.
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

// mayTake reports whether an arrival of s may take a free slot, under l.mu:
// in an arrival-order lane only while nobody waits, and in the transfer lane
// while s's class is under its cap (by the invariant, no waiter may take a
// free slot there).
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

// tryAcquire takes a free slot or reports none, never waiting: the
// maintenance lane's admission.
func (l *lane) tryAcquire() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held < l.size && l.waiting == 0 {
		l.held++
		return true
	}
	return false
}

// release returns a slot to the next waiter that may take it, else to the
// lane. A share lane first settles r with the CPU its tree spent, so a box's
// release puts its own class's waiters among those that may take the slot.
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
		// Every waiter's class is at its cap: the slot goes free.
		l.held--
		return
	}
	l.waiting--
	w.granted = true
	close(w.ready)
}

// lend lends up to n of the cores neither held nor lent, LESS ONE for the
// borrower's own thread ([MaxLentCores]), and reports how many it lent and the
// loans now out. A queued waiter means every slot is held (the invariant).
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

// free is the slots neither held nor lent, and none while a run waits: such a
// slot is being handed to a waiter (the invariant), or in the transfer lane is
// one only some classes may take, and this reading is told no class.
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
