package estate

import (
	"sync"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/statelog"
)

// repoFloors is how far this node's own writes reached in each repository:
// per repository, the furthest position a write of this node to it landed at
// — a push, a compaction, an expunge record, a change's write or a lifecycle
// request it proposed that applied or is pending, a push it proposed found
// landed, an engine write it asked that applied or is pending — which
// [CodeSeam.FloorOf] answers. Nothing else raises an entry: a read's position
// is somebody else's writes.
//
// BOUNDED at [maxRepoFloors] repositories: past it the entry at the LOWEST
// position goes, the write likeliest to be held by every copy already. A
// repository with no entry floors nothing, which costs a read whose copy
// lacks a commit this node pushed there one sync at the log's head — the git
// node's second look for an id its copy lacks (internal/code/query) — and
// never a wrong answer. Unbounded, the map would hold every repository the
// node ever wrote, purged ones and a seat's scratch repositories included,
// for its whole life.
//
// The zero value holds nothing and is ready to use.
type repoFloors struct {
	mu sync.Mutex
	at map[string]statelog.Position
}

// maxRepoFloors is how many repositories' floors a node keeps: the most
// repositories one company document declares (config.CodeMaxRepos), so a node
// writing to every repository its company declares keeps a floor for each —
// an id and a position, under two hundred bytes apiece with the map's own,
// under two megabytes at the bound.
const maxRepoFloors = config.CodeMaxRepos

// observe raises repoID's floor to at, where at is further than what it holds.
// A position with no stream or no sequence names nothing to wait for and is
// ignored, as the session ignores one ([Session.Observe]).
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

// evictLowest drops the entry at the lowest position; f.mu is held. A scan,
// since it runs only once the node has written to more repositories than one
// company declares, and then at most once a write to a repository it holds no
// floor of.
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
