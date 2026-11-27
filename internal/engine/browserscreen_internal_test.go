package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/browser/boxhelper"
	"github.com/crewlet/crewlet/internal/browser/screen"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/costlimit"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// A node builds a job's browser screen where the job's bridge opens: one hub
// for each opening of a job whose launch asked for a browser, on the node's
// one set of limits, serving only once the job's ensure answered ready at or
// above the floor, or at once for a job opened again from a record that says
// so.

// ensureReady is ensure's ready line for a Chromium of version, on build.
func ensureReady(version, build string) string {
	return "crewlet-browser-ensure/1 ready helper=0123456789abcdef pin=0.0.86 node=22.23.5 chromium=" +
		version + " build=" + build + " sandbox=required supervisor=started"
}

// screenState is a state a box's relay posts.
const screenState = `{"v":1,"boot":"00000000000000b2","rev":1,"running":"running",` +
	`"tabs":[{"id":"t1","title":"a page","url":"https://example.com/","front":true}]}`

// postScreenState posts screenState to the browser routes of endpoint, the
// job's own, answering the status and the refusal's code.
func postScreenState(t *testing.T, client *http.Client, endpoint string) (int, string) {
	t.Helper()
	res, err := client.Post(mcpbridge.BrowserURL(endpoint)+"/state", "application/json",
		strings.NewReader(screenState))
	if err != nil {
		t.Fatalf("post a state: %v", err)
	}
	defer res.Body.Close()
	var refusal struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(res.Body).Decode(&refusal)
	return res.StatusCode, refusal.Error
}

// screenedLaunch is a job launched in a container box whose bridge a listener
// serves: the endpoint its box was handed, the run's request and its launch.
type screenedLaunch struct {
	endpoint string
	run      sandbox.RunRequest
	launch   string
	client   *http.Client
}

// launchScreened launches a seat's job, a browser seat's where browserSeat,
// in a container box whose ensure answers ensure.
func launchScreened(t *testing.T, browserSeat bool, ensure string) screenedLaunch {
	t.Helper()
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	bridge := bridgeFor(t, mcpbridge.Options{Material: runtoken.OneKey("k", "test-key"), BaseURL: server.URL})
	mux := http.NewServeMux()
	mux.Handle(mcpbridge.PathPrefix+"{token}", bridge.Handler())
	mux.Handle(mcpbridge.BrowserPattern, bridge.BrowserHandler())
	server.Config.Handler = mux
	c, seat := chainCompany(t, chainMember{key: "api", agent: "claude-code", model: "claude-golden",
		runIn: config.PlacementContainer})
	if browserSeat {
		seat.Sandbox = &org.RoleSandbox{Browser: true}
	}
	c.Config.Providers.Sandbox = localCatalogue("")
	e, jobs := launchRig(t, c, bridge)
	provider, err := c.catalogue.Provider(sandbox.Container)
	if err != nil {
		t.Fatalf("the container cell's provider: %v", err)
	}
	provider.(*sandbox.FakeProvider).Made = func(box *sandbox.FakeSandbox) {
		box.ExecFunc = func(_ context.Context, cmd string, _ sandbox.ExecOptions) (sandbox.ExecResult, error) {
			if cmd == boxhelper.EnsureCommand(box.Home()) {
				return sandbox.ExecResult{Stdout: ensure}, nil
			}
			return sandbox.ExecResult{}, nil
		}
	}
	l := launcherOn(e, c, seat, "t1")
	if _, err := l.LaunchExecutor(t.Context(), launchRequest(l, nil)); err != nil {
		t.Fatalf("LaunchExecutor: %v", err)
	}
	started := jobs.Started()
	if len(started) != 1 {
		t.Fatalf("%d jobs started, want 1", len(started))
	}
	row, found, err := e.sandbox.Load().pending.Get(t.Context(), "t1")
	if err != nil || !found {
		t.Fatalf("the launch's row: found %v, %v", found, err)
	}
	return screenedLaunch{endpoint: started[0].MCPServers[config.ToolServerName].URL, run: started[0],
		launch: row.LaunchID, client: server.Client()}
}

// A JOB'S SCREEN IS BUILT WHERE ITS BRIDGE OPENS, AND SERVES ONLY ONCE READY.
// A browser seat's launch builds its job's hub before the bridge files the
// opening, and it serves once ensure answered ready at or above the floor,
// the run handed where its relay posts; ready below the floor, or withheld,
// it never serves, and the run is handed nothing; a seat with no browser has
// no screen. Every one not serving answers 404 `not_found`.
//
// Mutation: build every hub serving, and the screen of a browser below the
// floor, or withheld, serves.
func TestAJobsHubIsBuiltWhereItsBridgeOpensAndServesOnlyOnceReady(t *testing.T) {
	t.Parallel()
	withheld := "crewlet-browser-ensure/1 withheld reason=chromium_missing cause=not_found"
	below := ensureReady(fmt.Sprintf("%d.0.7000.10", browser.MinChromiumMajor-1), "untested")
	for name, c := range map[string]struct {
		browserSeat bool
		ensure      string
		serves      bool
	}{
		"ready on the expected build": {true, ensureReady(browser.ExpectedChromiumVersion, "expected"), true},
		"ready below the floor":       {true, below, false},
		"withheld":                    {true, withheld, false},
		"a seat with no browser":      {false, "", false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			job := launchScreened(t, c.browserSeat, c.ensure)
			status, code := postScreenState(t, job.client, job.endpoint)
			want := sandbox.BrowserWiring{}
			if c.serves {
				want = sandbox.BrowserWiring{URL: mcpbridge.BrowserURL(job.endpoint), Job: job.launch}
				if status != http.StatusNoContent {
					t.Errorf("the relay's first state was answered %d %q, want 204", status, code)
				}
			} else if status != http.StatusNotFound || code != "not_found" {
				t.Errorf("a state was answered %d %q, want 404 not_found: no screen serves", status, code)
			}
			if job.run.Browser != want {
				t.Errorf("the run was handed %+v for its relay, want %+v", job.run.Browser, want)
			}
		})
	}
}

// A RECOVERED JOB'S SCREEN SERVES AGAIN FROM ITS RECORD. A node that opens a
// running job's session again as it takes its seat over builds the job's hub
// serving where the job's record says its browser relays its screen, so the
// relay's first request reaching that node is served; where the record says
// it was withheld, there is no screen.
//
// Mutation: build a recovered job's hub waiting, and the relay's first
// request is answered 404, which ends the relay.
func TestARecoveredJobsBrowserIsServedAgainFromItsRecord(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		record *types.ExecutorBrowser
		status int
	}{
		"ready at the floor": {&types.ExecutorBrowser{State: browser.EnsureReady,
			ChromiumMajor: browser.MinChromiumMajor}, http.StatusNoContent},
		"withheld": {&types.ExecutorBrowser{State: browser.EnsureWithheld,
			Reason: browser.ReasonChromiumMissing}, http.StatusNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			run := launchRecoverable(t)
			run.restart()
			row := run.row(t)
			row.Launch.Browser = c.record
			run.e.reopenAgentRun(t.Context(), row)
			if status, code := postScreenState(t, httpxtest.Pool(t), run.endpoint); status != c.status {
				t.Errorf("the relay's first state after the recovery was answered %d %q, want %d", status,
					code, c.status)
			}
		})
	}
}

// A SCREEN WHOSE OPENING IS REFUSED IS CLOSED. Built before the bridge files
// the opening, a job's hub that no opening took is closed where it was built,
// since no opening's end ever will; and a job with no browser leaves its
// session's screen nil, never a typed nil the bridge would serve.
//
// Mutation: leave the refused opening's hub open, and it answers a poll.
func TestAHubWhoseOpeningIsRefusedIsClosed(t *testing.T) {
	t.Parallel()
	seat := &org.Role{Name: "Engineer", DeclaredHandle: "swe"}
	refused := &Engine{bridge: bridgeFor(t, mcpbridge.Options{Material: runtoken.OneKey("k", "test-key")}),
		screenLimits: screen.NewLimits(nil)}
	session := screenSession(seat)
	if _, err := refused.openBridge(t.Context(), session, newBridgeLedger(nil, nil), nil,
		sandbox.OpenedJob{LaunchID: "job-1", Browser: sandbox.JobBrowserAsked}); err == nil {
		t.Fatal("a bridge nothing reaches opened the job's session")
	}
	if session.Browser == nil {
		t.Fatal("the refused job's session holds no screen to look at")
	}
	if _, err := session.Browser.Answer(); !errors.Is(err, browser.ErrScreenClosed) {
		t.Errorf("the refused opening's screen answers %v, want %v", err, browser.ErrScreenClosed)
	}

	served := &Engine{bridge: bridgeFor(t, mcpbridge.Options{Material: runtoken.OneKey("k", "test-key"),
		BaseURL: "https://engine.example.com"}), screenLimits: screen.NewLimits(nil)}
	_ = served.bridge.Handler()
	bare := screenSession(seat)
	opening, err := served.openBridge(t.Context(), bare, newBridgeLedger(nil, nil), nil,
		sandbox.OpenedJob{LaunchID: "job-1"})
	if err != nil {
		t.Fatalf("openBridge: %v", err)
	}
	t.Cleanup(func() { served.bridge.Close(bare.RunID) })
	if bare.Browser != nil || opening.Browser.Set() || opening.BrowserReady != nil {
		t.Errorf("a job with no browser was given a screen %v, a relay's wiring %+v, a readiness %v",
			bare.Browser, opening.Browser, opening.BrowserReady != nil)
	}
}

// EVERY JOB'S SCREEN ON A NODE SHARES THE NODE'S LIMITS: built on the
// engine's one set, two jobs' hubs pay into one node bucket, so one job's
// post can spend what another's then waits for.
//
// Mutation: build each hub on limits of its own, and the second job's post
// is taken.
func TestEveryJobsScreenOnANodeSharesItsLimits(t *testing.T) {
	t.Parallel()
	one := costlimit.Rates{PrincipalRate: 100, PrincipalBurst: 100, ScopeRate: 1, ScopeBurst: 1}
	e := &Engine{bridge: bridgeFor(t, mcpbridge.Options{Material: runtoken.OneKey("k", "test-key"),
		BaseURL: "https://engine.example.com"}),
		screenLimits: screen.Limits{Frames: costlimit.New(one, nil), States: costlimit.New(one, nil),
			Checkins: costlimit.New(one, nil)}}
	_ = e.bridge.Handler()
	seat := &org.Role{Name: "Engineer", DeclaredHandle: "swe"}
	var answers []error
	for _, job := range []string{"job-1", "job-2"} {
		session := screenSession(seat)
		session.RunID = "run-" + job
		if _, err := e.openBridge(t.Context(), session, newBridgeLedger(nil, nil), nil,
			sandbox.OpenedJob{LaunchID: job, Browser: sandbox.JobBrowserReady}); err != nil {
			t.Fatalf("openBridge: %v", err)
		}
		t.Cleanup(func() { e.bridge.Close(session.RunID) })
		answers = append(answers, session.Browser.State(t.Context(), func() ([]byte, error) {
			return []byte(screenState), nil
		}))
	}
	if answers[0] != nil {
		t.Fatalf("the first job's state: %v, want it taken", answers[0])
	}
	if _, ok := errors.AsType[*browser.TooFast](answers[1]); !ok {
		t.Errorf("the second job's state, past the node's bucket the first spent: %v, want it refused too fast",
			answers[1])
	}
}

// screenSession is a session of the run run-1 on seat's turn, its surface
// empty.
func screenSession(seat *org.Role) *mcpbridge.Session {
	turn := &turnctx.Turn{RunID: "run-1", Seat: seat, Org: &org.Organization{Name: "Acme",
		Roles: []*org.Role{seat}}}
	return &mcpbridge.Session{RunID: "run-1", Handle: "swe", Role: seat.Name,
		Surface: tools.NewSurface("execute", tools.NewRegistry().Snapshot(), nil).ForTurn(turn)}
}
