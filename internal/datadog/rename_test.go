package datadog_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/datadog"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provision"
)

// A RENAMED SEAT KEEPS THE DATADOG ACCOUNT IT IS ALREADY WORKING AS.
//
// Datadog is the sharpest of the four, because its key for a seat is the
// account's ADDRESS and Datadog will not change one: an address derived from
// the live handle is unreachable the moment somebody renames the seat, with
// no gesture that can bring it back. This package already described what that
// costs, at orphanedAccounts — "a renamed seat's identity, live, holding
// whatever it held, matching nothing any pass will ever ask for again" — and
// could report it and nothing more. Derived from the origin, there is nothing
// to report: the address never moves.
func TestARenamedSeatKeepsItsAccountAddress(t *testing.T) {
	t.Parallel()
	cfg := &config.Datadog{
		Enabled:      true,
		Provisioning: &config.DatadogProvisioning{EmailDomain: "agents.example.com"},
	}

	before := datadogSeat("SRE")
	after := datadogSeat("SRE")
	after.DeclaredHandle, after.OriginHandle = "platform-sre", "sre"

	was := datadogPlanSeat(t, before, cfg)
	is := datadogPlanSeat(t, after, cfg)

	if was.Handle == is.Handle {
		t.Fatalf("the fixture did not rename the seat: both plans say %q", was.Handle)
	}
	if is.Handle != "platform-sre" {
		t.Errorf("plan handle = %q, want the seat as the chart addresses it today", is.Handle)
	}
	// THE ADDRESS IS THE ACCOUNT. It is what the pass looks a seat up by,
	// what the teardown disables by, and what the orphan sweep claims by.
	if is.Email != was.Email {
		t.Errorf("account address after the rename = %q, want the one the "+
			"account already has, %q — Datadog cannot change an address, so "+
			"the old account is live and unreachable for ever", is.Email, was.Email)
	}
	if got, want := datadog.AccountEmail(cfg.Provisioning, is.Origin), was.Email; got != want {
		t.Errorf("AccountEmail over the planned origin = %q, want %q", got, want)
	}
}

func datadogSeat(name string) *org.Role {
	return &org.Role{Name: name, MCPEnv: map[string]map[string]string{
		datadog.SeatEnv: {"DD_APP_KEY": "${DD_APP_KEY_" + name + "}"},
	}}
}

// datadogPlanSeat is the one seat datadog.PlanFor makes of a role.
func datadogPlanSeat(t *testing.T, role *org.Role, cfg *config.Datadog) provision.Seat {
	t.Helper()
	plan, err := datadog.PlanFor(&org.Organization{
		Name: "Nimbus", Roles: []*org.Role{role},
	}, cfg)
	if err != nil {
		t.Fatalf("PlanFor: %v", err)
	}
	if len(plan.Seats) != 1 {
		t.Fatalf("planned %d seats, want 1: %+v", len(plan.Seats), plan)
	}
	return plan.Seats[0]
}
