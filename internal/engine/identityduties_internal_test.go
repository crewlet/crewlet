package engine

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// identityDutyRoster is every duty [Engine.identityDutiesFor] arms, with its
// interval. The duty gates read the duties here, not at the claim: the claim is
// made inside one helper as `e.workerDuty(name, identityDutyTTL(interval))`,
// where a syntax-tree scan sees the parameters, never the values. The TTL table
// (dutyTTLs), the name table (dutyNames) and the ceiling check below take them
// from this roster, and [TestTheIdentityDutyRosterIsWhatTheEngineArms] holds it
// to what the engine arms.
var identityDutyRoster = map[string]time.Duration{
	identitySweepDuty: IdentitySweepInterval,
}

// longestIdentityDutyTTL is the longest lease any identity duty claims.
func longestIdentityDutyTTL() time.Duration {
	longest := time.Duration(0)
	for _, every := range identityDutyRoster {
		longest = max(longest, identityDutyTTL(every))
	}
	return longest
}

// THE ROSTER IS WHAT THE ENGINE ARMS, in both directions: a duty armed but not
// in the roster has a lease no TTL gate checks and a name no duty table holds;
// a roster entry not armed is a duty the pages list though nothing runs it.
// Read off identityDutiesFor on a workers node with no coordination store,
// where every claim is nil and nothing starts.
// Mutation: arm a second duty, or the sweep at another interval, and this fails.
func TestTheIdentityDutyRosterIsWhatTheEngineArms(t *testing.T) {
	t.Parallel()
	e := &Engine{profile: placement.NodeProfile{
		ID: "n1", Roles: placement.Roles(placement.RoleData, placement.RoleWorkers),
	}}
	e.core.Store(&core{})
	armed := map[string]time.Duration{}
	for _, duty := range e.identityDutiesFor(&config.Bootstrap{}) {
		if _, twice := armed[duty.name]; twice {
			t.Errorf("identityDutiesFor arms %q twice", duty.name)
		}
		armed[duty.name] = duty.interval
	}
	if len(armed) == 0 {
		t.Fatal("identityDutiesFor armed nothing on a workers node, so the " +
			"roster cannot be compared with it")
	}
	if !maps.Equal(armed, identityDutyRoster) {
		t.Errorf("identityDutiesFor arms %v, and identityDutyRoster lists %v; "+
			"change the roster to match what the engine arms",
			armed, identityDutyRoster)
	}
}

// identityDutyNames is the roster's names, sorted.
func identityDutyNames() []string {
	return slices.Sorted(maps.Keys(identityDutyRoster))
}

// THE IDENTITY DUTY'S LEASE FITS THE DUTY CEILING: its lease is three
// intervals, and every backend REFUSES one longer than coord.MaxDutyTTL — a
// refused duty never runs. Mutation: arm one at a day and its lease is refused.
func TestEveryIdentityDutysLeaseFitsTheDutyCeiling(t *testing.T) {
	t.Parallel()
	for name, every := range identityDutyRoster {
		if ttl := identityDutyTTL(every); ttl > coord.MaxDutyTTL {
			t.Errorf("%s runs every %s and takes a %s lease, which every "+
				"backend refuses — the duty would never run", name, every, ttl)
		}
	}
}

// ONLY A CLAIM THIS NODE HOLDS RUNS THE PASS: a claim a peer won runs over
// there, and one the coordination store could not answer may be the peer's
// too — running either here runs the pass twice. Mutation: run on an
// unanswered claim and the third row passes.
func TestOnlyAHeldClaimRunsThePass(t *testing.T) {
	t.Parallel()
	blip := errors.New("coordination store: no responders")
	for _, tc := range []struct {
		name string
		held bool
		err  error
		want bool
	}{
		{"held", true, nil, true},
		{"a peer won it", false, nil, false},
		{"nobody could say", false, blip, false},
	} {
		duty := identityDuty{
			name: "iam_test",
			claim: func(context.Context) (bool, error) {
				return tc.held, tc.err
			},
		}
		if got := mine(t.Context(), duty); got != tc.want {
			t.Errorf("%s: mine = %v, want %v", tc.name, got, tc.want)
		}
	}
}
