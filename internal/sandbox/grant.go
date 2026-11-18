package sandbox

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
)

// A RUN IS GRANTED ITS REPOSITORIES ONE CHECKOUT AT A TIME (ADR-0093): its
// launch gives it none, and each `code_checkout` its box calls through the
// bridge adds one, resolved for the seat, to the job's row
// ([PendingStore.GrantRepo]). Gitlet's box admission reads the row on every
// request, so a repository is reachable from the request after its checkout,
// and one never checked out never is.

// RepoGrant is one repository a running job is given: its grant resolved for
// the seat by the engine, the position that resolution read at (the run's
// first [PendingRun.CodeFloor], never lowering one it has), the colleague
// whose grant bounds the box ([PendingRun.CodeCeiling]) and whom the turn
// works for ([PendingRun.CodeWorkingFor]).
type RepoGrant struct {
	Repo RunRepo
	// Floor is the resolution's position, as statelog.Position's token.
	// Required: a grant read at no position would let the box read below
	// what it was shown.
	Floor string
	// Raise answers the higher of the row's current floor and Floor, or false
	// where Floor raises nothing: the store compares no positions
	// ([PendingRun.CodeFloor]).
	Raise func(current string) (next string, ok bool)
	// Ceiling is the colleague seat whose ask woke the turn, or "" for none.
	Ceiling string
	// WorkingFor is whom the turn's code writes work for, as the resolution
	// composed it; nil for a turn that works for its own seat.
	WorkingFor *types.WorkingFor
}

// GrantOutcome is what [PendingStore.GrantRepo] did.
type GrantOutcome string

const (
	// GrantAdded is a repository added to the job's grants.
	GrantAdded GrantOutcome = "added"
	// GrantHeld is a repository the job was given already, on the same
	// branch with the same access: the row's grant is answered, so a box that
	// lost its clone checks it out again. Only what follows it is written: the
	// repository's new name after a rename, and whom the job works for
	// narrowed to the apps the checkout's credential still holds
	// ([sameRequester]).
	GrantHeld GrantOutcome = "held"
	// GrantConflict is a repository the job was given on another branch or
	// with another access, or a grant under another ceiling or for another
	// requester than the job's grants, nobody counting as one
	// ([sameRequester]): one run works on one branch of a repository, for one
	// requester.
	GrantConflict GrantOutcome = "conflict"
	// GrantFull is a job given [MaxRunRepos] repositories already.
	GrantFull GrantOutcome = "full"
	// GrantNotRunning is a row that does not hold this job launching or
	// running (it ended, was stopped or was replaced): its box is given
	// nothing.
	GrantNotRunning GrantOutcome = "not_running"
)

// Validate refuses a grant no row could carry: a repository that is not an
// executor's ([RunRepo.ValidateFor] [CloneCoding]), one read at no position,
// or one with no Raise.
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
	// outcome is why not, as the last run of the decision saw it — never
	// "added": only the store's answer says a write landed (see RecordAsk).
	outcome, answered := GrantNotRunning, RunRepo{}
	_, won, err := s.mutateLive(ctx, turnID, func(run *PendingRun) bool {
		answered = RunRepo{}
		if run.LaunchID != launchID || (run.Status != StatusLaunching && run.Status != StatusRunning) {
			outcome = GrantNotRunning
			return false
		}
		// WHOM THE JOB WORKS FOR: the first grant's, then the row's, narrowed
		// by a later grant for the same credential ([sameRequester]).
		working := g.WorkingFor
		if len(run.Repos) > 0 {
			var same bool
			working, same = sameRequester(run.CodeWorkingFor, g.WorkingFor)
			if run.CodeCeiling != g.Ceiling || !same {
				outcome = GrantConflict
				return false
			}
		}
		narrowed := len(run.Repos) > 0 && !reflect.DeepEqual(working, run.CodeWorkingFor)
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
				run.CodeWorkingFor = working
				return narrowed
			}
			// RENAMED SINCE IT WAS CHECKED OUT: the grant follows the id, and
			// the box clones and is admitted by the current name.
			run.Repos[i].Repo = g.Repo.Repo
			run.CodeWorkingFor = working
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
		// EVERY PUSH OF THE BOX APPLIES ABOVE ITS FIRST CHECKOUT, before which
		// it can push nothing: the launch's floor ([PendingRun.CodeLaunch]) is
		// set once, by the first grant, and never raised.
		if run.CodeLaunch == "" {
			run.CodeLaunch = g.Floor
		}
		run.Repos = append(run.Repos, g.Repo)
		run.CodeCeiling, run.CodeWorkingFor = g.Ceiling, working
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

// sameRequester is whom a job granted for held works for once a grant for g
// reaches its row, and false where they are different requesters
// ([GrantConflict]). Nobody and nobody, one seat, and any other carrier naming
// exactly the same apps are one requester, kept as the row holds it. ONE
// CREDENTIAL is one requester whatever its apps: the row's are narrowed to g's,
// in the row's order, never widened, so a credential narrowed or widened mid-run pushes for no app
// either grant lacks.
//
// NOBODY IS A REQUESTER TOO, NEVER ADOPTED: a row with no carrier was granted
// for nobody, so a grant for somebody conflicts, failing closed (the job keeps
// what it has; the turn's work for them starts a new run). Adopting the first
// carrier a checkout names would serve only rows of a build no tag shipped.
func sameRequester(held, g *types.WorkingFor) (*types.WorkingFor, bool) {
	switch {
	case held == nil || g == nil:
		return held, held == nil && g == nil
	case held.Kind != g.Kind || held.Seat != g.Seat || held.Login != g.Login:
		return held, false
	case held.Kind != types.WorkingForCredential:
		return held, slices.Equal(held.Apps, g.Apps)
	}
	narrowed := held.Clone()
	narrowed.Apps = slices.DeleteFunc(narrowed.Apps, func(app iam.Grant) bool { return !slices.Contains(g.Apps, app) })
	return narrowed, true
}
