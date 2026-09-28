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
// [Map.Size] data nodes, chosen by a pure function of the object's hash and
// the map.
//
// # Slots, and groups that split
//
// A chunk's SLOT is the first two bytes of its content address: 0..65535, a
// pure function of the bytes, fixed for ever. It is what a row stores and what
// a data node's directories are keyed on, so nothing about a chunk has to be
// rewritten when the map changes.
//
// What is PLACED is a group of slots. A map has [Map.PGBits] group bits, and a
// chunk's group is the top PGBits bits of its slot — so every group is a
// contiguous range of slots ([Map.SlotRange]), a repair walks one range at a
// time, and a map change is compared group by group rather than chunk by
// chunk. A fixed count could not serve every fleet: 256 groups over thirty
// data nodes leave the fullest holding about one and a half times its fair
// share, and over two hundred more than two and a half times, because a
// node's share is a count of groups and a count of a few groups is mostly
// noise. So the count grows with the fleet
// ([TargetPGBits]: about a hundred copies per member, Ceph's own target) one
// bit at a time — and a doubling moves HALF the data rather than all of it:
// splitting group p gives 2p (its lower half) and 2p+1, and the seeds
// (seedKey) are built so 2p draws exactly what p drew while 2p+1 draws
// fresh. Ceph's placement-group split has the same property through
// stable_mod; here it comes from the seed. Groups never merge, since a merge
// would move the data a split kept still. Measured: a doubling keeps every
// lower child's holders and re-draws the upper ones, and 49.5% of the slots
// change holders over ten members — never more than half.
//
// # The draw is internal/placement's
//
// How a group's members are ranked — straw2 in integer fixed point, the
// failure-domain walk, the shares the balance stores and the members that
// take no copies — is not the object store's own: the estate map places its
// partitions with the same draw, so it is written once, in internal/placement
// (ADR-0008), whose package doc carries the rationale. What is left here is
// what is about slots: the map's record, its group count and how a group
// splits ([SplitGroups]), and the slot arithmetic. The object map draws under
// its own salt, [placement.ObjectSalt] — the key the draws were first written
// with, unchanged, so every pinned placement survives the move — and this
// package's tests pin every group of two maps, so anything in the draw that
// would change a placement fails here rather than in a fleet.
package placement

import (
	"fmt"
	"slices"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/placement"
)

// SlotBits is how many bits of a content address form its slot.
//
// SIXTEEN: 65536 slots, so the finest map ([MaxPGBits]) still gives a fleet
// of about two thousand data nodes its hundred copies per member at three
// copies, while a slot is two
// bytes a row stores for every chunk it names.
const SlotBits = 16

// Slots is how many slots there are.
const Slots = 1 << SlotBits

// MinPGBits is the fewest group bits a map has: 256 groups, which gives a
// fleet of up to seven data nodes at three copies its hundred copies per
// member. It is a FLOOR — [TargetPGBits] never answers below it and
// [Map.Validate] refuses a map below it — and not where every fleet starts: a
// first map is written at [TargetPGBits] for the data nodes placeable when it
// is created, which is this count only for a founding fleet of up to seven at
// three copies. Fifty founding nodes at three copies start at eleven bits,
// 2048 groups.
const MinPGBits = 8

// MaxPGBits is the most group bits a map has: a group is never smaller than
// one slot.
const MaxPGBits = SlotBits

// Map is the fleet's object placement: which data nodes hold objects, how
// many hold each one, and how the slots are grouped.
//
// A VALUE, identical on every node that read the same version of it, and
// every function on it is pure — so two nodes holding one map answer every
// question about it the same way without asking each other. Every method
// below assumes a map [Map.Validate] accepts; the stored map is validated on
// every read.
//
// ITS DRAW IS BUILT, NOT STORED ([Map.Draw]): the salt and the group shape
// are what make it the OBJECT map, the same for every object map there will
// ever be, so a stored copy of them could only ever disagree with PGBits.
type Map struct {
	// Generation names this map's lineage: minted when a map is first
	// created, so a map written after the stored key was lost and
	// recreated — whose epochs start again — is told apart from the one a
	// node was placing by.
	Generation uuid.UUID `json:"generation"`

	// Epoch counts placement changes. A node that read an older epoch
	// places by an older map, which is correct for as long as it takes to
	// read the next one: every chunk a newer map moves is still held where
	// the older map put it until a repair has copied it.
	Epoch uint64 `json:"epoch"`

	// Replicas is how many copies the map asks for, 1..[placement.MaxReplicas];
	// it places [Map.Size], which is bounded by the members it can place on.
	Replicas int `json:"replicas"`

	// PGBits is how many bits of a slot name its group:
	// [MinPGBits]..[MaxPGBits].
	PGBits int `json:"pg_bits"`

	// FailureDomain is the node label whose values a group's copies are
	// spread across, and empty for no constraint.
	FailureDomain string `json:"failure_domain,omitempty"`

	// Members are the data nodes in the map, sorted by node, unique.
	Members []placement.Member `json:"members"`
}

// Draw is the map as a draw: its members, copies and failure domain, under
// the object map's salt, over its split groups.
func (m Map) Draw() placement.Draw {
	return placement.Draw{
		Salt:          placement.ObjectSalt,
		Replicas:      m.Replicas,
		FailureDomain: m.FailureDomain,
		Members:       m.Members,
		Groups:        SplitGroups{PGBits: m.PGBits},
	}
}

// Validate refuses a map that cannot place, naming what is wrong with it.
//
// THE GROUP BITS BEFORE THE DRAW: a count of 1<<PGBits is what the draw
// validates next, and a negative PGBits has no such count at all.
func (m Map) Validate() error {
	switch {
	case m.Generation == uuid.Nil:
		return fmt.Errorf("%w: it names no generation", placement.ErrInvalid)
	case m.PGBits < MinPGBits || m.PGBits > MaxPGBits:
		return fmt.Errorf("%w: pg_bits %d, want %d..%d", placement.ErrInvalid, m.PGBits,
			MinPGBits, MaxPGBits)
	}
	return m.Draw().Validate()
}

// Equal reports whether two maps are the same map, field for field — the
// question a cached layout is kept across: a layout carries the map it was
// computed from, and a server answers the epoch it placed by from it, so a
// layout kept across an epoch that changed nothing else would answer the old
// epoch, and one kept across a changed member would render every share wrong.
//
// ONE COMPARISON for every cache of a layout — the node's map cache and the
// fleet view's — because written once per cache, a field added to the map is
// one a copy forgets, and that cache goes on answering with the layout of a
// map it no longer describes.
func (m Map) Equal(o Map) bool {
	return m.Generation == o.Generation && m.Epoch == o.Epoch && m.Replicas == o.Replicas &&
		m.PGBits == o.PGBits && m.FailureDomain == o.FailureDomain &&
		slices.Equal(m.Members, o.Members)
}

// Size is how many copies each group has in this map ([placement.Draw.Size]).
func (m Map) Size() int { return m.Draw().Size() }

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

// Member is the member named node, and false when the map has none.
func (m Map) Member(node string) (placement.Member, bool) { return m.Draw().Member(node) }

// Holds reports whether a node is a member ([placement.Draw.Holds]).
func (m Map) Holds(node string) bool { return m.Draw().Holds(node) }

// Placeable is every member that takes copies, in node order
// ([placement.Draw.Placeable]).
func (m Map) Placeable() []placement.Member { return m.Draw().Placeable() }

// DistinctDomains is how many failure domains the placeable members span
// ([placement.Draw.DistinctDomains]).
func (m Map) DistinctDomains() int { return m.Draw().DistinctDomains() }

// DomainLimited reports whether some groups hold two copies in one failure
// domain ([placement.Draw.DomainLimited]).
func (m Map) DomainLimited() bool { return m.Draw().DomainLimited() }

// Ranked is every member in the order group pg prefers them
// ([placement.Draw.Ranked]).
func (m Map) Ranked(pg int) []string { return m.Draw().Ranked(pg) }

// Groups is how many placement groups the map divides the slots into.
func (m Map) Groups() int { return 1 << m.PGBits }

// GroupOf is the group a slot belongs to: its top PGBits bits.
func (m Map) GroupOf(slot int) int { return slot >> (SlotBits - m.PGBits) }

// SlotRange is the slots a group holds, lo inclusive and hi exclusive.
func (m Map) SlotRange(pg int) (lo, hi int) {
	shift := SlotBits - m.PGBits
	return pg << shift, (pg + 1) << shift
}

// SlotOf is the slot of a content address: its first two bytes, big-endian.
//
// The address is a SHA-256 — already uniform — so its leading bytes are a
// uniform slot, and the same for one object everywhere. Anything shorter than
// two bytes is not a content address and is slot 0, which every caller's own
// validation refuses long before it matters.
func SlotOf(hash []byte) int {
	if len(hash) < 2 {
		return 0
	}
	return int(hash[0])<<8 | int(hash[1])
}
