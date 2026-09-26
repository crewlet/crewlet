// Package placement decides which data nodes hold an object, from a map every
// node agrees on and nothing else.
//
// # What is divided, and what is not
//
// The replicated estate is held WHOLE on every data node: a task, a page, the
// row that says a file exists. The bytes of the files themselves are not — a
// company's attachments grow without bound and every data node holding all of
// them is the storage bill the whole fleet design was meant to avoid. So the
// metadata stays replicated and the BULK is placed: each object is held by
// [Map.Replicas] data nodes, chosen by a pure function of the object's hash and
// the map.
//
// # Placement groups
//
// An object is first hashed into one of [PGCount] placement groups, and the
// GROUP is what is placed. The indirection is what keeps a map change cheap to
// reason about: which groups moved between two maps is a comparison over 256
// entries rather than over every object a company has, and a repair walks a
// group at a time. The count is FIXED for the life of a deployment, for the
// reason the search's 64 buckets are: changing it re-places every object.
//
// # Weighted rendezvous, in integers
//
// A group's holders are the [Map.Replicas] members with the highest DRAW, and a
// member of weight w draws w independent 64-bit hashes of (group, member,
// draw) and keeps its best. The chance that a member holds the top draw is its
// weight over the total — the property a weight is for — and adding a member
// moves only the groups that member now wins, while removing one moves only
// the groups it held: the two properties that make a map change move the
// least data it can.
//
// CRUSH's straw2 computes the same distribution with a natural logarithm, and
// this package deliberately does not: Go's math.Log is per-architecture
// assembly, and two nodes on different CPUs disagreeing in the last bit of a
// float would place a group on different holders — each asking the other for
// an object neither will store. Integers compare the same everywhere.
package placement

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// PGCount is how many placement groups the object space is divided into.
//
// TWO HUNDRED AND FIFTY-SIX: a fleet of three to thirty data nodes then holds
// eight to eighty-five groups each, which is the range in which a
// hash-placed share lands within a few percent of its weight — Ceph's own
// guidance is on the order of a hundred per device — and a map comparison or
// a repair pass over every group stays trivially cheap. Fixed, because
// changing it re-places every object in the company.
const PGCount = 256

// MaxWeight bounds a member's weight, and with it the draws one placement
// computes: a member of weight w costs w hashes per group.
//
// SIXTY-FOUR: a sixty-four-to-one ratio between the largest and the smallest
// data node is already a fleet whose small node holds a sliver, and
// 256 groups × 64 draws × a few dozen members is a map computed in
// milliseconds once per epoch.
const MaxWeight = 64

// Member is one data node in the map.
type Member struct {
	// Node is the node's id — the name its presence lease and its object
	// server are registered under.
	Node string `json:"node"`

	// Weight is its relative share of the groups, 1..[MaxWeight].
	Weight int `json:"weight"`
}

// Map is the fleet's object placement: which data nodes hold objects, and how
// many hold each one.
//
// A VALUE, identical on every node that read the same version of it, and
// every function on it is pure — so two nodes holding one map answer every
// question about it the same way without asking each other.
type Map struct {
	// Epoch counts changes. A node that read an older epoch places by an
	// older map, which is correct for as long as it takes to read the next
	// one: every object a newer map moves is still held where the older
	// map put it until a repair has copied it.
	Epoch uint64 `json:"epoch"`

	// Replicas is how many members hold each object — bounded by how many
	// members there are.
	Replicas int `json:"replicas"`

	// Members are the data nodes in the map, sorted by node.
	Members []Member `json:"members"`
}

// ErrInvalid is a map this package refuses to place by.
var ErrInvalid = errors.New("placement: invalid map")

// Validate refuses a map that cannot place: an empty member, a duplicate, a
// weight out of range, or members out of order.
func (m Map) Validate() error {
	if m.Replicas < 1 {
		return fmt.Errorf("%w: %d replicas", ErrInvalid, m.Replicas)
	}
	for i, member := range m.Members {
		switch {
		case strings.TrimSpace(member.Node) == "":
			return fmt.Errorf("%w: member %d names no node", ErrInvalid, i)
		case member.Weight < 1 || member.Weight > MaxWeight:
			return fmt.Errorf("%w: %s has weight %d, want 1..%d",
				ErrInvalid, member.Node, member.Weight, MaxWeight)
		case i > 0 && m.Members[i-1].Node >= member.Node:
			return fmt.Errorf("%w: members are not sorted and unique at %s",
				ErrInvalid, member.Node)
		}
	}
	return nil
}

// Size is how many members hold each object in this map: its replica count,
// bounded by the members it has.
func (m Map) Size() int { return min(m.Replicas, len(m.Members)) }

// Holds reports whether a node is a member.
func (m Map) Holds(node string) bool {
	_, found := slices.BinarySearchFunc(m.Members, node, func(a Member, n string) int {
		return strings.Compare(a.Node, n)
	})
	return found
}

// PG is the placement group a content hash falls into.
//
// The hash is the object's own content address — already uniform — so its
// first four bytes are the group. A hash shorter than that is not a content
// address and lands in group 0, which the caller's own validation refuses
// long before it matters.
func PG(hash []byte) int {
	if len(hash) < 4 {
		return 0
	}
	return int(binary.BigEndian.Uint32(hash[:4]) % PGCount)
}

// Quorum is how many members must hold a chunk before a writer may report it
// stored: a majority of [Map.Size].
//
// A MAJORITY, for the reason the stream a file's record lands on takes one:
// an acknowledgement from fewer is a copy one disk can lose, and asking for
// all of them would stop every upload while any one holder is down — for the
// whole time it takes the map to replace it. The record naming the chunks
// cannot land without a majority of the stream's own peers either, so an
// object store that accepted less would be storing bytes nothing can refer
// to.
func (m Map) Quorum() int { return m.Size()/2 + 1 }

// Up is the members that hold a group, best first. The first is the group's
// PRIMARY — the member a writer and a reader try before the others.
func (m Map) Up(pg int) []string {
	return m.Ranked(pg)[:m.Size()]
}

// Ranked is every member in the order a group prefers them: its up set first,
// then everybody else.
//
// The tail is where a writer goes when a holder does not answer — a copy on
// the next member in the order rather than one copy fewer — and where a reader
// looks when no holder has the chunk, because that is where such a copy is.
func (m Map) Ranked(pg int) []string {
	type scored struct {
		node string
		draw uint64
	}
	scores := make([]scored, 0, len(m.Members))
	for _, member := range m.Members {
		scores = append(scores, scored{node: member.Node, draw: bestDraw(pg, member)})
	}
	slices.SortFunc(scores, func(a, b scored) int {
		switch {
		case a.draw > b.draw:
			return -1
		case a.draw < b.draw:
			return 1
		}
		// A TIE IS BROKEN BY THE NAME, so it is broken the same way
		// everywhere. Two 64-bit draws colliding is a curiosity; two
		// nodes ordering it differently would be a bug.
		return strings.Compare(a.node, b.node)
	})
	out := make([]string, 0, len(scores))
	for _, s := range scores {
		out = append(out, s.node)
	}
	return out
}

// bestDraw is a member's highest of its weight's draws for one group.
func bestDraw(pg int, member Member) uint64 {
	var best uint64
	var buf [8]byte
	for i := range member.Weight {
		h := sha256.New()
		binary.BigEndian.PutUint32(buf[:4], uint32(pg))
		binary.BigEndian.PutUint32(buf[4:], uint32(i))
		_, _ = h.Write(buf[:])
		_, _ = h.Write([]byte(member.Node))
		if draw := binary.BigEndian.Uint64(h.Sum(nil)[:8]); draw > best {
			best = draw
		}
	}
	return best
}

// Layout is every group's holders under one map, computed once.
type Layout [PGCount][]string

// Layout computes where every group lives.
func (m Map) Layout() Layout {
	var out Layout
	for pg := range PGCount {
		out[pg] = m.Up(pg)
	}
	return out
}

// Moved is every group whose holders differ between two layouts — what a map
// change asks the fleet to copy.
func Moved(before, after Layout) []int {
	var out []int
	for pg := range PGCount {
		if !slices.Equal(before[pg], after[pg]) {
			out = append(out, pg)
		}
	}
	return out
}
