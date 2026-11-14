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
// launch gives it none, and every `code_checkout` its box calls through the
// bridge resolves one repository for the seat and adds it to the job's row
// ([PendingStore.GrantRepo]). Gitlet's box admission reads the row on
// every request, so a repository checked out is reachable from the next
// request on, and one never checked out never is.

// RepoGrant is one repository a running job is given: its grant, resolved
// for the seat by the engine, the position that resolution read at — the
// run's first code floor where it has none yet, and never below the one it
// has — the colleague whose grant bounds the box ([PendingRun.CodeCeiling])
// and whom the turn works for ([PendingRun.CodeWorkingFor]).
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
	// WorkingFor is whom the turn works for, as the resolution composed it
	// for the turn's own code writes — nil for a turn that works for its own
	// seat.
	WorkingFor *types.WorkingFor
}

// GrantOutcome is what [PendingStore.GrantRepo] did.
type GrantOutcome string

const (
	// GrantAdded is a repository added to the job's grants.
	GrantAdded GrantOutcome = "added"
	// GrantHeld is a repository the job was given already, on the same
	// branch with the same access: the row's grant is answered, so a box that
	// lost its clone checks it out again, and nothing is written but what
	// follows it — the repository's name where a rename moved it since, and
	// whom the job works for narrowed to the apps the checkout's credential
	// holds where it holds fewer ([sameRequester]).
	GrantHeld GrantOutcome = "held"
	// GrantConflict is a repository the job was given on another branch or
	// with another access, or a ceiling other than the one its grants are
	// bounded by, or a grant made for another requester than the one its
	// grants were made for, nobody counted as one ([sameRequester]): one run
	// works on one branch of a repository, for one requester.
	GrantConflict GrantOutcome = "conflict"
	// GrantFull is a job given [MaxRunRepos] repositories already.
	GrantFull GrantOutcome = "full"
	// GrantNotRunning is a row that does not hold this job launching or
	// running: the job ended, was stopped or was replaced, and its box is
	// given nothing.
	GrantNotRunning GrantOutcome = "not_running"
)

// Validate refuses a grant no row could carry: a repository that is not a
// executor's ([RunRepo.ValidateFor] [CloneCoding]), or one read at no
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
		// WHOM THE JOB WORKS FOR: the first grant's, and from then on the
		// row's, narrowed to a later grant's apps where both are made for one
		// credential ([sameRequester]).
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
			// RENAMED SINCE IT WAS CHECKED OUT: the grant follows the
			// repository's id, and the box clones and is admitted by the
			// name it has now.
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
		// EVERY PUSH OF THE BOX APPLIES ABOVE ITS FIRST CHECKOUT, which no
		// box can push before: the launch's floor ([PendingRun.CodeLaunch])
		// is set once, by the first grant, and never raised.
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

// sameRequester is whom a job whose grants were made for held works for once a
// grant made for g reaches its row, and false where the two name different
// requesters — one run works for one ([GrantConflict]). Nobody and nobody, one
// seat and the same seat, and every carrier and one naming exactly what it
// does are the same requester, held as the row holds it; ONE CREDENTIAL is the
// same requester whatever apps each names, and the row's is narrowed to the
// apps g's names too, in the row's order — never widened, so a credential an
// administrator narrowed while the job ran pushes for no app it no longer
// holds, and one a node widened is held to what the job was first given.
//
// NOBODY IS A REQUESTER TOO, AND NEVER ADOPTED: a row holding no carrier is a
// job granted for nobody, whichever build wrote it, so a grant made for
// somebody is another requester's. A build before the carrier wrote none on
// any row, its colleague's ceiling beside its grants alone, and a job it
// launched that a node of this build carries on, in a turn that works for
// somebody, is refused every checkout so — failing closed: the job keeps what
// it was given and pushes it for nobody, and the turn's work for them starts a
// new run. No field tells such a row from one this build granted for nobody,
// and none is kept: an arm adopting the first carrier a checkout names, or a
// mark of the build that wrote a row, would be code for a build no tag
// shipped, whose rows are not tolerated.
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
