// Package placement decides which members of a map hold each of its groups,
// from the map alone: a pure function every node evaluates identically, so
// nobody asks anybody where something is.
//
// # One draw for two maps, over an abstract set of groups
//
// Two maps in this engine place things on data nodes: the object store's map
// places the BYTES of a company's files (internal/objstore/placement), and
// the estate map places the partitions of the replicated estate. Both need
// the same thing — a ranking of the members for each of a set of groups that
// moves as little as possible when the members change, spreads copies across
// failure domains, honours a weight per member and agrees to the last bit on
// every CPU — and that ranking is subtle enough that two copies of it would
// drift the way every duplicated rule in this tree has (ADR-0008). So it is
// written once, here, over an ABSTRACT set of groups ([Groups]): how many
// there are and each one's seed. What a group IS belongs to the map:
//
//   - the object map's groups are runs of content-address slots, and their
//     count grows with the fleet by SPLITTING one bit at a time, with seeds
//     built so a split keeps the lower half of every group where it was
//     (objstore/placement.SplitGroups);
//   - the estate map's groups are the layout's partitions, one fixed seed
//     each, never split — a partition count changes only by a repartition.
//
// Everything a draw needs is one value, [Draw]: the members, how many copies
// each group takes, the failure-domain label, the groups, and the map's
// [Salt].
//
// # A salt per map
//
// A member's half of every draw is a hash of its node id under its map's
// salt. Without one, two maps over the same nodes and a group whose seed
// happened to coincide — group 7 of one, partition 7 of the other — would
// rank the members identically, and a correlation between the two maps is a
// correlation between two failures: the node whose loss costs the most of one
// map would be the node whose loss costs the most of the other. The salts are
// declared here, together ([ObjectSalt], [EstateSalt]), so no third map can
// quietly reuse one, and a draw naming none is refused.
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
// The tests hold these as properties and log the figures, over the object
// map's groups. An eleventh member joining ten at three copies over 1024
// groups enters 26% of the groups' up sets, each changed by exactly that one
// member — so the copies that move are exactly the newcomer's, 8.7% of them
// against the 1/11 it is entitled to — and removing it again puts every group
// back. And balancing keeps it small, because a balance starts from the
// shares the fleet has: an eleventh member of weight two joining a balanced
// fleet of mixed weights and then balanced in moves 317 of 3072 copies,
// against the 293 its weight entitles it to.
//
// # Shares are not weights
//
// A member's configured WEIGHT is the fraction of the copies it should hold.
// Straw2 is proportional for the FIRST copy only: the second and third are
// drawn among whoever is left, so at three copies over ten members a node of
// weight four holds about 2.8 times what a node of weight one does rather than
// four — its first copies crowd out nobody, but its later ones can only go to
// groups it does not already hold. A map therefore carries a SHARE per member
// — the weight [Balance] found makes the copy counts come out proportional —
// computed by whichever node writes a change to what the map places (the
// map's maintainer on its own tick, and the node serving an operator's out or
// in) and stored in it, so no node READING the map recomputes it and nothing
// computed in floating point is ever compared between nodes.
//
// Proportional to within a TOLERANCE, two percent by default, which is a
// number of copies: two percent of a member's target is one copy at fifty
// copies. A balance promises the tolerance wherever it is at least a copy and
// a half of every placeable member's target — seventy-five copies at two
// percent — and within that promise every fleet measured converged in at most
// 32 rounds, a median of five, inside the [DefaultMaxRounds] of sixty a
// balance is bounded by. The object map sizes its group count so a member of
// the mean weight holds a hundred copies (objstore/placement.TargetPGBits),
// which puts every member of at least three quarters of the mean weight
// inside the promise — PROVIDED no failure domain is capped. A domain holds at
// most one copy of each group, so one carrying more than a copy's worth of the
// fleet's weight — over a third of it at three copies, a crowded zone beside
// lone ones — divides only the groups among its members, and they are
// entitled to fewer copies than their weight says: twelve equal members in
// zones of one, one and ten at three copies get 51.2 each over 512 groups, a
// tolerance of about one copy. A fleet with a target too small for the
// promise — a light member, or one in a capped domain — nearly always
// converges while its tolerance is at least a copy, less often below that,
// and more often not below half a copy; one that does not stops at
// [DefaultMaxRounds] and answers the closest layout it measured, reporting
// Converged false, and the cure is more groups, not more rounds. [Balance]
// has the measurements.
//
// # Failure domains
//
// A map may name a node LABEL as its failure domain, and then a group's copies
// go to members with distinct values of it — no two copies of anything in one
// zone, one rack, one host — whenever the fleet has as many distinct values
// as copies. When it has fewer, the extra copies go to the best remaining
// members regardless ([Draw.DomainLimited] says so): a placement that refused
// to place would lose writes to protect against a failure it can no longer
// avoid anyway. Every domain then holds a copy of every group, whatever its
// weight — a lone node in a zone of its own holds every group — and the
// weights divide the copies above that.
//
// # Members that take no copies
//
// A member marked OUT — taken out by an operator, so its data moves off it
// while it keeps serving — takes no copies, but it stays in the map and at
// the tail of every group's ranking ([Draw.Ranked]), so a reader still finds
// the copies it holds until the data has moved and a collector finds it as a
// holder to confirm against. Not "drained": a drain in this engine is what a
// node does to itself on its way down.
//
// A member on PROBATION — a node the map removed for absence, seen back and
// proving itself stable — is placed exactly as an out member is, and for the
// same reason: it may hold the only copies of what it held before it went,
// and a node a reader cannot find is a node whose copies read as lost. The
// two are separate flags because they are separate decisions with separate
// ends — an operator's, lifted only by an operator, and the maintainer's,
// lifted by the ticks (internal/membership) — and one flag could not say
// which a member carries, nor end one without the other. [Member.Placeable]
// is the one question every placement rule asks of both.
package placement

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	seatplacement "github.com/crewlet/crewlet/internal/seat/placement"
)

// Salt separates one map's draws from every other's: it is the domain a
// member's node id is hashed under before it takes part in any draw.
type Salt string

// The salts, one per map that places — declared together so that two maps can
// never share one.
const (
	// ObjectSalt is the object store's map. Its value is the one the object
	// map's draws were first written with, unchanged, so every placement a
	// fleet already made survives — objstore/placement's pins hold it.
	ObjectSalt Salt = "crewlet-objstore-node\x00"

	// EstateSalt is the estate map's, which places the replicated estate's
	// partitions.
	EstateSalt Salt = "crewlet-estate-node\x00"
)

// Valid reports whether s is a salt this build declares.
func (s Salt) Valid() bool { return s == ObjectSalt || s == EstateSalt }

// Groups is what a map places: how many groups there are, and the seed each
// one draws its members with.
//
// A seed is the group's whole identity to a draw: two groups with one seed
// under one salt rank the same members identically. The map decides what a
// group is and how its seeds are built — see the package doc.
type Groups interface {
	// Count is how many groups there are, numbered 0 to Count()-1.
	Count() int

	// Seed is group g's seed, for 0 <= g < Count().
	Seed(g int) uint64
}

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
// copy across the broker, so a larger count is paid for by every write.
const MaxReplicas = 10

// shareOne is a share of weight 1.0: shares are 16.16 fixed point.
const shareOne = 1 << 16

// DefaultShare is the share a member of this weight starts at in a fleet that
// has never been balanced.
func DefaultShare(weight int) uint32 { return uint32(weight) * shareOne }

// Member is one data node in a map.
type Member struct {
	// Node is the node's id — the name its leases and its servers are
	// registered under.
	Node string `json:"node"`

	// Weight is the fraction of the copies it should hold, relative to
	// the others: 1..[MaxWeight], from the weight the node offers.
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
	// member, so a reader and a collector still find the copies it holds.
	// Only an operator's gesture sets or clears it.
	Out bool `json:"out,omitempty"`

	// Probation is a node the map removed for absence and has seen back,
	// present and healthy, for fewer ticks than it takes to trust it again:
	// placed on nothing, exactly as an out member is, but a member — at
	// the tail of every ranking, where a reader and a repair look for the
	// copies it held when it went. The map's maintainer sets and clears it;
	// see membership.Removal for the rule and why a node proving itself is
	// not simply left out of the map.
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

// Draw is everything a ranking needs: a map's members, its copies and failure
// domain, its groups and its salt.
//
// A VALUE, identical on every node that read the same version of the map it
// came from, and every function on it is pure — so two nodes holding one map
// answer every question about it the same way without asking each other. A
// map is stored without its salt and its groups, which are facts about what
// KIND of map it is, and builds its draw when asked; every method below
// assumes a draw [Draw.Validate] accepts.
type Draw struct {
	// Salt is the map's: see [ObjectSalt] and [EstateSalt].
	Salt Salt

	// Replicas is how many copies the map asks for, 1..[MaxReplicas]; it
	// places [Draw.Size], which is bounded by the members it can place on.
	Replicas int

	// FailureDomain is the node label whose values a group's copies are
	// spread across, and empty for no constraint.
	FailureDomain string

	// Members are the data nodes in the map, sorted by node, unique.
	Members []Member

	// Groups is what the map places.
	Groups Groups
}

// ErrInvalid is a map this package refuses to place by.
var ErrInvalid = errors.New("placement: invalid map")

// Validate refuses a draw that cannot place, naming what is wrong with it.
func (d Draw) Validate() error {
	switch {
	case !d.Salt.Valid():
		return fmt.Errorf("%w: salt %q is not one this build declares", ErrInvalid, string(d.Salt))
	case d.Groups == nil || d.Groups.Count() < 1:
		return fmt.Errorf("%w: it places no groups", ErrInvalid)
	case d.Replicas < 1 || d.Replicas > MaxReplicas:
		return fmt.Errorf("%w: replicas %d, want 1..%d", ErrInvalid, d.Replicas, MaxReplicas)
	}
	if d.FailureDomain != "" {
		if err := seatplacement.CheckLabelKey(d.FailureDomain); err != nil {
			return fmt.Errorf("%w: failure_domain: %w", ErrInvalid, err)
		}
	}
	for i, member := range d.Members {
		switch {
		case strings.TrimSpace(member.Node) == "":
			return fmt.Errorf("%w: member %d names no node", ErrInvalid, i)
		case i > 0 && d.Members[i-1].Node >= member.Node:
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
		case member.Domain != "" && d.FailureDomain == "":
			// A domain for a label the map does not spread across is a
			// writer that read one map's label and wrote another's.
			return fmt.Errorf("%w: %s carries failure domain %q but the map names "+
				"no failure-domain label", ErrInvalid, member.Node, member.Domain)
		}
	}
	return nil
}

// Size is how many copies each group has: the replica count, bounded by the
// members the draw can place on ([Member.Placeable]).
func (d Draw) Size() int {
	placeable := 0
	for _, member := range d.Members {
		if member.Placeable() {
			placeable++
		}
	}
	return min(d.Replicas, placeable)
}

// Member is the member named node, and false when the draw has none.
func (d Draw) Member(node string) (Member, bool) {
	i, found := slices.BinarySearchFunc(d.Members, node, func(a Member, n string) int {
		return strings.Compare(a.Node, n)
	})
	if !found {
		return Member{}, false
	}
	return d.Members[i], true
}

// Holds reports whether a node is a member — placeable or not, since a member
// out or on probation still holds copies somebody may read.
func (d Draw) Holds(node string) bool {
	_, ok := d.Member(node)
	return ok
}

// Placeable is every member that takes copies ([Member.Placeable]), in node
// order.
func (d Draw) Placeable() []Member {
	out := make([]Member, 0, len(d.Members))
	for _, member := range d.Members {
		if member.Placeable() {
			out = append(out, member)
		}
	}
	return out
}

// DistinctDomains is how many failure domains the placeable members span. A
// member with no domain — the map names no label, or the node does not carry
// it — is a domain of its own.
func (d Draw) DistinctDomains() int {
	seen := map[string]struct{}{}
	alone := 0
	for _, member := range d.Members {
		switch {
		case !member.Placeable():
		case d.FailureDomain == "" || member.Domain == "":
			alone++
		default:
			seen[member.Domain] = struct{}{}
		}
	}
	return len(seen) + alone
}

// DomainLimited reports whether the draw names a failure domain and the fleet
// spans fewer of them than a group has copies — so some groups hold two copies
// in one domain, and losing that domain costs them two.
func (d Draw) DomainLimited() bool {
	return d.FailureDomain != "" && d.DistinctDomains() < d.Size()
}
