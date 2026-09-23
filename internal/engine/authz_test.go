package engine_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/iam"
)

// A NODE WITH NO COMPANY SAYS SO, RATHER THAN DEMOTING EVERY LEAD.
//
// The seam this replaces opened with `if c == nil || c.Org == nil { return
// false }`, and a node holds no company while it is booting, while it is
// installing a revision, and for as long as it is behind the chart log. Every
// one of those told a lead they lead nobody — a refusal naming the project
// rather than the lag, on a node reporting itself healthy.
//
// Asserted on EVERY method, because the collapse was written once per method
// and a repair on one is the shape that leaves the others.
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
	// AND THE DECISION IS UNKNOWN, which is what a surface renders as 503
	// rather than 403 — the whole reason the error exists.
	//
	// ASKED WITH THE POLICY VERB, because `write_project` itself is a
	// colleague write now and reads no chart at all: adding a label is
	// open to every seat, and only the lead-only facets ask a relation.
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
// A chart says the same authority two ways — `manages:` on a seat and `lead:`
// on a unit — and a check that read one would refuse half the leads in any
// company, with which half depending on how the founder wrote their chart.
func TestAChartAuthorityReadsBothTheManagesChainAndTheUnitLead(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{})
	chart := engine.ChartAuthorityOf(e)
	company := e.Company()
	if company == nil || company.Org == nil {
		t.Fatal("the fixture engine runs no company")
	}

	// EVERY SEAT IS ASKED ABOUT ITSELF, which must never be a lead
	// relation: the own-or-lead class checks self first, and a chart that
	// answered true here would make the two indistinguishable in the
	// reason a refusal reports.
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
	// it is a name somebody typed for a person who is not in this
	// company, and "you do not lead them" is exactly true.
	led, err := chart.Leads(t.Context(), "ceo", "nobody-here")
	if err != nil {
		t.Errorf("a subject the chart does not hold errored: %v", err)
	}
	if led {
		t.Error("a handle no seat has reads as led")
	}
}

// leadPrincipal is a seat principal with no grant, so the decision turns on
// the chart alone — the admin path is checked first and would hide it.
func leadPrincipal(handle string) iam.Principal {
	return iam.Principal{ID: uuid.New(), Login: handle, Kind: iam.KindSeat,
		Seat: handle, Stage: iam.StageActive}
}
