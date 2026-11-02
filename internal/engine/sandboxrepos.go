package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// launchRepos is what a coding run's checkout resolves to on the company's own
// code host (ADR-0086): its grants, the code floor they were read at, the
// engine's steps giving the box each one — the client-floor check, then one
// clone each, then, in a cell whose boxes own their kernel, git's file-system
// monitor ([sandbox.CodingSteps]) — and the colleague whose grant bounds the
// box. Empty for a request naming none.
type launchRepos struct {
	repos []sandbox.RunRepo
	floor statelog.Position
	clone []sandbox.SetupStep
	// ceiling is the colleague seat whose ask woke the turn, whose own grant
	// bounds the box's on every repository (builtin.RunRepos.Ceiling); "" for
	// none.
	ceiling string
}

// resolveRunRepos resolves the repositories a coding run of turn t checks out
// into launchRepos, or refuses — as a [*builtin.RefusedCheckout] the seat
// reads, naming the repository and the reason — anything the seat may not be
// given:
//
//   - the company hosts no code (code.native unset), or this node does not
//     serve it now — named by why, a restart owed, an apply in flight;
//   - this node has no CREWLET_BOX_GIT_URL: the box would be given a
//     repository and no address to clone it from, so the request is refused
//     naming the node and the variable, and WARN sandbox_git_url_unset says
//     so where an operator reads it;
//   - a repository the code tools' resolution refuses
//     ([builtin.CodeDeps.ResolveRunRepos]: the seat's grant, the requester it
//     works for, a deletion, an archive under `write`, a base it does not hold).
//
// A turn a colleague seat woke names that seat on the run's row
// ([sandbox.RepoGrant.Ceiling]), and the code host's box admission holds the
// box to its grant as well as to the seat's.
//
// deps are the code tools' seams on this node — [CodeToolDeps] in a running
// engine, read by the caller at the checkout rather than held, so a checkout
// resolves through exactly what the seat's own code tools resolve through on
// the epoch it runs in. Their zero value serves none: a company that hosts no
// code, whose request naming repositories is refused.
//
// THE SEAMS ARE AN ARGUMENT, not state: they are what a test replaces, and
// the resolution is the same whatever carries the request to it.
//
// placement is the cell the box runs in, as the run's row names it: whether
// the box may run git's file-system monitor is the cell's to decide.
func (e *Engine) resolveRunRepos(ctx context.Context, deps builtin.CodeDeps, t *turnctx.Turn,
	reqs []builtin.RepoRequest, placement sandbox.Placement) (launchRepos, error) {

	if len(reqs) == 0 {
		return launchRepos{}, nil
	}
	if deps.Code == nil {
		if err := e.CodeUnserved(); !errors.Is(err, ErrCodeNotHosted) {
			return launchRepos{}, builtin.CheckoutNotServedNow(err, fmt.Sprintf("this node does not serve "+
				"the company's code host now (%v); nothing was checked out: check it out again once the code "+
				"host is served here", err))
		}
		return launchRepos{}, builtin.CheckoutUnavailable("this company hosts no repositories (code.native is " +
			"not set); clone a repository on another host with that host's own credentials")
	}
	if e.boxGitURL == "" {
		node := "this node"
		if e.node != nil {
			node = e.node.ID()
		}
		log.WarnContext(ctx, "sandbox_git_url_unset", "turn_id", t.RunID, "node", node,
			"detail", "a coding run checked out a repository on the code host and this node cannot give its "+
				"box git access: set "+sandbox.BoxGitURLVar+" to a git node's public listener and /box/git")
		return launchRepos{}, builtin.CheckoutUnavailable(fmt.Sprintf("this node cannot give a box git "+
			"access: %s is not set on %s (the base a box dials for /box/git/, a git node's public listener); "+
			"nothing was checked out", sandbox.BoxGitURLVar, node))
	}
	resolved, err := deps.ResolveRunRepos(ctx, t, reqs, func(asker code.Principal, repo code.RepoRef) code.Grant {
		return code.GrantOf(asker, repo, e.codeRoster())
	})
	if err != nil {
		return launchRepos{}, err
	}
	// EVERY READ OF THE RESOLUTION ANSWERED AT A CHECKPOINT OF THE CODE LOG,
	// so the run's floor is one: a floor elsewhere, or none, would have its
	// box read past nothing it was shown.
	if resolved.Floor.IsZero() || resolved.Floor.Stream != topics.CodeLogStream {
		return launchRepos{}, fmt.Errorf("engine: the code host answered this checkout's reads at %s, no "+
			"position of the code log", resolved.Floor)
	}
	// THE STEPS ARE THE CELL'S: whether the box may run git's file-system
	// monitor is decided by where it runs (sandbox.CodingSteps).
	clone, err := sandbox.CodingSteps(placement, resolved.Repos)
	if err != nil {
		return launchRepos{}, fmt.Errorf("engine: %w", err)
	}
	return launchRepos{repos: resolved.Repos, floor: resolved.Floor, ceiling: resolved.Ceiling, clone: clone}, nil
}

// runCheckouts is where `code_checkout` gives a running job a repository
// (ADR-0086): resolved for the turn's seat ([Engine.resolveRunRepos]) and
// granted on the job's own row, through the pending-run store of the sandbox
// runtime this node holds at the moment of the call — READ PER CALL, for
// [runAsks]' reason.
type runCheckouts struct {
	engine *Engine
	// deps are the code tools' seams a checkout resolves through; nil is
	// [CodeToolDeps] of the engine, read per call. A test's fake code host.
	deps func() builtin.CodeDeps
}

var _ builtin.RunCheckouts = runCheckouts{}

// Checkout resolves req for turn's seat and grants it to job launch, then
// answers the grant the row holds with the steps that clone it in the job's
// box. The row is read first, for the cell the box runs in and to refuse a
// job that is over before anything is resolved — the resolution takes the
// turn's holds — and the grant is written by a compare-and-set that asks the
// job again ([sandbox.PendingStore.GrantRepo]).
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
	resolved, err := e.resolveRunRepos(ctx, deps(e), turn, []builtin.RepoRequest{req},
		sandbox.Placement(run.Placement))
	if err != nil {
		return builtin.Checkout{}, err
	}
	if len(resolved.repos) != 1 {
		return builtin.Checkout{}, fmt.Errorf("engine: a checkout of one repository resolved to %d",
			len(resolved.repos))
	}
	floor := resolved.floor
	granted, outcome, err := rt.pending.GrantRepo(ctx, turn.RunID, launch, sandbox.RepoGrant{
		Repo: resolved.repos[0], Floor: floor.String(), Ceiling: resolved.ceiling,
		// A FLOOR THIS BUILD CANNOT READ IS RAISED OVER, as boxRuns' raise
		// does: the box reads at a floor it can be served at.
		Raise: func(current string) (string, bool) {
			if cur, perr := statelog.ParsePosition(current); perr == nil && holds(cur, floor) {
				return "", false
			}
			return floor.String(), true
		},
	})
	if err != nil {
		return builtin.Checkout{}, err
	}
	out := builtin.Checkout{Repo: granted, Outcome: outcome}
	switch outcome {
	case sandbox.GrantAdded:
		out.Steps = resolved.clone
	case sandbox.GrantHeld:
		// THE ROW'S GRANT, cloned as it was given: a box that lost its clone
		// starts from the base the first checkout resolved.
		if out.Steps, err = sandbox.CodingSteps(sandbox.Placement(run.Placement),
			[]sandbox.RunRepo{granted}); err != nil {
			return builtin.Checkout{}, fmt.Errorf("engine: %w", err)
		}
	}
	return out, nil
}

// runGrants are the checkouts a launch carries onto its job's row: the
// repositories, the floor they were read at and the colleague who bounds them.
type runGrants struct {
	Repos       []sandbox.RunRepo
	CodeFloor   string
	CodeCeiling string
}

// carriedGrants are the grants a launch of turn turnID opens its job with:
// for a round carried on (carries), the ones its row holds — the job it
// continues checked them out, and the work it continues is on their branches
// — and none for any other launch, whose box checks out what it needs. A row
// that holds no repository carries nothing, and a row that is gone carries
// nothing either: the launch then opens a new one.
func carriedGrants(ctx context.Context, pending sandbox.PendingStore, turnID string,
	carries bool) (runGrants, error) {

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
	return runGrants{Repos: run.Repos, CodeFloor: run.CodeFloor, CodeCeiling: run.CodeCeiling}, nil
}
