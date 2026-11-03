package codesearch

import (
	"cmp"
	"slices"
	"sync"
)

// merge is the first Limit lines of a search's answers, folded in as each
// answer arrives: a serving node's of its repositories, the asker's of its
// nodes.
//
// BOUNDED BY THE LIMIT, NEVER BY WHAT IT IS HANDED: a node assigned thousands
// of repositories, each answering up to Limit lines, holds Limit lines and
// the answers of the reads still in flight — never every answer until the
// last one returns, which at Limit 500 and about 400 bytes a line was a
// gigabyte for a node holding five thousand repositories, only to reply its
// first five hundred lines. Folding is exact for the same reason the scatter
// is: the first Limit lines of a union are the first Limit of the first Limit
// of each part.
type merge struct {
	limit int

	mu sync.Mutex
	// held is the first limit lines folded so far, in CompareHits order, each
	// line once; cut is set once a fold has dropped a line past them.
	held []Hit
	cut  bool
}

// newMerge is a merge keeping the first limit lines.
func newMerge(limit int) *merge { return &merge{limit: limit} }

// fold merges hits into the lines held, keeping the first limit, each line
// once. It sorts hits in place: an answer's lines are in its own order
// already, which a sort of a sorted slice keeps at a comparison a line.
func (m *merge) fold(hits []Hit) {
	if len(hits) == 0 {
		return
	}
	slices.SortFunc(hits, CompareHits)
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Hit, 0, min(m.limit, len(m.held)+len(hits)))
	i, j := 0, 0
	for len(out) < m.limit && (i < len(m.held) || j < len(hits)) {
		var next Hit
		switch {
		case j == len(hits) || i < len(m.held) && CompareHits(m.held[i], hits[j]) <= 0:
			next = m.held[i]
			i++
		default:
			next = hits[j]
			j++
		}
		if n := len(out); n > 0 && CompareHits(out[n-1], next) == 0 {
			continue
		}
		out = append(out, next)
	}
	// A LINE LEFT OVER IS CUT unless it is the last line kept again: an
	// answer naming a line twice drops nothing.
	for _, rest := range [][]Hit{m.held[i:], hits[j:]} {
		for _, h := range rest {
			if n := len(out); n == 0 || CompareHits(out[n-1], h) != 0 {
				m.cut = true
				break
			}
		}
	}
	m.held = out
}

// passed reports whether the repository named name is PAST THE LIMIT: the
// merge holds limit lines and the last of them is of a repository named
// before it, so every line of the repository would sort after them and none
// can be in the answer. The last line held only ever moves earlier as more
// are folded in, so this is final the moment it holds, whatever is still
// being read: a reader asks it before reading a repository, and passes over
// one it holds for; and once every answer is folded in, it decides by the
// name alone which repositories the coverage names past the limit — read or
// not — so the coverage is the same however the reads interleaved.
func (m *merge) passed(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.limit > 0 && len(m.held) == m.limit && cmp.Less(m.held[m.limit-1].Repo, name)
}

// lines is the lines held, and whether a fold cut any.
func (m *merge) lines() ([]Hit, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.held, m.cut
}
