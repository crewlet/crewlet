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
// IT EXISTS FOR ONE CALLER whose work before its process is the expensive
// part: an operator's verify of a repository (internal/code/cache), which
// reads and hashes every byte of every pack before its connectivity check.
// Admitted only at the check, a busy node answered "run it again" after
// reading up to the largest repository the defaults admit — twenty gibibytes —
// and the retry read them all again for as long as a bitmap held the lane;
// and nothing bounded how many such reads ran at once. Held from the start,
// a refusal costs nothing, and the lane bounds them.
//
// THE MAINTENANCE LANE ALONE, because it is the one lane that never waits: a
// reservation of a lane that queues would be a slot a waiting request queues
// behind while its holder reads files no git process is reading.
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
	return p.maint.release, nil
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
	return r.p.run(ctx, c, prep, nil)
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
