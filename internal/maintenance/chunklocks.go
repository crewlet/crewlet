package maintenance

import (
	"context"
	"time"
)

// ChunkLocks is the slice of the coordination store this job drives: the
// deletion of the bucket of chunk locks a build that kept files in
// content-addressed chunks opened. [coord.ObjectStores] is the contract and
// every fleet store satisfies it.
type ChunkLocks interface {
	// RetireChunkLocks deletes the bucket, reporting whether it was there.
	// Retiring what is not there is not an error.
	RetireChunkLocks(ctx context.Context) (bool, error)
}

// ChunkEra is whether any node that may still write a file in chunks is left
// in the fleet: the engine's census of the tracker log's readers, where a
// node reading below the record version that names a file's object is one
// that still writes chunks — and, writing over a chunk the store holds, takes
// that chunk's lock first.
type ChunkEra interface {
	// Over is true once no such node is left. An error is UNKNOWN, never
	// "not yet": see [Job.Gate].
	Over(ctx context.Context) (bool, error)
}

// RetiredChunkLockJobs is the one job that ends the chunk locks (ADR-0027): a
// coordination bucket no build that keeps one object per upload opens, and
// that a build keeping chunks locked each chunk in around a write of one the
// store already held.
//
// # Gated on the chunk era, because an older node still takes the locks
//
// A node of that build fails a write of a chunk the store holds when it
// cannot take the chunk's lock, so deleting the bucket under one fails its
// uploads mid-rollout. The gate is the CHUNK ERA ([ChunkEra]) rather than the
// lease protocol floor [RetiredBudgetJobs] reads, because what a node reads on
// the tracker's log is the fact in question — whether it writes files in
// chunks — while the move to one object per upload changed no lease's meaning
// and bumped no protocol. And a node of that build that comes back recreates
// the bucket at its boot, which the next tick after its departure deletes
// again. Until then the job has no work; afterwards every tick finds nothing
// to retire.
//
// A [Fleet] job: the bucket is the company's, and one node deleting it is the
// whole of the work. Nil on either seam contributes nothing: a node with no
// coordination store has no bucket, and one with no tracker log to count
// cannot say the era is over.
func RetiredChunkLockJobs(locks ChunkLocks, era ChunkEra) []Job {
	if locks == nil || era == nil {
		return nil
	}
	return []Job{{
		Name: "retired_chunk_locks", Scope: Fleet,
		Gate: era.Over,
		Run: func(ctx context.Context, _, _ time.Time) (int64, error) {
			retired, err := locks.RetireChunkLocks(ctx)
			if retired {
				return 1, err
			}
			return 0, err
		},
	}}
}
