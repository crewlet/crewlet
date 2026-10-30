package engine

import (
	"context"
	"runtime"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/checkrun"
	codehome "github.com/crewlet/crewlet/internal/code/home"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A GIT NODE'S CHECKS (internal/code/checkrun): its runner, which claims and
// runs the company's queued checks in boxes of this node's sandbox catalogue
// — on every git node, ranked per repository by its git order — and its share
// of the HOMES' sweep of the checks whose runner is gone, a worker of the
// home's loop on every git node, working the repositories it is home to.

// buildChecks builds this node's check runner and registers the home's sweep
// with its home loop, beside the code host they serve — over the router's code
// seam, the router's git orders, the current sandbox catalogue and epoch read
// at every walk and attempt, the code host's check tokens and this node's
// CREWLET_BOX_GIT_URL, its container cells git.max_checks of this machine's
// cores, nudged by a watch on this node's own code log. The runner is BUILT
// here and STARTED with the native half's duties beside the home's loop
// ([Engine.startNativeDuties]), for that loop's reasons; it is stopped FIRST
// in the code host's one bounded stop, before the home's loop and the host its
// boxes clone from ([native.stopCodeHost]).
//
// A NODE WITHOUT CREWLET_BOX_GIT_URL RUNS NO CHECK: a check's box clones its
// commit from the code host at that address and has no other way to reach
// it. Said once, at build; its sweep is registered all the same, since the
// home's sweep needs no box of its own. An error is the build's, which fails
// the code host's.
func (e *Engine) buildChecks(nodeID string, boot *config.Bootstrap, home *codehome.Loop,
	codeLog *runningLog) (*checkrun.Runner, error) {

	seam := e.router.Code()
	sweeper, err := checkrun.NewSweeper(checkrun.SweeperOptions{Estate: seam, Order: e.router,
		Boxes: e.checkBoxes, Poll: checkrun.CheckPoll})
	if err != nil {
		return nil, err
	}
	if err := home.Register(sweeper); err != nil {
		return nil, err
	}
	if e.boxGitURL == "" {
		log.Warn("code_checks_not_run", "detail", "this git node runs none of the company's checks: a check's "+
			"box clones its commit from the code host at "+sandbox.BoxGitURLVar+", which this node does not set; "+
			"the fleet's other git nodes run them, and code_checks_backlog names a fleet where none does")
		return nil, nil
	}
	return checkrun.New(checkrun.Options{
		NodeID: nodeID, Estate: seam, Order: e.router, Boxes: e.checkBoxes, Company: e.checkCompany,
		Tokens: checkTokens{tokens: e.checkTokens}, GitBase: e.boxGitURL,
		Slots: boot.Git.MaxChecksFor(runtime.GOMAXPROCS(0)), Timing: checkrun.Production(),
		Nudges: checkNudges{nudges: codeNudges{log: codeLog.log, runner: codeLog.runner}}, Metrics: e.metrics,
	})
}

// checkBoxes is this node's CURRENT sandbox catalogue, read at every walk and
// attempt — an apply swaps the manager — and false where the company
// configures none.
func (e *Engine) checkBoxes() (checkrun.Boxes, bool) {
	m := e.sandboxManager()
	if m == nil {
		return nil, false
	}
	return m, true
}

// checkCompany is the CURRENT epoch a check runs under — its configuration and
// this node's resolution of a ${VAR} reference — false before a company.
func (e *Engine) checkCompany() (checkrun.Company, bool) {
	c := e.Company()
	if c == nil || c.Config == nil {
		return checkrun.Company{}, false
	}
	return checkrun.Company{Config: c.Config, Resolve: e.resolver().Expand}, true
}

// checkTokens is the code host's CheckTokens as a runner mints a box's
// credential (checkrun.Tokens), the runner importing nothing of the host.
type checkTokens struct{ tokens *host.CheckTokens }

// Mint is the attempt's token for the repository named repo.
func (t checkTokens) Mint(attempt code.CheckKey, repo string, timeout time.Duration) string {
	if t.tokens == nil {
		return ""
	}
	return t.tokens.Mint(host.CheckRunRef{Attempt: attempt, Repo: repo}, timeout)
}

// checkNudges is the home's watch of this node's code log as a runner hears of
// check records (checkrun.Nudges): every record under its filter and every
// gap, one nudge each.
type checkNudges struct{ nudges codeNudges }

// Watch begins the watch.
func (n checkNudges) Watch(ctx context.Context, filter string, nudge func()) (func(), error) {
	w, err := n.nudges.Watch(ctx, filter, codehome.Nudged{
		Subject: func(string, statelog.Position) { nudge() },
		Gap:     func(statelog.Position) { nudge() },
	})
	if err != nil {
		return nil, err
	}
	return w.Stop, nil
}
