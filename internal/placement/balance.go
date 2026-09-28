package placement

import (
	"math"
	"slices"
)

// ShareFor is the share a member of this weight joins the map at: its weight
// at the placeable members' mean share per unit of weight, so a newcomer draws
// at the fleet's own rate and the balance that follows has only its copies to
// settle, rather than a newcomer that started too light or too heavy for a
// fleet whose rate had moved (the share of a member taken out or on
// probation, say, no longer counting). [DefaultShare] in a draw with nothing
// placeable.
func (d Draw) ShareFor(weight int) uint32 {
	var shares, weights uint64
	for _, member := range d.Members {
		if member.Placeable() {
			shares += uint64(member.Share)
			weights += uint64(member.Weight)
		}
	}
	if weights == 0 {
		return DefaultShare(weight)
	}
	return quantise(float64(shares*uint64(weight)) / float64(weights))
}

// Balancing defaults. Both are what [BalanceOptions] takes when a field is
// zero or negative.
const (
	// DefaultTolerance is the relative deviation a balanced map may keep:
	// every member within two percent of the copies its weight entitles it
	// to, which is finer than any two disks' free space is ever matched.
	//
	// A tolerance is a number of copies of each target, and [Balance]
	// promises it only where it is at least a copy and a half: at two
	// percent, targets of seventy-five copies and more. That is three
	// quarters of what the object map's group count
	// (objstore/placement.TargetPGBits) gives a member of the fleet's mean
	// weight, so every member down to three quarters of the mean weight —
	// in a fleet no failure domain caps. A domain holds at most one copy of
	// each group, so the members of one carrying more than its part of the
	// weight divide only the groups between them and are entitled to less
	// than their weight: equal members in zones of one, one and ten at
	// three copies target 51.2 copies each over the 512 groups the object
	// map picks for them, outside the promise however equal they are.
	DefaultTolerance = 0.02

	// DefaultMaxRounds bounds the layouts one balance computes. SIXTY: over
	// a four-thousand-fleet corpus every fleet within the promise [Balance]
	// makes converged in at most 32 rounds, and of the 557 just short of it
	// — a tolerance of one copy to a copy and a half — all but five within
	// sixty. A round is a whole layout: at two hundred members over 8192
	// groups the draws take 190 ms and each round 28 ms after them, so a
	// balance there that converges takes about half a second (thirteen
	// rounds for an equal fleet) and one that cannot is bounded at 1.8 s.
	// It is paid by whichever node writes the change — the map's maintainer
	// on its tick, and the node serving an operator's out or in, once per
	// compare-and-set it attempts — never by a node reading the map. At
	// fifty members over 2048 groups, 25 ms and 120 ms.
	DefaultMaxRounds = 60
)

// A balance corrects each share by a step of its own length, adapted round by
// round like Rprop's: it grows by stepGrowth while the error it corrects keeps
// its sign, and shrinks by stepShrink when a correction overshoots — never
// below minStep or above maxStep. A failure domain carries a step of its own
// in the same way (see [balancer.adjust]).
//
// WHY A STEP PER MEMBER, ADAPTED: how far a member's copy count moves for a
// given change of its share differs by an order of magnitude across one fleet.
// A member holding a few percent of the groups answers about in proportion;
// one holding most of them hardly at all, since it can only gain the groups it
// does not already hold; and two members contending for the same groups answer
// each other twice over. The rule this replaced raised every correction to one
// fleet-wide exponent, 0.6, and shortened it for the whole fleet whenever the
// worst member failed to improve — which undershot the first kind, overshot
// the third, and let whichever member was worst shrink everyone's step until
// nothing moved. Measured: nine members of weight 1 and one of weight 4 at
// three copies over 256 groups stopped at 5.6%, the heavy member thirteen
// copies short on a step of 0.0001, and 106 of a 228-fleet corpus ended
// outside the tolerance, where all 228 converge now.
//
// The values were chosen over a thousand-fleet corpus (one to ten copies,
// weights from one to sixty-four, failure domains honoured, too few and
// absent, shares at the defaults and thirty percent off), by the rounds its
// 604 fleets with a copy of tolerance or more took to converge — a mean of
// 6.97 and at most 37 with these:
const (
	// firstStep is where every step starts. 0.7: a full step measured the
	// same (a mean of 6.91 rounds, the slowest 39) and 0.5 slower (7.78).
	// Of the two, the shorter errs toward undershooting, which the next
	// round's growth recovers, where an overshoot costs a halving.
	firstStep = 0.7

	// stepGrowth: 1.2, the lowest mean — 7.05 rounds at 1.15, 7.31 at 1.3
	// and 7.73 at 1.5, where a step that grows fast has overshot by the time
	// its error turns round, and the slowest fleet took 79 rounds.
	stepGrowth = 1.2

	// stepShrink: a half. At 0.7 an oscillation damps more slowly (a mean of
	// 7.96 rounds, the slowest 45); at 0.35 a step shrinks past what the
	// next error needs (the slowest 58, and 103 against 69 among the fleets
	// finer than a copy).
	stepShrink = 0.5

	// minStep: 0.05. An endgame moves single copies, which needs short
	// steps: at a floor of 0.2 two of the 604 fleets never converged and
	// one took 115 rounds. A floor of 0.02 bought nothing (the slowest 52).
	minStep = 0.05

	// maxStep: 8, a guard against a step growing without end rather than
	// a tuning — 4 and 16 measured the same, since the ceiling term of a
	// correction (see [balancer.adjust]) already pushes a member that holds
	// nearly every group as hard as it needs.
	maxStep = 8
)

// half is what a count of zero is measured as wherever a correction takes a
// logarithm of it: half a copy, so a member or domain the layout gave nothing
// is pulled hard but finitely.
const half = 0.5

// logBudget bounds the logarithms [Balance] computes once and re-divides every
// round rather than re-drawing: 2^22 of them, 32 MiB — 256 members at 16384
// groups, the count the object map gives that fleet. The node balancing may be
// any node — the maintainer's duty moves, and an operator's out or in is
// balanced by whichever node serves it — including a small one, so a larger
// fleet draws afresh each round instead: slower, not wrong.
const logBudget = 1 << 22

// BalanceOptions tune [Balance]. The zero value takes both defaults.
type BalanceOptions struct {
	// Tolerance is the largest relative deviation a member's copy count
	// may keep from its target. Zero or negative takes
	// [DefaultTolerance].
	Tolerance float64

	// MaxRounds bounds the layouts computed. Zero or negative takes
	// [DefaultMaxRounds].
	MaxRounds int
}

// BalanceReport is what a balance achieved.
type BalanceReport struct {
	// Rounds is how many layouts it measured.
	Rounds int `json:"rounds"`

	// Deviation is the returned map's largest relative difference between
	// a member's copies and its target: 0.02 is two percent.
	Deviation float64 `json:"deviation"`

	// Converged is whether that is within the tolerance.
	Converged bool `json:"converged"`
}

// Balance sets the members' shares so each holds copies in proportion to its
// weight, and answers the draw with those shares and what it achieved.
//
// WHAT PROPORTIONAL MEANS. A group has [Draw.Size] copies, so the map holds
// Groups × Size of them, and a member's target is its weight's part of that
// total within the bounds any layout keeps to: a member holds at most one copy
// of a group, and a failure domain holds at most one copy of each group — or,
// in a fleet with fewer domains than copies, at least one (see targets). A
// member's weight is therefore a promise about its fraction of the data, which
// straw2 alone keeps only for the first copy.
//
// WHAT IT REACHES. A copy count is an integer, so a tolerance is only as fine
// as a copy of the target it is taken of, and it can promise only what the
// count can hold. Whenever the tolerance is at least a copy and a half of
// every placeable member's target — at [DefaultTolerance], every target at
// least seventy-five copies — the balance reaches it: every such fleet of a
// four-thousand-fleet corpus did (1651 of them: one to ten copies, weights
// from one to sixty-four, failure domains honoured, too few and absent,
// shares at the defaults and thirty percent off), in a median of five rounds
// and at most 32, and TestBalanceReachesTheToleranceWhereverACopyAndAHalfFitsInIt
// holds a sample of them to it. Between one copy and a copy and a half every
// member must land on one of two or three counts at once, and the balance
// nearly always gets there — 552 of 557 fleets within [DefaultMaxRounds], the
// rest within 210 — but it is not promised. Finer than a copy, a member's
// tolerance may hold no count at all — within two percent of 24.5 copies lies
// neither 24 nor 25 — and the corpus's fleets there ended unconverged one time
// in twenty between half a copy and a copy (32 of 694), and more often than
// not below half a copy (605 of 1098). The balance keeps trying for MaxRounds
// and answers the closest round it measured. What makes a target small is a
// small weight over a small group count, so the cure for such a fleet is more
// groups (the object map's objstore/placement.TargetPGBits), not more rounds.
// The corpus was drawn over the object map's groups; what it measured is this
// balance, whatever a map's groups are.
//
// HOW. From the members' CURRENT shares — so a small change to the fleet moves
// little — each round lays the map out, counts the copies, and corrects the
// shares in two parts (see [balancer.adjust]): each failure domain's copies
// toward its target, measured by where they sit between the fewest and the
// most the domain can hold, and each member's part of its domain's copies
// toward its part of the domain's target — a member of no domain being a
// domain of its own. Each correction has its own adaptive step (firstStep),
// and a member within the tolerance and within a copy of its target is left
// alone, since a copy is all a count can be corrected by. The shares are then
// renormalised so the mean share per unit of weight is 1.0, which keeps them
// comparable across balances and leaves [Draw.ShareFor] a meaningful starting
// point for a member joining later. It stops within tolerance, after
// MaxRounds, or when no share moves, and answers the BEST round measured,
// never merely the last: near the end a round measures a copy or two of noise
// per member either way.
//
// DETERMINISTIC: the same draw and options always answer the same shares, since
// nothing here reads a clock, a random source or a Go map's order.
//
// FLOATING POINT IS FINE HERE, as in everything computed once and STORED
// ([Draw.ShareFor] too), and in nothing a node places by: the result is
// written into the map, and every other node places by the stored integers
// rather than recomputing them — so two CPUs disagreeing about a last bit in
// here can never place one group two ways.
//
// Only shares change; the epoch is the caller's to move. A draw with nothing
// placeable is answered as it was.
func Balance(m Draw, opts BalanceOptions) (Draw, BalanceReport) {
	tolerance := opts.Tolerance
	if tolerance <= 0 {
		tolerance = DefaultTolerance
	}
	rounds := opts.MaxRounds
	if rounds <= 0 {
		rounds = DefaultMaxRounds
	}

	out := m
	out.Members = slices.Clone(m.Members)
	d := newDrawer(out)
	if d.size == 0 {
		return out, BalanceReport{Converged: true}
	}
	b := newBalancer(d, tolerance)
	groups := m.Groups.Count()
	if groups*len(d.placeable) <= logBudget {
		d.logs = make([]int64, groups*len(d.placeable))
		for pg := range groups {
			seed := m.Groups.Seed(pg)
			for j, i := range d.placeable {
				d.logs[pg*len(d.placeable)+j] = logDraw(seed, d.keys[i])
			}
		}
	}

	copies := make([]int, len(out.Members))
	best := slices.Clone(d.shares)
	report := BalanceReport{Deviation: math.Inf(1)}
	for round := 1; ; round++ {
		clear(copies)
		for pg := range groups {
			for _, i := range d.choose(pg) {
				copies[i]++
			}
		}
		report.Rounds = round
		// STRICTLY better: a round that only ties the best moved shares
		// for nothing, and the earlier one moves less data.
		if dev := deviation(copies, b.want.member); dev < report.Deviation {
			report.Deviation = dev
			copy(best, d.shares)
		}
		if report.Deviation <= tolerance || round == rounds {
			break
		}
		if !b.adjust(copies) {
			// Every correction was too short to move a share by one
			// unit: another round would lay out the same map again.
			break
		}
	}
	for i := range out.Members {
		out.Members[i].Share = best[i]
	}
	report.Converged = report.Deviation <= tolerance
	return out, report
}

// balancer is one balance's state between rounds.
type balancer struct {
	d         *drawer
	want      entitlement
	tolerance float64

	// logShare is every placeable member's share as a logarithm, the
	// quantity the corrections add to — so a step is a ratio whatever the
	// share's size — kept unrounded between rounds so that a correction
	// smaller than a unit of the stored share still accumulates.
	logShare []float64

	// step is per member: the step of its part within its domain. domain
	// is per domain index: the step of the domain's total. held, move and
	// settled are per domain index, for the round being corrected: its
	// copies, the move its total takes, and whether it is left alone.
	step, domain []stride
	held, move   []float64
	settled      []bool
}

// stride is one adaptive step length and the error it last corrected.
type stride struct {
	length float64
	last   float64
}

// toward is the step length to correct err by, adapting it first: longer if
// err has the sign of the last error corrected — the last step fell short —
// and shorter if it has flipped — the last step overshot. An err of zero is
// no correction, and leaves the next one nothing to compare against.
func (s *stride) toward(err float64) float64 {
	switch {
	case err*s.last > 0:
		s.length = min(s.length*stepGrowth, maxStep)
	case err*s.last < 0:
		s.length = max(s.length*stepShrink, minStep)
	}
	s.last = err
	return s.length
}

func newBalancer(d *drawer, tolerance float64) *balancer {
	b := &balancer{
		d:         d,
		want:      entitle(d),
		tolerance: tolerance,
		logShare:  make([]float64, len(d.m.Members)),
		step:      make([]stride, len(d.m.Members)),
		domain:    make([]stride, d.nDom),
		held:      make([]float64, d.nDom),
		move:      make([]float64, d.nDom),
		settled:   make([]bool, d.nDom),
	}
	for i := range b.step {
		b.step[i].length = firstStep
		b.logShare[i] = math.Log(float64(d.shares[i]))
	}
	for u := range b.domain {
		b.domain[u].length = firstStep
	}
	return b
}

// isSettled reports whether a count this far from its target is left alone: it
// is within the tolerance, and at one of the two counts either side of the
// target (strictly within a copy of it). Within the tolerance because that is
// the goal; within a copy as well because a member left resting at the edge of
// a wide tolerance is one neighbour's correction away from falling out of it,
// while a copy is the least a correction can move anyway: resting anywhere in
// the tolerance, the corpus behind the step constants took a fifth more rounds
// on average, and four fleets with a copy and a half of tolerance took more
// than sixty. STRICTLY within a copy: resting a whole copy off as well — three
// counts of an integer target rather than two — was slower too (a mean of
// 7.11 rounds against 6.97, the slowest 49 against 37).
func (b *balancer) isSettled(err, target float64) bool {
	return math.Abs(err) <= b.tolerance*target && math.Abs(err) < 1
}

// adjust corrects every placeable share from one round's copies, renormalises
// the fleet to a mean of 1.0 per unit of weight, and reports whether any
// stored share moved.
//
// IN TWO PARTS, because the bounds a layout keeps to are on DOMAINS (a member
// of no domain being one of its own). First a domain's total moves by
//
//	log(target / copies) + log((hi − copies) / (hi − target)) / size
//
// where hi is the most copies the domain can hold. The first term is the
// plain ratio. The second is what a domain near its ceiling needs, since there
// it answers a change of share hardly at all: a domain holding all but a few
// groups misses one only when Size others outrank it, so its misses fall as
// the Size-th power of its share, which is what the division is. Without it a
// heavy member a few copies short of every group crept up by a fraction of a
// percent a round — the corpus behind the step constants took 29% more rounds
// on average, and its slowest fleet with two copies of tolerance 42 rather
// than 23. The FLOOR of a fleet with fewer domains than copies needs no term
// of its own: a domain there sheds its extra copies at about the first power
// of its share, which the plain ratio already is, and measured, a term for it
// changed nothing (a mean of 6.89 rounds against 6.97).
//
// Then each member of a domain with more than one moves by the ratio of its
// part of the domain's target to its part of the domain's copies — while the
// domain's own move is still correcting the total, so the two never pull
// against each other — and by the ratio of its own target to its copies once
// the domain has settled. Its own, then, because that is what the tolerance
// is measured against: its part of a settled domain's copies differs from it
// by up to a copy, which is enough to leave a member outside its tolerance
// with nothing correcting it, and the balance would stop with no share moving
// and the tolerance in reach — four of the corpus's thousand fleets did, and
// all four converge now.
//
// The domain's move is common to its members, so it cannot reorder them — and
// it takes the DOMAIN'S step, never each member's: with each member's own, a
// member holding a surplus in a domain short of its ceiling was pushed up with
// its domain, its error kept its sign and its step grew, and 75 of the
// corpus's 462 fleets with a copy and a half of tolerance never converged.
//
// Every correction has its own step, and a member or a domain that has settled
// is left alone.
func (b *balancer) adjust(copies []int) bool {
	d := b.d
	clear(b.held)
	for _, i := range d.placeable {
		b.held[d.domain[i]] += float64(copies[i])
	}
	clear(b.move)
	for u, want := range b.want.domain {
		if want == 0 {
			continue
		}
		have := b.held[u]
		err := want - have
		if b.settled[u] = b.isSettled(err, want); b.settled[u] {
			b.domain[u].toward(0)
			continue
		}
		hi := b.want.hi[u]
		ratio := math.Log(want / max(have, half))
		ceiling := math.Log(max(hi-have, half)/max(hi-want, half)) / float64(d.size)
		b.move[u] = b.domain[u].toward(err) * (ratio + ceiling)
	}

	var sum, weights float64
	for _, i := range d.placeable {
		u := d.domain[i]
		b.logShare[i] += b.move[u]
		if b.want.count[u] > 1 {
			want := b.want.member[i]
			aim := want
			if !b.settled[u] {
				aim = want * b.held[u] / b.want.domain[u]
			}
			err := aim - float64(copies[i])
			if b.isSettled(err, want) {
				b.step[i].toward(0)
			} else {
				ratio := math.Log(max(aim, half) / max(float64(copies[i]), half))
				b.logShare[i] += b.step[i].toward(err) * ratio
			}
		}
		sum += math.Exp(b.logShare[i])
		weights += float64(d.m.Members[i].Weight)
	}
	shift := math.Log(shareOne * weights / sum)
	moved := false
	for _, i := range d.placeable {
		// Held within what a share can store, so the state never runs
		// ahead of the stored share it stands for.
		b.logShare[i] = min(max(b.logShare[i]+shift, 0), math.Log(math.MaxUint32))
		share := quantise(math.Exp(b.logShare[i]))
		moved = moved || share != d.shares[i]
		d.shares[i] = share
	}
	return moved
}

// quantise is a share as the map stores it: rounded, and never zero, which
// every draw divides by.
func quantise(share float64) uint32 {
	return uint32(math.Min(math.Max(math.Round(share), 1), math.MaxUint32))
}

// entitlement is what a balance aims at, and the bounds it aims within.
type entitlement struct {
	// member is per member: the copies its weight entitles it to, zero
	// for one that takes no copies.
	member []float64

	// domain, lo, hi and count are per domain index (the drawer's, in
	// which a member with no domain is one of its own): the copies its
	// members are entitled to together, the fewest and the most any
	// layout gives it, and how many placeable members it has.
	domain, lo, hi []float64
	count          []int
}

// targets is the copies each member's weight entitles it to, by member index:
// zero for one that takes no copies. See entitle.
func targets(d *drawer) []float64 { return entitle(d).member }

// entitle is what each member and each domain is entitled to.
//
// PROPORTIONAL TO WEIGHT, WITHIN WHAT A LAYOUT CAN HOLD. Every member holds at
// most one copy of a group, and its domain bounds what its members hold
// together, in one of two ways:
//
//   - With as many domains as copies — always, when the map names no
//     failure domain and every member is a domain of its own — a group holds
//     at most one copy per domain, so a domain holds at most one copy of each
//     group.
//   - With fewer ([Draw.DomainLimited]), placement puts one copy in every
//     domain before it puts a second in any, so a domain holds AT LEAST one
//     copy of each group — and at most one more for each copy the domains
//     fall short by, and never more than one per member. A lone node in its
//     own zone of a two-zone fleet at three copies therefore holds every
//     group whatever its weight says. A target below that was one no share
//     could reach, which the targets this replaced set: the balance spent
//     every round on it and reported forty percent for a layout as even as
//     the fleet allowed.
//
// The domains are filled in proportion to their weight within their bounds,
// then each domain's copies split among its members in proportion within
// theirs.
func entitle(d *drawer) entitlement {
	e := entitlement{
		member: make([]float64, len(d.m.Members)),
		domain: make([]float64, d.nDom),
		lo:     make([]float64, d.nDom),
		hi:     make([]float64, d.nDom),
		count:  make([]int, d.nDom),
	}
	if d.size == 0 {
		return e
	}
	groups := float64(d.m.Groups.Count())
	weight := make([]float64, d.nDom)
	members := make([][]int, d.nDom)
	for _, i := range d.placeable {
		u := d.domain[i]
		weight[u] += float64(d.m.Members[i].Weight)
		members[u] = append(members[u], i)
		e.count[u]++
	}
	limited := d.distinct < d.size
	for u, n := range e.count {
		e.hi[u] = groups
		if limited && n > 0 {
			e.lo[u] = groups
			e.hi[u] = groups * float64(min(n, 1+d.size-d.distinct))
		}
	}
	e.domain = fill(weight, e.lo, e.hi, groups*float64(d.size))
	for u, in := range members {
		if len(in) == 0 {
			continue
		}
		w := make([]float64, len(in))
		for k, i := range in {
			w[k] = float64(d.m.Members[i].Weight)
		}
		split := fill(w, make([]float64, len(in)), slices.Repeat([]float64{groups}, len(in)), e.domain[u])
		for k, i := range in {
			e.member[i] = split[k]
		}
	}
	return e
}

// fill divides total among the entries in proportion to weight, each held
// within [lo, hi]: every entry is clamp(λ × weight, lo, hi) for the one scale
// λ at which they sum to total. An entry of no weight is held at lo.
//
// BY THE BREAKPOINTS rather than by holding whoever overflows and sharing the
// excess again: with bounds on both sides, the entries a scale pushes past one
// bound move the scale enough to push others past the other, and that repair
// needs rounds nobody has counted. The sum is piecewise linear and never
// decreasing in λ, bending only where an entry reaches a bound — so the
// answer lies between the two bends the total falls between, and there it is
// a straight line. The caller guarantees the bounds admit the total.
func fill(weight, lo, hi []float64, total float64) []float64 {
	at := func(scale float64) float64 {
		var sum float64
		for u, w := range weight {
			sum += min(max(scale*w, lo[u]), hi[u])
		}
		return sum
	}
	var bends []float64
	for u, w := range weight {
		if w > 0 {
			bends = append(bends, lo[u]/w, hi[u]/w)
		}
	}
	slices.Sort(bends)
	k, _ := slices.BinarySearchFunc(bends, total, func(bend, total float64) int {
		if at(bend) < total {
			return -1
		}
		return 1
	})
	var scale float64
	switch {
	case len(bends) == 0:
	case k == 0:
		scale = bends[0]
	case k == len(bends):
		scale = bends[k-1]
	default:
		a, b := bends[k-1], bends[k]
		scale = b
		if fa, fb := at(a), at(b); fb > fa {
			scale = a + (total-fa)*(b-a)/(fb-fa)
		}
	}
	out := make([]float64, len(weight))
	for u, w := range weight {
		out[u] = min(max(scale*w, lo[u]), hi[u])
	}
	return out
}
