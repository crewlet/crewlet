package gitproc

import (
	"context"
	"fmt"
	"sync"

	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// Reserved is one slot of the MAINTENANCE lane held across the work that
// decides what a process will check, rather than taken only as the process
// starts: [Pool.Reserve] takes it — a free slot or [ErrBusy], never a wait,
// the lane's own rule — before that work, so a node with no slot free refuses
// the caller before it has read anything, and every process the work then
// runs ([Reserved.Run]) runs in the slot it holds rather than asking the lane
// again, until [Reserved.Release].
//
// IT EXISTS FOR THREE CALLERS whose work around their processes must not be
// done for, or cut off by, a process the lane then refuses. An operator's
// verify of a repository (internal/code/cache) reads and hashes every byte of
// every pack before its connectivity check: admitted only at the check, a
// busy node answered "run it again" after reading up to the largest
// repository the defaults admit — twenty gibibytes — and the retry read them
// all again for as long as a bitmap held the lane; and nothing bounded how
// many such reads ran at once. Held from the start, a refusal costs nothing,
// and the lane bounds them. A compaction's build (internal/code/compact,
// admitted by the cache's AdmitCompaction) runs one pack-objects and, where
// an output passes the bundle rule, ONE more at a smaller cut: both in the
// slot it was admitted to, so the rebuild is never refused a lane its own
// first build was holding, and the node's memory charge it was admitted at
// covers both. And the cache's write of a node's tip commit-graph rewrites
// the copy's commit-graph-chain without the old tip before git writes the new
// one over it: admitted only at the write, a busy lane would leave a copy
// whose old tip is off its chain and no new one written.
//
// THE MAINTENANCE LANE ALONE, because it is the one lane that never waits: a
// reservation of a lane that queues would be a slot a waiting request queues
// behind while its holder reads files no git process is reading. (A READ slot
// is held only by work that IS a read's — [Pool.HoldRead] — and only while
// that work runs.)
type Reserved struct {
	p       *Pool
	release func()

	// mu holds one process at a time in the slot — the slot is one
	// process's — and guards released.
	mu       sync.Mutex
	released bool
}

// Reserve takes a free slot of the maintenance lane and holds it until the
// answer's Release, or refuses [ErrBusy] at once when none is free — with the
// same sentence a maintenance [Pool.Run] the lane refuses is told.
func (p *Pool) Reserve() (*Reserved, error) {
	release, err := p.tryMaintenance()
	if err != nil {
		return nil, err
	}
	return &Reserved{p: p, release: release}, nil
}

// tryMaintenance takes a free slot of the maintenance lane, or refuses
// [ErrBusy] naming the lane's size: the one admission [Pool.Run] and
// [Pool.Reserve] share, so a refusal reads the same from either.
func (p *Pool) tryMaintenance() (func(), error) {
	if !p.maint.tryAcquire() {
		return nil, fmt.Errorf("%w: this node runs at most %d maintenance processes at once (half its %d process slots)",
			ErrBusy, p.maint.size, p.read.size)
	}
	return func() { p.maint.release(nil, 0) }, nil
}

// Run runs one maintenance process in the reserved slot, exactly as
// [Pool.Run] would once admitted — its checks, its group, its deadline and
// its end — and is the reservation's only way to a process: c's class must be
// [ClassMaintenance], and a released reservation runs nothing. One process at
// a time: a second Run waits for the first, since the slot is one process's.
//
// Its wait for a slot is none — the reservation's — and is recorded so, one
// observation a process like every run of the lane.
func (r *Reserved) Run(ctx context.Context, c Cmd) (Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.released:
		return Result{}, fmt.Errorf("%w: a released reservation runs nothing", ErrInvalid)
	case c.Class != ClassMaintenance:
		return Result{}, fmt.Errorf("%w: a reservation holds a maintenance slot, and class %q runs in another lane",
			ErrInvalid, c.Class)
	}
	prep, err := r.p.prepare(c)
	if err != nil {
		return Result{}, err
	}
	if ctx.Err() != nil {
		return Result{}, fmt.Errorf("gitproc: git was not started: its caller had already gone: %w", context.Cause(ctx))
	}
	if r.p.metrics != nil {
		r.p.metrics.Observe(metrics.CodeGitWait, 0, metrics.Attrs{"class": string(c.Class)})
	}
	res, err := r.p.run(ctx, c, prep, nil)
	r.p.report(c, res)
	return res, err
}

// Release gives the slot back, once — a later call does nothing — after
// waiting for a process running in it to end.
func (r *Reserved) Release() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.released {
		return
	}
	r.released = true
	r.release()
}

// HoldRead takes one slot of the READ lane for in-process work that is a
// read's — the cache's hash of a pack a hint names (internal/code/cache) —
// exactly as a read's run is admitted: in arrival order, waiting at most
// [SlotWait] and [ErrBusy] past it, or its caller's own end; counted in the
// lane's running gauge while it is held. release gives it back, once however
// often it is called. It runs nothing.
//
// A SLOT OF THE READ LANE, because that lane counts a core a slot for the
// work requests wait on, and hashing is a core's work a request waits on —
// never a semaphore of its own beside the lanes, which would count the same
// cores twice and let N hashes and N reads run on N cores at once. HELD ONLY
// WHILE THE WORK RUNS, a piece at a time by its caller, and never across a
// wait for anything else: the lanes' rule (admit) is that nothing waits for a
// slot of a lane it holds, and a holder that waited would be a slot every
// queued read waits behind while nothing runs — Reserve's reason for
// reserving only the maintenance lane.
func (p *Pool) HoldRead(ctx context.Context) (release func(), err error) {
	free, _, err := p.admit(ctx, ClassRead, Share{})
	if err != nil {
		return nil, err
	}
	p.track(ClassRead, +1)
	return sync.OnceFunc(func() {
		p.track(ClassRead, -1)
		free(nil)
	}), nil
}
