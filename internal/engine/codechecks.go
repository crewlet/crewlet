package engine

import (
	"context"
	"runtime"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/checkrun"
	gitlethome "github.com/crewlet/crewlet/internal/gitlet/home"
	"github.com/crewlet/crewlet/internal/gitlet/host"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A GITLET NODE'S CHECKS (internal/gitlet/checkrun): its runner, which claims and
// runs the company's queued checks in boxes of this node's sandbox catalogue
// — on every Gitlet node, ranked per repository by its git order — and its share
// of the HOMES' sweep of the checks whose runner is gone, a worker of the
// home's loop on every Gitlet node, working the repositories it is home to.

// buildChecks builds this node's check runner and registers the home's sweep
// with its home loop, beside Gitlet, which they serve — over the router's code
// seam, the router's git orders, the current sandbox catalogue and epoch read
// at every walk and attempt, Gitlet's check tokens and this node's
// CREWLET_BOX_GIT_URL, its container cells gitlet.max_checks of this machine's
// cores, its remote cells held to the company's caps on the coordination
// store (code.checks.remote), nudged by a watch on this node's own code log.
// The runner is BUILT here and STARTED with the native half's duties beside
// the home's loop ([Engine.startNativeDuties]), for that loop's reasons; it is
// stopped FIRST in Gitlet's one bounded stop, before the home's loop
// and the host its boxes clone from ([native.stopGitletHost]).
//
// A NODE WITHOUT CREWLET_BOX_GIT_URL RUNS NO CHECK: a check's box clones its
// commit from Gitlet at that address and has no other way to reach
// it. Said once, at build; its sweep is registered all the same, since the
// home's sweep needs no box of its own. An error is the build's, which fails
// Gitlet's.
func (e *Engine) buildChecks(nodeID string, boot *config.Bootstrap, home *gitlethome.Loop,
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
		log.Warn("code_checks_not_run", "detail", "this Gitlet node runs none of the company's checks: a check's "+
			"box clones its commit from Gitlet at "+sandbox.BoxGitURLVar+", which this node does not set; "+
			"the fleet's other Gitlet nodes run them, and code_checks_backlog names a fleet where none does")
		return nil, nil
	}
	o := checkrun.Options{
		NodeID: nodeID, Estate: seam, Order: e.router, Boxes: e.checkBoxes, Company: e.checkCompany,
		Tokens: checkTokens{tokens: e.checkTokens}, GitBase: e.boxGitURL, CodeBase: checkCodeBase(e.boxGitURL),
		Slots: boot.Gitlet.MaxChecksFor(runtime.GOMAXPROCS(0)), Timing: checkrun.Production(),
		// THE COMPANY'S REMOTE CAPS are the coordination store's: a slot
		// leased for each remote box, its minutes on the fleet's counter.
		Leases: e.backends.Coord, Minutes: e.backends.Fleet,
		Nudges: checkNudges{nudges: codeNudges{log: codeLog.log, runner: codeLog.runner}}, Metrics: e.metrics,
	}
	// A FAILED RUN'S LOG goes to the fleet's object store, which
	// Gitlet's own build has already required (store.objects): one object a
	// failed run, named by its attempt's code_check_logs row.
	if objects := e.Objects(); objects != nil {
		o.Logs = objects
	}
	return checkrun.New(o)
}

// checkCodeBase is where a check's box reads its commit without a clone
// (checkrun.Options.CodeBase, its CREWLET_CHECK_API): gitBase — this node's
// CREWLET_BOX_GIT_URL, Gitlet's public listener and the box git route
// — with that route traded for the one the API serves a check's reads under.
// "" for a gitBase that does not end in the box git route, which
// checkrun.New refuses by its own name.
//
// BOTH ROUTES ARE auth's OWN SPELLINGS (BoxGitPrefix, BoxGitletPrefix), never a
// copy: the runner spelled its own "/box/gitlet" before, which nothing held to
// the route the API mounts, so renaming either would have answered every
// check's read 404 with only a live run to show it.
func checkCodeBase(gitBase string) string {
	listener, ok := strings.CutSuffix(gitBase, strings.TrimSuffix(auth.BoxGitPrefix, "/"))
	if !ok || listener == "" {
		return ""
	}
	return listener + strings.TrimSuffix(auth.BoxGitletPrefix, "/")
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

// checkTokens is Gitlet's CheckTokens as a runner mints a box's
// credential (checkrun.Tokens), the runner importing nothing of the host.
type checkTokens struct{ tokens *host.CheckTokens }

// Mint is the attempt's token for the repository named repo.
func (t checkTokens) Mint(attempt gitlet.CheckKey, repo string, timeout time.Duration) string {
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
	w, err := n.nudges.Watch(ctx, filter, gitlethome.Nudged{
		Subject: func(string, statelog.Position) { nudge() },
		Gap:     func(statelog.Position) { nudge() },
	})
	if err != nil {
		return nil, err
	}
	return w.Stop, nil
}
