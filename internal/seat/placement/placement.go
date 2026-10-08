// Package placement is the vocabulary a node's declaration and a seat's
// constraint share: what a process is willing to do (node.roles, node.labels)
// and where a seat is allowed to run (role.placement).
//
// It imports nothing from the engine but coord, so the config layer that
// PARSES this vocabulary and the seat host that DECIDES with it can both
// depend on it. Two layers that disagree about a name never raise — they
// quietly do different things, which is the same reason the topic grammar
// lives in exactly one place.
//
// Everything here is a pure function over plain data. No I/O, no goroutines,
// no clock: the seat host is testable precisely because the arithmetic it
// runs every sweep can be exercised without a store.
//
// # The capacity math is the part that is easy to get wrong
//
// Placement is not a filter you bolt onto a fair share computed over the
// whole fleet. Nine seats pinned to one node and one seat free, across three
// nodes: the fleet-wide share is ceil(10/3) = 4, so the pinned node claims
// four of its nine and the remaining five are claimable by nobody — stranded
// forever, while every node in the fleet reports a perfectly healthy sweep.
//
// The share is therefore computed per placement GROUP, over the nodes
// eligible for that group. A fleet with no placements anywhere collapses to
// one group and the plain ceil(seats / nodes): the unconstrained company is
// the degenerate case, not a second code path. See [Compute].
//
// # A share bounds its own group and nothing else
//
// Summing the per-group shares into one number a node may fill with any
// seat it is eligible for brings the stranding back by another door. A
// satellite labelled for one pinned seat also matches the unpinned group, so
// its capacity is 1 (the pinned seat) + 1 (its third of three unpinned
// seats) = 2. Fill those two with unpinned seats — they sort first — and it
// is at capacity with the pinned seat unclaimed, while no other node may run
// that seat and every node's arithmetic says the fleet has room for it: no
// sweep claims it and nothing is reported unplaceable.
//
// So a node holds at most its share OF EACH GROUP, and capacity is not
// fungible across groups: [Plan.Room] is per group, and the host claims and
// sheds against it. Every group is then covered on its own terms — its
// eligible nodes' shares sum to at least its size, and nothing outside the
// group can occupy them — which is also what lets [Plan.Unplaceable] be read
// off the same arithmetic: a group is unplaceable exactly when its eligible
// nodes' shares cannot reach its size, and the only way that happens is when
// it has none.
package placement

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/coord"
)

// NodeRole is what a process is willing to do for the company. A node
// declares a subset; declaring nothing means all of them, which is the
// single-process deployment and the shape every config had before a fleet
// was possible.
type NodeRole string

const (
	// RoleData keeps the company's durable state on this node's own disk: a
	// copy of the replicated estate and the node's own database. A node
	// WITHOUT it keeps nothing that has to outlive it: its store is scratch,
	// and it reads and writes the company's tracker and knowledge base
	// through a data node. It is what a small, disposable agent node runs.
	//
	// IT SAYS NOTHING ABOUT THE BROKER. Whether this node's broker is a
	// member of the fleet's JetStream cluster, a leaf of it or a client of
	// an external one is its [BrokerKind], derived from the stream block;
	// a question about who holds broker state asks that, never this.
	RoleData NodeRole = "data"

	// RoleIngress terminates inbound traffic: the HTTP API, the dashboard,
	// and the webhooks every integration posts to. A fleet with none of
	// these still runs its agents and never hears from the outside world.
	RoleIngress NodeRole = "ingress"

	// RoleSeats runs agents: claims seat leases, spawns the instances,
	// consumes the inboxes, executes turns. It is the only role this
	// package's arithmetic counts.
	RoleSeats NodeRole = "seats"

	// RoleWorkers runs the company-wide singleton duties behind
	// worker:{duty} leases — the scheduler tick, the retention sweep, the
	// sandbox waiter, skill clustering and curation, seat-subscription
	// creation. Exactly one node does each at a time; a fleet with none of
	// these does none of them.
	RoleWorkers NodeRole = "workers"
)

// allRoles is the vocabulary, in the order a profile is written to the wire.
// Alphabetical, so two nodes describing the same role set produce byte-equal
// meta.
var allRoles = []NodeRole{RoleData, RoleIngress, RoleSeats, RoleWorkers}

// Vocabulary is every role, in wire order — a fresh slice per call.
//
// EXPORTED SO NOTHING WRITES THE LIST AGAIN. The fleet's unmanned-role check
// walked a copy of its own, and a copy is how a fourth role reaches every
// node's config and no node's warning about nobody holding it.
func Vocabulary() []NodeRole { return slices.Clone(allRoles) }

// ErrUnknownRole reports a role name that is not in the vocabulary. Config
// loading wraps it; nothing branches on it beyond refusing to boot.
var ErrUnknownRole = errors.New("unknown node role")

// RoleSet is the set of roles a node has declared.
//
// THE NIL SET MEANS EVERY ROLE, not none — the one place this package
// deliberately breaks Go's "a nil map reads as empty". A node that declared
// nothing does everything, and the alternative reading is an incident: a
// peer whose row this build cannot read, or a profile built by a caller that
// never set this, would drop out of the seat denominator and every other node
// would compute too large a share of the seats. "Declared nothing" and "does
// nothing" must never be the same answer.
//
// An empty non-nil set reads the same way for the same reason, and is also
// not expressible on the wire: a "roles": [] row round-trips to every role.
// Treat a RoleSet as immutable once it is in a profile.
type RoleSet map[NodeRole]struct{}

// DefaultRoles returns the every-role set an undeclared node resolves to.
// It is a function, not a package var, because a shared mutable set would be
// one careless insert away from redefining the default fleet-wide.
func DefaultRoles() RoleSet {
	out := make(RoleSet, len(allRoles))
	for _, r := range allRoles {
		out[r] = struct{}{}
	}
	return out
}

// Roles builds a set from explicit roles. Passing none yields the nil set,
// which reads as every role.
func Roles(roles ...NodeRole) RoleSet {
	if len(roles) == 0 {
		return nil
	}
	out := make(RoleSet, len(roles))
	for _, r := range roles {
		out[r] = struct{}{}
	}
	return out
}

// Has reports whether the node performs this role, resolving the unset set
// to every role.
func (s RoleSet) Has(role NodeRole) bool {
	if len(s) == 0 {
		return true
	}
	_, ok := s[role]
	return ok
}

// Names returns the declared roles as sorted strings — the wire form, and
// what a log line should carry.
func (s RoleSet) Names() []string {
	if len(s) == 0 {
		s = DefaultRoles()
	}
	// Every member, including one outside the vocabulary that a caller
	// built by hand: a profile that logs differently from how it behaves is
	// worse than one that logs something odd.
	out := make([]string, 0, len(s))
	for r := range s {
		out = append(out, string(r))
	}
	slices.Sort(out)
	return out
}

// Equal compares two sets by what they MEAN, so the unset set equals an
// explicit every-role set.
func (s RoleSet) Equal(other RoleSet) bool {
	return slices.Equal(s.Names(), other.Names())
}

// String renders the set for logs.
func (s RoleSet) String() string { return strings.Join(s.Names(), ",") }

// ParseRoles coerces role names from config, defaulting to every role.
//
// This half of the vocabulary fails CLOSED: an unknown name is an error
// rather than a skipped entry, because a typo'd role in a bootstrap file
// would otherwise silently subtract a duty from the fleet — nobody serving
// ingress, or nobody running the scheduler, with no single node's config
// looking wrong. Reading the same vocabulary back off a PEER's presence row
// fails open, for the opposite reason; see [FromMeta].
func ParseRoles(values []string) (RoleSet, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := make(RoleSet, len(values))
	for _, v := range values {
		role := NodeRole(v)
		if !slices.Contains(allRoles, role) {
			return nil, fmt.Errorf("%w: %q", ErrUnknownRole, v)
		}
		out[role] = struct{}{}
	}
	return out, nil
}

// SeatPlacement is where a seat may run. The zero value is "anywhere", which
// is what a role with no placement key means.
//
// Node pins to one node id; Labels requires every pair to be present and
// equal on the node. Give both and both must hold — the conditions are
// ANDed, so a placement can only ever narrow.
type SeatPlacement struct {
	// Node is an exact node id, or empty for no pin.
	Node string
	// Labels are required node labels, compared exactly. Treat as
	// immutable.
	Labels map[string]string
}

// IsAnywhere reports whether the placement constrains nothing.
func (p SeatPlacement) IsAnywhere() bool { return p.Node == "" && len(p.Labels) == 0 }

// Matches reports whether a node with these labels may run the seat.
//
// A required label must be PRESENT and equal, never merely equal to the
// zero value a missing key reads as: a selector asking for an empty value
// would otherwise match every node that has never heard of the key.
func (p SeatPlacement) Matches(nodeID string, labels map[string]string) bool {
	if p.Node != "" && p.Node != nodeID {
		return false
	}
	for k, want := range p.Labels {
		got, ok := labels[k]
		if !ok || got != want {
			return false
		}
	}
	return true
}

// Key is the group identity: seats with the same key share one fair share.
// Quoting each half keeps the encoding injective, so two different
// placements cannot collide into one group and halve each other's share.
func (p SeatPlacement) Key() string {
	var b strings.Builder
	b.WriteString(strconv.Quote(p.Node))
	for _, k := range slices.Sorted(maps.Keys(p.Labels)) {
		b.WriteByte('\n')
		b.WriteString(strconv.Quote(k))
		b.WriteByte('=')
		b.WriteString(strconv.Quote(p.Labels[k]))
	}
	return b.String()
}

// String renders the constraint for an operator: this is what the host puts
// in seats_unplaceable, and it is the whole diagnosis when a seat is served
// by nobody.
func (p SeatPlacement) String() string {
	var parts []string
	if p.Node != "" {
		parts = append(parts, "node="+p.Node)
	}
	if len(p.Labels) > 0 {
		pairs := make([]string, 0, len(p.Labels))
		for _, k := range slices.Sorted(maps.Keys(p.Labels)) {
			pairs = append(pairs, k+"="+p.Labels[k])
		}
		parts = append(parts, "labels="+strings.Join(pairs, ","))
	}
	if len(parts) == 0 {
		return "anywhere"
	}
	return strings.Join(parts, " ")
}

// NodeProfile is a live node as its peers see it: what it does and how it is
// labelled.
//
// It rides on the node's own presence lease (node:{id}) so every peer can
// answer "who is eligible for this seat" with no membership service. A node
// that is PRESENT but does not run seats must not count in the seat
// denominator — it would shrink everyone's share and strand the difference.
//
// The zero value is a node with no id that does everything and carries no
// labels; see [RoleSet] for why an unset role set is every role rather than
// none.
//
// NOTHING ABOUT THE OBJECT STORE rides here, deliberately. A data node's share
// of it was once a field of this profile; the store is now one the whole fleet
// shares — the broker's own bucket or an S3 bucket — so no node holds a share
// of it to advertise.
//
// THE BROKER DOES, because it is a fact about the PROCESS rather than about a
// disk: the broker starts and stops with the node, so the lease that says the
// node is running is the one that says what its broker is.
type NodeProfile struct {
	ID     string
	Roles  RoleSet
	Labels map[string]string

	// Held is how many seat leases the node says it holds — the seats it
	// runs and the ones whose teardown it could not prove — as of its last
	// presence renewal (see [HeldKey]). A count the row does not carry
	// readably reads as zero, which errs toward trying: the sweep claims
	// nothing on the counts, so a low sum costs a pass, never a seat.
	Held int

	// Broker is how this node's broker takes part in the fleet's. Read off
	// a peer's row, [BrokerUnknown] means the row did not say — see
	// [BrokerKind] for why that is never read as a leaf.
	Broker BrokerKind
}

// HeldKey is where a presence row carries [NodeProfile.Held].
//
// ON THE PRESENCE ROW because it is the one read every sweep already makes:
// a node with room for one more seat and nothing free — the steady state of
// any fleet whose seats do not divide evenly — used to learn that nothing was
// free by trying every seat it may run, a leader read each, every five
// seconds. The fleet's own counts answer it from the listing the sweep has
// already taken. Written by the seat host, beside the profile rather than in
// it, because it is live state and the profile is configuration.
const HeldKey = "seats_held"

// RunsSeats reports whether this node claims seats at all. It is the
// denominator test.
func (n NodeProfile) RunsSeats() bool { return n.Roles.Has(RoleSeats) }

// RunsWorkers reports whether this node runs the company-wide singleton
// duties.
func (n NodeProfile) RunsWorkers() bool { return n.Roles.Has(RoleWorkers) }

// RunsIngress reports whether this node serves inbound traffic.
func (n NodeProfile) RunsIngress() bool { return n.Roles.Has(RoleIngress) }

// HoldsData reports whether this node holds the company's durable state —
// a copy of the replicated estate on its own disk. A node that does not is
// stateless: see [RoleData]. It says nothing about the broker, which is
// [NodeProfile.Broker].
//
// READ OFF A PEER'S ROW, it fails the way every role read does — an unknown
// or unreadable set is every role — which is the safe reading for most
// questions and a NARROW one here: a peer whose row cannot be read is
// counted as holding data, so the trim may wait on a position that node does
// not report. The alternative reading would trim past a member's rows, which
// is the one thing a trim must never do.
func (n NodeProfile) HoldsData() bool { return n.Roles.Has(RoleData) }

// Meta is the lease payload for this node's presence row, in the shape
// [coord.AcquireOptions].Meta takes. Roles are written resolved and sorted,
// so a reader never has to know what this build's default was.
//
// THE BROKER ONLY WHEN IT IS KNOWN. Its absence is the one way to say
// [BrokerUnknown] — an empty string on the wire would be a value a reader had
// to know means nothing — and a profile nobody derived a kind for (a test's,
// a caller that built one by hand) must not claim one.
func (n NodeProfile) Meta() map[string]any {
	meta := map[string]any{
		"roles":  n.Roles.Names(),
		"labels": maps.Clone(n.Labels),
	}
	if n.Broker.Valid() {
		meta["broker"] = string(n.Broker)
	}
	return meta
}

// FromMeta reads a peer's profile back off its presence lease.
//
// It returns no error, deliberately. Every malformed shape has exactly one
// correct reading — a node that does everything and is labelled with
// nothing — and a peer's bad row must not take down the reader's sweep. An
// error return would create a branch whose only safe body is "use that
// reading anyway", and the tempting wrong body (skip the peer) is the
// incident: a live seat-running node missing from the denominator makes every
// other node claim more than its share.
//
// The broker follows the same rule with its own safe reading: anything this
// build cannot read is [BrokerUnknown], which a question counting members
// treats as one, and never a leaf.
func FromMeta(nodeID string, meta map[string]any) NodeProfile {
	return NodeProfile{
		ID:     nodeID,
		Roles:  rolesFromMeta(meta["roles"]),
		Labels: labelsFromMeta(meta["labels"]),
		Held:   heldFromMeta(meta[HeldKey]),
		Broker: brokerFromMeta(meta["broker"]),
	}
}

// heldFromMeta accepts the int this build writes and the float64 a JSON round
// trip through the lease store returns, and reads anything else — absent, a
// string, a negative or fractional count — as ZERO: the only reader sums the
// counts to decide whether to try claiming at all, and a count too low only
// sends it to try (see [NodeProfile.Held]).
func heldFromMeta(raw any) int {
	var n int
	switch v := raw.(type) {
	case int:
		n = v
	case int64:
		n = int(v)
	case float64:
		if v != float64(int(v)) {
			return 0
		}
		n = int(v)
	default:
		return 0
	}
	return max(n, 0)
}

// FromLease reads a peer's profile off a presence lease, reporting false for
// a lease that does not name a node. The prefix lives in coord; a caller
// stripping "node:" by hand is how a seat lease ends up read as a peer.
func FromLease(lease coord.Lease) (NodeProfile, bool) {
	id, ok := coord.NodeID(lease.Resource)
	if !ok {
		return NodeProfile{}, false
	}
	return FromMeta(id, lease.Meta), true
}

// rolesFromMeta accepts both the []string this build writes and the []any a
// JSON round trip through the lease store returns. Anything else — a bare
// string, a number, an unknown name — resolves to the unset set, which means
// every role.
func rolesFromMeta(raw any) RoleSet {
	var names []string
	switch v := raw.(type) {
	case []string:
		names = v
	case []any:
		names = make([]string, 0, len(v))
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil
			}
			names = append(names, s)
		}
	default:
		return nil
	}
	roles, err := ParseRoles(names)
	if err != nil {
		return nil
	}
	return roles
}

// labelsFromMeta coerces non-string values rather than dropping the row: a
// peer that wrote zone: 7 is describing a real node, and reading its other
// labels is better than reading none of them.
func labelsFromMeta(raw any) map[string]string {
	switch v := raw.(type) {
	case map[string]string:
		return maps.Clone(v)
	case map[string]any:
		out := make(map[string]string, len(v))
		for k, val := range v {
			if s, ok := val.(string); ok {
				out[k] = s
				continue
			}
			out[k] = fmt.Sprint(val)
		}
		return out
	default:
		return nil
	}
}

// Seat pairs a seat handle with where that seat is allowed to run.
type Seat struct {
	Handle    string
	Placement SeatPlacement
}

// Group is one placement group this node may claim from: the seats that share
// a placement, and how many of them this node may hold.
type Group struct {
	// Placement is the constraint every seat in the group carries.
	Placement SeatPlacement

	// Handles are the group's seats, in the order the seats were given.
	Handles []string

	// Nodes is how many live seat-running nodes match the placement, this
	// one included.
	Nodes int

	// Share is how many of THIS GROUP's seats this node may hold:
	// ceil(len(Handles) / Nodes). It bounds the group and nothing else —
	// room left in one group is never room in another (see the package
	// doc).
	Share int
}

// Plan is what this node may claim, how much of it, and what nobody can.
type Plan struct {
	// Capacity is the sum of this node's per-group shares — how many seats
	// it may hold in all. A total for logs and for the bound a node's whole
	// holding is kept within; it is never room to spend on any one group.
	// Zero for a node that does not run seats.
	Capacity int

	// Groups are the placement groups this node may claim from, MOST
	// CONSTRAINED FIRST: fewest eligible nodes, a placement before
	// "anywhere" at a tie, then the order the seats were given. A group few
	// nodes may serve has the fewest ways to be served, so when a pass can
	// claim only so many seats (the per-sweep claim limit) its seats are
	// the ones taken first. Coverage does not depend on this order —
	// [Plan.Room] does that — only how soon it is reached.
	Groups []Group

	// Eligible are the seat handles this node is allowed to hold, in the
	// order the seats were given. Eligibility is not a preference to be
	// sorted: a seat outside this list is one this node may never claim,
	// however much capacity it has spare.
	Eligible []string

	// Unplaceable are the seats the fleet's claim bounds cannot reach: a
	// group whose eligible nodes' shares sum to less than its size, which —
	// shares being ceilings — is exactly a group no live seat-running node
	// matches, such as a pin to a node that is down or a label nobody
	// carries. Not something the engine can fix, since widening the
	// selector is exactly what the operator asked it not to do, but it must
	// be reported rather than dropped: the seat is simply not being served,
	// and every node's sweep otherwise looks perfectly healthy. The host
	// logs seats_unplaceable from this.
	Unplaceable []string

	// SeatNodes is how many live nodes run seats at all — the denominator
	// that used to be "every live node".
	SeatNodes int
}

// Room is how many more seats this node may claim in each of [Plan.Groups],
// index for index, given which seat leases it holds. NEGATIVE is a group it
// holds more of than its share, by that many — what the host gives back.
//
// holds reports whether this node holds a seat's lease at all: one it runs,
// and one whose teardown it could not prove and is still renewing, which
// occupies the group's slot just the same — no peer can take it.
func (p Plan) Room(holds func(handle string) bool) []int {
	room := make([]int, len(p.Groups))
	for i, g := range p.Groups {
		room[i] = g.Share
		for _, h := range g.Handles {
			if holds(h) {
				room[i]--
			}
		}
	}
	return room
}

// Compute works out this node's share of a placement-constrained company.
//
// me is always counted as live whether or not the presence read returned it:
// before the first successful renew there is no row yet, and a store blip
// must not make a node invisible to itself. A node missing from its own
// fleet finds zero eligible nodes for every group, claims nothing, and
// reports every seat unplaceable.
//
// me also WINS over any profile for the same id in live. Its own presence
// row may have been written by its previous incarnation and can describe
// roles or labels this process no longer has; believing the row over the
// process would make a node claim seats it is no longer configured for.
//
// Seats are grouped by placement, and each group's share is
// ceil(group size / nodes eligible for that group) — a bound on that group
// alone (see the package doc for why a summed, fungible capacity strands a
// pinned seat). The share is a CEILING, and that is what makes the host's
// give-back settle instead of oscillating: a group's shares over its
// eligible nodes sum to at least its size, so a node that has shed a group
// down to its share has no room in it to immediately re-claim what it just
// let go.
func Compute(seats []Seat, me NodeProfile, live []NodeProfile) Plan {
	seatNodes := seatRunners(me, live)

	groups := make(map[string]*Group)
	var groupOrder []string
	handleOrder := make([]string, 0, len(seats))

	for _, seat := range seats {
		key := seat.Placement.Key()
		g, ok := groups[key]
		if !ok {
			g = &Group{Placement: seat.Placement}
			groups[key] = g
			groupOrder = append(groupOrder, key)
		}
		g.Handles = append(g.Handles, seat.Handle)
		handleOrder = append(handleOrder, seat.Handle)
	}

	plan := Plan{SeatNodes: len(seatNodes)}
	eligible := make(map[string]struct{})
	unplaceable := make(map[string]struct{})

	for _, key := range groupOrder {
		g := groups[key]
		for _, node := range seatNodes {
			if g.Placement.Matches(node.ID, node.Labels) {
				g.Nodes++
			}
		}
		// Per group, over the nodes eligible for THIS group. A fleet-wide
		// ratio here is the stranding bug the package doc opens with.
		g.Share = share(len(g.Handles), g.Nodes)

		// THE SAME ARITHMETIC THE CLAIMS RUN ON. Every eligible node holds
		// at most Share of this group, so Nodes × Share is all of it the
		// fleet will ever hold; a group that bound cannot cover is one no
		// sweep will finish, and it is reported rather than believed served.
		if g.Nodes*g.Share < len(g.Handles) {
			for _, h := range g.Handles {
				unplaceable[h] = struct{}{}
			}
			continue
		}
		if !me.RunsSeats() || !g.Placement.Matches(me.ID, me.Labels) {
			continue
		}

		plan.Capacity += g.Share
		plan.Groups = append(plan.Groups, *g)
		for _, h := range g.Handles {
			eligible[h] = struct{}{}
		}
	}

	// Fewest eligible nodes first. At a tie a placement goes before
	// "anywhere": both have the same homes today, but every node that joins
	// is a home for the unconstrained seat and few are for the placed one.
	// Stable beyond that, so groups as constrained as each other keep the
	// org's order and every node walks them the same way.
	slices.SortStableFunc(plan.Groups, func(a, b Group) int {
		if a.Nodes != b.Nodes {
			return a.Nodes - b.Nodes
		}
		return boolOrder(a.Placement.IsAnywhere()) - boolOrder(b.Placement.IsAnywhere())
	})

	for _, h := range handleOrder {
		if _, ok := eligible[h]; ok {
			plan.Eligible = append(plan.Eligible, h)
		}
		if _, ok := unplaceable[h]; ok {
			plan.Unplaceable = append(plan.Unplaceable, h)
		}
	}
	return plan
}

// boolOrder sorts false before true.
func boolOrder(b bool) int {
	if b {
		return 1
	}
	return 0
}

// share is how many of a group's seats each of its eligible nodes may hold.
// A group no node is eligible for gives nobody any of it, so the
// fleet's bound on it is zero and [Compute] reports it unplaceable by the
// same comparison that judges every other group.
func share(seats, nodes int) int {
	if nodes == 0 {
		return 0
	}
	return (seats + nodes - 1) / nodes
}

// seatRunners resolves the fleet this node divides the seats by: live peers
// plus me, deduplicated by id with me authoritative about itself, then
// filtered to the nodes that run seats at all.
func seatRunners(me NodeProfile, live []NodeProfile) []NodeProfile {
	index := make(map[string]int, len(live)+1)
	fleet := make([]NodeProfile, 0, len(live)+1)

	for _, node := range live {
		if node.ID == "" {
			// A presence row with no id names no node; counting it would
			// inflate the denominator with a peer nothing can be placed on.
			continue
		}
		if i, ok := index[node.ID]; ok {
			fleet[i] = node
			continue
		}
		index[node.ID] = len(fleet)
		fleet = append(fleet, node)
	}
	if i, ok := index[me.ID]; ok {
		fleet[i] = me
	} else {
		fleet = append(fleet, me)
	}

	runners := make([]NodeProfile, 0, len(fleet))
	for _, node := range fleet {
		if node.RunsSeats() {
			runners = append(runners, node)
		}
	}
	return runners
}
