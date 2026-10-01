package eventfan

import (
	"cmp"
	"slices"
)

// Coverage says which nodes a fleet answer was assembled from.
//
// ONE SHAPE for every fanned answer, `coverage{nodes:[{id,answered,error}],
// complete}`, so a screen renders one callout for all of them and an operator
// reads one sentence: which node did not answer, and why.
type Coverage struct {
	// Nodes is every node the answer asked or heard from, sorted by id. The
	// asker is always one of them and always answered — its own store is
	// read directly, and a failure there is an error rather than a gap.
	Nodes []NodeCoverage `json:"nodes"`

	// Complete is true only when the roster could be read and every node
	// on it answered. False is the honest reading whenever it cannot be
	// shown true — including when the roster itself was unreadable, when
	// nobody can say who was missing.
	Complete bool `json:"complete"`
}

// NodeCoverage is one node's part in an answer.
type NodeCoverage struct {
	ID       string `json:"id"`
	Answered bool   `json:"answered"`

	// Error says why a node did not answer, in words an operator can act
	// on: its own read failed, it speaks another protocol version, its
	// reply could not be read, or nothing arrived inside the budget. Empty
	// when it answered. Always present on the wire, so a reader never has
	// to tell an absent key from an empty one.
	Error string `json:"error"`
}

// Missing names the nodes that did not answer, sorted.
func (c Coverage) Missing() []string {
	var out []string
	for _, n := range c.Nodes {
		if !n.Answered {
			out = append(out, n.ID)
		}
	}
	return out
}

// And is the coverage of an answer assembled from TWO scatters: a node counts
// as answered only if it answered both, because an answer missing either half
// of one node is missing that node.
func (c Coverage) And(other Coverage) Coverage {
	byID := map[string]NodeCoverage{}
	for _, n := range c.Nodes {
		byID[n.ID] = n
	}
	for _, n := range other.Nodes {
		prior, seen := byID[n.ID]
		if !seen {
			byID[n.ID] = n
			continue
		}
		if prior.Answered && !n.Answered {
			byID[n.ID] = n
		}
	}
	out := Coverage{Complete: c.Complete && other.Complete}
	for _, n := range byID {
		out.Nodes = append(out.Nodes, n)
		if !n.Answered {
			out.Complete = false
		}
	}
	slices.SortFunc(out.Nodes, func(a, b NodeCoverage) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

// solo is the coverage of an answer this node gave alone, with nobody else to
// ask: complete, because the roster says it is the fleet.
func solo(self string) Coverage {
	return Coverage{Nodes: []NodeCoverage{{ID: self, Answered: true}}, Complete: true}
}
