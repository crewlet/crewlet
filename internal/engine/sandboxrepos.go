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

// launchRepos is a run_sandbox launch's repositories on the company's own code
// host (ADR-0063): their grants on the run, its first code floor, and the
// engine's steps giving the box each one — the client-floor check, then one
// clone each, in the order the call named them, then, in a cell whose boxes
// own their kernel, git's file-system monitor ([sandbox.CodingSteps]). Empty
// for a call naming none.
type launchRepos struct {
	repos []sandbox.RunRepo
	floor statelog.Position
	clone []sandbox.SetupStep
	// ceiling is the colleague seat whose ask woke the turn, whose own grant
	// bounds the box's on every repository (builtin.RunRepos.Ceiling); "" for
	// none.
	ceiling string
}

// resolveRunRepos resolves the repositories a run_sandbox call names into
// launchRepos, or refuses — as the tool's answer, naming the repository and
// the reason — anything the seat may not be given:
//
//   - the company hosts no code (code.native unset), or this node does not
//     serve it now — named by why, a restart owed, an apply in flight;
//   - this node has no CREWLET_BOX_GIT_URL: the box would be given a
//     repository and no address to clone it from, so the launch is refused
//     naming the node and the variable, and WARN sandbox_git_url_unset says
//     so where an operator reads it;
//   - a repository the code tools' resolution refuses
//     ([builtin.CodeDeps.ResolveRunRepos]: the seat's grant, the requester it
//     works for, a deletion, an archive under `write`, a base it does not hold).
//
// A turn a colleague seat woke names that seat on the run's row
// (sandbox.LaunchRequest.CodeCeiling), and the code host's box admission
// holds the box to its grant as well as to the seat's.
//
// CALLED AFTER THE LAUNCH'S OWN REFUSALS and before its run row is opened —
// the resolution reads at the seat's level and takes the turn's holds, which
// a launch a local check refused would have taken for nothing — and before
// the turn's holds are sealed for the row, since a hold the resolution takes
// must be in the set the row is opened with. And it takes them FOR THE LAUNCH
// ([turnctx.Holds.ForLaunch]): into the set the launch writes on its row,
// never through the writer a resumed executor's row has attached — so a
// relaunch's holds ride the BeginLaunch it makes anyway, rather than one
// compare-and-set on the same row per key before it (M3-boxes §10.6.5).
// placement is the cell the box runs in, as the launch's spec resolved it.
func (l *launcher) resolveRunRepos(ctx context.Context, t *turnctx.Turn, reqs []builtin.RepoRequest,
	placement sandbox.Placement) (launchRepos, error) {

	if len(reqs) == 0 {
		return launchRepos{}, nil
	}
	e := l.engine
	if l.code.Code == nil {
		if err := e.CodeUnserved(); !errors.Is(err, ErrCodeNotHosted) {
			return launchRepos{}, fmt.Errorf("this node does not serve the company's code host now (%v); "+
				"nothing was launched — call run_sandbox again later, or name the repository in the brief", err)
		}
		return launchRepos{}, errors.New("this company hosts no repositories (code.native is not set); " +
			"name the repository in the brief")
	}
	if e.boxGitURL == "" {
		node := "this node"
		if e.node != nil {
			node = e.node.ID()
		}
		log.WarnContext(ctx, "sandbox_git_url_unset", "turn_id", t.RunID, "node", node,
			"detail", "a coding run named repositories on the code host and this node cannot give its box "+
				"git access: set "+sandbox.BoxGitURLVar+" to a git node's public listener and /box/git")
		return launchRepos{}, fmt.Errorf("this node cannot give a box git access: %s is not set on %s (the "+
			"base a box dials for /box/git/, a git node's public listener); nothing was launched",
			sandbox.BoxGitURLVar, node)
	}
	resolved, err := l.code.ResolveRunRepos(t.Holding().ForLaunch(ctx), t, reqs, func(asker code.Principal, repo code.RepoRef) code.Grant {
		return code.GrantOf(asker, repo, e.codeRoster())
	})
	if err != nil {
		return launchRepos{}, err
	}
	// EVERY READ OF THE RESOLUTION ANSWERED AT A CHECKPOINT OF THE CODE LOG,
	// so the run's floor is one: a floor elsewhere, or none, would have its
	// box read past nothing it was shown.
	if resolved.Floor.IsZero() || resolved.Floor.Stream != topics.CodeLogStream {
		return launchRepos{}, fmt.Errorf("engine: the code host answered this launch's reads at %s, no position "+
			"of the code log", resolved.Floor)
	}
	// THE STEPS ARE THE CELL'S: whether the box may run git's file-system
	// monitor is decided by where it runs (sandbox.CodingSteps).
	clone, err := sandbox.CodingSteps(placement, resolved.Repos)
	if err != nil {
		return launchRepos{}, fmt.Errorf("engine: %w", err)
	}
	return launchRepos{repos: resolved.Repos, floor: resolved.Floor, ceiling: resolved.Ceiling, clone: clone}, nil
}
