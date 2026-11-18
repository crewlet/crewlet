package estate

import (
	"sync"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/statelog"
)

// repoFloors is, per repository, the furthest position a write of THIS node
// to it landed at (applied or pending), which [CodeSeam.FloorOf] answers. A
// read's position never raises an entry: it is somebody else's writes.
//
// Bounded at [maxRepoFloors]: past it the LOWEST entry goes, the write
// likeliest already held by every copy. A missing entry costs a read whose
// copy lacks this node's push one sync at the log's head (the Gitlet node's
// second look for an id its copy lacks, internal/gitlet/query), never a wrong
// answer; unbounded, the map would keep every repository the node ever wrote,
// purged and scratch ones included.
//
// The zero value holds nothing and is ready to use.
type repoFloors struct {
	mu sync.Mutex
	at map[string]statelog.Position
}

// maxRepoFloors is the most repositories one company declares
// (config.CodeMaxRepos), so a node writing to all of them keeps a floor for
// each: under two hundred bytes apiece, under two megabytes at the bound.
const maxRepoFloors = config.CodeMaxRepos

// observe raises repoID's floor to at where at is further. A position with no
// stream or sequence names nothing to wait for and is ignored, as
// [Session.Observe] ignores one.
func (f *repoFloors) observe(repoID string, at statelog.Position) {
	if repoID == "" || at.Stream == "" || at.Seq == 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	held, ok := f.at[repoID]
	switch {
	case ok && at.Packed() <= held.Packed():
		return
	case !ok && len(f.at) >= maxRepoFloors:
		f.evictLowest()
	}
	if f.at == nil {
		f.at = map[string]statelog.Position{}
	}
	f.at[repoID] = at
}

// evictLowest drops the entry at the lowest position; f.mu is held. A scan
// suffices: it runs only past the bound, at most once per write to a
// repository holding no floor.
func (f *repoFloors) evictLowest() {
	var (
		lowest string
		low    statelog.Position
	)
	for id, at := range f.at {
		if lowest == "" || at.Packed() < low.Packed() {
			lowest, low = id, at
		}
	}
	delete(f.at, lowest)
}

// floor is repoID's floor, zero where this node holds none.
func (f *repoFloors) floor(repoID string) statelog.Position {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.at[repoID]
}
