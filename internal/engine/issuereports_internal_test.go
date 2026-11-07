package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/issuereport"
	"github.com/crewlet/crewlet/internal/panics"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/queue"
	queuememory "github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// reportingEngine is an engine literal whose reporter sends to a counting relay.
func reportingEngine(t *testing.T) (*Engine, *atomic.Int32) {
	t.Helper()
	e, requests := idleReportingEngine(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go e.issueReports.Run(ctx)
	return e, requests
}

// idleReportingEngine is [reportingEngine] with no sender running.
func idleReportingEngine(t *testing.T) (*Engine, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"status":"filed","issue":"https://github.com/crewlet/crewlet/issues/1"}`)
	}))
	t.Cleanup(relay.Close)
	e := &Engine{issueReports: issuereport.New(issuereport.Config{Store: memory.NewFleet(),
		Client: relay.Client(), URL: relay.URL, Origin: issuereport.Origin{Build: issuereport.Build{Version: "v1.2.3",
			Commit: issuereport.CommitHash(strings.Repeat("a", 40)), Go: "go1.27.2", OS: "linux", Arch: "amd64"}}})}
	return e, &requests
}

func companyReporting(enabled bool) *Company {
	cfg := &config.Company{}
	cfg.Diagnostics.IssueReports.Enabled = enabled
	return &Company{Config: cfg}
}

func soon(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// panicAt is a contained panic in the module function named.
func panicAt(function string) *panics.Panic {
	return &panics.Panic{Scope: panics.ScopeTurn, Kind: panics.KindExplicit, Frames: []panics.Frame{
		{Function: "github.com/crewlet/crewlet/internal/engine." + function, File: "some.go", Line: 1}}}
}

// THE REVISION IS THE SWITCH, on every apply: a default company reports nothing,
// a revision turns it on, and the revision after it, or no company, turns it off.
func TestARevisionTurnsIssueReportsOnAndOff(t *testing.T) {
	t.Parallel()
	e, requests := reportingEngine(t)

	e.applyIssueReports(companyReporting(false))
	e.issueReports.Panicked(panicAt("first"))
	time.Sleep(20 * time.Millisecond)
	if got := requests.Load(); got != 0 {
		t.Fatalf("a default company sent %d", got)
	}

	e.applyIssueReports(companyReporting(true))
	e.issueReports.Panicked(panicAt("first"))
	soon(t, "the report", func() bool { return requests.Load() == 1 })

	for name, c := range map[string]*Company{"a revision off": companyReporting(false), "no company": nil} {
		e.applyIssueReports(companyReporting(true))
		e.applyIssueReports(c)
		e.issueReports.Panicked(panicAt(name[:1]))
		time.Sleep(20 * time.Millisecond)
		if got := requests.Load(); got != 1 {
			t.Fatalf("after %s, %d sent", name, got)
		}
	}
}

// A RUNNING SENDER HEARS EVERY PANIC THIS PROCESS CONTAINS, and a stopped one
// none: the observer goes with the sender, so an engine torn down reports nothing.
func TestTheSenderHearsEveryContainedPanicUntilItStops(t *testing.T) {
	e, requests := idleReportingEngine(t)
	e.applyIssueReports(companyReporting(true))
	e.startIssueSender(t.Context())
	t.Cleanup(e.stopIssueSender)
	contain := func(fn func()) {
		defer func() { panics.Recovered(panics.ScopeQueue, recover()) }()
		fn()
	}
	contain(func() { panic("a queue handler broke") })
	soon(t, "the report", func() bool { return requests.Load() == 1 })
	e.stopIssueSender()
	// A sender of the test's own, so a panic the stopped one still heard would go.
	go e.issueReports.Run(t.Context())
	contain(func() { panic("a second defect") })
	time.Sleep(20 * time.Millisecond)
	if got := requests.Load(); got != 1 {
		t.Errorf("a stopped sender's reporter heard a panic: %d sent", got)
	}
}

// THE LAST PROCESS'S CRASH IS REPORTED ONCE A REVISION SAYS ON, from the node's
// own disk, and a node that keeps nothing reports nothing. Not parallel: crash
// output is the process's.
func TestALeftCrashIsReportedWhenTheRevisionIsOn(t *testing.T) {
	t.Cleanup(func() { _ = debug.SetCrashOutput(nil, debug.CrashOptions{}) })
	dir := t.TempDir()
	kept := filepath.Join(dir, issuereport.NextStartDir)
	if err := os.MkdirAll(kept, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"setting": "on\n",
		"crash": "panic: assignment to entry in nil map\n\ngoroutine 1 [running]:\n" +
			"github.com/crewlet/crewlet/internal/engine.(*Engine).serve(0x1)\n\t/build/engine/serve.go:7 +0x1\n",
	} {
		if err := os.WriteFile(filepath.Join(kept, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	e, requests := reportingEngine(t)
	next, err := issuereport.OpenNextStart(dir, issuereport.Origin{Build: issuereport.Build{Version: "v1.2.3",
		Commit: issuereport.CommitHash(strings.Repeat("a", 40))}})
	if err != nil {
		t.Fatal(err)
	}
	e.nextStart = next
	e.applyIssueReports(companyReporting(true))
	soon(t, "the crash's report", func() bool { return requests.Load() == 1 })
	e.applyIssueReports(companyReporting(true))
	time.Sleep(20 * time.Millisecond)
	if got := requests.Load(); got != 1 {
		t.Errorf("the crash was reported %d times", got)
	}
}

// ONLY THE ENGINE'S OWN CAUSE IS A DEFECT TO REPORT: a host that cannot be
// reached, a spent plan and a refused key dead-letter mail too, and no
// maintainer can mend them. A message whose deliveries all went on hand-backs
// has no outside cause to name.
func TestADeadLetterIsReportedOnlyForTheEnginesOwnCause(t *testing.T) {
	t.Parallel()
	unreachable := sandbox.Because(sandbox.WhyProviderUnavailable, errors.New("the box host did not answer"))
	for name, c := range map[string]struct {
		result queue.Result
		want   int32
	}{
		"an unattributed failure": {queue.Nak(errors.New("a nil map somewhere")), 1},
		"hand-backs":              {queue.Defer("the seat moved"), 1},
		"an unreachable host":     {queue.Nak(fmt.Errorf("engine: turn: %w", unreachable)), 0},
		"a spent plan":            {queue.Nak(&llm.Error{Kind: llm.KindRateLimit, Err: errors.New("limit")}), 0},
		"a refused key":           {queue.Nak(&llm.Error{Kind: llm.KindAuth, Err: errors.New("401")}), 0},
		"a provider outage":       {queue.Nak(&llm.Error{Kind: llm.KindServer, Err: errors.New("overloaded")}), 0},
		"a budget":                {queue.Nak(fmt.Errorf("x: %w", toolloop.ErrBudgetExhausted)), 0},
		"a cancelled context":     {queue.Nak(context.Canceled), 0},
		"a deadline":              {queue.Nak(context.DeadlineExceeded), 0},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e, requests := reportingEngine(t)
			e.applyIssueReports(companyReporting(true))
			e.reportDeadLetter(t.Context(), "a-seat", c.result)
			if c.want > 0 {
				soon(t, "the report", func() bool { return requests.Load() == c.want })
				return
			}
			time.Sleep(20 * time.Millisecond)
			if got := requests.Load(); got != 0 {
				t.Errorf("%d sent for an outside cause", got)
			}
		})
	}
}

// A DELIVERY'S HEADROOM DECIDES WHETHER ITS HAND-BACK RETIRES A MESSAGE, on what
// the queue states of each message, a spent sibling and a deferral included.
func TestAHandBackRetiresOnlyASpentMessage(t *testing.T) {
	t.Parallel()
	spent, healthy := uuid.New(), uuid.New()
	failing := queue.Nak(errors.New("failed"))
	ev := func(id uuid.UUID) *events.Event { return &events.Event{ID: id} }
	stated := queue.WithHeadroom(context.Background(), []queue.Headroom{{ID: spent, Left: 0}, {ID: healthy, Left: 7}})
	for name, c := range map[string]struct {
		result queue.Result
		evs    []*events.Event
		ctx    context.Context
		want   bool
	}{
		"a spent message handed back":   {failing, []*events.Event{ev(spent)}, stated, true},
		"a spent sibling":               {failing, []*events.Event{ev(healthy), ev(spent)}, stated, true},
		"a deferral of a spent message": {queue.Defer("moved"), []*events.Event{ev(spent)}, stated, true},
		"headroom in hand":              {failing, []*events.Event{ev(healthy)}, stated, false},
		"acknowledged":                  {queue.Ack(), []*events.Event{ev(spent)}, stated, false},
		"no headroom stated":            {failing, []*events.Event{ev(spent)}, context.Background(), false},
		"no message":                    {failing, []*events.Event{nil}, stated, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := deadLetters(c.ctx, c.result, c.evs); got != c.want {
				t.Errorf("deadLetters = %v, want %v", got, c.want)
			}
		})
	}
}

// reportTopology names the placement a company's cell resolves to, and a company
// with no sandbox as none.
func TestAReportsTopologyIsTheClosedSets(t *testing.T) {
	t.Parallel()
	got := issuereport.NewTopology(3, string(config.StreamEmbedded), string(config.CoordinationEmbeddedKV), string(config.PlacementE2B))
	want := issuereport.Topology{Nodes: "2-3", Stream: "embedded", Coordination: "embedded-kv", Sandbox: "e2b"}
	if got != want {
		t.Errorf("topology = %+v, want %+v", got, want)
	}
}

// A NODE WITHOUT `data` SENDS WHAT IT DETECTS. It builds a reporter and listens to
// its own publishes like any other, and runs no core, so a sender tied to the core
// left it counting and queueing for nobody.
func TestANodeThatBuildsAReporterRunsItsSender(t *testing.T) {
	t.Parallel()
	boot := config.DefaultBootstrap()
	boot.Node.Roles = []string{"seats"}
	if HoldsEstate(&boot) {
		t.Fatal("the case wants a node without data")
	}
	queue := queuememory.NewBroker().Client()
	e := &Engine{}
	e.startIssueReports(&boot, &Backends{Queue: queue, Fleet: memory.NewFleet()})
	if e.issueReports == nil {
		t.Fatal("a node with a broker and a fleet store built no reporter")
	}
	e.startIssueSender(t.Context())
	soon(t, "the sender to run", e.issueReports.Sending)
	e.stopIssueSender()
	if e.issueReports.Sending() {
		t.Error("the sender outlived its stop")
	}
}

// A NODE WITH NO REPORTER STARTS NOTHING, and stopping what never started is safe:
// a failed boot's teardown comes through here.
func TestNoReporterNoSender(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	e.startIssueSender(t.Context())
	e.stopIssueSender()
	if e.endIssueSender != nil {
		t.Error("a node with no reporter started a sender")
	}
}

// A RESUME THAT KEPT FAILING IS REPORTED ONLY ON THE ENGINE'S OWN KIND, so a run's
// announcement must carry the classifier's word: unwired, it reads as no kind at all
// and that report is never sent, with nothing to say so.
func TestARunsAnnouncementCarriesTheClassifiersWord(t *testing.T) {
	t.Parallel()
	source, err := os.ReadFile("sandbox.go")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`FailureKind:\s+runner\.ErrorKind,`).Match(source) {
		t.Error("the coordinator is built without the classifier")
	}
}
