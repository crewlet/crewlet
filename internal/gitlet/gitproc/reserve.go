package gitproc

import (
	"context"
	"fmt"
	"sync"

	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// Reserved is one MAINTENANCE-lane slot held across the work that decides what
// a process will check: [Pool.Reserve] takes it (a free slot or [ErrBusy],
// never a wait) before that work, every process the work runs ([Reserved.Run])
// runs in it, and [Reserved.Release] frees it. Admitted only at the process,
// the work would be wasted or cut off: a verify (internal/gitlet/cache) would
// hash every pack, up to the defaults' 20 GiB repository, before a refusal,
// unbounded in number; a compaction's rebuild at a smaller cut
// (internal/gitlet/compact, admitted by the cache's AdmitCompaction) could lose
// the slot and memory charge its first build held; the tip commit-graph write
// could drop the old tip from the chain and write no new one. Maintenance alone, the lane that never waits: a held
// slot of a queueing lane is one requests queue behind while no git runs.
type Reserved struct {
	p       *Pool
	release func()

	// mu holds one process at a time in the slot and guards released.
	mu       sync.Mutex
	released bool
}

// Reserve holds a free maintenance-lane slot until the answer's Release, or
// refuses [ErrBusy] at once, in the words a refused maintenance [Pool.Run] gets.
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

// Run runs one maintenance process in the reserved slot exactly as an admitted
// [Pool.Run] would, and is the reservation's only way to a process: c's class
// must be [ClassMaintenance], and a released reservation runs nothing. A second
// Run waits for the first, since the slot is one process's. Its slot wait is
// recorded as zero, one observation per process like every run of the lane.
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

// Version is the git the slot runs, the pool's ([Pool.Version]): a
// publication's layer names it as the release that wrote it
// (internal/gitlet/compact), so the layer is held to the git that ran its
// build, never a second reading of the binary.
func (r *Reserved) Version() Version { return r.p.version }

// Release gives the slot back once, after a process running in it ends; a
// later call does nothing.
func (r *Reserved) Release() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.released {
		return
	}
	r.released = true
	r.release()
}

// HoldRead takes one READ-lane slot for in-process work that is a read's (the
// cache's hash of a pack a hint names, internal/gitlet/cache), admitted as a
// read's run is: in arrival order, [ErrBusy] past [SlotWait], or the caller's
// end; counted in the lane's running gauge while held. release frees it, once
// however often called. It runs nothing.
//
// The read lane, never a semaphore of its own: the lane counts a core a slot
// for work requests wait on, as hashing is, and a second count would let N hashes and N reads share N cores. The caller
// holds it only while the work runs, never across another wait: nothing waits
// for a slot of a lane it holds (admit), or queued reads wait behind a slot
// running nothing, as with [Reserved].
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
