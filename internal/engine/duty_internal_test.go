package engine

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// A NODE THAT DOES NOT RUN WORKERS REFUSES EVERY DUTY, never abstains: a nil
// duty means "no fleet, so always mine", the opposite of `roles: [seats]`.
func TestANodeWithoutTheWorkersRoleRefusesEveryDuty(t *testing.T) {
	t.Parallel()
	e := &Engine{profile: placement.NodeProfile{
		ID: "n1", Roles: placement.Roles(placement.RoleSeats),
	}}
	duty := e.workerDuty("maintenance", time.Minute)
	if duty == nil {
		t.Fatal("a non-worker node got a nil duty, which every caller reads " +
			"as 'always mine'")
	}
	holds, err := duty(t.Context())
	if err != nil {
		t.Fatalf("duty: %v", err)
	}
	if holds {
		t.Fatal("a node told to run only seats claimed a worker duty")
	}
}

// A WORKER WITH NO COORDINATION STORE IS THE SINGLE-NODE CASE: nil, since a
// wrapper that always said yes would report a lone node as a fleet member.
func TestALoneWorkerNodeGetsNoDutyFunctionAtAll(t *testing.T) {
	t.Parallel()
	e := &Engine{profile: placement.NodeProfile{
		ID: "n1", Roles: placement.Roles(placement.RoleWorkers),
	}}
	if duty := e.workerDuty("maintenance", time.Minute); duty != nil {
		t.Fatal("a node with no coordination store got a claim function")
	}
}

// THE DEFAULT IS EVERY ROLE, so a single-node deployment with no `node:` block
// still runs its duties.
func TestAnUnconfiguredNodeRunsWorkerDuties(t *testing.T) {
	t.Parallel()
	e := &Engine{profile: (&config.Bootstrap{}).Profile("n1")}
	if !e.profile.RunsWorkers() {
		t.Fatal("a node that declared no roles does not run worker duties")
	}
	if duty := e.workerDuty("maintenance", time.Minute); duty != nil {
		t.Fatal("the single-node case should have no claim function")
	}
}

func TestDeclaredRolesAreResolvedAsWritten(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		names                   []string
		seats, workers, ingress bool
	}{
		{names: nil, seats: true, workers: true, ingress: true},
		{names: []string{"seats"}, seats: true},
		{names: []string{"workers"}, workers: true},
		{names: []string{"ingress"}, ingress: true},
		{names: []string{"seats", "ingress"}, seats: true, ingress: true},
	} {
		p := (&config.Bootstrap{Node: config.Node{Roles: tc.names}}).Profile("n1")
		if p.RunsSeats() != tc.seats || p.RunsWorkers() != tc.workers ||
			p.RunsIngress() != tc.ingress {
			t.Errorf("%v: seats=%v workers=%v ingress=%v, want %v/%v/%v",
				tc.names, p.RunsSeats(), p.RunsWorkers(), p.RunsIngress(),
				tc.seats, tc.workers, tc.ingress)
		}
	}
}
