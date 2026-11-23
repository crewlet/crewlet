package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"maps"
	"net"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/browser/boxhelper"
	"github.com/crewlet/crewlet/internal/browser/hostreach"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/toolname"
)

// readyEnsure is ensure's ready line, every field it requires of its grammar,
// on the Chromium build the pinned Playwright expects.
const readyEnsure = "crewlet-browser-ensure/1 ready helper=0123456789abcdef pin=0.0.86 node=22.23.5 " +
	"chromium=" + browser.ExpectedChromiumVersion + " build=expected sandbox=required supervisor=started"

// browserReq is [launchReq] of a browser seat whose run lands in cell.
func browserReq(turnID string, cell Placement) LaunchRequest {
	req := launchReq(turnID)
	req.Spec.Placement = cell
	req.Browser = &BrowserLaunch{Sandbox: browser.SandboxRequired}
	return req
}

// ensureAnswers has box answer ensure with stdout, and every other command
// with an empty success.
func ensureAnswers(box *FakeSandbox, stdout string) {
	box.ExecFunc = func(_ context.Context, cmd string, _ ExecOptions) (ExecResult, error) {
		if cmd == boxhelper.EnsureCommand(box.Home()) {
			return ExecResult{Stdout: stdout}, nil
		}
		return ExecResult{}, nil
	}
}

// ensureRuns is how many times box was asked to run ensure.
func ensureRuns(box *FakeSandbox) int {
	n := 0
	for _, e := range box.Execs() {
		if e.Command == boxhelper.EnsureCommand(box.Home()) {
			n++
		}
	}
	return n
}

// stopRuns is how many times box was asked to stop the browser it runs.
func stopRuns(box *FakeSandbox) int {
	n := 0
	for _, e := range box.Execs() {
		if e.Command == boxhelper.StopCommand(box.Home()) {
			n++
		}
	}
	return n
}

// checkRuns is how many times box was asked to run the launch's check of its
// network.
func checkRuns(box *FakeSandbox) int {
	n := 0
	for _, e := range box.Execs() {
		if e.Command == hostreach.Command() {
			n++
		}
	}
	return n
}

// reaching answers the launch's check of a box's network as a box one of
// whose routes leads to the engine's loopback: it dials the engine's port with
// that route's token, then prints the double's clean line, as a box's half
// does that saw its own dials answered.
func reaching(t *testing.T, route browser.EnsureCause) func(context.Context, ExecOptions) (ExecResult, error) {
	return func(ctx context.Context, opts ExecOptions) (ExecResult, error) {
		var token string
		for pair := range strings.SplitSeq(opts.Env[hostreach.TokensEnv], ",") {
			if r, tok, _ := strings.Cut(pair, ":"); r == string(route) {
				token = tok
			}
		}
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp",
			net.JoinHostPort("127.0.0.1", opts.Env[hostreach.PortEnv]))
		if err != nil {
			t.Errorf("connecting to the check's port: %v", err)
			return ExecResult{}, nil
		}
		defer func() { _ = conn.Close() }()
		if _, err := io.WriteString(conn, token+"\n"); err != nil {
			t.Errorf("writing the %s token: %v", route, err)
		}
		_, _ = io.ReadAll(conn)
		return ExecResult{Stdout: hostreach.Answer(FakeKernel)}, nil
	}
}

// printing answers the launch's check of a box's network with stdout alone.
func printing(stdout string) func(context.Context, ExecOptions) (ExecResult, error) {
	return func(context.Context, ExecOptions) (ExecResult, error) { return ExecResult{Stdout: stdout}, nil }
}

// otherKernel is a boot id that is not the double's.
const otherKernel = "3c2b1a09-8f7e-4d6c-b5a4-938271605f4e"

// helperFiles is every file box holds under its browser folder.
func helperFiles(box *FakeSandbox) []string {
	return slices.DeleteFunc(box.Files(), func(p string) bool {
		return !strings.HasPrefix(p, boxhelper.Folder(box.Home())+"/")
	})
}

// workspaceRoot is where a container box's browser server is rooted: its
// workspace, never its home.
const workspaceRoot = DefaultHome + "/" + WorkspaceSubdir

// browserCoordinator is a coordinator over rig, refreshing a kept box's files
// through refresh where it is set ([CoordinatorOptions.Refresh]).
func browserCoordinator(t *testing.T, rig *waiterRig,
	refresh func(context.Context, *Manager, PendingRun, Sandbox, RecordHanded)) *Coordinator {
	t.Helper()
	c, err := NewCoordinator(CoordinatorOptions{
		Pushes: &pushSpy{}, Answers: rigAnswers(), Leads: rigLeads(),
		Queue: rig.queue, Pending: rig.pending, Manager: rig.manager,
		Resume: &resumeSpy{}, Audience: &audienceSpy{},
		Lease: leased(rigLease), Now: func() time.Time { return rig.now },
		Refresh: refresh,
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	t.Cleanup(c.Stop)
	return c
}

// keptLaunch launches first, ends its job as a resumed turn's claim does,
// and launches again, which continues in first's box: a relaunch of the
// resumed turn, the one shape a box is continued in ([PriorBox.ContinuesIn]).
func keptLaunch(t *testing.T, rig *waiterRig, c *Coordinator, first, again LaunchRequest) (LaunchResult, LaunchResult) {
	t.Helper()
	ctx := t.Context()
	made, err := c.Launch(ctx, rig.manager, first)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	rig.suspend(first.Turn.TurnID)
	if _, won, err := rig.pending.ClaimForResume(ctx, first.Turn.TurnID, CompletionTail(made.LaunchID),
		rigLease); err != nil || !won {
		t.Fatalf("ClaimForResume = %v, %v", won, err)
	}
	kept, err := c.Launch(ctx, rig.manager, again)
	if err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	if !kept.Reused || kept.SandboxID != made.SandboxID {
		t.Fatalf("the relaunch ran in %s (reused %t), want it continued in %s", kept.SandboxID, kept.Reused,
			made.SandboxID)
	}
	return made, kept
}

// A LAUNCH OUTSIDE A CONTAINER BOX NEVER RUNS ANYTHING IN IT for the browser:
// in every cell but container the browser is withheld before the box's
// network is checked, before anything is written and before ensure: no check,
// no helper file, no ensure, no browser server, and the job's record says
// cell_not_container, with no cause.
//
// Mutations, each red: check the box's network before the cell; check the
// cell after the helper's write; write the rule as a comparison with direct.
func TestALaunchOutsideAContainerBoxNeverRunsAnythingInIt(t *testing.T) {
	t.Parallel()
	for _, cell := range Placements {
		if cell == Container {
			continue
		}
		t.Run(string(cell), func(t *testing.T) {
			t.Parallel()
			rig := newWaiterRigIn(t, cell)
			rig.provider.Made = func(box *FakeSandbox) { ensureAnswers(box, readyEnsure) }
			res, err := rig.launchVia(t.Context(), rig.manager, browserReq("t1", cell))
			if err != nil {
				t.Fatalf("Launch: %v", err)
			}
			box := rig.provider.Box(res.SandboxID)
			if files := helperFiles(box); len(files) > 0 || checkRuns(box) > 0 || ensureRuns(box) > 0 ||
				stopRuns(box) > 0 {
				t.Errorf("the box was written %q, ran the check %d times, ensure %d times and stop %d", files,
					checkRuns(box), ensureRuns(box), stopRuns(box))
			}
			if _, served := rig.runner.Started()[0].MCPServers[browser.ServerName]; served {
				t.Error("the run was handed a browser server")
			}
			got := rig.get("t1").Launch.Browser
			if got == nil || got.State != browser.EnsureWithheld || got.Reason != browser.ReasonCellNotContainer ||
				got.Cause != "" {
				t.Errorf("the job's record holds %+v, want withheld cell_not_container with no cause", got)
			}
		})
	}
}

// kernelless is a box whose handle cannot say which kernel its runtime runs
// on: it hides the double's EngineKernel behind the interface.
type kernelless struct{ Sandbox }

// kernellessBoxes is a provider of [kernelless] boxes.
type kernellessBoxes struct{ *FakeProvider }

func (p kernellessBoxes) Create(ctx context.Context, spec Spec) (Sandbox, error) {
	box, err := p.FakeProvider.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	return kernelless{box}, nil
}

// A BOX THAT MAY REACH ITS HOST'S LOOPBACK NEVER RUNS ENSURE: a box whose
// check carried a route's token to the engine's loopback, printed another
// kernel, printed nothing or said held, and a box whose handle cannot say
// its runtime's kernel, has its browser withheld as host_network with the
// route, another_kernel or unchecked, before the helper is written: no helper
// file, no ensure, no browser server, and the box's stop run once, after the
// check.
//
// Mutations, each red: write the helper before the check; ignore a token that
// arrived; treat a handle with no kernel as clean; skip the stop.
func TestABoxThatMayReachItsHostsLoopbackNeverRunsEnsure(t *testing.T) {
	t.Parallel()
	type tc struct {
		reach      func(context.Context, ExecOptions) (ExecResult, error)
		kernelless bool
		cause      browser.EnsureCause
	}
	cases := map[string]tc{
		"another kernel":  {reach: printing(hostreach.Answer(otherKernel)), cause: browser.CauseAnotherKernel},
		"nothing printed": {reach: printing(""), cause: browser.CauseUnchecked},
		"a held dial": {reach: printing("crewlet-browser-reach/1 kernel=" + string(FakeKernel) + " held\n"),
			cause: browser.CauseUnchecked},
		"no kernel to say": {kernelless: true, cause: browser.CauseUnchecked},
	}
	for _, route := range browser.ReasonHostNetwork.Causes() {
		if route != browser.CauseAnotherKernel && route != browser.CauseUnchecked {
			cases["by "+string(route)] = tc{reach: reaching(t, route), cause: route}
		}
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newWaiterRigIn(t, Container)
			rig.provider.Made = func(box *FakeSandbox) {
				ensureAnswers(box, readyEnsure)
				box.ReachFunc = c.reach
			}
			m := rig.manager
			if c.kernelless {
				var err error
				if m, err = NewManager(ManagerOptions{
					Providers: map[Placement]Provider{Container: kernellessBoxes{FakeProvider: rig.provider}},
					Runners:   map[string]Runner{"claude-code": rig.runner},
				}); err != nil {
					t.Fatalf("NewManager: %v", err)
				}
			}
			res, err := rig.launchVia(t.Context(), m, browserReq("t1", Container))
			if err != nil {
				t.Fatalf("Launch: %v", err)
			}
			box := rig.provider.Box(res.SandboxID)
			if files := helperFiles(box); len(files) > 0 || ensureRuns(box) > 0 || stopRuns(box) != 1 {
				t.Errorf("the box was written %q, ran ensure %d times and stop %d, want stop once", files,
					ensureRuns(box), stopRuns(box))
			}
			if execs := box.Execs(); !c.kernelless && (len(execs) == 0 ||
				execs[len(execs)-1].Command != boxhelper.StopCommand(box.Home())) {
				t.Errorf("the box ran %d commands, the stop not last after the check", len(execs))
			}
			if _, served := rig.runner.Started()[0].MCPServers[browser.ServerName]; served {
				t.Error("the run was handed a browser server")
			}
			got := rig.get("t1").Launch.Browser
			if got == nil || got.State != browser.EnsureWithheld || got.Reason != browser.ReasonHostNetwork ||
				got.Cause != c.cause {
				t.Errorf("the job's record holds %+v, want withheld host_network %s", got, c.cause)
			}
		})
	}
}

// THE NETWORK CHECK RUNS WITH THE RUN'S ENVIRONMENT BEFORE ANYTHING IS
// WRITTEN: in the box's home, with the run's environment and every one of the
// check's variables over it, capped in the box at exactly the check's bound,
// and while the box holds no helper file yet.
//
// Mutations, each red: drop the run's environment; leave TimeoutSec at zero;
// write the helper first.
func TestTheNetworkCheckRunsWithTheRunsEnvironmentBeforeAnythingIsWritten(t *testing.T) {
	t.Parallel()
	rig := newWaiterRigIn(t, Container)
	var mu sync.Mutex
	var got []ExecOptions
	var written [][]string
	rig.provider.Made = func(box *FakeSandbox) {
		ensureAnswers(box, readyEnsure)
		box.ReachFunc = func(_ context.Context, opts ExecOptions) (ExecResult, error) {
			mu.Lock()
			got, written = append(got, opts), append(written, helperFiles(box))
			mu.Unlock()
			return ExecResult{Stdout: hostreach.Answer(FakeKernel)}, nil
		}
	}
	req := browserReq("t1", Container)
	req.Spec.Env = map[string]string{"HTTPS_PROXY": "http://proxy.example.com:3128", "NPM_TOKEN": "npm-..."}
	if _, err := rig.launchVia(t.Context(), rig.manager, req); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("the check ran %d times, want once", len(got))
	}
	opts := got[0]
	for k, v := range req.Spec.Env {
		if opts.Env[k] != v {
			t.Errorf("the check ran with %s=%q, want the run's %q", k, opts.Env[k], v)
		}
	}
	for _, k := range []string{hostreach.PortEnv, hostreach.TokensEnv, hostreach.RouteEnv, hostreach.AnswerEnv,
		hostreach.DeadlineEnv, hostreach.BeyondEnv, hostreach.HostEnv} {
		if opts.Env[k] == "" {
			t.Errorf("the check ran with no %s", k)
		}
	}
	switch {
	case opts.Cwd != DefaultHome:
		t.Errorf("the check ran in %q, want the box's home", opts.Cwd)
	case opts.TimeoutSec != hostreach.CheckTimeout.Seconds():
		t.Errorf("the check's cap is %v seconds, want exactly the check's %v", opts.TimeoutSec,
			hostreach.CheckTimeout.Seconds())
	case len(written[0]) > 0:
		t.Errorf("the box held %q when its network was checked, want nothing of the helper yet", written[0])
	}
}

// A BROWSER THE LAUNCH WITHHOLDS NAMES WHAT TO CHANGE on the operator's log:
// a run outside a container names the entry it launched on, its cell and
// that entry's run_in; one denied the web, the entry and its web_tools; one
// whose harness would hide the bridge, the entry, the harness, its count and
// the three counts that reach it, and another harness or no browser; a box
// whose check found a route, the setting that leads it to the host's
// loopback; one on another kernel, the runtime to run instead. A launch's own
// decisions, which no line the box printed could explain.
//
// Mutations, each red: leave the entry out of the cell's detail; leave the
// web denial's detail empty; name no setting for a route; leave the counts
// out of the bridge's detail.
func TestAWithheldBrowsersDetailNamesWhatToChange(t *testing.T) {
	t.Parallel()
	type tc struct {
		cell      Placement
		reach     func(context.Context, ExecOptions) (ExecResult, error)
		webDenied bool
		deferAt   int
		want      []string
	}
	cases := map[string]tc{
		"a run denied the web": {cell: Container, webDenied: true,
			want: []string{"providers.llm.coder", "overrides.web_tools: denied"}},
		"a harness that would hide the bridge": {cell: Container, deferAt: 80,
			want: []string{"providers.llm.coder", "muse-code", "list 80", "bridge's 70", "box's own servers' 3",
				"browser's " + strconv.Itoa(len(boxhelper.Tools())), "another harness", "remove sandbox.browser"}},
		"by loopback": {cell: Container, reach: reaching(t, browser.CauseLoopback),
			want: []string{"containers.conf", "next fresh box"}},
		"by gateway": {cell: Container, reach: reaching(t, browser.CauseGateway),
			want: []string{"allow_host_loopback", "--map-gw"}},
		"by host alias": {cell: Container, reach: reaching(t, browser.CauseHostAlias),
			want: []string{"hosts file", "--add-host"}},
		"by rootless gateway": {cell: Container, reach: reaching(t, browser.CauseRootlessGateway),
			want: []string{"DOCKERD_ROOTLESS_ROOTLESSKIT_DISABLE_HOST_LOOPBACK"}},
		"by proxy": {cell: Container, reach: reaching(t, browser.CauseProxy),
			want: []string{"deny to_localhost"}},
		"another kernel": {cell: Container, reach: printing(hostreach.Answer(otherKernel)),
			want: []string{"Docker Engine or Podman"}},
	}
	for _, cell := range Placements {
		if cell != Container {
			cases["a box in "+string(cell)] = tc{cell: cell,
				want: []string{"providers.llm.coder", "in " + string(cell), "run_in to container"}}
		}
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			box := NewFakeSandbox("box-1")
			ensureAnswers(box, readyEnsure)
			box.ReachFunc = c.reach
			out, err := EnsureBrowser(t.Context(), box, BrowserStep{Placement: c.cell,
				Browser: BrowserLaunch{Sandbox: browser.SandboxRequired}, LaunchID: "launch-1",
				WebDenied: c.webDenied, ProviderKey: "coder", Harness: "muse-code", DeferAt: c.deferAt, Bridged: 70,
				InBox: 3})
			if err != nil || out.Record.State != browser.EnsureWithheld {
				t.Fatalf("EnsureBrowser = %+v, %v, want a withheld browser", out, err)
			}
			for _, w := range c.want {
				if !strings.Contains(out.Detail, w) {
					t.Errorf("the operator's detail %q does not say %q", out.Detail, w)
				}
			}
		})
	}
}

// A BROWSER THE BOX ALREADY RUNS IS STOPPED WHERE THE BOX MAY REACH ITS
// HOST'S LOOPBACK: the launch runs the box's stop after a check that finds a
// route, in the box's home with the run's environment, capped in the box at
// exactly stop's bound; a stop that fails leaves the browser withheld, the
// detail saying the browser it ran may still run and why. A box withheld for
// its cell, the web or a harness that would hide the bridge is asked nothing,
// so its running browser is left as the launch found it.
//
// Mutations, each red: drop the run's environment; leave TimeoutSec at zero;
// say nothing of a stop that failed; stop for a web denial; stop for a bridge
// that would be hidden; check the box's network before the bridge's count.
func TestABoxsRunningBrowserIsStoppedWhereItMayReachItsHostsLoopback(t *testing.T) {
	t.Parallel()
	env := map[string]string{"HTTPS_PROXY": "http://proxy.example.com:3128"}
	const deferAt = 100
	for name, c := range map[string]struct {
		exit      int
		webDenied bool
		deferAt   int
		stops     int
		says      string
	}{
		"a stop that answers":                  {stops: 1},
		"a stop that fails":                    {exit: 1, stops: 1, says: "could not be stopped (stop exited 1)"},
		"a run denied the web":                 {webDenied: true},
		"a harness that would hide the bridge": {deferAt: deferAt},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			box := NewFakeSandbox("box-1")
			var got ExecOptions
			box.ExecFunc = func(_ context.Context, cmd string, opts ExecOptions) (ExecResult, error) {
				if cmd == boxhelper.StopCommand(box.Home()) {
					got = opts
					return ExecResult{ExitCode: c.exit}, nil
				}
				return ExecResult{}, nil
			}
			box.ReachFunc = reaching(t, browser.CauseGateway)
			out, err := EnsureBrowser(t.Context(), box, BrowserStep{Placement: Container,
				Browser: BrowserLaunch{Sandbox: browser.SandboxRequired}, Env: env, LaunchID: "launch-1",
				WebDenied: c.webDenied, ProviderKey: "coder", Harness: "muse-code", DeferAt: c.deferAt,
				Bridged: deferAt - len(boxhelper.Tools())})
			if err != nil || out.Record.State != browser.EnsureWithheld {
				t.Fatalf("EnsureBrowser = %+v, %v, want a withheld browser", out, err)
			}
			if n := stopRuns(box); n != c.stops {
				t.Fatalf("the box ran stop %d times, want %d", n, c.stops)
			}
			if c.stops == 0 {
				return
			}
			switch {
			case got.Env["HTTPS_PROXY"] != env["HTTPS_PROXY"] || got.Cwd != DefaultHome:
				t.Errorf("stop ran with %v in %q, want the run's environment in the box's home", got.Env, got.Cwd)
			case got.TimeoutSec != boxhelper.StopTimeout.Seconds():
				t.Errorf("stop's cap is %v seconds, want exactly stop's %v", got.TimeoutSec,
					boxhelper.StopTimeout.Seconds())
			case c.says == "" && strings.Contains(out.Detail, "could not be stopped"):
				t.Errorf("the detail %q says a stop failed, and it answered", out.Detail)
			case c.says != "" && !strings.Contains(out.Detail, c.says):
				t.Errorf("the detail %q does not say %q", out.Detail, c.says)
			}
		})
	}
}

// A RUN DENIED THE WEB WITHHOLDS ITS BROWSER. A seat whose entry denies its
// harness the web (web_tools: denied) and which reaches a container box anyway
// (a revision another build admitted) has its browser withheld as web_denied
// before anything is written or run, the box's network check among it: a
// browser would open the web that denial closes.
//
// Mutations, each red: drop the web check, and the browser is readied; check
// the box's network before the web.
func TestARunDeniedTheWebWithholdsItsBrowser(t *testing.T) {
	t.Parallel()
	rig := newWaiterRigIn(t, Container)
	rig.provider.Made = func(box *FakeSandbox) { ensureAnswers(box, readyEnsure) }
	req := browserReq("t1", Container)
	req.WebDenied = true
	res, err := rig.launchVia(t.Context(), rig.manager, req)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	box := rig.provider.Box(res.SandboxID)
	if files := helperFiles(box); len(files) > 0 || checkRuns(box) > 0 || ensureRuns(box) > 0 {
		t.Errorf("the box was written %q, ran the check %d times and ensure %d times", files, checkRuns(box),
			ensureRuns(box))
	}
	if got := rig.get("t1").Launch.Browser; got == nil || got.Reason != browser.ReasonWebDenied || got.Cause != "" {
		t.Errorf("the job's record holds %+v, want withheld web_denied with no cause", got)
	}
	if !rig.runner.Started()[0].WebDenied {
		t.Error("the run's harness was handed the web")
	}
}

// A BROWSER THAT WOULD HIDE THE BRIDGE IS WITHHELD before anything runs in the
// box. On a harness that hides every MCP server's tools once a run's servers
// list a count of them, a browser whose own list would bring the run's
// servers', the bridge's and the box's own, from below that count to it or
// past is withheld as bridge_deferred, with no cause, check, helper, ensure or
// stop; one that leaves them below it is readied, as is one beside servers at
// it alone, hidden either way, and one on a harness that states no count.
//
// Mutations, each red: withhold only past the count; withhold where the
// servers alone reach it; weigh the servers' without the browser's; weigh the
// bridge's without the box's own; check the box's network before the rule.
func TestABrowserThatWouldHideTheBridgeIsWithheld(t *testing.T) {
	t.Parallel()
	const deferAt = 100
	browserTools := len(boxhelper.Tools())
	for name, c := range map[string]struct {
		deferAt, bridged, inBox int
		withheld                bool
	}{
		"below the count":           {deferAt: deferAt, bridged: deferAt - browserTools - 1},
		"at the count":              {deferAt: deferAt, bridged: deferAt - browserTools, withheld: true},
		"past the count":            {deferAt: deferAt, bridged: deferAt - 1, withheld: true},
		"the bridge alone at it":    {deferAt: deferAt, bridged: deferAt},
		"the bridge alone past it":  {deferAt: deferAt, bridged: deferAt + browserTools},
		"a harness with no count":   {bridged: deferAt - browserTools},
		"no count and no bridge":    {},
		"a count the browser meets": {deferAt: browserTools, withheld: true},
		"the box's own below it": {deferAt: deferAt, bridged: deferAt - browserTools - 11,
			inBox: 10},
		"the box's own bring it to the count": {deferAt: deferAt, bridged: deferAt - browserTools - 10,
			inBox: 10, withheld: true},
		"the bridge and the box's own at it": {deferAt: deferAt, bridged: deferAt - 10, inBox: 10},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			box := NewFakeSandbox("box-1")
			ensureAnswers(box, readyEnsure)
			out, err := EnsureBrowser(t.Context(), box, BrowserStep{Placement: Container,
				Browser: BrowserLaunch{Sandbox: browser.SandboxRequired}, LaunchID: "launch-1",
				ProviderKey: "coder", Harness: "muse-code", DeferAt: c.deferAt, Bridged: c.bridged,
				InBox: c.inBox})
			if err != nil {
				t.Fatalf("EnsureBrowser: %v", err)
			}
			if !c.withheld {
				if out.Record.State != browser.EnsureReady || out.Server == nil {
					t.Errorf("with a count of %d, %d bridged and %d in the box, the browser came to %+v, "+
						"want it ready", c.deferAt, c.bridged, c.inBox, out.Record)
				}
				return
			}
			if r := out.Record; r.State != browser.EnsureWithheld || r.Reason != browser.ReasonBridgeDeferred ||
				r.Cause != "" || out.Server != nil {
				t.Errorf("with a count of %d, %d bridged and %d in the box, the browser came to %+v, want it "+
					"withheld bridge_deferred with no cause and no server", c.deferAt, c.bridged, c.inBox, r)
			}
			if files := helperFiles(box); len(files) > 0 || checkRuns(box) > 0 || ensureRuns(box) > 0 ||
				stopRuns(box) > 0 {
				t.Errorf("the box was written %q, ran the check %d times, ensure %d times and stop %d", files,
					checkRuns(box), ensureRuns(box), stopRuns(box))
			}
		})
	}
}

// A LAUNCH WEIGHS ITS RUNNER'S COUNT AGAINST WHAT ITS SERVERS LIST: the count
// of MCP tools its runner states its harness hides them all from
// (MCPDeferrer), against how many tools what named the job says its servers
// list (Opening.Tools) and the engine says the box's own servers list
// (LaunchRequest.MCPTools), so servers that a browser would bring to the
// count have the browser withheld as bridge_deferred and the run handed no
// browser server, and servers a tool short of it have the browser readied
// and handed.
//
// Mutations, each red: hand the step no runner's count; hand it no bridge's;
// hand it no count of the box's own.
func TestALaunchWeighsItsRunnersCountAgainstWhatItsOpeningLists(t *testing.T) {
	t.Parallel()
	const deferAt = 100
	at := deferAt - len(boxhelper.Tools())
	for name, c := range map[string]struct {
		bridged, inBox int
		withheld       bool
	}{
		"at the count":                        {bridged: at, withheld: true},
		"a tool short of it":                  {bridged: at - 1},
		"the box's own bring it to the count": {bridged: at - 5, inBox: 5, withheld: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newWaiterRigIn(t, Container)
			rig.runner.DeferAt = deferAt
			rig.provider.Made = func(box *FakeSandbox) { ensureAnswers(box, readyEnsure) }
			req := browserReq("t1", Container)
			req.MCPTools = c.inBox
			req.Opened = func(job OpenedJob) (Opening, error) {
				return Opening{Servers: map[string]MCPServer{"crewlet": {Name: "crewlet", Transport: TransportHTTP,
					URL: "https://engine.example.com/mcp/" + job.LaunchID, AlwaysLoaded: true}},
					Tools: c.bridged}, nil
			}
			if _, err := rig.launchVia(t.Context(), rig.manager, req); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			got := rig.get("t1").Launch.Browser
			_, served := rig.runner.Started()[0].MCPServers[browser.ServerName]
			switch {
			case c.withheld && (got == nil || got.Reason != browser.ReasonBridgeDeferred || served):
				t.Errorf("with %d bridged and %d in the box, the job's record holds %+v and the run was handed "+
					"a browser: %t; want it withheld bridge_deferred and none handed", c.bridged, c.inBox, got, served)
			case !c.withheld && (got == nil || got.State != browser.EnsureReady || !served):
				t.Errorf("with %d bridged and %d in the box, the job's record holds %+v and the run was handed "+
					"a browser: %t; want it ready and handed", c.bridged, c.inBox, got, served)
			}
		})
	}
}

// ENSURE RUNS WITH THE RUN'S ENVIRONMENT IN THE BOX'S HOME: the command is
// ensure's for the box's home, the environment the launch's own (the seat's
// run environment and the engine's additions for the job), the directory the
// home, and the box's own cap exactly the engine's bound, never left to the
// cell's default.
//
// Mutations, each red: drop the environment; leave TimeoutSec at zero.
func TestEnsureRunsWithTheRunsEnvironmentInTheBoxsHome(t *testing.T) {
	t.Parallel()
	rig := newWaiterRigIn(t, Container)
	var mu sync.Mutex
	var got []ExecOptions
	rig.provider.Made = func(box *FakeSandbox) {
		box.ExecFunc = func(_ context.Context, cmd string, opts ExecOptions) (ExecResult, error) {
			if cmd != boxhelper.EnsureCommand(box.Home()) {
				return ExecResult{}, nil
			}
			mu.Lock()
			got = append(got, opts)
			mu.Unlock()
			return ExecResult{Stdout: readyEnsure}, nil
		}
	}
	req := browserReq("t1", Container)
	req.Spec.Env = map[string]string{"NPM_CONFIG_REGISTRY": "https://registry.example.com/", "NPM_TOKEN": "npm-..."}
	req.RunEnv = func(string) (LaunchEnv, error) {
		return LaunchEnv{Env: map[string]string{"CREWLET_JOB": "job-env"}}, nil
	}
	if _, err := rig.launchVia(t.Context(), rig.manager, req); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	want := map[string]string{"NPM_CONFIG_REGISTRY": "https://registry.example.com/", "NPM_TOKEN": "npm-...",
		"CREWLET_JOB": "job-env"}
	mu.Lock()
	defer mu.Unlock()
	switch {
	case len(got) != 1:
		t.Fatalf("ensure ran %d times, want once", len(got))
	case !maps.Equal(got[0].Env, want):
		t.Errorf("ensure ran with %v, want the run's environment %v", got[0].Env, want)
	case got[0].Cwd != DefaultHome:
		t.Errorf("ensure ran in %q, want the box's home", got[0].Cwd)
	case got[0].TimeoutSec != boxhelper.EnsureTimeout.Seconds():
		t.Errorf("ensure's cap is %v seconds, want exactly the engine's %v", got[0].TimeoutSec,
			boxhelper.EnsureTimeout.Seconds())
	}
}

// THE BROWSER SERVER REACHES ONLY A BOX WHOSE ENSURE ANSWERED READY. Ready, the
// run is handed `browser`, the shim's plain command line for its job rooted at
// the box's workspace with no environment, headers, secrets, description or
// call bound, in its own wiring and in the wiring a session of its own starts
// on; it replaces a server of that name naming the job bridged, which the
// run's sources then count as the box's own. Every withheld answer hands none.
//
// Mutations, each red: hand the server on any answer; root it at the home;
// leave a bridged browser among what the bridge serves.
func TestTheBrowserServerReachesOnlyABoxWhoseEnsureAnsweredReady(t *testing.T) {
	t.Parallel()
	t.Run("ready", func(t *testing.T) {
		t.Parallel()
		rig := newWaiterRigIn(t, Container)
		rig.provider.Made = func(box *FakeSandbox) { ensureAnswers(box, readyEnsure) }
		c := browserCoordinator(t, rig, nil)
		bridgedBrowser := MCPServer{Name: browser.ServerName, Transport: TransportHTTP,
			URL: "https://engine.example.com/mcp/bridged-browser"}
		opened := func(job OpenedJob) (Opening, error) {
			bridge := MCPServer{Name: "crewlet", Transport: TransportHTTP, URL: "https://engine.example.com/mcp/" +
				job.LaunchID, AlwaysLoaded: true}
			servers := map[string]MCPServer{"crewlet": bridge, browser.ServerName: bridgedBrowser}
			return Opening{Servers: servers, Fresh: &Opening{Servers: maps.Clone(servers)}}, nil
		}
		first, again := browserReq("t1", Container), browserReq("t1", Container)
		first.Opened, again.Opened = opened, opened
		again.Continue = &Continuation{SessionID: "session-cmd-1", Brief: "carry on",
			Summarized: "carry on, from a summary"}
		_, kept := keptLaunch(t, rig, c, first, again)

		started := rig.runner.Started()
		if len(started) != 2 || started[1].FreshWiring == nil {
			t.Fatalf("started %d runs, the relaunch's fresh wiring %v: want two, the second continuing its "+
				"session", len(started), started[len(started)-1].FreshWiring)
		}
		command, args := boxhelper.ShimArgs(DefaultHome, kept.LaunchID, workspaceRoot)
		want := MCPServer{Name: browser.ServerName, Transport: TransportStdio, Command: command, Args: args}
		if i := slices.Index(want.Args, "--root"); i < 0 || want.Args[i+1] != workspaceRoot {
			t.Fatalf("the shim's command line %q is not rooted at the workspace", want.Args)
		}
		for wiring, servers := range map[string]map[string]MCPServer{
			"its own": started[1].MCPServers, "a session of its own": started[1].FreshWiring.MCPServers} {
			if got := servers[browser.ServerName]; !reflect.DeepEqual(got, want) {
				t.Errorf("the run's %s wiring holds the browser as %+v, want %+v", wiring, got, want)
			}
			if _, ok := servers["crewlet"]; !ok {
				t.Errorf("the run's %s wiring lost the bridge's own server", wiring)
			}
		}
		asks := rig.runner.PointAsks()
		if len(asks) == 0 || asks[len(asks)-1].Sources == nil ||
			!slices.Contains(asks[len(asks)-1].Sources.Unsized, browser.ServerName) {
			t.Errorf("the run's sources were read as %+v, want the browser among the box's own", asks)
		}
	})
	type withheldCase struct {
		cell   Placement
		stdout string
		edit   func(*LaunchRequest)
	}
	withheld := map[string]withheldCase{
		"denied the web": {cell: Container, stdout: readyEnsure,
			edit: func(r *LaunchRequest) { r.WebDenied = true }},
		"a box's own reason": {cell: Container,
			stdout: "crewlet-browser-ensure/1 withheld reason=chromium_missing cause=not_found"},
		"an answer it cannot read": {cell: Container, stdout: "warming up"},
	}
	for _, cell := range Placements {
		if cell != Container {
			withheld["a box in "+string(cell)] = withheldCase{cell: cell, stdout: readyEnsure}
		}
	}
	for name, w := range withheld {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newWaiterRigIn(t, w.cell)
			rig.provider.Made = func(box *FakeSandbox) { ensureAnswers(box, w.stdout) }
			req := browserReq("t1", w.cell)
			if w.edit != nil {
				w.edit(&req)
			}
			if _, err := rig.launchVia(t.Context(), rig.manager, req); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			if got, served := rig.runner.Started()[0].MCPServers[browser.ServerName]; served {
				t.Errorf("a withheld browser handed the run %+v", got)
			}
		})
	}
}

// ENSURE RUNS AFTER THE BOX IS READY AND BEFORE THE HARNESS: in a fresh box
// after its setup's commands and its reading, in a kept one after its files
// are refreshed, and in either after the check of the box's network, before
// the launch's caller is told how the run takes up its session and before
// the harness starts.
//
// Mutations, each red: run ensure before the box's reading, or after the
// run's start; check the box's network after ensure, or only in a fresh box.
func TestEnsureRunsAfterTheBoxIsReadyAndBeforeTheHarness(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var order []string
	note := func(what string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, what)
	}
	rig := newWaiterRigIn(t, Container)
	rig.provider.Made = func(box *FakeSandbox) {
		box.ReachFunc = func(context.Context, ExecOptions) (ExecResult, error) {
			note(fmt.Sprintf("check after %d starts", len(rig.runner.Started())))
			return ExecResult{Stdout: hostreach.Answer(FakeKernel)}, nil
		}
		box.ExecFunc = func(_ context.Context, cmd string, _ ExecOptions) (ExecResult, error) {
			switch cmd {
			case "make deps":
				note("setup")
			case boxFactsScript:
				note("read")
			case boxhelper.EnsureCommand(box.Home()):
				note(fmt.Sprintf("ensure after %d starts", len(rig.runner.Started())))
				return ExecResult{Stdout: readyEnsure}, nil
			}
			return ExecResult{}, nil
		}
	}
	c := browserCoordinator(t, rig, func(context.Context, *Manager, PendingRun, Sandbox, RecordHanded) {
		note("refresh")
	})
	first, again := browserReq("t1", Container), browserReq("t1", Container)
	first.Setup, first.ReadBox = []SetupStep{{Name: "deps", Commands: []string{"make deps"}}}, true
	first.Decided = func(LaunchDecision) { note("decided") }
	again.Decided = first.Decided
	keptLaunch(t, rig, c, first, again)
	mu.Lock()
	defer mu.Unlock()
	want := []string{"setup", "read", "check after 0 starts", "ensure after 0 starts", "decided",
		"refresh", "check after 1 starts", "ensure after 1 starts", "decided"}
	if !slices.Equal(order, want) {
		t.Errorf("the launches ran %q, want %q", order, want)
	}
}

// AN ENSURE THAT SAYS NOTHING IN TWO MINUTES WITHHOLDS THE BROWSER: an exec
// silent until its context ends is cut at exactly the engine's bound and read
// as ensure_timeout, and the launch goes on, its run started without a
// browser server and told why.
//
// Mutation: run ensure unbounded, and the bubble reports the deadlock.
func TestAnEnsureThatSaysNothingInTwoMinutesWithholdsTheBrowser(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		rig := newWaiterRigIn(t, Container)
		var took time.Duration
		rig.provider.Made = func(box *FakeSandbox) {
			box.ExecFunc = func(ctx context.Context, cmd string, _ ExecOptions) (ExecResult, error) {
				if cmd != boxhelper.EnsureCommand(box.Home()) {
					return ExecResult{}, nil
				}
				began := time.Now()
				<-ctx.Done()
				took = time.Since(began)
				return ExecResult{ExitCode: 124}, nil
			}
		}
		res, err := rig.launchVia(t.Context(), rig.manager, browserReq("t1", Container))
		if err != nil {
			t.Fatalf("a launch whose ensure said nothing: %v", err)
		}
		if took != boxhelper.EnsureTimeout {
			t.Errorf("ensure was cut after %v, want exactly %v", took, boxhelper.EnsureTimeout)
		}
		got := rig.get("t1").Launch.Browser
		if got == nil || got.Reason != browser.ReasonEnsureTimeout || got.Cause != "" || res.CommandID == "" {
			t.Errorf("the job's record holds %+v and its command %q, want ensure_timeout and the run started",
				got, res.CommandID)
		}
		run := rig.runner.Started()[0]
		if _, served := run.MCPServers[browser.ServerName]; served ||
			!strings.Contains(run.System, browserWithheldWords(browser.ReasonEnsureTimeout)) {
			t.Errorf("the run was handed a browser server, or not told why it has none:\n%s", run.System)
		}
	})
}

// AN ENSURE THAT FAILS WITH NOTHING PRINTED IS UNREADABLE, and the operator's
// log alone says how: an exec that could not run gives its error, and one
// that ran and exited non-zero its exit status, which a container exec
// answers with no error, never what the box wrote on stderr (a box with no
// sh exits 126 or 127, a killed one 137, a helper that failed 1). The browser
// is withheld as ensure_unreadable no_line, and the run starts without it, its
// standing instructions saying why in the engine's words, never the error's.
//
// Mutations, each red: read a failed exec as the launch's failure; leave the
// exit status out of the detail; put the box's stderr in it.
func TestAnEnsureThatFailsWithNothingPrintedIsUnreadable(t *testing.T) {
	t.Parallel()
	const said = "zq-the-runtime-said-this"
	for name, c := range map[string]struct {
		res    ExecResult
		err    error
		detail string
	}{
		"an exec that could not run": {err: errors.New(said), detail: said},
		"a box with no shell": {res: ExecResult{ExitCode: 127, Stderr: said},
			detail: "ensure exited 127 and printed no line"},
		"a killed ensure": {res: ExecResult{ExitCode: 137, Stderr: said},
			detail: "ensure exited 137 and printed no line"},
		"a helper that failed": {res: ExecResult{ExitCode: 1, Stderr: said},
			detail: "ensure exited 1 and printed no line"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			answer := func(box *FakeSandbox) {
				box.ExecFunc = func(_ context.Context, cmd string, _ ExecOptions) (ExecResult, error) {
					if cmd == boxhelper.EnsureCommand(box.Home()) {
						return c.res, c.err
					}
					return ExecResult{}, nil
				}
			}
			rig := newWaiterRigIn(t, Container)
			rig.provider.Made = answer
			res, err := rig.launchVia(t.Context(), rig.manager, browserReq("t1", Container))
			if err != nil {
				t.Fatalf("a launch whose ensure failed: %v", err)
			}
			got := rig.get("t1").Launch.Browser
			if got == nil || got.Reason != browser.ReasonEnsureUnreadable || got.Cause != browser.CauseNoLine ||
				res.CommandID == "" {
				t.Errorf("the job's record holds %+v and its command %q, want ensure_unreadable no_line and the "+
					"run started", got, res.CommandID)
			}
			if system := rig.runner.Started()[0].System; strings.Contains(system, said) {
				t.Errorf("the standing instructions carry the exec's error:\n%s", system)
			}
			box := NewFakeSandbox("box-1")
			answer(box)
			out, err := EnsureBrowser(t.Context(), box, BrowserStep{Placement: Container,
				Browser: BrowserLaunch{Sandbox: browser.SandboxRequired}, LaunchID: "launch-1"})
			if err != nil || out.Detail != c.detail {
				t.Errorf("the operator's detail is %q (%v), want %q", out.Detail, err, c.detail)
			}
		})
	}
}

// THE LAUNCH'S OWN CONTEXT ENDING WHILE ENSURE RUNS ABANDONS THE LAUNCH, as
// every failure past the box does: the box is reclaimed, the run's record
// ended, and no harness starts. It is the one way the step fails a launch.
//
// Mutation: read the launch's own cancellation as a withheld browser.
func TestTheLaunchsOwnEndWhileEnsureRunsAbandonsIt(t *testing.T) {
	t.Parallel()
	rig := newWaiterRigIn(t, Container)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var made string
	rig.provider.Made = func(box *FakeSandbox) {
		made = box.ID()
		box.ExecFunc = func(_ context.Context, cmd string, _ ExecOptions) (ExecResult, error) {
			if cmd == boxhelper.EnsureCommand(box.Home()) {
				cancel()
			}
			return ExecResult{}, nil
		}
	}
	if _, err := rig.launchVia(ctx, rig.manager, browserReq("t1", Container)); !errors.Is(err, context.Canceled) {
		t.Fatalf("a launch cancelled while ensure ran answered %v, want its cancellation", err)
	}
	if killed := rig.provider.KilledIDs(); made == "" || !slices.Contains(killed, made) {
		t.Errorf("killed %q, want the launch's own box %q among them", killed, made)
	}
	if _, found, err := rig.pending.Get(t.Context(), "t1"); err != nil || found {
		t.Errorf("the abandoned launch left its row (found %v, err %v)", found, err)
	}
	if started := rig.runner.Started(); len(started) != 0 {
		t.Errorf("an abandoned launch started %d jobs", len(started))
	}
}

// refusingBox is a box that refuses every write under its browser folder
// with err, as a box whose job put a link or a pipe there refuses one.
type refusingBox struct {
	*FakeSandbox
	err error
}

func (b refusingBox) WriteFile(ctx context.Context, p string, content []byte) error {
	if strings.HasPrefix(p, boxhelper.Folder(b.Home())+"/") {
		return fmt.Errorf("writing %s: %w", p, b.err)
	}
	return b.FakeSandbox.WriteFile(ctx, p, content)
}

// refusingBoxes is a provider of [refusingBox]es, each refusing with err.
type refusingBoxes struct {
	*FakeProvider
	err error
}

func (p refusingBoxes) Create(ctx context.Context, spec Spec) (Sandbox, error) {
	box, err := p.FakeProvider.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	return refusingBox{FakeSandbox: box.(*FakeSandbox), err: p.err}, nil
}

// A HELPER THE BOX REFUSES WITHHOLDS THE BROWSER, never the launch: a write
// refused as no regular file is helper_unwritten refused, any other failed
// write helper_unwritten failed; ensure never runs, and the run starts.
//
// Mutation: fail the launch on the helper's write, or read every failure as
// refused.
func TestAHelperTheBoxRefusesWithholdsTheBrowser(t *testing.T) {
	t.Parallel()
	for cause, err := range map[browser.EnsureCause]error{
		browser.CauseRefused: &NotRegularFileError{Path: "/home/user/.crewlet", Kind: "a symbolic link", Write: true},
		browser.CauseFailed:  errors.New("the box is gone"),
	} {
		t.Run(string(cause), func(t *testing.T) {
			t.Parallel()
			rig := newWaiterRigIn(t, Container)
			rig.provider.Made = func(box *FakeSandbox) { ensureAnswers(box, readyEnsure) }
			m, mErr := NewManager(ManagerOptions{
				Providers: map[Placement]Provider{Container: refusingBoxes{FakeProvider: rig.provider, err: err}},
				Runners:   map[string]Runner{"claude-code": rig.runner},
			})
			if mErr != nil {
				t.Fatalf("NewManager: %v", mErr)
			}
			res, lErr := rig.launchVia(t.Context(), m, browserReq("t1", Container))
			if lErr != nil {
				t.Fatalf("a launch whose box refused the helper: %v", lErr)
			}
			got := rig.get("t1").Launch.Browser
			if got == nil || got.Reason != browser.ReasonHelperUnwritten || got.Cause != cause || res.CommandID == "" {
				t.Errorf("the job's record holds %+v, its command %q: want helper_unwritten %s and the run started",
					got, res.CommandID, cause)
			}
			if n := ensureRuns(rig.provider.Box(res.SandboxID)); n > 0 {
				t.Errorf("ensure ran %d times over a helper the box refused", n)
			}
		})
	}
}

// NOTHING A BOX PRINTS REACHES THE BRIEF. Words beside ensure's line, before
// it, and inside a line off its grammar are read as ensure_unreadable
// bad_line, and none of them is in the run's standing instructions, which say
// why in the engine's words alone.
//
// Mutation: put the box's line in the brief.
func TestNothingABoxPrintsReachesTheBrief(t *testing.T) {
	t.Parallel()
	const marker = "zq-the-box-said-this"
	for name, stdout := range map[string]string{
		"beside the line":               "crewlet-browser-ensure/1 withheld reason=chromium_missing cause=not_found " + marker,
		"before a line off its grammar": marker + "\ncrewlet-browser-ensure/1 maybe helper=0123456789abcdef",
		"inside the line":               strings.Replace(readyEnsure, "node=22.23.5", "node="+marker, 1),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newWaiterRigIn(t, Container)
			rig.provider.Made = func(box *FakeSandbox) { ensureAnswers(box, stdout) }
			if _, err := rig.launchVia(t.Context(), rig.manager, browserReq("t1", Container)); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			got := rig.get("t1").Launch.Browser
			if got == nil || got.Reason != browser.ReasonEnsureUnreadable || got.Cause != browser.CauseBadLine {
				t.Errorf("the job's record holds %+v, want ensure_unreadable bad_line", got)
			}
			if system := rig.runner.Started()[0].System; strings.Contains(system, marker) ||
				!strings.Contains(system, browserWithheldWords(browser.ReasonEnsureUnreadable)) {
				t.Errorf("the standing instructions carry what the box printed, or not why:\n%s", system)
			}
		})
	}
}

// A LAUNCH RECORDS ITS BROWSER ON ITS ANNOUNCEMENT AND ITS JOB, alike: ready
// with what ensure knew, or withheld with its reason and cause; a seat with
// no browser has neither. The operator's log says it too, each line with the
// entry the run launched on, the withheld line with its reason, cause, the
// cell the box is in and a detail naming what to change.
//
// Not parallel: it captures the process-wide logger ([captureLogs]).
//
// Mutations, each red: leave the browser off the attach naming the job's
// command; leave the cell, the entry or the detail off the withheld line.
func TestALaunchRecordsItsBrowserOnItsAnnouncementAndItsJob(t *testing.T) {
	for name, c := range map[string]struct {
		req  func() LaunchRequest
		want *types.ExecutorBrowser
		line string
	}{
		"ready": {req: func() LaunchRequest {
			req := browserReq("t1", Container)
			req.ProviderKey = "coder"
			return req
		},
			want: &types.ExecutorBrowser{State: browser.EnsureReady, Chromium: browser.ExpectedChromiumVersion,
				ChromiumMajor: expectedMajor(t), Build: browser.BuildExpected, Sandbox: browser.SandboxRequired,
				Node: "22.23.5", Helper: "0123456789abcdef", Pin: "0.0.86"},
			line: "msg=executor_browser_ready"},
		"withheld": {req: func() LaunchRequest {
			req := browserReq("t1", Container)
			req.WebDenied, req.ProviderKey = true, "coder"
			return req
		}, want: &types.ExecutorBrowser{State: browser.EnsureWithheld, Reason: browser.ReasonWebDenied},
			line: "msg=executor_browser_withheld"},
		"no browser": {req: func() LaunchRequest {
			req := browserReq("t1", Container)
			req.Browser = nil
			return req
		}},
	} {
		t.Run(name, func(t *testing.T) {
			logs := captureLogs(t)
			rig := newWaiterRigIn(t, Container)
			rig.provider.Made = func(box *FakeSandbox) { ensureAnswers(box, readyEnsure) }
			if _, err := rig.launchVia(t.Context(), rig.manager, c.req()); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			rig.queue.mu.Lock()
			announced := rig.queue.published[0].event.Data.(*types.ExecutorLaunched).Browser
			rig.queue.mu.Unlock()
			if job := rig.get("t1").Launch.Browser; !reflect.DeepEqual(announced, c.want) ||
				!reflect.DeepEqual(job, c.want) {
				t.Errorf("announced %+v, the job's record %+v, want both %+v", announced, job, c.want)
			}
			said := logs.String()
			var line string
			for l := range strings.Lines(said) {
				if strings.Contains(l, "executor_browser_") {
					line = l
				}
			}
			switch {
			case c.line == "" && line != "":
				t.Errorf("a seat with no browser logged one: %s", line)
			case c.line == "":
			case !strings.Contains(line, c.line) || !strings.Contains(line, "provider_key=coder"):
				t.Errorf("the log has no %s naming the entry:\n%s", c.line, said)
			case c.want.State == browser.EnsureWithheld && (!strings.Contains(line, "reason=web_denied") ||
				!strings.Contains(line, "placement=container") ||
				!strings.Contains(line, "providers.llm.coder denies its harness the web")):
				t.Errorf("the withheld line names no reason, cell or what to change: %s", line)
			}
		})
	}
}

// expectedMajor is the major of the Chromium build the pinned Playwright
// expects.
func expectedMajor(t *testing.T) int {
	t.Helper()
	major, ok := browser.ChromiumMajor(browser.ExpectedChromiumVersion)
	if !ok {
		t.Fatalf("browser.ExpectedChromiumVersion %q has no major", browser.ExpectedChromiumVersion)
	}
	return major
}

// AN OLDER BUILD'S WRITE KEEPS THE JOB'S BROWSER. A fleet mid-upgrade rewrites
// the job's record on a build that does not know it, and a reader on this
// build reads it after: so it rides the record under its own key, which that
// build keeps verbatim and writes back.
//
// Mutation: tag the field `json:"-"`, and this goes red.
func TestAnOlderBuildsWriteKeepsTheJobsBrowser(t *testing.T) {
	t.Parallel()
	carriedByAnOlderBuild(t, reflect.TypeFor[LaunchRecord](), []string{"browser"},
		LaunchRecord{Browser: &types.ExecutorBrowser{State: browser.EnsureWithheld,
			Reason: browser.ReasonHelperUnwritten, Cause: browser.CauseRefused}}, func(raw []byte) any {
			var back LaunchRecord
			if err := json.Unmarshal(raw, &back); err != nil {
				t.Fatal(err)
			}
			return back.Browser
		})
}

// THE BRIEF TELLS A READY BROWSER AND A WITHHELD ONE IN THE ENGINE'S WORDS: a
// ready browser's paragraph names its server, where its unnamed files go, how
// long it keeps its state, the fence on a page's text, the approval before a
// consequential act through the ask tool, and no secret; a withheld one's
// line names the reason in the engine's words; and neither changes with what
// ensure printed of the box.
//
// Mutation: put the box's Chromium version in the withheld line.
func TestTheBriefTellsAReadyBrowserAndAWithheldOneInTheEnginesWords(t *testing.T) {
	t.Parallel()
	printed := types.ExecutorBrowser{Chromium: "zq.1.2.3", Node: "zq-node", Pin: "zq-pin", Helper: "zq-helper"}
	ready := BrowserOutcome{Record: printed}
	ready.Record.State = browser.EnsureReady
	brief := ready.Brief()
	for _, said := range []string{
		"**Your browser.** This box has a web browser: a headless Chromium you drive through the `" +
			browser.ServerName + "` MCP server",
		"go to " + path.Join(boxhelper.Folder("~"), "output") + ";",
		"a file you name is saved in your working directory",
		"a new box starts it empty",
		"fenced as text somebody else wrote",
		"ask the people who lead you with `" + toolname.AskAndWait + "`",
		"Never type a password or other secret into a page",
	} {
		if !strings.Contains(brief, said) {
			t.Errorf("the ready paragraph does not say %q:\n%s", said, brief)
		}
	}
	for i, r := range brief {
		if r > 0x7e || (r < 0x20 && r != '\n') {
			t.Errorf("the ready paragraph holds %q at %d: it is ASCII alone", r, i)
		}
	}
	for _, reason := range browser.EnsureReasons() {
		withheld := BrowserOutcome{Record: printed}
		withheld.Record.State, withheld.Record.Reason = browser.EnsureWithheld, reason
		want := "**Your browser.** This seat has a browser, but this box cannot run it: " +
			browserWithheldWords(reason) + ". Say so in your report if the work needed one."
		if got := withheld.Brief(); got != want {
			t.Errorf("withheld as %s, the brief says %q, want %q", reason, got, want)
		}
	}
	if (*BrowserOutcome)(nil).Brief() != "" {
		t.Error("a seat with no browser is told of one")
	}
}

// EVERY WITHHELD REASON HAS THE ENGINE'S WORDS FOR THE MODEL, its figures read
// from the constants they name: the Node floor from boxhelper.MinNodeMajor,
// the seconds from boxhelper.EnsureTimeout, and no number written into the
// words themselves.
//
// Mutations, each red: drop a reason's words; write the Node floor as 22.
func TestEveryWithheldReasonHasTheEnginesWordsForTheModel(t *testing.T) {
	t.Parallel()
	for _, r := range browser.EnsureReasons() {
		if browserWithheldWords(r) == "" {
			t.Errorf("%s has no words for the model", r)
		}
	}
	if got := browserWithheldWords(browser.ReasonNodeTooOld); !strings.Contains(got,
		strconv.Itoa(boxhelper.MinNodeMajor)) {
		t.Errorf("node_too_old reads %q, which does not name the floor %d", got, boxhelper.MinNodeMajor)
	}
	if got := browserWithheldWords(browser.ReasonEnsureTimeout); !strings.Contains(got,
		strconv.Itoa(int(boxhelper.EnsureTimeout.Seconds()))+" seconds") {
		t.Errorf("ensure_timeout reads %q, which does not name the bound", got)
	}
	file, err := parser.ParseFile(token.NewFileSet(), "browser.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "browserWithheldWords" {
			continue
		}
		found = true
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && strings.ContainsAny(lit.Value, "0123456789") {
				t.Errorf("browserWithheldWords writes the figure %s itself, rather than reading its constant", lit.Value)
			}
			return true
		})
	}
	if !found {
		t.Fatal("browser.go declares no browserWithheldWords, so this case reads nothing")
	}
}

// ONLY THE LAUNCH MAKES A BROWSER SERVER: no Go file of the module but
// internal/sandbox/browser.go calls the shim's command line (boxhelper and its
// test support aside, which run the shim on a host) or builds an MCP server
// named browser.ServerName, so the server reaches a box only through ensure's
// ready answer; and the walk finds that file, so it reads the tree it holds.
//
// Mutation: build one in the engine's bridge servers.
func TestOnlyTheLaunchMakesABrowserServer(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	var makers []string
	err := sourcetree.Walk(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") ||
			strings.HasPrefix(rel, "internal/browser/boxhelper/") {
			return nil
		}
		raw, err := os.ReadFile(p) //nolint:gosec // a file of the module's own tree
		if err != nil {
			return err
		}
		if !bytes.Contains(raw, []byte("ShimArgs")) && !bytes.Contains(raw, []byte("ServerName")) {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), p, raw, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		if makesBrowserServer(file) {
			makers = append(makers, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"internal/sandbox/browser.go"}; !slices.Equal(makers, want) {
		t.Errorf("browser servers are made in %q, want only %q", makers, want)
	}
}

// makesBrowserServer reports whether file calls the shim's command line or
// builds an MCP server named browser.ServerName.
func makesBrowserServer(file *ast.File) bool {
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "ShimArgs" {
				found = true
			}
		case *ast.CompositeLit:
			if !namesMCPServer(n.Type) {
				return true
			}
			for _, elt := range n.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, keyed := kv.Key.(*ast.Ident)
				value, named := kv.Value.(*ast.SelectorExpr)
				if keyed && named && key.Name == "Name" && value.Sel.Name == "ServerName" {
					found = true
				}
			}
		}
		return !found
	})
	return found
}

// namesMCPServer reports whether typ is MCPServer, by its own name or its
// package's.
func namesMCPServer(typ ast.Expr) bool {
	switch typ := typ.(type) {
	case *ast.Ident:
		return typ.Name == "MCPServer"
	case *ast.SelectorExpr:
		return typ.Sel.Name == "MCPServer"
	}
	return false
}
