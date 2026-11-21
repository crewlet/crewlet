package engine

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A BRANCH IS PUSHED BY ONE OF A SEAT'S RUNS AT A TIME (ADR-0169). Two runs of
// one seat, each defaulting to seats/<handle>/<item>, would push over each
// other where no protection rule refuses a force; so a checkout to push a
// branch another of the seat's holding runs pushes is refused, naming it. The
// run's own grant, a parked run's, another branch, another seat's run, and a
// read checkout all go.
//
// Mutation: drop the check and the second checkout is granted; count a parked
// run and the resumed run's own branch is refused while it waits.
func TestACheckoutOfABranchAnotherRunPushesIsRefused(t *testing.T) {
	t.Parallel()
	web := func(branch string, access sandbox.RepoAccess) sandbox.RunRepo {
		return sandbox.RunRepo{Repo: "web", Branch: branch, Access: access}
	}
	store := fakeSeatRuns{runs: []sandbox.PendingRun{
		{TurnID: "t1", AgentHandle: "swe", Status: sandbox.StatusRunning,
			Repos: []sandbox.RunRepo{web("seats/swe/ENG-1", sandbox.RepoWrite)}},
		{TurnID: "t3", AgentHandle: "swe", Status: sandbox.StatusAwaiting,
			Repos: []sandbox.RunRepo{web("seats/swe/ENG-3", sandbox.RepoWrite)}},
		{TurnID: "o1", AgentHandle: "qa", Status: sandbox.StatusRunning,
			Repos: []sandbox.RunRepo{web("seats/swe/ENG-2", sandbox.RepoWrite)}},
	}}
	me := sandbox.PendingRun{TurnID: "t2", AgentHandle: "swe", Status: sandbox.StatusRunning}

	err := branchWorkedBeside(t.Context(), store, me, web("seats/swe/ENG-1", sandbox.RepoWrite))
	if !errors.Is(err, errBranchWorked) {
		t.Fatalf("a checkout of the branch another running run of the seat pushes = %v, want errBranchWorked", err)
	}
	for name, repo := range map[string]sandbox.RunRepo{
		"another branch":        web("seats/swe/ENG-9", sandbox.RepoWrite),
		"a parked run's branch": web("seats/swe/ENG-3", sandbox.RepoWrite),
		"another seat's branch": web("seats/swe/ENG-2", sandbox.RepoWrite),
		"a read of the run's":   web("seats/swe/ENG-1", sandbox.RepoRead),
	} {
		if err := branchWorkedBeside(t.Context(), store, me, repo); err != nil {
			t.Errorf("%s = %v, want granted", name, err)
		}
	}
	own := sandbox.PendingRun{TurnID: "t1", AgentHandle: "swe", Status: sandbox.StatusRunning}
	if err := branchWorkedBeside(t.Context(), store, own, web("seats/swe/ENG-1", sandbox.RepoWrite)); err != nil {
		t.Fatalf("the run's own branch checked out again = %v, want granted", err)
	}
}
