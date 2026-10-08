// The placement arithmetic, which is the part that is easy to get wrong.
//
// Placement is not a filter you bolt onto a fleet-wide fair share. The share
// has to be computed per placement group, over the nodes eligible for that
// group — and the test that proves it is
// TestComputePinnedMajorityIsNotStrandedByAFleetWideRatio: under the naive
// ratio every node reports a healthy sweep while five seats are served by
// nobody. Nor is a share a number a node may spend on any group it matches:
// TestGreedyClaimsBoundedPerGroupPlaceEveryPlaceableSeat is why.
package placement

import (
	"encoding/json"
	"errors"
	"maps"
	"math/rand/v2"
	"slices"
	"strconv"
	"testing"

	"github.com/crewlet/crewlet/internal/coord"
)

// anywhere is the unconstrained placement, spelled out at every use site so
// the tests read the way a config does.
var anywhere = SeatPlacement{}

func seatsWith(p SeatPlacement, handles ...string) []Seat {
	out := make([]Seat, 0, len(handles))
	for _, h := range handles {
		out = append(out, Seat{Handle: h, Placement: p})
	}
	return out
}

// ── matching ─────────────────────────────────────────────────────────

func TestSeatPlacementMatches(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		placement SeatPlacement
		nodeID    string
		labels    map[string]string
		want      bool
	}{
		{
			name:      "an empty placement matches everything",
			placement: anywhere,
			nodeID:    "whatever",
			want:      true,
		},
		{
			name:      "a node pin is exact",
			placement: SeatPlacement{Node: "sat-1"},
			nodeID:    "sat-1",
			want:      true,
		},
		{
			name:      "a node pin does not prefix match",
			placement: SeatPlacement{Node: "sat-1"},
			nodeID:    "sat-10",
			want:      false,
		},
		{
			name:      "a node pin rejects another node",
			placement: SeatPlacement{Node: "sat-1"},
			nodeID:    "core-1",
			want:      false,
		},
		{
			name:      "every label must match",
			placement: SeatPlacement{Labels: map[string]string{"zone": "eu", "gpu": "true"}},
			nodeID:    "n",
			labels:    map[string]string{"zone": "eu", "gpu": "true", "extra": "ok"},
			want:      true,
		},
		{
			name:      "a missing label fails",
			placement: SeatPlacement{Labels: map[string]string{"zone": "eu", "gpu": "true"}},
			nodeID:    "n",
			labels:    map[string]string{"zone": "eu"},
			want:      false,
		},
		{
			name:      "a differing label fails",
			placement: SeatPlacement{Labels: map[string]string{"zone": "eu", "gpu": "true"}},
			nodeID:    "n",
			labels:    map[string]string{"zone": "us", "gpu": "true"},
			want:      false,
		},
		{
			// A selector asking for an empty value must not match a node
			// that has never heard of the key — a missing key reads as the
			// zero value, and equality alone would place the seat anywhere.
			name:      "a required empty value still requires the key",
			placement: SeatPlacement{Labels: map[string]string{"zone": ""}},
			nodeID:    "n",
			labels:    map[string]string{"other": "x"},
			want:      false,
		},
		{
			name:      "a required empty value matches an empty value",
			placement: SeatPlacement{Labels: map[string]string{"zone": ""}},
			nodeID:    "n",
			labels:    map[string]string{"zone": ""},
			want:      true,
		},
		{
			// Both conditions, so a placement can only ever narrow.
			name:      "a node pin and labels are ANDed",
			placement: SeatPlacement{Node: "sat-1", Labels: map[string]string{"zone": "eu"}},
			nodeID:    "sat-1",
			labels:    map[string]string{"zone": "eu"},
			want:      true,
		},
		{
			name:      "the pin holding is not enough",
			placement: SeatPlacement{Node: "sat-1", Labels: map[string]string{"zone": "eu"}},
			nodeID:    "sat-1",
			labels:    map[string]string{"zone": "us"},
			want:      false,
		},
		{
			name:      "the labels holding are not enough",
			placement: SeatPlacement{Node: "sat-1", Labels: map[string]string{"zone": "eu"}},
			nodeID:    "sat-2",
			labels:    map[string]string{"zone": "eu"},
			want:      false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.placement.Matches(tc.nodeID, tc.labels); got != tc.want {
				t.Fatalf("Matches(%q, %v) = %v, want %v", tc.nodeID, tc.labels, got, tc.want)
			}
		})
	}
}

func TestSeatPlacementIsAnywhere(t *testing.T) {
	t.Parallel()

	if !anywhere.IsAnywhere() {
		t.Fatal("the zero placement must be anywhere")
	}
	if (SeatPlacement{Node: "n"}).IsAnywhere() {
		t.Fatal("a pin constrains")
	}
	if (SeatPlacement{Labels: map[string]string{"a": "b"}}).IsAnywhere() {
		t.Fatal("a selector constrains")
	}
}

// Two placements sharing a key share one fair share, so a collision would
// silently halve the share of both groups.
func TestSeatPlacementKeyIsInjective(t *testing.T) {
	t.Parallel()

	distinct := []SeatPlacement{
		anywhere,
		{Node: "a"},
		{Node: "b"},
		{Labels: map[string]string{"a": "b"}},
		{Labels: map[string]string{"a": "b", "c": "d"}},
		{Labels: map[string]string{"a=b": "c"}},
		{Labels: map[string]string{"a": "b=c"}},
		{Node: "a", Labels: map[string]string{"a": "b"}},
	}
	seen := map[string]SeatPlacement{}
	for _, p := range distinct {
		key := p.Key()
		if other, clash := seen[key]; clash {
			t.Fatalf("%v and %v collide on key %q", p, other, key)
		}
		seen[key] = p
	}

	// Label order is not part of the identity: two roles writing the same
	// selector must land in one group.
	a := SeatPlacement{Labels: map[string]string{"zone": "eu", "gpu": "true"}}
	b := SeatPlacement{Labels: map[string]string{"gpu": "true", "zone": "eu"}}
	if a.Key() != b.Key() {
		t.Fatalf("equal selectors keyed differently: %q vs %q", a.Key(), b.Key())
	}
}

func TestSeatPlacementString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		placement SeatPlacement
		want      string
	}{
		{anywhere, "anywhere"},
		{SeatPlacement{Node: "sat-1"}, "node=sat-1"},
		{SeatPlacement{Labels: map[string]string{"zone": "eu", "gpu": "true"}}, "labels=gpu=true,zone=eu"},
		{SeatPlacement{Node: "sat-1", Labels: map[string]string{"zone": "eu"}}, "node=sat-1 labels=zone=eu"},
	}
	for _, tc := range tests {
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			if got := tc.placement.String(); got != tc.want {
				t.Fatalf("String() = %q, want %q", got, tc.want)
			}
		})
	}
}

// ── the role vocabulary ──────────────────────────────────────────────

func TestParseRoles(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		values  []string
		want    []string
		wantErr bool
	}{
		{
			name:   "nil is every role",
			values: nil,
			want:   []string{"data", "ingress", "seats", "workers"},
		},
		{
			name:   "empty is every role",
			values: []string{},
			want:   []string{"data", "ingress", "seats", "workers"},
		},
		{
			name:   "one role is one role",
			values: []string{"seats"},
			want:   []string{"seats"},
		},
		{
			name:   "duplicates collapse",
			values: []string{"seats", "seats", "workers"},
			want:   []string{"seats", "workers"},
		},
		{
			// A typo'd role in a bootstrap file must not silently subtract a
			// duty from the fleet, so this half fails closed.
			name:    "the plausible typo is rejected",
			values:  []string{"seat"},
			wantErr: true,
		},
		{
			name:    "one bad name rejects the whole list",
			values:  []string{"seats", "nonsense"},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseRoles(tc.values)
			if tc.wantErr {
				if !errors.Is(err, ErrUnknownRole) {
					t.Fatalf("ParseRoles(%v) error = %v, want ErrUnknownRole", tc.values, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseRoles(%v): %v", tc.values, err)
			}
			if !slices.Equal(got.Names(), tc.want) {
				t.Fatalf("ParseRoles(%v) = %v, want %v", tc.values, got.Names(), tc.want)
			}
		})
	}
}

// The Go-native half of "a presence row with no meta reads as the old
// behaviour": a caller that never set Roles gets a node that does
// everything, not one that does nothing and drops out of the denominator.
func TestUnsetRoleSetIsEveryRole(t *testing.T) {
	t.Parallel()

	var unset RoleSet
	for _, role := range Vocabulary() {
		if !unset.Has(role) {
			t.Fatalf("the unset role set must have %q", role)
		}
	}
	if !unset.Equal(DefaultRoles()) {
		t.Fatalf("unset = %v, want every role", unset.Names())
	}

	zero := NodeProfile{ID: "n1"}
	if !zero.RunsSeats() || !zero.RunsWorkers() || !zero.RunsIngress() || !zero.HoldsData() {
		t.Fatalf("a profile with no declared roles must do everything, got %v", zero.Roles.Names())
	}

	declared := NodeProfile{ID: "api", Roles: Roles(RoleIngress)}
	if declared.RunsSeats() {
		t.Fatal("an ingress-only node must not run seats")
	}
	// AND A PEER THAT DECLARES NOTHING HOLDS DATA. A row naming no roles
	// reads as every role, and reading it as stateless would drop a
	// member's position out of the trim's minimum.
	if !zero.HoldsData() {
		t.Fatal("a profile with no declared roles must hold data")
	}
	stateless := NodeProfile{ID: "agent", Roles: Roles(RoleSeats)}
	if stateless.HoldsData() {
		t.Fatal("a seats-only node must not hold data")
	}
}

// DefaultRoles hands out a fresh set precisely so a careless insert cannot
// redefine the default fleet-wide.
func TestDefaultRolesIsNotShared(t *testing.T) {
	t.Parallel()

	first := DefaultRoles()
	first["nonsense"] = struct{}{}
	if _, leaked := DefaultRoles()["nonsense"]; leaked {
		t.Fatal("DefaultRoles handed out shared state")
	}
}

// ── the node profile on the wire ─────────────────────────────────────

func TestProfileRoundTripsThroughLeaseMeta(t *testing.T) {
	t.Parallel()

	me := NodeProfile{
		ID:     "n1",
		Roles:  Roles(RoleSeats, RoleWorkers, RoleData),
		Labels: map[string]string{"zone": "eu"},
	}

	back := FromMeta("n1", me.Meta())
	assertProfile(t, back, me)

	// The lease store round-trips meta through JSON, which turns []string
	// into []any and map[string]string into map[string]any. A reader that
	// only understood this build's own types would read every peer as
	// unset — that is, as a node doing everything.
	raw, err := json.Marshal(me.Meta())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	assertProfile(t, FromMeta("n1", decoded), me)
}

// A row that does not say. "Does everything, labelled with nothing" is the
// only safe reading — the alternative is a node with no roles, which drops a
// live peer out of the denominator and over-subscribes the rest of the fleet.
func TestFromMetaOfARowThatDoesNotSayDoesEverything(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		meta map[string]any
	}{
		{"absent", nil},
		{"empty", map[string]any{}},
		{"other fields only", map[string]any{"zone": "eu"}},
		{"explicit nulls", map[string]any{"roles": nil, "labels": nil}},
		{"an empty role list", map[string]any{"roles": []any{}}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			back := FromMeta("silent", tc.meta)
			if !back.Roles.Equal(DefaultRoles()) {
				t.Fatalf("roles = %v, want every role", back.Roles.Names())
			}
			if len(back.Labels) != 0 {
				t.Fatalf("labels = %v, want none", back.Labels)
			}
			if !back.RunsSeats() {
				t.Fatal("a row that does not say must still count as a seat runner")
			}
			if back.ID != "silent" {
				t.Fatalf("id = %q, want %q", back.ID, "silent")
			}
		})
	}
}

// A peer's bad row must not take down the reader's sweep, and there is only
// one safe reading of one.
func TestFromMetaOfAMalformedRowDoesEverythingToo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		meta map[string]any
	}{
		{"roles as a bare string", map[string]any{"roles": "seats"}},
		{"an unknown role name", map[string]any{"roles": []any{"nonsense"}}},
		{"a numeric role", map[string]any{"roles": []any{7}}},
		{"roles as a map", map[string]any{"roles": map[string]any{"seats": true}}},
		{"labels as a number", map[string]any{"labels": 7}},
		{"labels as a list", map[string]any{"labels": []any{"zone", "eu"}}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			back := FromMeta("weird", tc.meta)
			if !back.Roles.Equal(DefaultRoles()) {
				t.Fatalf("roles = %v, want every role", back.Roles.Names())
			}
			if len(back.Labels) != 0 {
				t.Fatalf("labels = %v, want none", back.Labels)
			}
		})
	}
}

// A label whose value is not a string still describes a real node; reading
// its other labels beats reading none of them.
func TestFromMetaCoercesLabelValues(t *testing.T) {
	t.Parallel()

	back := FromMeta("n", map[string]any{"labels": map[string]any{"zone": "eu", "rack": 7}})
	want := map[string]string{"zone": "eu", "rack": "7"}
	if !maps.Equal(back.Labels, want) {
		t.Fatalf("labels = %v, want %v", back.Labels, want)
	}
}

func TestFromLease(t *testing.T) {
	t.Parallel()

	profile := NodeProfile{ID: "n1", Roles: Roles(RoleSeats), Labels: map[string]string{"zone": "eu"}}
	lease := coord.Lease{Resource: coord.NodeResource("n1"), Meta: profile.Meta()}

	back, ok := FromLease(lease)
	if !ok {
		t.Fatal("a presence lease must read as a profile")
	}
	assertProfile(t, back, profile)

	// A caller sweeping the wrong prefix must not read a seat as a peer:
	// a phantom node in the denominator shrinks everyone's share.
	if _, ok := FromLease(coord.Lease{Resource: coord.SeatResource("alice")}); ok {
		t.Fatal("a seat lease must not read as a node profile")
	}
}

// ── the share ────────────────────────────────────────────────────────

// The unconstrained fleet is the degenerate case, not a second path.
func TestComputeWithNoPlacementIsTheCeilingShare(t *testing.T) {
	t.Parallel()

	seats := seatsWith(anywhere, "a", "b", "c", "d", "e")
	live := []NodeProfile{{ID: "n1"}, {ID: "n2"}, {ID: "n3"}}

	plan := Compute(seats, live[0], live)

	if plan.Capacity != 2 { // ceil(5 / 3)
		t.Fatalf("capacity = %d, want 2", plan.Capacity)
	}
	assertHandles(t, "eligible", plan.Eligible, []string{"a", "b", "c", "d", "e"})
	assertHandles(t, "unplaceable", plan.Unplaceable, nil)
	if plan.SeatNodes != 3 {
		t.Fatalf("seat nodes = %d, want 3", plan.SeatNodes)
	}
}

// The reason the share is per group.
//
// Nine seats pinned to one node and one free, over three nodes: a fleet-wide
// ceil(10/3) = 4 lets the pinned node take four of its nine, and the other
// five are claimable by nobody — stranded forever, while every node in the
// fleet reports a perfectly healthy sweep.
func TestComputePinnedMajorityIsNotStrandedByAFleetWideRatio(t *testing.T) {
	t.Parallel()

	pinned := SeatPlacement{Node: "big"}
	seats := append(
		seatsWith(pinned, "p0", "p1", "p2", "p3", "p4", "p5", "p6", "p7", "p8"),
		Seat{Handle: "free", Placement: anywhere},
	)
	live := []NodeProfile{{ID: "big"}, {ID: "n2"}, {ID: "n3"}}

	// Nine pinned seats with one eligible node → all nine, plus its third
	// of the one free seat.
	big := Compute(seats, live[0], live)
	if big.Capacity != 10 {
		t.Fatalf("pinned node capacity = %d, want 10 (9 pinned + 1 free)", big.Capacity)
	}
	assertHandles(t, "eligible", big.Eligible,
		[]string{"p0", "p1", "p2", "p3", "p4", "p5", "p6", "p7", "p8", "free"})
	assertHandles(t, "unplaceable", big.Unplaceable, nil)

	other := Compute(seats, live[1], live)
	if other.Capacity != 1 {
		t.Fatalf("peer capacity = %d, want 1", other.Capacity)
	}
	assertHandles(t, "eligible", other.Eligible, []string{"free"})

	// The stranding itself: with the naive fleet-wide ratio the pinned
	// node's capacity is ceil(10/3) = 4 and nobody else is eligible, so
	// five of the nine pinned seats are served by no node at all.
	if reach := big.Capacity + other.Capacity + Compute(seats, live[2], live).Capacity; reach < len(seats) {
		t.Fatalf("fleet capacity %d cannot cover %d seats — seats are stranded", reach, len(seats))
	}
}

// The one placement failure that is otherwise invisible: the engine will not
// widen a selector to place a seat, so it has to say so.
func TestComputeReportsSeatsNoNodeMatches(t *testing.T) {
	t.Parallel()

	seats := []Seat{
		{Handle: "a", Placement: anywhere},
		{Handle: "gpu", Placement: SeatPlacement{Labels: map[string]string{"gpu": "true"}}},
		{Handle: "pinned", Placement: SeatPlacement{Node: "gone"}},
	}
	live := []NodeProfile{{ID: "n1"}, {ID: "n2"}}

	plan := Compute(seats, live[0], live)

	assertHandles(t, "unplaceable", plan.Unplaceable, []string{"gpu", "pinned"})
	assertHandles(t, "eligible", plan.Eligible, []string{"a"})
	if plan.Capacity != 1 {
		t.Fatalf("capacity = %d, want 1", plan.Capacity)
	}
}

// An ingress-only node would otherwise shrink everyone's share and strand
// the difference — it is present, and it will never claim.
func TestComputeExcludesNonSeatNodesFromTheDenominator(t *testing.T) {
	t.Parallel()

	seats := seatsWith(anywhere, "a", "b", "c", "d")
	live := []NodeProfile{
		{ID: "n1"},
		{ID: "n2"},
		{ID: "api", Roles: Roles(RoleIngress)},
		{ID: "duties", Roles: Roles(RoleIngress, RoleWorkers)},
	}

	plan := Compute(seats, live[0], live)

	if plan.SeatNodes != 2 {
		t.Fatalf("seat nodes = %d, want 2", plan.SeatNodes)
	}
	if plan.Capacity != 2 { // ceil(4 / 2), not ceil(4 / 4)
		t.Fatalf("capacity = %d, want 2", plan.Capacity)
	}
}

func TestComputeGivesANonSeatNodeNothing(t *testing.T) {
	t.Parallel()

	api := NodeProfile{ID: "api", Roles: Roles(RoleIngress, RoleWorkers)}
	plan := Compute(seatsWith(anywhere, "a"), api, []NodeProfile{api, {ID: "n1"}})

	if plan.Capacity != 0 {
		t.Fatalf("capacity = %d, want 0", plan.Capacity)
	}
	assertHandles(t, "eligible", plan.Eligible, nil)
	// The seat is served by n1, so it is not unplaceable — a node that
	// claims nothing must not report the company as broken.
	assertHandles(t, "unplaceable", plan.Unplaceable, nil)
}

// First sweep of the first node, and every store blip after. A node that is
// invisible to itself computes a share out of a fleet it is not in — zero
// eligible nodes for every group, so it claims nothing and reports every
// seat unplaceable.
func TestComputeCountsThisNodeBeforeItsPresenceRowLands(t *testing.T) {
	t.Parallel()

	me := NodeProfile{ID: "n1"}
	plan := Compute(seatsWith(anywhere, "a", "b"), me, nil)

	if plan.Capacity != 2 {
		t.Fatalf("capacity = %d, want 2", plan.Capacity)
	}
	assertHandles(t, "unplaceable", plan.Unplaceable, nil)
	if plan.SeatNodes != 1 {
		t.Fatalf("seat nodes = %d, want 1", plan.SeatNodes)
	}
}

// me is authoritative about itself. Its own presence row was written by its
// previous incarnation and can describe roles or labels this process no
// longer has; believing the row over the process would make a node claim
// seats it is no longer configured for.
func TestComputeIgnoresAStaleProfileForThisNode(t *testing.T) {
	t.Parallel()

	me := NodeProfile{ID: "n1", Labels: map[string]string{"zone": "us"}}
	stale := NodeProfile{ID: "n1", Labels: map[string]string{"zone": "eu"}}
	seats := []Seat{{Handle: "eu-seat", Placement: SeatPlacement{Labels: map[string]string{"zone": "eu"}}}}

	plan := Compute(seats, me, []NodeProfile{stale})

	assertHandles(t, "eligible", plan.Eligible, nil)
	assertHandles(t, "unplaceable", plan.Unplaceable, []string{"eu-seat"})

	// The same staleness in the other direction: a row claiming this node
	// runs seats does not resurrect a process that no longer does.
	quit := NodeProfile{ID: "n1", Roles: Roles(RoleIngress)}
	stalePlan := Compute(seatsWith(anywhere, "a"), quit, []NodeProfile{{ID: "n1"}})
	if stalePlan.SeatNodes != 0 || stalePlan.Capacity != 0 {
		t.Fatalf("stale seat-running row won: seatNodes=%d capacity=%d", stalePlan.SeatNodes, stalePlan.Capacity)
	}
}

func TestComputeEligibilityFollowsPinsAndSelectors(t *testing.T) {
	t.Parallel()

	fleet := []NodeProfile{
		{ID: "core-1"},
		{ID: "sat-eu", Roles: Roles(RoleSeats), Labels: map[string]string{"zone": "eu"}},
		{ID: "sat-us", Roles: Roles(RoleSeats), Labels: map[string]string{"zone": "us"}},
	}
	seats := []Seat{
		{Handle: "free", Placement: anywhere},
		{Handle: "eu", Placement: SeatPlacement{Labels: map[string]string{"zone": "eu"}}},
		{Handle: "pinned", Placement: SeatPlacement{Node: "core-1"}},
		{Handle: "eu-pinned", Placement: SeatPlacement{Node: "sat-eu", Labels: map[string]string{"zone": "eu"}}},
		{Handle: "impossible", Placement: SeatPlacement{Node: "sat-us", Labels: map[string]string{"zone": "eu"}}},
	}

	tests := []struct {
		me           string
		wantEligible []string
	}{
		{"core-1", []string{"free", "pinned"}},
		{"sat-eu", []string{"free", "eu", "eu-pinned"}},
		{"sat-us", []string{"free"}},
	}

	for _, tc := range tests {
		t.Run(tc.me, func(t *testing.T) {
			t.Parallel()
			me := fleet[slices.IndexFunc(fleet, func(n NodeProfile) bool { return n.ID == tc.me })]
			plan := Compute(seats, me, fleet)
			assertHandles(t, "eligible", plan.Eligible, tc.wantEligible)
			// A pin ANDed with a selector its own node fails is a seat
			// nobody may run, and every node must say so.
			assertHandles(t, "unplaceable", plan.Unplaceable, []string{"impossible"})
		})
	}
}

// EVERY GROUP IS COVERED BY ITS OWN NODES' SHARES. A group's eligible nodes
// may each hold Share of it and nothing outside the group can occupy that
// room, so the shares of the nodes it matches must sum to at least its size,
// or a seat is stranded with every sweep reporting healthy.
//
// Summing over the whole fleet instead — the check this replaced — passes the
// satellite shape below while it strands the pinned seat: the satellite's
// total covers it, and then spends itself on unpinned seats.
func TestEveryGroupIsCoveredByItsOwnNodesShares(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		seats []Seat
		fleet []NodeProfile
	}{
		{
			name:  "unconstrained, indivisible",
			seats: seatsWith(anywhere, "s0", "s1", "s2", "s3", "s4", "s5", "s6"),
			fleet: []NodeProfile{{ID: "n0"}, {ID: "n1"}, {ID: "n2"}},
		},
		{
			name:  "one node, every seat",
			seats: seatsWith(anywhere, "a", "b", "c"),
			fleet: []NodeProfile{{ID: "solo"}},
		},
		{
			name: "a pinned majority plus a free seat",
			seats: append(
				seatsWith(SeatPlacement{Node: "big"}, "p0", "p1", "p2", "p3", "p4", "p5", "p6", "p7", "p8"),
				Seat{Handle: "free", Placement: anywhere},
			),
			fleet: []NodeProfile{{ID: "big"}, {ID: "n2"}, {ID: "n3"}},
		},
		{
			name: "a satellite for one pinned seat beside two cores",
			seats: append(
				seatsWith(anywhere, "aaron", "bob", "carl"),
				Seat{Handle: "zed", Placement: SeatPlacement{Labels: map[string]string{"seat": "zed"}}},
			),
			fleet: []NodeProfile{
				{ID: "sat-zed", Roles: Roles(RoleSeats), Labels: map[string]string{"seat": "zed"}},
				{ID: "core-1"},
				{ID: "core-2"},
			},
		},
		{
			name: "overlapping selectors",
			seats: append(
				append(
					seatsWith(SeatPlacement{Labels: map[string]string{"zone": "eu"}}, "eu0", "eu1", "eu2", "eu3", "eu4"),
					seatsWith(anywhere, "f0", "f1", "f2")...,
				),
				seatsWith(SeatPlacement{Node: "core-1"}, "c0", "c1")...,
			),
			fleet: []NodeProfile{
				{ID: "core-1", Labels: map[string]string{"zone": "eu"}},
				{ID: "sat-eu", Labels: map[string]string{"zone": "eu"}},
				{ID: "sat-us", Labels: map[string]string{"zone": "us"}},
				{ID: "api", Roles: Roles(RoleIngress)},
			},
		},
		{
			name:  "a seat nobody matches",
			seats: append(seatsWith(anywhere, "a", "b"), Seat{Handle: "gpu", Placement: SeatPlacement{Labels: map[string]string{"gpu": "true"}}}),
			fleet: []NodeProfile{{ID: "n1"}, {ID: "n2"}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			reach := map[string]int{}
			size := map[string]int{}
			for _, s := range tc.seats {
				size[s.Placement.Key()]++
			}
			var unplaceable []string
			for i, me := range tc.fleet {
				plan := Compute(tc.seats, me, tc.fleet)
				total := 0
				for _, g := range plan.Groups {
					reach[g.Placement.Key()] += g.Share
					total += g.Share
				}
				if total != plan.Capacity {
					t.Fatalf("%s: capacity %d is not the sum of its group shares %d", me.ID, plan.Capacity, total)
				}

				// Every node must reach the same verdict about the company
				// from the same membership read; two nodes disagreeing about
				// what is unplaceable is a company nobody can diagnose.
				if i == 0 {
					unplaceable = plan.Unplaceable
					continue
				}
				assertHandles(t, "unplaceable", plan.Unplaceable, unplaceable)
			}

			for _, s := range tc.seats {
				key := s.Placement.Key()
				if slices.Contains(unplaceable, s.Handle) {
					if reach[key] != 0 {
						t.Fatalf("%s is reported unplaceable, but its group's shares reach %d", s.Handle, reach[key])
					}
					continue
				}
				if reach[key] < size[key] {
					t.Fatalf("group %s: its nodes' shares reach %d of its %d seats — %s is stranded "+
						"with nothing reported", s.Placement, reach[key], size[key], s.Handle)
				}
			}
		})
	}
}

// GREEDY CLAIMS BOUNDED PER GROUP PLACE EVERY PLACEABLE SEAT, in any order,
// and the seats left over are exactly the ones reported unplaceable.
//
// This is the host's policy run against the plan alone: each node, in a
// shuffled order, walks its groups in a shuffled order and takes free seats
// while [Plan.Room] says it has room in that seat's group — the order a
// claim loop happens to use must not be what decides whether a seat is
// served. Random fleets over a fixed seed, so a failure reproduces.
func TestGreedyClaimsBoundedPerGroupPlaceEveryPlaceableSeat(t *testing.T) {
	t.Parallel()

	rng := rand.New(rand.NewPCG(1, 2))
	for run := range 500 {
		seats, fleet := randomCompany(rng)

		plans := make([]Plan, len(fleet))
		for i, me := range fleet {
			plans[i] = Compute(seats, me, fleet)
		}
		held := simulateClaims(rng, fleet, plans)

		unplaceable := plans[0].Unplaceable
		for _, s := range seats {
			_, isHeld := held[s.Handle]
			if isHeld == slices.Contains(unplaceable, s.Handle) {
				t.Fatalf("run %d: seat %s (%s) held=%v but unplaceable=%v\nseats=%v\nfleet=%v\nheld=%v",
					run, s.Handle, s.Placement, isHeld, unplaceable, seats, fleet, held)
			}
		}
		for i, plan := range plans {
			id := fleet[i].ID
			for gi, room := range plan.Room(func(h string) bool { return held[h] == id }, nil) {
				if room < 0 {
					t.Fatalf("run %d: %s holds more than its share of %s", run, id, plan.Groups[gi].Placement)
				}
			}
		}
	}
}

// simulateClaims runs greedy, per-group-bounded claims to a fixed point and
// returns who holds what.
func simulateClaims(rng *rand.Rand, fleet []NodeProfile, plans []Plan) map[string]string {
	held := map[string]string{}
	for progress := true; progress; {
		progress = false
		for _, i := range rng.Perm(len(fleet)) {
			id, plan := fleet[i].ID, plans[i]
			room := plan.Room(func(h string) bool { return held[h] == id }, nil)
			for _, gi := range rng.Perm(len(plan.Groups)) {
				handles := plan.Groups[gi].Handles
				for _, hi := range rng.Perm(len(handles)) {
					if room[gi] <= 0 {
						break
					}
					if _, taken := held[handles[hi]]; taken {
						continue
					}
					held[handles[hi]] = id
					room[gi]--
					progress = true
				}
			}
		}
	}
	return held
}

// randomCompany is a fleet of one to six nodes — some running no seats, each
// carrying some of a small label vocabulary — and up to sixteen seats placed
// anywhere, on a label, on a node id that may not be live, or on both.
func randomCompany(rng *rand.Rand) ([]Seat, []NodeProfile) {
	zones := []string{"eu", "us", "ap"}
	fleet := make([]NodeProfile, 1+rng.IntN(6))
	for i := range fleet {
		node := NodeProfile{ID: "n" + strconv.Itoa(i), Labels: map[string]string{}}
		if rng.IntN(4) == 0 {
			node.Roles = Roles(RoleIngress, RoleWorkers)
		}
		if rng.IntN(2) == 0 {
			node.Labels["zone"] = zones[rng.IntN(len(zones))]
		}
		if rng.IntN(3) == 0 {
			node.Labels["gpu"] = "true"
		}
		fleet[i] = node
	}

	seats := make([]Seat, rng.IntN(17))
	for i := range seats {
		var p SeatPlacement
		switch rng.IntN(5) {
		case 0:
			p.Labels = map[string]string{"zone": zones[rng.IntN(len(zones))]}
		case 1:
			p.Labels = map[string]string{"gpu": "true"}
		case 2:
			p.Node = "n" + strconv.Itoa(rng.IntN(len(fleet)+1))
		case 3:
			p.Node = "n" + strconv.Itoa(rng.IntN(len(fleet)))
			p.Labels = map[string]string{"zone": zones[rng.IntN(len(zones))]}
		}
		seats[i] = Seat{Handle: "s" + strconv.Itoa(i), Placement: p}
	}
	return seats, fleet
}

// Room is per group: what this node runs of one group never uses up another,
// and a group held past what it may keep reads negative by exactly the
// surplus. A lease it cannot give back is charged first, wherever it sits, and
// squeezes the least constrained group — never the pinned one.
func TestRoomIsCountedPerGroup(t *testing.T) {
	t.Parallel()

	zed := SeatPlacement{Labels: map[string]string{"seat": "zed"}}
	seats := append(seatsWith(anywhere, "aaron", "bob", "carl"), Seat{Handle: "zed", Placement: zed})
	sat := NodeProfile{ID: "sat-zed", Roles: Roles(RoleSeats), Labels: map[string]string{"seat": "zed"}}
	fleet := []NodeProfile{sat, {ID: "core-1"}, {ID: "core-2"}}

	plan := Compute(seats, sat, fleet)
	if plan.Capacity != 2 {
		t.Fatalf("capacity = %d, want 2 (1 pinned + a third of 3 unpinned)", plan.Capacity)
	}
	if len(plan.Groups) != 2 || plan.Groups[0].Placement.Key() != zed.Key() {
		t.Fatalf("groups = %+v, want the pinned group first", plan.Groups)
	}

	holding := func(handles ...string) func(string) bool {
		return func(h string) bool { return slices.Contains(handles, h) }
	}
	tests := []struct {
		name    string
		running []string
		stuck   []string
		want    []int // pinned group, unpinned group
	}{
		{"nothing held", nil, nil, []int{1, 1}},
		// The stranding: two unpinned seats fill the capacity of 2, and a
		// pooled room would read zero everywhere. Per group, the pinned
		// group still has its room and the unpinned one is over by one.
		{"two unpinned seats", []string{"aaron", "bob"}, nil, []int{1, -1}},
		{"its own seats", []string{"zed", "carl"}, nil, []int{0, 0}},
		{"a running seat outside every group", []string{"elsewhere"}, nil, []int{1, 1}},
		// A stuck lease outside every group takes one of the two: out of
		// the unpinned group, so the pinned seat keeps its room.
		{"a stuck seat outside every group", nil, []string{"elsewhere"}, []int{1, 0}},
		// The same, with an unpinned seat running: total holding 2 of 2,
		// and a pooled bound would read "full" and shed nothing while the
		// pinned seat waits. Per group, the unpinned seat is the surplus.
		{"a stuck seat outside every group and an unpinned one", []string{"aaron"}, []string{"elsewhere"},
			[]int{1, -1}},
		// A stuck seat of a group uses that group's own share first.
		{"the pinned seat stuck", nil, []string{"zed"}, []int{0, 1}},
		{"an unpinned seat stuck beside a running one", []string{"bob"}, []string{"aaron"}, []int{1, -1}},
		// More stuck than capacity: nothing running may stay anywhere.
		{"stuck past capacity", []string{"zed"}, []string{"aaron", "bob", "elsewhere"}, []int{-1, 0}},
	}
	for _, tc := range tests {
		got := plan.Room(holding(tc.running...), tc.stuck)
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: room = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Most constrained first: fewest eligible nodes, and a placement before
// "anywhere" when the count ties, so a claim-limited pass spends itself on
// the seats with the fewest other homes. Beyond that, the org's order.
func TestGroupsAreOrderedMostConstrainedFirst(t *testing.T) {
	t.Parallel()

	eu := SeatPlacement{Labels: map[string]string{"zone": "eu"}}
	pinned := SeatPlacement{Node: "n1"}
	gpu := SeatPlacement{Labels: map[string]string{"gpu": "true"}}
	seats := []Seat{
		{Handle: "free", Placement: anywhere},
		{Handle: "eu", Placement: eu},
		{Handle: "gpu", Placement: gpu},
		{Handle: "pinned", Placement: pinned},
	}
	me := NodeProfile{ID: "n1", Labels: map[string]string{"zone": "eu", "gpu": "true"}}
	fleet := []NodeProfile{me, {ID: "n2", Labels: map[string]string{"zone": "eu"}}, {ID: "n3"}}

	plan := Compute(seats, me, fleet)
	var got []string
	for _, g := range plan.Groups {
		got = append(got, g.Handles...)
	}
	// gpu and pinned each have one node, in the org's order; eu has two;
	// free has three.
	assertHandles(t, "group order", got, []string{"gpu", "pinned", "eu", "free"})

	// Alone, every group has one node, and the placed ones still go first.
	alone := Compute(seats, me, nil)
	got = nil
	for _, g := range alone.Groups {
		got = append(got, g.Handles...)
	}
	assertHandles(t, "group order alone", got, []string{"eu", "gpu", "pinned", "free"})
}

// Eligibility is not a preference to be sorted: the host claims in the order
// it is given, and that order is the org's, not a map's.
func TestComputePreservesSeatOrder(t *testing.T) {
	t.Parallel()

	eu := SeatPlacement{Labels: map[string]string{"zone": "eu"}}
	seats := []Seat{
		{Handle: "z", Placement: anywhere},
		{Handle: "m", Placement: eu},
		{Handle: "a", Placement: anywhere},
		{Handle: "b", Placement: eu},
	}
	me := NodeProfile{ID: "n1", Labels: map[string]string{"zone": "eu"}}

	plan := Compute(seats, me, []NodeProfile{me})
	assertHandles(t, "eligible", plan.Eligible, []string{"z", "m", "a", "b"})

	// Repeated across runs: a plan built off Go's randomised map iteration
	// would drift between sweeps and make the host's claims non-repeatable.
	for range 20 {
		again := Compute(seats, me, []NodeProfile{me})
		assertHandles(t, "eligible", again.Eligible, plan.Eligible)
	}
}

// A presence row that names no node cannot be placed on, so counting it just
// shrinks everyone's share.
func TestComputeIgnoresAnonymousAndDuplicatePeers(t *testing.T) {
	t.Parallel()

	live := []NodeProfile{{ID: ""}, {ID: "n1"}, {ID: "n2"}, {ID: "n2"}}
	plan := Compute(seatsWith(anywhere, "a", "b", "c", "d"), live[1], live)

	if plan.SeatNodes != 2 {
		t.Fatalf("seat nodes = %d, want 2", plan.SeatNodes)
	}
	if plan.Capacity != 2 {
		t.Fatalf("capacity = %d, want 2", plan.Capacity)
	}
}

func TestComputeWithNoSeats(t *testing.T) {
	t.Parallel()

	plan := Compute(nil, NodeProfile{ID: "n1"}, []NodeProfile{{ID: "n1"}, {ID: "n2"}})
	if plan.Capacity != 0 {
		t.Fatalf("capacity = %d, want 0", plan.Capacity)
	}
	assertHandles(t, "eligible", plan.Eligible, nil)
	assertHandles(t, "unplaceable", plan.Unplaceable, nil)
	if plan.SeatNodes != 2 {
		t.Fatalf("seat nodes = %d, want 2", plan.SeatNodes)
	}
}

// ── helpers ──────────────────────────────────────────────────────────

func assertHandles(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !slices.Equal(got, want) {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

func assertProfile(t *testing.T, got, want NodeProfile) {
	t.Helper()
	if got.ID != want.ID {
		t.Fatalf("id = %q, want %q", got.ID, want.ID)
	}
	if !got.Roles.Equal(want.Roles) {
		t.Fatalf("roles = %v, want %v", got.Roles.Names(), want.Roles.Names())
	}
	if len(got.Labels) != 0 || len(want.Labels) != 0 {
		if !maps.Equal(got.Labels, want.Labels) {
			t.Fatalf("labels = %v, want %v", got.Labels, want.Labels)
		}
	}
}

// PRESENCE IS A NODE'S PROFILE AND NOTHING ELSE. A share written onto it would
// be a second answer to what the node holds — one that a shutdown drain
// withdraws, at its first step, while the node is still serving.
func TestPresenceCarriesNoObjectShare(t *testing.T) {
	t.Parallel()
	meta := NodeProfile{ID: "n1", Roles: Roles(RoleData)}.Meta()
	for key := range meta {
		if key != "roles" && key != "labels" {
			t.Errorf("presence carries %q: a node's profile is its roles and labels", key)
		}
	}
}

// A NODE'S SEAT COUNT READS OFF ITS ROW, and a count it cannot read is zero.
//
// Peers sum these counts to learn whether any seat is free. The count arrives
// as an int from this process and as a float64 after a round trip through the
// lease store; anything else — absent, or not a whole non-negative number —
// reads as zero, which only ever sends the summing sweep to try.
func TestASeatCountReadsOffTheRowAndAnUnreadableOneIsZero(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		raw  any
		want int
	}{
		"written here":       {raw: 3, want: 3},
		"after a round trip": {raw: float64(3), want: 3},
		"zero, said":         {raw: float64(0), want: 0},
		"absent":             {raw: nil, want: 0},
		"a string":           {raw: "3", want: 0},
		"negative":           {raw: float64(-1), want: 0},
		"a fraction":         {raw: 2.5, want: 0},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			meta := map[string]any{}
			if tc.raw != nil {
				meta[HeldKey] = tc.raw
			}
			if got := FromMeta("node-a", meta).Held; got != tc.want {
				t.Fatalf("Held = %d, want %d", got, tc.want)
			}
		})
	}
}

// ── the broker kind on the wire ──────────────────────────────────────

// EVERY KIND A NODE ADVERTISES READS BACK AS ITSELF, through the JSON round
// trip the lease store puts every row through. A kind that came back as
// unknown would count a leaf as a member of every seal — safe, and a seal that
// waits on a node that can never acknowledge it.
func TestEveryAdvertisedBrokerKindRoundTrips(t *testing.T) {
	t.Parallel()
	for _, kind := range BrokerKinds() {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			raw, err := json.Marshal(NodeProfile{ID: "n1", Broker: kind}.Meta())
			if err != nil {
				t.Fatal(err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatal(err)
			}
			if got := FromMeta("n1", decoded).Broker; got != kind {
				t.Fatalf("advertised %q, read back %q", kind, got)
			}
		})
	}
}

// A ROW THAT DOES NOT SAY IS UNKNOWN, NEVER A LEAF — a row with no kind, a
// value of the wrong type, a kind a newer build added. A leaf is the one
// reading that would drop a member out of a capacity seal, so it is the one a
// row that says nothing must never be given; unknown is what a seal counts.
func TestABrokerKindTheRowDoesNotStateIsUnknownNeverALeaf(t *testing.T) {
	t.Parallel()
	for name, meta := range map[string]map[string]any{
		"a row with no kind":         {"roles": []any{"data"}},
		"no meta at all":             nil,
		"an empty string":            {"broker": ""},
		"a value of the wrong type":  {"broker": 7},
		"a list":                     {"broker": []any{"member"}},
		"a kind a newer build added": {"broker": "observer"},
		"another spelling":           {"broker": "Member"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := FromMeta("n1", meta).Broker
			if got != BrokerUnknown {
				t.Fatalf("read %q; a row that does not say must read as unknown", got)
			}
			if got.String() != "unknown" {
				t.Errorf("an unknown kind renders as %q, want \"unknown\" — an "+
					"empty cell reads as nothing to look at", got.String())
			}
		})
	}
}

// A PROFILE WITH NO KIND WRITES NONE, so absence stays the one way to say
// unknown and a profile nobody derived a kind for does not claim one.
func TestAnUnknownBrokerKindIsNotWritten(t *testing.T) {
	t.Parallel()
	if _, ok := (NodeProfile{ID: "n1"}).Meta()["broker"]; ok {
		t.Fatal("a profile with no broker kind advertised one")
	}
	if BrokerUnknown.Valid() {
		t.Fatal("unknown is valid: it would be written onto a presence row as a kind")
	}
}
