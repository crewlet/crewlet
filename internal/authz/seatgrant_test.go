package authz_test

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// A SEAT GRANT DECIDES AN AGENT, AND THE CLASS DECIDES EVERYBODY ELSE.
//
// On every row naming a grant: the seat whose role carries it is admitted on
// the grant, naming it and no permission; a seat whose role does not is
// refused as not granted, naming the grant and no permission — admin among
// what it holds, because no permission a seat could hold is what admits it;
// a chart that cannot say is unknown, never a refusal; and a person is the
// class's, admitted on admin and refused naming admin without it. Each case
// asks one fact the arm reads, so a mutation that ignores the grant (deciding
// a seat by the class), reads it for a person, or turns the chart's silence
// into a no, turns one of them red.
func TestASeatGrantDecidesAnAgentAndTheClassEverybodyElse(t *testing.T) {
	t.Parallel()
	walked := 0
	for _, a := range authz.Actions() {
		grant, _ := authz.SeatGrantOf(a)
		if grant == "" {
			continue
		}
		walked++
		object := authz.Object{Kind: authz.KindCompany}

		granted := authz.Decide(t.Context(), seat("sre"), a, object, nimbus(), decidedAt)
		if !granted.Allowed || granted.Reason != authz.ReasonSeatGrant ||
			granted.SeatGrant != grant || len(granted.Grants) != 0 {
			t.Errorf("%s: the seat whose role carries %s decided %+v, want admitted "+
				"on the grant, naming it and no permission", a, grant, granted)
		}
		for name, p := range map[string]iam.Principal{
			"holding nothing": seat("cto"),
			"holding admin":   seat("cto", iam.GrantAdmin),
		} {
			d := authz.Decide(t.Context(), p, a, object, nimbus(), decidedAt)
			if d.Allowed || d.Unknown() || d.Reason != authz.ReasonNotGranted ||
				d.SeatGrant != grant || len(d.Grants) != 0 {
				t.Errorf("%s: a seat %s whose role does not carry %s decided %+v, "+
					"want refused as not granted, naming the grant and no permission",
					a, name, grant, d)
			}
		}
		blind := chart{err: errors.New("the chart is behind")}
		if d := authz.Decide(t.Context(), seat("sre"), a, object, blind, decidedAt); !d.Unknown() ||
			d.SeatGrant != grant {
			t.Errorf("%s: a seat on a node whose chart cannot answer decided %+v, "+
				"want unknown naming the grant — never a refusal", a, d)
		}

		admin := authz.Decide(t.Context(), person("jane.doe", iam.GrantAdmin), a, object,
			nimbus(), decidedAt)
		if !admin.Allowed || admin.Reason != authz.ReasonGrant || admin.SeatGrant != "" {
			t.Errorf("%s: a person holding admin decided %+v, want admitted by the "+
				"class, naming no grant", a, admin)
		}
		// A PERSON WHOSE OWN SEAT'S ROLE CARRIES THE GRANT is still the
		// class's: the grant is an agent's, and a person bound to `sre` holds
		// no more for it.
		bound := personLeading("sre")
		if d := authz.Decide(t.Context(), bound, a, object, nimbus(), decidedAt); d.Allowed ||
			d.Reason != authz.ReasonNoGrant || len(d.Grants) != 1 || d.Grants[0] != iam.GrantAdmin {
			t.Errorf("%s: a person bound to a granted seat decided %+v, want refused "+
				"by the class naming admin", a, d)
		}
	}
	if walked == 0 {
		t.Fatal("no row names a seat grant, so this walk certifies nothing")
	}
}

// NO ROW NAMES A GRANT IT COULD NEVER DECIDE ON.
//
// A grant beside human-only reaches no agent at all, so it reads as a
// decision and decides nothing; beside seat-only, the class is left deciding
// nobody, which is the same dead half the other way round. And a grant this
// build does not know is one no role can carry, which admits nobody. Mutation:
// mark fleet_health human-only and this goes red.
func TestNoRowNamesASeatGrantItCouldNeverDecideOn(t *testing.T) {
	t.Parallel()
	for _, a := range authz.Actions() {
		grant, _ := authz.SeatGrantOf(a)
		if grant == "" {
			continue
		}
		if !grant.Valid() {
			t.Errorf("%s names the grant %q, which this build does not know", a, grant)
		}
		if humanOnly, _ := authz.HumanOnlyOf(a); humanOnly {
			t.Errorf("%s names the grant %s and is human-only, so no agent ever "+
				"reaches the grant", a, grant)
		}
		if seatOnly, _ := authz.SeatOnlyOf(a); seatOnly {
			t.Errorf("%s names the grant %s and is seat-only, so its class decides "+
				"nobody", a, grant)
		}
	}
	if _, known := authz.SeatGrantOf("no.such.verb"); known {
		t.Error("SeatGrantOf knows a verb the table has no row for")
	}
	if authz.SeatGrant("").Valid() || authz.SeatGrant("root").Valid() {
		t.Error("an empty or unknown grant reads as one this build knows")
	}
}
