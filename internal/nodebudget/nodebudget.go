// Package nodebudget computes the lines of a node's memory budget that the
// engine keeps beside Gitlet's transfers, serving share and background work:
// the Go runtime's share for the executor's pools, the store's page cache and
// Gitlet's read lane. Gitlet's reserve (internal/gitlet/gitproc's
// Options.Reserve) and the Go runtime's memory limit (internal/engine's
// limitMemory) read the same [Lines], so the two cannot disagree on a node's
// memory, and a short node is told so once (internal/gitlet/cache's
// code_cache_memory_short).
//
// # The Go runtime's share and the limit
//
// The limit is nine tenths of what the node holds outside the runtime leaves:
// git's processes (each admitted transfer, the background budget and the read
// lane) and the store's page cache, which lives in Turso's native engine. The
// tenth is the margin for what no line counts: a direct box's harness runs and
// native fragmentation. So the runtime's line is the executor's pools, kept
// with the heap's room under the limit (executor.NodeWork.MemoryPools), over
// nine tenths: what keeps the limit at or above the pools wherever the node
// holds what it keeps. The serving share (internal/gitlet/cache) is the rest of
// the runtime's limit: the broker, the engine's own heap and the store beyond
// its page cache.
package nodebudget

import (
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/store"
)

// Lines is what a node keeps of its memory beside Gitlet's transfers, serving
// share and background budget, each line in bytes.
type Lines struct {
	// Runtime is the Go runtime's share for the executor's pools: the pools
	// over nine tenths, the tenth the limit leaves.
	Runtime int64
	// PageCache is the store's page cache across every connection of its
	// files (store.PageCacheShare), outside the runtime.
	PageCache int64
	// ReadLane is Gitlet's read lane at each of its slots
	// (gitproc.ReadMemory), git processes outside the runtime; zero on a node
	// serving no Gitlet.
	ReadLane int64
}

// Node is what a node's lines are computed from.
type Node struct {
	// Work is what the node runs of the executor's pools.
	Work executor.NodeWork
	// Conns is how many connections its store holds across its files
	// (store.Options.NodeConnections).
	Conns int
	// PageCacheSized is whether its store sizes its page cache by the node's
	// memory, as a running node does, or keeps the floor, as a test binary
	// does (internal/engine's storeSizedBy).
	PageCacheSized bool
}

// Of is n's lines on a node of memory bytes whose Gitlet read lane is
// readSlots wide (zero where it serves none).
func (n Node) Of(memory int64, readSlots int) Lines {
	sizedBy := int64(0)
	if n.PageCacheSized {
		sizedBy = memory
	}
	return Lines{
		Runtime:   n.Work.MemoryPools(memory) * 10 / 9,
		PageCache: store.PageCacheShare(sizedBy, n.Conns),
		ReadLane:  int64(readSlots) * gitproc.ReadMemory,
	}
}

// Kept is what the node keeps beside Gitlet before sizing its lanes and its
// background budget: every line.
func (l Lines) Kept() int64 { return l.Runtime + l.PageCache + l.ReadLane }

// Outside is what of the lines the node holds outside the Go runtime: the
// page cache and the read lane, which the limit takes out beside git's
// transfers and background budget.
func (l Lines) Outside() int64 { return l.PageCache + l.ReadLane }

// Limit is the Go runtime's memory limit on a node of memory bytes of which
// outside is held outside the runtime: nine tenths of what that leaves, or
// zero where nothing is left (a limit at nothing would spend half the cores
// collecting and hold nothing).
func Limit(memory, outside int64) int64 {
	left := memory - outside
	if left <= 0 {
		return 0
	}
	return left / 10 * 9
}
