package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// EVERY IDENTITY DUTY'S LEASE FITS THE DUTY CEILING.
//
// A duty claims once per tick on a lease of three intervals, and every backend
// REFUSES a lease longer than coord.MaxDutyTTL — a refused duty never runs at
// all. So every interval a duty is armed at has to fit three times under the
// ceiling. Mutation: arm one at a day and its lease is refused.
func TestEveryIdentityDutysLeaseFitsTheDutyCeiling(t *testing.T) {
	t.Parallel()
	for name, every := range map[string]time.Duration{
		identitySweepDuty:  IdentitySweepInterval,
		identityClaimsDuty: IdentityClaimsInterval,
	} {
		if ttl := identityDutyTTL(every); ttl > coord.MaxDutyTTL {
			t.Errorf("%s runs every %s and takes a %s lease, which every "+
				"backend refuses — the duty would never run", name, every, ttl)
		}
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
