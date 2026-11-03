package gitproc

import "fmt"

// THE NODE'S MEMORY, AND THE TRANSFER LANE SIZED BY IT.
//
// A transfer spends MEMORY for its whole life and a core only now and then
// (TransfersPerProcess), so the transfer lane is a memory bound — and a lane
// sized from the cores alone is sized from the wrong resource wherever the
// two part company: a container with a memory limit and no CPU limit sees
// every core of its host, and on a 64-core host with 16 GiB the default of
// four transfers a core admitted 256 transfers that could hold 56 GiB. So the
// pool reads the node's memory once, at New, and the lane's default is the
// lesser of four a core and ONE FOR EVERY TransferShare of that memory — the
// share CoreMemory gives each of a core's four transfers, so a node of
// CoreMemory a core or more is sized by its cores exactly as before, and one
// with less is sized as the cores its memory backs.
//
// WHAT THE NODE HAS is the least of its physical memory and every cgroup
// limit set on the process's cgroup or above it — v2's memory.max, and v1's
// memory.limit_in_bytes on a host still running the older hierarchy, where a
// container's limit is visible nowhere else (memory_linux.go). Darwin reads
// sysctl hw.memsize and has no cgroups. Where none of it can be read, New
// REFUSES, naming what it could not read: a lane guessed from nothing is the
// OOM this exists to prevent, on exactly the node nothing measured.
//
// The pool reads it once because its lanes are fixed for its life; what
// moves with the memory while a node runs — an in-place resize of a running
// container, which Kubernetes performs without a restart — is read again by
// whoever holds a budget of it ([Pool.ReadMemory]), and the lanes follow at
// the next restart.

// Memory is the memory this node's processes may hold, as it was read, and
// where from: the least of the machine's physical memory and every cgroup
// limit set above the process — the file or the call that bound it, which is
// what an operator reads to see why.
type Memory struct {
	Bytes int64
	From  string
}

// CoreMemory is the memory a git node is taken to have for each core of its
// git pool (the read lane's slots): the shape every memory figure of the
// node's git lanes is held inside.
//
// TWO GIBIBYTES, the compute-optimised instance shapes' — the least a vCPU is
// sold with (general-purpose shapes give four, memory-optimised eight) — so
// lanes sized to fit it fit every shape a git node runs on. A SHAPE, never a
// reading: what a node actually has is read at New, and a node with less than
// this a core runs only as many cores' worth of transfers as its memory backs
// (TransferShare).
const CoreMemory int64 = 2 << 30

// TransferShare is how much of a node's memory backs each transfer of the
// TRANSFER lane's default: CoreMemory over TransfersPerProcess, 512 MiB — a
// core's memory shared four ways, as a core's transfers are.
const TransferShare = CoreMemory / TransfersPerProcess

// TransferMemory is the most one transfer is taken to hold when nothing about
// its repository is known: what a git node keeps back of its memory for every
// transfer the lane admits.
//
// 224 MIB, A QUARTER OF 896: the delta window (pack.windowMemory, twice the
// blob limit: 100 MiB at the defaults) and the delta base cache
// (core.deltaBaseCacheLimit, git's default 96 MiB) a transfer may fill — a
// receive-pack's lent cores raise its index-pack's base cache by that limit a
// lent thread, and the loans out never reach the read lane's width, so four a
// core stay under 896 MiB — and the object list of a repository of about
// 120 000 objects at TransferBytesPerObject. A transfer of a larger
// repository holds more, by TransferBytesPerObject an object: that is no
// figure a lane can be sized by, since a lane does not know which repository
// each slot will serve, so whoever knows the repository charges it (the
// cache's background work keeps room for one beside each whole-repository
// step, beside the node's tip commit-graph over its tail and beside each step
// of a publication; a compaction's build keeps room for a transfer of the
// packs it merges alone, since a maintainer that refused a large repository's
// every merge would refuse its pushes).
//
// The window's term is pack.windowMemory, which caps the window's entries
// and not the process around them: a pack-objects whose delta search meets
// the largest blobs a push may carry holds the object it deltifies and its
// delta beside the window and the base cache. internal/code/compact's
// BenchmarkCompact measured that around a compaction's build — the same
// pack-objects over eight revisions of one 50 MiB blob, each searched against
// the one before: 269.7 MiB, 74 MiB past the 196 this figure gives the window
// and the base cache at the defaults (the cache's CompactWindowBytes charges
// that build six blobs). A transfer searches only what its packs hold whole —
// an object stored as a delta of one it also sends is sent as stored, and a
// compacted repository's packs carry the deltas their build found — so this
// stays the figure of a transfer of no particular repository, and one whose
// search meets blobs of max_blob_bytes held whole may hold about those
// 74 MiB more, as a transfer of millions of objects holds more by its objects.
const TransferMemory int64 = 224 << 20

// TransferBytesPerObject is the memory one transfer of a whole repository —
// a clone's pack-objects — holds for every object the repository's packs
// hold, past the part TransferMemory covers whatever the repository.
//
// 240 BYTES, MEASURED (internal/code/cache's BenchmarkMidx, its
// memory/step=clone-walk arm: git 2.43, the peak anonymous memory of the
// pack-objects, sampled every millisecond, over a source repository's
// composition): a clone of a repository with no bitmap held 670 MiB over
// 2.97 million objects and 132 MiB over 296 000 — about 200 bytes an object
// over a part of some 75 MiB that does not grow — rounded up a fifth. A clone
// of a repository WITH the maintainer's bitmap holds less (330 MiB and
// 36 MiB: it counts what it sends from the bitmap rather than walking it),
// but the walk is the figure, since it is what a repository whose bitmap a
// node cannot hold is served with, and those are the largest repositories a
// node serves. The packs' own pages a process maps in count toward its
// resident size and are not here: the kernel takes those back under pressure
// rather than killing anybody.
const TransferBytesPerObject = 240

// defaultTransfers is the TRANSFER lane's size when Options.MaxTransfers is 0
// on a pool of cores read slots over a node of memory bytes:
// TransfersPerProcess a core, and no more than one for every TransferShare of
// the memory — at least one, since a node with no transfer serves no clone.
func defaultTransfers(cores, memory int64) int64 {
	return min(TransfersPerProcess*cores, max(1, memory/TransferShare))
}

// resolveMemory is what New sizes the lanes by: o's figure when it gives one,
// else the node's own, read now by read (readNodeMemory but in the suite).
func resolveMemory(o Options, read func() (Memory, error)) (Memory, error) {
	switch {
	case o.Memory < 0:
		return Memory{}, fmt.Errorf("gitproc: Memory %d is negative: 0 reads the node's own", o.Memory)
	case o.Memory > 0:
		return Memory{Bytes: o.Memory, From: "gitproc.Options.Memory"}, nil
	}
	m, err := read()
	if err != nil {
		return Memory{}, fmt.Errorf("gitproc: the node's memory could not be read, and the transfer lane "+
			"is sized by it (a transfer holds up to %d bytes, and %d an object of its repository): %w",
			TransferMemory, TransferBytesPerObject, err)
	}
	return m, nil
}

// Memory is the node's memory as New read it — or as Options.Memory gave
// it — and what the pool's lanes were sized by.
func (p *Pool) Memory() Memory { return p.memory }

// ReadMemory is the node's memory NOW: read again from the node where New read
// it, and the figure Options.Memory gave where it gave one, which a pool
// holds for its life. A holder of a budget of the memory — the cache's
// maintenance — reads it again rather than keeping New's, so a container
// resized in place is held to its new limit before the next restart resizes
// the lanes.
func (p *Pool) ReadMemory() (Memory, error) {
	if p.fixedMemory {
		return p.memory, nil
	}
	return readNodeMemory()
}
