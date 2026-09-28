package placement

// TargetCopiesPerMember is how many group copies [TargetPGBits] sizes a map to
// give each member.
//
// ONE HUNDRED, Ceph's mon_target_pg_per_osd, and for the reason Ceph gives: a
// member's share of the data is a COUNT of groups, and a count of n has noise
// of about the square root of n — so a hundred copies holds each member to
// about a tenth of its share (one standard deviation) before balancing, and
// leaves the balancer a quantity it can tune to two percent (two copies of a
// hundred), while a thousand would multiply every layout's cost by ten to buy
// precision nobody's disks need.
//
// It is a member of the fleet's MEAN weight that gets the hundred. Copies
// follow weight, so a member at a quarter of the mean weight gets twenty-five,
// and a count of twenty-five is four times as coarse: one copy is four percent
// of it. That granularity, not the balancer, is what bounds how close
// the balance (internal/placement's Balance) can hold such a member — see
// there. And copies follow weight only where no failure domain is capped: the
// members of a crowded domain get less (see its DefaultTolerance).
const TargetCopiesPerMember = 100

// TargetPGBits is the group bits a map with this many placeable members and
// this many copies per group should have: the smallest count, from
// [MinPGBits] up to [MaxPGBits], that gives a member of the fleet's mean
// weight [TargetCopiesPerMember] copies. Pass the copies the map PLACES
// ([Map.Size]); a count above the members lands on [MinPGBits] whichever is
// passed, since the smallest count already gives each member a copy of every
// group.
//
// It counts MEMBERS, not domains: it sizes for the mean copies per member,
// which is what a member of the mean weight holds wherever no failure domain
// is capped at one copy of each group, and more than the members of a capped
// domain hold (see internal/placement's DefaultTolerance).
//
// It only answers what the count SHOULD be. Moving a map to it is the
// maintainer's decision, one bit per epoch and only on a clean fleet, because
// each bit re-places half the data.
func TargetPGBits(placeable, replicas int) int {
	if placeable < 1 || replicas < 1 {
		return MinPGBits
	}
	for k := MinPGBits; k < MaxPGBits; k++ {
		if (1<<k)*replicas/placeable >= TargetCopiesPerMember {
			return k
		}
	}
	return MaxPGBits
}
