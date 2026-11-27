package sandbox

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/events/types"
)

// What serves a job's box is told its browser, and the run where its relay
// posts, only for a browser whose screen the engine relays: ready, at or above
// the floor the suite proves the box's screen on.

// belowFloorEnsure is ensure's ready line for a Chromium a major below the
// floor, which the box's ensure readies as it readies any.
var belowFloorEnsure = strings.Replace(readyEnsure, "chromium="+browser.ExpectedChromiumVersion+" build=expected",
	fmt.Sprintf("chromium=%d.0.7000.10 build=untested", browser.MinChromiumMajor-1), 1)

// A JOB IS NAMED WITH THE BROWSER ITS RECORD OR ITS LAUNCH ASKS. Off its
// record, a job whose browser is ready at or above the floor is named ready,
// so a node that opens it again serves its screen at once, and one below the
// floor, withheld or with none is named with none; a launch names a browser
// seat's job asked, its ensure still to answer, and any other's with none.
//
// Mutation: name a job ready off a record below the floor, and it is.
func TestAJobIsNamedWithTheBrowserItsRecordOrItsLaunchAsks(t *testing.T) {
	t.Parallel()
	ready := func(major int) *types.ExecutorBrowser {
		return &types.ExecutorBrowser{State: browser.EnsureReady, ChromiumMajor: major}
	}
	for name, c := range map[string]struct {
		record *types.ExecutorBrowser
		want   JobBrowser
	}{
		"ready at the floor":    {ready(browser.MinChromiumMajor), JobBrowserReady},
		"ready above the floor": {ready(browser.MinChromiumMajor + 6), JobBrowserReady},
		"ready below the floor": {ready(browser.MinChromiumMajor - 1), ""},
		"withheld": {&types.ExecutorBrowser{State: browser.EnsureWithheld, Reason: browser.ReasonChromiumMissing,
			ChromiumMajor: browser.MinChromiumMajor}, ""},
		"none": {nil, ""},
	} {
		run := PendingRun{TurnID: "t1", LaunchID: "job-1", Launch: LaunchRecord{Browser: c.record}}
		if got := run.NamedJob(0, "", 0).Browser; got != c.want {
			t.Errorf("a record whose browser is %s names its job's browser %q, want %q", name, got, c.want)
		}
	}
	for name, c := range map[string]struct {
		req  LaunchRequest
		want JobBrowser
	}{
		"a browser seat's launch": {browserReq("t1", Container), JobBrowserAsked},
		"any other launch":        {launchReq("t1"), ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newWaiterRigIn(t, Container)
			rig.provider.Made = func(box *FakeSandbox) { ensureAnswers(box, readyEnsure) }
			var told []JobBrowser
			req := c.req
			req.Spec.Placement = Container
			req.Opened = func(job OpenedJob) (Opening, error) {
				told = append(told, job.Browser)
				return Opening{}, nil
			}
			if _, err := rig.launchVia(t.Context(), rig.manager, req); err != nil {
				t.Fatalf("Launch: %v", err)
			}
			if len(told) != 1 || told[0] != c.want {
				t.Errorf("the launch named its job's browser %q, want %q once", told, c.want)
			}
		})
	}
}

// ONLY A READY BROWSER AT OR ABOVE THE FLOOR IS WIRED TO ITS RUN. Where its
// ensure answered ready at or above the floor, what serves the box is told
// before the job starts, and the run is handed where its relay posts, and its
// fresh wiring's where it continues a session; withheld, or ready below the
// floor, nothing is told and the run is handed neither, so it starts no relay
// and the waiting screen serves nothing.
//
// Mutations, each red: hand the wiring before ensure has answered; hand it to
// a withheld browser.
func TestOnlyAReadyBrowserAtOrAboveTheFloorIsWiredToItsRun(t *testing.T) {
	t.Parallel()
	wired := func(job OpenedJob) BrowserWiring {
		return BrowserWiring{URL: "https://engine.example.com/mcp/" + job.LaunchID + "/browser", Job: job.LaunchID}
	}
	opening := func(note func(string), when func() string) func(job OpenedJob) (Opening, error) {
		return func(job OpenedJob) (Opening, error) {
			fresh := wired(job)
			fresh.URL += "/fresh"
			return Opening{Browser: wired(job), BrowserReady: func() { note("ready " + when()) },
				Fresh: &Opening{Browser: fresh}}, nil
		}
	}
	t.Run("ready at or above the floor", func(t *testing.T) {
		t.Parallel()
		var mu sync.Mutex
		var said []string
		note := func(what string) {
			mu.Lock()
			defer mu.Unlock()
			said = append(said, what)
		}
		rig := newWaiterRigIn(t, Container)
		var box *FakeSandbox
		rig.provider.Made = func(made *FakeSandbox) {
			box = made
			ensureAnswers(made, readyEnsure)
		}
		c := browserCoordinator(t, rig, nil)
		first, again := browserReq("t1", Container), browserReq("t1", Container)
		when := func() string {
			ensured := 0
			if box != nil {
				ensured = ensureRuns(box)
			}
			return fmt.Sprintf("after %d ensures and %d starts", ensured, len(rig.runner.Started()))
		}
		first.Opened, again.Opened = opening(note, when), opening(note, when)
		again.Continue = &Continuation{SessionID: "session-cmd-1", Brief: "carry on",
			Summarized: "carry on, from a summary"}
		made, kept := keptLaunch(t, rig, c, first, again)
		mu.Lock()
		defer mu.Unlock()
		want := []string{"ready after 1 ensures and 0 starts", "ready after 2 ensures and 1 starts"}
		if !slices.Equal(said, want) {
			t.Errorf("what serves the boxes was told %q, want %q: once each ensure answered, before its job "+
				"started", said, want)
		}
		started := rig.runner.Started()
		if len(started) != 2 {
			t.Fatalf("started %d runs, want 2", len(started))
		}
		if got, want := started[0].Browser, wired(OpenedJob{LaunchID: made.LaunchID}); got != want {
			t.Errorf("the first run was handed %+v, want %+v", got, want)
		}
		if started[0].FreshWiring != nil {
			t.Errorf("a run that continues no session was handed a fresh wiring %+v", started[0].FreshWiring)
		}
		if got, want := started[1].Browser, wired(OpenedJob{LaunchID: kept.LaunchID}); got != want {
			t.Errorf("the continuing run was handed %+v, want %+v", got, want)
		}
		if f := started[1].FreshWiring; f == nil || !f.Browser.Set() || f.Browser == started[1].Browser {
			t.Errorf("the continuing run's fresh wiring names its relay %+v, want its other token's", f)
		}
	})
	for name, stdout := range map[string]string{
		"withheld":              "crewlet-browser-ensure/1 withheld reason=chromium_missing cause=not_found",
		"ready below the floor": belowFloorEnsure,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			told := 0
			rig := newWaiterRigIn(t, Container)
			rig.provider.Made = func(box *FakeSandbox) { ensureAnswers(box, stdout) }
			c := browserCoordinator(t, rig, nil)
			first, again := browserReq("t1", Container), browserReq("t1", Container)
			opened := opening(func(string) { told++ }, func() string { return "" })
			first.Opened, again.Opened = opened, opened
			again.Continue = &Continuation{SessionID: "session-cmd-1", Brief: "carry on",
				Summarized: "carry on, from a summary"}
			keptLaunch(t, rig, c, first, again)
			if told != 0 {
				t.Errorf("what serves a %s browser's box was told it is ready %d times", name, told)
			}
			for i, run := range rig.runner.Started() {
				if run.Browser.Set() || (run.FreshWiring != nil && run.FreshWiring.Browser.Set()) {
					t.Errorf("run %d of a %s browser was handed where a relay posts: %+v, fresh %+v", i, name,
						run.Browser, run.FreshWiring)
				}
			}
		})
	}
}
