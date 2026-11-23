package googlegrowth_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/googlegrowth"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/seataccess"

	_ "github.com/crewlet/crewlet/internal/googleads"
	_ "github.com/crewlet/crewlet/internal/googleanalytics"
	_ "github.com/crewlet/crewlet/internal/googleplay"
	_ "github.com/crewlet/crewlet/internal/searchconsole"
)

// productsOf is every registered Google growth product's seat access.
func productsOf(t *testing.T) []googlegrowth.SeatAccess {
	t.Helper()
	var out []googlegrowth.SeatAccess
	for _, v := range seataccess.Registered() {
		switch a := v.(type) {
		case googlegrowth.SeatAccess:
			out = append(out, a)
		case *googlegrowth.SeatAccess:
			out = append(out, *a)
		}
	}
	if len(out) != 4 {
		t.Fatalf("%d Google growth products registered, want Analytics, Ads, Search Console and Play", len(out))
	}
	return out
}

// EVERY GOOGLE GROWTH LANE OFFERS THE SEAT'S OWN IDENTITY, STANDING, and
// offers Add it for me exactly where the product has an admin API for it: a
// lane whose catalog offered it with no admin calls behind it would refuse
// every press, and one with the calls and no offer would hide them.
//
// Mutation: set Adds on Search Console, and its lane offers a way it cannot
// take.
func TestCrewletAddsAnIdentityExactlyWhereTheProductLetsIt(t *testing.T) {
	t.Parallel()
	for _, v := range productsOf(t) {
		spec, _ := v.Catalog().Lane(v.Lane)
		id := spec.Identity
		if !id.Standing || id.Company || id.Default != seataccess.IdentityOwn || id.Ways[0] != seataccess.WayInvited {
			t.Errorf("%s: identity %+v, want the seat's own, standing, invited first", v.Name, id)
		}
		if made := slices.Contains(id.Ways, seataccess.WayMade); made != (v.Identity().Admin != nil) || made != v.Adds {
			t.Errorf("%s: offers Add it for me %v, has admin calls %v", v.Name, made, v.Identity().Admin != nil)
		}
	}
}

// EVERY LEVEL NEEDS A ROLE THE PRODUCT HAS, each level at least the one
// below it, so the check never asks for a role a person cannot give.
//
// Mutation: name operate's Analytics role "marketer_plus", and the check asks
// for a role Analytics does not have.
func TestEveryLevelNeedsARoleTheProductHas(t *testing.T) {
	t.Parallel()
	for _, v := range productsOf(t) {
		id := v.Identity()
		var below googlegrowth.Need
		for i, level := range growth.Levels() {
			need := id.Needs(level)
			if len(need.Roles) == 0 {
				t.Errorf("%s at %s needs no role", v.Name, level)
			}
			for _, r := range append(slices.Clone(need.Roles), need.Broad...) {
				if id.Roles.Name(r) == r {
					t.Errorf("%s at %s names %q, a role it does not have", v.Name, level, r)
				}
			}
			if i > 0 && len(id.Roles.Missing(need.Roles, below)) > 0 {
				t.Errorf("%s: %s needs less than the level below it", v.Name, level)
			}
			below = need
		}
	}
}

// THE ACCESS LOG SAYS WHOSE IDENTITY SIGNED A GOOGLE CALL, as the client
// says it: the seat's own for a product's client, the company's for a reader
// client, and nobody's for a consent product, whose lane offers no seat
// identity.
//
// Mutation: leave the record's signer unset, and every record names nobody.
func TestAGoogleCallIsRecordedAsSignedByItsClientsIdentity(t *testing.T) {
	t.Parallel()
	call := growth.Call{Tool: "google_play_read", Handle: "mobile-lead"}
	for _, signer := range []growth.Signer{growth.SignerOwn, growth.SignerCompany, ""} {
		c := &googlegrowth.Client{Kind: "google_play", Signer: signer}
		if got := c.Record(call, googlegrowth.Request{}, growth.ClassRead).Signer; got != signer {
			t.Errorf("a client signing as %q records %q", signer, got)
		}
	}
}
