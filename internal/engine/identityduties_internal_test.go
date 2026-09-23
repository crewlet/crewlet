package engine

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iamdomain"
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

// A DUPLICATED ADDRESS IS NEVER LOGGED BY ITS BLIND.
//
// An address's claim token is its keyed blind: the same value for the same
// address for the life of the company, so a log line carrying it hands
// whatever aggregates the logs a stable pseudonym to join on — and the address
// itself to anybody who ever holds the blind key. The claim report names an
// address by its kind alone, and the holders are what an operator acts on. A
// login and a seat are in the clear on every dashboard row, so theirs are
// logged; and a kind this build does not list is NOT, so a claim kind added
// later leaks nothing by default.
func TestADuplicatedAddressIsNeverLoggedByItsBlind(t *testing.T) {
	t.Parallel()
	const blind = "k7f3q9-the-keyed-blind-of-an-address"
	people := []string{"018f3a9c-0000-7000-8000-0000000000a1",
		"018f3a9c-0000-7000-8000-0000000000b2"}
	for _, tc := range []struct {
		kind      iamdomain.ObjectKind
		wantToken bool
	}{
		{iamdomain.KindEmail, false},
		{iamdomain.KindLogin, true},
		{iamdomain.KindSeat, true},
		{iamdomain.ObjectKind("a-kind-added-later"), false},
	} {
		attrs := duplicateAttrs(iamdomain.DuplicateClaim{
			Kind: tc.kind, Token: blind, People: people}, "1.42")
		carried, named := false, false
		for i := 0; i+1 < len(attrs); i += 2 {
			if attrs[i] == "token" {
				carried = true
			}
			if attrs[i] == "people" {
				named = true
			}
			if value, ok := attrs[i+1].(string); ok && value == blind &&
				!tc.wantToken {
				t.Errorf("%s: the claim token reached the log under %q", tc.kind,
					attrs[i])
			}
		}
		if carried != tc.wantToken {
			t.Errorf("%s: token logged = %v, want %v", tc.kind, carried, tc.wantToken)
		}
		if !named {
			t.Errorf("%s: the warning does not name the holders, which are "+
				"what an operator acts on", tc.kind)
		}
	}
}

// A CLAIM THIS NODE DID NOT HOLD NEITHER RUNS THE PASS NOR ADVANCES THE
// SCHEDULE.
//
// A duty that claims more often than it runs counts the claims it WON, so a
// claim a peer won — or one the store could not answer — must not be counted
// here: counted, a node that lost the lease for a while would run early the
// moment it won it back. The sequence below is three claims per run, with a
// lost claim and an unanswerable one before the first run is due again; held
// claims are the only ones counted, so the second run lands on the fourth HELD
// claim, which is the seventh tick.
func TestALostClaimNeitherRunsNorAdvancesTheSchedule(t *testing.T) {
	t.Parallel()
	blip := errors.New("coordination store: no responders")
	type answer struct {
		held bool
		err  error
	}
	claims := []answer{
		{held: true},  // 1: the first held claim runs
		{held: false}, // a peer won it
		{err: blip},   // nobody could say
		{held: true},  // held claim 2
		{held: true},  // held claim 3
		{held: false}, // a peer again
		{held: true},  // held claim 4: runs
		{held: true},  // held claim 5
	}
	next := 0
	passes := 0
	duty := identityDuty{
		name: "iam_test",
		claim: func(context.Context) (bool, error) {
			a := claims[next]
			next++
			return a.held, a.err
		},
		pass: func(context.Context) { passes++ },
	}
	plan := newPassSchedule(3)
	var ran []int
	for tick := 1; tick <= len(claims); tick++ {
		if tickIdentityDuty(t.Context(), duty, plan) {
			ran = append(ran, tick)
		}
	}
	if !slices.Equal(ran, []int{1, 7}) || passes != 2 {
		t.Errorf("ran at ticks %v (%d passes), want [1 7] — a claim this node "+
			"did not hold was counted towards its schedule", ran, passes)
	}
}
