package gitproc

import "fmt"

// The transfer lane is bounded by memory, not cores (package doc, "Slots:
// three lanes, each sized by what it spends"): a container with a memory limit
// and no CPU limit sees every core of its host, so a lane sized by cores alone
// admits transfers its memory cannot hold (64 cores and 16 GiB: 256 transfers
// that could hold 56 GiB).
//
// The node's memory is the least of its physical memory and every cgroup
// limit above the process (memory_linux.go). Where it cannot be read New
// refuses: a lane guessed from nothing is the OOM this prevents. The pool reads
// it once, its lanes being fixed for its life; a budget holder rereads it
// ([Pool.ReadMemory]) to follow a container resized in place.

// Memory is the memory this node's processes may hold, and From the file or
// call that bound it, which an operator reads to see why.
type Memory struct {
	Bytes int64
	From  string
}

// CoreMemory is the memory a Gitlet node is taken to have for each core of its
// git pool (the read lane's slots): the shape every memory figure of the
// node's git lanes is held inside.
//
// 2 GiB, the least a vCPU is sold with (compute-optimised shapes), so lanes
// sized to fit it fit every shape a Gitlet node runs on. A shape, not a
// reading: a node with less a core runs only the transfers its memory backs
// (TransferShare).
const CoreMemory int64 = 2 << 30

// TransferShare is how much of a node's memory backs each transfer of the
// transfer lane's default: a core's CoreMemory shared among its
// TransfersPerProcess transfers, 512 MiB.
const TransferShare = CoreMemory / TransfersPerProcess

// TransferMemory is the most one transfer is taken to hold when nothing about
// its repository is known: what a node keeps back for each transfer admitted.
//
// 224 MiB, so four a core stay under 896 MiB: the delta window
// (pack.windowMemory, twice the blob limit, 100 MiB at the defaults), the
// base cache (core.deltaBaseCacheLimit, 96 MiB, again per thread lent to a
// receive-pack, and loans never reach the read lane's width) and the object
// list of ~120 000 objects at TransferBytesPerObject. A lane cannot know a
// larger repository, so whoever does charges its objects. pack.windowMemory
// caps only the window's entries: a search over whole max_blob_bytes blobs
// holds ~74 MiB more (compact's BenchmarkCompact; CompactWindowBytes), but a
// transfer sends its packs' deltas as stored, so this figure stands.
const TransferMemory int64 = 224 << 20

// TransferBytesPerObject is the memory one transfer of a whole repository (a
// clone's pack-objects) holds for every object in the repository's packs,
// past what TransferMemory covers.
//
// 240 bytes, measured (internal/gitlet/cache's BenchmarkMidx,
// memory/step=clone-walk, git 2.43's peak anonymous memory): a clone walking
// a repository with no bitmap held 670 MiB over 2.97 million objects and
// 132 MiB over 296 000, about 200 bytes an object over a fixed ~75 MiB,
// rounded up a fifth.
// A bitmap needs less, but the walk is the figure: the largest repositories
// are those whose bitmap a node cannot hold. Mapped pack pages are not
// counted: the kernel reclaims them under pressure rather than killing anybody.
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

// ReadMemory is the node's memory now: read again where New read it, or
// Options.Memory's figure, which a pool holds for its life. A budget holder
// (the cache's maintenance) rereads it rather than keeping New's, so a
// container resized in place is held to its new limit before a restart
// resizes the lanes.
func (p *Pool) ReadMemory() (Memory, error) {
	if p.fixedMemory {
		return p.memory, nil
	}
	return readNodeMemory()
}
