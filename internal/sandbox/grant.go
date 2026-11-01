package sandbox

import (
	"context"
	"errors"
	"fmt"
)

// A RUN IS GRANTED ITS REPOSITORIES ONE CHECKOUT AT A TIME (ADR-0086): its
// launch gives it none, and every `code_checkout` its box calls through the
// bridge resolves one repository for the seat and adds it to the job's row
// ([PendingStore.GrantRepo]). The code host's box admission reads the row on
// every request, so a repository checked out is reachable from the next
// request on, and one never checked out never is.

// RepoGrant is one repository a running job is given: its grant, resolved
// for the seat by the engine, the position that resolution read at — the
// run's first code floor where it has none yet, and never below the one it
// has — and the colleague whose grant bounds the box ([PendingRun.CodeCeiling]).
type RepoGrant struct {
	Repo RunRepo
	// Floor is the resolution's position, as statelog.Position's token.
	// Required: a grant read at no position would let the box read below
	// what it was shown.
	Floor string
	// Raise is the caller's judgement of the floor the row should hold:
	// handed the row's floor as it stands, it answers the higher of that and
	// Floor, or false where Floor raises nothing. The store compares no
	// positions ([PendingRun.CodeFloor]).
	Raise func(current string) (next string, ok bool)
	// Ceiling is the colleague seat whose ask woke the turn, or "" for none.
	Ceiling string
}

// GrantOutcome is what [PendingStore.GrantRepo] did.
type GrantOutcome string

const (
	// GrantAdded is a repository added to the job's grants.
	GrantAdded GrantOutcome = "added"
	// GrantHeld is a repository the job was given already, on the same
	// branch with the same access: nothing is written, and the row's grant
	// is answered, so a box that lost its clone checks it out again.
	GrantHeld GrantOutcome = "held"
	// GrantConflict is a repository the job was given on another branch or
	// with another access, or a ceiling other than the one its grants are
	// bounded by: one run works on one branch of a repository, under one
	// requester.
	GrantConflict GrantOutcome = "conflict"
	// GrantFull is a job given [MaxRunRepos] repositories already.
	GrantFull GrantOutcome = "full"
	// GrantNotRunning is a row that does not hold this job launching or
	// running: the job ended, was stopped or was replaced, and its box is
	// given nothing.
	GrantNotRunning GrantOutcome = "not_running"
)

// Validate refuses a grant no row could carry: a repository that is not a
// coding run's ([RunRepo.ValidateFor] [CloneCoding]), or one read at no
// position, or no way to raise the floor by it.
func (g RepoGrant) Validate() error {
	switch {
	case g.Floor == "":
		return errors.New("sandbox: a repository granted to a run needs the position it was resolved at")
	case g.Raise == nil:
		return errors.New("sandbox: a repository granted to a run needs its floor's raise")
	}
	return g.Repo.ValidateFor(CloneCoding)
}

// GrantRepo adds one repository to a running job's grants. See the contract
// on [PendingStore].
func (s *CoordStore) GrantRepo(ctx context.Context, turnID, launchID string, g RepoGrant,
) (RunRepo, GrantOutcome, error) {
	if launchID == "" {
		return RunRepo{}, GrantNotRunning, nil
	}
	if err := g.Validate(); err != nil {
		return RunRepo{}, "", err
	}
	// WHY NOT, as the decision that last ran saw it — never "added": only
	// the store's own answer says a write landed (see RecordAsk).
	outcome, answered := GrantNotRunning, RunRepo{}
	_, won, err := s.mutateLive(ctx, turnID, func(run *PendingRun) bool {
		answered = RunRepo{}
		if run.LaunchID != launchID || (run.Status != StatusLaunching && run.Status != StatusRunning) {
			outcome = GrantNotRunning
			return false
		}
		if len(run.Repos) > 0 && run.CodeCeiling != g.Ceiling {
			outcome = GrantConflict
			return false
		}
		for i, held := range run.Repos {
			if held.RepoID != g.Repo.RepoID && held.Repo != g.Repo.Repo {
				continue
			}
			answered = held
			if held.RepoID != g.Repo.RepoID || held.Branch != g.Repo.Branch || held.Access != g.Repo.Access {
				outcome = GrantConflict
				return false
			}
			outcome = GrantHeld
			if held.Repo == g.Repo.Repo {
				return false
			}
			// RENAMED SINCE IT WAS CHECKED OUT: the grant follows the
			// repository's id, and the box clones and is admitted by the
			// name it has now.
			run.Repos[i].Repo = g.Repo.Repo
			answered = run.Repos[i]
			return true
		}
		if len(run.Repos) >= MaxRunRepos {
			outcome = GrantFull
			return false
		}
		next, raised := g.Raise(run.CodeFloor)
		switch {
		case raised && next != "":
			run.CodeFloor = next
		case run.CodeFloor == "":
			run.CodeFloor = g.Floor
		}
		// EVERY PUSH OF THE BOX APPLIES ABOVE ITS FIRST CHECKOUT, which no
		// box can push before: the launch's floor ([PendingRun.CodeLaunch])
		// is set once, by the first grant, and never raised.
		if run.CodeLaunch == "" {
			run.CodeLaunch = g.Floor
		}
		run.Repos = append(run.Repos, g.Repo)
		run.CodeCeiling = g.Ceiling
		outcome, answered = GrantAdded, g.Repo
		return true
	})
	switch {
	case errors.Is(err, ErrRunEnding):
		return RunRepo{}, GrantNotRunning, nil
	case err != nil:
		return RunRepo{}, "", fmt.Errorf("sandbox: grant repository %s to run %s: %w", g.Repo.Repo, turnID, err)
	case won || outcome == GrantHeld || outcome == GrantConflict:
		return answered, outcome, nil
	}
	return RunRepo{}, outcome, nil
}
