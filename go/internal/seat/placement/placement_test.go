// The placement arithmetic, which is the part that is easy to get wrong.
//
// Placement is not a filter you bolt onto a fleet-wide fair share. The share
// has to be computed per placement group, over the nodes eligible for that
// group — and the test that proves it is
// TestComputePinnedMajorityIsNotStrandedByAFleetWideRatio: under the naive
// ratio every node reports a healthy sweep while five seats are served by
// nobody.
package placement

import (
	"encoding/json"
	"errors"
	"maps"
	"slices"
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
			want:   []string{"ingress", "seats", "workers"},
		},
		{
			name:   "empty is every role",
			values: []string{},
			want:   []string{"ingress", "seats", "workers"},
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
	for _, role := range []NodeRole{RoleIngress, RoleSeats, RoleWorkers} {
		if !unset.Has(role) {
			t.Fatalf("the unset role set must have %q", role)
		}
	}
	if !unset.Equal(DefaultRoles()) {
		t.Fatalf("unset = %v, want every role", unset.Names())
	}

	zero := NodeProfile{ID: "n1"}
	if !zero.RunsSeats() || !zero.RunsWorkers() || !zero.RunsIngress() {
		t.Fatalf("a profile with no declared roles must do everything, got %v", zero.Roles.Names())
	}

	declared := NodeProfile{ID: "api", Roles: Roles(RoleIngress)}
	if declared.RunsSeats() {
		t.Fatal("an ingress-only node must not run seats")
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
		Roles:  Roles(RoleSeats, RoleWorkers),
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

// A peer running a build that predates the field. "Does everything, labelled
// with nothing" is the only safe reading — the alternative is a node with no
// roles, which drops a live peer out of the denominator and over-subscribes
// the rest of the fleet.
func TestFromMetaWithNoMetaReadsAsTheOldBehaviour(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		meta map[string]any
	}{
		{"absent", nil},
		{"empty", map[string]any{}},
		{"other fields only", map[string]any{"build": "v3"}},
		{"explicit nulls", map[string]any{"roles": nil, "labels": nil}},
		{"an empty role list", map[string]any{"roles": []any{}}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			back := FromMeta("old", tc.meta)
			if !back.Roles.Equal(DefaultRoles()) {
				t.Fatalf("roles = %v, want every role", back.Roles.Names())
			}
			if len(back.Labels) != 0 {
				t.Fatalf("labels = %v, want none", back.Labels)
			}
			if !back.RunsSeats() {
				t.Fatal("an older peer must still count as a seat runner")
			}
			if back.ID != "old" {
				t.Fatalf("id = %q, want %q", back.ID, "old")
			}
		})
	}
}

// A peer's bad row must not take down the reader's sweep, and there is only
// one safe reading of one.
func TestFromMetaMalformedReadsAsTheOldBehaviourToo(t *testing.T) {
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
func TestComputeWithNoPlacementIsTheOldArithmetic(t *testing.T) {
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

// What makes the give-back settle instead of oscillating: a ceiling per
// group means a node that has shed down to its share has no room to
// immediately re-claim what it let go.
//
// The invariant is fleet-wide — the shares must cover every placeable seat,
// or a seat is stranded with every sweep reporting healthy.
func TestComputeSharesCoverEveryPlaceableSeat(t *testing.T) {
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

			total := 0
			var unplaceable []string
			for i, me := range tc.fleet {
				plan := Compute(tc.seats, me, tc.fleet)
				total += plan.Capacity

				// Every node must reach the same verdict about the company
				// from the same membership read; two nodes disagreeing about
				// what is unplaceable is a company nobody can diagnose.
				if i == 0 {
					unplaceable = plan.Unplaceable
					continue
				}
				assertHandles(t, "unplaceable", plan.Unplaceable, unplaceable)
			}

			placeable := len(tc.seats) - len(unplaceable)
			if total < placeable {
				t.Fatalf("shares sum to %d, below the %d placeable seats — seats are stranded", total, placeable)
			}
		})
	}
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
