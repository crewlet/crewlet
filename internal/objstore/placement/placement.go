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
// would move the data a split kept still.
//
// # Straw2, in integers
//
// A group's members are ranked by STRAW2, Ceph's own draw: each member scores
// log(u)/share for a uniform u hashed from (group, member), and the highest
// score wins. That is an exponential race, so a member's chance of the top
// place is its share over the total, and adding a member moves only the
// groups it now wins while removing one moves only the groups it held — the
// two properties that make a map change copy the least data it can. The
// logarithm is computed in INTEGER fixed point (log2fixed), as Ceph's
// crush_ln is, because a float logarithm is per-architecture assembly and two
// nodes a last bit apart would place one group on different members.
//
// # What a change moves, measured
//
// The tests hold these as properties and log the figures. An eleventh member
// joining ten at three copies over 1024 groups enters 26% of the groups' up
// sets, each changed by exactly that one member — so the copies that move are
// exactly the newcomer's, 8.7% of them against the 1/11 it is entitled to —
// and removing it again puts every group back. A doubling of the group count
// keeps every lower child's holders and re-draws the upper ones: 49.5% of the
// slots change holders over ten members, never more than half. And
// balancing keeps it small, because a balance starts from the shares the
// fleet has: an eleventh member of weight two joining a balanced fleet of
// mixed weights and then balanced in moves 317 of 3072 copies, against the
// 293 its weight entitles it to.
//
// # Shares are not weights
//
// A member's configured WEIGHT is the fraction of the copies it should hold.
// Straw2 is proportional for the FIRST copy only: the second and third are
// drawn among whoever is left, so at three copies over ten members a node of
// weight four holds about 2.8 times what a node of weight one does rather than
// four — its first copies crowd out nobody, but its later ones can only go to
// groups it does not already hold. The map therefore carries a SHARE per
// member — the weight [Balance] found makes the copy counts come out
// proportional — computed by whichever node writes a change to what the map
// places (the map's maintainer on its own tick, and the node serving an
// operator's out or in) and stored in it, so no node READING the map
// recomputes it and nothing computed in floating point is ever compared
// between nodes.
//
// Proportional to within a TOLERANCE, two percent by default, which is a
// number of copies: two percent of a member's target is one copy at fifty
// copies. A balance promises the tolerance wherever it is at least a copy and
// a half of every placeable member's target — seventy-five copies at two
// percent — and within that promise every fleet measured converged in at most
// 32 rounds, a median of five, inside the [DefaultMaxRounds] of sixty a
// balance is bounded by. [TargetPGBits] sizes a map so a member of the mean
// weight holds a hundred copies, which puts every member of at least three
// quarters of the mean weight inside the promise — PROVIDED no failure domain
// is capped. A domain holds at most one copy of each group, so one carrying
// more than a copy's worth of the fleet's weight — over a third of it at three
// copies, a crowded zone beside lone ones — divides only the groups among its
// members, and they are entitled to fewer copies than their weight says:
// twelve equal members in zones of one, one and ten at three copies get 51.2
// each over the 512 groups [TargetPGBits] picks, a tolerance of about one
// copy. A fleet with a target too small for the promise — a light member, or
// one in a capped domain — nearly always converges while its tolerance is at
// least a copy, less often below that, and more often not below half a copy;
// one that does not stops at [DefaultMaxRounds] and answers the closest layout
// it measured, reporting Converged false, and the cure is more groups, not
// more rounds. [Balance] has the measurements.
//
// # Failure domains
//
// A map may name a node LABEL as its failure domain, and then a group's copies
// go to members with distinct values of it — no two copies of anything in one
// zone, one rack, one host — whenever the fleet has as many distinct values
// as copies. When it has fewer, the extra copies go to the best remaining
// members regardless ([Map.DomainLimited] says so): a placement that refused
// to place would lose writes to protect against a failure it can no longer
// avoid anyway. Every domain then holds a copy of every group, whatever its
// weight — a lone node in a zone of its own holds every group — and the
// weights divide the copies above that.
//
// # Members that take no copies
//
// A member marked OUT — taken out by an operator, so its data moves off it
// while it keeps serving — takes no copies, but it stays in the map and at
// the tail of every group's ranking ([Map.Ranked]), so a reader still finds
// the copies it holds until the data has moved and the collector finds it as
// a holder to confirm against. Not "drained": a drain in this engine is what
// a node does to itself on its way down.
//
// A member on PROBATION — a node the map removed for absence, seen back and
// proving itself stable — is placed exactly as an out member is, and for the
// same reason: it may hold the only copies of what it held before it went,
// and a node a reader cannot find is a node whose copies read as lost. The
// two are separate flags because they are separate decisions with separate
// ends — an operator's, lifted only by an operator, and the maintainer's,
// lifted by the ticks — and one flag could not say which a member carries,
// nor end one without the other. [Member.Placeable] is the one question
// every placement rule asks of both.
package placement

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	seatplacement "github.com/crewlet/crewlet/internal/seat/placement"
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

// MaxWeight bounds a member's configured weight.
//
// SIXTY-FOUR: a sixty-four-to-one ratio between the largest and the smallest
// data node is already a fleet whose small node holds a sliver, and a share
// ([Member.Share]) is a weight in 16.16 fixed point, so the largest default
// share, 64<<16, leaves the balancer room to multiply it well past anything
// it needs inside a uint32.
const MaxWeight = 64

// MaxReplicas bounds how many copies a map asks for.
//
// TEN, Ceph's own ceiling on a pool's size: past it a copy buys nothing a
// failure domain has not already bought, and every write fans out to every
// copy across the broker, so a larger count is paid for by every upload.
const MaxReplicas = 10

// shareOne is a share of weight 1.0: shares are 16.16 fixed point.
const shareOne = 1 << 16

// DefaultShare is the share a member of this weight starts at in a fleet that
// has never been balanced.
func DefaultShare(weight int) uint32 { return uint32(weight) * shareOne }

// Member is one data node in the map.
type Member struct {
	// Node is the node's id — the name its leases and its object server are
	// registered under.
	Node string `json:"node"`

	// Weight is the fraction of the copies it should hold, relative to
	// the others: 1..[MaxWeight], from its store.objects.weight.
	Weight int `json:"weight"`

	// Share is what its draws are divided by: the balancer's effective
	// weight, in 16.16 fixed point (1<<16 is weight 1.0), never zero. It
	// differs from the weight because straw2 is proportional only for the
	// first copy — see the package doc.
	Share uint32 `json:"share"`

	// Domain is this node's value of the map's failure-domain label, and
	// empty when the map names none or the node does not carry the label —
	// such a member is a domain of its own and never collides.
	Domain string `json:"domain,omitempty"`

	// Out is a member an OPERATOR took out: placed on nothing, but still a
	// member, so a reader and the collector still find the copies it holds.
	// Only an operator's gesture sets or clears it.
	Out bool `json:"out,omitempty"`

	// Probation is a node the map removed for absence and has seen back,
	// present and healthy, for fewer ticks than it takes to trust it again:
	// placed on nothing, exactly as an out member is, but a member — at
	// the tail of every ranking, where a reader and a repair look for the
	// copies it held when it went. The maintainer sets and clears it; see
	// objstore.Removal for the rule and why a node proving itself is not
	// simply left out of the map.
	//
	// ITS OWN FLAG, NOT OUT: out is an operator's decision that the
	// maintainer never lifts, and probation the maintainer's that ends by
	// itself — a member can be both, and ending one must leave the other.
	Probation bool `json:"probation,omitempty"`
}

// Placeable reports whether the member takes copies: neither taken out nor
// on probation. Every placement rule — the up sets, the copies a map places,
// the balance, the rate a newcomer joins at — asks this and nothing else, so
// a member that takes no copies is one thing to all of them whichever reason
// it has.
func (m Member) Placeable() bool { return !m.Out && !m.Probation }

// Map is the fleet's object placement: which data nodes hold objects, how
// many hold each one, and how the slots are grouped.
//
// A VALUE, identical on every node that read the same version of it, and
// every function on it is pure — so two nodes holding one map answer every
// question about it the same way without asking each other. Every method
// below assumes a map [Map.Validate] accepts; the stored map is validated on
// every read.
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

	// Replicas is how many copies the map asks for, 1..[MaxReplicas]; it
	// places [Map.Size], which is bounded by the members it can place on.
	Replicas int `json:"replicas"`

	// PGBits is how many bits of a slot name its group:
	// [MinPGBits]..[MaxPGBits].
	PGBits int `json:"pg_bits"`

	// FailureDomain is the node label whose values a group's copies are
	// spread across, and empty for no constraint.
	FailureDomain string `json:"failure_domain,omitempty"`

	// Members are the data nodes in the map, sorted by node, unique.
	Members []Member `json:"members"`
}

// ErrInvalid is a map this package refuses to place by.
var ErrInvalid = errors.New("placement: invalid map")

// Validate refuses a map that cannot place, naming what is wrong with it.
func (m Map) Validate() error {
	switch {
	case m.Generation == uuid.Nil:
		return fmt.Errorf("%w: it names no generation", ErrInvalid)
	case m.Replicas < 1 || m.Replicas > MaxReplicas:
		return fmt.Errorf("%w: replicas %d, want 1..%d", ErrInvalid, m.Replicas, MaxReplicas)
	case m.PGBits < MinPGBits || m.PGBits > MaxPGBits:
		return fmt.Errorf("%w: pg_bits %d, want %d..%d", ErrInvalid, m.PGBits,
			MinPGBits, MaxPGBits)
	}
	if m.FailureDomain != "" {
		if err := seatplacement.CheckLabelKey(m.FailureDomain); err != nil {
			return fmt.Errorf("%w: failure_domain: %w", ErrInvalid, err)
		}
	}
	for i, member := range m.Members {
		switch {
		case strings.TrimSpace(member.Node) == "":
			return fmt.Errorf("%w: member %d names no node", ErrInvalid, i)
		case i > 0 && m.Members[i-1].Node >= member.Node:
			return fmt.Errorf("%w: members are not sorted and unique at %s",
				ErrInvalid, member.Node)
		case member.Weight < 1 || member.Weight > MaxWeight:
			return fmt.Errorf("%w: %s has weight %d, want 1..%d",
				ErrInvalid, member.Node, member.Weight, MaxWeight)
		case member.Share == 0:
			// A zero share would divide by zero in every draw: the
			// balancer never writes one, so a map carrying it was not
			// written by one.
			return fmt.Errorf("%w: %s has share 0, want at least 1", ErrInvalid, member.Node)
		case member.Domain != "" && m.FailureDomain == "":
			// A domain for a label the map does not spread across is a
			// writer that read one map's label and wrote another's.
			return fmt.Errorf("%w: %s carries failure domain %q but the map names "+
				"no failure-domain label", ErrInvalid, member.Node, member.Domain)
		}
	}
	return nil
}

// Size is how many copies each group has in this map: its replica count,
// bounded by the members it can place on ([Member.Placeable]).
func (m Map) Size() int {
	placeable := 0
	for _, member := range m.Members {
		if member.Placeable() {
			placeable++
		}
	}
	return min(m.Replicas, placeable)
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

// Member is the member named node, and false when the map has none.
func (m Map) Member(node string) (Member, bool) {
	i, found := slices.BinarySearchFunc(m.Members, node, func(a Member, n string) int {
		return strings.Compare(a.Node, n)
	})
	if !found {
		return Member{}, false
	}
	return m.Members[i], true
}

// Holds reports whether a node is a member — placeable or not, since a member
// out or on probation still holds copies somebody may read.
func (m Map) Holds(node string) bool {
	_, ok := m.Member(node)
	return ok
}

// Placeable is every member that takes copies ([Member.Placeable]), in node
// order.
func (m Map) Placeable() []Member {
	out := make([]Member, 0, len(m.Members))
	for _, member := range m.Members {
		if member.Placeable() {
			out = append(out, member)
		}
	}
	return out
}

// DistinctDomains is how many failure domains the placeable members span. A
// member with no domain — the map names no label, or the node does not carry
// it — is a domain of its own.
func (m Map) DistinctDomains() int {
	seen := map[string]struct{}{}
	alone := 0
	for _, member := range m.Members {
		switch {
		case !member.Placeable():
		case m.FailureDomain == "" || member.Domain == "":
			alone++
		default:
			seen[member.Domain] = struct{}{}
		}
	}
	return len(seen) + alone
}

// DomainLimited reports whether the map names a failure domain and the fleet
// spans fewer of them than a group has copies — so some groups hold two copies
// in one domain, and losing that domain costs them two.
func (m Map) DomainLimited() bool {
	return m.FailureDomain != "" && m.DistinctDomains() < m.Size()
}

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
