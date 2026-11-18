package engine_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/iam"
)

// A NODE WITH NO COMPANY SAYS SO, RATHER THAN DEMOTING EVERY LEAD: while
// booting or installing a revision, every method answers authz.ErrNoChart,
// never "you lead nobody". Asserted on EVERY method, since a collapse written
// once per method is repaired one method at a time.
func TestAChartAuthorityWithNoCompanyIsUnknownRatherThanFalse(t *testing.T) {
	t.Parallel()
	chart := engine.ChartAuthorityOf(nil)

	if _, err := chart.Leads(t.Context(), "cto", "sre"); !errors.Is(err, authz.ErrNoChart) {
		t.Errorf("Leads err = %v, want it to name the absent chart", err)
	}
	if _, err := chart.LeadsProject(t.Context(), "cto", "PLATFORM"); !errors.Is(err, authz.ErrNoChart) {
		t.Errorf("LeadsProject err = %v, want it to name the absent chart", err)
	}
	if _, err := chart.LeadsUnit(t.Context(), "cto", "sre"); !errors.Is(err, authz.ErrNoChart) {
		t.Errorf("LeadsUnit err = %v, want it to name the absent chart", err)
	}
	if _, err := chart.LeadsContainer(t.Context(), "cto", "RUNBOOKS"); !errors.Is(err, authz.ErrNoChart) {
		t.Errorf("LeadsContainer err = %v, want it to name the absent chart", err)
	}
	if _, err := chart.LeadsAnyone(t.Context(), "cto"); !errors.Is(err, authz.ErrNoChart) {
		t.Errorf("LeadsAnyone err = %v, want it to name the absent chart", err)
	}
	if _, err := chart.MemberOf(t.Context(), "sre", "platform"); !errors.Is(err, authz.ErrNoChart) {
		t.Errorf("MemberOf err = %v, want it to name the absent chart", err)
	}
	// AND THE DECISION IS UNKNOWN, which a surface renders as 503 rather
	// than 403. Asked with the policy verb: `write_project` reads no chart (a
	// label is open to every seat); only the lead-only facets ask a relation.
	d := authz.Decide(t.Context(), leadPrincipal("cto"), authz.ActionProjectPolicy,
		authz.Object{Kind: authz.KindProject, Container: "PLATFORM"}, chart,
		time.Now())
	if !d.Unknown() {
		t.Errorf("a node with no company decided %v (%q) rather than "+
			"reporting that it cannot tell", d.Allowed, d.Reason)
	}
}

// AND A RUNNING NODE ANSWERS BOTH RELATIONS A COMPANY CAN EXPRESS.
//
// A chart says the same authority two ways, `manages:` on a seat and `lead:` on
// a unit; a check reading one would refuse whichever leads the other wrote.
func TestAChartAuthorityReadsBothTheManagesChainAndTheUnitLead(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})
	chart := engine.ChartAuthorityOf(e)
	company := e.Company()
	if company == nil || company.Org == nil {
		t.Fatal("the fixture engine runs no company")
	}

	// A SEAT NEVER LEADS ITSELF: the own-or-lead class checks self first, and
	// a true here would blur the two in the reason a refusal reports.
	for role := range company.Org.AllRoles() {
		handle := role.Handle()
		led, err := chart.Leads(t.Context(), handle, handle)
		if err != nil {
			t.Fatalf("Leads(%s, %s): %v", handle, handle, err)
		}
		if led {
			t.Errorf("%s reads as leading itself", handle)
		}
	}

	// AND A HANDLE THE CHART DOES NOT HOLD IS AN ANSWER, NOT AN ERROR:
	// "you do not lead them" is exactly true of somebody not in the company.
	led, err := chart.Leads(t.Context(), "ceo", "nobody-here")
	if err != nil {
		t.Errorf("a subject the chart does not hold errored: %v", err)
	}
	if led {
		t.Error("a handle no seat has reads as led")
	}
}

// leadPrincipal is a seat principal holding the apps every agent seat holds and
// no admin, so the decision turns on the chart alone: an admin, or a caller
// short of a relation row's app, is decided before the chart is asked.
func leadPrincipal(handle string) iam.Principal {
	return iam.Principal{ID: uuid.New(), Login: handle, Kind: iam.KindSeat,
		Seat: handle, Stage: iam.StageActive, Grants: slices.Clone(iam.Apps)}
}
