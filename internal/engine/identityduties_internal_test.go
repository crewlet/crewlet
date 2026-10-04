package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// THE IDENTITY DUTY'S LEASE FITS THE DUTY CEILING.
//
// A duty claims once per tick on a lease of three intervals, and every backend
// REFUSES a lease longer than coord.MaxDutyTTL — a refused duty never runs at
// all. So every interval a duty is armed at has to fit three times under the
// ceiling. Mutation: arm one at a day and its lease is refused.
func TestEveryIdentityDutysLeaseFitsTheDutyCeiling(t *testing.T) {
	t.Parallel()
	for name, every := range map[string]time.Duration{
		identitySweepDuty: IdentitySweepInterval,
	} {
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
