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

// identityDutyRoster is every duty [Engine.identityDutiesFor] arms, with the
// interval it runs at.
//
// THE DUTY GATES READ THESE DUTIES HERE rather than at the claim, because the
// claim is made once, inside the helper every identity duty goes through, as
// `e.workerDuty(name, identityDutyTTL(interval))` — a syntax-tree scan sees
// the parameters, never the values. So the TTL table (dutyTTLs), the name
// table (dutyNames) and the ceiling check below all take them from this one
// roster, and [TestTheIdentityDutyRosterIsWhatTheEngineArms] holds the roster
// to what the engine actually arms.
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

// THE ROSTER IS WHAT THE ENGINE ARMS, in both directions.
//
// A duty armed and missing from the roster is one whose lease no TTL gate
// checks and whose name no duty table is held to; a roster entry the engine
// does not arm is a duty the pages would be made to list although nothing
// runs it. Read off identityDutiesFor itself, on a workers node with no
// coordination store, where every claim is nil and nothing is started.
// Mutation: arm a second duty in identityDutiesFor, or arm the sweep at
// another interval, and this fails.
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

// THE IDENTITY DUTY'S LEASE FITS THE DUTY CEILING.
//
// A duty claims once per tick on a lease of three intervals, and every backend
// REFUSES a lease longer than coord.MaxDutyTTL — a refused duty never runs at
// all. So every interval a duty is armed at has to fit three times under the
// ceiling. Mutation: arm one at a day and its lease is refused.
func TestEveryIdentityDutysLeaseFitsTheDutyCeiling(t *testing.T) {
	t.Parallel()
	for name, every := range identityDutyRoster {
		if ttl := identityDutyTTL(every); ttl > coord.MaxDutyTTL {
			t.Errorf("%s runs every %s and takes a %s lease, which every "+
				"backend refuses — the duty would never run", name, every, ttl)
		}
	}
}

// ONLY A CLAIM THIS NODE HOLDS RUNS THE PASS.
//
// A duty is a fleet singleton: a claim a peer won is a pass that runs over
// there, and one the coordination store could not answer is a pass nobody can
// say is this node's — running it on either would run it twice. Mutation: run
// on an unanswered claim and the third row passes.
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
