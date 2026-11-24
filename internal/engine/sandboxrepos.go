package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/origin"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// launchRepos is what an executor's checkout resolves to on Gitlet
// (ADR-0093): its grants, the code floor they were read at, the steps giving
// the box each one ([sandbox.CodingSteps]), the colleague whose grant bounds
// the box, and whom its turn works for. Empty for a request naming none.
type launchRepos struct {
	repos []sandbox.RunRepo
	// heads are where each grant's branch stood as the resolution read it,
	// in repos' order, "" where it does not exist yet
	// (builtin.RunRepos.Heads).
	heads []string
	floor statelog.Position
	clone []sandbox.SetupStep
	// ceiling is the colleague seat whose ask woke the turn, whose own grant
	// bounds the box's on every repository (builtin.RunRepos.Ceiling); "" for
	// none.
	ceiling string
	// workingFor is whom the turn works for (builtin.RunRepos.WorkingFor):
	// the box's pushes and the change its collection opens are the turn's
	// writes made late, and carry it too. Nil for the turn's own seat.
	workingFor *types.WorkingFor
}

// resolveRunRepos resolves the repositories an executor of turn t checks out,
// or refuses as a [*builtin.RefusedCheckout] naming the repository and why: no
// code hosted or none served here now, no CREWLET_BOX_GIT_URL on this node
// (also WARN sandbox_git_url_unset), or a [builtin.CodeDeps.ResolveRunRepos] refusal.
// A colleague who woke the turn bounds the box by its grant too
// ([sandbox.RepoGrant.Ceiling]); whom the turn works for goes on the row as the
// resolution composed it, never re-derived ([sandbox.RepoGrant.WorkingFor]), and
// a credential's holds the box to its own grant, which pushes nothing (ADR-0110).
// deps are the seat's own code-tool seams on this epoch ([CodeToolDeps] in a
// running engine), an argument rather than state so a test replaces them; their
// zero value serves no code.
func (e *Engine) resolveRunRepos(ctx context.Context, deps builtin.CodeDeps, t *turnctx.Turn,
	reqs []builtin.RepoRequest) (launchRepos, error) {

	if len(reqs) == 0 {
		return launchRepos{}, nil
	}
	if deps.Code == nil {
		if err := e.CodeUnserved(); !errors.Is(err, ErrCodeNotHosted) {
			return launchRepos{}, builtin.CheckoutNotServedNow(err, fmt.Sprintf("this node does not serve "+
				"Gitlet now (%v); nothing was checked out: check it out again once Gitlet "+
				"is served here", err))
		}
		return launchRepos{}, builtin.CheckoutUnavailable("this company hosts no repositories (code.backend is not " +
			"gitlet); clone a repository on another host with that host's own credentials")
	}
	if e.boxGitURL == "" {
		node := "this node"
		if e.node != nil {
			node = e.node.ID()
		}
		log.WarnContext(ctx, "sandbox_git_url_unset", "turn_id", t.RunID, "node", node,
			"detail", "an executor checked out a repository on Gitlet and this node cannot give its "+
				"box git access: set "+sandbox.BoxGitURLVar+" to a Gitlet node's public listener and /box/git")
		return launchRepos{}, builtin.CheckoutUnavailable(fmt.Sprintf("this node cannot give a box git "+
			"access: %s is not set on %s (the base a box dials for /box/git/, a Gitlet node's public listener); "+
			"nothing was checked out", sandbox.BoxGitURLVar, node))
	}
	resolved, err := deps.ResolveRunRepos(ctx, t, reqs, func(asker gitlet.Principal, repo gitlet.RepoRef) gitlet.Grant {
		return gitlet.GrantOf(asker, repo, e.codeRoster())
	})
	if err != nil {
		return launchRepos{}, err
	}
	// The resolution's reads answer at a code-log position, the run's floor:
	// any other floor, or none, would not hold the box's reads to what it was shown.
	if resolved.Floor.IsZero() || resolved.Floor.Stream != topics.CodeLogStream {
		return launchRepos{}, fmt.Errorf("engine: Gitlet answered this checkout's reads at %s, no "+
			"position of the code log", resolved.Floor)
	}
	clone, err := sandbox.CodingSteps(resolved.Repos)
	if err != nil {
		return launchRepos{}, fmt.Errorf("engine: %w", err)
	}
	return launchRepos{repos: resolved.Repos, heads: resolved.Heads, floor: resolved.Floor,
		ceiling: resolved.Ceiling, workingFor: resolved.WorkingFor, clone: clone}, nil
}

// runCheckouts is where `code_checkout` gives a running job a repository
// (ADR-0093): resolved for the turn's seat ([Engine.resolveRunRepos]) and
// granted on the job's row through this node's sandbox runtime's pending-run
// store, read per call for [runAsks]' reason.
type runCheckouts struct {
	engine *Engine
	// deps are the seams a checkout resolves through: nil is the engine's
	// [CodeToolDeps], read per call; a test sets a fake of Gitlet.
	deps func() builtin.CodeDeps
}

var _ builtin.RunCheckouts = runCheckouts{}

// Checkout resolves req for turn's seat, grants it to job launch, and answers
// the row's grant with the steps that clone it in the job's box. The row is
// read first, for the box's cell and to refuse a finished job before the
// resolution takes the turn's holds; the grant is a compare-and-set that asks
// the job again ([sandbox.PendingStore.GrantRepo]).
func (d runCheckouts) Checkout(ctx context.Context, turn *turnctx.Turn, launch string,
	req builtin.RepoRequest) (builtin.Checkout, error) {

	e := d.engine
	rt := e.sandbox.Load()
	if rt == nil || rt.pending == nil {
		return builtin.Checkout{}, errNoRunStore
	}
	run, found, err := rt.pending.Get(ctx, turn.RunID)
	switch {
	case err != nil:
		return builtin.Checkout{}, err
	case !found || run.LaunchID != launch ||
		(run.Status != sandbox.StatusLaunching && run.Status != sandbox.StatusRunning):
		return builtin.Checkout{Outcome: sandbox.GrantNotRunning}, nil
	}
	deps := CodeToolDeps
	if d.deps != nil {
		deps = func(*Engine) builtin.CodeDeps { return d.deps() }
	}
	resolved, err := e.resolveRunRepos(ctx, deps(e), turn, []builtin.RepoRequest{req})
	if err != nil {
		return builtin.Checkout{}, err
	}
	if len(resolved.repos) != 1 {
		return builtin.Checkout{}, fmt.Errorf("engine: a checkout of one repository resolved to %d",
			len(resolved.repos))
	}
	floor := resolved.floor
	// A floor this build cannot read is raised over, as boxRuns' raise does,
	// so the box reads at a floor it can be served at.
	raise := func(current string) (string, bool) {
		if cur, perr := statelog.ParsePosition(current); perr == nil && holds(cur, floor) {
			return "", false
		}
		return floor.String(), true
	}
	if err = branchWorkedBeside(ctx, rt.pending, run, resolved.repos[0]); err != nil {
		return builtin.Checkout{}, err
	}
	granted, outcome, err := rt.pending.GrantRepo(ctx, turn.RunID, launch, sandbox.RepoGrant{
		Repo: resolved.repos[0], Floor: floor.String(), Ceiling: resolved.ceiling, WorkingFor: resolved.workingFor,
		Raise: raise,
	})
	if err != nil {
		return builtin.Checkout{}, err
	}
	out := builtin.Checkout{Repo: granted, Outcome: outcome}
	switch outcome {
	case sandbox.GrantAdded:
		out.Steps = resolved.clone
	case sandbox.GrantHeld:
		// The row's grant, cloned where its branch stands now: a box asking
		// again lost its clone (a fresh box of a carried round) and continues
		// the job before it, whose work is what it pushed. So the clone starts
		// at the head just read, and at the first base only where the branch
		// does not exist yet; from the first base the box stands behind its
		// own job's pushes, refused as no fast-forward or, in the seat's
		// namespace, forced over them. A kept clone skips this
		// ([sandbox.CloneRecipe]).
		//
		// Its floor is raised to that read first, as an added grant's is, so
		// the node serving the box's fetch has applied the head. A floor says
		// only how fresh reads are, never what they may read: this widens nothing.
		if head := resolved.heads[0]; head != "" && head != granted.Base {
			if _, err = rt.pending.RaiseCodeFloor(ctx, turn.RunID, launch, raise); err != nil {
				return builtin.Checkout{}, err
			}
			granted.Base = head
			out.Repo = granted
		}
		if out.Steps, err = sandbox.CodingSteps([]sandbox.RunRepo{granted}); err != nil {
			return builtin.Checkout{}, fmt.Errorf("engine: %w", err)
		}
	}
	return out, nil
}

// errBranchWorked is a checkout refused because another of the seat's runs
// holding it pushes to the same branch of the repository (ADR-0169).
var errBranchWorked = errors.New("engine: another of this seat's running executor runs works that branch")

// branchWorkedBeside refuses run a branch of repo to push that another of its
// seat's runs holding the seat was given to push: two runs of one seat at
// once, each defaulting to seats/<handle>/<item>, would push over each other
// where no protection rule refuses a force. Read off the store, not this
// node's counts, since the bridge serves a checkout on any node; two
// checkouts of one branch inside one listing's round trip are not told apart.
func branchWorkedBeside(ctx context.Context, store seatRunLister, run sandbox.PendingRun,
	repo sandbox.RunRepo) error {
	if repo.Access != sandbox.RepoWrite || repo.Branch == "" {
		return nil
	}
	runs, err := store.ListActiveForSeat(ctx, run.AgentHandle)
	if err != nil {
		return fmt.Errorf("engine: reading which branches %s's other runs push: %w", run.AgentHandle, err)
	}
	for _, other := range runs {
		if other.TurnID == run.TurnID || !slices.Contains(sandbox.Holding, other.Status) {
			continue
		}
		if slices.ContainsFunc(other.Repos, func(r sandbox.RunRepo) bool {
			return r.Repo == repo.Repo && r.Branch == repo.Branch && r.Access == sandbox.RepoWrite
		}) {
			return fmt.Errorf("%w: %s's branch %s is pushed by turn %s's run; name another `branch`, "+
				"or work on that run's conversation once it is done", errBranchWorked, repo.Repo, repo.Branch,
				other.TurnID)
		}
	}
	return nil
}

// runGrants are the checkouts a launch carries onto its job's row: the
// repositories, the floor they were read at, the colleague who bounds them and
// whom the turn works for, which the box's pushes carry.
type runGrants struct {
	Repos          []sandbox.RunRepo
	CodeFloor      string
	CodeCeiling    string
	CodeWorkingFor *types.WorkingFor
}

// carriedGrants are the grants a launch of turn turnID opens its job with:
// for a round carried on (carries), the ones its row holds, since the work it
// continues is on their branches; none for any other launch, whose box checks
// out what it needs, nor for a row with no repository or gone (the launch
// then opens a new one).
func carriedGrants(ctx context.Context, pending sandbox.PendingStore, turnID string,
	carries bool) (_ runGrants, err error) {
	defer origin.Stamp(&err)

	if !carries {
		return runGrants{}, nil
	}
	run, found, err := pending.Get(ctx, turnID)
	switch {
	case err != nil:
		return runGrants{}, fmt.Errorf("engine: reading the checkouts run %s carries on: %w", turnID, err)
	case !found || len(run.Repos) == 0:
		return runGrants{}, nil
	}
	return runGrants{Repos: run.Repos, CodeFloor: run.CodeFloor, CodeCeiling: run.CodeCeiling,
		CodeWorkingFor: run.CodeWorkingFor}, nil
}
