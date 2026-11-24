// Package fleet is the LLM fleet balancer: which member of a fleet of model
// entries takes a seat job's next piece of work.
//
// A fleet is a set of providers.llm entries of one model class (config's
// `llm_fleets`). A seat job set to `fleet:<key>` is not pinned to an entry:
// at the start of every new piece of work (a phase turn, a follow-up, an
// executor launch) the balancer is asked, and it hands the work one member.
// "Fleet" alone already names the engine's node fleet, so this one is always
// the LLM fleet in code.
//
// # One rule
//
// The rule, which the simulation in balance_test.go holds to its promise
// (the mean spread within the threshold):
//
//   - A member's STATE comes from what its sign-in already reported
//     ([Read]): sitting out a vendor's limit or a `reached` window is
//     [Limit], a refused sign-in newer than its last answer is [SignedOut],
//     a session or weekly window from [llm.LimitWarnAt] is [Near], anything
//     else [Available]. A reading older than [llm.LimitStaleAfter] says
//     nothing, so the member is available with its share unknown.
//   - Its SHARE is percent, compared in whole points: a plan's is the higher
//     of its session and weekly use, so plans of different sizes compare by
//     percent. A key, cloud account or gateway has no spend cap the engine
//     knows, so its share is 0 and only a sit-out or a refusal moves it. An
//     unknown share ranks as the pool's lowest known one, so the member
//     serves once and reports.
//   - The POOL takes new work. Sequential: the first available member in
//     list order, alone. Balanced: the available plans, or every available
//     member where none is a plan, so keys are overflow. A member near its
//     limit finishes its running turns and takes nothing new while another
//     member is available; with none available the near members form the
//     pool, by the same rule, rather than hold seats while they have room.
//   - PICK: Sequential returns the pool's head. Balanced returns the least
//     share, ties to the member that served least recently, except that the
//     seat job's HOME (the member that served its last turn) keeps the work
//     while it is in the pool and within [Fleet.Threshold] points of the
//     least, for the harness's cache. A member with every slot taken
//     (max_concurrent) is skipped while another pool member has one free.
//
// # The walk
//
// A call that fails retryably is retried on the next pick, the failed member
// excluded ([Fleet.Walk]). After the pool come the members at their limit,
// earliest room first: a member sitting out refuses without starting a
// process, so a walk that meets every limit fails as a chain every member of
// which is limited (chain.Limited), and the seat parks with one probe at
// the earliest instant any member has room. A signed-out member leaves
// routing at once; it is walked only when nothing else is, so the seat meets
// the sign-in failure a pinned seat would. An empty fleet has no walk: its
// seats are held until a member joins.
//
// # Who has to agree
//
// This node alone. The readings are the node's own (harness.Plans); the
// homes and last-served instants are this node's memory ([Ledger]), lost on
// restart, after which a seat job's next pick is the least-used member. A
// running turn is never moved by balancing, only by a limit or a sign-out.
package fleet

import (
	"cmp"
	"slices"
	"time"
)

// Strategy is how a fleet spreads new work across its members.
type Strategy string

const (
	// Balanced keeps every member at a similar share (the zero value's
	// meaning in config).
	Balanced Strategy = "balanced"
	// Sequential fills the first member, then the next, in list order.
	Sequential Strategy = "sequential"
)

// Strategies is the closed set.
var Strategies = []Strategy{Balanced, Sequential}

// Valid reports whether s is one of [Strategies].
func (s Strategy) Valid() bool { return slices.Contains(Strategies, s) }

// State is where a member stands for new work.
type State string

const (
	// Available takes new work.
	Available State = "available"
	// Near is a session or weekly window from [llm.LimitWarnAt]: its running
	// turns finish on it, and it takes new work only while no member is
	// available.
	Near State = "near"
	// Limit is a member sitting out a vendor's limit, or whose window reads
	// reached: it takes no work until [Member.Until].
	Limit State = "limit"
	// SignedOut is a member whose sign-in the vendor refused more recently
	// than it last answered: it leaves routing at once.
	SignedOut State = "signedout"
)

// States is the closed set, in the order a person reads them.
var States = []State{Available, Near, Limit, SignedOut}

// Valid reports whether s is one of [States].
func (s State) Valid() bool { return slices.Contains(States, s) }

// Reason is why a pick went to its member: the decision record's
// `fleet_reason`.
type Reason string

const (
	// ReasonLowestShare is the pool's least-used member.
	ReasonLowestShare Reason = "lowest_share"
	// ReasonWithinThreshold is the seat job's home, kept within the
	// threshold of the least-used member.
	ReasonWithinThreshold Reason = "within_threshold"
	// ReasonNextInOrder is a Sequential fleet's first member with room.
	ReasonNextInOrder Reason = "next_in_order"
	// ReasonOverflow is a key, cloud account or gateway of a Balanced fleet,
	// taking work because no plan can.
	ReasonOverflow Reason = "overflow"
	// ReasonLimitHandover is a member the walk reached after another failed
	// (a limit, mostly), or one waited on for its limit to lift.
	ReasonLimitHandover Reason = "limit_handover"
)

// Reasons is the closed set.
var Reasons = []Reason{ReasonLowestShare, ReasonWithinThreshold, ReasonNextInOrder,
	ReasonOverflow, ReasonLimitHandover}

// Valid reports whether r is one of [Reasons].
func (r Reason) Valid() bool { return slices.Contains(Reasons, r) }

// Member is one member of a fleet as the balancer reads it at one instant.
type Member struct {
	// Key is the providers.llm entry.
	Key string
	// Plan is whether it signs in on a plan (auth.mode subscription).
	Plan bool
	// State is where it stands, and Until when it has room again for a
	// [Limit] member (zero where unknown).
	State State
	Until time.Time
	// Why is the state in a person's words, empty for an available member.
	Why string
	// Share is its usage share in whole points, meaningful where Known.
	Share int
	Known bool
	// LastServed is when it last took a piece of work on this node.
	LastServed time.Time
	// Full is every one of its slots taken (max_concurrent).
	Full bool
}

// Overflow reports whether m takes work in a fleet of strategy only once no
// plan can: a non-plan member of a Balanced fleet.
func (m Member) Overflow(strategy Strategy) bool { return strategy == Balanced && !m.Plan }

// Fleet is one fleet with its members read at one instant.
type Fleet struct {
	Key      string
	Strategy Strategy
	// Threshold is the Balanced home's allowance in points: config's
	// threshold_points, its default resolved.
	Threshold int
	// Members are in list order, which is a Sequential fleet's order.
	Members []Member
}

// Choice is the member a pick hands the work to, and why.
type Choice struct {
	Member
	Reason Reason
}

// open is who may take new work with excluded left out: the available
// members, else the near ones (see the package doc).
func (f Fleet) open(excluded []string) []Member {
	var avail, near []Member
	for _, m := range f.Members {
		if slices.Contains(excluded, m.Key) {
			continue
		}
		switch m.State {
		case Available:
			avail = append(avail, m)
		case Near:
			near = append(near, m)
		}
	}
	if len(avail) > 0 {
		return avail
	}
	return near
}

// Pool is the members that take new work, excluded left out.
func (f Fleet) Pool(excluded ...string) []Member {
	open := f.open(excluded)
	if len(open) == 0 {
		return nil
	}
	if f.Strategy == Sequential {
		return open[:1]
	}
	plans := slices.DeleteFunc(slices.Clone(open), func(m Member) bool { return !m.Plan })
	if len(plans) > 0 {
		return plans
	}
	return open
}

// Pick is the member that takes a seat job's next piece of work: home is the
// member that served its last turn ("" for none), and excluded the members
// this piece of work already failed on. False where the pool is empty.
func (f Fleet) Pick(home string, excluded ...string) (Choice, bool) {
	pool := f.Pool(excluded...)
	if len(pool) == 0 {
		return Choice{}, false
	}
	if free := slices.DeleteFunc(slices.Clone(pool), func(m Member) bool { return m.Full }); len(free) > 0 {
		pool = free
	}
	choice := f.choose(pool, home)
	switch {
	case len(excluded) > 0:
		choice.Reason = ReasonLimitHandover
	case choice.Overflow(f.Strategy):
		choice.Reason = ReasonOverflow
	}
	return choice, true
}

// choose applies the strategy to a non-empty pool.
func (f Fleet) choose(pool []Member, home string) Choice {
	if f.Strategy == Sequential {
		return Choice{Member: pool[0], Reason: ReasonNextInOrder}
	}
	floor := lowestKnown(pool)
	share := func(m Member) int {
		if m.Known {
			return m.Share
		}
		return floor
	}
	// Stable over list order, so a full tie goes to the first listed.
	least := slices.MinFunc(pool, func(a, b Member) int {
		return cmp.Or(cmp.Compare(share(a), share(b)), a.LastServed.Compare(b.LastServed))
	})
	if i := slices.IndexFunc(pool, func(m Member) bool { return m.Key == home }); i >= 0 &&
		pool[i].Key != least.Key && share(pool[i])-share(least) <= f.Threshold {
		return Choice{Member: pool[i], Reason: ReasonWithinThreshold}
	}
	return Choice{Member: least, Reason: ReasonLowestShare}
}

// lowestKnown is the lowest known share among members, 0 where none is known.
func lowestKnown(members []Member) int {
	floor, found := 0, false
	for _, m := range members {
		if m.Known && (!found || m.Share < floor) {
			floor, found = m.Share, true
		}
	}
	return floor
}

// Walk is the order a piece of work tries the members in: each pick with
// the ones before it excluded, then the members at their limit, earliest
// room first (unknown last), then, only where nothing else is, the signed-out
// ones (see the package doc). Empty for a fleet with no members.
func (f Fleet) Walk(home string) []Choice {
	var (
		out      []Choice
		excluded []string
	)
	for {
		c, ok := f.Pick(home, excluded...)
		if !ok {
			break
		}
		out = append(out, c)
		excluded = append(excluded, c.Key)
	}
	var limited []Member
	for _, m := range f.Members {
		if m.State == Limit {
			limited = append(limited, m)
		}
	}
	slices.SortStableFunc(limited, func(a, b Member) int {
		if a.Until.IsZero() != b.Until.IsZero() {
			return cmp.Compare(boolRank(a.Until.IsZero()), boolRank(b.Until.IsZero()))
		}
		return a.Until.Compare(b.Until)
	})
	for _, m := range limited {
		out = append(out, Choice{Member: m, Reason: ReasonLimitHandover})
	}
	if len(out) > 0 {
		return out
	}
	for _, m := range f.Members {
		if m.State == SignedOut {
			out = append(out, Choice{Member: m, Reason: ReasonLimitHandover})
		}
	}
	return out
}

// FirstRoom is the earliest instant a member at its limit has room again and
// that member's key: what a fleet with no member to take work waits for.
// False where no member names one.
func (f Fleet) FirstRoom() (time.Time, string, bool) {
	var (
		at  time.Time
		key string
	)
	for _, m := range f.Members {
		if m.State == Limit && !m.Until.IsZero() && (at.IsZero() || m.Until.Before(at)) {
			at, key = m.Until, m.Key
		}
	}
	return at, key, !at.IsZero()
}

// Spread is the points between the most and least used members that take
// new work, for a Balanced fleet with two or more known plans (or members,
// where none is a plan) in its pool; false otherwise.
func (f Fleet) Spread() (int, bool) {
	if f.Strategy != Balanced {
		return 0, false
	}
	var shares []int
	for _, m := range f.Pool() {
		if m.Known {
			shares = append(shares, m.Share)
		}
	}
	if len(shares) < 2 {
		return 0, false
	}
	return slices.Max(shares) - slices.Min(shares), true
}

// boolRank orders false before true.
func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}
