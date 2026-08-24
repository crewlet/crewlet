package engine

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/seat/placement"
)

// A NODE THAT DOES NOT RUN WORKERS REFUSES EVERY DUTY, and refuses rather
// than abstaining: the duty function's nil means "no fleet, so always mine",
// which is the exact opposite of what `roles: [seats]` asks for.
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

// A WORKER WITH NO COORDINATION STORE IS THE SINGLE-NODE CASE: nil, because
// there is nobody to be a singleton among, and a wrapper that always said
// yes would make a lone node report itself as a fleet member.
func TestALoneWorkerNodeGetsNoDutyFunctionAtAll(t *testing.T) {
	t.Parallel()
	e := &Engine{profile: placement.NodeProfile{
		ID: "n1", Roles: placement.Roles(placement.RoleWorkers),
	}}
	if duty := e.workerDuty("maintenance", time.Minute); duty != nil {
		t.Fatal("a node with no coordination store got a claim function")
	}
}

// THE DEFAULT IS EVERY ROLE, which is what makes a single-node deployment
// work with no `node:` block at all — and what stops this gate turning every
// existing deployment into one that sweeps nothing.
func TestAnUnconfiguredNodeRunsWorkerDuties(t *testing.T) {
	t.Parallel()
	e := &Engine{profile: placement.NodeProfile{ID: "n1", Roles: nodeRoles(nil)}}
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
		p := placement.NodeProfile{ID: "n1", Roles: nodeRoles(tc.names)}
		if p.RunsSeats() != tc.seats || p.RunsWorkers() != tc.workers ||
			p.RunsIngress() != tc.ingress {
			t.Errorf("%v: seats=%v workers=%v ingress=%v, want %v/%v/%v",
				tc.names, p.RunsSeats(), p.RunsWorkers(), p.RunsIngress(),
				tc.seats, tc.workers, tc.ingress)
		}
	}
}
