package engine

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
)

// AN IDENTITY DUTY CLAIMS AT LEAST HOURLY AND RUNS ON ITS OWN INTERVAL.
//
// The probe's interval is an operator's setting up to a day, and a lease may
// live no longer than coord.MaxDutyTTL — every backend REFUSES a longer claim,
// and a refused duty never runs at all. So for every interval an operator may
// set, the lease has to fit, and the interval they set has to be the one the
// duty keeps: claimed on a cadence that divides it, run every so many claims.
func TestAnIdentityDutyClaimsAtLeastHourlyAndRunsOnItsInterval(t *testing.T) {
	t.Parallel()
	for every := config.DeactivationProbeFloor; every <= config.DeactivationProbeCeiling; every += time.Minute {
		cadence, claims := claimCadence(every)
		if cadence > identityClaimCeiling {
			t.Fatalf("an interval of %s claims every %s, past the hourly ceiling",
				every, cadence)
		}
		if ttl := identityDutyTTL(every); ttl > coord.MaxDutyTTL {
			t.Fatalf("an interval of %s takes a %s lease, which every backend refuses "+
				"— the duty would never run", every, ttl)
		}
		// THE OPERATOR'S INTERVAL IS THE ONE KEPT, to within the rounding
		// of one division.
		if kept := cadence * time.Duration(claims); kept > every ||
			every-kept >= time.Duration(claims) {
			t.Fatalf("an interval of %s runs every %s (%d claims of %s)", every,
				kept, claims, cadence)
		}
	}
	for _, tc := range []struct {
		every   time.Duration
		cadence time.Duration
		claims  int
	}{
		{5 * time.Minute, 5 * time.Minute, 1},
		{time.Hour, time.Hour, 1},
		{90 * time.Minute, 45 * time.Minute, 2},
		{24 * time.Hour, time.Hour, 24},
	} {
		if cadence, claims := claimCadence(tc.every); cadence != tc.cadence ||
			claims != tc.claims {
			t.Errorf("claimCadence(%s) = (%s, %d), want (%s, %d)", tc.every,
				cadence, claims, tc.cadence, tc.claims)
		}
	}
}

// A DUTY RUNS ON ITS FIRST HELD CLAIM AND THEN EVERY `claims` HELD CLAIMS, and a
// claim a peer won does not count.
//
// The first-run rule is what names a restore's duplicate the moment the
// restored node is back, and what lands a shred that failed during the outage;
// the peer rule is what stops a node that lost the lease for a while from
// running early the moment it regains it.
func TestAnIdentityDutyRunsOnItsFirstHeldClaimAndThenOnItsInterval(t *testing.T) {
	t.Parallel()
	plan := newPassSchedule(3)
	var ran []int
	for claim := 1; claim <= 7; claim++ {
		if plan.held() {
			ran = append(ran, claim)
		}
	}
	if len(ran) != 3 || ran[0] != 1 || ran[1] != 4 || ran[2] != 7 {
		t.Errorf("a duty claiming three times per run ran at claims %v, want "+
			"[1 4 7]", ran)
	}
}
