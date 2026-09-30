package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/statelog"
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

// A PARTITION'S DUTY IS NAMED FOR ITS PARTITION, AND LAYOUT 0's KEEPS ITS NAME.
//
// Each partition's trim, embedding and repairs are its own singleton, so the
// lease names the partition — `retention@tracker.007`. Layout 0's one
// partition keeps the name the duty has always had, byte for byte: a node on
// this build and one on the build before it must contend for ONE lease, or
// both run the duty for the length of a rolling upgrade.
func TestAPartitionsDutyIsNamedForItsPartition(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		duty string
		p    statelog.PartitionID
		want string
	}{
		{retentionDutyName, statelog.EstatePartition, "retention"},
		{embedDutyName, statelog.EstatePartition, "embeddings"},
		{retentionDutyName, statelog.PartitionID{Space: statelog.SpaceTracker, Index: 7},
			"retention@tracker.007"},
		{embedDutyName, statelog.PartitionID{Space: statelog.SpacePages, Index: 12},
			"embeddings@pages.012"},
	} {
		got := partitionDutyName(tc.duty, tc.p)
		if got != tc.want {
			t.Errorf("the %s duty of %s is named %q, want %q", tc.duty, tc.p, got, tc.want)
		}
		if err := coord.CheckResource(coord.WorkerResource(got)); err != nil {
			t.Errorf("the %s duty of %s names no lease: %v", tc.duty, tc.p, err)
		}
	}
}

// A PARTITION'S DUTY IS CLAIMED ONLY WHILE THIS NODE SERVES THE PARTITION.
//
// Its work is decided from the partition's rows and published to its logs, so a
// node that does not serve the partition — holds none of it, is still joining,
// has begun to leave — never claims it, and one that cannot tell whether it
// serves claims nothing and says why. Only then is the fleet's own gate asked:
// a node that runs no workers still refuses.
func TestAPartitionsDutyIsClaimedOnlyWhileTheNodeServesIt(t *testing.T) {
	t.Parallel()
	p := statelog.PartitionID{Space: statelog.SpaceTracker, Index: 3}
	lone := &Engine{profile: placement.NodeProfile{ID: "n1", Roles: placement.Roles(placement.RoleWorkers)}}
	seatsOnly := &Engine{profile: placement.NodeProfile{ID: "n1", Roles: placement.Roles(placement.RoleSeats)}}
	unknown := errors.New("the estate view has not been read")
	for _, tc := range []struct {
		name    string
		e       *Engine
		holding statelog.Holding
		mine    bool
		err     bool
	}{
		{"serving, alone", lone, statelog.ServesOnly(p), true, false},
		{"not serving", lone, statelog.ServesOnly(statelog.PartitionID{Space: statelog.SpaceTracker}), false, false},
		{"serving nothing", lone, statelog.ServesOnly(), false, false},
		{"cannot tell", lone, unknownHolding{unknown}, false, true},
		{"serving, but runs no workers", seatsOnly, statelog.ServesOnly(p), false, false},
	} {
		mine, err := tc.e.partitionDuty(retentionDutyName, retentionDutyTTL, p, tc.holding)(t.Context())
		if mine != tc.mine || (err != nil) != tc.err {
			t.Errorf("%s: claimed %v (%v), want %v with an error %v", tc.name, mine, err, tc.mine, tc.err)
		}
		if tc.err && !errors.Is(err, unknown) {
			t.Errorf("%s: the refusal %v does not carry why", tc.name, err)
		}
	}
}

// unknownHolding is a node that cannot tell what it serves.
type unknownHolding struct{ err error }

func (u unknownHolding) Serving(statelog.PartitionID) (bool, error) { return false, u.err }

// THE TRIM CLAIMS A DUTY PER PARTITION IT RUNS, EACH ONCE.
//
// A partition's logs are trimmed only by the holder of that partition's duty,
// so a tick asks for the duty of every partition this node runs a log of —
// once however many logs the partition carries — and evaluates the logs of the
// partitions it was given and no other. A partition whose claim could not be
// answered is skipped, not assumed.
func TestTheTrimClaimsADutyPerPartitionItRuns(t *testing.T) {
	t.Parallel()
	layout := statelog.Layout{Number: 1, Spaces: []statelog.SpaceLayout{
		{Space: statelog.SpaceTracker, Partitions: 3, Domains: []string{"tracker", "vectors"}},
	}}
	var ids []statelog.LogID
	for _, p := range layout.Partitions() {
		ids = append(ids, layout.Logs(p)...)
	}
	s := censusLogs(layout, ids...)
	asked := map[statelog.PartitionID]int{}
	r := &retention{state: s, claim: func(_ context.Context, p statelog.PartitionID) (bool, error) {
		asked[p]++
		switch p.Index {
		case 0:
			return true, nil
		case 1:
			return false, nil
		}
		return false, errors.New("the store did not answer")
	}}
	mine := r.claimed(t.Context())
	for _, p := range layout.Partitions() {
		if asked[p] != 1 {
			t.Errorf("%s's duty was asked for %d times in one tick, want once", p, asked[p])
		}
	}
	if len(mine) != 1 || !mine[statelog.PartitionID{Space: statelog.SpaceTracker}] {
		t.Errorf("the tick holds the duties of %v, want tracker.000's alone", mine)
	}
}
